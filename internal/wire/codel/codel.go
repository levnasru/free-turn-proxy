// Package codel реализует управление очередью по стандарту RFC 8289 (CoDel,
// "Controlled Delay") для связки между DTLS-слоем и paced TURN-отправкой
// (см. internal/proxy/udprelay/loop.go oneDTLS).
//
// Проблема: connutil.AsyncPacketPipe(), которым раньше соединялись DTLS-запись
// и paced TURN-отправка, ничем не ограничен (softLimit=0 означает "без лимита").
// При входящем потоке быстрее -obf-timing (например, быстрый TCP-всплеск от WireGuard)
// пакеты копились в памяти без предела (bufferbloat). Задержка росла до десятков
// секунд, TCP не получал своевременного сигнала о перегрузке, а затем сваливался
// в глубокий RTO backoff со штормом ретрансмитов.
//
// CoDel решает bufferbloat, отслеживая время нахождения пакета в очереди (sojourn time):
//   - Если задержка головы очереди ниже Target (30ms) или в очереди остался <= 1 пакет,
//     пакеты отдаются без вмешательства.
//   - Кратковременные всплески (< Interval, 100ms) проходят без потерь.
//   - При устойчивой перегрузке (задержка держится выше Target дольше Interval) CoDel
//     начинает прореживать очередь по закону control law: интервал между сбросами
//     сокращается как Interval / sqrt(count).
//   - При экстремальном переполнении (hardCap = 30 пакетов) применяется tail-drop
//     (отбрасывание входящего пакета, а не головы), что сохраняет строгий порядок
//     FIFO и защищает TCP от расщепления потока.
package codel

import (
	"math"
	"sync"
	"time"
)

const (
	// Target — целевая задержка пакета в очереди (RFC 8289 §5.3).
	// Калибровка под VK TURN: nominal pacing ~7ms (143pps), p50 интервал ~16ms.
	// 30ms соответствует ~4 пакетам в очереди — всплеск до 4 пакетов проходит
	// без каких-либо потерь.
	Target = 30 * time.Millisecond

	// Interval — скользящее окно подтверждения перегрузки (RFC 8289 §5.3).
	// Если задержка превышает Target непрерывно дольше Interval, очередь признаётся
	// перегруженной. 100ms (RFC default) даёт TCP ~2 RTT на адаптацию.
	Interval = 100 * time.Millisecond

	// DefaultHardCap — абсолютный предел очереди (защита памяти и верхней границы RTT).
	// При 7ms на пакет 30 пакетов = максимум 210ms задержки в наихудшем случае.
	// TCP никогда не получает многосекундного буферблота.
	DefaultHardCap = 30
)

type item struct {
	data     []byte
	enqueued time.Time
}

// Queue — потокобезопасная FIFO-очередь с алгоритмом RFC 8289 CoDel на Dequeue.
type Queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []item
	closed bool

	hardCap int
	noCoDel bool

	// Состояние CoDel (RFC 8289 §5.2)
	dropping       bool
	firstAboveTime time.Time
	dropNext       time.Time
	count          uint32
	lastCount      uint32

	// Статистика
	pushed  int64
	popped  int64
	dropped int64

	readDeadline time.Time
}

// ErrTimeout возвращается Pop при истечении дедлайна SetReadDeadline.
var ErrTimeout timeoutError

type timeoutError struct{}

func (timeoutError) Error() string   { return "codel: read deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// ErrClosed возвращается Pop при закрытии очереди.
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "codel: queue closed" }

