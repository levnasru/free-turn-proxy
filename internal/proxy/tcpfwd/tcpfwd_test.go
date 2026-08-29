package tcpfwd

import (
	"testing"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/proxy/common"
)

func TestSessionRotateDeadlineStaggersById(t *testing.T) {
	t.Parallel()
	connectedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	d1 := sessionRotateDeadline(connectedAt, 1)
	d2 := sessionRotateDeadline(connectedAt, 2)
	if d1.Equal(d2) {
		t.Fatalf("expected different deadlines for different stream IDs, got same: %s", d1)
	}
}

func TestSessionRotateDeadlineIsDeterministic(t *testing.T) {
	t.Parallel()
	connectedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	got1 := sessionRotateDeadline(connectedAt, 7)
	got2 := sessionRotateDeadline(connectedAt, 7)
	if !got1.Equal(got2) {
		t.Fatalf("expected same stream ID to always produce the same deadline, got %s and %s", got1, got2)
	}
}

func TestSessionRotateDeadlineWithinMargin(t *testing.T) {
	t.Parallel()
	connectedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	for _, streamID := range []int{0, 1, 5, 9, 10, 11, 100} {
		got := sessionRotateDeadline(connectedAt, streamID)
		min := connectedAt.Add(common.CredentialSafetyMargin)
		max := connectedAt.Add(2 * common.CredentialSafetyMargin)
		if got.Before(min) || !got.Before(max) {
			t.Fatalf("streamID=%d: deadline %s outside [%s, %s)", streamID, got, min, max)
		}
	}
}
