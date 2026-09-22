package reseq

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestHeaderWrapUnwrap(t *testing.T) {
	orig := []byte("hello wireguard packet")
	seq := uint32(12345)
	epoch := uint16(42)

	wrapped := Wrap(nil, orig, seq, epoch)
	if !IsReseq(wrapped) {
		t.Fatalf("expected IsReseq to be true")
	}

	gotSeq, gotEpoch, gotPayload, ok := Unwrap(wrapped)
	if !ok {
		t.Fatalf("expected Unwrap ok")
	}
	if gotSeq != seq {
		t.Errorf("seq mismatch: got %d, want %d", gotSeq, seq)
	}
	if gotEpoch != epoch {
		t.Errorf("epoch mismatch: got %d, want %d", gotEpoch, epoch)
	}
	if !bytes.Equal(gotPayload, orig) {
		t.Errorf("payload mismatch: got %q, want %q", gotPayload, orig)
	}
}

func TestLegacyWireGuardDetection(t *testing.T) {
	// WireGuard packets start with 0x01, 0x02, 0x03, 0x04 followed by three zero bytes
	wgData := []byte{0x04, 0x00, 0x00, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	if IsReseq(wgData) {
		t.Fatalf("WireGuard packet should not be detected as Reseq")
	}
	_, _, _, ok := Unwrap(wgData)
	if ok {
		t.Fatalf("Unwrap should fail for WireGuard packet")
	}
}

func TestResequencerInOrder(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	for i := 1; i <= 5; i++ {
		r.Push(uint32(i), []byte{byte(i)})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 5 {
		t.Fatalf("expected 5 delivered, got %d", len(delivered))
	}
	for i := 0; i < 5; i++ {
		if delivered[i] != i+1 {
			t.Errorf("pos %d: got %d, want %d", i, delivered[i], i+1)
		}
	}
}

func TestResequencerOutOrderReordering(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	r := New(50*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Push sequence: 1, 3, 2, 5, 4
	r.Push(1, []byte{1})
	r.Push(3, []byte{3})
	r.Push(2, []byte{2})
	r.Push(5, []byte{5})
	r.Push(4, []byte{4})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 5 {
		t.Fatalf("expected 5 delivered, got %d", len(delivered))
	}
	for i := 0; i < 5; i++ {
		if delivered[i] != i+1 {
			t.Errorf("pos %d: got %d, want %d", i, delivered[i], i+1)
		}
	}
}

func TestResequencerDwellTimeout(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	// 15ms dwell timeout
	r := New(15*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Send packet 1, skip 2, send packet 3
	r.Push(1, []byte{1})
	r.Push(3, []byte{3})

	mu.Lock()
	if len(delivered) != 1 || delivered[0] != 1 {
		t.Fatalf("expected only packet 1 delivered initially, got %v", delivered)
	}
	mu.Unlock()

	// Wait for dwell timeout to expire hole at 2
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("expected 2 delivered after timeout, got %d (%v)", len(delivered), delivered)
	}
	if delivered[1] != 3 {
		t.Errorf("second delivered packet should be 3, got %d", delivered[1])
	}
}

func TestResequencerMultiPacketBurstLossTimeout(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	// 15ms dwell timeout
	r := New(15*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Send packet 1, skip 2, 3, 4, 5, send packet 6 and 7
	r.Push(1, []byte{1})
	r.Push(6, []byte{6})
	r.Push(7, []byte{7})

	mu.Lock()
	if len(delivered) != 1 || delivered[0] != 1 {
		t.Fatalf("expected only packet 1 delivered initially, got %v", delivered)
	}
	mu.Unlock()

	// Wait 35ms: in ONE dwell timeout cycle, packets 2..5 should be skipped and 6, 7 delivered
	time.Sleep(35 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 {
		t.Fatalf("expected 3 delivered (1, 6, 7) after single dwell timeout, got %d (%v)", len(delivered), delivered)
	}
	if delivered[1] != 6 || delivered[2] != 7 {
		t.Errorf("expected [1, 6, 7], got %v", delivered)
	}
}

func TestResequencerStaleDuplicatesIgnored(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	r.Push(1, []byte{1})
	r.Push(2, []byte{2})
	// Duplicate 1 arrives late
	r.Push(1, []byte{1})
	r.Push(3, []byte{3})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 {
		t.Fatalf("expected 3 delivered, got %d (%v)", len(delivered), delivered)
	}
}

func TestResequencerSequenceReset(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Initial session packets 1 and 2
	r.Push(1, []byte{10})
	r.Push(2, []byte{11})

	mu.Lock()
	if len(delivered) != 2 {
		t.Fatalf("expected 2 delivered, got %d", len(delivered))
	}
	mu.Unlock()

	// Advance sequence to simulate a long session
	for i := uint32(3); i <= 100; i++ {
		r.Push(i, []byte{byte(i)})
	}

	mu.Lock()
	if len(delivered) != 100 {
		t.Fatalf("expected 100 delivered, got %d", len(delivered))
	}
	mu.Unlock()

	// Client restarts and sequence restarts from 1!
	r.Push(1, []byte{1})
	r.Push(2, []byte{2})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 102 {
		t.Fatalf("expected 102 delivered (reset should accept seq=1), got %d (%v)", len(delivered), delivered)
	}
	if delivered[100] != 1 || delivered[101] != 2 {
		t.Errorf("expected packets 1 and 2 after reset, got %v", delivered[100:])
	}
}

func TestResequencerStats(t *testing.T) {
	var delivered []int
	var mu sync.Mutex
	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	r.Push(1, []byte{1})
	r.Push(3, []byte{3})
	// Wait for gap 2 to expire
	time.Sleep(35 * time.Millisecond)

	// Late packet 2 arrives (timed-out gap) -> delivered!
	r.Push(2, []byte{2})

	// Duplicate of already-delivered packet 3 arrives -> dropped!
	r.Push(3, []byte{3})

	st := r.Stats()
	if st.Pushed != 4 {
		t.Errorf("expected Pushed=4, got %d", st.Pushed)
	}
	if st.Delivered != 2 { // 1 and 3 delivered contiguous
		t.Errorf("expected Delivered=2, got %d", st.Delivered)
	}
	if st.GapsTimedOut != 1 { // gap 2 timed out
		t.Errorf("expected GapsTimedOut=1, got %d", st.GapsTimedOut)
	}
	if st.LateDelivered != 1 { // late 2 delivered
		t.Errorf("expected LateDelivered=1, got %d", st.LateDelivered)
	}
	if st.StaleDropped != 1 { // duplicate 1 dropped
		t.Errorf("expected StaleDropped=1, got %d", st.StaleDropped)
	}

	mu.Lock()
	defer mu.Unlock()
	// Delivered should contain 1, 3, and then 2!
	if len(delivered) != 3 || delivered[0] != 1 || delivered[1] != 3 || delivered[2] != 2 {
		t.Errorf("expected delivered [1, 3, 2], got %v", delivered)
	}
}

func TestResequencerDwellNoIndefiniteAccumulation(t *testing.T) {
	var delivered []int
	var mu sync.Mutex
	// 20ms dwell timeout
	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// In-order packet 1
	r.Push(1, []byte{1})

	// Burst out-of-order packets arriving together: 4 and 7 (gaps at 2, 3 and 5, 6)
	r.Push(4, []byte{4})
	r.Push(7, []byte{7})

	mu.Lock()
	if len(delivered) != 1 || delivered[0] != 1 {
		t.Fatalf("expected only packet 1 delivered initially, got %v", delivered)
	}
	mu.Unlock()

	// Wait 35ms (> 20ms dwell, but < 40ms double-dwell)
	time.Sleep(35 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// Both 4 and 7 should be delivered without waiting for a second 20ms dwell period!
	if len(delivered) != 3 {
		t.Fatalf("expected 3 delivered (1, 4, 7), got %d (%v)", len(delivered), delivered)
	}
	if delivered[1] != 4 || delivered[2] != 7 {
		t.Errorf("expected [1, 4, 7], got %v", delivered)
	}
}

func TestResequencerNaturalWrapAround(t *testing.T) {
	var delivered []int
	var mu sync.Mutex
	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Advance sequence close to 2^32 - 1
	startSeq := uint32(4294967293) // ^uint32(0) - 2
	r.mu.Lock()
	r.baseSeq = startSeq
	r.mu.Unlock()

	r.Push(startSeq, []byte{1})
	r.Push(startSeq+1, []byte{2})
	r.Push(startSeq+2, []byte{3}) // 4294967295
	r.Push(0, []byte{4})          // wrap to 0
	r.Push(1, []byte{5})          // wrap to 1 (must NOT trigger false reset!)
	r.Push(2, []byte{6})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 6 {
		t.Fatalf("expected 6 delivered across 32-bit wrap-around, got %d (%v)", len(delivered), delivered)
	}
	for i, expected := range []int{1, 2, 3, 4, 5, 6} {
		if delivered[i] != expected {
			t.Errorf("index %d: expected %d, got %d", i, expected, delivered[i])
		}
	}
}

func TestResequencerLateZeroPacket(t *testing.T) {
	var delivered []int
	var mu sync.Mutex
	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Initial packet wraps near 0: packet 4294967295 arrives
	startSeq := uint32(4294967295)
	r.mu.Lock()
	r.baseSeq = startSeq
	r.mu.Unlock()
	r.Push(startSeq, []byte{100})

	// Packet 0 is dropped in transit. Packet 1 arrives!
	r.Push(1, []byte{1})

	// Wait for dwell timeout to expire on missing packet 0
	time.Sleep(35 * time.Millisecond)

	// Now late packet 0 arrives! It must be delivered via LateDelivered
	r.Push(0, []byte{0})

	mu.Lock()
	defer mu.Unlock()
	st := r.Stats()
	if st.LateDelivered != 1 {
		t.Errorf("expected 1 LateDelivered for late seq=0, got %d", st.LateDelivered)
	}
	foundZero := false
	for _, p := range delivered {
		if p == 0 {
			foundZero = true
			break
		}
	}
	if !foundZero {
		t.Errorf("expected payload 0 to be delivered to callback, got %v", delivered)
	}
}

func TestResequencerLatePacketOneDoesNotReset(t *testing.T) {
	var delivered []int
	var mu sync.Mutex
	r := New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer r.Close()

	// Initial packet 1 is delayed in transit. Packet 2 arrives first!
	r.Push(2, []byte{2})

	// Wait for dwell timeout to expire on missing packet 1
	time.Sleep(35 * time.Millisecond)

	// After timeout, packet 2 should have been delivered, and baseSeq advanced
	// Now late packet 1 arrives! It must be delivered as late packet WITHOUT resetting baseSeq
	r.Push(1, []byte{1})

	// Packet 3 arrives
	r.Push(3, []byte{3})

	mu.Lock()
	defer mu.Unlock()
	st := r.Stats()
	if st.LateDelivered != 1 {
		t.Errorf("expected 1 LateDelivered for late seq=1, got %d", st.LateDelivered)
	}
	if len(delivered) != 3 {
		t.Fatalf("expected 3 delivered packets, got %d: %v", len(delivered), delivered)
	}
}
