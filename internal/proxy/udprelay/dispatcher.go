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

	// health - per-slot throughput+RTT signal (см. slothealth.go). Только
	// логирование на Шаге 1 - route() его не читает.
	health *slotHealth

	// launchedAt - когда sessionManager.launchSlot поднял этот слот. Только
	// для refreshOne's pickAgeExpiredCandidate (P14, 2026-08-29) - route() его
	// не читает. Пишется один раз при создании, до публикации слота в
	// dispatcher, дальше не мутируется - читать без мьютекса безопасно.
	launchedAt time.Time
}

// slotInboundBufferSize - буфер на слот (16 пакетов). Предоставляет запас
// для микробатчинга, исключая дропы пакетов при обработке пачки DTLS-воркером.
const (
	slotInboundBufferSize = 16
	defaultBatchSize      = 4 // число последовательных пакетов в один слот перед ротацией (микробатчинг)
)

// dispatcher - единственный читатель общего inboundChan (см. run.go).
// Маршрутизирует пакеты микробатчингом round-robin по всем живым членам hot-set'а (см.
// route()). active/rotateManual остаются только как учёт "какой слот
// сейчас защищён от retire в replaceSlot" для sessionManager.refreshOne -
// на фактическую маршрутизацию пакетов больше не влияют. Не открывает и
// не закрывает сами TURN/DTLS-сессии - этим занимается sessionManager;
// dispatcher только маршрутизирует пакеты уже поднятых слотов.
type dispatcher struct {
	mu     sync.Mutex
	slots  []*slotHandle
	active int

	roundRobin int // индекс для route()'s round-robin по всем слотам
	burstCount int // число пакетов, уже отправленных в текущий слот в рамках батча
	batchSize  int // размер батча (по умолчанию defaultBatchSize)
}

func newDispatcher() *dispatcher {
	return newDispatcherWithBatch(defaultBatchSize)
}

func newDispatcherWithBatch(batchSize int) *dispatcher {
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	return &dispatcher{batchSize: batchSize}
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
	d.burstCount = 0
	if len(slots) > 0 {
		d.roundRobin = d.roundRobin % len(slots)
	} else {
		d.roundRobin = 0
	}
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

// addSlot добавляет новый слот в конец hot-set'а - живой рост K (Шаг 3).
// Не трогает d.active - индексы существующих членов не меняются, значит и
// защита replaceSlot/removeSlot от TOCTOU не ломается.
func (d *dispatcher) addSlot(slot *slotHandle) {
	d.mu.Lock()
	defer d.mu.Unlock()
	newSlots := make([]*slotHandle, len(d.slots)+1)
	copy(newSlots, d.slots)
	newSlots[len(d.slots)] = slot
	d.slots = newSlots
}

// removeSlot убирает streamID из hot-set'а - живое уменьшение K (Шаг 3).
// Тот же TOCTOU-guard, что и в replaceSlot: отказывает, если streamID -
// текущий активный (диспетчер мог переключиться на него между чтением
// состава вызывающим и этим вызовом) или не найден. Возвращает true, если
// реально убрали.
func (d *dispatcher) removeSlot(streamID int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.slots) == 0 || d.slots[d.active].streamID == streamID {
		return false
	}
	for i, s := range d.slots {
		if s.streamID == streamID {
			newSlots := make([]*slotHandle, 0, len(d.slots)-1)
			newSlots = append(newSlots, d.slots[:i]...)
			newSlots = append(newSlots, d.slots[i+1:]...)
			d.slots = newSlots
			if d.active > i {
				d.active--
			}
			if len(newSlots) > 0 {
				d.roundRobin = d.roundRobin % len(newSlots)
			} else {
				d.roundRobin = 0
			}
			d.burstCount = 0
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

// route отдаёт пакет очередному слоту микробатчингом (batchSize пакетов
// подряд в один слот) по кругу среди живых членов hot-set'а.
// Микробатчинг предотвращает разрывы порядка (out-of-order) в TCP-потоках
// внутри коротких всплесков, из-за которых TCP режет cwnd и генерирует
// тысячи DUPACK. Если текущий целевой слот полон (перегружен или отвалился),
// диспетчер перенаправляет пакет следующему свободному слоту и переносит
// батч на него.
func (d *dispatcher) route(pkt *Packet) {
	d.mu.Lock()
	n := len(d.slots)
	if n == 0 {
		d.mu.Unlock()
		packetPool.Put(pkt)
		return
	}
	startIdx := d.roundRobin % n
	slots := d.slots
	bs := d.batchSize
	if bs <= 0 {
		bs = defaultBatchSize
	}
	d.mu.Unlock()

	for i := 0; i < n; i++ {
		idx := (startIdx + i) % n
		target := slots[idx]
		select {
		case target.inbound <- pkt:
			d.mu.Lock()
			if len(d.slots) > 0 {
				if idx != d.roundRobin%len(d.slots) {
					// Слот по умолчанию был занят; переносим указатель на принявший слот и начинаем новый батч
					d.roundRobin = idx % len(d.slots)
					d.burstCount = 1
				} else {
					d.burstCount++
					if d.burstCount >= bs {
						d.burstCount = 0
						d.roundRobin = (d.roundRobin + 1) % len(d.slots)
					}
				}
			}
			d.mu.Unlock()
			return
		default:
		}
	}
	packetPool.Put(pkt)
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
