// cmd/desktop/lanexclude.go
package main

import (
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// privateIPv4CIDRs mirrors turn-proxy-android's PRIVATE_IPV4_CIDRS
// (WireGuardTunnelManager.kt) — kept identical rather than independently
// curated, so LAN devices (printer/NAS/router) stay reachable outside the
// tunnel the same way on every platform.
var privateIPv4CIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
	"127.0.0.0/8", "255.255.255.255/32", "224.0.0.0/4",
}

type ipv4Range struct{ lo, hi uint32 } // both inclusive

func ipToUint32(ip string) (uint32, error) {
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return 0, fmt.Errorf("lanexclude: malformed IPv4 %q", ip)
	}
	var v uint32
	for _, o := range octets {
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 || n > 255 {
			return 0, fmt.Errorf("lanexclude: bad octet %q in %q", o, ip)
		}
		v = v<<8 | uint32(n)
	}
	return v, nil
}

func cidrToRange(cidr string) (ipv4Range, error) {
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) != 2 {
		return ipv4Range{}, fmt.Errorf("lanexclude: malformed CIDR %q", cidr)
	}
	base, err := ipToUint32(parts[0])
	if err != nil {
		return ipv4Range{}, err
	}
	prefix, err := strconv.Atoi(parts[1])
	if err != nil || prefix < 0 || prefix > 32 {
		return ipv4Range{}, fmt.Errorf("lanexclude: bad prefix in %q", cidr)
	}
	hostBits := 32 - prefix
	var size uint32 = 1
	if hostBits > 0 {
		size = uint32(1) << uint(hostBits)
	}
	return ipv4Range{lo: base, hi: base + size - 1}, nil
}

// mergeRanges sorts and coalesces overlapping/adjacent ranges.
func mergeRanges(ranges []ipv4Range) []ipv4Range {
	if len(ranges) == 0 {
		return nil
	}
	sorted := append([]ipv4Range(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].lo < sorted[j].lo })
	merged := []ipv4Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// gaps returns the parts of the full IPv4 space [0, 2^32-1] not covered by
// excluded (must already be merged and sorted by mergeRanges).
func gaps(excluded []ipv4Range) []ipv4Range {
	var out []ipv4Range
	var cursor uint32
	for _, r := range excluded {
		if r.lo > cursor {
			out = append(out, ipv4Range{lo: cursor, hi: r.lo - 1})
		}
		if r.hi == 0xFFFFFFFF {
			return out
		}
		cursor = r.hi + 1
	}
	out = append(out, ipv4Range{lo: cursor, hi: 0xFFFFFFFF})
	return out
}

// rangeToCIDRs splits [lo,hi] into the minimal set of CIDR-aligned blocks.
func rangeToCIDRs(lo, hi uint32) []string {
	var out []string
	for {
		// alignBits: block size lo's own address alignment allows (32 = fully aligned, lo==0).
		alignBits := 32
		if lo != 0 {
			alignBits = bits.TrailingZeros32(lo)
		}
		// fitBits: largest power-of-two block size (as an exponent) that still fits in [lo,hi].
		fitBits := 32
		if !(lo == 0 && hi == 0xFFFFFFFF) {
			fitBits = bits.Len32(hi-lo+1) - 1
		}
		blockBits := alignBits
		if fitBits < blockBits {
			blockBits = fitBits
		}
		prefix := 32 - blockBits
		blockSize := uint32(1) << uint(blockBits)
		out = append(out, fmt.Sprintf("%d.%d.%d.%d/%d", byte(lo>>24), byte(lo>>16), byte(lo>>8), byte(lo), prefix))
		if hi-lo+1 == blockSize {
			break
		}
		lo += blockSize
	}
	return out
}

// publicRoutes returns the IPv4 public-internet address space as a minimal
// set of CIDR blocks: the full space minus privateIPv4CIDRs. Used as
// autoSystemRoutingTable for the tun xray inbound, so LAN devices stay
// reachable outside the tunnel (same policy as Android's
// RealityVpnService.excludeLanFromAllowedIps) instead of routing a blanket
// 0.0.0.0/0.
func publicRoutes() []string {
	var excluded []ipv4Range
	for _, c := range privateIPv4CIDRs {
		r, err := cidrToRange(c)
		if err != nil {
			// privateIPv4CIDRs is a fixed compile-time literal — a parse
			// failure here is a bug in this file, not a runtime condition.
			panic(fmt.Sprintf("lanexclude: invalid entry in privateIPv4CIDRs: %v", err))
		}
		excluded = append(excluded, r)
	}
	var out []string
	for _, g := range gaps(mergeRanges(excluded)) {
		out = append(out, rangeToCIDRs(g.lo, g.hi)...)
	}
	return out
}
