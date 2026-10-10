//go:build freebsd

package bhyve

import (
	"fmt"
	"strings"
)

// PCI device model names, as bhyve spells them. The installed bhyve
// reports its own list with "bhyve -s help"; these are the ones this
// package models, and anything else is reachable through Config.Extra.
const (
	modelHostBridge  = "hostbridge"
	modelLPC         = "lpc"
	modelVirtioBlk   = "virtio-blk"
	modelVirtioNet   = "virtio-net"
	modelAHCIHD      = "ahci-hd"
	modelAHCICD      = "ahci-cd"
	modelNVMe        = "nvme"
	modelE1000       = "e1000"
	modelPassthru    = "passthru"
	modelFramebuffer = "fbuf"
)

// Per-device configuration variable names, relative to a device node.
const (
	keyDevice   = "device"
	keyPath     = "path"
	keyBackend  = "backend"
	keyReadOnly = "ro"
	keyNoCache  = "nocache"
	keyDirect   = "direct"
	keySectSize = "sectorsize"
	keyFBWait   = "wait"
	keyFBHost   = "host"
	keyFBPort   = "port"
	keyFBWidth  = "w"
	keyFBHeight = "h"
)

// DiskModel is the controller a disk is attached through.
//
// Virtio is the fast path and what a guest with virtio drivers should
// use. AHCI looks like a SATA controller, which is what an installer
// without drivers can see, and AHCICD presents the disk as an optical
// drive so a guest will boot an ISO from it.
type DiskModel int

const (
	// DiskVirtioBlk is a virtio block device.
	DiskVirtioBlk DiskModel = iota

	// DiskAHCIHD is a SATA hard disk.
	DiskAHCIHD

	// DiskAHCICD is a SATA optical drive, for booting an ISO.
	DiskAHCICD

	// DiskNVMe is an NVM Express namespace.
	DiskNVMe
)

// model returns the bhyve device model name.
func (m DiskModel) model() (string, bool) {
	switch m {
	case DiskVirtioBlk:
		return modelVirtioBlk, true
	case DiskAHCIHD:
		return modelAHCIHD, true
	case DiskAHCICD:
		return modelAHCICD, true
	case DiskNVMe:
		return modelNVMe, true
	default:
		return "", false
	}
}

// String renders the model as bhyve names it.
func (m DiskModel) String() string {
	if name, ok := m.model(); ok {
		return name
	}
	return fmt.Sprintf("diskmodel(%d)", int(m))
}

// Disk is a block device given to the guest.
//
// Path must already exist. It is whatever the host can present as a
// block device or a file: a ZFS zvol, a disk image, an ISO, a raw
// device. Creating it is not this package's job.
type Disk struct {
	Path  string
	Model DiskModel

	// ReadOnly presents the device to the guest as read-only, which
	// is what an ISO wants.
	ReadOnly bool

	// NoCache opens the backing file without the host's buffer
	// cache.
	NoCache bool

	// Direct opens the backing file for synchronous writes, so a
	// guest's flush means what it says.
	Direct bool

	// SectorSize overrides the sector size reported to the guest,
	// as "logical" or "logical/physical" bytes. Empty uses bhyve's
	// default.
	SectorSize string
}

// validate checks one disk.
func (d *Disk) validate(index int) error {
	if d.Path == "" {
		return configErrorf("disk %d has no path", index)
	}
	if _, ok := d.Model.model(); !ok {
		return configErrorf("disk %d has unknown model %d", index, int(d.Model))
	}
	if err := checkConfigValue(fmt.Sprintf("disk %d path", index), d.Path); err != nil {
		return err
	}
	if d.SectorSize != "" {
		return checkConfigValue(fmt.Sprintf("disk %d sector size", index),
			d.SectorSize)
	}
	return nil
}

// NetModel is the network interface presented to the guest.
type NetModel int

const (
	// NetVirtio is a virtio network device.
	NetVirtio NetModel = iota

	// NetE1000 emulates an Intel e82545, for guests without virtio
	// drivers.
	NetE1000
)

// model returns the bhyve device model name.
func (m NetModel) model() (string, bool) {
	switch m {
	case NetVirtio:
		return modelVirtioNet, true
	case NetE1000:
		return modelE1000, true
	default:
		return "", false
	}
}

