# Contributing to go-freebsd-bhyve

Thanks for your interest in improving the library! Bug reports, fixes,
support for bhyve options and devices that are still missing, tests on
releases and hardware we have not seen, and documentation are all welcome.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Security problems should be reported privately, see [SECURITY.md](SECURITY.md).

## Table of Contents

- [Getting Started](#getting-started)
- [Running Tests](#running-tests)
- [Continuous Integration](#continuous-integration)
- [Project rules](#project-rules)
- [Tests](#tests)
- [Style](#style)
- [Submitting Changes](#submitting-changes)
- [Where Help Is Wanted](#where-help-is-wanted)

## Getting Started

### Prerequisites

- FreeBSD 14 or 15 with `bhyve(8)` (part of the base system)
- Go 1.22+ (`pkg install go`)
- For level 2: root (or `doas`), `vmm(4)` loaded, a CPU with VT-x or AMD-V,
  and UEFI firmware (`pkg install edk2-bhyve`) for the tests that need a VM
  to stay running. Without firmware those tests skip with the reason.

### Setup

```sh
git clone https://github.com/<your-username>/go-freebsd-bhyve.git
cd go-freebsd-bhyve
make build
make check
```

## Running Tests

| Command | Runs as | What it covers |
|---|---|---|
| `make test` | any user | Level 1: exit decoding, validation, and the renderer checked against bhyve's own parser. Needs `bhyve(8)`, no privilege. |
| `make race` | any user | The same under the race detector. |
| `make check` | any user | Formatting, `go vet` and `make test`. |
| `make integration` | root (via `doas`/`sudo`) | Level 2 (`tests/`): starts, stops, kills and destroys real bhyve processes. |

```sh
doas env PATH=$PATH go test -count=1 -run Kill -v ./tests/lifecycle
```

Level 2 only touches VMs it creates, named `gobhyve-test-<pid>-<random>`, and
destroys them however a test ends. It runs bhyve as root and loads `vmm(4)`:
prefer a test machine or a VM with nested virtualization over a host that
runs production guests.

## Continuous Integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push to
`main` and every pull request:

- **Lint** (Linux): `gofmt` and `go mod tidy -diff`.
- **FreeBSD 14.5 and 15.1** (QEMU VMs via
  [vmactions/freebsd-vm](https://github.com/vmactions/freebsd-vm)): installs Go
  and `edk2-bhyve`, runs `go vet`, builds, runs level 1 as an unprivileged user
  and under the race detector, then level 2 as root. It fails if a VM or a
  bhyve process is left behind.

Level 2 runs UEFI firmware in a guest, and under nested VT-x on GitHub's Intel
runners that hangs the whole FreeBSD VM. Runners are Intel or AMD at random, so
level 2 runs only on the AMD ones and is skipped with a notice otherwise. If
your change touches the lifecycle, run `make integration` on real hardware and
say where in the pull request.

## Project rules

1. **This package knows about bhyve, not about resources.** Disks, network
   interfaces and firewall rules belong to other libraries. A file here that
   imports one of them is a mistake.

2. **Configuration variable names are constants.** bhyve accepts names it does
   not understand without a word, so a name built from a string at a call site
   is a silent bug waiting to happen. `Config.Extra` is the only exception and
   is documented as sharp.

3. **Nothing is assumed about bhyve's behaviour.** Check it: read
   `usr.sbin/bhyve` and then run the thing. Several of the decisions in this
   package exist because the obvious assumption turned out to be wrong, and the
   comments say which.

4. **Exit statuses are facts, restart policy is not.** The library decodes why
   bhyve finished. Whether to start it again is the caller's decision.

5. **No VM is left behind.** bhyve does not clean up after itself, so anything
   that creates a VM is responsible for destroying it, tests included.

6. **Waits take a context and honour it.** Unlike `go-freebsd-vmm`, where a
   blocking ioctl cannot be cancelled, everything slow here is a process wait
   and genuinely cancellable.

## Tests

- **Level 1**, in the root package: no privilege, no VM. The renderer is checked
  against bhyve's own parser through `config.dump`, which is what catches drift
  when FreeBSD changes a variable. Most new tests belong here.
- **Level 2**, in `tests/`: real bhyve processes, root, `vmm(4)`. Use the
  helpers in `tests/harness` so that every VM is named with the sweep prefix and
  destroyed afterwards. Tests needing a live VM must go through
  `harness.RequireRunningVM`, which skips when there is no firmware rather than
  failing.

## Style

`gofmt`, `go vet` and `staticcheck` clean. Every exported symbol documented,
saying what the caller has to know that the signature does not: which bhyve
behaviour it depends on, what it will not catch, what it does not own.

The library does not log. Return errors.

Dependencies are the standard library and `go-freebsd-vmm`.

## Submitting Changes

1. Create a branch from `main`.
2. Make the change with tests; update doc comments and the README when the
   public API changes, and add an entry to [CHANGELOG.md](CHANGELOG.md) under
   "Unreleased".
3. Run `make check`, and `make integration` if the change touches the
   lifecycle.
4. Open a pull request; the template has a short checklist. CI must be green.

Commit messages: `type(scope): summary` (e.g. `fix(render): write bootrom for
FreeBSD 15`), a blank line, then what changed and why. If the change depends
on how bhyve behaves, say where in `usr.sbin/bhyve` you checked it. Common
types are `feat`, `fix`, `docs`, `test`, `refactor`, `ci`, `chore`.

Avoid breaking public API changes within 1.x. When FreeBSD releases differ,
make the same `Config` work on both, as the boot ROM keys do, rather than
asking callers which release they are on.

## Where Help Is Wanted

- **Runtime control**: a thin type over go-freebsd-vmm to reach a running VM
  for statistics, registers, suspend and resume, replacing `bhyvectl`.
- **Console access**: `nmdm(4)` pairs as an `io.ReadWriteCloser`.
- **A guest that boots an operating system in the tests**, so that `Stop` as
  an ACPI power button press can be tested; today the firmware only reaches
  its boot menu.
- **`TestStartCancellationKillsTheProcess`** always skips, because the VM
  appears before the 50 ms context expires; it needs a way to hold bhyve
  before it creates the VM.
- **`bhyveload(8)`** and **save and restore** (`bhyve -r`, needs a kernel
  with `BHYVE_SNAPSHOT`).
- **More examples**, for example one that composes this library with
  go-freebsd-ifc to create a `tap(4)` interface and a bridge for the VM.
- **Reports from other releases and CPUs**: if level 2 passes or fails on a
  setup not listed in the README, open an issue with the output.

Questions and design discussions are welcome in GitHub issues.
