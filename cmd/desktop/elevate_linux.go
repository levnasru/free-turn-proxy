//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// isElevated reports whether this process already has root — Linux's only
// notion of "administrator" for the CAP_NET_ADMIN this mode needs to create
// a TUN device.
func isElevated() bool {
	return os.Geteuid() == 0
}

// relaunchElevated re-execs the current binary under pkexec (graphical
// polkit auth dialog) with extraArgs appended, and blocks until it exits.
// A declined/cancelled auth prompt returns a non-nil error.
func relaunchElevated(extraArgs []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relaunchElevated: resolve self path: %w", err)
	}
	args := append([]string{self}, extraArgs...)
	cmd := exec.Command("pkexec", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
