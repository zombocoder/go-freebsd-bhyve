//go:build freebsd

package bhyve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	vmm "github.com/zombocoder/go-freebsd-vmm"
)

// vmmDir is where vmm(4) publishes its virtual machines.
const vmmDir = "/dev/vmm"

// runningPollInterval is how often Start looks for the guest to start
// executing.
//
// This only affects how quickly StateRunning is noticed; whether a VM
// ran at all is decided from the device node, not from this poll, so a
// missed tick costs nothing but latency.
const runningPollInterval = 5 * time.Millisecond

// maxStderr bounds how much of bhyve's standard error is kept.
//
// bhyve explains a startup failure in its first line or two, so keeping
// the beginning is enough; the cap stops a guest wired to stdio from
// filling memory with console output.
const maxStderr = 8 << 10

// Option configures a VM.
type Option func(*VM)

// WithTool sets the paths of the external programs to run.
func WithTool(t Tool) Option {
	return func(vm *VM) { vm.tool = t }
}

// WithDir sets the directory for the files a run needs, which for now
// is the generated configuration file.
//
// The default is the system temporary directory. A control plane will
// usually want somewhere it controls, so that a configuration can be
// inspected after a failure.
func WithDir(dir string) Option {
	return func(vm *VM) { vm.dir = dir }
}

// WithStderrFile sends bhyve's standard error to the file at path
// instead of to a pipe read by this process.
//
// By default Start collects bhyve's standard error through a pipe, so
// that a startup failure can be reported in bhyve's own words. The read
// end of that pipe belongs to the calling process. bhyve runs in its own
// process group and can outlive its caller, which is what lets a
// supervising daemon restart without taking its guests down; but once
// the caller has exited, the next thing bhyve writes to standard error,
// an unhandled MSR warning or a device model complaining, gets SIGPIPE
// or EPIPE and can kill the guest. A caller that needs guests to survive
// its own restart should use this option.
//
// The file is opened for appending, created with mode 0600 if it does
// not exist, and handed to bhyve as its file descriptor 2 directly: no
// pipe and no copying goroutine. Its directory must already exist.
// Start closes this process's copy as soon as bhyve has been started.
//
// Error messages are unaffected. Start notes the file's size before
// running bhyve, and when bhyve exits, whatever it appended since then
// (at most the last 8 KiB) becomes ExitReason.Message.
//
// The file only grows; rotating it is the caller's job. Because bhyve
// writes in append mode, newsyslog-style rotation that copies and then
// truncates the file is safe: bhyve's next write lands at the new end.
func WithStderrFile(path string) Option {
	return func(vm *VM) { vm.stderrPath = path }
}

// VM is a virtual machine described by a configuration and, while it
// runs, the bhyve process running it.
//
// # Lifecycle
//
// New describes a VM, Start runs it, Wait collects the reason it
// stopped, and Destroy removes it from the kernel. A VM that has exited
// can be started again, which is how a guest-requested reboot is
// serviced.
//
// Destroy is not optional. bhyve leaves the virtual machine in the
// kernel when it exits, so a VM that is never destroyed stays visible
// in /dev/vmm and keeps its name taken:
//
//	vm, err := bhyve.New(cfg)
//	if err != nil {
//		return err
//	}
//	defer vm.Destroy()
//
//	if err := vm.Start(ctx); err != nil {
//		return err
//	}
//	reason, err := vm.Wait(ctx)
//
// # What this does not own
//
// The disks and network interfaces named in the configuration are not
// touched: not created, not removed, not checked for being in use.
// Their lifecycle belongs to whatever provisioned them.
//
// # Concurrency
//
// Methods are safe to call concurrently. Wait may be called from
// several goroutines; they all see the same reason.
type VM struct {
	cfg        Config
	tool       Tool
	dir        string
	stderrPath string

	mu       sync.Mutex
	state    State
	cmd      *exec.Cmd
	confPath string

	// observed records that the virtual machine was seen in
	// /dev/vmm. See the note in reap about why this is not the only
	// source for ExitReason.Started.
	observed bool

	reason    ExitReason
	hasReason bool

	// running closes when the VM is first observed, done when the
	// process has been reaped. Both are replaced by each Start.
	running chan struct{}
	done    chan struct{}
}

