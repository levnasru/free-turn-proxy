//go:build linux

package main

import (
	"fmt"
	"regexp"
	"strings"
)

var defaultRouteDevRegexp = regexp.MustCompile(`\bdev\s+(\S+)`)

// defaultRouteInterface asks the kernel routing table for the interface the
// default (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink,
// as opposed to the tun device this process is about to create.
func defaultRouteInterface() (string, error) {
	out, err := runCommand("ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: ip route show default: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if line == "" {
		return "", errNoDefaultRoute
	}
	m := defaultRouteDevRegexp.FindStringSubmatch(line)
	if m == nil {
		return "", fmt.Errorf("defaultRouteInterface: could not parse %q", line)
	}
	return m[1], nil
}
