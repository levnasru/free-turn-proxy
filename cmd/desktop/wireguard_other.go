// cmd/desktop/wireguard_other.go
//go:build !linux

package main

import (
	"context"
	"fmt"
)

func startNativeWGTunnel(ctx context.Context, parsed *WGConfigParsed, routes []string, physicalIface string) (func(), error) {
	return nil, fmt.Errorf("нативный WireGuard tun в настоящий момент поддержан только на Linux (используйте режим xray tun)")
}
