//go:build freebsd

package bhyve

import (
	"errors"
	"fmt"
	"strings"
)

// Library-level errors, describing a misuse of this API or a limitation
// of the host rather than something a guest did.
var (
	// ErrInvalidConfig is returned for a configuration this package
	// rejects before bhyve(8) ever sees it.
	ErrInvalidConfig = errors.New("bhyve: invalid configuration")

	// ErrNotFound is returned when the bhyve(8) binary cannot be
	// located.
	ErrNotFound = errors.New("bhyve: bhyve(8) not found")

	// ErrNotRunning is returned by operations that need a running VM.
	ErrNotRunning = errors.New("bhyve: VM is not running")

	// ErrAlreadyRunning is returned by Start on a VM that is already
	// running.
	ErrAlreadyRunning = errors.New("bhyve: VM is already running")

	// ErrUnsupported is returned for operations this FreeBSD, or this
	// kernel build, does not provide.
	ErrUnsupported = errors.New("bhyve: unsupported")
)

// Error reports a failed operation on a VM.
type Error struct {
	// Op names the operation, in the vocabulary of this package
	// rather than of bhyve's command line.
	Op string

	// VM is the virtual machine the operation was attempted on.
	VM string

	// Err is the underlying cause.
	Err error
}

// Error renders the failure, for example:
//
//	bhyve: start "web01": fork/exec /usr/sbin/bhyve: permission denied
func (e *Error) Error() string {
	var b strings.Builder

	b.WriteString("bhyve: ")
	b.WriteString(e.Op)
	if e.VM != "" {
		b.WriteString(" ")
		b.WriteString(`"` + e.VM + `"`)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}

	return b.String()
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an Error, or nil when err is nil.
func newError(op, vm string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, VM: vm, Err: err}
}

// configErrorf builds an error wrapping ErrInvalidConfig.
func configErrorf(format string, args ...any) error {
	return fmt.Errorf("bhyve: "+format+": %w",
		append(args, ErrInvalidConfig)...)
}

// ExitKind is why a bhyve process finished, decoded from its exit
// status.
//
// bhyve reports the guest's own wishes this way: a guest that reboots
// itself and a guest that powers itself off both leave bhyve exiting
// normally, with the difference carried only in the status.
type ExitKind int

const (
	// ExitUnknown is a status this package does not recognise.
	ExitUnknown ExitKind = iota

	// ExitRebooted is the guest having reset itself. The usual
	// response is to start it again.
	ExitRebooted

	// ExitPoweredOff is the guest having powered itself off.
	ExitPoweredOff

	// ExitHalted is the guest having halted.
	ExitHalted

	// ExitTripleFault is the guest having triple faulted, which is a
	// crash inside the guest rather than a failure of bhyve.
	ExitTripleFault

	// ExitStartupFailed is bhyve having exited before the VM ever
	// ran.
	//
	// This shares an exit status with ExitPoweredOff, which is why
	// ExitReason tracks whether the VM started; see ExitReason.
	ExitStartupFailed

	// ExitBhyveError is a failure bhyve reported about itself,
	// including a configuration it would not accept.
	ExitBhyveError

	// ExitUsageError is bhyve rejecting its own command line, which
	// means this package built it wrongly.
	ExitUsageError

	// ExitSignalled is bhyve having been killed by a signal, which is
	// what Stop does when a guest will not shut down.
	ExitSignalled
)

// String renders the kind.
func (k ExitKind) String() string {
	switch k {
	case ExitRebooted:
		return "rebooted"
	case ExitPoweredOff:
		return "powered off"
	case ExitHalted:
		return "halted"
	case ExitTripleFault:
		return "triple fault"
	case ExitStartupFailed:
		return "startup failed"
	case ExitBhyveError:
		return "bhyve error"
	case ExitUsageError:
		return "usage error"
	case ExitSignalled:
		return "signalled"
	default:
		return "unknown"
	}
}

