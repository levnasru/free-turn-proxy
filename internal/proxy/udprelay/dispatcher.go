package udprelay

import (
	"context"
	"sync"
	"time"
)

// slotHandle is what the dispatcher needs to route packets to, and observe
// the liveness of, one hot-set member. In production its fields are the
// SAME channels passed as DTLSLoop's inboundChan/okchan arguments (see
// sessionManager.launchSlot in sessionmgr.go), so wiring a slotHandle in
// doesn't change oneDTLS/oneTURN's behavior at all - the dispatcher is just
// a new writer/reader sitting between the listener and the existing
// per-stream loop. Tests build slotHandles directly and never launch
// DTLSLoop, exercising dispatcher logic with no network at all.
type slotHandle struct {
	streamID int
	inbound  chan *Packet  // dispatcher writes; DTLSLoop's write-goroutine reads it as its inboundChan
	up       chan struct{} // DTLSLoop signals here on every successful handshake, including reconnects (its okchan)
}

// slotInboundBufferSize - маленький буфер на слот, НАМЕРЕННО мал: живой
// слот (oneDTLS вычитывает inboundChan почти со скоростью сети) держит его
// почти всегда пустым, а мёртвый (между reconnect-попытками DTLSLoop, до
// 10-30s backoff) заполняет его за пару пакетов - переполнение служит
// дешёвым сигналом "слот сейчас не читает", см. maxConsecutiveDropsBeforeFailover.
const slotInboundBufferSize = 4

// rotateInterval - как долго активный слот остаётся активным до плановой
// ротации на следующего кандидата hot-set'а. Триггер по ВРЕМЕНИ, не по
// объёму байт: байтовый порог структурно смещён против цели "усреднение по
// пулу" из спеки (docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md,
// раздел "Интерпретация") - медленному пути дольше набрать тот же объём,
// значит он и получает БОЛЬШЕ эфирного времени, а не меньше. Живой замер
// throughput 2026-08-24 подтвердил эффект (0.3 МБ/с при 2МиБ-пороге, как
// будто выдан один пир). Время даёт каждому кандидату РАВНОЕ эфирное время
// вне зависимости от его скорости - корректная "усреднение по пулу".
// НЕ окончательно откалибровано - стартовая точка. var (не const) только
// затем, чтобы dispatcher_test.go мог временно подставить короткий интервал
// вместо ожидания секунд в юнит-тесте - в бою значение не меняется.
var rotateInterval = 1500 * time.Millisecond

// maxConsecutiveDropsBeforeFailover - сколько подряд неудачных попыток
// отдать пакет активному слоту считать его мёртвым и переключаться
// немедленно, не дожидаясь плановой ротации по объёму.
const maxConsecutiveDropsBeforeFailover = 5

// dispatcher - единственный читатель общего inboundChan (см. run.go).
// Владеет индексом активного слота hot-set'а, переключает его по
// истечении времени, по ручному триггеру и по liveness-отказу активного
// слота. Не открывает и не закрывает сами TURN/DTLS-сессии - этим
// занимается sessionManager; dispatcher только маршрутизирует пакеты уже
// поднятых слотов.
type dispatcher struct {
	mu     sync.Mutex
	slots  []*slotHandle
	active int

	lastRotate       time.Time
	consecutiveDrops int
	lastUp           map[int]time.Time // streamID -> время последнего up-сигнала
}

func newDispatcher() *dispatcher {
	return &dispatcher{lastUp: make(map[int]time.Time), lastRotate: time.Now()}
}

// setSlots (пере)задаёт состав hot-set'а. keepActiveStreamID - какой слот
// должен стать активным (первый слот из slots, если его там нет, например
// он и есть только что подключённый первый). Вызывается sessionManager'ом
// при старте и при каждой замене одного члена на refresh-тике.
func (d *dispatcher) setSlots(slots []*slotHandle, keepActiveStreamID int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slots = slots
	d.active = 0
	for i, s := range slots {
		if s.streamID == keepActiveStreamID {
			d.active = i
			break
		}
	}
	d.lastRotate = time.Now()
	d.consecutiveDrops = 0
}

