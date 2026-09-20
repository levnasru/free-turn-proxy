package codel

import (
	"testing"
	"time"
)

func TestQueue_PushPopOrder(t *testing.T) {
	q := NewQueue(10)
	want := [][]byte{[]byte("packet1"), []byte("packet2"), []byte("packet3")}
	for _, w := range want {
		q.Push(w)
	}
	for i, w := range want {
		got, err := q.Pop()
		if err != nil {
			t.Fatalf("Pop %d: %v", i, err)
		}
		if string(got) != string(w) {
			t.Fatalf("Pop %d: got %q, want %q", i, got, w)
		}
	}
}

func TestQueue_TailDrop_PreservesHead(t *testing.T) {
	q := NewQueue(3)
	q.Push([]byte("first"))
	q.Push([]byte("second"))
	q.Push([]byte("third"))
	// Excess packets should be tail-dropped
	q.Push([]byte("fourth"))
	q.Push([]byte("fifth"))

	st := q.Stats()
	if st.Dropped != 2 {
		t.Fatalf("expected 2 dropped packets, got %d", st.Dropped)
	}
	if st.Pushed != 3 {
		t.Fatalf("expected 3 pushed packets, got %d", st.Pushed)
	}

	// First packet in queue MUST remain intact
	got, err := q.Pop()
	if err != nil || string(got) != "first" {
		t.Fatalf("head packet corrupted or dropped: got %q, err %v", got, err)
	}
	got, err = q.Pop()
	if err != nil || string(got) != "second" {
		t.Fatalf("second packet corrupted: got %q, err %v", got, err)
	}
	got, err = q.Pop()
	if err != nil || string(got) != "third" {
		t.Fatalf("third packet corrupted: got %q, err %v", got, err)
	}
}

func TestQueue_BriefBurst_NoDrops(t *testing.T) {
	q := NewQueue(10)

	// Inject 3 packets with enqueued time = now - Target*2 (delay is high)
	q.mu.Lock()
	oldTime := time.Now().Add(-Target * 2)
	q.buf = append(q.buf,
		item{data: []byte("p1"), enqueued: oldTime},
		item{data: []byte("p2"), enqueued: oldTime},
		item{data: []byte("p3"), enqueued: oldTime},
	)
	q.mu.Unlock()

	// Popping immediately: firstAboveTime is armed, but Interval has NOT elapsed yet.
	// Therefore none of the packets should be dropped!
	for i := 0; i < 3; i++ {
		got, err := q.Pop()
		if err != nil {
			t.Fatalf("Pop %d failed: %v", i, err)
		}
		if len(got) == 0 {
			t.Fatalf("Pop %d returned empty data", i)
		}
	}

	st := q.Stats()
	if st.Dropped != 0 {
		t.Fatalf("expected 0 drops for brief burst, got %d", st.Dropped)
	}
	if st.Popped != 3 {
		t.Fatalf("expected 3 popped packets, got %d", st.Popped)
	}
}

func TestQueue_SustainedOverload_TriggersDrop(t *testing.T) {
	q := NewQueue(20)

	// Inject packets with enqueued time = now - (Target + Interval + 10ms)
	q.mu.Lock()
	overloadTime := time.Now().Add(-(Target + Interval + 10*time.Millisecond))
	for i := 0; i < 10; i++ {
		q.buf = append(q.buf, item{data: []byte("data"), enqueued: overloadTime})
	}
	// Arm firstAboveTime as if delay was already above target for Interval
	q.firstAboveTime = time.Now().Add(-time.Millisecond)
	q.mu.Unlock()

	// First pop should trigger drop state and drop a packet, returning the subsequent one
	got, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("unexpected data: %q", got)
	}

	st := q.Stats()
	if st.Dropped == 0 {
		t.Fatal("expected at least 1 drop during sustained overload, got 0")
	}
	if st.Popped != 1 {
		t.Fatalf("expected 1 popped packet, got %d", st.Popped)
	}
}

func TestQueue_BacklogGuard_SinglePacketNotDropped(t *testing.T) {
	q := NewQueue(10)

	// Inject a single packet that is very old (> Target + Interval)
	q.mu.Lock()
	q.buf = append(q.buf, item{data: []byte("lonely"), enqueued: time.Now().Add(-1 * time.Second)})
	q.firstAboveTime = time.Now().Add(-500 * time.Millisecond)
	q.mu.Unlock()

	// Because len(q.buf) == 0 after dequeuing the only packet, backlog guard prevents drop!
	got, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if string(got) != "lonely" {
		t.Fatalf("got %q, want 'lonely'", got)
	}

	st := q.Stats()
	if st.Dropped != 0 {
		t.Fatalf("backlog guard failed: dropped the only packet in queue (drops=%d)", st.Dropped)
	}
}

func TestQueue_CloseUnblocksPop(t *testing.T) {
	q := NewQueue(0)
	done := make(chan struct{})
	go func() {
		_, err := q.Pop()
		if err == nil {
			t.Error("Pop returned nil error on a closed empty queue")
		}
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	q.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Pop did not unblock after Close")
	}
}

func TestQueue_UnderflowGuard(t *testing.T) {
	q := NewQueue(20)

	// Simulate state where count was 1 and lastCount was 10 from earlier episode
	q.mu.Lock()
	q.count = 1
	q.lastCount = 10
	q.dropNext = time.Now().Add(50 * time.Millisecond) // future scheduled drop

	overloadTime := time.Now().Add(-(Target + Interval + 10*time.Millisecond))
	for i := 0; i < 5; i++ {
		q.buf = append(q.buf, item{data: []byte("pkt"), enqueued: overloadTime})
	}
	q.firstAboveTime = time.Now().Add(-time.Millisecond)
	q.mu.Unlock()

	// Pop triggers drop transition
	got, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if string(got) != "pkt" {
		t.Fatalf("unexpected data: %q", got)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	// q.count MUST NOT underflow to ~4.29 billion!
	if q.count > 100 {
		t.Fatalf("q.count underflowed! count = %d", q.count)
	}
}
