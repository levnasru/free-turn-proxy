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