// replaceSlot atomically retires oldStreamID and installs newSlot in its
// place, IF oldStreamID is not the currently active slot at the moment the
// swap actually happens (not at some earlier snapshot) - closes a TOCTOU
// window where sessionManager.refreshOne's separate read-decide-write calls
// could otherwise retire a slot the dispatcher had already rotated onto
// between the read and the write, silently reverting a legitimate rotation
// and tearing down live traffic. Returns false (no-op) if oldStreamID is
// currently active or not found in the hot-set - caller should treat that
// as "nothing to do this tick", not an error.
func (d *dispatcher) replaceSlot(oldStreamID int, newSlot *slotHandle) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.slots) == 0 || d.slots[d.active].streamID == oldStreamID {
		return false
	}
	for i, s := range d.slots {
		if s.streamID == oldStreamID {
			newSlots := make([]*slotHandle, len(d.slots))
			copy(newSlots, d.slots)
			newSlots[i] = newSlot
			d.slots = newSlots
			delete(d.lastUp, oldStreamID)
			return true
		}
	}
	return false
}

// currentSlots возвращает копию текущего состава hot-set'а - для
// sessionManager.refreshOne, чтобы решать, кого заменить, не держа мьютекс
// диспетчера дольше одного вызова.
func (d *dispatcher) currentSlots() []*slotHandle {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*slotHandle, len(d.slots))
	copy(out, d.slots)
	return out
}

// activeStreamID возвращает streamID текущего активного слота, 0 если
// hot-set пуст.
func (d *dispatcher) activeStreamID() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.slots) == 0 {
		return 0
	}
	return d.slots[d.active].streamID
}

// markUp записывает момент последнего успешного handshake слота streamID.
// Вызывается отдельной горутиной-наблюдателем на каждый слот (см.
// sessionManager.launchSlot) - фан-ин через мьютекс вместо динамического
// select по растущему/убывающему числу каналов (состав hot-set'а меняется
// во время refresh).
func (d *dispatcher) markUp(streamID int, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastUp[streamID] = at
}

// rotateManual переключает активный слот немедленно, в обход таймера -
// вызывается из run() при получении сигнала на rotateCh.
func (d *dispatcher) rotateManual() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rotateLocked()
}

// rotateLocked переключает активный индекс на следующий слот по кругу.
// Вызывающий обязан держать d.mu.
func (d *dispatcher) rotateLocked() {
	if len(d.slots) == 0 {
		return
	}
	d.active = (d.active + 1) % len(d.slots)
	d.lastRotate = time.Now()
	d.consecutiveDrops = 0
}

// route отдаёт один пакет активному слоту, проверяет, не истёк ли таймер
// плановой ротации, и отслеживает подряд идущие отказы для liveness-failover.
func (d *dispatcher) route(pkt *Packet) {
	d.mu.Lock()
	if len(d.slots) == 0 {
		d.mu.Unlock()
		packetPool.Put(pkt)
		return
	}
	active := d.slots[d.active]
	d.mu.Unlock()

	select {
	case active.inbound <- pkt:
		d.mu.Lock()
		d.consecutiveDrops = 0
		if time.Since(d.lastRotate) >= rotateInterval {
			d.rotateLocked()
		}
		d.mu.Unlock()
	default:
		packetPool.Put(pkt)
		d.mu.Lock()
		d.consecutiveDrops++
		if d.consecutiveDrops >= maxConsecutiveDropsBeforeFailover {
			d.failoverLocked()
		}
		d.mu.Unlock()
	}
}

// run - главный цикл диспетчера: единственный читатель inboundChan,
// опционально слушает rotateCh на ручной триггер (nil rotateCh блокируется
// навсегда в select - безопасно, TCP+bond им не пользуется). Возвращается
// при отмене ctx.
func (d *dispatcher) run(ctx context.Context, inboundChan <-chan *Packet, rotateCh <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-inboundChan:
			d.route(pkt)
		case <-rotateCh:
			d.rotateManual()
		}
	}
}

// failoverLocked switches the active slot to the most recently up-signaled
// among the OTHERS (never the one that just failed) - the best available
// proxy for "pick a live one" without a separate down-signal (oneDTLS/
// DTLSLoop don't expose one; adding it would mean changing their internals,
// which this feature deliberately avoids - see loop.go's doc comment and
// the spec's "no changes inside them" constraint). Falls back to plain
// round-robin if no other slot has ever signaled up yet. Caller must hold d.mu.
func (d *dispatcher) failoverLocked() {
	if len(d.slots) <= 1 {
		d.lastRotate = time.Now()
		d.consecutiveDrops = 0
		return
	}
	deadID := d.slots[d.active].streamID
	best := -1
	var bestTime time.Time
	for i, s := range d.slots {
		if s.streamID == deadID {
			continue
		}
		if t := d.lastUp[s.streamID]; t.After(bestTime) {
			bestTime = t
			best = i
		}
	}
	if best >= 0 {
		d.active = best
	} else {
		d.active = (d.active + 1) % len(d.slots)
	}
	d.lastRotate = time.Now()
	d.consecutiveDrops = 0
}
