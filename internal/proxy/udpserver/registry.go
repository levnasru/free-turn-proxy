package udpserver

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/wire/reseq"
)

const (
	slotInboundBufferSize = 16
	defaultBatchSize      = 4
	sessionIdleGrace      = 2 * time.Minute
	slotIdleTimeout       = 5 * time.Minute
	maxClientSlots        = 200
)

// Deps объединяет зависимости хост-процесса для UDP-сервера.
type Deps struct {
	Log       logx.Logger
	BatchSize int
}

func (d *Deps) log() logx.Logger {
	if d.Log == nil {
		return logx.Nop()
	}
	return d.Log
}

type streamSlot struct {
	id      int
	conn    net.Conn
	inbound chan []byte
	done    chan struct{}
	once    sync.Once
	epoch   uint16
}

func (s *streamSlot) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

// Registry агрегирует и мультиплексирует входящие DTLS-стримы от одного
// клиента (по Client ID) в единственный общий UDP-сокет к WireGuard-бэкенду.
// Это решает проблему WireGuard Single-Endpoint Roaming Inversion: ядро
// WireGuard видит единый постоянный endpoint для каждого пира, а входящий
// обратный трафик (downlink) равномерно распределяется микробатчами
// по всем активным стримам клиента.
type Registry struct {
	deps Deps

	mu       sync.Mutex
	sessions map[string]*clientSession
}

func NewRegistry(deps Deps) *Registry {
	if deps.BatchSize <= 0 {
		deps.BatchSize = defaultBatchSize
	}
	return &Registry{
		deps:     deps,
		sessions: make(map[string]*clientSession),
	}
}

// Handle обрабатывает входящее DTLS-соединение conn. Если clientID пуст,
// используется старый standalone-режим (1:1 сокет). Если clientID задан,
// стрим присоединяется к пулу стримов этого клиента.
func (r *Registry) Handle(ctx context.Context, logger logx.Logger, conn net.Conn, connectAddr, clientID string) {
	if clientID == "" {
		handleStandalone(ctx, logger, conn, connectAddr)
		return
	}

	session, err := r.getOrCreate(ctx, clientID, connectAddr)
	if err != nil {
		logger.Errorf("udpserver [%s]: getOrCreate session: %v", clientID, err)
		return
	}

	slot := session.addSlot(conn)
	defer session.removeSlot(slot)

	session.runSlot(ctx, slot)
}

func (r *Registry) getOrCreate(ctx context.Context, clientID, connectAddr string) (*clientSession, error) {
	key := clientID + "@" + connectAddr
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[key]; ok && !s.isClosed() {
		s.cancelIdleTimer()
		return s, nil
	}

	backendConn, err := (&net.Dialer{}).DialContext(ctx, "udp", connectAddr)
	if err != nil {
		return nil, err
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	s := &clientSession{
		registry:    r,
		clientID:    clientID,
		connectAddr: connectAddr,
		ctx:         sessCtx,
		cancel:      sessCancel,
		backendConn: backendConn,
		slots:       make([]*streamSlot, 0),
	}
	s.uplinkReseq = reseq.New(reseq.DefaultDwellTimeout, func(payload []byte) {
		_ = s.backendConn.SetWriteDeadline(time.Now().Add(udpIdleTimeout))
		if _, werr := s.backendConn.Write(payload); werr != nil {
			s.registry.deps.log().Debugf("udpserver [%s]: backend write error: %v", s.clientID, werr)
		}
	})
	r.sessions[key] = s

	go s.readBackendLoop()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				st := s.uplinkReseq.Stats()
				if st.Pushed > 0 {
					s.registry.deps.log().Debugf("udpserver [%s]: [Reseq Uplink] pushed=%d delivered=%d lateDelivered=%d gapsTimedOut=%d overrun=%d",
						s.clientID, st.Pushed, st.Delivered, st.LateDelivered, st.GapsTimedOut, st.WindowOverrun)
				}
			}
		}
	}()
	r.deps.log().Infof("udpserver [%s]: created shared backend session -> %s (local %s)",
		clientID, connectAddr, backendConn.LocalAddr())
	return s, nil
}

type clientSession struct {
	registry    *Registry
	clientID    string
	connectAddr string
	ctx         context.Context
	cancel      context.CancelFunc

	backendConn net.Conn

	slotsMu    sync.Mutex
	slots      []*streamSlot
	nextSlotID int
	roundRobin int
	burstCount int

	idleTimer *time.Timer
	closed    bool

	currentEpoch        uint16
	uplinkReseq         *reseq.Resequencer
	enableReseqDownlink atomic.Bool
	downlinkSeq         uint32
}

