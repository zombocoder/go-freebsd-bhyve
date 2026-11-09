//go:build freebsd

// Package lifecycle_test drives real bhyve processes: starting them,
// supervising them, interpreting why they finished, and cleaning up
// after them.
package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
	"github.com/zombocoder/go-freebsd-bhyve/tests/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Run(m))
}

// ctx returns a context bounded by the harness timeout.
func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), harness.Timeout)
	t.Cleanup(cancel)
	return c
}

// TestUnbootableVM is the lifecycle end to end for a VM with no
// firmware: bhyve accepts the configuration, finds nothing to boot, and
// exits with status 4.
//
// It is a real test despite the guest never running, because it covers
// the parts that are hard: launching, detecting whether the VM reached
// the kernel, decoding the exit, and cleaning up.
//
// Releases differ in the middle. bhyve 14 creates the VM before it
// notices there is nothing to boot, and leaves it behind. bhyve 15
// checks first and exits without creating anything. Either is fine;
// what must hold is that Started agrees with what is in the kernel.
func TestUnbootableVM(t *testing.T) {
	cfg := harness.Config(t)
	vm := harness.RequireVM(t, cfg)

	if got := vm.State(); got != bhyve.StateDefined {
		t.Fatalf("State() before Start = %v, want defined", got)
	}

	err := vm.Start(ctx(t))
	if err == nil {
		t.Fatal("Start succeeded on a VM with no firmware, want an error")
	}

	reason, waitErr := vm.Wait(ctx(t))
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}

	if reason.Kind != bhyve.ExitBhyveError {
		t.Errorf("Kind = %v, want bhyve error", reason.Kind)
	}
	if reason.Status != 4 {
		t.Errorf("Status = %d, want 4", reason.Status)
	}
	// bhyve's own words are the only explanation available here, so
	// they have to survive.
	if reason.Message == "" {
		t.Error("Message is empty; bhyve's explanation was lost")
	}
	t.Logf("exit reason: %v", reason)

	if got := vm.State(); got != bhyve.StateExited {
		t.Errorf("State() = %v, want exited", got)
	}

	// Started is taken from the device node, so it has to agree with
	// it. A VM that reached the kernel outlives the process, which is
	// the reason Destroy exists.
	if exists := vm.Exists(); exists != reason.Started {
		t.Errorf("Started = %v but the VM exists = %v", reason.Started, exists)
	}
	if reason.Started {
		t.Log("bhyve created the VM before finding nothing to boot (FreeBSD 14)")
	} else {
		t.Log("bhyve refused before creating the VM (FreeBSD 15)")
	}
}

// TestStartIsDeterministic guards the race that the first version of
// Start had: with the VM appearing and the process exiting at almost the
// same moment, a select over both channels picked at random.
func TestStartIsDeterministic(t *testing.T) {
	harness.RequireHost(t)

	const runs = 5
	var kinds []bhyve.ExitKind

	for i := 0; i < runs; i++ {
		cfg := harness.Config(t)
		vm := harness.RequireVM(t, cfg)

		if err := vm.Start(ctx(t)); err == nil {
			t.Fatalf("run %d: Start succeeded, want an error", i)
		}
		reason, err := vm.Wait(ctx(t))
		if err != nil {
			t.Fatalf("run %d: Wait: %v", i, err)
		}
		kinds = append(kinds, reason.Kind)
	}

	for i, kind := range kinds {
		if kind != kinds[0] {
			t.Fatalf("run %d decoded to %v but run 0 to %v; Start is not deterministic",
				i, kind, kinds[0])
		}
	}
	t.Logf("%d runs all reported %v", runs, kinds[0])
}

// TestStartFailsWhenADeviceCannotOpen covers bhyve failing after it
// has created the VM: the VM appears in /dev/vmm, then a device backend
// cannot be opened and bhyve exits 4. FreeBSD 14 does the same for a VM
// with nothing to boot.
//
// Start used to succeed whenever it saw the VM before bhyve exited,
// which made the outcome depend on scheduling. It must fail every time.
func TestStartFailsWhenADeviceCannotOpen(t *testing.T) {
	harness.RequireHost(t)
	rom := harness.FirmwarePath(t)

	const runs = 10
	for i := 0; i < runs; i++ {
		cfg := harness.Config(t)
		cfg.Firmware = &bhyve.Firmware{ROM: rom}
		cfg.Disks = []bhyve.Disk{{Path: "/nonexistent/gobhyve-test.img"}}
		// Wiring guest memory happens after the VM is created and
		// before devices are opened. It takes long enough to make
		// the window between the two certain rather than a race.
		cfg.Memory = bhyve.Memory{Size: 1 << 30, Wired: true}
		vm := harness.RequireVM(t, cfg)

		if err := vm.Start(ctx(t)); err == nil {
			t.Fatalf("run %d: Start succeeded although bhyve could not open the disk", i)
		}
		reason, err := vm.Wait(ctx(t))
		if err != nil {
			t.Fatalf("run %d: Wait: %v", i, err)
		}
		if reason.Kind != bhyve.ExitBhyveError {
			t.Fatalf("run %d: Kind = %v, want bhyve error", i, reason.Kind)
		}
		if i == 0 {
			t.Logf("exit reason: %v", reason)
		}
	}
}

