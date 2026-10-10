//go:build freebsd

package bhyve

import (
	"fmt"
	"strings"

	vmm "github.com/zombocoder/go-freebsd-vmm"
)

// Configuration variable names, from bhyve_config(5).
//
// These are constants rather than strings built at call sites on
// purpose. bhyve ignores configuration variables it does not recognise
// and says nothing about them, so a typo in a key name does not fail:
// it silently leaves the setting at its default. Keeping every name here
// means a typo is a compile error instead.
const (
	keyName              = "name"
	keyCPUs              = "cpus"
	keySockets           = "sockets"
	keyCores             = "cores"
	keyThreads           = "threads"
	keyMemorySize        = "memory.size"
	keyMemoryWired       = "memory.wired"
	keyMemoryGuestInCore = "memory.guest_in_core"
	keyACPITables        = "acpi_tables"
	keyDestroyOnPowerOff = "destroy_on_poweroff"
	keyUUID              = "uuid"
	keyRTCUseLocalTime   = "rtc.use_localtime"
	keyX2APIC            = "x86.x2apic"
	keyVMExitOnHLT       = "x86.vmexit_on_hlt"
	keyVMExitOnPause     = "x86.vmexit_on_pause"
	keyConfigDump        = "config.dump"

	keyLPCBootROM  = "lpc.bootrom"
	keyLPCBootVars = "lpc.bootvars"
)

// Config describes a virtual machine.
//
// # What this does not cover
//
// Everything here is something bhyve(8) itself understands. Provisioning
// is not: Disk.Path must already exist, and Network.Backend must already
// be configured on the host. This package creates no storage and no
// network interfaces, and removes none either.
//
// That boundary is deliberate. Storage belongs to a ZFS or file layer,
// interfaces to a networking layer, and firewall rules to a packet
// filter layer; a VM runner that reached into all three could not be
// used with anything it did not provision itself.
type Config struct {
	// Name is the VM name, which becomes /dev/vmm/<name>.
	Name string

	CPU    CPU
	Memory Memory

	// Firmware is the boot ROM to run. A VM without one has nothing
	// to execute: bhyve reports "virtual machine cannot be booted"
	// and exits.
	Firmware *Firmware

	Disks    []Disk
	Networks []Network
	Serials  []Serial

	// Passthrough lists host PCI devices to hand to the guest. Each
	// must already be attached to ppt(4) on the host.
	Passthrough []vmm.PCIDevice

	// Framebuffer attaches a VNC-backed display, for guests that
	// want one during installation.
	Framebuffer *Framebuffer

	// ACPITables writes ACPI tables for the guest. Modern guests
	// expect them.
	ACPITables bool

	// RTCUseLocalTime presents the host's local time to the guest
	// rather than UTC, which is what a Windows guest expects.
	RTCUseLocalTime bool

	// X2APIC exposes the x2APIC interface to the guest.
	X2APIC bool

	// VMExitOnHLT makes the guest leave to the VMM when it halts,
	// instead of idling inside the guest. An idle guest without this
	// keeps a host CPU busy.
	VMExitOnHLT bool

	// VMExitOnPause makes the guest leave to the VMM on a PAUSE
	// instruction, which helps a guest spinning on a lock give the
	// host its CPU back.
	VMExitOnPause bool

	// UUID is the SMBIOS system UUID. Empty lets bhyve derive one
	// from the VM name.
	UUID string

	// Extra sets configuration variables directly, for settings this
	// package does not model.
	//
	// This is an escape hatch and it is sharp: bhyve accepts unknown
	// names in silence, so a misspelled key here does nothing at all
	// and reports nothing. Validate catches it by checking that every
	// name reached bhyve, which is the only protection available.
	Extra map[string]string
}

// CPU describes the guest's processors.
//
// Count is how many vCPUs the guest gets. The topology fields describe
// how they are presented; leave them zero to let bhyve arrange them.
//
// Note that the number of vCPUs a VM can have is capped by the host:
// vmm(4) fixes it at VM creation from the hw.vmm.maxcpu tunable, which
// defaults to the host's CPU count.
type CPU struct {
	Count   int
	Sockets int
	Cores   int
	Threads int
}

// topology reports whether the arrangement fields are set.
func (c CPU) topology() bool {
	return c.Sockets != 0 || c.Cores != 0 || c.Threads != 0
}

// validate checks the processor configuration.
func (c CPU) validate() error {
	if c.Count < 1 {
		return configErrorf("cpu count %d is less than 1", c.Count)
	}
	if !c.topology() {
		return nil
	}

	if c.Sockets < 1 || c.Cores < 1 || c.Threads < 1 {
		return configErrorf("cpu topology %dx%dx%d has a zero dimension",
			c.Sockets, c.Cores, c.Threads)
	}
	if got := c.Sockets * c.Cores * c.Threads; got != c.Count {
		return configErrorf("cpu topology %dx%dx%d describes %d cpus but count is %d",
			c.Sockets, c.Cores, c.Threads, got, c.Count)
	}
	return nil
}

// Memory describes the guest's memory.
type Memory struct {
	// Size is the amount of guest memory in bytes.
	Size uint64

	// Wired keeps guest memory resident, never paged out. A VM that
	// is going to run rather than sit idle usually wants this.
	Wired bool

	// InCore includes guest memory in a core dump of the bhyve
	// process, which makes those dumps enormous.
	InCore bool
}

// Minimum guest memory. Below this a guest cannot get far enough to
// report what went wrong.
const minMemory = 16 << 20

