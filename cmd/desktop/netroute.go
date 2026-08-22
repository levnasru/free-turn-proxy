package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// errNoDefaultRoute is returned by defaultRouteInterface when the OS
// reports no default route at all (e.g. no network connectivity yet).
var errNoDefaultRoute = errors.New("netroute: no default route found")

// errRouteUnparseable is returned by defaultRouteInterface when the OS
// reports default route line(s) that exist but don't match the expected
// "dev <iface>" pattern — distinct from errNoDefaultRoute ("no route
// configured at all"), so callers/tests don't conflate a parse failure
// with a genuinely disconnected host.
var errRouteUnparseable = errors.New("netroute: default route present but interface unparseable")

var defaultRouteDevRegexp = regexp.MustCompile(`\bdev\s+(\S+)`)

// runCommand is a package-level var so tests can replace it with a canned
// fake instead of depending on a real network stack / real OS route table.
var runCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// isVirtualIface reports whether iface looks like a virtual/tunnel
// interface rather than a physical uplink — anything without a
// /sys/class/net/<iface>/device symlink. Package-level var so tests can
// replace it, same pattern as runCommand. Shared between
// netroute_linux.go's defaultRouteInterface (find the real uplink to
// exclude from tun routes) and envcheck.go's rival-VPN check (an
// independently-written copy of this same check used to exist there and
// disagree with this one).
var isVirtualIface = func(iface string) bool {
	_, err := os.Stat(filepath.Join("/sys/class/net", iface, "device"))
	return err != nil
}
