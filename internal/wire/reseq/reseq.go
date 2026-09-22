package reseq

import (
	"sync"
	"time"
)

const (
	// WindowSize is the capacity of the resequencing ring buffer.
	// Must be an integral power of two to allow bitwise modulo masking.
	WindowSize = 4096
	windowMask = WindowSize - 1

	// DefaultDwellTimeout is the maximum time to hold subsequent packets
	// waiting for a missing sequence hole before advancing the read head.
	// 35ms cleanly absorbs cellular LTE radio scheduling jitter and inter-relay variance
	// without introducing HOL blocking penalties on normal packet loss. Any packet
	// arriving after the dwell timeout is still delivered directly to onOrdered so that
	// WireGuard / TCP receive it without loss.
	DefaultDwellTimeout = 35 * time.Millisecond
)

type slot struct {
	occupied   bool
	seq        uint32
	payload    []byte
	receivedAt time.Time
}

// Stats captures resequencing counters for observability.
type Stats struct {
	Pushed        uint64
	Delivered     uint64
	LateDelivered uint64
	StaleDropped  uint64
	WindowOverrun uint64
	GapsTimedOut  uint64
}

// Resequencer reorders out-of-order packets into strict ascending sequence
// order before delivering them to the consumer via onOrdered callback.
type Resequencer struct {
	mu           sync.Mutex
	dwellTimeout time.Duration
	onOrdered    func(payload []byte)

	initialized  bool
	baseSeq      uint32
	pendingCount int
	gapDeadline  time.Time

	stats Stats

	slots       [WindowSize]slot
	timedOut    [WindowSize]uint32
	timedOutOcc [WindowSize]bool

	closed chan struct{}
	ticker *time.Ticker
}