// New describes a virtual machine.
//
// The configuration is checked locally; whether bhyve agrees is a
// separate question, answered by Config.Validate or by Start.
func New(cfg Config, opts ...Option) (*VM, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}

	vm := &VM{cfg: cfg, state: StateDefined}
	for _, opt := range opts {
		opt(vm)
	}

	return vm, nil
}

// Name returns the VM name.
func (vm *VM) Name() string { return vm.cfg.Name }

// Config returns a copy of the configuration.
func (vm *VM) Config() Config { return vm.cfg }

// State returns where the VM is in its lifecycle.
func (vm *VM) State() State {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.state
}

// Reason returns why the VM stopped, and whether it has.
func (vm *VM) Reason() (ExitReason, bool) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.reason, vm.hasReason
}

// Pid returns the bhyve process id, or 0 when none is running.
func (vm *VM) Pid() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.cmd == nil || vm.cmd.Process == nil || !vm.state.Active() {
		return 0
	}
	return vm.cmd.Process.Pid
}

// Exists reports whether the virtual machine is present in the kernel.
//
// This is not the same as running. bhyve leaves the VM behind when it
// exits, so a VM can exist with no process attached to it; that is what
// Destroy is for.
func (vm *VM) Exists() bool { return vmExists(vm.cfg.Name) }

// vmExists reports whether /dev/vmm holds a VM of this name.
func vmExists(name string) bool {
	_, err := os.Stat(filepath.Join(vmmDir, name))
	return err == nil
}

// Start runs the VM and returns once it is running.
//
// It returns when bhyve has released the guest's boot processor, or
// when bhyve has exited before doing so, whichever comes first. That
// makes a configuration bhyve will not accept, a guest with nothing to
// boot, or a device backend that cannot be opened an error from Start
// rather than a surprise from a later Wait.
//
// The VM appearing under /dev/vmm is not that moment. bhyve creates the
// VM first and only then loads the boot ROM and initialises its devices,
// any of which can still fail; FreeBSD 14 even creates it before
// noticing there is nothing to boot. What marks the end of startup is
// the boot processor: bhyve activates every vCPU suspended and resumes
// vCPU 0 as the last step before it starts serving the guest, on both
// FreeBSD 14 and 15.
//
// A guest quick enough to finish before Start notices it is not an
// error: Start succeeds and Wait reports what happened.
//
// If ctx is cancelled while the VM is still starting, the bhyve process
// is killed before Start returns, so that no unsupervised process is
// left behind.
//
// Start refuses to run if the virtual machine already exists in the
// kernel. That is deliberate rather than defensive: because bhyve
// leaves the VM behind on exit, a leftover is the normal state after a
// previous run, and reusing it would make the previous guest's memory
// and device state the new guest's starting point. Call Destroy first.
func (vm *VM) Start(ctx context.Context) error {
	const op = "start"

	running, done, err := vm.launch()
	if err != nil {
		return err
	}

	// Wait for the first thing to happen, then decide once.
	//
	// Deciding inside the select would be wrong: a VM that fails
	// immediately makes both channels ready at the same moment, and
	// select picks between ready cases at random. The same
	// configuration would then succeed or fail depending on
	// scheduling. Everything below reads the recorded facts instead.
	select {
	case <-running:
	case <-done:
	case <-ctx.Done():
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		// Leaving a process running that nobody is waiting for
		// would be worse than the cancellation itself.
		_ = vm.Kill()
		<-done
		return newError(op, vm.cfg.Name, ctxErr)
	}

	if reason, exited := vm.Reason(); exited {
		// bhyve is already gone. Whether that is a failure
		// depends on why: a guest that booted and powered itself
		// off before the poll noticed did start, and Wait has the
		// details. Anything else did not.
		if reason.Kind.GuestRequested() {
			return nil
		}
		return newError(op, vm.cfg.Name,
			fmt.Errorf("bhyve exited during startup: %v", reason))
	}

	return nil
}

