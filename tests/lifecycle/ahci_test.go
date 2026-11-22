//go:build freebsd

package lifecycle_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
	"github.com/zombocoder/go-freebsd-bhyve/tests/harness"
)

// TestAHCIDisksStart boots a VM with each SATA disk model and checks
// that bhyve accepts the device.
//
// bhyve reads a configuration file differently from its command line.
// "-s 4,ahci-cd,/path" is translated by the command-line parser into an
// "ahci" device with a port node; a configuration file saying
// device=ahci-cd is refused by FreeBSD 15 as a legacy name, and bhyve
// exits 4 before the guest runs. config.dump cannot see this, because
// it only reports what was parsed, so it takes a real start.
func TestAHCIDisksStart(t *testing.T) {
	harness.RequireHost(t)
	rom := harness.FirmwarePath(t)

	dir := t.TempDir()

	// An empty disk for the guest's main drive, as an installer would
	// see it.
	img := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatalf("creating disk image: %v", err)
	}
	if err := os.Truncate(img, 16<<20); err != nil {
		t.Fatalf("sizing disk image: %v", err)
	}

	iso := makeISO(t, dir)

	cases := []struct {
		name    string
		disk    bhyve.Disk
		vncPort int
	}{
		{
			name:    "cd",
			disk:    bhyve.Disk{Path: iso, Model: bhyve.DiskAHCICD, ReadOnly: true},
			vncPort: 15905,
		},
		{
			name:    "hd",
			disk:    bhyve.Disk{Path: img, Model: bhyve.DiskAHCIHD},
			vncPort: 15906,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := harness.Config(t)
			cfg.Firmware = &bhyve.Firmware{ROM: rom}
			cfg.Framebuffer = &bhyve.Framebuffer{Port: tc.vncPort}
			cfg.Disks = []bhyve.Disk{tc.disk}
			if tc.disk.Model == bhyve.DiskAHCICD {
				// The usual installer layout: a blank drive and
				// the ISO beside it.
				cfg.Disks = []bhyve.Disk{{Path: img}, tc.disk}
			}

			vm := harness.RequireVM(t, cfg)

			if err := vm.Start(ctx(t)); err != nil {
				t.Fatalf("Start with an AHCI %s disk: %v", tc.name, err)
			}
			if got := vm.State(); got != bhyve.StateRunning {
				t.Fatalf("State() after Start = %v, want running", got)
			}

			// And it stays up: the firmware is probing the drive,
			// not bhyve tearing down after a late device error.
			time.Sleep(time.Second)
			if got := vm.State(); got != bhyve.StateRunning {
				reason, _ := vm.Wait(ctx(t))
				t.Fatalf("State() a second after Start = %v, want running (exit: %v)", got, reason)
			}
		})
	}
}

// makeISO builds a small ISO 9660 image in dir with makefs(8).
func makeISO(t *testing.T, dir string) string {
	t.Helper()

	makefs, err := exec.LookPath("makefs")
	if err != nil {
		t.Skipf("makefs(8) is not available: %v", err)
	}

	src := filepath.Join(dir, "iso-root")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("creating ISO source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("gobhyve test\n"), 0o644); err != nil {
		t.Fatalf("writing ISO content: %v", err)
	}

	iso := filepath.Join(dir, "test.iso")
	out, err := exec.Command(makefs, "-t", "cd9660", iso, src).CombinedOutput()
	if err != nil {
		t.Fatalf("makefs: %v\n%s", err, out)
	}
	return iso
}
