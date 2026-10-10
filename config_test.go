//go:build freebsd

package bhyve

import (
	"context"
	"errors"
	"strings"
	"testing"

	vmm "github.com/zombocoder/go-freebsd-vmm"
)

func TestCheckAcceptsValidConfigs(t *testing.T) {
	good := map[string]Config{
		"minimal": {
			Name:   "vm01",
			CPU:    CPU{Count: 1},
			Memory: Memory{Size: 256 << 20},
		},
		"topology without firmware": {
			Name:   "vm01",
			CPU:    CPU{Count: 4, Sockets: 2, Cores: 2, Threads: 1},
			Memory: Memory{Size: 1 << 30},
		},
		"everything": basicConfig(),
	}

	for name, cfg := range good {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Check(); err != nil {
				t.Errorf("Check: %v", err)
			}
		})
	}
}

func TestCheckName(t *testing.T) {
	bad := map[string]string{
		"empty":          "",
		"path separator": "group/vm01",
		"traversal":      "../../dev/null",
		"dot":            ".",
		"dotdot":         "..",
		"newline":        "vm\n01",
		"nul":            "vm\x0001",
		"too long":       strings.Repeat("a", vmm.MaxNameLen+1),
	}

	for name, vmName := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				Name:   vmName,
				CPU:    CPU{Count: 1},
				Memory: Memory{Size: 256 << 20},
			}
			if err := cfg.Check(); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Check with name %q = %v, want ErrInvalidConfig",
					vmName, err)
			}
		})
	}

	// At the limit is fine.
	cfg := Config{
		Name:   strings.Repeat("a", vmm.MaxNameLen),
		CPU:    CPU{Count: 1},
		Memory: Memory{Size: 256 << 20},
	}
	if err := cfg.Check(); err != nil {
		t.Errorf("Check with a name at the limit = %v, want nil", err)
	}
}

func TestCheckCPU(t *testing.T) {
	bad := map[string]CPU{
		"zero count":        {Count: 0},
		"negative count":    {Count: -1},
		"zero sockets":      {Count: 2, Sockets: 0, Cores: 2, Threads: 1},
		"zero cores":        {Count: 2, Sockets: 1, Cores: 0, Threads: 1},
		"zero threads":      {Count: 2, Sockets: 1, Cores: 2, Threads: 0},
		"product mismatch":  {Count: 2, Sockets: 2, Cores: 7, Threads: 1},
		"product too small": {Count: 8, Sockets: 1, Cores: 2, Threads: 1},
	}

	for name, cpu := range bad {
		t.Run(name, func(t *testing.T) {
			if err := cpu.validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("validate(%+v) = %v, want ErrInvalidConfig", cpu, err)
			}
		})
	}

	good := []CPU{
		{Count: 1},
		{Count: 4},
		{Count: 1, Sockets: 1, Cores: 1, Threads: 1},
		{Count: 16, Sockets: 2, Cores: 4, Threads: 2},
	}
	for _, cpu := range good {
		if err := cpu.validate(); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", cpu, err)
		}
	}
}

func TestCheckMemory(t *testing.T) {
	if err := (Memory{Size: 0}).validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("zero memory = %v, want ErrInvalidConfig", err)
	}
	if err := (Memory{Size: 1 << 10}).validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("1 KiB of memory = %v, want ErrInvalidConfig", err)
	}
	if err := (Memory{Size: minMemory}).validate(); err != nil {
		t.Errorf("memory at the minimum = %v, want nil", err)
	}
}

