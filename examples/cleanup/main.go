//go:build freebsd

// Command cleanup finds virtual machines that bhyve left in the kernel
// and destroys them.
//
//	cleanup                 list every VM and whether a bhyve process holds it
//	doas cleanup -destroy   destroy the ones no bhyve process holds
//	doas cleanup -destroy -prefix ci-
//
// bhyve does not remove a VM when it exits. Until something destroys it,
// the VM keeps its name and its guest memory, and a new bhyve of the same
// name refuses to start. A supervisor that crashed, or anything that ran
// bhyve by hand, leaves these behind.
//
// A VM held by a running bhyve is never touched: destroying it would pull
// the guest out from under a live process.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	bhyve "github.com/zombocoder/go-freebsd-bhyve"
)

const vmmDir = "/dev/vmm"

func main() {
	log.SetFlags(0)
	log.SetPrefix("cleanup: ")

	destroy := flag.Bool("destroy", false, "destroy VMs that no bhyve process holds")
	prefix := flag.String("prefix", "", "only consider VMs whose name starts with this")
	flag.Parse()

	entries, err := os.ReadDir(vmmDir)
	if os.IsNotExist(err) {
		fmt.Println("no virtual machines (vmm(4) has none, or is not loaded)")
		return
	}
	if err != nil {
		log.Fatal(err)
	}

	held := runningBhyves()

	var orphans []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, *prefix) {
			continue
		}
		if pid, ok := held[name]; ok {
			fmt.Printf("%-30s running (bhyve pid %s)\n", name, pid)
			continue
		}
		fmt.Printf("%-30s left behind\n", name)
		orphans = append(orphans, name)
	}

	if len(orphans) == 0 {
		return
	}
	if !*destroy {
		fmt.Printf("\n%d left behind; run with -destroy to remove them\n", len(orphans))
		return
	}

	failed := false
	for _, name := range orphans {
		if err := bhyve.DestroyByName(name); err != nil {
			log.Print(err)
			failed = true
			continue
		}
		fmt.Printf("destroyed %s\n", name)
	}
	if failed {
		os.Exit(1)
	}
}

// runningBhyves maps VM names to the pid of the bhyve process running
// them. bhyve takes the VM name as its last argument, which is what ps
// reports.
//
// This shells out to ps(1) because it is an example: it is the
// supervisor's job, not the library's, to know which processes it
// started, and a real one would keep its own record rather than guess.
func runningBhyves() map[string]string {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,comm=,args=").Output()
	if err != nil {
		log.Printf("ps: %v; treating every VM as unheld", err)
		return nil
	}
	held := map[string]string{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 3 || fields[1] != "bhyve" {
			continue
		}
		held[fields[len(fields)-1]] = fields[0]
	}
	return held
}
