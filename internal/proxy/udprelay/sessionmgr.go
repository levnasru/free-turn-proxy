package udprelay

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/proxy/common"
	"github.com/samosvalishe/free-turn-proxy/internal/stats"
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
	grad *gradientTracker // Шаг 2: скользящий RTT-baseline для gradientLoop

	// Шаг 4: решающий слой между предложением градиента и реальным ресайзом.
	// baseK - стартовое K (= cfg.TURN.N), из него считаются пол и потолок;
	// неизменен после конструктора, поэтому читается без синхронизации.
	// auto трогает только горутина gradientLoop. autoEnabled - живой тумблер
	// (stdin-команда "auto"), пишется из refreshLoop, читается из
	// gradientLoop, отсюда atomic. autoCh - решение gradientLoop'а на
	// применение в refreshLoop: сам ресайз обязан идти из горутины run()'s,
	// потому что трогает nextID/cancels (см. комментарий к полям ниже).
	baseK       int
	auto        *autoscaleDecider
	autoEnabled atomic.Bool
	autoCh      chan int

	nextID int // следующий свободный streamID; трогает только run()'s горутина

	mu      sync.Mutex // защищает только groups - onAllocated зовётся из per-slot горутин
	groups  map[int]string
	cancels map[int]context.CancelFunc // трогает только run()'s горутина (launchSlot/refreshOne)
}

