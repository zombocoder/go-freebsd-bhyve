//go:build freebsd

package bhyve

import (
	"errors"
	"syscall"
	"testing"
)

// Decoding why bhyve finished is the heart of the supervisor, and the
// case that matters is exit status 1: bhyve uses it both for a guest
// powering itself off and for most of its own startup failures. Started
// is what separates them, and it comes from the device node rather than
// from the status.

func TestDecodeExit(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		signal  int
		started bool
		want    ExitKind
	}{
		{name: "guest reset", status: 0, started: true, want: ExitRebooted},

		// The ambiguity this type exists for.
		{name: "guest powered off", status: 1, started: true, want: ExitPoweredOff},
		{name: "failed before running", status: 1, started: false, want: ExitStartupFailed},

		{name: "guest halted", status: 2, started: true, want: ExitHalted},
		{name: "guest triple faulted", status: 3, started: true, want: ExitTripleFault},

		// Status 4 is bhyve's own, whether or not it got as far as
		// creating the VM: an unbootable guest is reported this way
		// with the VM already in place.
		{name: "bhyve error after creating the VM", status: 4, started: true, want: ExitBhyveError},
		{name: "bhyve error before creating the VM", status: 4, started: false, want: ExitBhyveError},

		{name: "bad command line", status: 64, want: ExitUsageError},
		{name: "unknown suspend reason", status: 100, want: ExitBhyveError},
		{name: "status we do not know", status: 42, want: ExitUnknown},

		// A signal beats any status.
		{name: "killed", status: -1, signal: 9, started: true, want: ExitSignalled},
		{name: "terminated", status: -1, signal: 15, started: true, want: ExitSignalled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeExit(tc.status, tc.signal, tc.started, "")
			if got.Kind != tc.want {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.want)
			}
			if got.Started != tc.started {
				t.Errorf("Started = %v, want %v", got.Started, tc.started)
			}
			if tc.signal != 0 {
				if got.Signal != tc.signal {
					t.Errorf("Signal = %d, want %d", got.Signal, tc.signal)
				}
				if got.Status != -1 {
					t.Errorf("Status = %d, want -1 for a signalled process", got.Status)
				}
			}
		})
	}
}

// TestDecodeExitStatusOneIsAmbiguous is the whole point, stated as a
// test: the same status means opposite things, and only Started tells
// them apart.
func TestDecodeExitStatusOneIsAmbiguous(t *testing.T) {
	ran := decodeExit(1, 0, true, "")
	never := decodeExit(1, 0, false, "")

	if ran.Kind == never.Kind {
		t.Fatalf("status 1 decoded to %v either way; Started is being ignored",
			ran.Kind)
	}
	if !ran.Kind.GuestRequested() {
		t.Errorf("a guest that powered off decoded to %v, which is not guest-requested",
			ran.Kind)
	}
	if never.Kind.GuestRequested() {
		t.Errorf("a failure to start decoded to %v, which claims the guest asked for it",
			never.Kind)
	}
}

func TestExitKindGuestRequested(t *testing.T) {
	// What the guest asked for, so a supervisor may act on it.
	for _, k := range []ExitKind{ExitRebooted, ExitPoweredOff, ExitHalted} {
		if !k.GuestRequested() {
			t.Errorf("%v.GuestRequested() = false, want true", k)
		}
	}
	// What went wrong, so restarting blindly would loop.
	for _, k := range []ExitKind{
		ExitTripleFault, ExitStartupFailed, ExitBhyveError,
		ExitUsageError, ExitSignalled, ExitUnknown,
	} {
		if k.GuestRequested() {
			t.Errorf("%v.GuestRequested() = true, want false", k)
		}
	}
}

func TestExitKindStrings(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range []ExitKind{
		ExitUnknown, ExitRebooted, ExitPoweredOff, ExitHalted,
		ExitTripleFault, ExitStartupFailed, ExitBhyveError,
		ExitUsageError, ExitSignalled,
	} {
		name := k.String()
		if name == "" {
			t.Errorf("ExitKind(%d) has an empty name", int(k))
		}
		if seen[name] {
			t.Errorf("two kinds are both named %q", name)
		}
		seen[name] = true
	}
}

func TestExitReasonString(t *testing.T) {
	tests := []struct {
		in   ExitReason
		want string
	}{
		{
			in:   ExitReason{Kind: ExitPoweredOff, Status: 1, Started: true},
			want: "powered off (status 1)",
		},
		{
			in: ExitReason{
				Kind:    ExitBhyveError,
				Status:  4,
				Message: "virtual machine cannot be booted",
			},
			want: "bhyve error (status 4): virtual machine cannot be booted",
		},
		{
			in:   ExitReason{Kind: ExitSignalled, Status: -1, Signal: 9},
			want: "signalled (signal 9)",
		},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// TestDecodeExitKeepsBhyveMessage matters because for a startup failure
// bhyve's own line is usually the only explanation there is.
func TestDecodeExitKeepsBhyveMessage(t *testing.T) {
	r := decodeExit(4, 0, false, "  virtual machine cannot be booted\n")
	if r.Message != "virtual machine cannot be booted" {
		t.Errorf("Message = %q, want it trimmed and kept", r.Message)
	}
}

func TestErrorWrapping(t *testing.T) {
	err := newError("start", "web01", syscall.EPERM)
	if err == nil {
		t.Fatal("newError returned nil for a real error")
	}

	want := `bhyve: start "web01": operation not permitted`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Error("errors.Is(err, syscall.EPERM) = false, want true")
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("errors.As(err, *Error) = false")
	}
	if e.Op != "start" || e.VM != "web01" {
		t.Errorf("unwrapped to %+v, lost a field", e)
	}

	if newError("start", "web01", nil) != nil {
		t.Error("newError with a nil cause returned non-nil")
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		ErrInvalidConfig, ErrNotFound, ErrNotRunning,
		ErrAlreadyRunning, ErrUnsupported,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = true, want false", a, b)
			}
		}
	}
}

func TestStateStrings(t *testing.T) {
	active := []State{StateStarting, StateRunning, StateStopping}
	inactive := []State{StateDefined, StateExited}

	for _, s := range active {
		if !s.Active() {
			t.Errorf("%v.Active() = false, want true", s)
		}
	}
	for _, s := range inactive {
		if s.Active() {
			t.Errorf("%v.Active() = true, want false", s)
		}
	}

	seen := map[string]bool{}
	for _, s := range append(append([]State{}, active...), inactive...) {
		name := s.String()
		if name == "" || seen[name] {
			t.Errorf("State(%d) has an empty or duplicate name %q", int(s), name)
		}
		seen[name] = true
	}
}