// launch starts the bhyve process and the goroutines that watch it.
func (vm *VM) launch() (running, done chan struct{}, err error) {
	const op = "start"

	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.state.Active() {
		return nil, nil, newError(op, vm.cfg.Name, ErrAlreadyRunning)
	}
	if vmExists(vm.cfg.Name) {
		return nil, nil, newError(op, vm.cfg.Name, fmt.Errorf(
			"%s already exists; bhyve leaves it behind on exit, so call Destroy first: %w",
			filepath.Join(vmmDir, vm.cfg.Name), ErrAlreadyRunning))
	}

	path, err := vm.tool.bhyvePath()
	if err != nil {
		return nil, nil, newError(op, vm.cfg.Name, err)
	}

	rendered, err := vm.cfg.Render()
	if err != nil {
		return nil, nil, err
	}
	confPath, err := vm.writeConfig(rendered)
	if err != nil {
		return nil, nil, newError(op, vm.cfg.Name, err)
	}

	cmd := exec.Command(path, "-k", confPath, vm.cfg.Name)

	// stderrText is what reap passes to decodeExit once bhyve is gone.
	var stderrText func() string
	if vm.stderrPath != "" {
		file, offset, err := openStderrFile(vm.stderrPath)
		if err != nil {
			os.Remove(confPath)
			return nil, nil, newError(op, vm.cfg.Name, err)
		}
		// bhyve gets its own descriptor at Start; this process's
		// copy is not needed past this function, whether or not
		// Start succeeds.
		defer file.Close()

		// An *os.File, not a wrapper: os/exec then passes the
		// descriptor itself rather than a pipe it has to drain.
		cmd.Stderr = file
		stderrPath := vm.stderrPath
		stderrText = func() string {
			// Failing to read back is not worth failing the
			// reap over; the exit status still says enough.
			text, _ := readStderrTail(stderrPath, offset, maxStderr)
			return text
		}
	} else {
		stderr := &boundedBuffer{limit: maxStderr}
		cmd.Stderr = stderr
		stderrText = stderr.String
	}

	// Its own process group, so that a hard kill can reach anything
	// bhyve started rather than only bhyve itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		os.Remove(confPath)
		return nil, nil, newError(op, vm.cfg.Name, err)
	}

	running = make(chan struct{})
	done = make(chan struct{})

	vm.cmd = cmd
	vm.confPath = confPath
	vm.observed = false
	vm.hasReason = false
	vm.reason = ExitReason{}
	vm.state = StateStarting
	vm.running = running
	vm.done = done

	go vm.watch(running, done)
	go vm.reap(cmd, stderrText, confPath, running, done)

	return running, done, nil
}

// watch looks for the guest to start executing, and moves the VM to
// StateRunning when it does. See Start for why that is the boot
// processor being resumed rather than the VM appearing.
func (vm *VM) watch(running, done chan struct{}) {
	ticker := time.NewTicker(runningPollInterval)
	defer ticker.Stop()

	var handle *vmm.VM
	defer func() {
		if handle != nil {
			handle.Close()
		}
	}()

	for {
		if handle == nil && vmExists(vm.cfg.Name) {
			// Failing to open is not fatal: the VM may be gone
			// again already, and reap will say why.
			handle, _ = vmm.Open(vm.cfg.Name)
		}
		if handle != nil && bootProcessorRunning(handle) {
			vm.mu.Lock()
			vm.observed = true
			if vm.state == StateStarting {
				vm.state = StateRunning
			}
			vm.mu.Unlock()
			close(running)
			return
		}

		select {
		case <-done:
			// The process is gone and the guest never started
			// while we were looking. Whether the VM was created
			// at all is decided in reap, which can still see the
			// device node bhyve left behind.
			return
		case <-ticker.C:
		}
	}
}

// bootProcessorRunning reports whether bhyve has released vCPU 0: it is
// active, which bhyve makes it while adding vCPUs, and no longer
// suspended, which bhyve undoes only once all of its setup is done.
func bootProcessorRunning(handle *vmm.VM) bool {
	active, err := handle.ActiveCPUs()
	if err != nil || !active.Contains(0) {
		return false
	}
	suspended, err := handle.SuspendedCPUs()
	if err != nil {
		return false
	}
	return !suspended.Contains(0)
}

