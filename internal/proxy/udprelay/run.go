// Package udprelay реализует UDP-режим прокси: терминирует DTLS от локального
// пира (WireGuard) и ретранслирует пакеты через per-stream TURN-аллокацию
// обратно к удалённому пиру. Run - точка входа; владеет локальным listener,
// fan-in входящего dispatch и per-stream DTLS/TURN циклами.
package udprelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/proxy/common"
	"github.com/samosvalishe/free-turn-proxy/internal/stats"
	"github.com/samosvalishe/free-turn-proxy/internal/transport/dtlsdial"
)

// GetCredsFunc реэкспортирован из common, чтобы вызывающие не выходили за пределы импортов пакета.
type GetCredsFunc = common.GetCredsFunc

// AuthHandler - подмножество provider.Provider, необходимое пакету.
// Определено как локальный интерфейс, чтобы тесты могли подменять fake без
// импорта реализации провайдера. Sentinel-ошибки auth-флоу проверяются через
// provider.ErrXxx.
type AuthHandler interface {
	IsAuthError(err error) bool
	HandleAuthError(streamID int) bool
	ResetErrors(streamID int)
	BackoffUntilUnix() int64
}

// Params - per-stream конфигурация TURN/wrap, общая для DTLS и TURN циклов.
type Params struct {
	Host         string
	Port         string
	TransportUDP bool
	Profile      string
	ObfKey       []byte
	ObfTiming    time.Duration
	GetCreds     GetCredsFunc
	ClientID     string
	TrafficStats *stats.Stats

	// OnAllocated, если задан, вызывается один раз сразу после успешного
	// TURN-allocate для потока streamID с адресом реального relay-сервера,
	// на который он сел. sessionManager использует это для /24-группировки
	// при обновлении состава hot-set'а (см. pickReplacementCandidate и
	// docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md).
	// nil - no-op, как и TrafficStats.
	OnAllocated func(streamID int, relayAddr *net.UDPAddr)

	// RotateCh, если задан, немедленно переключает активный слот hot-set'а
	// на следующего кандидата при получении сигнала - ручной триггер для
	// пользователя, когда деградация видна, но не ловится liveness-проверкой
	// (см. ту же спеку, "Failover"). nil - ручного переключения нет
	// (TCP+bond режим его не использует).
	RotateCh <-chan struct{}

	// GrowCh/ShrinkCh, если заданы, - ручной триггер живого ресайза
	// hot-set'а на ±1 (Шаг 3, см. sessionManager.growHotSet/shrinkHotSet).
	// Ручной ввод идёт наравне с автоскейлером (Шаг 4), не вместо него:
	// вручную можно выйти за его потолок, и тогда он сам утянет K обратно.
	// nil - ресайза нет (как и у RotateCh).
	GrowCh   <-chan struct{}
	ShrinkCh <-chan struct{}

	// AutoToggleCh, если задан, переключает автоскейлер K (Шаг 4, см.
	// autoscale.go) вкл/выкл на живую по каждому сигналу. Автоскейлер
	// включён при старте, так что первый сигнал его ВЫКЛЮЧАЕТ; выключенный
	// продолжает считать и логировать решения, но не применяет их. nil -
	// тумблера нет, автоскейлер работает.
	AutoToggleCh <-chan struct{}
}

// streamStartBarrier - максимум, который стримы 2..N ждут прогрева кэша
// credentials стримом 1 перед стартом. Защита от вечного стопора, если
// стрим 1 не поднимается.
const streamStartBarrier = 20 * time.Second

// ErrFatal возвращается из Run, когда поток встречает условие, требующее
// завершения всего приложения (см. provider.ErrFatalNoStreams). Вызывающий
// должен проверить через errors.Is и вызвать os.Exit сам - udprelay не
// вмешивается в хост-процесс.
var ErrFatal = errors.New("udprelay: fatal error")

// Deps объединяет всё, что циклы берут из хост-процесса. Атомики принадлежат
// Run и экспонированы здесь, чтобы DTLSLoop/TURNLoop могли разделять их при
// прямом вызове (Run подключает их автоматически).
type Deps struct {
	DTLSDialer       *dtlsdial.Dialer
	Auth             AuthHandler
	Log              logx.Logger
	ActiveLocalPeer  *atomic.Value
	ConnectedStreams *atomic.Int32
	// fatalCh - внутренний сигнальный канал; устанавливается Run, пишется
	// TURNLoop, читается Run для проброса фатальной ошибки наверх.
	fatalCh chan error
}

func (d *Deps) log() logx.Logger {
	if d.Log == nil {
		return logx.Nop()
	}
	return d.Log
}

// Run - точка входа UDP-режима. Биндит listenAddr, распределяет входящие
// пакеты через dispatcher в hot-set из hotSetK живых DTLS+TURN сессий
// (вместо прежних N независимых, см. docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md).
// connectedStreams принадлежит вызывающему (provider может читать через
// свой StreamsAlive-аналог) и инкрементируется/декрементируется в oneTURN,
// как и раньше. Возвращается после выхода всех потоков (т.е. при отмене
// ctx). При фатальной provider-ошибке возвращает ErrFatal - вызывающий
// делает os.Exit без вмешательства udprelay в хост-процесс.
func Run(ctx context.Context, dtlsDialer *dtlsdial.Dialer, auth AuthHandler, logger logx.Logger, connectedStreams *atomic.Int32, params *Params, peer *net.UDPAddr, listenAddr string, hotSetK int) error {
	listenConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", listenAddr)
	if err != nil {
		return fmt.Errorf("udprelay listen %s: %w", listenAddr, err)
	}
	context.AfterFunc(ctx, func() {
		if closeErr := listenConn.Close(); closeErr != nil {
			logger.Errorf("udprelay: close local connection: %s", closeErr)
		}
	})

	if hotSetK <= 0 {
		hotSetK = 1
	}

	fatalCh := make(chan error, 1)
	var activeLocalPeer atomic.Value
	deps := &Deps{
		DTLSDialer:       dtlsDialer,
		Auth:             auth,
		Log:              logger,
		ActiveLocalPeer:  &activeLocalPeer,
		ConnectedStreams: connectedStreams,
		fatalCh:          fatalCh,
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	inboundChan := make(chan *Packet, inboundQueueCap)
	wg := sync.WaitGroup{}
	wg.Go(func() {
		runListener(runCtx, listenConn, &activeLocalPeer, inboundChan)
	})
	t := time.Tick(200 * time.Millisecond)

	sm := newSessionManager(deps, params, peer, listenConn, hotSetK, t)
	params.OnAllocated = sm.onAllocated
	wg.Go(func() {
		sm.run(runCtx, inboundChan, params.RotateCh, params.GrowCh, params.ShrinkCh, params.AutoToggleCh)
	})

	// При фатальной ошибке отменяем остальные горутины и пробрасываем наверх.
	// watcherDone синхронизирует watcher-горутину с возвратом Run, обеспечивая
	// happens-after между store и load fatalErr.
	var fatalErr atomic.Pointer[error]
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case err := <-fatalCh:
			fatalErr.Store(&err)
			runCancel()
		case <-runCtx.Done():
		}
	}()

	wg.Wait()
	runCancel()
	<-watcherDone
	if p := fatalErr.Load(); p != nil {
		return *p
	}
	return nil
}
