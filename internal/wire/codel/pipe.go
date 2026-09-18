package codel

import (
	"net"
	"time"
)

// fakeAddr - как у cbeuw/connutil: этот pipe локальный, in-memory, реальный
// адрес ему не нужен, только чтобы удовлетворить интерфейс net.Addr.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "codelpipe" }
func (fakeAddr) String() string  { return "codelpipe" }

// End - один конец пары, реализующий net.PacketConn. Write кладёт пакет в
// writeQ; Read/ReadFrom забирает из readQ (с CoDel-управлением на стороне
// Pop - см. codel.go).
type End struct {
	writeQ *Queue
	readQ  *Queue
}

// NewPipe создаёт пару связанных net.PacketConn. outboundHardCap ограничивает
// память "исходящей" очереди (a.Write -> b.Read) - именно она под CoDel
// (см. package doc: сюда пишет DTLS-слой, отсюда paced-отправка в TURN
// читает, здесь и живёт риск bufferbloat). inboundHardCap - для обратного
// направления (b.Write -> a.Read, ответы из TURN обратно в DTLS) - там
// естественный источник (сеть) сам ограничивает скорость прихода, отдельного
// CoDel-давления там исторически не было и не требовалось; отдельный hard
// cap всё равно защищает от утечки памяти, если чтение вообще остановится.
func NewPipe(outboundHardCap, inboundHardCap int) (a, b *End) {
	outQ := NewQueue(outboundHardCap)
	inQ := NewQueue(inboundHardCap)
	inQ.SetNoCoDel(true)
	a = &End{writeQ: outQ, readQ: inQ}
	b = &End{writeQ: inQ, readQ: outQ}
	return a, b
}

func (e *End) ReadFrom(p []byte) (int, net.Addr, error) {
	data, err := e.readQ.Pop()
	if err != nil {
		return 0, nil, err
	}
	n := copy(p, data)
	return n, fakeAddr{}, nil
}

func (e *End) Read(p []byte) (int, error) {
	n, _, err := e.ReadFrom(p)
	return n, err
}

func (e *End) WriteTo(p []byte, _ net.Addr) (int, error) {
	e.writeQ.Push(p)
	return len(p), nil
}

func (e *End) Write(p []byte) (int, error) {
	return e.WriteTo(p, nil)
}

// SetNoCoDel toggles CoDel drop logic on the read queue of this End.
func (e *End) SetNoCoDel(v bool) {
	e.readQ.SetNoCoDel(v)
}

// Close закрывает ОБЕ очереди пары - как у cbeuw/connutil.PacketPipe.Close,
// закрытие любого конца останавливает оба.
func (e *End) Close() error {
	e.readQ.Close()
	e.writeQ.Close()
	return nil
}

func (e *End) LocalAddr() net.Addr  { return fakeAddr{} }
func (e *End) RemoteAddr() net.Addr { return fakeAddr{} }

func (e *End) SetReadDeadline(t time.Time) error {
	e.readQ.SetReadDeadline(t)
	return nil
}

// SetWriteDeadline - нет смысла: Push (Write/WriteTo) никогда не блокируется,
// только hardCap может уронить самый старый элемент, что не связано с
// дедлайнами. Оставлено no-op ради соответствия net.PacketConn.
func (e *End) SetWriteDeadline(time.Time) error { return nil }

func (e *End) SetDeadline(t time.Time) error {
	return e.SetReadDeadline(t)
}

// WriteQueueStats возвращает живые счётчики очереди, в которую пишет Write/
// WriteTo ЭТОГО конца (не того, откуда он читает) - удобно для диагностики.
// На конце, обёрнутом DTLS-слоем (conn1 в oneDTLS), это и есть та самая
// CoDel-управляемая очередь между DTLS-записью и paced-отправкой в TURN.
func (e *End) WriteQueueStats() Stats { return e.writeQ.Stats() }
