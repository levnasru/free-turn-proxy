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
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | "+
			"Where-Object { $_.InterfaceAlias -notmatch '(?i)(wintun|wireguard|tap|tun|tailscale|zerotier|hyper-v|virtual|vkturn)' } | "+
			"ForEach-Object { "+
			"[PSCustomObject]@{ Alias = $_.InterfaceAlias; "+
			"Metric = $_.RouteMetric + (Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4).InterfaceMetric } "+
			"} | Sort-Object Metric | Select-Object -First 1 -ExpandProperty Alias)")

	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: Get-NetRoute: %w", err)
	}
	iface := strings.TrimSpace(out)
	if iface == "" {
		return "", errNoDefaultRoute
	}
	return iface, nil
}