// reap waits for bhyve to finish and records why.
func (vm *VM) reap(cmd *exec.Cmd, stderrText func() string, confPath string, running, done chan struct{}) {
	waitErr := cmd.Wait()
	status, signal := exitStatus(waitErr)

	vm.mu.Lock()

	// Whether the VM ran cannot come from the exit status, which is
	// ambiguous, nor from the poll above, which can miss a guest that
	// finishes immediately. The device node answers it: bhyve creates
	// the VM only after it has accepted its configuration, and leaves
	// it behind on exit, so finding it now means bhyve got that far.
	//
	// Start refuses to run when the node already exists, which is
	// what makes this inference sound: the node cannot be a leftover
	// from an earlier run.
	started := vm.observed || vmExists(vm.cfg.Name)

	vm.reason = decodeExit(status, signal, started, stderrText())
	vm.hasReason = true
	vm.state = StateExited
	vm.mu.Unlock()

	os.Remove(confPath)
	close(done)
}

// writeConfig writes the rendered configuration for one run.
func (vm *VM) writeConfig(contents string) (string, error) {
	dir := vm.dir
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}

	file, err := os.CreateTemp(dir,
		"gobhyve-"+sanitiseForFilename(vm.cfg.Name)+"-*.conf")
	if err != nil {
		return "", err
	}
	defer file.Close()

	if _, err := file.WriteString(contents); err != nil {
		os.Remove(file.Name())
		return "", err
	}

	return file.Name(), nil
}

// Wait returns why the VM stopped, waiting for it to do so.
//
// It is safe to call from several goroutines and after the VM has
// already exited. Cancelling ctx stops waiting; it does not stop the VM.
func (vm *VM) Wait(ctx context.Context) (ExitReason, error) {
	const op = "wait"

	vm.mu.Lock()
	done := vm.done
	reason, has := vm.reason, vm.hasReason
	state := vm.state
	vm.mu.Unlock()

	if has {
		return reason, nil
	}
	if done == nil || state == StateDefined {
		return ExitReason{}, newError(op, vm.cfg.Name, ErrNotRunning)
	}

	select {
	case <-done:
		reason, _ := vm.Reason()
		return reason, nil
	case <-ctx.Done():
		return ExitReason{}, newError(op, vm.cfg.Name, ctx.Err())
	}
}

// Stop asks the guest to shut down and waits for bhyve to finish.
//
// # What SIGTERM means to bhyve
//
// Stop sends SIGTERM, and what that does depends on the guest. bhyve
// turns it into an ACPI power button press, which a guest that is
// listening handles by shutting down cleanly. But bhyve only installs
// that handler once the guest has enabled ACPI, by writing to the PM1
// control register. Before then, during firmware or early boot, and for
// any guest that never enables ACPI, SIGTERM keeps its default
// disposition and kills bhyve outright.
//
// So a successful Stop reports either outcome, and the difference
// matters: ExitPoweredOff means the guest shut itself down, while
// ExitSignalled means it was cut off mid-flight, with whatever that
// implies for its filesystems.
//
// If ctx is cancelled before bhyve exits, the process is killed and the
// reason reports ExitSignalled.
func (vm *VM) Stop(ctx context.Context) (ExitReason, error) {
	const op = "stop"

	vm.mu.Lock()
	if !vm.state.Active() {
		reason, has := vm.reason, vm.hasReason
		vm.mu.Unlock()
		if has {
			return reason, nil
		}
		return ExitReason{}, newError(op, vm.cfg.Name, ErrNotRunning)
	}
	proc := vm.cmd.Process
	done := vm.done
	vm.state = StateStopping
	vm.mu.Unlock()

	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return ExitReason{}, newError(op, vm.cfg.Name, err)
	}

	select {
	case <-done:
		reason, _ := vm.Reason()
		return reason, nil

	case <-ctx.Done():
		if err := vm.Kill(); err != nil {
			return ExitReason{}, newError(op, vm.cfg.Name, err)
		}
		<-done
		reason, _ := vm.Reason()
		return reason, nil
	}
}

