package udpserver

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
)

const (
	slotInboundBufferSize = 16
	defaultBatchSize      = 4
	sessionIdleGrace      = 2 * time.Minute
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
	r.sessions[key] = s

	go s.readBackendLoop()
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

	slot := &streamSlot{
		id:      s.nextSlotID,
		conn:    conn,
		inbound: make(chan []byte, slotInboundBufferSize),
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

		if err := slot.conn.SetReadDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
			return
		}
		n, err := slot.conn.Read(buf)
		if err != nil {
			return
		}

		if werr := s.backendConn.SetWriteDeadline(time.Now().Add(udpIdleTimeout)); werr != nil {
			return
		}
		if _, werr := s.backendConn.Write(buf[:n]); werr != nil {
			s.registry.deps.log().Debugf("udpserver [%s]: backend write error: %v", s.clientID, werr)
			return
		}
	}
}

func (s *clientSession) writeSlotLoop(slot *streamSlot) {
	for {
		select {
		case <-slot.done:
			return
		case <-s.ctx.Done():
			return
		case pkt := <-slot.inbound:
			_ = slot.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
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

		s.route(buf[:n])
	}
}

func (s *clientSession) route(data []byte) {
	s.slotsMu.Lock()
	n := len(s.slots)
	if n == 0 {
		s.slotsMu.Unlock()
		return
	}
	startIdx := s.roundRobin % n
	slots := make([]*streamSlot, n)
	copy(slots, s.slots)
	bs := s.registry.deps.BatchSize
	if bs <= 0 {
		bs = defaultBatchSize
	}
	s.slotsMu.Unlock()

	pkt := make([]byte, len(data))
	copy(pkt, data)

	for i := 0; i < n; i++ {
		idx := (startIdx + i) % n
		target := slots[idx]
		select {
		case target.inbound <- pkt:
			s.slotsMu.Lock()
			if len(s.slots) > 0 {
				if idx != s.roundRobin%len(s.slots) {
					s.roundRobin = idx % len(s.slots)
					s.burstCount = 1
				} else {
					s.burstCount++
					if s.burstCount >= bs {
						s.burstCount = 0
						s.roundRobin = (s.roundRobin + 1) % len(s.slots)
					}
				}
			}
			s.slotsMu.Unlock()
			return
		default:
		}
	}
}
