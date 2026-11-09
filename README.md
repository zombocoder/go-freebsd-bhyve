# go-freebsd-bhyve

[![CI](https://github.com/zombocoder/go-freebsd-bhyve/actions/workflows/ci.yml/badge.svg)](https://github.com/zombocoder/go-freebsd-bhyve/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zombocoder/go-freebsd-bhyve.svg)](https://pkg.go.dev/github.com/zombocoder/go-freebsd-bhyve)
[![Go Report Card](https://goreportcard.com/badge/github.com/zombocoder/go-freebsd-bhyve)](https://goreportcard.com/report/github.com/zombocoder/go-freebsd-bhyve)
[![Release](https://img.shields.io/github/v/release/zombocoder/go-freebsd-bhyve?sort=semver)](https://github.com/zombocoder/go-freebsd-bhyve/releases)
[![FreeBSD](https://img.shields.io/badge/FreeBSD-14%20%7C%2015-red.svg)](https://www.freebsd.org/)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.22-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-BSD--2--Clause-green.svg)](LICENSE)

Run and supervise FreeBSD virtual machines from Go.

This is the layer above [go-freebsd-vmm](https://github.com/zombocoder/go-freebsd-vmm), which binds `vmm(4)`
through `libvmmapi`. That library can inspect and control a virtual machine but
cannot boot one, because device emulation lives in bhyve's userspace process.
This package supplies the missing half: it describes a VM, runs `bhyve(8)` to
execute it, supervises that process, and interprets what happens.

```go
cfg := bhyve.Config{
	Name:   "web01",
	CPU:    bhyve.CPU{Count: 2},
	Memory: bhyve.Memory{Size: 2 << 30, Wired: true},
	Firmware: &bhyve.Firmware{
		ROM: "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
	},
	Disks:    []bhyve.Disk{{Path: "/dev/zvol/zroot/vm/web01"}},
	Networks: []bhyve.Network{{Backend: "tap0"}},
	ACPITables: true,
}

vm, err := bhyve.New(cfg)
if err != nil {
	return err
}
defer vm.Destroy()

if err := vm.Start(ctx); err != nil {
	return err
}

reason, err := vm.Wait(ctx)
```

## Architecture

```
          application
               |
               v
      go-freebsd-bhyve      configuration, process lifecycle
               |
               v
          bhyve(8)          device models, boot ROM
               |
               v
      go-freebsd-vmm        runtime inspection and control
               |
               v
      libvmmapi / vmm(4)
```

## Scope

This package knows about bhyve: its configuration variables, its command line,
and what its exit statuses mean. It does not know about the resources a VM uses.

Disk paths must already exist and network backends must already be configured.
Nothing here creates a ZFS volume, a disk image, a `tap(4)` interface or a
bridge, and nothing here removes one or writes a firewall rule. Each of those
has its own library:

| Concern | Library |
| --- | --- |
| VM lifecycle | this one |
| VM runtime control | [go-freebsd-vmm](https://github.com/zombocoder/go-freebsd-vmm) |
| Storage | [go-freebsd-libzfs](https://github.com/zombocoder/go-freebsd-libzfs) |
| Networking | [go-freebsd-ifc](https://github.com/zombocoder/go-freebsd-ifc) |
| Packet filter | [go-freebsd-pf](https://github.com/zombocoder/go-freebsd-pf) |

Composing them belongs to whatever sits above. That is not fastidiousness: a VM
runner that provisioned its own storage could not be used with storage it did
not provision, and could not be tested on a host without it.

## Three things about bhyve worth knowing

Each of these shapes how the package has to be used, and each was established
by reading bhyve's sources and then checking against the real thing.

**A VM outlives the process that ran it.** bhyve leaves the virtual machine in
the kernel when it exits, so `Destroy` is a required step rather than a
tidy-up: until it runs, the VM holds its name and its guest memory. `Start`
refuses to run over a leftover, because reusing one would hand the previous
guest's memory and device state to the new one.

**Exit status 1 is ambiguous.** bhyve uses it for a guest powering itself off
and for most of its own startup failures — ten distinct call sites, including a
successful configuration dump — with nothing to separate them. `ExitReason`
therefore carries `Started`, established from the device node rather than from
the status:

| Status | Meaning | Decodes to |
| --- | --- | --- |
| 0 | guest reset itself | `ExitRebooted` |
| 1 | guest powered off, **if** it ran | `ExitPoweredOff` |
| 1 | anything else | `ExitStartupFailed` |
| 2 | guest halted | `ExitHalted` |
| 3 | guest triple faulted | `ExitTripleFault` |
| 4 | bhyve's own failure | `ExitBhyveError` |
| 64 | bhyve rejected its command line | `ExitUsageError` |

**A graceful stop is not always graceful.** `Stop` sends SIGTERM, which bhyve
turns into an ACPI power button press — but only once the guest has enabled
ACPI, by writing to the PM1 control register. Before that, during firmware or
early boot, and for any guest that never enables it, SIGTERM keeps its default
disposition and kills bhyve outright. The returned reason says which happened,
and the difference matters to the guest's filesystems.

## Configuration

A typed `Config` renders to a `bhyve_config(5)` file, which bhyve reads with
`-k`. Variable names are compile-time constants, never built from caller input,
because bhyve accepts names it does not understand without a word.

Two validation steps, with different reach:

```go
err := cfg.Check()                              // local, no subprocess
resolved, err := cfg.Validate(ctx, bhyve.Tool{}) // asks bhyve itself
```

`Validate` runs `bhyve -o config.dump=true`, which parses the configuration
with bhyve's own parser, reports every variable including bhyve's defaults, and
exits without creating anything. It needs no privilege, which is what makes the
configuration layer testable in an ordinary unit test.

Its limits are worth stating, because the obvious assumptions are wrong in both
directions. It does **not** catch a configuration that parses but means
nothing — a CPU topology that contradicts the vCPU count passes `Validate` and
fails at `Start` — and it **cannot** catch a misspelled variable name, since
bhyve stores every name it is given so that values can refer to each other. A
typo therefore comes back in the dump looking healthy. `Check` covers what can
be decided locally; the rest is only knowable by running.

## Examples

Five runnable programs live under [`examples/`](examples/README.md):

| Example | What it shows |
| --- | --- |
| `render` | a `Config`, its rendered file, its PCI layout, and bhyve's own view of it; no privilege |
| `run` | booting a VM, Ctrl-C as an ACPI power button, decoding the exit, destroying the VM |
| `supervise` | a restart policy owned by the caller: restart on reboot, stop on power off, back off on failure |
| `inspect` | bhyve running a VM while go-freebsd-vmm reads its registers and exit counters |
| `cleanup` | finding and destroying VMs that bhyve left in the kernel |

```sh
go run ./examples/render -name db01 -cpus 4 -mem 8G
```

## Installation

```sh
go get github.com/zombocoder/go-freebsd-bhyve
```

## Requirements

- FreeBSD 14 or 15 with `vmm(4)` and `bhyve(8)`
- Go 1.22 or newer
- root, to run a VM
- UEFI firmware for a guest that boots: `pkg install edk2-bhyve`

## Testing

```sh
make test         # level 1: no privilege, no VM; needs bhyve(8) installed
make race         # the same, under the race detector
make lint         # go vet and staticcheck
make check        # formatting, vet and level 1: run before a PR
make integration  # level 2: real bhyve processes; needs root
```

Level 1 checks the configuration layer against bhyve's own parser rather than
against a copy of its rules, so it catches drift when FreeBSD renames or adds a
variable.

Level 2 starts real VMs. Every test destroys what it created, including when it
fails, and each package sweeps leftover `/dev/vmm/gobhyve-test-*` objects before
and after the run. Tests that need a VM to stay running also need UEFI
firmware, and skip with that reason when it is missing.

Both levels are green on FreeBSD 15.1 amd64 with `edk2-bhyve` installed. CI
runs level 1 on FreeBSD 14.5 and 15.1, and level 2 on runners that can execute
a nested guest; see [CONTRIBUTING.md](CONTRIBUTING.md#continuous-integration).

The boot ROM is one place the releases differ: bhyve 14 reads `lpc.bootrom`,
bhyve 15 reads `bootrom` and warns about the old name. The renderer writes
both, so the same configuration file works on either.

## Status

Implemented: the configuration model and renderer, validation through bhyve, PCI
slot assignment, the process lifecycle with its state machine, exit
interpretation, and cleanup.

Not implemented yet, in rough order of usefulness:

- **Runtime control.** A thin type over `go-freebsd-vmm` to reach a running VM
  for statistics, registers, suspend and resume, replacing `bhyvectl` entirely.
  This is the part with no equivalent anywhere today.
- **Console access.** `nmdm(4)` pairs as an `io.ReadWriteCloser`, which is what
  a web console in a control plane needs.
- **`bhyveload(8)`.** The other way to boot, for FreeBSD guests without UEFI. It
  adds a second process to the lifecycle, so it is worth doing only if needed.
- **Save and restore.** Through `bhyve -r`. Gated on a kernel built with
  `BHYVE_SNAPSHOT`, which is not the default; there is no live migration in
  bhyve at all.
- **A guest operating system that boots, in the tests.** The tests that need a
  live VM run UEFI firmware, which sits at its boot menu; no operating system
  is booted, so ACPI shutdown (`Stop` as a power button press) is untested.

## Contributing

Contributions are welcome -- see [CONTRIBUTING.md](CONTRIBUTING.md) for how to
run the tests, what CI checks, and where help is wanted. Please follow the
[Code of Conduct](CODE_OF_CONDUCT.md) and report security issues privately as
described in [SECURITY.md](SECURITY.md). [CHANGELOG.md](CHANGELOG.md) has the
release history.

## License

BSD-2-Clause. See [LICENSE](LICENSE).
