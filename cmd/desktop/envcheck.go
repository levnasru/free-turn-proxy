package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// checkEnvironment validates that the required ports are available
// and (if checkVPN is true) no conflicting VPN tunnels are currently routing the default traffic.
func checkEnvironment(checkVPN bool) error {
	// 1. Check if ports are available
	for _, port := range []int{vkTurnLocalSocksPort, 9000} {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("Порт %d уже занят. Пожалуйста, убедитесь, что не запущен другой экземпляр приложения или другой VPN, использующий этот порт", port)
		}
		l.Close()
	}

	// 2. Check for other active VPNs on Linux
	if checkVPN && runtime.GOOS == "linux" {
		out, err := runCommand("ip", "route", "show", "default")
		if err == nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				m := defaultRouteDevRegexp.FindStringSubmatch(line)
				if m != nil {
					iface := m[1]
					// If the default route goes through an interface without a physical device symlink,
					// it is likely a virtual tunnel (like another VPN).
					if _, err := os.Stat(filepath.Join("/sys/class/net", iface, "device")); err != nil {
						// Ignore our own interface if it's already created for some reason
						if iface != vkTurnTunInterfaceName {
							return fmt.Errorf("Обнаружен другой активный VPN (маршрут через интерфейс '%s'). Пожалуйста, выключите его перед запуском TUN-режима.", iface)
						}
					}
				}
			}
		}
	}
	return nil
}
