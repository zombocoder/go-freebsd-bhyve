//go:build freebsd && amd64

// Command inspect shows the two libraries working together: bhyve runs
// the VM, and go-freebsd-vmm reaches into it while it runs.
//
//	doas inspect
//	doas inspect -name web01 -cpus 2 -samples 5
//
// It boots a VM with only UEFI firmware, which runs and then waits at its
// boot menu, opens the same VM through vmm(4), and samples each vCPU's
// instruction pointer and exit counters while the guest executes. Then it
// shuts the VM down and destroys it.
//
// This is the part bhyvectl is used for today. Nothing here runs it:
// opening a VM that bhyve created is an ordinary vmm.Open by name.
//
// Needs root, vmm(4) and UEFI firmware (pkg install edk2-bhyve).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
	vmm "github.com/zombocoder/go-freebsd-vmm"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("inspect: ")

	name := flag.String("name", "inspect01", "VM name")
	cpus := flag.Int("cpus", 2, "number of vCPUs")
	rom := flag.String("rom", "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd", "UEFI firmware image")
	samples := flag.Int("samples", 3, "how many times to sample")
	interval := flag.Duration("interval", time.Second, "time between samples")
	flag.Parse()

	cfg := bhyve.Config{
		Name:       *name,
		CPU:        bhyve.CPU{Count: *cpus},
		Memory:     bhyve.Memory{Size: 256 << 20},
		Firmware:   &bhyve.Firmware{ROM: *rom},
		ACPITables: true,
	}

	vm, err := bhyve.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer vm.Destroy()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := vm.Start(ctx); err != nil {
		if reason, ok := vm.Reason(); ok {
			log.Printf("bhyve exited: %v", reason)
		}
		log.Fatal(err)
	}
	defer func() {
		reason, err := vm.Stop(ctx)
		if err != nil {
			log.Printf("stop: %v", err)
			return
		}
		fmt.Printf("\nstopped: %v\n", reason)
	}()
	fmt.Printf("bhyve pid %d is running %s\n", vm.Pid(), vm.Name())

	// The same VM, seen from the kernel side.
	handle, err := vmm.Open(vm.Name())
	if err != nil {
		log.Fatal(err)
	}
	defer handle.Close()

	topo, err := handle.Topology()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("topology: %d socket(s) x %d core(s) x %d thread(s)\n",
		topo.Sockets, topo.Cores, topo.Threads)

	var cpusOpen []*vmm.VCPU
	for id := 0; id < *cpus; id++ {
		cpu, err := handle.VCPU(id)
		if err != nil {
			log.Fatal(err)
		}
		defer cpu.Close()
		cpusOpen = append(cpusOpen, cpu)
	}

	regs := []vmm.Register{vmm.RegRIP, vmm.RegCR0, vmm.RegRFLAGS}

	for i := 1; i <= *samples; i++ {
		time.Sleep(*interval)

		active, err := handle.ActiveCPUs()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("\nsample %d: active vCPUs %v\n", i, active)

		for id, cpu := range cpusOpen {
			// One kernel call for the whole set.
			values, err := cpu.Registers(regs)
			if err != nil {
				log.Printf("vcpu %d registers: %v", id, err)
				continue
			}
			var b strings.Builder
			for _, v := range values {
				fmt.Fprintf(&b, " %v=%#x", v.Register, v.Value)
			}

			exits, err := totalExits(cpu)
			if err != nil {
				log.Printf("vcpu %d stats: %v", id, err)
				continue
			}
			// Only the boot processor runs firmware. The others stay
			// in their reset state, all zeros, until a guest operating
			// system starts them.
			fmt.Printf("  vcpu %d:%s  exits=%d\n", id, b.String(), exits)
		}
	}
}

// totalExits sums the counters that count VM exits. Counter names come
// from the kernel and differ between Intel and AMD, so they are matched
// loosely rather than by index.
func totalExits(cpu *vmm.VCPU) (uint64, error) {
	stats, err := cpu.Stats()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, s := range stats {
		if strings.Contains(s.Description, "vm exits") {
			total += s.Value
		}
	}
	return total, nil
}
