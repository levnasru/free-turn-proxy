// cmd/desktop/wireguard.go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// WGConfigParsed contains the connection parameters extracted from a WireGuard .conf.
type WGConfigParsed struct {
	PrivateKey string
	Address    string
	PublicKey  string
	Endpoint   string
	MTU        int
	Keepalive  int
}

// parseWGConfig extracts WireGuard credentials and peer info from an INI-like WireGuard config.
func parseWGConfig(raw string) (*WGConfigParsed, error) {
	parsed := &WGConfigParsed{
		MTU:       1050,
		Keepalive: 15,
		Endpoint:  "127.0.0.1:9000",
	}

	scanner := bufio.NewScanner(strings.NewReader(raw))
	currentSection := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.ToLower(line[1 : len(line)-1])
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch currentSection {
		case "interface":
			switch key {
			case "privatekey":
				parsed.PrivateKey = val
			case "address":
				// Could be comma-separated; take the IPv4 address
				for _, addr := range strings.Split(val, ",") {
					addr = strings.TrimSpace(addr)
					if strings.Contains(addr, ".") {
						parsed.Address = addr
						break
					}
				}
				if parsed.Address == "" {
					parsed.Address = val
				}
			case "mtu":
				if m, err := strconv.Atoi(val); err == nil && m > 0 {
					parsed.MTU = m
				}
			}
		case "peer":
			switch key {
			case "publickey":
				parsed.PublicKey = val
			case "endpoint":
				parsed.Endpoint = val
			case "persistentkeepalive":
				if k, err := strconv.Atoi(val); err == nil && k >= 0 {
					parsed.Keepalive = k
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read wireguard config: %w", err)
	}

	if parsed.PrivateKey == "" {
		return nil, fmt.Errorf("missing PrivateKey in wireguard config")
	}
	if parsed.PublicKey == "" {
		return nil, fmt.Errorf("missing PublicKey in wireguard config")
	}
	if parsed.Address == "" {
		return nil, fmt.Errorf("missing Address in wireguard config")
	}
	if !strings.Contains(parsed.Address, "/") {
		parsed.Address += "/32"
	}

	return parsed, nil
}

// resolveBypassDomainsToIPs performs DNS lookups on bypass domains to convert
// them into IPv4 /32 subnets so they can be excluded from WireGuard's AllowedIPs.
func resolveBypassDomainsToIPs(domains []string) []string {
	if len(domains) == 0 {
		return nil
	}
	var ips []string
	seen := make(map[string]bool)
	for _, domain := range domains {
		d := strings.TrimSpace(domain)
		d = strings.TrimPrefix(d, "domain:")
		d = strings.TrimPrefix(d, "full:")
		if d == "" || strings.Contains(d, "/") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		resolved, err := net.DefaultResolver.LookupIP(ctx, "ip4", d)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range resolved {
			str := ip.String() + "/32"
			if !seen[str] {
				seen[str] = true
				ips = append(ips, str)
			}
		}
	}
	return ips
}

// buildNativeWGConf produces a standard WireGuard configuration file (.conf)
// suitable for wg-quick or wg setconf.
func buildNativeWGConf(parsed *WGConfigParsed, routes []string, withDNS bool) string {
	mtu := parsed.MTU
	if mtu <= 0 {
		mtu = 1050
	}
	endpoint := parsed.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("127.0.0.1:%d", vkTurnClientListenPort)
	}
	keepalive := parsed.Keepalive
	if keepalive <= 0 {
		keepalive = 15
	}

	allowedIPs := strings.Join(routes, ", ")
	if allowedIPs == "" {
		allowedIPs = "0.0.0.0/0"
	}

	var sb strings.Builder
	sb.WriteString("[Interface]\n")
	sb.WriteString(fmt.Sprintf("PrivateKey = %s\n", parsed.PrivateKey))
	sb.WriteString(fmt.Sprintf("Address = %s\n", parsed.Address))
	sb.WriteString(fmt.Sprintf("MTU = %d\n", mtu))
	if withDNS {
		sb.WriteString("DNS = 1.1.1.1\n")
	}
	sb.WriteString("\n[Peer]\n")
	sb.WriteString(fmt.Sprintf("PublicKey = %s\n", parsed.PublicKey))
	sb.WriteString(fmt.Sprintf("Endpoint = %s\n", endpoint))
	sb.WriteString(fmt.Sprintf("AllowedIPs = %s\n", allowedIPs))
	sb.WriteString(fmt.Sprintf("PersistentKeepalive = %d\n", keepalive))

	return sb.String()
}
