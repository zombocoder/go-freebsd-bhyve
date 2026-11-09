//go:build freebsd

// Command supervise keeps a virtual machine running, with the restart
// policy written out in the open.
//
//	doas supervise -name web01 -disk /dev/zvol/zroot/vm/web01 -tap tap0
//
// The library decodes why bhyve exited; whether to start it again is the
// caller's decision, and this is one reasonable answer:
//
//   - the guest rebooted itself: start it again straight away;
//   - the guest powered off or halted: it meant to, so stop supervising;
//   - anything else (a crash, a triple fault, bhyve failing): start it
//     again after a back-off, and give up after -max-failures in a row.
//
// Each run is a fresh bhyve process on a freshly destroyed VM, because
// Start refuses to reuse a VM left in the kernel by the previous run.
//
// The guest console goes to an nmdm(4) pair by default, which needs
// kldload nmdm; -console none runs without one.
//
// Needs root, vmm(4) and UEFI firmware (pkg install edk2-bhyve).
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
)

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("supervise: ")

	name := flag.String("name", "example01", "VM name")
	memMB := flag.Uint64("mem", 512, "guest memory in MiB")
	rom := flag.String("rom", "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd", "UEFI firmware image")
	disk := flag.String("disk", "", "disk image or zvol (optional)")
	tap := flag.String("tap", "", "tap(4) interface (optional)")
	maxFailures := flag.Int("max-failures", 3, "consecutive failures before giving up")
	grace := flag.Duration("grace", 30*time.Second, "how long a shutdown may take on Ctrl-C")
	console := flag.String("console", "nmdm", "guest console: nmdm or none")
	flag.Parse()

	cfg := bhyve.Config{
		Name:        *name,
		CPU:         bhyve.CPU{Count: 1},
		Memory:      bhyve.Memory{Size: *memMB << 20},
		Firmware:    &bhyve.Firmware{ROM: *rom},
		ACPITables:  true,
		VMExitOnHLT: true,
	}
	switch *console {
	case "nmdm":
		cfg.Serials = []bhyve.Serial{{Port: bhyve.COM1, Backend: "/dev/nmdm-" + *name + "-A"}}
	case "none":
	default:
		log.Fatalf("-console must be nmdm or none, not %q", *console)
	}
	if *disk != "" {
		cfg.Disks = []bhyve.Disk{{Path: *disk, Model: bhyve.DiskVirtioBlk}}
	}
	if *tap != "" {
		cfg.Networks = []bhyve.Network{{Backend: *tap, Model: bhyve.NetVirtio}}
	}

	// Cancelled on Ctrl-C or SIGTERM: the end of supervision.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A leftover from an earlier, interrupted run would make Start
	// refuse. Only do this for a name this program owns.
	if err := bhyve.DestroyByName(cfg.Name); err != nil {
		log.Fatalf("clearing a leftover VM: %v", err)
	}

	failures := 0
	for run := 1; ; run++ {
		reason, err := runOnce(ctx, cfg, *grace)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("run %d ended: %v", run, reason)

		if ctx.Err() != nil {
			log.Print("supervision stopped")
			return
		}

		switch {
		case reason.Kind == bhyve.ExitRebooted:
			// A reset is the guest asking to start again.
			failures = 0
			continue

		case reason.Kind.GuestRequested():
			// Powered off or halted: the guest's own decision.
			log.Printf("the guest %v; not restarting", reason.Kind)
			return

		default:
			failures++
			if failures >= *maxFailures {
				log.Fatalf("%d failures in a row; giving up", failures)
			}
			backoff := time.Duration(1<<(failures-1)) * time.Second
			log.Printf("failure %d of %d; restarting in %v", failures, *maxFailures, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				log.Print("supervision stopped")
				return
			}
		}
	}
}

// runOnce starts the VM, waits for it to stop or for ctx to end, and
// destroys it. Its error is for problems the supervisor cannot recover
// from; a VM that failed to start is reported through the reason.
func runOnce(ctx context.Context, cfg bhyve.Config, grace time.Duration) (bhyve.ExitReason, error) {
	vm, err := bhyve.New(cfg)
	if err != nil {
		return bhyve.ExitReason{}, err
	}
	defer func() {
		if err := vm.Destroy(); err != nil {
			log.Printf("destroy: %v", err)
		}
	}()

	if err := vm.Start(ctx); err != nil {
		if reason, ok := vm.Reason(); ok {
			// bhyve ran and gave up; that is a failed run, not a
			// reason to stop supervising.
			return reason, nil
		}
		return bhyve.ExitReason{}, err
	}
	log.Printf("%s running, bhyve pid %d", vm.Name(), vm.Pid())

	reason, err := vm.Wait(ctx)
	if err == nil {
		return reason, nil
	}

	// ctx ended while the guest was running: shut it down properly.
	stopCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return vm.Stop(stopCtx)
}
