package udprelay

import (
	"context"
	"net"
	"sync"
	"time"
)

// sessionManager владеет hot-set'ом (K живых DTLS+TURN слотов) и
// dispatcher'ом, который маршрутизирует в него пакеты. Заменяет прежний
// цикл `for i := range numStreams { go maintainSession... }` в Run():
// вместо N параллельных независимых стримов держит K << N живых, ротируя
// активный через dispatcher и периодически обновляя состав (см.
// refreshOne, sessionmgr.go продолжение в Task 8). Переиспользует
// DTLSLoop/TURNLoop без изменений - каждому слоту достаётся собственный
// маленький inbound-канал вместо общего inboundChan, который теперь
// единолично читает dispatcher.
type sessionManager struct {
	deps       *Deps
	params     *Params
	peer       *net.UDPAddr
	listenConn net.PacketConn
	k          int
	t          <-chan time.Time // общий тик TURNLoop, один на процесс, как и раньше

	disp *dispatcher

	nextID int // следующий свободный streamID; трогает только run()'s горутина

	mu      sync.Mutex // защищает только groups - onAllocated зовётся из per-slot горутин
	groups  map[int]string
	cancels map[int]context.CancelFunc // трогает только run()'s горутина (launchSlot/refreshOne)
}

func newSessionManager(deps *Deps, params *Params, peer *net.UDPAddr, listenConn net.PacketConn, k int, t <-chan time.Time) *sessionManager {
	if k <= 0 {
		k = 1
	}
	return &sessionManager{
		deps:       deps,
		params:     params,
		peer:       peer,
		listenConn: listenConn,
		k:          k,
		t:          t,
		disp:       newDispatcher(),
		groups:     make(map[int]string),
		cancels:    make(map[int]context.CancelFunc),
	}
}

// onAllocated записывается в sm.params.OnAllocated до запуска первого
// слота - общий на весь hot-set, различает слоты по streamID. Вызывается
// из oneTURN, т.е. из per-slot горутины, конкурентно с остальными - отсюда
// мьютекс на groups (в отличие от nextID/cancels, которые трогает только
// горутина run()).
func (sm *sessionManager) onAllocated(streamID int, addr *net.UDPAddr) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.groups[streamID] = groupPrefix24(addr.String())
}

// launchSlot поднимает DTLSLoop/TURNLoop для нового streamID и заводит
// slotHandle для диспетчера - ТЕ ЖЕ функции, что Run() запускал раньше для
// каждого из N стримов, просто с собственным маленьким inbound-каналом
// вместо общего. barrierCh, если не nil, получает один сигнал при первом
// успешном handshake этого слота - используется только для стартового
// барьера первого слота hot-set'а (см. run(), тот же барьер, что раньше
// был в Run() для стрима 1). Каждое успешное (пере)подключение слота
// дополнительно отмечается в диспетчере через markUp для liveness-failover.
func (sm *sessionManager) launchSlot(ctx context.Context, wg *sync.WaitGroup, streamID int, barrierCh chan<- struct{}) *slotHandle {
	slotCtx, cancel := context.WithCancel(ctx)
	sm.cancels[streamID] = cancel

	slot := &slotHandle{
		streamID: streamID,
		inbound:  make(chan *Packet, slotInboundBufferSize),
		up:       make(chan struct{}, 1),
	}

	cchan := make(chan net.PacketConn)
	wg.Add(1)
	go func() {
		defer wg.Done()
		DTLSLoop(slotCtx, sm.deps, sm.params, sm.peer, sm.listenConn, slot.inbound, cchan, slot.up, streamID)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		TURNLoop(slotCtx, sm.deps, sm.params, sm.peer, cchan, sm.t, streamID)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		first := true
		for {
			select {
			case <-slotCtx.Done():
				return
			case <-slot.up:
				sm.disp.markUp(streamID, time.Now())
				if first && barrierCh != nil {
					select {
					case barrierCh <- struct{}{}:
					default:
					}
				}
				first = false
			}
		}
	}()

	return slot
}

