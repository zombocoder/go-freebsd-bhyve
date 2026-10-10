//go:build freebsd

package bhyve

import (
	"os"
	"syscall"
)

// signalOf reports the signal that killed a process, if one did.
//
// Go's ProcessState keeps this behind an opaque Sys(), so unwrapping it
// is unavoidable. It is confined here.
func signalOf(state *os.ProcessState) (int, bool) {
	if state == nil {
		return 0, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return 0, false
	}
	if !status.Signaled() {
		return 0, false
	}
	return int(status.Signal()), true
}