func newSessionManager(deps *Deps, params *Params, peer *net.UDPAddr, listenConn net.PacketConn, k int, t <-chan time.Time) *sessionManager {
	if k <= 0 {
		k = 1
	}
	batchSize := defaultBatchSize
	if params != nil && params.BatchSize > 0 {
		batchSize = params.BatchSize
	}
	sm := &sessionManager{
		deps:       deps,
		params:     params,
		peer:       peer,
		listenConn: listenConn,
		k:          k,
		t:          t,
		disp:       newDispatcherWithBatch(batchSize),
		grad:       newGradientTracker(),
		baseK:      k,
		auto:       &autoscaleDecider{},
		autoCh:     make(chan int, 1),
		groups:     make(map[int]string),
		cancels:    make(map[int]context.CancelFunc),
	}
	// Автоскейлер включён по умолчанию: без этого Шаг 4 на Android'е (где
	// stdin есть только у ядра-подпроцесса, а кнопки в UI пока нет) остался бы
	// мёртвым кодом. Выключается на живую stdin-командой "auto".
	sm.autoEnabled.Store(true)
	return sm
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
// был в Run() для стрима 1).
func (sm *sessionManager) launchSlot(ctx context.Context, wg *sync.WaitGroup, streamID int, barrierCh chan<- struct{}) *slotHandle {
	slotCtx, cancel := context.WithCancel(ctx)
	sm.cancels[streamID] = cancel

	health := newSlotHealth()
	slot := &slotHandle{
		streamID:   streamID,
		inbound:    make(chan *Packet, slotInboundBufferSize),
		up:         make(chan struct{}, 1),
		health:     health,
		launchedAt: time.Now(),
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
		TURNLoop(slotCtx, sm.deps, sm.params, sm.peer, cchan, sm.t, streamID, health)
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
// dispatcher и refresh-цикл (см. Task 8's refreshOne) до отмены ctx. growCh/
// shrinkCh - ручной триггер живого ресайза hot-set'а (Шаг 3, см.
// growHotSet/shrinkHotSet), autoToggleCh - тумблер автоскейлера (Шаг 4);
// nil-канал блокируется в select навсегда - безопасно, TCP+bond ими не
// пользуется, как и rotateCh.
func (sm *sessionManager) run(ctx context.Context, inboundChan <-chan *Packet, rotateCh, growCh, shrinkCh, autoToggleCh <-chan struct{}) {
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

	go sm.logHealthLoop(ctx)
	go sm.gradientLoop(ctx)

	sm.refreshLoop(ctx, &wg, growCh, shrinkCh, autoToggleCh)

	wg.Wait()
	<-dispDone
}

// growHotSet launches one more slot and appends it to the dispatcher's live
// hot-set - K goes up by one (Шаг 3, живой ресайз). Mirrors launchSlot's use
// in refreshOne, but appends instead of swapping - dispatcher.addSlot never
// touches existing members, so unlike replaceSlot there's no TOCTOU window
// to guard here.
func (sm *sessionManager) growHotSet(ctx context.Context, wg *sync.WaitGroup) {
	sm.nextID++
	fresh := sm.launchSlot(ctx, wg, sm.nextID, nil)
	sm.disp.addSlot(fresh)
	sm.k++
	sm.deps.log().Infof("[HOTSET] выросли до K=%d (добавлен поток %d)", sm.k, fresh.streamID)
}

// shrinkHotSet retires one slot (worst measured RTT - see
// pickSlotToRetireForShrink) and shrinks K by one (Шаг 3, живой ресайз).
// No-op if only one slot remains (never shrink to zero) or the dispatcher
// refuses the removal (target became active between the read and the
// removal - same TOCTOU class replaceSlot guards against).
func (sm *sessionManager) shrinkHotSet(ctx context.Context) {
	slots := sm.disp.currentSlots()
	if len(slots) <= 1 {
		return
	}
	active := sm.disp.activeStreamID()

	ids := make([]int, len(slots))
	rtts := make(map[int]time.Duration, len(slots))
	for i, s := range slots {
		ids[i] = s.streamID
		rtts[s.streamID] = s.health.rtt()
	}

	retireID := pickSlotToRetireForShrink(ids, active, rtts)
	if retireID < 0 || !sm.disp.removeSlot(retireID) {
		return
	}

	if cancel, ok := sm.cancels[retireID]; ok {
		delete(sm.cancels, retireID)
		// Даём retiring-слоту graceful drain период (5 секунд), чтобы
		// дослать уже находящиеся в очереди пакеты и принять ответы с сервера,
		// пока серверный WireGuard переключается на оставшиеся активные потоки.
		go func(c context.CancelFunc) {
			select {
			case <-ctx.Done():
				c()
			case <-time.After(5 * time.Second):
				c()
			}
		}(cancel)
	}
	sm.mu.Lock()
	delete(sm.groups, retireID)
	sm.mu.Unlock()
	sm.k--
	sm.deps.log().Infof("[HOTSET] сжались до K=%d (убран поток %d)", sm.k, retireID)
}

// hotSetRefreshInterval - как часто sessionManager рассматривает замену
// одного неактивного члена hot-set'а на свежего кандидата. НЕ
// откалибровано живым замером - стартовая точка по спеке, требует
// эмпирической калибровки (см. dispatcher.go's rotateThresholdBytes comment
// for the same caveat).
const hotSetRefreshInterval = 5 * time.Minute

// slotHealthLogInterval - как часто logHealthLoop печатает per-slot
// throughput+RTT в Debug. Шаг 1: только видимость для живой проверки на
// железе, ни на что не влияет.
const slotHealthLogInterval = 5 * time.Second

// logHealthLoop периодически логирует health (throughput+RTT) каждого члена
// hot-set'а на уровне Debug - для живой проверки на реальном железе, что
// сигнал вообще приходит вменяемым. Не читается ни route(), ни refreshOne -
// Шаг 1 это чистая наблюдаемость, до градиента по K дело ещё не дошло.
func (sm *sessionManager) logHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(slotHealthLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, s := range sm.disp.currentSlots() {
				tx, rx := s.health.stats.Counters()
				sm.deps.log().Debugf("[STREAM %d] health: rtt=%s tx=%s rx=%s",
					s.streamID, s.health.rtt(), stats.FormatByteCount(tx), stats.FormatByteCount(rx))
			}
		}
	}
}

// gradientLogInterval - как часто gradientLoop печатает предложение по
// размеру hot-set'а. Реже, чем slotHealthLogInterval - это агрегат по всему
// hot-set'у, не per-slot событие, и решение о ресайзе не должно дёргаться
// каждые 5с. НЕ откалибровано живым замером.
const gradientLogInterval = 30 * time.Second

// gradientLoop - раз в gradientLogInterval считает средний RTT по текущему
// hot-set'у, кормит им скользящий baseline (sm.grad) и отдаёт получившийся
// gradient решающему слою Шага 4 (autoDecide -> autoscaleDecider). Сам ресайз
// не делает: решение уходит в sm.autoCh, применяет его refreshLoop, у которого
// на это есть право (nextID/cancels). См. gradient.go про формулу и её
// ограничение (RTT неотличим от честной загруженности канала), autoscale.go -
// про пороги, гистерезис и кулдаун.
//
// K берётся как len(slots), а НЕ sm.k: sm.k пишут growHotSet/shrinkHotSet из
// горутины run()'s, читать его отсюда было гонкой (существовала с Шага 2).
// Реальный размер hot-set'а и так живёт в диспетчере.
func (sm *sessionManager) gradientLoop(ctx context.Context) {
	ticker := time.NewTicker(gradientLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slots := sm.disp.currentSlots()
			k := len(slots)
			var sum time.Duration
			var n int
			for _, s := range slots {
				if rtt := s.health.rtt(); rtt > 0 {
					sum += rtt
					n++
				}
			}
			if n == 0 {
				// Ни одного слота с измеренным RTT: сигнала нет. Тик всё
				// равно отдаём решающему слою нейтралью - иначе внутри него
				// стоит время, и кулдаун после длинного провала окажется
				// "уже истёкшим" по данным, которых не было.
				sm.autoDecide(0, k)
				continue
			}
			avgRTT := sum / time.Duration(n)
			sm.grad.observe(avgRTT)
			minRTT := sm.grad.baseline()

			gradient, suggested := suggestedHotSetSize(k, avgRTT, minRTT)
			delta := sm.autoDecide(gradient, k)
			sm.deps.log().Debugf(
				"[GRADIENT] K=%d avgRTT=%s minRTT=%s gradient=%.3f suggestedK=%d delta=%+d",
				k, avgRTT, minRTT, gradient, suggested, delta,
			)
			if sm.deps.log().DebugEnabled() {
				tx, rx := sm.trafficTotals()
				appendGradientLog(time.Now(), k, avgRTT, minRTT, gradient, suggested, delta, tx, rx)
			}
		}
	}
}