func (s *clientSession) handleEpoch(slot *streamSlot, epoch uint16) {
	if epoch == 0 {
		return
	}
	s.slotsMu.Lock()
	slot.epoch = epoch
	if s.currentEpoch == 0 {
		s.currentEpoch = epoch
		s.slotsMu.Unlock()
		return
	}
	if epoch == s.currentEpoch {
		s.slotsMu.Unlock()
		return
	}

	oldEpoch := s.currentEpoch
	s.currentEpoch = epoch
	s.registry.deps.log().Infof("udpserver [%s]: epoch change detected (%d -> %d), pruning stale slots", s.clientID, oldEpoch, epoch)

	if s.uplinkReseq != nil {
		s.uplinkReseq.Reset()
	}
	atomic.StoreUint32(&s.downlinkSeq, 0)

	var kept []*streamSlot
	var pruned []*streamSlot
	for _, sl := range s.slots {
		if sl == slot || sl.epoch == epoch {
			kept = append(kept, sl)
		} else if sl.epoch != 0 {
			pruned = append(pruned, sl)
		} else {
			kept = append(kept, sl)
		}
	}
	s.slots = kept
	if s.roundRobin >= len(s.slots) {
		s.roundRobin = 0
		s.burstCount = 0
	}
	s.slotsMu.Unlock()

	for _, sl := range pruned {
		s.registry.deps.log().Debugf("udpserver [%s]: pruned ghost slot %d (old epoch %d)", s.clientID, sl.id, sl.epoch)
		sl.close()
	}
}

func (s *clientSession) isClosed() bool {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	return s.closed
}

func (s *clientSession) cancelIdleTimer() {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
		s.registry.deps.log().Debugf("udpserver [%s]: canceled idle teardown timer", s.clientID)
	}
}

func (s *clientSession) addSlot(conn net.Conn) *streamSlot {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()

	if len(s.slots) == 0 {
		if s.uplinkReseq != nil {
			s.uplinkReseq.Reset()
		}
		atomic.StoreUint32(&s.downlinkSeq, 0)
	}

	// If ungraceful reconnects caused ghost slots to accumulate, prune the oldest
	for len(s.slots) >= maxClientSlots {
		oldest := s.slots[0]
		s.slots = s.slots[1:]
		oldest.close()
		s.registry.deps.log().Debugf("udpserver [%s]: pruned stale stream slot %d (capped at %d)", s.clientID, oldest.id, maxClientSlots)
	}

	bufCap := 16 * s.registry.deps.BatchSize
	if bufCap < 128 {
		bufCap = 128
	}

	slot := &streamSlot{
		id:      s.nextSlotID,
		conn:    conn,
		inbound: make(chan []byte, bufCap),
		done:    make(chan struct{}),
	}
	s.nextSlotID++
	s.slots = append(s.slots, slot)
	count := len(s.slots)

	go s.writeSlotLoop(slot)
	s.registry.deps.log().Debugf("udpserver [%s]: stream slot %d attached (active=%d)", s.clientID, slot.id, count)
	return slot
}

func (s *clientSession) removeSlot(slot *streamSlot) {
	s.slotsMu.Lock()
	slot.close()

	for i, cur := range s.slots {
		if cur == slot {
			s.slots = append(s.slots[:i], s.slots[i+1:]...)
			if s.roundRobin >= len(s.slots) {
				s.roundRobin = 0
				s.burstCount = 0
			}
			break
		}
	}
	count := len(s.slots)
	shouldStartTimer := (count == 0 && !s.closed && s.idleTimer == nil)
	s.slotsMu.Unlock()

	s.registry.deps.log().Debugf("udpserver [%s]: stream slot %d removed (active=%d)", s.clientID, slot.id, count)

	if shouldStartTimer {
		s.slotsMu.Lock()
		if len(s.slots) == 0 && !s.closed && s.idleTimer == nil {
			s.idleTimer = time.AfterFunc(sessionIdleGrace, func() {
				s.close()
			})
			s.registry.deps.log().Debugf("udpserver [%s]: 0 active streams, scheduled teardown in %v", s.clientID, sessionIdleGrace)
		}
		s.slotsMu.Unlock()
	}
}

func (s *clientSession) close() {
	s.slotsMu.Lock()
	if s.closed {
		s.slotsMu.Unlock()
		return
	}
	s.closed = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.cancel()
	_ = s.backendConn.Close()
	if s.uplinkReseq != nil {
		s.uplinkReseq.Close()
	}

	slotsToClose := make([]*streamSlot, len(s.slots))
	copy(slotsToClose, s.slots)
	s.slots = nil
	s.slotsMu.Unlock()

	for _, slot := range slotsToClose {
		slot.close()
	}

	key := s.clientID + "@" + s.connectAddr
	s.registry.mu.Lock()
	if s.registry.sessions[key] == s {
		delete(s.registry.sessions, key)
	}
	s.registry.mu.Unlock()
	s.registry.deps.log().Infof("udpserver [%s]: session closed and removed from registry", s.clientID)
}

