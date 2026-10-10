//go:build freebsd

package bhyve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// DefaultBhyvePath is where FreeBSD installs bhyve(8).
//
// /usr/sbin is often missing from an unprivileged PATH, so the path is
// spelled out rather than looked up.
const DefaultBhyvePath = "/usr/sbin/bhyve"

// Tool locates the external programs this package runs.
//
// The zero value uses the base system's own paths, which is what a
// caller normally wants. Overriding it is for testing and for hosts
// where bhyve lives somewhere unusual.
type Tool struct {
	// Bhyve is the path to bhyve(8). Empty means DefaultBhyvePath.
	Bhyve string
}

// bhyvePath returns the bhyve(8) path to run, checking that it exists.
func (t Tool) bhyvePath() (string, error) {
	path := t.Bhyve
	if path == "" {
		path = DefaultBhyvePath
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory: %w", path, ErrNotFound)
	}

	return path, nil
}

// Resolved is a configuration as bhyve itself understands it: every
// variable, including the defaults this package never set.
type Resolved struct {
	// Vars holds the variables bhyve reported, by name.
	Vars map[string]string
}

// Get returns a variable's value and whether bhyve had it.
func (r Resolved) Get(key string) (string, bool) {
	value, ok := r.Vars[key]
	return value, ok
}

// Keys returns the variable names, sorted.
func (r Resolved) Keys() []string {
	out := make([]string, 0, len(r.Vars))
	for key := range r.Vars {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// String renders the resolved configuration as a configuration file.
func (r Resolved) String() string {
	var b strings.Builder
	for _, key := range r.Keys() {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(r.Vars[key])
		b.WriteByte('\n')
	}
	return b.String()
}

// configDumpStatus is the exit status bhyve uses after dumping its
// configuration.
//
// It is 1, the same status a guest powering off produces, because the
// dump path shares an exit() with nine other things. Here it means
// success.
const configDumpStatus = 1

// Validate asks bhyve to parse the configuration and reports what it
// made of it.
//
// It starts no virtual machine and needs no privilege: bhyve dumps its
// configuration and exits before touching /dev/vmm. That makes it
// usable from an ordinary test, and from a control plane checking a
// definition before committing it.
//
// # What this does and does not catch
//
// It is worth being exact, because the obvious assumption is wrong in
// both directions.
//
// What it catches: a file bhyve's parser will not read, a command line
// bhyve will not accept, and the resolved value of every variable
// including the defaults this package never set. The last is the real
// value here, since it is bhyve's own answer rather than a guess.
//
// What it does not catch is a configuration that parses but means
// nothing sensible. bhyve dumps and exits before it checks any of that,
// so a CPU topology that does not match the vCPU count, a device model
// it does not implement, or a disk path that does not exist all pass
// Validate and then fail at Start. Config.Check covers what can be
// decided locally; the rest is only knowable by running.
//
// It also cannot catch a misspelled variable name. bhyve stores every
// name it is given, whether or not anything reads it, because names can
// be referenced by other values; an unknown one therefore appears in the
// dump looking perfectly healthy. Nothing in bhyve distinguishes a
// variable it understands from one it is merely holding, which is why
// Config.Extra is documented as sharp and why the names this package
// manages are compile-time constants.
func (c *Config) Validate(ctx context.Context, tool Tool) (Resolved, error) {
	const op = "validate"

	if err := c.Check(); err != nil {
		return Resolved{}, err
	}

	path, err := tool.bhyvePath()
	if err != nil {
		return Resolved{}, newError(op, c.Name, err)
	}

	rendered, err := c.Render()
	if err != nil {
		return Resolved{}, err
	}

	file, cleanup, err := writeTempConfig(c.Name, rendered)
	if err != nil {
		return Resolved{}, newError(op, c.Name, err)
	}
	defer cleanup()

	// -o is applied after -k, so the dump flag cannot be overridden
	// by the file.
	cmd := exec.CommandContext(ctx, path,
		"-k", file,
		"-o", keyConfigDump+"=true",
		c.Name)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	status, signal := exitStatus(runErr)

	switch {
	case signal != 0:
		return Resolved{}, newError(op, c.Name,
			fmt.Errorf("bhyve was killed by signal %d", signal))

	case status == configDumpStatus:
		// The expected outcome.

	case status == statusUsage:
		// bhyve rejected the command line, which this package
		// built: a bug here rather than in the caller's config.
		return Resolved{}, newError(op, c.Name, fmt.Errorf(
			"bhyve rejected its command line (status %d): %s",
			status, firstLine(stderr.String())))

	default:
		return Resolved{}, fmt.Errorf("bhyve rejected the configuration: %s: %w",
			firstLine(stderr.String()), ErrInvalidConfig)
	}

	resolved := parseResolved(stdout.String())

	if err := c.checkReached(resolved); err != nil {
		return resolved, err
	}

	return resolved, nil
}

// checkReached verifies that every variable this package sent arrived.
//
// This does not detect a name bhyve does not understand: it holds those
// too, and dumps them. What it catches is a variable lost on the way,
// which would mean a bug in this package's rendering rather than in the
// caller's configuration. Cheap, and it would be an unpleasant bug to
// find any other way.
func (c *Config) checkReached(resolved Resolved) error {
	v, err := c.variables()
	if err != nil {
		return err
	}

	var missing []string
	for _, key := range v.keys() {
		if _, ok := resolved.Vars[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("bhyve did not record %d variable(s) that were set, which means it does not know them: %s: %w",
			len(missing), strings.Join(missing, ", "), ErrInvalidConfig)
	}

	return nil
}

// parseResolved reads bhyve's configuration dump.
func parseResolved(out string) Resolved {
	r := Resolved{Vars: make(map[string]string)}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		r.Vars[key] = value
	}

	return r
}

// writeTempConfig writes a configuration file for one bhyve invocation
// and returns a function that removes it.
func writeTempConfig(name, contents string) (path string, cleanup func(), err error) {
	file, err := os.CreateTemp("", "gobhyve-"+sanitiseForFilename(name)+"-*.conf")
	if err != nil {
		return "", nil, err
	}

	cleanup = func() {
		file.Close()
		os.Remove(file.Name())
	}

	if _, err := file.WriteString(contents); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}

	return file.Name(), cleanup, nil
}

// sanitiseForFilename reduces a VM name to something safe in a path.
//
// VM names are already checked for path separators, so this only guards
// the remaining awkward characters.
func sanitiseForFilename(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
}

// exitStatus extracts the exit status and signal from a command error.
//
// A command that ran and exited reports a status; one killed by a signal
// reports a signal and no meaningful status.
func exitStatus(err error) (status, signal int) {
	if err == nil {
		return 0, 0
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, 0
	}

	state := exitErr.ProcessState
	if sig, ok := signalOf(state); ok {
		return -1, sig
	}

	return state.ExitCode(), 0
}

// firstLine returns the first non-empty line, which is where bhyve puts
// the reason it is unhappy.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
