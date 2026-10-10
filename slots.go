//go:build freebsd

package bhyve

import (
	"fmt"

	vmm "github.com/zombocoder/go-freebsd-vmm"
)

// Fixed positions on the guest's PCI bus.
//
// Two devices are not free to move. The host bridge belongs at 0:0:0,
// where every guest looks for it. The LPC bridge carries the boot ROM
// and the serial ports and must be on bus 0; slot 31 is the convention
// bhyve's own documentation and every example uses, and putting it
// elsewhere confuses guests that expect the legacy bridge at the top of
// the bus.
const (
	hostBridgeSlot = 0
	lpcSlot        = 31

	// firstFreeSlot is where allocation begins. Slots 1 and 2 are
	// left alone because guests and firmware have historically
	// assumed things about them.
	firstFreeSlot = 3
)

// slot is a position on the guest's PCI bus.
//
// Only bus 0 is used: bhyve supports more, but a VM that needs a second
// PCI bus is past what this package models.
type slot struct {
	bus      int
	slot     int
	function int
}

// String renders the slot as bhyve's configuration path fragment.
func (s slot) String() string {
	return fmt.Sprintf("%d.%d.%d", s.bus, s.slot, s.function)
}

// assignment is the result of laying devices out on the bus.
//
// Everything that renders a device node reads its position from here,
// so the layout is decided once and in one place.
type assignment struct {
	hostBridge slot
	lpc        slot

	disks       []slot
	networks    []slot
	passthrough []slot
	framebuffer *slot
}

// assign lays the configuration's devices out on the guest's PCI bus.
//
// The order is stable: disks, then networks, then passthrough devices,
// then the framebuffer, each in the order given. That matters because a
// guest names its disks and interfaces by the order it finds them, so a
// layout that shifted between runs would rename the guest's devices
// underneath it.
func (c *Config) assign() (*assignment, error) {
	a := &assignment{
		hostBridge: slot{bus: 0, slot: hostBridgeSlot},
		lpc:        slot{bus: 0, slot: lpcSlot},
	}

	next := firstFreeSlot
	take := func(what string) (slot, error) {
		for next < lpcSlot {
			s := slot{bus: 0, slot: next}
			next++
			return s, nil
		}
		return slot{}, configErrorf(
			"no PCI slot left for %s: bus 0 has %d usable slots",
			what, lpcSlot-firstFreeSlot)
	}

	for i := range c.Disks {
		s, err := take(fmt.Sprintf("disk %d (%s)", i, c.Disks[i].Path))
		if err != nil {
			return nil, err
		}
		a.disks = append(a.disks, s)
	}
	for i := range c.Networks {
		s, err := take(fmt.Sprintf("network %d (%s)", i, c.Networks[i].Backend))
		if err != nil {
			return nil, err
		}
		a.networks = append(a.networks, s)
	}
	for i := range c.Passthrough {
		s, err := take(fmt.Sprintf("passthrough %d (%v)", i, c.Passthrough[i]))
		if err != nil {
			return nil, err
		}
		a.passthrough = append(a.passthrough, s)
	}
	if c.Framebuffer != nil {
		s, err := take("framebuffer")
		if err != nil {
			return nil, err
		}
		a.framebuffer = &s
	}

	return a, nil
}

// UsablePCISlots is how many devices a VM may have beyond the host
// bridge and the LPC bridge.
const UsablePCISlots = lpcSlot - firstFreeSlot

// PCIAddress reports where a device of the configuration ended up on the
// guest's bus.
//
// The guest enumerates devices in bus order, so this is what decides
// whether a disk appears as vtbd0 or vtbd1 inside the guest. kind is
// "disk", "network", "passthrough" or "framebuffer"; index is the
// position within that slice of the configuration.
func (c *Config) PCIAddress(kind string, index int) (vmm.PCIDevice, error) {
	a, err := c.assign()
	if err != nil {
		return vmm.PCIDevice{}, err
	}

	var slots []slot
	switch kind {
	case "disk":
		slots = a.disks
	case "network":
		slots = a.networks
	case "passthrough":
		slots = a.passthrough
	case "framebuffer":
		if a.framebuffer != nil {
			slots = []slot{*a.framebuffer}
		}
	default:
		return vmm.PCIDevice{}, configErrorf("unknown device kind %q", kind)
	}

	if index < 0 || index >= len(slots) {
		return vmm.PCIDevice{}, configErrorf("no %s at index %d", kind, index)
	}

	s := slots[index]
	return vmm.PCIDevice{
		Bus:      uint8(s.bus),
		Slot:     uint8(s.slot),
		Function: uint8(s.function),
	}, nil
}