func (s *clientSession) runSlot(ctx context.Context, slot *streamSlot) {
	buf := make([]byte, udpRelayBufSize)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-slot.done:
			return
		default:
		}

		if err := slot.conn.SetReadDeadline(time.Now().Add(slotIdleTimeout)); err != nil {
			return
		}
		n, err := slot.conn.Read(buf)
		if err != nil {
			return
		}

		if seq, epoch, payload, ok := reseq.Unwrap(buf[:n]); ok {
			s.enableReseqDownlink.Store(true)
			s.handleEpoch(slot, epoch)
			s.uplinkReseq.Push(seq, payload)
		} else {
			if werr := s.backendConn.SetWriteDeadline(time.Now().Add(udpIdleTimeout)); werr != nil {
				return
			}
			if _, werr := s.backendConn.Write(buf[:n]); werr != nil {
				s.registry.deps.log().Debugf("udpserver [%s]: backend write error: %v", s.clientID, werr)
				return
			}
		}
	}
}

func (s *clientSession) writeSlotLoop(slot *streamSlot) {
	defer slot.close()
	for {
		select {
		case <-slot.done:
			return
		case <-s.ctx.Done():
			return
		case pkt := <-slot.inbound:
			_ = slot.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := slot.conn.Write(pkt); err != nil {
				s.registry.deps.log().Debugf("udpserver [%s]: slot %d write error: %v", s.clientID, slot.id, err)
				return
			}
		}
	}
}

func (s *clientSession) readBackendLoop() {
	defer s.close()
	buf := make([]byte, udpRelayBufSize)
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		if err := s.backendConn.SetReadDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
			return
		}
		n, err := s.backendConn.Read(buf)
		if err != nil {
			s.registry.deps.log().Debugf("udpserver [%s]: backend read error: %v", s.clientID, err)
			return
		}

		if s.enableReseqDownlink.Load() {
			seq := atomic.AddUint32(&s.downlinkSeq, 1)
			s.slotsMu.Lock()
			ep := s.currentEpoch
			s.slotsMu.Unlock()
			framed := reseq.Wrap(nil, buf[:n], seq, ep)
			s.route(framed)
		} else {
			s.route(buf[:n])
		}
	}
}

func (s *clientSession) route(data []byte) {
	s.slotsMu.Lock()
	n := len(s.slots)
	if n == 0 {
		s.slotsMu.Unlock()
		s.registry.deps.log().Warnf("udpserver [%s]: DROPPED downlink %d bytes (0 slots)", s.clientID, len(data))
		return
	}

	curEpoch := s.currentEpoch
	candidates := make([]*streamSlot, 0, n)
	if curEpoch != 0 {
		for _, sl := range s.slots {
			select {
			case <-sl.done:
				continue
			default:
				if sl.epoch == curEpoch {
					candidates = append(candidates, sl)
				}
			}
		}
	}
	if len(candidates) == 0 {
		for _, sl := range s.slots {
			select {
			case <-sl.done:
				continue
			default:
				if sl.epoch == 0 || sl.epoch == curEpoch {
					candidates = append(candidates, sl)
				}
			}
		}
	}

	numCandidates := len(candidates)
	if numCandidates == 0 {
		s.slotsMu.Unlock()
		s.registry.deps.log().Warnf("udpserver [%s]: DROPPED downlink %d bytes (no eligible candidates among %d slots)", s.clientID, len(data), n)
		return
	}

	startIdx := s.roundRobin % numCandidates
	slots := make([]*streamSlot, numCandidates)
	copy(slots, candidates)
	bs := s.registry.deps.BatchSize
	if bs <= 0 {
		bs = defaultBatchSize
	}
	s.slotsMu.Unlock()

	pkt := make([]byte, len(data))
	copy(pkt, data)

	for i := 0; i < numCandidates; i++ {
		idx := (startIdx + i) % numCandidates
		target := slots[idx]
		select {
		case <-target.done:
			continue
		case target.inbound <- pkt:
			s.slotsMu.Lock()
			if numCandidates > 0 {
				if idx != s.roundRobin%numCandidates {
					s.roundRobin = idx % numCandidates
					s.burstCount = 1
				} else {
					s.burstCount++
					if s.burstCount >= bs {
						s.burstCount = 0
						s.roundRobin = (s.roundRobin + 1) % numCandidates
					}
				}
			}
			s.slotsMu.Unlock()
			return
		default:
		}
	}
	s.registry.deps.log().Warnf("udpserver [%s]: DROPPED downlink packet %d bytes (all %d candidates full)", s.clientID, len(pkt), numCandidates)
}
