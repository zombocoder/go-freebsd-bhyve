//go:build freebsd

package bhyve

import (
	"context"
	"errors"
	"fmt"
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

// runningPollInterval is how often Start looks for the VM to appear.
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
	cfg  Config
	tool Tool
	dir  string

	mu       sync.Mutex
	state    State
	cmd      *exec.Cmd
	confPath string
	stderr   *boundedBuffer

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
// It returns when the virtual machine has appeared in the kernel, or
// when bhyve has exited without it appearing, whichever comes first.
// That makes a configuration bhyve will not accept, or a guest with
// nothing to boot, an error from Start rather than a surprise from a
// later Wait.
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

	stderr := &boundedBuffer{limit: maxStderr}
	cmd := exec.Command(path, "-k", confPath, vm.cfg.Name)
	cmd.Stderr = stderr

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
	vm.stderr = stderr
	vm.observed = false
	vm.hasReason = false
	vm.reason = ExitReason{}
	vm.state = StateStarting
	vm.running = running
	vm.done = done

	go vm.watch(running, done)
	go vm.reap(cmd, stderr, confPath, running, done)

	return running, done, nil
}

// watch looks for the virtual machine to appear, and moves the VM to
// StateRunning when it does.
func (vm *VM) watch(running, done chan struct{}) {
	ticker := time.NewTicker(runningPollInterval)
	defer ticker.Stop()

	for {
		if vmExists(vm.cfg.Name) {
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
			// The process is gone and the VM never appeared
			// while we were looking. Whether it ran at all is
			// decided in reap, which can still see the device
			// node bhyve left behind.
			return
		case <-ticker.C:
		}
	}
}

// reap waits for bhyve to finish and records why.
func (vm *VM) reap(cmd *exec.Cmd, stderr *boundedBuffer, confPath string, running, done chan struct{}) {
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

	vm.reason = decodeExit(status, signal, started, stderr.String())
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
