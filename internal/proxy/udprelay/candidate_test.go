package udprelay

import (
	"testing"
	"time"
)

func TestGroupPrefix24(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		hostport string
		want    string
	}{
		{"ipv4 with port", "203.0.113.42:3478", "203.0.113"},
		{"ipv4 different last octet still same group", "203.0.113.7:443", "203.0.113"},
		{"ipv4 different /24", "203.0.114.7:3478", "203.0.114"},
		{"no port falls back to raw host", "203.0.113.42", "203.0.113.42"},
		{"unresolved hostname falls back to itself", "relay.example.internal:3478", "relay.example.internal:3478"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := groupPrefix24(c.hostport); got != c.want {
				t.Errorf("groupPrefix24(%q) = %q, want %q", c.hostport, got, c.want)
			}
		})
	}
}

func TestPickReplacementCandidate(t *testing.T) {
	t.Parallel()

	t.Run("no groups known yet - nothing to retire", func(t *testing.T) {
		t.Parallel()
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, map[int]string{})
		if got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("all groups distinct - nothing over-represented", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "b", 3: "c"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, groups)
		if got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("one group has two members - retires the non-active one", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, groups)
		if got != 2 {
			t.Errorf("got %d, want 2 (streamID 1 is active, must not be retired)", got)
		}
	})

	t.Run("active slot's own group never retires the active slot", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 2, groups)
		if got != 1 {
			t.Errorf("got %d, want 1 (streamID 2 is active, must not be retired)", got)
		}
	})

	t.Run("picks the most over-represented group", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "a", 4: "b", 5: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3, 4, 5}, 1, groups)
		if got != 2 && got != 3 {
			t.Errorf("got %d, want one of the non-active members of the 3-strong group a (2 or 3)", got)
		}
	})
}

func TestShouldRefresh(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before interval elapsed", base.Add(4 * time.Minute), false},
		{"exactly at interval", base.Add(5 * time.Minute), true},
		{"past interval", base.Add(10 * time.Minute), true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRefresh(base, c.now, interval); got != c.want {
				t.Errorf("shouldRefresh(%v, %v, %v) = %v, want %v", base, c.now, interval, got, c.want)
			}
		})
	}
}
