# Examples

Five small programs, each showing one part of the library. They build on
FreeBSD only; `inspect` also needs amd64.

| Example | Needs | What it shows |
| --- | --- | --- |
| [`render`](render/main.go) | `bhyve(8)` | A `Config`, `Check`, the rendered `bhyve_config(5)` file, where each device lands on the PCI bus, and `Validate` through bhyve's own parser. Creates nothing. |
| [`run`](run/main.go) | root, firmware | Booting a VM and supervising it: Ctrl-C as an ACPI power button, a kill when the grace period runs out, what the exit reason means, and `Destroy` on every way out. |
| [`supervise`](supervise/main.go) | root, firmware | A restart policy written by the caller: restart on reboot, stop when the guest powers off, back off and give up on repeated failures. |
| [`inspect`](inspect/main.go) | root, firmware | Both libraries together: bhyve runs the VM, and go-freebsd-vmm reads its registers, active vCPUs and exit counters while it runs. |
| [`cleanup`](cleanup/main.go) | root to destroy | Finding VMs that bhyve left in the kernel and destroying the ones no bhyve process holds. |

"Firmware" means UEFI for bhyve: `pkg install edk2-bhyve`. With only
firmware and no disk, a guest boots to the firmware's own menu and waits
there, which is enough to see everything above working.

## Trying them

```sh
# No privilege: see what a Config turns into.
go run ./examples/render -name db01 -cpus 4 -mem 8G -tap tap1

# A VM with no disk, stopped with Ctrl-C.
go build -o /tmp/run ./examples/run
doas /tmp/run -name demo01 -console none

# Read a running guest's registers through vmm(4).
go build -o /tmp/inspect ./examples/inspect
doas /tmp/inspect -cpus 2 -samples 3

# What did bhyve leave behind?
go run ./examples/cleanup
```

`run` and `supervise` put the guest console on an `nmdm(4)` pair by
default, which needs `kldload nmdm`; attach to it with
`cu -l /dev/nmdm-<name>-B`. `-console none` runs without one, and `run`
also accepts `-console stdio`.

To boot an installer, give `run` a disk and an ISO, and a VNC port to watch
it on:

```sh
truncate -s 20G /vm/inst01.img
doas /tmp/run -name inst01 -disk /vm/inst01.img \
    -iso FreeBSD-15.1-RELEASE-amd64-disc1.iso -vnc 5900 -mem 2048
```

None of the examples creates a disk, a `tap(4)` interface or a bridge. That
is deliberate, and the same boundary the library keeps: storage and
networking belong to their own libraries, and composing them is the
caller's job.