// NewQueue создаёт очередь с заданным hardCap (0 = DefaultHardCap).
func NewQueue(hardCap int) *Queue {
	if hardCap <= 0 {
		hardCap = DefaultHardCap
	}
	q := &Queue{hardCap: hardCap}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// SetReadDeadline задаёт дедлайн чтения.
func (q *Queue) SetReadDeadline(t time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.readDeadline = t
	q.cond.Broadcast()
}

// Push помещает пакет в очередь (RFC 8289 §5.4).
// Если очередь заполнена до hardCap, пакет отбрасывается (tail drop).
func (q *Queue) Push(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(q.buf) >= q.hardCap {
		// RFC 8289 §5.4: Packets arriving at a full buffer are dropped.
		// Строго tail-drop: никогда не удаляем голову очереди, чтобы не портить
		// последовательность TCP-пакетов и не вызывать ложных скачков RTT.
		q.dropped++
		return
	}
	q.buf = append(q.buf, item{data: cp, enqueued: time.Now()})
	q.pushed++
	q.cond.Signal()
}

type dequeueResult struct {
	it       item
	ok       bool
	okToDrop bool
}

// dodequeue извлекает первый элемент и проверяет задержку относительно Target (RFC 8289 §5.6).
// Вызывается под q.mu.
func (q *Queue) dodequeue(now time.Time) dequeueResult {
	if len(q.buf) == 0 {
		q.firstAboveTime = time.Time{}
		return dequeueResult{ok: false}
	}

	it := q.buf[0]
	q.buf = q.buf[1:]

	sojourn := now.Sub(it.enqueued)

	// RFC 8289 §5.6: если sojourn < Target ИЛИ в очереди не осталось пакетов (>0),
	// мы не сбрасываем пакеты (backlog guard: utilization protection).
	if sojourn < Target || len(q.buf) == 0 {
		q.firstAboveTime = time.Time{}
		return dequeueResult{it: it, ok: true, okToDrop: false}
	}

	okToDrop := false
	if q.firstAboveTime.IsZero() {
		// Задержка только что превысила Target — взводим окно проверки Interval
		q.firstAboveTime = now.Add(Interval)
	} else if !now.Before(q.firstAboveTime) {
		// Задержка держится выше Target непрерывно дольше Interval — перегрузка подтверждена
		okToDrop = true
	}

	return dequeueResult{it: it, ok: true, okToDrop: okToDrop}
}

// controlLaw вычисляет время следующего сброса (RFC 8289 §5.6):
// t + Interval / sqrt(count).
func controlLaw(t time.Time, count uint32) time.Time {
	if count == 0 {
		count = 1
	}
	interval := time.Duration(float64(Interval) / math.Sqrt(float64(count)))
	return t.Add(interval)
}

// Pop извлекает пакет из очереди по алгоритму RFC 8289 CoDel (§5.5).
func (q *Queue) Pop() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		for len(q.buf) == 0 {
			q.dropping = false
			q.firstAboveTime = time.Time{}
			if q.closed {
				return nil, ErrClosed
			}
			if !q.readDeadline.IsZero() {
				d := time.Until(q.readDeadline)
				if d <= 0 {
					return nil, ErrTimeout
				}
				timer := time.AfterFunc(d, q.cond.Broadcast)
				q.cond.Wait()
				timer.Stop()
				continue
			}
			q.cond.Wait()
		}

		now := time.Now()
		r := q.dodequeue(now)
		if !r.ok {
			continue
		}

		if q.noCoDel {
			q.popped++
			return r.it.data, nil
		}

		if q.dropping {
			if !r.okToDrop {
				// Задержка упала ниже Target или очередь опустела — выходим из drop state
				q.dropping = false
			}
			// RFC 8289 §5.5: пока наступило время дропа и мы в drop state
			for q.dropping && !now.Before(q.dropNext) {
				q.dropped++
				q.count++
				r = q.dodequeue(now)
				if !r.ok {
					break
				}
				if !r.okToDrop {
					q.dropping = false
				} else {
					q.dropNext = controlLaw(q.dropNext, q.count)
				}
			}
		} else if r.okToDrop {
			// Начальный переход в drop state: сбрасываем первый пакет и планируем следующий
			q.dropped++
			r = q.dodequeue(now)
			q.dropping = true

			delta := uint32(0)
			if q.count > q.lastCount {
				delta = q.count - q.lastCount
			}
			q.count = 1
			if delta > 1 && !now.Before(q.dropNext) && now.Sub(q.dropNext) < 16*Interval {
				q.count = delta
			}
			q.dropNext = controlLaw(now, q.count)
			q.lastCount = q.count
		}

		if r.ok {
			q.popped++
			return r.it.data, nil
		}
	}
}

// SetNoCoDel toggles CoDel drop logic on/off (pure FIFO queue).
func (q *Queue) SetNoCoDel(v bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.noCoDel = v
}

// Close закрывает очередь.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// Stats возвращает текущие счетчики.
type Stats struct {
	Pushed, Popped, Dropped int64
	QueueLen                int
}

func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Pushed: q.pushed, Popped: q.popped, Dropped: q.dropped, QueueLen: len(q.buf)}
}
