//go:build freebsd

package bhyve

// State is where a VM is in its lifecycle.
//
// The terminal state is a single one, StateExited, with ExitReason
// saying what happened. Encoding "rebooted", "crashed" and "failed to
// start" as separate states would duplicate what ExitKind already
// carries, and the two would then have to be kept in agreement.
type State int

const (
	// StateDefined is a VM that has been described but not started.
	// A VM returns here only through New; after a run it stays
	// StateExited until Start is called again.
	StateDefined State = iota

	// StateStarting is a bhyve process that has been launched but
	// whose virtual machine has not appeared yet.
	//
	// A VM can fail without ever leaving this state: a configuration
	// bhyve will not accept, or a guest with nothing to boot.
	StateStarting

	// StateRunning is a VM that exists in the kernel, observed
	// through /dev/vmm.
	StateRunning

	// StateStopping is a VM that has been asked to stop and has not
	// finished.
	StateStopping

	// StateExited is a bhyve process that has finished. Reason says
	// why.
	StateExited
)

// String renders the state.
func (s State) String() string {
	switch s {
	case StateDefined:
		return "defined"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateExited:
		return "exited"
	default:
		return "unknown"
	}
}

// Active reports whether a bhyve process is expected to be alive.
func (s State) Active() bool {
	switch s {
	case StateStarting, StateRunning, StateStopping:
		return true
	default:
		return false
	}
}
