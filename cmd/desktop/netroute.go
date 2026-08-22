package main

import (
	"errors"
	"os/exec"
)

// errNoDefaultRoute is returned by defaultRouteInterface when the OS
// reports no default route at all (e.g. no network connectivity yet).
var errNoDefaultRoute = errors.New("netroute: no default route found")

// runCommand is a package-level var so tests can replace it with a canned
// fake instead of depending on a real network stack / real OS route table.
var runCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