// autoDecide прокручивает решающий слой (Шаг 4) ровно на один тик
// gradientLoop и, если решение непустое и автоскейлер включён, просит
// refreshLoop применить его. Возвращает решение как есть - в лог и в CSV оно
// идёт независимо от того, применяется ли: при выключенном автоскейлере это и
// есть dry-run-запись, по которой видно, что слой сделал бы.
func (sm *sessionManager) autoDecide(gradient float64, k int) int {
	b := autoscaleBoundsFor(sm.baseK)
	delta, refused := sm.auto.decide(gradient, k, b)
	if refused != 0 {
		// Серия подтверждений собралась целиком и кулдаун истёк, но зажим по
		// [min..max] оставил K тем же. Уровень Debug, а не Info: у того, кто
		// крутится на своём N (потолок == -n), здоровый канал даёт этот отказ
		// регулярно, в Info он забил бы лог. Зато без строки вообще отказ
		// выглядит ровно как "сигнала не было" - см. комментарий к decide.
		action, name, bound := "рост", "потолок", b.max
		if refused < 0 {
			action, name, bound = "сжатие", "пол", b.min
		}
		sm.deps.log().Debugf("[HOTSET-AUTO] %s K=%d -> %d отказан: %s=%d (gradient=%.3f)",
			action, k, k+refused, name, bound, gradient)
	}
	if delta == 0 {
		return 0
	}
	if !sm.autoEnabled.Load() {
		sm.deps.log().Infof("[HOTSET-AUTO] выключен: сейчас изменил бы K=%d -> %d (gradient=%.3f)",
			k, k+delta, gradient)
		return delta
	}
	select {
	case sm.autoCh <- delta:
		sm.deps.log().Infof("[HOTSET-AUTO] решение K=%d -> %d (gradient=%.3f)", k, k+delta, gradient)
	default:
		// refreshLoop ещё не разобрал предыдущее решение (буфер 1). Кулдаун
		// уже сброшен, серия обнулена - следующая попытка будет не раньше,
		// чем через новую полную серию подтверждений. Так и надо: если
		// refreshLoop занят дольше 30с, добавлять ему очередь решений незачем.
		sm.deps.log().Infof("[HOTSET-AUTO] решение K=%d -> %d отброшено: refreshLoop занят", k, k+delta)
	}
	return delta
}

