package reseq

import (
	"sync"
	"time"
)

const (
	// WindowSize is the capacity of the resequencing ring buffer.
	// Must be an integral power of two to allow bitwise modulo masking.
	WindowSize = 1024
	windowMask = WindowSize - 1

	// DefaultDwellTimeout is the maximum time to hold subsequent packets
	// waiting for a missing sequence hole before declaring it lost and
	// advancing the read head. 20ms cleanly covers inter-relay jitter (10-15ms)
	// without introducing perceptible TCP latency penalties.
	DefaultDwellTimeout = 20 * time.Millisecond
)

type slot struct {
	occupied bool
	seq      uint32
	payload  []byte
}

// Resequencer reorders out-of-order packets into strict ascending sequence
// order before delivering them to the consumer via onOrdered callback.
type Resequencer struct {
	mu           sync.Mutex
	dwellTimeout time.Duration
	onOrdered    func(payload []byte)

	initialized bool
	baseSeq     uint32
	gapDeadline time.Time

	slots [WindowSize]slot

	closed chan struct{}
	ticker *time.Ticker
}

// New creates a new Resequencer with the specified dwell timeout and delivery callback.
// If dwellTimeout <= 0, DefaultDwellTimeout (20ms) is used.
func New(dwellTimeout time.Duration, onOrdered func(payload []byte)) *Resequencer {
	if dwellTimeout <= 0 {
		dwellTimeout = DefaultDwellTimeout
	}
	r := &Resequencer{
		dwellTimeout: dwellTimeout,
		onOrdered:    onOrdered,
		closed:       make(chan struct{}),
		ticker:       time.NewTicker(5 * time.Millisecond),
		baseSeq:      1,
		initialized:  true,
	}

	for i := range r.slots {
		r.slots[i].payload = make([]byte, 0, 1600)
	}

	go r.timerLoop()
	return r
}

func (r *Resequencer) timerLoop() {
	for {
		select {
		case <-r.closed:
			return
		case now := <-r.ticker.C:
			r.checkTimeout(now)
		}
	}
}

// Push inserts a packet with sequence number `seq` and payload into the ring buffer.
// Any packets that become contiguous are drained and delivered to onOrdered.
func (r *Resequencer) Push(seq uint32, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.initialized {
		r.initialized = true
		r.baseSeq = seq
	}

	diff := int32(seq - r.baseSeq)

	// Case 1: Sequence reset or backwards jump.
	// If the sequence restarted from 1 (while baseSeq > 1) or jumped backwards beyond the window,
	// the sender has restarted. Re-synchronize baseSeq immediately.
	if (seq == 1 && r.baseSeq > 1) || diff < -WindowSize {
		r.drainAllLocked()
		r.baseSeq = seq
		diff = 0
	} else if diff < 0 {
		// Stale or duplicate packet (already drained or expired).
		return
	}

	// Case 2: Window overrun. Jumped too far ahead (>= WindowSize).
	// Fast-forward baseSeq to avoid stalling the pipeline.
	if diff >= WindowSize {
		r.drainContiguousLocked()
		r.drainAllLocked()
		r.baseSeq = seq
		diff = 0
	}

	// Case 3: Within active window [baseSeq, baseSeq + WindowSize - 1].
	idx := seq & windowMask
	s := &r.slots[idx]

	if !s.occupied || s.seq != seq {
		s.occupied = true
		s.seq = seq
		s.payload = append(s.payload[:0], payload...)
	}

	if diff == 0 {
		// Packet at head arrived. Drain contiguous sequence.
		r.drainContiguousLocked()
	} else {
		// Out of order packet arrived, creating or widening a gap at baseSeq.
		if r.gapDeadline.IsZero() {
			r.gapDeadline = time.Now().Add(r.dwellTimeout)
		}
	}
}

// drainContiguousLocked delivers all sequential packets starting from baseSeq.
func (r *Resequencer) drainContiguousLocked() {
	for {
		idx := r.baseSeq & windowMask
		s := &r.slots[idx]
		if !s.occupied || s.seq != r.baseSeq {
			break
		}

		// Slot is in order: deliver and advance head
		r.onOrdered(s.payload)
		s.occupied = false
		r.baseSeq++
	}

	// Check if there are remaining gaps
	if r.hasPendingSlotsLocked() {
		r.gapDeadline = time.Now().Add(r.dwellTimeout)
	} else {
		r.gapDeadline = time.Time{}
	}
}

func (r *Resequencer) hasPendingSlotsLocked() bool {
	for i := 0; i < WindowSize; i++ {
		if r.slots[i].occupied {
			return true
		}
	}
	return false
}

func (r *Resequencer) checkTimeout(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.gapDeadline.IsZero() || now.Before(r.gapDeadline) {
		return
	}

	// Gap dwell timeout expired: skip the missing gap and advance baseSeq
	// to the next occupied slot in the window to unblock HOL stall in one shot.
	found := false
	for i := 0; i < WindowSize; i++ {
		idx := r.baseSeq & windowMask
		if r.slots[idx].occupied && r.slots[idx].seq == r.baseSeq {
			found = true
			break
		}
		r.baseSeq++
	}

	if found {
		r.drainContiguousLocked()
	} else {
		r.gapDeadline = time.Time{}
	}
}

func (r *Resequencer) drainAllLocked() {
	for i := range r.slots {
		r.slots[i].occupied = false
	}
	r.gapDeadline = time.Time{}
}

// Close stops the background timer and cleans up resources.
func (r *Resequencer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	select {
	case <-r.closed:
		return
	default:
		close(r.closed)
		r.ticker.Stop()
		r.drainAllLocked()
	}
}

// Reset clears all buffered packets and resets the resequencer to initial state at seq=1.
func (r *Resequencer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initialized = true
	r.baseSeq = 1
	r.gapDeadline = time.Time{}
	for i := range r.slots {
		r.slots[i].occupied = false
		r.slots[i].payload = r.slots[i].payload[:0]
	}
}
