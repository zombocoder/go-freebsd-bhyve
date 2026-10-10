# go-freebsd-bhyve
#
# Written for both FreeBSD make and GNU make: no conditionals, all
# branching happens in the shell.

GO?=	go

all: build

build:
	$(GO) build ./...

# Level 1. Needs bhyve(8) installed, because the configuration layer is
# checked against bhyve's own parser rather than against a copy of its
# rules. Needs no privilege and starts no VM.
test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || \
	    echo "staticcheck not installed, skipping"

# Level 2. Runs real bhyve processes, so it needs root and vmm(4). Tests
# that need a VM to stay running also need UEFI firmware from the ports
# tree and skip with that reason when it is missing.
integration:
	$(GO) test -c -o /tmp/gobhyve-lifecycle.test ./tests/lifecycle/
	@echo "running as root; this starts and stops real VMs"
	@if command -v doas >/dev/null 2>&1; then \
	    doas /tmp/gobhyve-lifecycle.test -test.v; \
	else \
	    sudo /tmp/gobhyve-lifecycle.test -test.v; \
	fi
	rm -f /tmp/gobhyve-lifecycle.test

clean:
	rm -f /tmp/gobhyve-lifecycle.test
	$(GO) clean ./...

.PHONY: all build test race integration vet fmt lint clean