// trafficTotals - накопленные с старта процесса байты туннеля из ОБЩЕГО
// счётчика (Params.TrafficStats, тот же, что кормит [STATS]-строку), а не
// сумма по слотам hot-set'а: у per-slot счётчиков байты уходящего слота
// исчезают вместе с ним, то есть сумма врала бы ровно в момент ресайза - тот
// самый, который мы и хотим измерить. nil - нули: счётчик по контракту
// Params не обязателен.
func (sm *sessionManager) trafficTotals() (tx, rx uint64) {
	if sm.params.TrafficStats == nil {
		return 0, 0
	}
	return sm.params.TrafficStats.Counters()
}

// gradientLogFile - относительный путь (CWD ядра - на Android это
// context.filesDir, на desktop - рабочая директория запуска, тот же
// принцип, что у -hub-cache) для построчного CSV-хвоста [GRADIENT]-тиков.
// Причина отдельного файла: Android-приложение держит только 200 последних
// строк в своём буфере логов (ProxyServiceState) - при 30 строках/тик от
// per-slot health-лога это ~30с истории, а с одним лишь GRADIENT+STATS -
// всё равно только ~25 минут. Для накопления сигнала за дни обычного
// использования (нужно для Шага 4) кольцевой буфер не годится ни в каком
// виде - файл переживает и это, и рестарты процесса.
const gradientLogFile = "gradient-log.csv"

// appendGradientLog дописывает один [GRADIENT]-тик в gradientLogFile.
// Ошибка открытия/записи проглатывается - это диагностика, не должна ронять
// сессию. Пишет заголовок CSV один раз, если файл пуст/только что создан.
//
// delta - решение слоя Шага 4 на этом тике (-1/0/+1). Оно пишется всегда,
// включая выключенный автоскейлер: тогда столбец delta показывает, что слой
// сделал бы, а столбец k - что K не двинулся.
//
// txBytes/rxBytes - АБСОЛЮТНЫЕ счётчики туннеля с старта процесса, не
// приращение за тик. Абсолютные сознательно: скорость на любом отрезке
// считается как разность двух ЛЮБЫХ строк, делённая на разность их timestamp,
// в том числе через пропуски (тик без измеренного RTT строки не пишет вовсе).
// Падение значения между соседними строками - не потеря байт, а рестарт ядра;
// заодно это бесплатный детектор рестарта в дополнение к сетке секунд в
// timestamp.
//
// Столбцов росло по мере шагов: 6 до Шага 4, 7 с delta, 9 с байтами. Разбор по
// $2/$5 работает на всех трёх, по $7 - начиная с Шага 4.
func appendGradientLog(ts time.Time, k int, avgRTT, minRTT time.Duration, gradient float64, suggested, delta int, txBytes, rxBytes uint64) {
	f, err := os.OpenFile(gradientLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr == nil && info.Size() == 0 {
		fmt.Fprintln(f, "timestamp,k,avg_rtt_ms,min_rtt_ms,gradient,suggested_k,delta,tx_bytes,rx_bytes")
	}
	fmt.Fprintf(f, "%s,%d,%.3f,%.3f,%.3f,%d,%d,%d,%d\n",
		ts.Format(time.RFC3339), k, avgRTT.Seconds()*1000, minRTT.Seconds()*1000, gradient, suggested, delta, txBytes, rxBytes)
}

// refreshLoop periodically calls refreshOne while ctx is alive. Runs on the
// same goroutine as run() (called at the end of it, see Task 7) rather than
// its own - nextID and cancels are only ever touched from here or from
// launchSlot, which this goroutine also calls, so neither field needs its
// own lock (see the sessionManager doc comment). По той же причине сюда, а не
// в gradientLoop, приходят решения автоскейлера (sm.autoCh, Шаг 4) - ресайз
// обязан идти из этой горутины.
func (sm *sessionManager) refreshLoop(ctx context.Context, wg *sync.WaitGroup, growCh, shrinkCh, autoToggleCh <-chan struct{}) {
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
		case <-growCh:
			sm.growHotSet(ctx, wg)
		case <-shrinkCh:
			sm.shrinkHotSet(ctx)
		case delta := <-sm.autoCh:
			if delta > 0 {
				sm.growHotSet(ctx, wg)
			} else {
				sm.shrinkHotSet(ctx)
			}
		case <-autoToggleCh:
			on := !sm.autoEnabled.Load()
			sm.autoEnabled.Store(on)
			if on {
				b := autoscaleBoundsFor(sm.baseK)
				sm.deps.log().Infof("[HOTSET-AUTO] включён, K в диапазоне [%d..%d]", b.min, b.max)
			} else {
				sm.deps.log().Infof("[HOTSET-AUTO] выключен, K остаётся %d (решения только в лог)", sm.k)
			}
		}
	}
}