// TestDestroyReleasesTheName covers the consequence of bhyve leaving the
// VM behind: until it is destroyed, the name is taken.
//
// The leftover comes from killing a running VM. bhyve cannot clean up
// after SIGKILL on any release, which makes this the reliable way to get
// one; an unbootable VM leaves one behind only on FreeBSD 14.
func TestDestroyReleasesTheName(t *testing.T) {
	harness.RequireHost(t)
	rom := harness.FirmwarePath(t)

	cfg := harness.Config(t)
	cfg.Firmware = &bhyve.Firmware{ROM: rom}
	cfg.Framebuffer = &bhyve.Framebuffer{Port: 15904}

	first, err := bhyve.New(cfg, bhyve.WithDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { first.Destroy() })

	// Run once and kill it, leaving the VM in the kernel.
	if err := first.Start(ctx(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := first.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := first.Wait(ctx(t)); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !first.Exists() {
		t.Fatal("the VM is gone after bhyve was killed; it should have been left behind")
	}

	// A second VM of the same name refuses to start rather than
	// inheriting the first one's memory and device state.
	second, err := bhyve.New(cfg, bhyve.WithDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = second.Start(ctx(t))
	if !errors.Is(err, bhyve.ErrAlreadyRunning) {
		t.Errorf("Start over a leftover VM = %v, want ErrAlreadyRunning", err)
	}

	// Destroying it frees the name.
	if err := first.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if first.Exists() {
		t.Fatal("the VM still exists after Destroy")
	}

	// With the name free, a new VM under it starts normally.
	third, err := bhyve.New(cfg, bhyve.WithDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		third.Kill()
		third.Wait(ctx(t))
		third.Destroy()
	})
	if err := third.Start(ctx(t)); err != nil {
		t.Errorf("Start after Destroy freed the name: %v", err)
	}
}

// TestDestroyRefusesWhileRunning keeps a VM from being pulled out from
// under a live bhyve process.
func TestDestroyRefusesWhileRunning(t *testing.T) {
	harness.RequireHost(t)

	vm := harness.RequireRunningVM(t, 15900)

	if err := vm.Destroy(); !errors.Is(err, bhyve.ErrAlreadyRunning) {
		t.Errorf("Destroy while running = %v, want ErrAlreadyRunning", err)
	}

	// And Start refuses a second time too.
	if err := vm.Start(ctx(t)); !errors.Is(err, bhyve.ErrAlreadyRunning) {
		t.Errorf("Start on a running VM = %v, want ErrAlreadyRunning", err)
	}

	if got := vm.Pid(); got <= 0 {
		t.Errorf("Pid() = %d, want the bhyve process id", got)
	}
}

// TestKillStopsARunningVM covers the hard path, which is the one that
// has to work when nothing else does.
func TestKillStopsARunningVM(t *testing.T) {
	harness.RequireHost(t)

	vm := harness.RequireRunningVM(t, 15901)

	if err := vm.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	reason, err := vm.Wait(ctx(t))
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if reason.Kind != bhyve.ExitSignalled {
		t.Errorf("Kind = %v, want signalled", reason.Kind)
	}
	if reason.Signal == 0 {
		t.Error("Signal = 0 for a killed process")
	}
	// The rendered configuration must suit the installed bhyve
	// without complaint. FreeBSD 15 warns here if only the old
	// lpc.bootrom name is given.
	if strings.Contains(reason.Message, "deprecated") {
		t.Errorf("bhyve complained about the configuration: %s", reason.Message)
	}
	t.Logf("exit reason: %v", reason)

	// Killing twice is harmless.
	if err := vm.Kill(); err != nil {
		t.Errorf("Kill on an already dead VM = %v, want nil", err)
	}
}

// TestStopSignalsTheProcess covers Stop on a guest that has not enabled
// ACPI, where SIGTERM keeps its default disposition and simply kills
// bhyve. That is the documented behaviour and it is worth pinning down,
// because it is the case a caller is most likely to get wrong.
func TestStopSignalsTheProcess(t *testing.T) {
	harness.RequireHost(t)

	vm := harness.RequireRunningVM(t, 15902)

	reason, err := vm.Stop(ctx(t))
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// No guest has run, so nothing enabled ACPI, so SIGTERM killed
	// bhyve outright rather than pressing a power button.
	if reason.Kind != bhyve.ExitSignalled {
		t.Errorf("Kind = %v, want signalled for a guest that never enabled ACPI",
			reason.Kind)
	}
	t.Logf("exit reason: %v", reason)
}

// TestStopOnAnExitedVMReportsTheOldReason keeps the state machine from
// losing what already happened.
func TestStopOnAnExitedVMReportsTheOldReason(t *testing.T) {
	cfg := harness.Config(t)
	vm := harness.RequireVM(t, cfg)

	_ = vm.Start(ctx(t))
	first, err := vm.Wait(ctx(t))
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	second, err := vm.Stop(ctx(t))
	if err != nil {
		t.Fatalf("Stop on an exited VM: %v", err)
	}
	if second.Kind != first.Kind || second.Status != first.Status {
		t.Errorf("Stop reported %v, Wait had reported %v", second, first)
	}
}

// TestWaitIsRepeatableAndConcurrent is the contract several goroutines
// in a control plane will rely on.
func TestWaitIsRepeatableAndConcurrent(t *testing.T) {
	cfg := harness.Config(t)
	vm := harness.RequireVM(t, cfg)

	_ = vm.Start(ctx(t))

	const waiters = 8
	results := make(chan bhyve.ExitReason, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			reason, err := vm.Wait(ctx(t))
			if err != nil {
				t.Errorf("Wait: %v", err)
			}
			results <- reason
		}()
	}

	first := <-results
	for i := 1; i < waiters; i++ {
		got := <-results
		if got.Kind != first.Kind || got.Status != first.Status {
			t.Errorf("waiter %d saw %v, the first saw %v", i, got, first)
		}
	}
}

// TestStartCancellationKillsTheProcess checks the promise that a
// cancelled Start does not leave a process nobody is waiting for.
func TestStartCancellationKillsTheProcess(t *testing.T) {
	harness.RequireHost(t)
	// Needs firmware: a VM that dies on its own would reach the same
	// end state for the wrong reason.
	rom := harness.FirmwarePath(t)

	cfg := harness.Config(t)
	cfg.Firmware = &bhyve.Firmware{ROM: rom}
	// Hold the guest at the boot ROM until a viewer connects, so the
	// VM is reliably still starting when the context expires.
	cfg.Framebuffer = &bhyve.Framebuffer{Port: 15903, Wait: true}

	vm := harness.RequireVM(t, cfg)

	// Long enough for bhyve to launch, short enough to cancel while
	// it is still starting.
	c, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := vm.Start(c)
	if err == nil {
		// The VM came up inside the window, which is not what this
		// test is for but is not a failure either.
		t.Skip("the VM started before the context expired")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want a deadline error", err)
	}

	// Start must not return until the process is gone.
	if got := vm.State(); got != bhyve.StateExited {
		t.Errorf("State() = %v after a cancelled Start, want exited", got)
	}
	if got := vm.Pid(); got != 0 {
		t.Errorf("Pid() = %d after a cancelled Start, want 0", got)
	}
}

// TestValidateNeedsNoPrivilege is the claim that makes the configuration
// layer testable: bhyve parses and reports without creating anything.
func TestValidateNeedsNoPrivilege(t *testing.T) {
	cfg := harness.Config(t)

	resolved, err := cfg.Validate(ctx(t), bhyve.Tool{})
	if err != nil {
		if errors.Is(err, bhyve.ErrNotFound) {
			t.Skip("bhyve(8) is not installed")
		}
		t.Fatalf("Validate: %v", err)
	}
	if got, _ := resolved.Get("name"); got != cfg.Name {
		t.Errorf("resolved name = %q, want %q", got, cfg.Name)
	}

	// Nothing was created.
	if _, err := os.Stat("/dev/vmm/" + cfg.Name); err == nil {
		t.Error("Validate created a virtual machine")
	}
}
