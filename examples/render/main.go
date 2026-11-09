//go:build freebsd

// Command render shows the configuration layer without running anything:
// it describes a VM, checks it, renders the bhyve_config(5) file bhyve
// would read, reports where each device lands on the guest's PCI bus, and
// asks bhyve itself to parse the result.
//
//	render
//	render -name db01 -cpus 4 -mem 8G -disk /dev/zvol/zroot/vm/db01 -tap tap1
//
// It needs no privilege and creates nothing, which makes it a safe way to
// see what a Config turns into before handing it to New.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("render: ")

	name := flag.String("name", "example01", "VM name")
	cpus := flag.Int("cpus", 2, "number of vCPUs")
	mem := flag.String("mem", "2G", "guest memory, e.g. 512M or 4G")
	rom := flag.String("rom", "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd", "UEFI firmware image")
	disk := flag.String("disk", "", "disk image or zvol (need not exist); default /dev/zvol/zroot/vm/<name>")
	iso := flag.String("iso", "", "installer ISO to attach as a CD (optional)")
	tap := flag.String("tap", "tap0", "network backend (need not exist)")
	flag.Parse()

	if *disk == "" {
		*disk = "/dev/zvol/zroot/vm/" + *name
	}

	size, err := parseSize(*mem)
	if err != nil {
		log.Fatalf("-mem: %v", err)
	}

	cfg := bhyve.Config{
		Name:   *name,
		CPU:    bhyve.CPU{Count: *cpus},
		Memory: bhyve.Memory{Size: size},
		Firmware: &bhyve.Firmware{
			ROM: *rom,
		},
		Disks: []bhyve.Disk{
			{Path: *disk, Model: bhyve.DiskVirtioBlk},
		},
		Networks: []bhyve.Network{
			{Backend: *tap, Model: bhyve.NetVirtio},
		},
		Serials: []bhyve.Serial{
			{Port: bhyve.COM1, Backend: "/dev/nmdm-" + *name + "-A"},
		},
		Framebuffer: &bhyve.Framebuffer{Port: 5900},
		ACPITables:  true,
		VMExitOnHLT: true,
	}
	if *iso != "" {
		cfg.Disks = append(cfg.Disks, bhyve.Disk{
			Path:     *iso,
			Model:    bhyve.DiskAHCICD,
			ReadOnly: true,
		})
	}

	// Check decides what can be decided locally: names, counts, sizes,
	// slot exhaustion, values that would corrupt the file.
	if err := cfg.Check(); err != nil {
		log.Fatal(err)
	}

	text, err := cfg.Render()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("== bhyve_config(5) ==")
	fmt.Print(text)

	// The guest enumerates devices in bus order, so this is what
	// decides whether a disk is vtbd0 or vtbd1 inside it.
	fmt.Println("\n== PCI layout ==")
	for i, d := range cfg.Disks {
		printSlot(&cfg, "disk", i, fmt.Sprintf("%s %s", d.Model, d.Path))
	}
	for i, n := range cfg.Networks {
		printSlot(&cfg, "network", i, fmt.Sprintf("%s %s", n.Model, n.Backend))
	}
	printSlot(&cfg, "framebuffer", 0, "VNC framebuffer")

	// Validate hands the file to bhyve's own parser. It needs no
	// privilege and starts no VM, but it cannot tell a misspelled
	// variable from a real one, nor a topology that makes no sense.
	fmt.Println("\n== bhyve's view ==")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resolved, err := cfg.Validate(ctx, bhyve.Tool{})
	if errors.Is(err, bhyve.ErrNotFound) {
		fmt.Println("bhyve(8) is not installed; skipping")
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bhyve accepted the file and resolved %d variables, defaults included:\n",
		len(resolved.Keys()))
	for _, key := range resolved.Keys() {
		value, _ := resolved.Get(key)
		fmt.Printf("  %s=%s\n", key, value)
	}
}

func printSlot(cfg *bhyve.Config, kind string, index int, what string) {
	addr, err := cfg.PCIAddress(kind, index)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  %d:%d:%d  %s\n", addr.Bus, addr.Slot, addr.Function, what)
}

// parseSize reads sizes such as 512M or 4G.
func parseSize(s string) (uint64, error) {
	var n uint64
	var unit string
	if _, err := fmt.Sscanf(s, "%d%s", &n, &unit); err != nil {
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return 0, fmt.Errorf("%q is not a size", s)
		}
	}
	switch unit {
	case "", "B":
		return n, nil
	case "K":
		return n << 10, nil
	case "M":
		return n << 20, nil
	case "G":
		return n << 30, nil
	default:
		return 0, fmt.Errorf("%q: unit must be K, M or G", s)
	}
}
