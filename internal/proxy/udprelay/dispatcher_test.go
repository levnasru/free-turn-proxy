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

func TestDispatcherRotatesByVolume(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	// Один пакет с N=rotateThresholdBytes уже должен вызвать ротацию.
	d.route(&Packet{Data: make([]byte, 1), N: rotateThresholdBytes})

	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected rotation to streamID 2 after crossing volume threshold, got %d", got)
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
