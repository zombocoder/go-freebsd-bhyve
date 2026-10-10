# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
The API may change until v1.0.0.

## [Unreleased]

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

[Unreleased]: https://github.com/zombocoder/go-freebsd-bhyve/commits/main
