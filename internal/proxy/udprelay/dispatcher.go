package udprelay

import (
	"context"
	"sync"
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

	// health - per-slot throughput+RTT signal (см. slothealth.go). Только
	// логирование на Шаге 1 - route() его не читает.
	health *slotHealth
}

// slotInboundBufferSize - маленький буфер на слот. Живой слот (oneDTLS
// вычитывает inboundChan почти со скоростью сети) держит его почти всегда
// пустым; мёртвый (между reconnect-попытками DTLSLoop, до 10-30s backoff)
// заполняет его за пару пакетов и route() просто роняет пакет через
// default - см. route().
const slotInboundBufferSize = 4

// dispatcher - единственный читатель общего inboundChan (см. run.go).
// Маршрутизирует пакеты round-robin'ом по всем живым членам hot-set'а (см.
// route()). active/rotateManual остаются только как учёт "какой слот
// сейчас защищён от retire в replaceSlot" для sessionManager.refreshOne -
// на фактическую маршрутизацию пакетов больше не влияют (см. route()'s
// doc comment - разгрузка на единственный активный слот и её
// liveness-failover убраны 2026-08-24 по прямой просьбе). Не открывает и
// не закрывает сами TURN/DTLS-сессии - этим занимается sessionManager;
// dispatcher только маршрутизирует пакеты уже поднятых слотов.
type dispatcher struct {
	mu     sync.Mutex
	slots  []*slotHandle
	active int

	roundRobin int // индекс для route()'s round-robin по всем слотам
}

func newDispatcher() *dispatcher {
	return &dispatcher{}
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

// rotateManual сдвигает d.active на следующий слот по кругу - это больше не
// влияет на маршрутизацию пакетов (см. route()), только на то, какой слот
// replaceSlot защищает от retire. Оставлен как no-op в этом смысле для
// совместимости с существующим ручным триггером (stdin 'rotate',
// mobile.TriggerRotate) и тестами; вызывается из run() при получении
// сигнала на rotateCh.
func (d *dispatcher) rotateManual() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.slots) == 0 {
		return
	}
	d.active = (d.active + 1) % len(d.slots)
}

// route отдаёт один пакет очередному слоту round-robin'ом по ВСЕМ живым
// членам hot-set'а. Разгрузка на единственный "активный" слот убрана по
// прямой просьбе 2026-08-24 - она душила пропускную способность до
// потолка одного relay-пути (единственный путь = единственная пропускная
// способность, а не разнообразие, ради которого city заводился hot-set
// изначально). Round-robin возвращает параллелизм ценой части
// endpoint-стабильности на сервере, которую чинила session-affinity
// (docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md) -
// осознанный откат, не забытая недоделка.
func (d *dispatcher) route(pkt *Packet) {
	d.mu.Lock()
	if len(d.slots) == 0 {
		d.mu.Unlock()
		packetPool.Put(pkt)
		return
	}
	target := d.slots[d.roundRobin%len(d.slots)]
	d.roundRobin++
	d.mu.Unlock()

	select {
	case target.inbound <- pkt:
	default:
		packetPool.Put(pkt)
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
