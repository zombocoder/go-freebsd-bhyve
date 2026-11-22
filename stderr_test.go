//go:build freebsd

package bhyve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// WithStderrFile exists so that bhyve's standard error does not depend
// on the caller staying alive. None of this needs privilege: the tail
// reader is tested on a temporary file, and the wiring through Start is
// tested with a stand-in for bhyve that fails the way bhyve does.

func TestWithStderrFileSetsThePath(t *testing.T) {
	vm, err := New(basicConfig(), WithStderrFile("/var/log/bhyve/vm01.err"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if vm.stderrPath != "/var/log/bhyve/vm01.err" {
		t.Errorf("stderrPath = %q, want /var/log/bhyve/vm01.err", vm.stderrPath)
	}

	vm, err = New(basicConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if vm.stderrPath != "" {
		t.Errorf("stderrPath = %q without the option, want empty", vm.stderrPath)
	}
}

// openTestStderr opens path as Start would and closes it when the test
// ends.
func openTestStderr(t *testing.T, path string) *stderrFile {
	t.Helper()

	out, err := openStderrFile(path)
	if err != nil {
		t.Fatalf("openStderrFile(%s): %v", path, err)
	}
	t.Cleanup(func() {
		out.w.Close()
		out.r.Close()
	})
	return out
}

func TestOpenStderrFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm.err")

	out := openTestStderr(t, path)
	if out.offset != 0 {
		t.Errorf("offset of a new file = %d, want 0", out.offset)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}

	if err := os.WriteFile(path, []byte("earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = openTestStderr(t, path)
	if out.offset != int64(len("earlier run\n")) {
		t.Errorf("offset = %d, want %d", out.offset, len("earlier run\n"))
	}
	// Append mode: a write lands after what is there, never over it.
	if _, err := out.w.WriteString("this run\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "earlier run\nthis run\n" {
		t.Errorf("file = %q, want the earlier contents kept", got)
	}
	if got, err := readStderrTail(out.r, out.offset, maxStderr); err != nil || got != "this run\n" {
		t.Errorf("read back = %q, %v; want this run's output only", got, err)
	}
}

func TestOpenStderrFileNeedsTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	path := filepath.Join(missing, "vm.err")

	_, err := openStderrFile(path)
	if err == nil {
		t.Fatal("openStderrFile succeeded in a missing directory")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %q, want it to name the missing directory", err)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the directory was created; it must be left to the caller")
	}
}

// A FIFO at the path would block an open for writing until a reader
// came along, under the VM's lock. It must be refused, promptly.
func TestOpenStderrFileRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.err")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// A reader, so that the non-blocking open itself succeeds and the
	// type check is what refuses it.
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening the FIFO for reading: %v", err)
	}
	defer reader.Close()

	result := make(chan error, 1)
	go func() {
		out, err := openStderrFile(path)
		if err == nil {
			out.w.Close()
			out.r.Close()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("openStderrFile accepted a FIFO")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %q, want it to say the path is not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openStderrFile blocked on a FIFO")
	}
}

// Without a reader, the open for writing fails rather than waits.
func TestOpenStderrFileDoesNotWaitOnAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.err")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := openStderrFile(path)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("openStderrFile accepted a FIFO with no reader")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openStderrFile blocked on a FIFO with no reader")
	}
}

func TestOpenStderrFileRefusesADevice(t *testing.T) {
	if _, err := openStderrFile("/dev/null"); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("openStderrFile(/dev/null) = %v, want it refused as not a regular file", err)
	}
}

