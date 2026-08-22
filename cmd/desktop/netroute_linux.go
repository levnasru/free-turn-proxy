//go:build linux

package main

import (
	"fmt"
	"strings"
)

// defaultRouteInterface asks the kernel routing table for the interface the
// default (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink,
// as opposed to the tun device this process is about to create.
// It skips virtual interfaces (like other VPNs' tun devices) via
// isVirtualIface (netroute.go).
func defaultRouteInterface() (string, error) {
	out, err := runCommand("ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: ip route show default: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := defaultRouteDevRegexp.FindStringSubmatch(line)
		if m != nil {
			iface := m[1]
			if !isVirtualIface(iface) {
				return iface, nil
			}
		}
	}
	// Fallback to the first one if no physical device found (e.g. in
	// containers, or an uplink that's inherently virtual like PPPoE).
	if len(lines) > 0 && lines[0] != "" {
		m := defaultRouteDevRegexp.FindStringSubmatch(lines[0])
		if m != nil {
			return m[1], nil
		}
		return "", fmt.Errorf("%w: %q", errRouteUnparseable, lines[0])
	}
	return "", errNoDefaultRoute
}
