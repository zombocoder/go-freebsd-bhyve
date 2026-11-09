# go-freebsd-bhyve
#
# Written for both FreeBSD make and GNU make: no conditionals, all
# branching happens in the shell.

GO?=	go

# Every suite finishes in seconds. A test binary still running after
# this is hung, and go test then fails it with every goroutine's stack,
# which names the test that stopped.
TEST_TIMEOUT?=	5m

# The race detector refuses to run with ASLR, which FreeBSD enables by
# default: the test binary prints "This sanitizer is not compatible with
# enabled ASLR" and exits 0, so go test reports ok without having run a
# single test. proccontrol(1) turns ASLR off for go test and everything
# it starts.
RACE=	proccontrol -m aslr -s disable

all: build

build:
	$(GO) build ./...

# Level 1. Needs bhyve(8) installed, because the configuration layer is
# checked against bhyve's own parser rather than against a copy of its
# rules. Needs no privilege and starts no VM.
test:
	$(GO) test -timeout $(TEST_TIMEOUT) ./...

race:
	$(RACE) $(GO) test -timeout $(TEST_TIMEOUT) -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Fail, listing the files, if anything is not gofmt-clean.
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
	    echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

# What to run before sending a change: no root needed.
check: fmt-check vet test

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
	    doas /tmp/gobhyve-lifecycle.test -test.v -test.timeout $(TEST_TIMEOUT); \
	else \
	    sudo /tmp/gobhyve-lifecycle.test -test.v -test.timeout $(TEST_TIMEOUT); \
	fi
	rm -f /tmp/gobhyve-lifecycle.test

clean:
	rm -f /tmp/gobhyve-lifecycle.test
	$(GO) clean ./...

.PHONY: all build test race integration vet fmt fmt-check check lint clean
