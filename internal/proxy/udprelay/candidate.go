package udprelay

import (
	"fmt"
	"net"
)

// groupPrefix24 returns the /24 group key for a "host:port" relay address:
// the first three octets of an IPv4 host. Non-IPv4 hosts (unresolved
// hostnames, IPv6, or anything net.SplitHostPort can't parse) fall back to
// the raw input string as their own singleton group - grouping degrades to
// a no-op instead of erroring, matching the spec's "not worse than random"
// ceiling (docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md,
// "Не цели").
func groupPrefix24(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port found, return raw input
		return hostport
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		// Not IPv4, return raw input
		return hostport
	}
	return fmt.Sprintf("%d.%d.%d", ip[0], ip[1], ip[2])
}
