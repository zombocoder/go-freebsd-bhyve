## What and why

<!-- What does this change do, and what problem does it solve? Link issues with "Fixes #123".
     If it depends on how bhyve behaves, say where in usr.sbin/bhyve you checked it. -->

## How it was tested

<!-- CI runs level 1 on FreeBSD 14.x and 15.x VMs, and level 2 only when the runner can
     execute a nested guest. If you ran level 2 yourself, say where, e.g.:
     make integration on FreeBSD 15.1, edk2-bhyve installed -->

## Checklist

- [ ] New behaviour is covered by tests (a failing test first, then the fix), at the lowest level that can see it
- [ ] Configuration variable names are constants, and the renderer is still checked against `bhyve -o config.dump=true`
- [ ] Behaviour that differs between FreeBSD 14 and 15 works on both, or the difference is documented
- [ ] Tests that start bhyve use `tests/harness`, and leave no VM and no process behind
- [ ] Nothing here provisions storage, interfaces or firewall rules
- [ ] Public API changes are documented (doc comments, README) and listed in `CHANGELOG.md`
- [ ] `make check` passes