// GuestRequested reports whether the guest asked for this, as opposed to
// something having gone wrong.
//
// A reboot, a power off and a halt are all the guest's own decisions. A
// triple fault is not: the guest crashed.
func (k ExitKind) GuestRequested() bool {
	switch k {
	case ExitRebooted, ExitPoweredOff, ExitHalted:
		return true
	default:
		return false
	}
}

// bhyve exit statuses, from vmexit_suspend() in usr.sbin/bhyve and from
// the errx and usage paths around it.
//
// Only the first four come from the guest. The rest are bhyve reporting
// about itself.
const (
	statusReset       = 0  // VM_SUSPEND_RESET
	statusPowerOff    = 1  // VM_SUSPEND_POWEROFF, and much else besides
	statusHalt        = 2  // VM_SUSPEND_HALT
	statusTripleFault = 3  // VM_SUSPEND_TRIPLEFAULT
	statusError       = 4  // errx: bad config file, unbootable VM, ...
	statusUsage       = 64 // EX_USAGE: a command line bhyve would not take
	statusBadSuspend  = 100
)

// ExitReason is why a VM stopped.
//
// # The ambiguity this type exists for
//
// Exit status 1 means the guest powered itself off. It also means at
// least nine other things, among them a bad vCPU count, an unreadable
// checkpoint file, and a successful configuration dump. bhyve gives no
// way to tell them apart.
//
// Started is therefore not derived from the exit status at all: it
// records whether the VM was ever observed running, which the supervisor
// establishes independently. Status 1 with Started is a power off; status
// 1 without it is a failure to launch.
type ExitReason struct {
	// Kind is the decoded reason.
	Kind ExitKind

	// Status is the raw exit status, or -1 when bhyve was signalled.
	Status int

	// Signal is the signal that killed bhyve, or 0.
	Signal int

	// Started reports whether the VM reached a running state before
	// bhyve exited.
	Started bool

	// Message is whatever bhyve wrote to its standard error, trimmed.
	// For a startup failure this is usually the only explanation
	// available, so it is kept rather than discarded.
	Message string
}

// String renders the reason, for example:
//
//	powered off (status 1)
//	startup failed (status 4): virtual machine cannot be booted
func (r ExitReason) String() string {
	var b strings.Builder

	b.WriteString(r.Kind.String())
	if r.Signal != 0 {
		fmt.Fprintf(&b, " (signal %d)", r.Signal)
	} else {
		fmt.Fprintf(&b, " (status %d)", r.Status)
	}
	if r.Message != "" {
		b.WriteString(": ")
		b.WriteString(r.Message)
	}

	return b.String()
}

// decodeExit works out why bhyve finished.
//
// started must say whether the VM was seen running, because the exit
// status cannot answer that; see ExitReason.
func decodeExit(status, signal int, started bool, stderr string) ExitReason {
	r := ExitReason{
		Status:  status,
		Signal:  signal,
		Started: started,
		Message: strings.TrimSpace(stderr),
	}

	if signal != 0 {
		r.Kind = ExitSignalled
		r.Status = -1
		return r
	}

	switch status {
	case statusReset:
		r.Kind = ExitRebooted
	case statusPowerOff:
		// The whole reason Started exists.
		if started {
			r.Kind = ExitPoweredOff
		} else {
			r.Kind = ExitStartupFailed
		}
	case statusHalt:
		r.Kind = ExitHalted
	case statusTripleFault:
		r.Kind = ExitTripleFault
	case statusError:
		// Not disambiguated by Started. bhyve uses this status for
		// its own failures only, and it reaches some of them after
		// creating the VM: an unbootable guest is reported this
		// way, with the VM already in place. Status 1 is the only
		// one the guest shares with bhyve.
		r.Kind = ExitBhyveError
	case statusUsage:
		r.Kind = ExitUsageError
	case statusBadSuspend:
		r.Kind = ExitBhyveError
	default:
		r.Kind = ExitUnknown
	}

	return r
}
