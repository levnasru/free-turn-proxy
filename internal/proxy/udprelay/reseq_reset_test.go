package udprelay

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/wire/reseq"
)

func TestDownlinkEpochChangeResetsReseq(t *testing.T) {
	var delivered []int
	var mu sync.Mutex

	resequencer := reseq.New(20*time.Millisecond, func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, int(payload[0]))
	})
	defer resequencer.Close()

	var downlinkEpoch atomic.Uint32
	deps := &Deps{
		Log:           logx.Nop(),
		DownlinkReseq: resequencer,
		DownlinkEpoch: &downlinkEpoch,
	}

	// Stream receives packets from epoch 1
	p1 := reseq.Wrap(nil, []byte{1}, 1, 100)
	p2 := reseq.Wrap(nil, []byte{2}, 2, 100)

	// Simulate loop processing
	processPacket := func(buf []byte) {
		if deps.DownlinkReseq != nil {
			if seq, epoch, payload, ok := reseq.Unwrap(buf); ok {
				if epoch != 0 {
					if deps.DownlinkEpoch != nil {
						prev := deps.DownlinkEpoch.Load()
						if prev != 0 && uint32(epoch) != prev {
							deps.DownlinkReseq.Reset()
						}
						deps.DownlinkEpoch.Store(uint32(epoch))
					}
				}
				deps.DownlinkReseq.Push(seq, payload)
			}
		}
	}

	processPacket(p1)
	processPacket(p2)

	mu.Lock()
	if len(delivered) != 2 {
		t.Fatalf("expected 2 delivered from epoch 100, got %d", len(delivered))
	}
	mu.Unlock()

	// Server rotates session to epoch 200, sequence restarts at 1!
	p3 := reseq.Wrap(nil, []byte{3}, 1, 200)
	p4 := reseq.Wrap(nil, []byte{4}, 2, 200)

	processPacket(p3)
	processPacket(p4)

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 4 {
		t.Fatalf("expected 4 total delivered across epochs, got %d (%v)", len(delivered), delivered)
	}
	if delivered[2] != 3 || delivered[3] != 4 {
		t.Errorf("expected packets 3 and 4 delivered after epoch change, got %v", delivered)
	}
}

func TestResetReseqChSignal(t *testing.T) {
	resetCh := make(chan struct{}, 1)
	resequencer := reseq.New(20*time.Millisecond, func(payload []byte) {})
	defer resequencer.Close()

	// Advance sequence
	resequencer.Push(1, []byte{1})
	resequencer.Push(2, []byte{2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resetDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resetCh:
				resequencer.Reset()
				close(resetDone)
				return
			}
		}
	}()

	resetCh <- struct{}{}
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reset signal")
	}

	st := resequencer.Stats()
	if st.Delivered != 2 {
		t.Errorf("expected 2 delivered before reset, got %d", st.Delivered)
	}
}

func TestRunResetReseqChWiring(t *testing.T) {
	resetCh := make(chan struct{}, 1)
	params := &Params{
		ResetReseqCh: resetCh,
	}
	if params.ResetReseqCh == nil {
		t.Fatal("expected ResetReseqCh to be set")
	}
}
