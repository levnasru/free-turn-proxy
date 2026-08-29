package common

import (
	"context"
	"net"
	"strings"
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
	_, err := DialTURN(context.Background(), "", "", false, peer, 0, 0, getCreds)
	if err == nil {
		t.Fatal("expected error on empty candidate list")
	}
}

// TestDialTURNCandidateOffset проверяет, что candidateOffset сдвигает
// стартовую точку перебора кандидатов независимо от streamID, переданного в
// getCreds - см. common.go's doc-comment у DialTURN про то, зачем это нужно
// tcpfwd.maintainSession (P14: генерация редайла не должна менять
// streamID-based credential-bucketing, только точку старта по кандидатам).
// Кандидаты - заведомо невалидные host:port строки: net.SplitHostPort падает
// синхронно, без сети, и DialTURN оборачивает каждую ошибку как "candidate:
// err", так что порядок попыток виден прямо в тексте агрегированной ошибки.
func TestDialTURNCandidateOffset(t *testing.T) {
	t.Parallel()
	rawURLs := []string{"cand-0", "cand-1", "cand-2", "cand-3"}
	getCreds := func(context.Context, int) (string, string, []string, error) {
		return "u", "p", rawURLs, nil
	}
	peer := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1}

	const streamID = 1
	const offset = 2
	_, err := DialTURN(context.Background(), "", "", false, peer, streamID, offset, getCreds)
	if err == nil {
		t.Fatal("expected error - all candidates are malformed placeholders")
	}

	wantFirst := rawURLs[candidateIdx(streamID+offset, 0, len(rawURLs))]
	gotFirstLine := strings.SplitN(err.Error(), "\n", 2)[0]
	if !strings.Contains(gotFirstLine, wantFirst) {
		t.Fatalf("first attempted candidate line = %q, want it to contain %q (full error: %s)", gotFirstLine, wantFirst, err)
	}
}
