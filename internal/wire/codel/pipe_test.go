package codel

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPipe_RoundTrip(t *testing.T) {
	a, b := NewPipe(0, 0)
	defer a.Close()
	defer b.Close()

	if _, err := a.WriteTo([]byte("hello"), nil); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	buf := make([]byte, 64)
	n, _, err := b.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q, want %q", buf[:n], "hello")
	}

	// And the other direction.
	if _, err := b.WriteTo([]byte("world"), nil); err != nil {
		t.Fatalf("WriteTo (b->a): %v", err)
	}
	n, _, err = a.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom (a): %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Fatalf("got %q, want %q", buf[:n], "world")
	}
}

func TestPipe_ReadDeadline(t *testing.T) {
	a, b := NewPipe(0, 0)
	defer a.Close()
	defer b.Close()

	if err := a.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	_, _, err := a.ReadFrom(buf)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected a net.Error with Timeout()==true, got %v (%T)", err, err)
	}
}

func TestPipe_CloseUnblocksBothEnds(t *testing.T) {
	a, b := NewPipe(0, 0)
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		_, _, _ = a.ReadFrom(buf)
		close(doneA)
	}()
	go func() {
		buf := make([]byte, 64)
		_, _, _ = b.ReadFrom(buf)
		close(doneB)
	}()
	time.Sleep(10 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-doneA:
	case <-time.After(time.Second):
		t.Fatal("end a did not unblock after Close")
	}
	select {
	case <-doneB:
	case <-time.After(time.Second):
		t.Fatal("end b did not unblock after Close (a.Close should close both queues)")
	}
}

// TestPipe_PacedDrainAndOverload tests the realistic environment:
// End A writes packets, End B reads with simulated pacing (7ms per read).
// Verifies:
// 1. Monotonic packet delivery (no head-drop reordering).
// 2. Queue length never exceeds hardCap (30).
// 3. Short bursts pass without drops.
// 4. Overload sheds packets cleanly without stalling.
// 5. Clean, fast recovery once overload subsides.
func TestPipe_PacedDrainAndOverload(t *testing.T) {
	a, b := NewPipe(30, 30)
	defer a.Close()
	defer b.Close()

	var mu sync.Mutex
	var receivedSeq []uint32

	done := make(chan struct{})
	// Reader goroutine: reads and simulates 5ms shaper pacing per packet
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			n, _, err := b.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= 4 {
				seq := binary.BigEndian.Uint32(buf[:4])
				mu.Lock()
				receivedSeq = append(receivedSeq, seq)
				mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	sendPacket := func(seq uint32) {
		p := make([]byte, 4)
		binary.BigEndian.PutUint32(p, seq)
		_, _ = a.WriteTo(p, nil)
	}

	// 1. Normal traffic (10 packets spaced at 10ms > 5ms drain time)
	for i := uint32(1); i <= 10; i++ {
		sendPacket(i)
		time.Sleep(10 * time.Millisecond)
	}

	// 2. Short burst (10 packets sent instantaneously < hardCap 30)
	for i := uint32(11); i <= 20; i++ {
		sendPacket(i)
	}
	time.Sleep(80 * time.Millisecond)

	// 3. Heavy overload (100 packets sent with 1ms gap into 5ms drain)
	for i := uint32(21); i <= 120; i++ {
		sendPacket(i)
		time.Sleep(1 * time.Millisecond)
		st := a.WriteQueueStats()
		if st.QueueLen > 30 {
			t.Fatalf("queue length exceeded hardCap 30: len=%d", st.QueueLen)
		}
	}

	// 4. Recovery: wait for queue to drain
	time.Sleep(200 * time.Millisecond)

	// 5. Normal traffic after recovery
	for i := uint32(121); i <= 125; i++ {
		sendPacket(i)
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	_ = a.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()

	if len(receivedSeq) == 0 {
		t.Fatal("no packets received")
	}

	// Check that received sequence numbers are strictly monotonically increasing!
	// (No out-of-order packets, no head-drop resurrection, no corrupted sequence)
	var prev uint32
	for idx, seq := range receivedSeq {
		if seq <= prev {
			t.Fatalf("packet sequence violation at index %d: seq=%d <= prev=%d", idx, seq, prev)
		}
		prev = seq
	}

	// Packets 1-20 (normal + brief burst) must all have been delivered without loss
	for i := uint32(1); i <= 20; i++ {
		found := false
		for _, s := range receivedSeq {
			if s == i {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected packet %d (from normal/burst phase) to be delivered, but was lost", i)
		}
	}

	// Packets 121-125 (post-recovery) must all be delivered (no permanent stall!)
	for i := uint32(121); i <= 125; i++ {
		found := false
		for _, s := range receivedSeq {
			if s == i {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("post-recovery packet %d not delivered (stall detected!)", i)
		}
	}
}