// New creates a new Resequencer with the specified dwell timeout and delivery callback.
// If dwellTimeout <= 0, DefaultDwellTimeout (6ms) is used.
func New(dwellTimeout time.Duration, onOrdered func(payload []byte)) *Resequencer {
	if dwellTimeout <= 0 {
		dwellTimeout = DefaultDwellTimeout
	}
	r := &Resequencer{
		dwellTimeout: dwellTimeout,
		onOrdered:    onOrdered,
		closed:       make(chan struct{}),
		ticker:       time.NewTicker(2 * time.Millisecond),
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
	var toDeliver [][]byte
	var latePkt []byte

	r.mu.Lock()
	r.stats.Pushed++

	if !r.initialized {
		r.initialized = true
		r.baseSeq = 1
	}

	diff := int32(seq - r.baseSeq)

	// Case 1: Sequence reset or backwards jump.
	// If the sequence restarted backwards from 1 (diff < 0) or jumped backwards beyond the window,
	// the sender has restarted. Re-synchronize baseSeq immediately.
	// Note: if seq == 1 was recorded as a timed-out gap from the current session, do not reset;
	// it will be delivered as a late packet in the branch below.
	if (seq == 1 && diff < 0 && !(r.timedOutOcc[1] && r.timedOut[1] == 1)) || diff < -WindowSize {
		r.drainAllLocked()
		r.baseSeq = seq
		diff = 0
	} else if diff < 0 {
		idx := seq & windowMask
		if r.timedOutOcc[idx] && r.timedOut[idx] == seq {
			// Late packet (arrived after its gap timed out).
			// Deliver immediately to consumer so WireGuard / TCP receive it without loss.
			r.timedOutOcc[idx] = false
			r.timedOut[idx] = 0
			r.stats.LateDelivered++
			latePkt = make([]byte, len(payload))
			copy(latePkt, payload)
		} else {
			// True duplicate of an already-delivered packet.
			r.stats.StaleDropped++
		}
		r.mu.Unlock()
		if latePkt != nil {
			r.onOrdered(latePkt)
		}
		return
	}

	// Case 2: Window overrun. Jumped too far ahead (>= WindowSize).
	// Fast-forward baseSeq to avoid stalling the pipeline.
	if diff >= WindowSize {
		r.stats.WindowOverrun++
		toDeliver = append(toDeliver, r.drainContiguousLocked()...)
		r.drainAllLocked()
		r.baseSeq = seq
		diff = 0
	}

	// Case 3: Within active window [baseSeq, baseSeq + WindowSize - 1].
	idx := seq & windowMask
	s := &r.slots[idx]

	if !s.occupied || s.seq != seq {
		if !s.occupied {
			r.pendingCount++
		}
		s.occupied = true
		s.seq = seq
		s.payload = append(s.payload[:0], payload...)
		s.receivedAt = time.Now()
	}

	if diff == 0 {
		// Packet at head arrived. Drain contiguous sequence.
		toDeliver = append(toDeliver, r.drainContiguousLocked()...)
	} else {
		// Out of order packet arrived, creating or widening a gap at baseSeq.
		if r.gapDeadline.IsZero() {
			r.gapDeadline = s.receivedAt.Add(r.dwellTimeout)
		}
	}
	r.mu.Unlock()

	for _, p := range toDeliver {
		r.onOrdered(p)
	}
}

// drainContiguousLocked delivers all sequential packets starting from baseSeq.
// Must be called while holding r.mu. Returns packets to deliver outside the lock.
func (r *Resequencer) drainContiguousLocked() [][]byte {
	var toDeliver [][]byte
	for {
		idx := r.baseSeq & windowMask
		s := &r.slots[idx]
		if !s.occupied || s.seq != r.baseSeq {
			break
		}

		// Slot is in order: copy payload to deliver outside lock
		p := make([]byte, len(s.payload))
		copy(p, s.payload)
		toDeliver = append(toDeliver, p)

		r.stats.Delivered++
		s.occupied = false
		s.receivedAt = time.Time{}
		r.pendingCount--
		r.baseSeq++
	}

	// Check if there are remaining gaps
	if r.pendingCount > 0 {
		if r.gapDeadline.IsZero() {
			earliest := r.earliestPendingReceivedLocked()
			if !earliest.IsZero() {
				r.gapDeadline = earliest.Add(r.dwellTimeout)
			} else {
				r.gapDeadline = time.Now().Add(r.dwellTimeout)
			}
		}
	} else {
		r.gapDeadline = time.Time{}
	}

	return toDeliver
}

func (r *Resequencer) hasPendingSlotsLocked() bool {
	return r.pendingCount > 0
}

func (r *Resequencer) earliestPendingReceivedLocked() time.Time {
	var earliest time.Time
	found := 0
	for i := 0; i < WindowSize && found < r.pendingCount; i++ {
		idx := (r.baseSeq + uint32(i)) & windowMask
		s := &r.slots[idx]
		if s.occupied {
			if earliest.IsZero() || s.receivedAt.Before(earliest) {
				earliest = s.receivedAt
			}
			found++
		}
	}
	return earliest
}

func (r *Resequencer) checkTimeout(now time.Time) {
	var toDeliver [][]byte

	r.mu.Lock()
	for r.pendingCount > 0 {
		if r.gapDeadline.IsZero() || now.Before(r.gapDeadline) {
			break
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
			r.timedOutOcc[idx] = true
			r.timedOut[idx] = r.baseSeq
			r.stats.GapsTimedOut++
			r.baseSeq++
		}

		if found {
			toDeliver = append(toDeliver, r.drainContiguousLocked()...)
		}
		if r.pendingCount > 0 {
			earliest := r.earliestPendingReceivedLocked()
			if !earliest.IsZero() {
				r.gapDeadline = earliest.Add(r.dwellTimeout)
			} else {
				r.gapDeadline = now.Add(r.dwellTimeout)
			}
		} else {
			r.gapDeadline = time.Time{}
		}
	}
	r.mu.Unlock()

	for _, p := range toDeliver {
		r.onOrdered(p)
	}
}

// Stats returns a copy of current resequencer metrics.
func (r *Resequencer) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *Resequencer) drainAllLocked() {
	for i := range r.slots {
		r.slots[i].occupied = false
		r.slots[i].receivedAt = time.Time{}
		r.timedOut[i] = 0
		r.timedOutOcc[i] = false
	}
	r.pendingCount = 0
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

// Reset clears all buffered packets and resets the resequencer to wait for the next sequence.
func (r *Resequencer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initialized = true
	r.baseSeq = 1
	r.pendingCount = 0
	r.gapDeadline = time.Time{}
	for i := range r.slots {
		r.slots[i].occupied = false
		r.slots[i].payload = r.slots[i].payload[:0]
		r.slots[i].receivedAt = time.Time{}
		r.timedOut[i] = 0
		r.timedOutOcc[i] = false
	}
}
