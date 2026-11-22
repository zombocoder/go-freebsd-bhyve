//go:build freebsd

package lifecycle_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
	"github.com/zombocoder/go-freebsd-bhyve/tests/harness"
)

// TestStderrFileKeepsBhyvesMessage checks that sending bhyve's standard
// error to a file costs nothing in error reporting: a device that cannot
// be opened is still explained in bhyve's own words, and those words are
// in the file too.
func TestStderrFileKeepsBhyvesMessage(t *testing.T) {
	harness.RequireHost(t)
	rom := harness.FirmwarePath(t)

	const bhyveSays = "Could not open backing file"

	errPath := filepath.Join(t.TempDir(), "bhyve.err")
	if err := os.WriteFile(errPath, []byte("from an earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := harness.Config(t)
	cfg.Firmware = &bhyve.Firmware{ROM: rom}
	cfg.Disks = []bhyve.Disk{{Path: "/nonexistent/gobhyve-test.img"}}
	vm := harness.RequireVM(t, cfg, bhyve.WithStderrFile(errPath))

	err := vm.Start(ctx(t))
	if err == nil {
		t.Fatal("Start succeeded although bhyve could not open the disk")
	}
	if !strings.Contains(err.Error(), bhyveSays) {
		t.Errorf("Start error = %q, want bhyve's %q in it", err, bhyveSays)
	}

	reason, err := vm.Wait(ctx(t))
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if reason.Kind != bhyve.ExitBhyveError {
		t.Errorf("Kind = %v, want bhyve error", reason.Kind)
	}
	if !strings.Contains(reason.Message, bhyveSays) {
		t.Errorf("Message = %q, want bhyve's %q", reason.Message, bhyveSays)
	}
	if strings.Contains(reason.Message, "earlier run") {
		t.Errorf("Message = %q, includes output from before this run", reason.Message)
	}
	t.Logf("exit reason: %v", reason)

	contents, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(contents), "from an earlier run\n") {
		t.Errorf("file = %q, want the earlier contents kept", contents)
	}
	if !strings.Contains(string(contents), bhyveSays) {
		t.Errorf("file = %q, want bhyve's %q in it", contents, bhyveSays)
	}
}

// The environment that turns the test binary into a starter: a process
// that starts a VM and exits, as a supervising daemon does when it is
// restarted.
const (
	starterEnv     = "GOBHYVE_TEST_STARTER"
	starterNameEnv = "GOBHYVE_TEST_STARTER_NAME"
	starterROMEnv  = "GOBHYVE_TEST_STARTER_ROM"
	starterErrEnv  = "GOBHYVE_TEST_STARTER_STDERR"
	starterDirEnv  = "GOBHYVE_TEST_STARTER_DIR"
)

// Ports for the starter's VM. The framebuffer port follows the ones the
// other tests use.
const (
	starterVNCPort = 15905
	starterGDBPort = 15906
)

// runStarter starts a VM with its standard error going to a file,
// prints bhyve's pid, and exits leaving bhyve running.
func runStarter() int {
	cfg := bhyve.Config{
		Name:       os.Getenv(starterNameEnv),
		CPU:        bhyve.CPU{Count: 1},
		Memory:     bhyve.Memory{Size: 256 << 20},
		ACPITables: true,
		Firmware:   &bhyve.Firmware{ROM: os.Getenv(starterROMEnv)},
		// Framebuffer keeps the firmware waiting at its boot menu.
		Framebuffer: &bhyve.Framebuffer{Port: starterVNCPort},
		// bhyve's debug server warns on standard error when a
		// connection to it fails, which gives the test a way to make
		// bhyve write there on demand. See writeToBhyveStderr.
		// The address is explicit because bhyve's default,
		// "localhost", may resolve to ::1 only.
		Extra: map[string]string{
			"gdb.address": "127.0.0.1",
			"gdb.port":    strconv.Itoa(starterGDBPort),
		},
	}

	vm, err := bhyve.New(cfg,
		bhyve.WithDir(os.Getenv(starterDirEnv)),
		bhyve.WithStderrFile(os.Getenv(starterErrEnv)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "starter: New: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), harness.Timeout)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "starter: Start: %v\n", err)
		return 1
	}

	fmt.Println(vm.Pid())
	return 0
}

// TestStderrFileOutlivesTheStarter is why WithStderrFile exists. With
// the default pipe, bhyve's standard error is read by the process that
// started it; once that process exits, bhyve's next write there gets
// SIGPIPE or EPIPE. With a file, bhyve holds the file itself.
//
// A child process starts the VM and exits. The test then checks that
// bhyve's descriptor 2 is the file rather than a pipe, makes bhyve write
// to it, and checks that bhyve is still running afterwards.
func TestStderrFileOutlivesTheStarter(t *testing.T) {
	harness.RequireHost(t)
	rom := harness.FirmwarePath(t)
	if _, err := exec.LookPath("procstat"); err != nil {
		t.Skipf("procstat(1) is not available: %v", err)
	}

	name := harness.Name(t)
	errPath := filepath.Join(t.TempDir(), "bhyve.err")

	starter := exec.Command(os.Args[0], "-test.run=^$")
	starter.Env = append(os.Environ(),
		starterEnv+"=1",
		starterNameEnv+"="+name,
		starterROMEnv+"="+rom,
		starterErrEnv+"="+errPath,
		starterDirEnv+"="+t.TempDir())
	var starterErr bytes.Buffer
	starter.Stderr = &starterErr

	// Cleanup is registered before anything can be left running.
	var pid int
	t.Cleanup(func() { reapOrphan(t, name, pid) })

	out, err := starter.Output()
	if err != nil {
		t.Fatalf("starter: %v\n%s", err, starterErr.String())
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		t.Fatalf("starter printed %q, want a pid", out)
	}

	// The starter has exited and been reaped; bhyve must not have gone
	// with it.
	if !alive(pid) {
		t.Fatalf("bhyve (pid %d) exited with its starter", pid)
	}

	// The property that removes SIGPIPE: descriptor 2 is the file.
	kind, path := descriptor(t, pid, 2)
	if kind != "v" {
		t.Fatalf("bhyve's fd 2 has type %q, want v (a file); p would be a pipe", kind)
	}
	if path != errPath {
		t.Errorf("bhyve's fd 2 is %q, want %q", path, errPath)
	}

	before, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}

	written := writeToBhyveStderr(t, errPath, len(before))
	t.Logf("bhyve wrote after its starter exited: %q", written)

	// Give a SIGPIPE, had there been one, time to land.
	time.Sleep(500 * time.Millisecond)
	if !alive(pid) {
		t.Fatalf("bhyve (pid %d) died after writing to its standard error", pid)
	}
	if _, err := os.Stat("/dev/vmm/" + name); err != nil {
		t.Errorf("the VM is gone after bhyve wrote to its standard error: %v", err)
	}
}

// writeToBhyveStderr makes bhyve write to its standard error, and
// returns what it appended to the file after offset.
//
// bhyve's debug server (gdb.c) reports a failed read on its connection
// with warn(), so a connection reset by the peer comes out on standard
// error as "bhyve: Read from GDB socket: Connection reset by peer".
// Closing with a zero linger time sends that reset. The write is to the
// file only because of WithStderrFile; with a pipe whose reader had
// exited, it would raise SIGPIPE.
func writeToBhyveStderr(t *testing.T, errPath string, offset int) string {
	t.Helper()

	deadline := time.Now().Add(harness.Timeout)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(starterGDBPort)), time.Second)
		if err != nil {
			t.Fatalf("connecting to bhyve's debug server: %v", err)
		}
		// Let bhyve accept it before resetting it.
		time.Sleep(200 * time.Millisecond)
		conn.(*net.TCPConn).SetLinger(0)
		conn.Close()

		for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); {
			contents, err := os.ReadFile(errPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) > offset {
				return string(contents[offset:])
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("attempt %d: bhyve wrote nothing yet", attempt)
	}

	t.Fatal("bhyve never wrote to its standard error")
	return ""
}

