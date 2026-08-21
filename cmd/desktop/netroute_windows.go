//go:build windows

package main

import (
	"fmt"
	"strings"
)

// defaultRouteInterface asks Windows for the interface alias the default
// (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink, as opposed
// to the wintun adapter this process is about to create.
func defaultRouteInterface() (string, error) {
	out, err := runCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Sort-Object -Property RouteMetric | "+
			"Select-Object -First 1 -ExpandProperty InterfaceAlias)")
	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: Get-NetRoute: %w", err)
	}
	iface := strings.TrimSpace(out)
	if iface == "" {
		return "", errNoDefaultRoute
	}
	return iface, nil
}