// refreshOne retires one hot-set member and launches a fresh candidate (next
// sequential streamID) in its place. Two triggers, tried in order:
//
//  1. Age (pickAgeExpiredCandidate, P14 2026-08-29) - any slot alive past
//     common.CredentialSafetyMargin, oldest first. Takes priority because it
//     always eventually fires, unlike (2).
//  2. Diversity (pickReplacementCandidate) - retires an over-represented
//     group's member. Falls back to this only when nothing is age-eligible;
//     stalls permanently once the pool is fully spread across groups (see
//     its doc comment), which is exactly why (1) exists as a backstop.
//
// Neither picker exempts the dispatcher's active streamID (see their doc
// comments) - if the chosen retireID happens to be active, rotateManual()
// moves the pointer off it first (active carries no routing weight
// post-round-robin-rollback, see dispatcher.route()), so replaceSlot's own
// TOCTOU guard doesn't reject a legitimate, deliberately-chosen retirement.
// A no-op if nothing is eligible, or if the dispatcher's active slot changed
// again between that rotateManual() and replaceSlot's actual swap (same
// TOCTOU class replaceSlot always guards against).
func (sm *sessionManager) refreshOne(ctx context.Context, wg *sync.WaitGroup) {
	slots := sm.disp.currentSlots()
	if len(slots) == 0 {
		return
	}
	ids := make([]int, len(slots))
	launchedAt := make(map[int]time.Time, len(slots))
	for i, s := range slots {
		ids[i] = s.streamID
		launchedAt[s.streamID] = s.launchedAt
	}
	active := sm.disp.activeStreamID()

	retireID := pickAgeExpiredCandidate(ids, launchedAt, time.Now(), common.CredentialSafetyMargin)
	if retireID < 0 {
		sm.mu.Lock()
		groupsCopy := make(map[int]string, len(sm.groups))
		for k, v := range sm.groups {
			groupsCopy[k] = v
		}
		sm.mu.Unlock()
		retireID = pickReplacementCandidate(ids, active, groupsCopy)
	}
	if retireID < 0 {
		return
	}
	if retireID == active {
		sm.disp.rotateManual()
	}

	sm.nextID++
	fresh := sm.launchSlot(ctx, wg, sm.nextID, nil)

	if !sm.disp.replaceSlot(retireID, fresh) {
		// Диспетчер уже сам сменил активный слот на retireID (или тот
		// вообще исчез из hot-set'а) между чтением active выше и этим
		// моментом - отменяем свежий кандидат, чтобы не оставить висящую
		// TURN-аллокацию, и просто пропускаем этот тик обновления.
		if cancel, ok := sm.cancels[fresh.streamID]; ok {
			cancel()
			delete(sm.cancels, fresh.streamID)
		}
		return
	}

	if cancel, ok := sm.cancels[retireID]; ok {
		delete(sm.cancels, retireID)
		go func(c context.CancelFunc) {
			select {
			case <-ctx.Done():
				c()
			case <-time.After(5 * time.Second):
				c()
			}
		}(cancel)
	}
	sm.mu.Lock()
	delete(sm.groups, retireID)
	sm.mu.Unlock()
}