// Kill terminates bhyve immediately, without giving the guest a chance
// to shut down.
//
// The whole process group is signalled, so anything bhyve started goes
// with it. Killing a VM that is not running does nothing.
func (vm *VM) Kill() error {
	const op = "kill"

	vm.mu.Lock()
	if !vm.state.Active() || vm.cmd == nil || vm.cmd.Process == nil {
		vm.mu.Unlock()
		return nil
	}
	pid := vm.cmd.Process.Pid
	vm.mu.Unlock()

	// Negative pid reaches the group, which is why Setpgid was set.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return newError(op, vm.cfg.Name, err)
	}
	return nil
}

// Destroy removes the virtual machine from the kernel.
//
// This is a separate step from stopping, because bhyve leaves the VM in
// place when it exits: a VM that is only stopped still occupies its name
// and still holds its guest memory. Destroy is what releases both.
//
// It refuses to run while bhyve is alive, since pulling the VM out from
// under a running process is not something to do by accident. Destroying
// a VM that does not exist succeeds.
func (vm *VM) Destroy() error {
	const op = "destroy"

	vm.mu.Lock()
	active := vm.state.Active()
	vm.mu.Unlock()

	if active {
		return newError(op, vm.cfg.Name,
			fmt.Errorf("bhyve is still running; stop it first: %w",
				ErrAlreadyRunning))
	}

	return destroyVM(vm.cfg.Name)
}

// destroyVM removes a VM from the kernel by name.
//
// The work is done through go-freebsd-vmm rather than by running
// bhyvectl: opening and destroying a VM is exactly what that library is
// for, and a library that shelled out for it would be reimplementing
// what it already depends on.
func destroyVM(name string) error {
	const op = "destroy"

	if !vmExists(name) {
		return nil
	}

	handle, err := vmm.Open(name)
	if err != nil {
		// Gone between the check and the open is success, not
		// failure.
		if !vmExists(name) {
			return nil
		}
		return newError(op, name, err)
	}

	if err := handle.Destroy(); err != nil {
		return newError(op, name, err)
	}
	return nil
}

// boundedBuffer collects output up to a limit, keeping the beginning.
//
// bhyve says why it is unhappy in its first line, so the start is the
// part worth keeping; the limit is what stops a guest console wired to
// stdio from growing without bound.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

// Write implements io.Writer, discarding anything past the limit.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if room := b.limit - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}

	// Report the whole write as accepted: the writer is bhyve, and
	// a short write would make it fail rather than simply be
	// truncated here.
	return len(p), nil
}

// String returns what was collected.
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// openStderrFile opens the file bhyve's standard error goes to, and
// returns its size, which is where this run's output will start.
func openStderrFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("stderr file %s: directory %s does not exist: %w",
				path, filepath.Dir(path), err)
		}
		return nil, 0, fmt.Errorf("stderr file: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("stderr file: %w", err)
	}

	return file, info.Size(), nil
}

// readStderrTail returns what was written to path after offset, keeping
// at most the last limit bytes.
//
// The end is kept rather than the beginning, unlike boundedBuffer: the
// file may hold a long run's worth of warnings, and the reason bhyve
// exited is the last thing it wrote. If the file is now shorter than
// offset it was truncated by rotation, and everything in it is newer
// than offset, so all of it counts.
func readStderrTail(path string, offset int64, limit int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}

	size := info.Size()
	if size < offset {
		offset = 0
	}
	if size-offset > int64(limit) {
		offset = size - int64(limit)
	}

	buf := make([]byte, size-offset)
	n, err := file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:n]), nil
}

// DestroyByName removes a virtual machine from the kernel by name,
// without a VM to go through.
//
// This is for cleaning up after something that is no longer running:
// a crashed supervisor, an interrupted test, a VM whose process was
// killed from outside. Destroying one that does not exist succeeds.
//
// It does not check whether a bhyve process still holds the VM, because
// there may be no way to find out; use VM.Destroy when there is a VM to
// ask.
func DestroyByName(name string) error {
	return destroyVM(name)
}
