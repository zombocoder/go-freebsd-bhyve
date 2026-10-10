# Contributing

## Building and testing

```sh
make test         # level 1; needs bhyve(8) but no privilege
make race
make lint
make integration  # level 2; needs root and vmm(4)
```

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
