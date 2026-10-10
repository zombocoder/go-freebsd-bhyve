//go:build freebsd

// Package bhyve runs and supervises FreeBSD virtual machines.
//
// It sits on top of go-freebsd-vmm, which binds vmm(4) through
// libvmmapi. That library can inspect and control a virtual machine but
// cannot boot one, because device emulation lives in bhyve's userspace
// process. This package supplies the missing half: it describes a VM,
// runs bhyve(8) to execute it, supervises that process, and uses
// go-freebsd-vmm to reach inside the result.
//
//	application
//	    |
//	    v
//	go-freebsd-bhyve     configuration, process lifecycle
//	    |
//	    v
//	bhyve(8)             device models, boot ROM
//	    |
//	    v
//	go-freebsd-vmm       runtime inspection and control
//	    |
//	    v
//	libvmmapi / vmm(4)
//
// # Scope
//
// This package knows about bhyve: its configuration variables, its
// command line, and what its exit statuses mean. It does not know about
// the resources a VM uses.
//
// Disk paths must already exist and network backends must already be
// configured. Nothing here creates a ZFS volume, a disk image, a tap
// interface or a bridge, and nothing here removes one or writes a
// firewall rule. Storage, networking and packet filtering each have
// their own library; composing them is the job of whatever sits above
// this one.
//
// That boundary is not fastidiousness. A VM runner that provisioned its
// own storage could not be used with storage it did not provision, and
// could not be tested on a host without it.
//
// # Running a VM
//
//	cfg := bhyve.Config{
//		Name:   "web01",
//		CPU:    bhyve.CPU{Count: 2},
//		Memory: bhyve.Memory{Size: 2 << 30, Wired: true},
//		Firmware: &bhyve.Firmware{
//			ROM: "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
//		},
//		Disks: []bhyve.Disk{
//			{Path: "/dev/zvol/zroot/vm/web01", Model: bhyve.DiskVirtioBlk},
//		},
//		Networks:   []bhyve.Network{{Backend: "tap0"}},
//		Serials:    []bhyve.Serial{{Port: bhyve.COM1, Backend: "/dev/nmdm-web01-A"}},
//		ACPITables: true,
//	}
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
//
//	reason, err := vm.Wait(ctx)
//	if err != nil {
//		return err
//	}
//	if reason.Kind == bhyve.ExitRebooted {
//		// the guest asked to restart
//	}
//
// # Three things about bhyve worth knowing
//
// These are not incidental details; each one shapes how this package
// has to be used.
//
// A VM outlives the process that ran it. bhyve leaves the virtual
// machine in the kernel when it exits, so Destroy is a required step
// rather than a tidy-up: until it runs, the VM holds its name and its
// guest memory. Start refuses to run over a leftover, because reusing
// one would hand the previous guest's state to the new one.
//
// Exit status 1 is ambiguous. bhyve uses it both for a guest powering
// itself off and for most of its own startup failures, with nothing to
// separate them. ExitReason therefore carries Started, established from
// the device node rather than from the status, and the two cases decode
// to ExitPoweredOff and ExitStartupFailed accordingly.
//
// A graceful stop is not always graceful. Stop sends SIGTERM, which
// bhyve turns into an ACPI power button press, but only once the guest
// has enabled ACPI. Before that, and for any guest that never enables
// it, SIGTERM kills bhyve outright. The reason Stop returns says which
// happened, and the difference matters to the guest's filesystems.
//
// # Validation
//
// Config.Check decides what it can locally. Config.Validate asks bhyve,
// which parses the generated configuration with its own parser and
// reports every variable including its own defaults; it needs no
// privilege and starts no VM. Neither is complete, and the limits are
// documented on each: in particular bhyve cannot be asked which variable
// names it understands, and it does not check whether a configuration
// means anything until it runs.
//
// # Portability
//
// FreeBSD only, since bhyve is. The package builds on any architecture
// FreeBSD supports, but the device models and the configuration this
// package generates are amd64's.
package bhyve
