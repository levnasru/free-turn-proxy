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
	// pkexec sanitizes the environment (HOME -> /root, VKTURN_* dropped) —
	// without explicitly forwarding these, the elevated child can't find the
	// unprivileged user's cached config (CachePath reads $HOME/.vkturn/) and
	// falls back to a second interactive portal login, and
	// VKTURN_DEBUG/VKTURN_PORTAL_URL silently stop working right where
	// debugging matters most.
	envArgs := []string{"env"}
	if home := os.Getenv("HOME"); home != "" {
		envArgs = append(envArgs, "HOME="+home)
	}
	if v := os.Getenv("VKTURN_DEBUG"); v != "" {
		envArgs = append(envArgs, "VKTURN_DEBUG="+v)
	}
	if v := os.Getenv("VKTURN_PORTAL_URL"); v != "" {
		envArgs = append(envArgs, "VKTURN_PORTAL_URL="+v)
	}
	args := append(envArgs, self)
	args = append(args, extraArgs...)
	cmd := exec.Command("pkexec", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
