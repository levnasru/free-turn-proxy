//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultRouteInterface asks the kernel routing table for the interface the
// default (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink,
// as opposed to the tun device this process is about to create.
// It skips virtual interfaces (like other VPNs' tun devices) by checking
// for the presence of a /sys/class/net/<iface>/device symlink.
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
			if _, err := os.Stat(filepath.Join("/sys/class/net", iface, "device")); err == nil {
				return iface, nil
			}
		}
	}
	// Fallback to the first one if no physical device found (e.g. in containers)
	if len(lines) > 0 && lines[0] != "" {
		m := defaultRouteDevRegexp.FindStringSubmatch(lines[0])
		if m != nil {
			return m[1], nil
		}
	}
	return "", errNoDefaultRoute
}