func TestCheckDisks(t *testing.T) {
	bad := map[string]Disk{
		"no path":       {Model: DiskVirtioBlk},
		"unknown model": {Path: "/tmp/d", Model: DiskModel(99)},
		"newline":       {Path: "/tmp/d\nx", Model: DiskVirtioBlk},
	}
	for name, d := range bad {
		t.Run(name, func(t *testing.T) {
			if err := d.validate(0); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("validate = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestCheckNetworks(t *testing.T) {
	bad := map[string]Network{
		"no backend":    {Model: NetVirtio},
		"unknown model": {Backend: "tap0", Model: NetModel(99)},
		"short MAC":     {Backend: "tap0", MAC: "58:9c:fc:00:11"},
		"bad hex":       {Backend: "tap0", MAC: "58:9c:fc:00:11:zz"},
		"wrong width":   {Backend: "tap0", MAC: "5:9c:fc:00:11:22"},
	}
	for name, n := range bad {
		t.Run(name, func(t *testing.T) {
			if err := n.validate(0); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("validate = %v, want ErrInvalidConfig", err)
			}
		})
	}

	good := []Network{
		{Backend: "tap0"},
		{Backend: "tap0", MAC: "58:9c:fc:00:11:22"},
		{Backend: "tap0", MAC: "AA:BB:CC:DD:EE:FF"},
		{Backend: "netgraph", Model: NetE1000},
	}
	for _, n := range good {
		if err := n.validate(0); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", n, err)
		}
	}
}

func TestCheckSerials(t *testing.T) {
	bad := map[string]Serial{
		"port zero":  {Port: 0, Backend: StdioBackend},
		"port five":  {Port: 5, Backend: StdioBackend},
		"no backend": {Port: COM1},
	}
	for name, s := range bad {
		t.Run(name, func(t *testing.T) {
			if err := s.validate(0); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("validate = %v, want ErrInvalidConfig", err)
			}
		})
	}

	if err := (&Serial{Port: COM1, Backend: StdioBackend}).validate(0); err != nil {
		t.Errorf("stdio on COM1 = %v, want nil", err)
	}
}

func TestCheckFirmware(t *testing.T) {
	if err := (&Firmware{}).validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("firmware with no ROM = %v, want ErrInvalidConfig", err)
	}
	// Absent firmware is allowed by Check: bhyve is the one that
	// refuses to boot without it, and says so clearly.
	var none *Firmware
	if err := none.validate(); err != nil {
		t.Errorf("absent firmware = %v, want nil", err)
	}
}

func TestCheckFramebuffer(t *testing.T) {
	bad := []Framebuffer{
		{Port: -1},
		{Port: 70000},
		{Width: -1},
	}
	for _, f := range bad {
		fb := f
		if err := fb.validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("validate(%+v) = %v, want ErrInvalidConfig", f, err)
		}
	}
}

func TestCheckPassthroughAddress(t *testing.T) {
	cfg := basicConfig()
	cfg.Passthrough = []vmm.PCIDevice{{Bus: 2, Slot: 99}}

	if err := cfg.Check(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Check with an out-of-range PCI slot = %v, want ErrInvalidConfig", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New with an empty config = %v, want ErrInvalidConfig", err)
	}
}

func TestNewStartsDefined(t *testing.T) {
	vm, err := New(basicConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := vm.State(); got != StateDefined {
		t.Errorf("State() = %v, want %v", got, StateDefined)
	}
	if _, has := vm.Reason(); has {
		t.Error("a VM that has not run reports a reason")
	}
	if got := vm.Pid(); got != 0 {
		t.Errorf("Pid() = %d, want 0", got)
	}
	if got := vm.Name(); got != basicConfig().Name {
		t.Errorf("Name() = %q, want %q", got, basicConfig().Name)
	}
}

// TestOperationsOnADefinedVM checks the state machine refuses what it
// should before anything has been started.
func TestOperationsOnADefinedVM(t *testing.T) {
	vm, err := New(basicConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := vm.Wait(ctxBackground()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Wait on a defined VM = %v, want ErrNotRunning", err)
	}
	if _, err := vm.Stop(ctxBackground()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Stop on a defined VM = %v, want ErrNotRunning", err)
	}
	// Killing something that is not running is a no-op, not an error.
	if err := vm.Kill(); err != nil {
		t.Errorf("Kill on a defined VM = %v, want nil", err)
	}
	// Nor is destroying a VM that does not exist.
	if err := vm.Destroy(); err != nil {
		t.Errorf("Destroy on a nonexistent VM = %v, want nil", err)
	}
}

// ctxBackground keeps the table-driven tests above readable.
func ctxBackground() context.Context { return context.Background() }
