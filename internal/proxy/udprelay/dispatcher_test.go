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

func TestDispatcherRoutesToActiveSlot(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	pkt := &Packet{Data: []byte("x"), N: 1}
	d.route(pkt)

	select {
	case got := <-s1.inbound:
		if got != pkt {
			t.Fatal("wrong packet delivered to active slot")
		}
	default:
		t.Fatal("expected packet on active slot s1")
	}
	select {
	case <-s2.inbound:
		t.Fatal("inactive slot s2 must not receive packets")
	default:
	}
}

func TestDispatcherManualRotate(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	d.rotateManual()
	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected active streamID 2 after manual rotate, got %d", got)
	}

	d.route(&Packet{Data: []byte("x"), N: 1})
	select {
	case <-s2.inbound:
	default:
		t.Fatal("expected packet on newly active slot s2")
	}
}

func TestDispatcherRotatesByTime(t *testing.T) {
	// Не t.Parallel(): подменяет пакетную переменную rotateInterval.
	orig := rotateInterval
	rotateInterval = time.Millisecond
	defer func() { rotateInterval = orig }()

	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	// Первый пакет сразу после setSlots не должен ротировать - таймер только
	// что сброшен.
	d.route(&Packet{Data: []byte("x"), N: 1})
	if got := d.activeStreamID(); got != 1 {
		t.Fatalf("expected no rotation immediately after setSlots, got streamID %d", got)
	}

	time.Sleep(5 * time.Millisecond)
	d.route(&Packet{Data: []byte("x"), N: 1})

	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected rotation to streamID 2 after rotateInterval elapsed, got %d", got)
	}
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

func TestDispatcherFailoverPicksMostRecentlyUpSlot(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	dead := &slotHandle{streamID: 1, inbound: make(chan *Packet)} // unbuffered: route() always hits default, nobody reads
	stale := newTestSlot(2)
	fresh := newTestSlot(3)
	d.setSlots([]*slotHandle{dead, stale, fresh}, 1)

	d.markUp(2, time.Now().Add(-time.Minute))
	d.markUp(3, time.Now())

	for i := 0; i < maxConsecutiveDropsBeforeFailover; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}

	if got := d.activeStreamID(); got != 3 {
		t.Fatalf("expected failover to most recently up slot (3), got %d", got)
	}
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

		// Routing still works after the swap: active slot untouched, new
		// member reachable once made active.
		d.route(&Packet{Data: []byte("x"), N: 1})
		select {
		case <-s1.inbound:
		default:
			t.Fatal("expected packet still routed to unchanged active slot s1")
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

func TestDispatcherFailoverFallsBackToNextWhenNoUpSignalKnown(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	dead := &slotHandle{streamID: 1, inbound: make(chan *Packet)}
	other := newTestSlot(2)
	d.setSlots([]*slotHandle{dead, other}, 1)
	// Ни один markUp не вызывался - failoverLocked не должен паниковать
	// и должен просто уйти по кругу.

	for i := 0; i < maxConsecutiveDropsBeforeFailover; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}

	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected fallback round-robin to streamID 2, got %d", got)
	}
}
