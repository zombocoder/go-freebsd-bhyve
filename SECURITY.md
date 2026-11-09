# Security Policy

## Supported versions

Security fixes are made on the `main` branch and released as a new patch or
minor version of the latest major release (currently 1.x).

## Reporting a vulnerability

Please **do not open a public issue** for security problems.

Report them privately through GitHub:
[Report a vulnerability](https://github.com/zombocoder/go-freebsd-bhyve/security/advisories/new)
(Security tab → "Report a vulnerability").

Include what you can of:

- the affected function and the commit you tested;
- the FreeBSD version (`freebsd-version -ku`) and the bhyve it ships;
- the `Config` involved, or the rendered configuration file, with anything
  private removed;
- steps or a minimal program that reproduces the problem;
- the impact you expect (a configuration that means something other than
  what was asked, a VM or process acted on that was not requested, ...).

You should get a first response within a week. Once a fix is available, the
advisory is published with credit to the reporter unless you prefer otherwise.

## Scope

This library runs `bhyve(8)` as root, usually inside a control plane that
builds a `Config` from input it did not write itself. Of particular interest:

- **configuration injection**: a value that changes the meaning of the
  rendered `bhyve_config(5)` file, for example by reaching a variable other
  than the one it was meant for. Values are refused if they contain a newline
  or NUL; a way around that, or another way to smuggle a variable in, is in
  scope;
- `Config.Extra` overriding a variable the package manages, such as
  `destroy_on_poweroff`, the VM name or the boot ROM;
- operations reaching the wrong VM or process: `Stop`, `Kill` or `Destroy`
  acting on a VM, a process or a process group other than the one started;
- a VM, a bhyve process or a configuration file left behind where the API
  promises cleanup;
- a Go program's environment, file descriptors or privileges leaking into
  the bhyve process beyond what is documented.

Bugs in `bhyve(8)` or `vmm(4)` themselves should be reported to the
[FreeBSD security team](https://www.freebsd.org/security/).
