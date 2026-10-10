//go:build freebsd

// Package harness supports the integration tests, which run real bhyve
// processes and therefore need root and vmm(4).
//
// Two things matter here. Tests skip with a reason when the host cannot
// run a VM, so the suite stays useful on a developer machine. And every
// VM a test creates is destroyed afterwards, including when the test
// fails: bhyve leaves the virtual machine in the kernel when it exits,
// so nothing cleans up by itself.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
)

// NamePrefix marks the VMs these tests create, so leftovers can be
// recognised and swept.
const NamePrefix = "gobhyve-test-"

// vmmDir is where vmm(4) publishes its virtual machines.
const vmmDir = "/dev/vmm"

// Timeout bounds any individual wait in these tests.
const Timeout = 30 * time.Second

// Name returns a VM name unique to this process and test.
func Name(t *testing.T) string {
	t.Helper()

	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("harness: reading random bytes: %v", err)
	}
	return fmt.Sprintf("%s%d-%08x", NamePrefix, os.Getpid(),
		binary.BigEndian.Uint32(buf[:]))
}

// RequireHost skips the test unless it can run bhyve at all.
func RequireHost(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skipf("harness: running bhyve needs privilege and this process is uid %d",
			os.Geteuid())
	}
	if _, err := os.Stat(bhyve.DefaultBhyvePath); err != nil {
		t.Skipf("harness: %s is not available: %v", bhyve.DefaultBhyvePath, err)
	}
}

// Config returns a minimal configuration with a unique name.
//
// It has no firmware, so a VM built from it reaches the kernel and then
// has nothing to boot. That is enough to exercise the lifecycle; a test
// that needs a guest to actually run has to supply firmware.
func Config(t *testing.T) bhyve.Config {
	t.Helper()

	return bhyve.Config{
		Name:       Name(t),
		CPU:        bhyve.CPU{Count: 1},
		Memory:     bhyve.Memory{Size: 256 << 20},
		ACPITables: true,
	}
}

// RequireVM builds a VM from cfg and arranges for it to be destroyed
// when the test ends.
func RequireVM(t *testing.T, cfg bhyve.Config) *bhyve.VM {
	t.Helper()

	RequireHost(t)

	vm, err := bhyve.New(cfg, bhyve.WithDir(t.TempDir()))
	if err != nil {
		t.Fatalf("harness: New(%q): %v", cfg.Name, err)
	}

	t.Cleanup(func() {
		// Order matters: the VM cannot be destroyed while bhyve
		// holds it.
		if err := vm.Kill(); err != nil {
			t.Errorf("harness: killing %q: %v", cfg.Name, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), Timeout)
		defer cancel()
		if _, err := vm.Wait(ctx); err != nil && !errors.Is(err, bhyve.ErrNotRunning) {
			t.Errorf("harness: waiting for %q: %v", cfg.Name, err)
		}
		if err := vm.Destroy(); err != nil {
			t.Errorf("harness: destroying %q: %v", cfg.Name, err)
		}
		if vm.Exists() {
			t.Errorf("harness: %q still exists after Destroy", cfg.Name)
		}
	})

	return vm
}

// Sweep destroys every VM left behind by these tests.
//
// It runs from TestMain, because a run interrupted part way through
// leaves virtual machines in the kernel that the next run would collide
// with.
func Sweep() {
	entries, err := os.ReadDir(vmmDir)
	if err != nil {
		// No /dev/vmm means there is nothing to clean up.
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, NamePrefix) {
			continue
		}
		if err := bhyve.DestroyByName(name); err != nil {
			fmt.Fprintf(os.Stderr, "harness: sweeping %s: %v\n", name, err)
		}
	}
}

// Run is the body of TestMain for every integration test package.
func Run(m *testing.M) int {
	Sweep()
	code := m.Run()
	Sweep()
	return code
}

// firmwareLocations are where a UEFI image for bhyve is conventionally
// installed. FreeBSD ships none in the base system; these come from the
// ports tree.
var firmwareLocations = []string{
	"/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
	"/usr/local/share/uefi-firmware/BHYVE_UEFI_CODE.fd",
	"/usr/local/share/edk2-bhyve/BHYVE_UEFI.fd",
}

// FirmwarePath returns an installed UEFI firmware image, skipping the
// test when there is none.
//
// Without firmware a VM cannot be kept alive: bhyve creates it, finds
// nothing to boot and exits straight away. Any test that needs a running
// VM therefore needs this, and there is no way around it from inside the
// test.
func FirmwarePath(t *testing.T) string {
	t.Helper()

	for _, path := range firmwareLocations {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	t.Skipf("harness: no UEFI firmware installed; tried %s. Install sysutils/edk2 (or edk2-bhyve) to run tests that need a live VM",
		strings.Join(firmwareLocations, ", "))
	return ""
}

// RequireRunningVM builds a VM that will stay up, and starts it.
//
// It needs firmware, so it skips on a host without any. The guest has
// nothing to boot from, so the firmware sits at its boot menu, which is
// exactly what a test supervising a live process wants.
func RequireRunningVM(t *testing.T, vncPort int) *bhyve.VM {
	t.Helper()

	RequireHost(t)
	rom := FirmwarePath(t)

	cfg := Config(t)
	cfg.Firmware = &bhyve.Firmware{ROM: rom}
	cfg.Framebuffer = &bhyve.Framebuffer{Port: vncPort}

	vm := RequireVM(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	if err := vm.Start(ctx); err != nil {
		t.Fatalf("harness: starting %q: %v", cfg.Name, err)
	}
	if got := vm.State(); got != bhyve.StateRunning {
		t.Fatalf("harness: %q is %v after Start, want running", cfg.Name, got)
	}

	return vm
}