// String renders the model as bhyve names it.
func (m NetModel) String() string {
	if name, ok := m.model(); ok {
		return name
	}
	return fmt.Sprintf("netmodel(%d)", int(m))
}

// Network is a network interface given to the guest.
//
// Backend names a host interface that must already exist and be
// configured: a tap(4) device, usually already added to a bridge. This
// package neither creates it nor destroys it, and does not touch the
// bridge or any firewall rules.
type Network struct {
	// Backend is the host side, for example "tap0".
	Backend string

	Model NetModel

	// MAC is the guest's hardware address. Empty lets bhyve generate
	// one, which it derives from the VM name and PCI slot and so is
	// stable across restarts.
	MAC string
}

// validate checks one interface.
func (n *Network) validate(index int) error {
	if n.Backend == "" {
		return configErrorf("network %d has no backend", index)
	}
	if _, ok := n.Model.model(); !ok {
		return configErrorf("network %d has unknown model %d", index, int(n.Model))
	}
	if err := checkConfigValue(fmt.Sprintf("network %d backend", index),
		n.Backend); err != nil {
		return err
	}
	if n.MAC != "" {
		if err := checkConfigValue(fmt.Sprintf("network %d MAC", index),
			n.MAC); err != nil {
			return err
		}
		if !looksLikeMAC(n.MAC) {
			return configErrorf("network %d MAC %q is not six colon-separated octets",
				index, n.MAC)
		}
	}
	return nil
}

// looksLikeMAC reports whether a string has the shape of a MAC address.
//
// This checks the shape only. Whether the address is sensible, locally
// administered or already in use on the segment is not something this
// package can know.
func looksLikeMAC(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return false
	}
	for _, p := range parts {
		if len(p) != 2 {
			return false
		}
		for i := 0; i < len(p); i++ {
			c := p[i] | 0x20 // fold case
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// SerialPort is one of the four COM ports the LPC bridge provides.
type SerialPort int

const (
	// COM1 is the port a guest's console normally appears on.
	COM1 SerialPort = 1
	COM2 SerialPort = 2
	COM3 SerialPort = 3
	COM4 SerialPort = 4
)

// String renders the port as bhyve names it.
func (p SerialPort) String() string { return fmt.Sprintf("com%d", int(p)) }

// Serial is a serial port given to the guest.
//
// Backend is where the host end goes. "stdio" connects it to the bhyve
// process's own terminal, which is useful interactively and useless for
// anything supervised. The alternative is one end of an nmdm(4) pair,
// which a control plane can reopen at will.
type Serial struct {
	Port SerialPort

	// Backend is "stdio" or a device path such as
	// "/dev/nmdm-web01-A".
	Backend string
}

// StdioBackend connects a serial port to the bhyve process's terminal.
const StdioBackend = "stdio"

// validate checks one serial port.
func (s *Serial) validate(index int) error {
	if s.Port < COM1 || s.Port > COM4 {
		return configErrorf("serial %d has port %d, want 1 to 4",
			index, int(s.Port))
	}
	if s.Backend == "" {
		return configErrorf("serial %d (%v) has no backend", index, s.Port)
	}
	return checkConfigValue(fmt.Sprintf("serial %d backend", index), s.Backend)
}

// Framebuffer is a VNC-backed display for the guest.
//
// A guest being installed from an ISO usually needs one; a guest that
// has a serial console does not.
type Framebuffer struct {
	// Host is the address to listen on. Empty listens on localhost,
	// which is the only safe default: the VNC server has no
	// transport security.
	Host string

	// Port is the TCP port. Zero uses bhyve's default of 5900.
	Port int

	// Width and Height are the display size in pixels. Zero uses
	// bhyve's defaults.
	Width  int
	Height int

	// Wait holds the guest at the boot ROM until a viewer connects,
	// so that an installer's first screen is not missed.
	Wait bool
}

// validate checks the framebuffer configuration.
func (f *Framebuffer) validate() error {
	if f == nil {
		return nil
	}
	if f.Port < 0 || f.Port > 65535 {
		return configErrorf("framebuffer port %d is not a valid TCP port", f.Port)
	}
	if f.Width < 0 || f.Height < 0 {
		return configErrorf("framebuffer size %dx%d is negative", f.Width, f.Height)
	}
	if f.Host != "" {
		return checkConfigValue("framebuffer host", f.Host)
	}
	return nil
}