// run запускает начальный hot-set: первый слот один, с барьером прогрева
// кэша credentials (идентично тому, как раньше Run() ждал стрим 1 перед
// запуском 2..N), затем оставшиеся K-1 сразу следом. После этого ведёт
// dispatcher и refresh-цикл (см. Task 8's refreshOne) до отмены ctx.
func (sm *sessionManager) run(ctx context.Context, inboundChan <-chan *Packet, rotateCh <-chan struct{}) {
	wg := sync.WaitGroup{}
	sm.nextID = 1

	barrierCh := make(chan struct{}, 1)
	first := sm.launchSlot(ctx, &wg, 1, barrierCh)

	select {
	case <-barrierCh:
	case <-ctx.Done():
	case <-time.After(streamStartBarrier):
	}

	slots := []*slotHandle{first}
	for i := 1; i < sm.k; i++ {
		sm.nextID++
		slots = append(slots, sm.launchSlot(ctx, &wg, sm.nextID, nil))
	}
	sm.disp.setSlots(slots, first.streamID)

	dispDone := make(chan struct{})
	go func() {
		defer close(dispDone)
		sm.disp.run(ctx, inboundChan, rotateCh)
	}()

	sm.refreshLoop(ctx, &wg)

	wg.Wait()
	<-dispDone
}

// hotSetRefreshInterval - как часто sessionManager рассматривает замену
// одного неактивного члена hot-set'а на свежего кандидата. НЕ
// откалибровано живым замером - стартовая точка по спеке, требует
// эмпирической калибровки (см. dispatcher.go's rotateThresholdBytes comment
// for the same caveat).
const hotSetRefreshInterval = 5 * time.Minute

// refreshLoop periodically calls refreshOne while ctx is alive. Runs on the
// same goroutine as run() (called at the end of it, see Task 7) rather than
// its own - nextID and cancels are only ever touched from here or from
// launchSlot, which this goroutine also calls, so neither field needs its
// own lock (see the sessionManager doc comment).
func (sm *sessionManager) refreshLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(hotSetRefreshInterval)
	defer ticker.Stop()
	lastRefresh := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if shouldRefresh(lastRefresh, now, hotSetRefreshInterval) {
				sm.refreshOne(ctx, wg)
				lastRefresh = now
			}
		}
	}
}

// refreshOne retires one non-active, group-over-represented hot-set member
// (see pickReplacementCandidate) and launches a fresh candidate (next
// sequential streamID) in its place. A no-op if nothing is clearly
// over-represented right now (including while some members' groups are
// still unknown - see Params.OnAllocated) - degrades to no churn, never to
// an error.
func (sm *sessionManager) refreshOne(ctx context.Context, wg *sync.WaitGroup) {
	slots := sm.disp.currentSlots()
	if len(slots) == 0 {
		return
	}
	ids := make([]int, len(slots))
	for i, s := range slots {
		ids[i] = s.streamID
	}
	active := sm.disp.activeStreamID()

	sm.mu.Lock()
	groupsCopy := make(map[int]string, len(sm.groups))
	for k, v := range sm.groups {
		groupsCopy[k] = v
	}
	sm.mu.Unlock()

	retireID := pickReplacementCandidate(ids, active, groupsCopy)
	if retireID < 0 {
		return
	}

	sm.nextID++
	fresh := sm.launchSlot(ctx, wg, sm.nextID, nil)

	newSlots := make([]*slotHandle, 0, len(slots))
	for _, s := range slots {
		if s.streamID == retireID {
			continue
		}
		newSlots = append(newSlots, s)
	}
	newSlots = append(newSlots, fresh)
	sm.disp.setSlots(newSlots, active)

	if cancel, ok := sm.cancels[retireID]; ok {
		cancel()
		delete(sm.cancels, retireID)
	}
	sm.mu.Lock()
	delete(sm.groups, retireID)
	sm.mu.Unlock()
}