// validate checks the memory configuration.
func (m Memory) validate() error {
	if m.Size < minMemory {
		return configErrorf("memory size %d is below the %d byte minimum",
			m.Size, minMemory)
	}
	return nil
}

// render formats a byte count the way bhyve's own parser expects, using
// the largest suffix that divides it exactly.
func (m Memory) render() string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)

	switch {
	case m.Size%tb == 0:
		return fmt.Sprintf("%dT", m.Size/tb)
	case m.Size%gb == 0:
		return fmt.Sprintf("%dG", m.Size/gb)
	case m.Size%mb == 0:
		return fmt.Sprintf("%dM", m.Size/mb)
	case m.Size%kb == 0:
		return fmt.Sprintf("%dK", m.Size/kb)
	default:
		return fmt.Sprintf("%d", m.Size)
	}
}

// Firmware is the boot ROM the guest starts from.
//
// This is the UEFI path: the ROM is copied into guest memory below 4 GB
// and finds its own way to a bootable disk. FreeBSD ships no firmware
// image in the base system; the usual source is the sysutils/edk2 port,
// which installs BHYVE_UEFI.fd.
//
// The other way to boot a guest, bhyveload(8), runs a second process
// before bhyve and works only for FreeBSD guests. It is not modelled
// here.
type Firmware struct {
	// ROM is the path to the firmware image.
	ROM string

	// Vars is an optional writable copy of the firmware's variable
	// store, which is where a guest keeps its boot entries. Without
	// one, changes the guest makes are lost on restart.
	Vars string
}

// validate checks the firmware configuration.
func (f *Firmware) validate() error {
	if f == nil {
		return nil
	}
	if f.ROM == "" {
		return configErrorf("firmware has no ROM path")
	}
	if err := checkConfigValue("firmware ROM path", f.ROM); err != nil {
		return err
	}
	if f.Vars != "" {
		return checkConfigValue("firmware vars path", f.Vars)
	}
	return nil
}

// Check validates the configuration locally, without running anything.
//
// It covers required fields, value syntax, and conflicts between
// devices. Whether bhyve agrees is a separate question, answered by
// Config.Validate, which asks bhyve itself.
func (c *Config) Check() error {
	if err := validateName(c.Name); err != nil {
		return err
	}
	if err := c.CPU.validate(); err != nil {
		return err
	}
	if err := c.Memory.validate(); err != nil {
		return err
	}
	if err := c.Firmware.validate(); err != nil {
		return err
	}

	for i := range c.Disks {
		if err := c.Disks[i].validate(i); err != nil {
			return err
		}
	}
	for i := range c.Networks {
		if err := c.Networks[i].validate(i); err != nil {
			return err
		}
	}
	for i := range c.Serials {
		if err := c.Serials[i].validate(i); err != nil {
			return err
		}
	}
	for i, dev := range c.Passthrough {
		if dev.Slot > vmm.MaxPCISlot || dev.Function > vmm.MaxPCIFunction {
			return configErrorf("passthrough device %d (%v) is not a valid PCI address",
				i, dev)
		}
	}
	if err := c.Framebuffer.validate(); err != nil {
		return err
	}

	managed, err := c.managedKeys()
	if err != nil {
		return err
	}
	for key, value := range c.Extra {
		if err := checkConfigKey(key); err != nil {
			return err
		}
		if err := checkConfigValue("value of "+key, value); err != nil {
			return err
		}
		// A silent override of something this package manages is
		// worse than an error: destroy_on_poweroff is the clearest
		// case, where turning it on would let bhyve destroy the VM
		// out from under a Runtime holding it open.
		if managed[key] {
			return configErrorf("Extra sets %q, which this package manages itself",
				key)
		}
	}

	// Devices have to fit on the bus, which is where a configuration
	// with too many of them fails.
	if _, err := c.assign(); err != nil {
		return err
	}

	return nil
}

// validateName checks a VM name.
//
// The length limit is the one vmm(4) enforces, since the name becomes a
// device node. The rest guards the two things that would otherwise go
// wrong quietly: a name that cannot survive the configuration file
// format, and one that would make libvmmapi open a different device.
func validateName(name string) error {
	switch {
	case name == "":
		return configErrorf("VM name is empty")

	case len(name) > vmm.MaxNameLen:
		return configErrorf("VM name %q is %d bytes, limit is %d",
			name, len(name), vmm.MaxNameLen)

	case strings.ContainsRune(name, '/'):
		return configErrorf("VM name %q contains a path separator", name)

	case name == "." || name == "..":
		return configErrorf("VM name %q is a directory reference", name)
	}

	return checkConfigValue("VM name", name)
}

// checkConfigKey rejects a configuration variable name that the file
// format cannot carry.
func checkConfigKey(key string) error {
	switch {
	case key == "":
		return configErrorf("configuration key is empty")
	case strings.ContainsRune(key, '='):
		return configErrorf("configuration key %q contains '='", key)
	case strings.ContainsRune(key, '#'):
		return configErrorf("configuration key %q contains '#'", key)
	}
	return checkConfigValue("configuration key "+key, key)
}

// checkConfigValue rejects a value that the configuration file format
// cannot carry.
//
// The format is one key=value per line, so a newline would silently
// split a setting in two, and the second half would be parsed as its own
// line. A NUL would truncate it on the way through exec.
func checkConfigValue(what, value string) error {
	if i := strings.IndexAny(value, "\n\r"); i >= 0 {
		return configErrorf("%s contains a newline at byte %d", what, i)
	}
	if i := strings.IndexByte(value, 0); i >= 0 {
		return configErrorf("%s contains NUL at byte %d", what, i)
	}
	return nil
}
