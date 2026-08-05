package common

import (
	"context"
	"net"
	"testing"
)

func TestCandidateIdx(t *testing.T) {
	t.Parallel()
	// Каждый стрим обходит всех кандидатов ровно по разу, старт - свой.
	for _, n := range []int{1, 2, 3, 5} {
		for streamID := 0; streamID < 12; streamID++ {
			seen := make(map[int]bool, n)
			for j := 0; j < n; j++ {
				seen[candidateIdx(streamID, j, n)] = true
			}
			if len(seen) != n {
				t.Fatalf("n=%d stream=%d: обошли %d кандидатов из %d", n, streamID, len(seen), n)
			}
			if got, want := candidateIdx(streamID, 0, n), streamID%n; got != want {
				t.Fatalf("n=%d stream=%d: старт %d, ожидали %d", n, streamID, got, want)
			}
		}
	}
}

func TestDialTURNNoCandidates(t *testing.T) {
	t.Parallel()
	getCreds := func(context.Context, int) (string, string, []string, error) {
		return "u", "p", nil, nil
	}
	peer := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1}
	_, err := DialTURN(context.Background(), "", "", false, peer, 0, getCreds)
	if err == nil {
		t.Fatal("expected error on empty candidate list")
	}
}
