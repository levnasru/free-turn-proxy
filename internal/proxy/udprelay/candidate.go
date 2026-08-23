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

// pickReplacementCandidate chooses which non-active hot-set member to
// retire at the next refresh tick: the member whose relay's /24 group has
// the most OTHER representatives currently in the hot-set (over-represented
// groups first), so refresh churn actively pushes toward group diversity
// instead of picking at random. groups maps streamID -> its relay's /24 key;
// a streamID missing from groups (still connecting, or connection failed
// before Params.OnAllocated fired) is never picked - retiring a slot we
// know nothing about yet would be guessing, not measuring. Returns -1 if no
// eligible member exists (empty hot-set, every known group has exactly one
// representative, or no group memberships are known yet).
func pickReplacementCandidate(streamIDs []int, activeStreamID int, groups map[int]string) int {
	counts := make(map[string]int, len(streamIDs))
	for _, id := range streamIDs {
		if g, ok := groups[id]; ok {
			counts[g]++
		}
	}

	best := -1
	bestCount := 1 // только группы с >=2 представителями считаются избыточными
	for _, id := range streamIDs {
		if id == activeStreamID {
			continue
		}
		g, ok := groups[id]
		if !ok {
			continue
		}
		if counts[g] > bestCount {
			bestCount = counts[g]
			best = id
		}
	}
	return best
}
