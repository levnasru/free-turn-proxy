// Package common содержит хелперы, общие для udprelay и tcpfwd
// (TURN-dial + создание obf-кодека). Два режима прокси по-разному компонуют DTLS
// и rtpopus, поэтому полная абстракция Engine/Handler намеренно не вводится -
// пакет собирает только действительно идентичный код.
package common

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/transport/turndial"
	"github.com/samosvalishe/free-turn-proxy/internal/wire"
)

// CredentialSafetyMargin - максимальный возраст TURN-сессии, прежде чем
// tcpfwd.maintainSession/udprelay.sessionManager принудительно её
// перезапустят, даже если она полностью здорова (P14, 2026-08-29). Живой
// журнал семьи (test.txt, 2 суток замера с телефона) не показал никакой
// периодической деградации - единственный просевший участок (2026-08-27,
// 10:11-14:14) выглядит как обычный шум сигнала/перемещения, не расписание.
// Число взято не из этого журнала, а из независимого пользовательского
// наблюдения "+-8 часов активного использования" (устно, та же дата) -
// половина от него как запас на дрожание stagger-расписания и то, что оценка
// "+-8ч" сама приблизительная. НЕ откалибровано измерением через эту логику
// именно (см. тот же принцип, что у minRTTResetInterval/HANDOVER_COOLDOWN_MS
// в других местах проекта) - поднимать/опускать по факту живого замера того,
// действительно ли принудительный редайл убирает деградацию к 8ч.
const CredentialSafetyMargin = 4 * time.Hour

// GetCredsFunc разрешает TURN-реквизиты для streamID. Реализуется provider'ом
// (см. internal/provider): provider держит идентификатор сессии (link/room/key)
// внутри, pipeline передаёт только streamID. rawURLs - кандидаты host:port в
// порядке предпочтения.
type GetCredsFunc func(ctx context.Context, streamID int) (user, pass string, rawURLs []string, err error)

// candidateIdx - индекс j-го по счёту кандидата для стрима streamID: стартовая
// точка сдвинута на streamID, дальше по кругу. Так стримы распределяются по
// разным relay вместо того, чтобы всем садиться на первый. streamID
// неотрицательный (1-based в udprelay и tcpfwd, 0 в тестах).
func candidateIdx(streamID, j, n int) int { return (streamID + j) % n }

// DialTURN получает реквизиты и открывает TURN-поток, пробуя кандидатов по
// очереди со сдвигом по streamID+candidateOffset (см. candidateIdx): если
// allocate не проходит (DPI-дроп/RST на relay-IP), берёт следующего по кругу.
// Возвращает первый успешный Stream. Вызывающий отвечает за закрытие потока и
// политику retry при auth-ошибке (udprelay) или перезапуска сессии (tcpfwd).
//
// candidateOffset сдвигает ТОЛЬКО стартовую точку перебора кандидатов, не
// streamID, переданный в getCreds - credential-bucketing (например
// -streams-per-cred у -provider vk) остаётся привязан к реальному streamID.
// Нужен tcpfwd.maintainSession (P14, 2026-08-29): без этого повторный
// DialTURN для того же стрима после добровольного редайла каждый раз
// пересчитывал бы тот же самый стартовый кандидат (j всегда стартует с 0),
// то есть сессия молча возвращалась бы на тот же relay вместо диверсификации.
// udprelay не нуждается в ненулевом offset - там у каждого нового кандидата
// hot-set'а и так строго возрастающий streamID (sessionManager.nextID++),
// что уже даёт разный candidateIdx без дополнительного сдвига.
func DialTURN(ctx context.Context, host, port string, udp bool, peer *net.UDPAddr, streamID, candidateOffset int, getCreds GetCredsFunc) (*turndial.Stream, error) {
	user, pass, rawURLs, err := getCreds(ctx, streamID)
	if err != nil {
		return nil, fmt.Errorf("get TURN creds: %w", err)
	}
	if len(rawURLs) == 0 {
		return nil, fmt.Errorf("no TURN candidates")
	}
	// HostOverride (-turn) принудительно задаёт host -> все кандидаты резолвятся
	// в одну цель, гонять их нет смысла; пробуем только первого.
	if host != "" {
		rawURLs = rawURLs[:1]
	}
	var errs []error
	n := len(rawURLs)
	base := streamID + candidateOffset
	for j := 0; j < n; j++ {
		rawURL := rawURLs[candidateIdx(base, j, n)]
		stream, derr := turndial.Open(ctx, turndial.Config{
			HostOverride: host,
			PortOverride: port,
			TransportUDP: udp,
		}, peer, user, pass, rawURL)
		if derr == nil {
			return stream, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", rawURL, derr))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("all TURN candidates failed: %w", errors.Join(errs...))
}

// NewClientObf возвращает клиентский wire.Codec для профиля obf или (nil, nil),
// если profile=none. Диспатч и валидация ключа - в wire.NewClientCodec.
func NewClientObf(profile string, key []byte) (wire.Codec, error) {
	return wire.NewClientCodec(profile, key)
}