// descriptor returns the type and path procstat(1) reports for one of a
// process's file descriptors.
func descriptor(t *testing.T, pid, fd int) (kind, path string) {
	t.Helper()

	out, err := exec.Command("procstat", "-f", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("procstat -f %d: %v", pid, err)
	}

	// PID COMM FD T V FLAGS REF OFFSET PRO NAME
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[2] != strconv.Itoa(fd) {
			continue
		}
		kind = fields[3]
		if kind == "v" && len(fields) >= 10 {
			path = fields[len(fields)-1]
		}
		return kind, path
	}

	t.Fatalf("procstat -f %d lists no fd %d:\n%s", pid, fd, out)
	return "", ""
}

// alive reports whether a process exists.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// reapOrphan kills a bhyve that is no longer anyone's child and destroys
// its VM.
//
// The harness cannot do this, because it only knows VMs started from
// the test process. The process group is killed, as Kill does, and the
// VM can only be destroyed once bhyve has let go of it.
func reapOrphan(t *testing.T, name string, pid int) {
	t.Helper()

	if pid > 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("killing bhyve (pid %d): %v", pid, err)
		}
		// init reaps it; wait for that to happen.
		deadline := time.Now().Add(harness.Timeout)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("bhyve (pid %d) still running after SIGKILL", pid)
		}
	}

	if err := bhyve.DestroyByName(name); err != nil {
		t.Errorf("destroying %q: %v", name, err)
	}
	if _, err := os.Stat("/dev/vmm/" + name); err == nil {
		t.Errorf("%q still exists after DestroyByName", name)
	}
}
