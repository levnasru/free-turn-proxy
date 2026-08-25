package udprelay

import (
	"sync/atomic"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/stats"
)

// slotHealth - пассивная наблюдаемость одного члена hot-set'а: throughput
// (собственный экземпляр stats.Stats, отдельный от общего на весь процесс
// Params.TrafficStats, который использует существующий [STATS]-логгер/UI) и
// последний замер RTT до relay-пути через turndial.Stream.Ping(). Шаг 1:
// только проводка и логирование - на маршрутизацию (route()) и на размер
// hot-set'а пока не влияет.
type slotHealth struct {
	stats *stats.Stats
	rttNs atomic.Int64 // 0 до первого успешного замера
}

func newSlotHealth() *slotHealth {
	return &slotHealth{stats: stats.New(true)}
}

func (h *slotHealth) recordRTT(d time.Duration) {
	h.rttNs.Store(int64(d))
}

// rtt возвращает последний удачный замер, 0 - если ни одного ещё не было.
func (h *slotHealth) rtt() time.Duration {
	return time.Duration(h.rttNs.Load())
}
