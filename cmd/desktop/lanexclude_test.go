// cmd/desktop/lanexclude_test.go
package main

import (
	"sort"
	"testing"
)

func TestPublicRoutesExcludePrivateRanges(t *testing.T) {
	routes := publicRoutes()
	if len(routes) == 0 {
		t.Fatal("publicRoutes() returned no routes")
	}

	excluded := []string{
		"192.168.1.1", "10.0.0.5", "172.16.5.5", "169.254.1.1",
		"127.0.0.1", "255.255.255.255", "224.0.0.1",
	}
	for _, ip := range excluded {
		if routeCovers(routes, ip) {
			t.Errorf("publicRoutes() unexpectedly covers private/reserved address %s", ip)
		}
	}

	included := []string{"8.8.8.8", "1.1.1.1", "91.231.135.181"}
	for _, ip := range included {
		if !routeCovers(routes, ip) {
			t.Errorf("publicRoutes() does not cover public address %s", ip)
		}
	}
}

func TestPublicRoutesDoNotOverlap(t *testing.T) {
	routes := publicRoutes()
	var ranges []ipv4Range
	for _, cidr := range routes {
		r, err := cidrToRange(cidr)
		if err != nil {
			t.Fatalf("publicRoutes() produced invalid CIDR %q: %v", cidr, err)
		}
		ranges = append(ranges, r)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].lo < ranges[j].lo })
	for i := 1; i < len(ranges); i++ {
		if ranges[i].lo <= ranges[i-1].hi {
			t.Fatalf("overlapping routes: %s and %s", routes[i-1], routes[i])
		}
	}
}

// routeCovers reports whether ip falls inside any of the given CIDR routes.
func routeCovers(routes []string, ip string) bool {
	target, err := ipToUint32(ip)
	if err != nil {
		panic(err)
	}
	for _, cidr := range routes {
		r, err := cidrToRange(cidr)
		if err != nil {
			panic(err)
		}
		if target >= r.lo && target <= r.hi {
			return true
		}
	}
	return false
}
