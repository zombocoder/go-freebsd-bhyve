# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
The API may change until v1.0.0.

## [Unreleased]

### Added

- CI on GitHub Actions: formatting and `go mod tidy` on Linux; build, vet
  and level 1 on FreeBSD 14.5 and 15.1 VMs, with level 1 run unprivileged;
  level 2 with UEFI firmware where the runner can execute a guest (nested
  AMD-V). Each run fails if a VM or a bhyve process is left behind.
- `make check`, and `-timeout $(TEST_TIMEOUT)` (5m) on every `go test`.
- Examples: `render`, `run`, `supervise`, `inspect` (with go-freebsd-vmm)
  and `cleanup`, each run against FreeBSD 15.1 before being added.
- Community files: Code of Conduct, security policy, issue and pull request
  templates, Dependabot for the workflow actions and go-freebsd-vmm, and an
  expanded CONTRIBUTING with where help is wanted.

### Fixed

- **`Start` reported success for a VM that then failed to start.** It
  returned as soon as the VM appeared under `/dev/vmm`, but bhyve creates
  the VM before it loads the boot ROM and opens device backends, any of
  which can still fail; FreeBSD 14 even creates it before noticing there is
  nothing to boot. Whether `Start` succeeded then depended on scheduling.
  It now waits for bhyve to resume the boot processor, which bhyve 14 and
  15 both do as the last step of startup, read through go-freebsd-vmm.
  `StateRunning` means the same. Covered by
  `TestStartFailsWhenADeviceCannotOpen`, which wires guest memory to make
  the window certain and failed on every run before the fix.
- **The race detector never ran.** FreeBSD enables ASLR by default, and a
  `-race` test binary then prints "This sanitizer is not compatible with
  enabled ASLR" and exits 0, so `go test -race` reported ok without running
  a test. `make race` and CI now run it under `proccontrol -m aslr -s
  disable`. Run that way, it finds no races.
- **Builds on FreeBSD 15.** Requires go-freebsd-vmm v1.1.0; v1.0.0 did not
  compile against FreeBSD 15's headers.
- **Boot ROM on FreeBSD 15.** bhyve 15 reads the firmware from `bootrom`
  and `bootvars` and only falls back to `lpc.bootrom` / `lpc.bootvars`
  with a deprecation warning; bhyve 14 reads only the `lpc.` names. Both
  pairs are now rendered, so one configuration file works on either
  release without the warning. `Config.Extra` cannot override either.
- **Lifecycle tests on FreeBSD 15.** Without a boot ROM, bhyve 15 exits
  with "no bootrom was configured" before creating the VM, where bhyve 14
  created it and left it behind. `TestUnbootableVM` now accepts both and
  checks that `ExitReason.Started` agrees with the kernel.
  `TestDestroyReleasesTheName` gets its leftover VM by killing a running
  one, which works on both releases, instead of skipping on 15.

### Verified

- Levels 1 and 2 on FreeBSD 15.1-RELEASE amd64 (AMD Ryzen 7 5700G) with
  `edk2-bhyve` installed, so the tests that need a live VM ran rather than
  skipped.

## [1.0.2] - 2026-10-16

### Added

- A non-FreeBSD stub so pkg.go.dev can render the package documentation.

## [1.0.0] - 2026-10-10

### Added

- **Configuration model.** `Config` with `CPU`, `Memory`, `Firmware`, `Disk`,
  `Network`, `Serial`, `Framebuffer` and passthrough devices, rendering to a
  `bhyve_config(5)` file. Variable names are compile-time constants.
- **PCI slot assignment.** Stable layout with the host bridge at 0:0:0 and the
  LPC bridge at 0:31:0, the rest allocated in configuration order so that a
  guest's device names do not shift between runs. `Config.PCIAddress` reports
  where a device ended up.
- **Validation.** `Config.Check` locally, and `Config.Validate` through
  `bhyve -o config.dump=true`, which parses with bhyve's own parser and needs
  no privilege.
- **Lifecycle.** `New`, `Start`, `Wait`, `Stop`, `Kill`, `Destroy`,
  `DestroyByName`, with `State` and `ExitReason`. All the waits take a
  `context.Context` and honour it.
- **Tests.** Level 1 covering exit decoding, configuration validation and the
  renderer against bhyve's own parser. Level 2 under `tests/` driving real
  bhyve processes, skipping with a reason when the host cannot run one.

### Notes on the bhyve side

Each of these was established by reading `usr.sbin/bhyve` and then checking
against the installed bhyve, and each one shaped the API:

- bhyve leaves the virtual machine in the kernel when it exits. `Destroy` is
  therefore a required step, and `Start` refuses to run over a leftover rather
  than handing the previous guest's state to the new one.
- Exit status 1 is used by `VM_SUSPEND_POWEROFF` and by nine other call sites,
  among them a bad vCPU count and a successful `config.dump`. `ExitReason`
  carries `Started`, taken from the device node rather than the status, because
  the status cannot answer it. Status 4 is not ambiguous and does not need it.
- SIGTERM becomes an ACPI power button press only after the guest has enabled
  ACPI; before that it kills bhyve outright. `Stop` reports which happened.
- `config.dump` resolves and prints the whole configuration without privilege
  and without creating a VM, which is what makes the configuration layer
  testable in a unit test.
- `config.dump` does **not** validate semantics: a CPU topology contradicting
  the vCPU count passes it and fails at startup. The local checks in
  `Config.Check` exist for that reason.
- bhyve stores every configuration variable it is handed, understood or not, so
  that values can reference each other. A misspelled name therefore appears in
  the dump looking healthy and cannot be detected. `Config.Extra` is documented
  as sharp for this reason, and every name this package manages is a constant.
- `destroy_on_poweroff` lets bhyve destroy the VM itself. It is pinned off and
  `Config.Extra` is refused if it tries to set it, since that would pull the VM
  out from under anything holding it open.
- Without a boot ROM, bhyve creates the VM, reports "virtual machine cannot be
  booted" and exits 4 — leaving the VM behind.

[Unreleased]: https://github.com/zombocoder/go-freebsd-bhyve/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/zombocoder/go-freebsd-bhyve/compare/v1.0.0...v1.0.2
[1.0.0]: https://github.com/zombocoder/go-freebsd-bhyve/releases/tag/v1.0.0

### Changed

- Depend on the published `github.com/zombocoder/go-freebsd-vmm v1.0.0` instead
  of a local `replace` directive. The module is now `go get`-able.
