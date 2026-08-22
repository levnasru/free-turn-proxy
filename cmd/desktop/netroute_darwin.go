// cmd/desktop/netroute_darwin.go
//go:build darwin

package main

// defaultRouteInterface is not implemented on macOS yet — vk-turn (tun)
// needs this to find the real physical uplink to exclude from the tun
// routes (see cmd/desktop/tun.go), which nothing on this platform provides
// yet. vk-turn (socks) and xray-подписка don't call this at all.
func defaultRouteInterface() (string, error) {
	return "", errTunNotSupportedDarwin
}
