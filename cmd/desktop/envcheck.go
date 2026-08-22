package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
)

// checkEnvironment validates the environment before starting a mode: it
// probes ports (best-effort — see below) and, if checkVPN is true, refuses
// to start when a virtual default route is shadowing a physical one (the
// signature of another VPN having grabbed the default route).
//
// ports is mode-specific: vk-turn (socks) binds both the local xray SOCKS
// bridge and the client's raw listener, vk-turn (tun) only binds the
// latter (its xray inbound is a tun device, not a TCP port) — see the two
// callers in main.go/tun.go.
func checkEnvironment(checkVPN bool, ports []int) error {
	// 1. Best-effort port probe: net.Listen+Close has an inherent TOCTOU gap
	// (nothing stops another process from grabbing the port between this
	// check and the real bind moments later), so its only value is a
	// clearer error message than whatever xray/the client would produce on
	// their own — not a hard gate. Warn and continue instead of failing
	// startup on a transient probe artifact.
	for _, port := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Предупреждение: порт %d похоже уже занят (%v) — возможно, запущен другой экземпляр приложения или другой VPN, использующий этот порт\n", port, err)
			continue
		}
		l.Close()
	}

	// 2. Check for another active VPN on Linux. Only refuse when a virtual
	// default route is shadowing a physical one that also exists — that
	// pattern (both present, virtual one taking priority) is the actual
	// "another VPN grabbed the default route" signature. A topology with
	// *only* virtual default routes (PPPoE-only via ppp0, bridge-primary,
	// a container's veth) is just the user's normal uplink and must be
	// allowed — see netroute_linux.go's defaultRouteInterface, which
	// already tolerates exactly these via its fallback behavior; this
	// check used to disagree with it via its own inline classification.
	if checkVPN && runtime.GOOS == "linux" {
		out, err := runCommand("ip", "route", "show", "default")
		if err == nil {
			var sawPhysical, sawVirtual bool
			var rivalIface string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				m := defaultRouteDevRegexp.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				iface := m[1]
				if iface == vkTurnTunInterfaceName {
					continue
				}
				if isVirtualIface(iface) {
					sawVirtual = true
					if rivalIface == "" {
						rivalIface = iface
					}
				} else {
					sawPhysical = true
				}
			}
			if sawPhysical && sawVirtual {
				return fmt.Errorf("Обнаружен другой активный VPN (маршрут через интерфейс '%s'). Пожалуйста, выключите его перед запуском TUN-режима.", rivalIface)
			}
		}
	}
	return nil
}
