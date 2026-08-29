package udprelay

import (
	"fmt"
	"net"
	"time"
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

// pickAgeExpiredCandidate returns the streamID of the OLDEST hot-set member
// whose age (now - launchedAt) is at or past margin, or -1 if none qualify
// yet (P14, 2026-08-29: no TURN allocation should silently outlive
// common.CredentialSafetyMargin). Unlike pickReplacementCandidate's
// group-diversity heuristic - which permanently stalls once the pool is
// fully spread across groups, see its doc comment - age always eventually
// fires regardless of group convergence, so refreshOne tries this first and
// only falls back to diversity when nothing is age-eligible. Deliberately
// does NOT exempt the dispatcher's active streamID: post-round-robin-rollback
// "active" carries no routing weight (see dispatcher.route()'s doc comment),
// so refreshOne is responsible for calling dispatcher.rotateManual() before
// retiring a chosen slot that happens to be active, rather than this
// function silently protecting it forever. A streamID missing from
// launchedAt (still connecting) is skipped, not guessed.
func pickAgeExpiredCandidate(streamIDs []int, launchedAt map[int]time.Time, now time.Time, margin time.Duration) int {
	oldestID := -1
	var oldestAge time.Duration
	for _, id := range streamIDs {
		t, ok := launchedAt[id]
		if !ok {
			continue
		}
		if age := now.Sub(t); age >= margin && age > oldestAge {
			oldestAge = age
			oldestID = id
		}
	}
	return oldestID
}

// pickSlotToRetireForShrink chooses which hot-set member to drop when
// shrinking K by one (Шаг 3, живой ресайз): the one with the worst measured
// RTT (see slotHealth) - the whole point of shrinking is to walk away from
// paths dragging avgRTT down, and that's the signal we already have from
// Шаг 1/2. Falls back to the first non-active member if nobody has an RTT
// sample yet (rtts all zero - fresh hot-set, no measurements landed) -
// retiring at random beats not shrinking at all when asked to. Never picks
// activeStreamID, same reasoning as pickReplacementCandidate. Returns -1 if
// nothing is eligible (empty, or the only member is active).
func pickSlotToRetireForShrink(streamIDs []int, activeStreamID int, rtts map[int]time.Duration) int {
	worst := -1
	var worstRTT time.Duration
	for _, id := range streamIDs {
		if id == activeStreamID {
			continue
		}
		if rtt := rtts[id]; rtt > worstRTT {
			worstRTT = rtt
			worst = id
		}
	}
	if worst >= 0 {
		return worst
	}
	for _, id := range streamIDs {
		if id != activeStreamID {
			return id
		}
	}
	return -1
}

// shouldRefresh reports whether interval has elapsed since lastRefresh, as
// of now. A plain function of three values, not a stateful ticker wrapper -
// keeps the refresh decision itself testable without a real clock or sleep.
func shouldRefresh(lastRefresh, now time.Time, interval time.Duration) bool {
	return !now.Before(lastRefresh.Add(interval))
}
