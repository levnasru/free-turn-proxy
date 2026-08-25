package udprelay

import (
	"context"
	"testing"
	"time"
)

func newTestSlot(streamID int) *slotHandle {
	return &slotHandle{
		streamID: streamID,
		inbound:  make(chan *Packet, slotInboundBufferSize),
		up:       make(chan struct{}, 1),
	}
}

func TestDispatcherRoutesRoundRobin(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2, s3 := newTestSlot(1), newTestSlot(2), newTestSlot(3)
	d.setSlots([]*slotHandle{s1, s2, s3}, 1)

	for i := 0; i < 6; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}

	for _, s := range []*slotHandle{s1, s2, s3} {
		if got := len(s.inbound); got != 2 {
			t.Fatalf("expected 2 packets on slot %d after 6 round-robin routes, got %d", s.streamID, got)
		}
	}
}

func TestDispatcherManualRotateChangesActiveStreamID(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	d.rotateManual()
	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected active streamID 2 after manual rotate, got %d", got)
	}
	// rotateManual no longer affects routing (see route()'s doc comment) -
	// only replaceSlot's retire-guard cares about d.active now.
}

func TestDispatcherRunReadsInboundAndRotate(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	inboundChan := make(chan *Packet, 1)
	rotateCh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx, inboundChan, rotateCh)
	}()

	rotateCh <- struct{}{}
	deadline := time.After(time.Second)
	for d.activeStreamID() != 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for run() to process rotateCh")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestDispatcherReplaceSlot(t *testing.T) {
	t.Parallel()

	t.Run("retiring the active slot is a no-op", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		fresh := newTestSlot(3)
		if ok := d.replaceSlot(1, fresh); ok {
			t.Fatal("expected replaceSlot to refuse retiring the active slot")
		}
		if got := d.activeStreamID(); got != 1 {
			t.Fatalf("active slot must be unchanged, got %d", got)
		}
		got := d.currentSlots()
		if len(got) != 2 || got[0].streamID != 1 || got[1].streamID != 2 {
			t.Fatalf("hot-set must be unchanged, got %+v", got)
		}
	})

	t.Run("retiring an unknown streamID is a no-op", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		fresh := newTestSlot(3)
		if ok := d.replaceSlot(99, fresh); ok {
			t.Fatal("expected replaceSlot to refuse retiring an unknown streamID")
		}
		got := d.currentSlots()
		if len(got) != 2 || got[0].streamID != 1 || got[1].streamID != 2 {
			t.Fatalf("hot-set must be unchanged, got %+v", got)
		}
	})

	t.Run("retiring a real non-active slot swaps it in", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		fresh := newTestSlot(3)
		if ok := d.replaceSlot(2, fresh); !ok {
			t.Fatal("expected replaceSlot to succeed retiring the non-active slot")
		}
		if got := d.activeStreamID(); got != 1 {
			t.Fatalf("active slot must be unchanged, got %d", got)
		}
		got := d.currentSlots()
		if len(got) != 2 || got[0].streamID != 1 || got[1].streamID != 3 {
			t.Fatalf("expected slot 2 swapped for slot 3 in place, got %+v", got)
		}

		// Routing still works after the swap: round-robin's counter is
		// fresh (0) and s1 sits at index 0, so the first route() after
		// the swap still lands there - coincidence of index, not of
		// "active slot" (route() no longer looks at d.active).
		d.route(&Packet{Data: []byte("x"), N: 1})
		select {
		case <-s1.inbound:
		default:
			t.Fatal("expected packet routed to slot at round-robin index 0 (s1)")
		}
	})

	t.Run("simulated TOCTOU: dispatcher rotates onto the retire candidate first", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		// sessionManager.refreshOne would have read active=1 here and decided
		// to retire streamID 2 - then the dispatcher rotates onto 2 before
		// the swap actually happens.
		d.rotateManual()
		if got := d.activeStreamID(); got != 2 {
			t.Fatalf("setup: expected active streamID 2 after manual rotate, got %d", got)
		}

		fresh := newTestSlot(3)
		if ok := d.replaceSlot(2, fresh); ok {
			t.Fatal("expected replaceSlot to refuse retiring the now-active slot 2")
		}
		if got := d.activeStreamID(); got != 2 {
			t.Fatalf("active slot must still be 2 (live traffic preserved), got %d", got)
		}
	})
}

func TestDispatcherAddSlot(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	s3 := newTestSlot(3)
	d.addSlot(s3)

	got := d.currentSlots()
	if len(got) != 3 || got[2].streamID != 3 {
		t.Fatalf("expected slot 3 appended, got %+v", got)
	}
	if got := d.activeStreamID(); got != 1 {
		t.Fatalf("addSlot must not disturb the active slot, got %d", got)
	}

	// New member actually receives traffic once round-robin reaches it.
	for i := 0; i < 3; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}
	select {
	case <-s3.inbound:
	default:
		t.Fatal("expected the newly added slot to receive a packet within one full round-robin cycle")
	}
}

func TestDispatcherRemoveSlot(t *testing.T) {
	t.Parallel()

	t.Run("removing the active slot is a no-op", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		if ok := d.removeSlot(1); ok {
			t.Fatal("expected removeSlot to refuse removing the active slot")
		}
		if got := d.currentSlots(); len(got) != 2 {
			t.Fatalf("hot-set must be unchanged, got %+v", got)
		}
	})

	t.Run("removing an unknown streamID is a no-op", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2 := newTestSlot(1), newTestSlot(2)
		d.setSlots([]*slotHandle{s1, s2}, 1)

		if ok := d.removeSlot(99); ok {
			t.Fatal("expected removeSlot to refuse an unknown streamID")
		}
	})

	t.Run("removing a real non-active slot shrinks the hot-set", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2, s3 := newTestSlot(1), newTestSlot(2), newTestSlot(3)
		d.setSlots([]*slotHandle{s1, s2, s3}, 1)

		if ok := d.removeSlot(2); !ok {
			t.Fatal("expected removeSlot to succeed removing the non-active slot")
		}
		got := d.currentSlots()
		if len(got) != 2 || got[0].streamID != 1 || got[1].streamID != 3 {
			t.Fatalf("expected slot 2 gone, 1 and 3 remaining in place, got %+v", got)
		}
		if got := d.activeStreamID(); got != 1 {
			t.Fatalf("active slot must still be 1, got %d", got)
		}
	})

	t.Run("removing a slot before the active index keeps active pointed at the same streamID", func(t *testing.T) {
		t.Parallel()
		d := newDispatcher()
		s1, s2, s3 := newTestSlot(1), newTestSlot(2), newTestSlot(3)
		d.setSlots([]*slotHandle{s1, s2, s3}, 3) // active = index 2 (streamID 3)

		if ok := d.removeSlot(1); !ok { // removes index 0, before active
			t.Fatal("expected removeSlot to succeed")
		}
		if got := d.activeStreamID(); got != 3 {
			t.Fatalf("active must still resolve to streamID 3 after the index shift, got %d", got)
		}
	})
}
