//go:build freebsd

// Command run boots a virtual machine and supervises it until it stops.
//
//	doas run -name web01 -disk /dev/zvol/zroot/vm/web01 -tap tap0
//	doas run -name inst01 -disk /vm/inst01.img -iso FreeBSD-15.1-RELEASE-amd64-disc1.iso -vnc 5900
//
// Ctrl-C asks the guest to shut down (an ACPI power button press), and a
// second Ctrl-C, or -grace running out, kills it. Either way the VM is
// destroyed afterwards, since bhyve leaves it in the kernel when it exits.
//
// Nothing here provisions anything: the disk, the ISO and the tap(4)
// interface must already exist. By default the guest console goes to an
// nmdm(4) pair, which needs the module loaded (kldload nmdm); attach to
// it with
//
//	cu -l /dev/nmdm-<name>-B
//
// -console stdio puts it on this terminal instead, and -console none
// leaves the guest without one.
//
// Needs root, vmm(4) and UEFI firmware (pkg install edk2-bhyve).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
)

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("run: ")

	name := flag.String("name", "example01", "VM name")
	cpus := flag.Int("cpus", 1, "number of vCPUs")
	memMB := flag.Uint64("mem", 512, "guest memory in MiB")
	rom := flag.String("rom", "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd", "UEFI firmware image")
	disk := flag.String("disk", "", "disk image or zvol (optional)")
	iso := flag.String("iso", "", "installer ISO to boot from (optional)")
	tap := flag.String("tap", "", "tap(4) interface for networking (optional)")
	vnc := flag.Int("vnc", 0, "VNC port for a framebuffer, 0 for none")
	console := flag.String("console", "nmdm", "guest console: nmdm, stdio or none")
	grace := flag.Duration("grace", 30*time.Second, "how long a shutdown may take before the VM is killed")
	flag.Parse()

	cfg := bhyve.Config{
		Name:        *name,
		CPU:         bhyve.CPU{Count: *cpus},
		Memory:      bhyve.Memory{Size: *memMB << 20, Wired: true},
		Firmware:    &bhyve.Firmware{ROM: *rom},
		ACPITables:  true,
		VMExitOnHLT: true,
	}
	switch *console {
	case "nmdm":
		cfg.Serials = []bhyve.Serial{{Port: bhyve.COM1, Backend: "/dev/nmdm-" + *name + "-A"}}
	case "stdio":
		cfg.Serials = []bhyve.Serial{{Port: bhyve.COM1, Backend: bhyve.StdioBackend}}
	case "none":
	default:
		log.Fatalf("-console must be nmdm, stdio or none, not %q", *console)
	}
	if *disk != "" {
		cfg.Disks = append(cfg.Disks, bhyve.Disk{Path: *disk, Model: bhyve.DiskVirtioBlk})
	}
	if *iso != "" {
		cfg.Disks = append(cfg.Disks, bhyve.Disk{Path: *iso, Model: bhyve.DiskAHCICD, ReadOnly: true})
	}
	if *tap != "" {
		cfg.Networks = append(cfg.Networks, bhyve.Network{Backend: *tap, Model: bhyve.NetVirtio})
	}
	if *vnc != 0 {
		cfg.Framebuffer = &bhyve.Framebuffer{Port: *vnc}
	}

	// New checks the configuration; nothing is started yet.
	vm, err := bhyve.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	// bhyve leaves the VM in the kernel when it exits, so destroying
	// it is part of every way out of this program.
	defer func() {
		if err := vm.Destroy(); err != nil {
			log.Printf("destroy: %v", err)
			return
		}
		log.Printf("%s destroyed", vm.Name())
	}()

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = vm.Start(startCtx)
	cancel()
	if err != nil {
		// Start returns once bhyve has either created the VM or
		// given up. The reason says which, in bhyve's own words.
		if reason, ok := vm.Reason(); ok {
			log.Printf("bhyve exited during startup: %v", reason)
		}
		log.Print(err)
		return
	}
	log.Printf("%s is %v, bhyve pid %d", vm.Name(), vm.State(), vm.Pid())
	if *console == "nmdm" {
		log.Printf("console: cu -l /dev/nmdm-%s-B", vm.Name())
	}
	if *vnc != 0 {
		log.Printf("display: vnc://localhost:%d", *vnc)
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	exited := make(chan bhyve.ExitReason, 1)
	go func() {
		reason, err := vm.Wait(context.Background())
		if err != nil {
			log.Printf("wait: %v", err)
		}
		exited <- reason
	}()

	var reason bhyve.ExitReason
	select {
	case reason = <-exited:
	case sig := <-signals:
		log.Printf("%v: asking the guest to shut down (up to %v)", sig, *grace)
		reason = shutdown(vm, signals, *grace)
	}

	describe(reason)
}

// shutdown presses the guest's power button and waits, killing the VM if
// the grace period runs out or a second signal arrives.
func shutdown(vm *bhyve.VM, signals <-chan os.Signal, grace time.Duration) bhyve.ExitReason {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	go func() {
		select {
		case <-signals:
			log.Print("second signal: killing the VM")
			cancel()
		case <-ctx.Done():
		}
	}()

	// When ctx ends first, Stop kills bhyve and reports that.
	reason, err := vm.Stop(ctx)
	if err != nil {
		log.Printf("stop: %v", err)
	}
	return reason
}

// describe says what the exit reason means for whoever runs this.
func describe(reason bhyve.ExitReason) {
	log.Printf("bhyve exited: %v", reason)

	switch {
	case reason.Kind == bhyve.ExitSignalled:
		// SIGTERM is only a power button once the guest has enabled
		// ACPI; before that it kills bhyve outright.
		fmt.Println("the guest was cut off rather than shut down; check its filesystems")
	case reason.Kind.GuestRequested():
		fmt.Printf("the guest %v by itself\n", reason.Kind)
	case strings.Contains(reason.Message, "nmdm"):
		// bhyve creates the VM before it opens device backends, so
		// this one fails after Start has already succeeded.
		fmt.Println("bhyve could not open the console; run kldload nmdm, or use -console stdio")
	case !reason.Started:
		fmt.Println("the VM never ran; bhyve's message above says why")
	default:
		fmt.Println("the VM stopped abnormally")
	}
}
