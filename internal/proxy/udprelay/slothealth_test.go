package udprelay

import (
	"testing"
	"time"
)

func TestSlotHealthRecordsRTTAndThroughput(t *testing.T) {
	h := newSlotHealth()

	if got := h.rtt(); got != 0 {
		t.Fatalf("expected zero RTT before first probe, got %s", got)
	}

	h.recordRTT(42 * time.Millisecond)
	if got := h.rtt(); got != 42*time.Millisecond {
		t.Fatalf("expected 42ms RTT, got %s", got)
	}

	h.stats.AddTx(100)
	h.stats.AddRx(50)
	tx, rx := h.stats.Counters()
	if tx != 100 || rx != 50 {
		t.Fatalf("expected tx=100 rx=50, got tx=%d rx=%d", tx, rx)
	}
}