// newsyslog rotates by renaming. bhyve keeps writing to the renamed
// file, and that is where its message has to be read back from, not
// from whatever now sits at the path.
func TestReadStderrTailFollowsARename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm.err")
	if err := os.WriteFile(path, []byte("earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := openTestStderr(t, path)
	if _, err := out.w.WriteString("before rotation\n"); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(path, path+".0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a new file at the path\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// bhyve's descriptor still names the renamed inode.
	if _, err := out.w.WriteString("after rotation\n"); err != nil {
		t.Fatal(err)
	}

	got, err := readStderrTail(out.r, out.offset, maxStderr)
	if err != nil {
		t.Fatalf("readStderrTail: %v", err)
	}
	if got != "before rotation\nafter rotation\n" {
		t.Errorf("read back = %q, want this run's output from the renamed file", got)
	}
}

func TestReadStderrTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.err")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	write := func(s string) {
		t.Helper()
		if err := file.Truncate(0); err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte(s), 0); err != nil {
			t.Fatal(err)
		}
	}
	read := func(offset int64, limit int) string {
		t.Helper()
		got, err := readStderrTail(file, offset, limit)
		if err != nil {
			t.Fatalf("readStderrTail(%d, %d): %v", offset, limit, err)
		}
		return got
	}

	write("old run\nnew run\n")
	if got := read(int64(len("old run\n")), 100); got != "new run\n" {
		t.Errorf("after offset = %q, want only what followed it", got)
	}
	if got := read(int64(len("old run\nnew run\n")), 100); got != "" {
		t.Errorf("nothing appended = %q, want empty", got)
	}
	if got := read(0, 100); got != "old run\nnew run\n" {
		t.Errorf("from 0 = %q, want everything", got)
	}

	// The cap keeps the end, where bhyve's reason for exiting is.
	write("0123456789")
	if got := read(2, 4); got != "6789" {
		t.Errorf("capped = %q, want the last 4 bytes", got)
	}
	if got := read(7, 4); got != "789" {
		t.Errorf("under the cap = %q, want all 3 bytes after the offset", got)
	}

	// Shorter than the offset means it was truncated by rotation;
	// what is left was all written since.
	write("rotated\n")
	if got := read(1000, 100); got != "rotated\n" {
		t.Errorf("after truncation = %q, want the whole file", got)
	}

	closed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := readStderrTail(closed, 0, 100); err == nil {
		t.Error("readStderrTail on a closed file succeeded, want an error")
	}
}

// fakeBhyve writes a program that stands in for bhyve: it prints what
// bhyve prints for a backend it cannot open and exits 4, as bhyve does.
func fakeBhyve(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "bhyve")
	script := "#!/bin/sh\n" +
		"echo 'Could not open backing file: /nonexistent.img: No such file or directory' >&2\n" +
		"exit 4\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartReportsBhyvesWordsFromTheStderrFile(t *testing.T) {
	cfg := basicConfig()
	cfg.Name = "gobhyve-unit-" + strings.ReplaceAll(t.Name(), "/", "-")
	errPath := filepath.Join(t.TempDir(), "vm.err")
	if err := os.WriteFile(errPath, []byte("from an earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	vm, err := New(cfg,
		WithTool(Tool{Bhyve: fakeBhyve(t)}),
		WithDir(t.TempDir()),
		WithStderrFile(errPath))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = vm.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded although the program exited 4")
	}
	if !strings.Contains(err.Error(), "Could not open backing file") {
		t.Errorf("Start error = %q, want bhyve's own message in it", err)
	}

	reason, err := vm.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if reason.Kind != ExitBhyveError {
		t.Errorf("Kind = %v, want bhyve error", reason.Kind)
	}
	want := "Could not open backing file: /nonexistent.img: No such file or directory"
	if reason.Message != want {
		t.Errorf("Message = %q, want %q and nothing from earlier runs", reason.Message, want)
	}

	got, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from an earlier run\n"+want+"\n" {
		t.Errorf("file = %q, want the earlier contents followed by bhyve's", got)
	}
}

func TestStartFailsClearlyWithoutTheStderrDirectory(t *testing.T) {
	cfg := basicConfig()
	cfg.Name = "gobhyve-unit-" + strings.ReplaceAll(t.Name(), "/", "-")
	confDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	vm, err := New(cfg,
		WithTool(Tool{Bhyve: fakeBhyve(t)}),
		WithDir(confDir),
		WithStderrFile(filepath.Join(missing, "vm.err")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = vm.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with the stderr file's directory missing")
	}
	var bErr *Error
	if !errors.As(err, &bErr) || bErr.Op != "start" {
		t.Errorf("err = %v, want a start *Error", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %q, want it to name %s", err, missing)
	}
	if got := vm.State(); got != StateDefined {
		t.Errorf("State() = %v, want defined: nothing was started", got)
	}

	// The configuration written for the run must not be left behind.
	if entries, _ := os.ReadDir(confDir); len(entries) != 0 {
		t.Errorf("%d files left in the configuration directory", len(entries))
	}
}

// Without the option nothing changes: the message comes through the
// pipe as before.
func TestStartWithoutStderrFileStillReportsBhyvesWords(t *testing.T) {
	cfg := basicConfig()
	cfg.Name = "gobhyve-unit-" + strings.ReplaceAll(t.Name(), "/", "-")

	vm, err := New(cfg, WithTool(Tool{Bhyve: fakeBhyve(t)}), WithDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := vm.Start(ctx); err == nil {
		t.Fatal("Start succeeded although the program exited 4")
	}
	reason, err := vm.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.HasPrefix(reason.Message, "Could not open backing file") {
		t.Errorf("Message = %q, want bhyve's own message", reason.Message)
	}
}
