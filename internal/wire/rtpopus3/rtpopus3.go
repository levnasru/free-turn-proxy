// SPDX-License-Identifier: MIT

// Package rtpopus3 - wire-профиль обфускации с улучшенной RTP-мимикрией:
// четыре one-byte extension (audio-level, transport-wide-cc, abs-send-time,
// sdes:mid), вариативный шаг timestamp, эмуляция потери пакетов (gaps в
// seq), VAD-модель с переключением silence/speech, padding payload под
// размер реального RED-аудио (RFC 2198 и dred живут внутри SRTP-payload,
// снаружи виден только итоговый размер - см. WrapInPlace).
//
// Wire-формат (HeaderLen=44, Overhead=60, MaxWire(n)=Overhead+max(n,padTarget)+2):
//
//	[12B RTP hdr | 20B one-byte ext | 12B explicit nonce | AEAD(payload‖padding‖2B len-marker) | 16B tag]
//
// RTP header (RFC 3550):
//
//	byte 0:    0x90        V=2, P=0, X=1, CC=0
//	byte 1:    M<<7 | 0x6F M=1 на старте talkspurt, PT=111 (opus)
//	byte 2-3:  seq16 BE    монотонный с пропусками (loss simulation)
//	byte 4-7:  ts32 BE     вариативный шаг 480/960/1920 (10/20/40ms)
//	byte 8-11: SSRC        полностью random per conn
//
// RTP extension (RFC 8285 one-byte, 16 байт данных -> 4 слова):
//
//	byte 12-13: 0xBE 0xDE      профиль one-byte
//	byte 14-15: 0x0004         длина = 4 слова (16 байт данных)
//	byte 16:    0x10           ssrc-audio-level: id=1, len=1
//	byte 17:    0x80|level     VAD + level (-dBov)
//	byte 18:    0x21           transport-wide-cc: id=2, len=2
//	byte 19-20: tccSeq16       монотонный transport-cc sequence
//	byte 21:    0x32           abs-send-time: id=3, len=2
//	byte 22-24: abs_send_time  24-bit NTP timestamp (mod 64s)
//	byte 25:    0x40           sdes:mid: id=4, len=1
//	byte 26:    '0'            mid-тег (совпадает с реальным VK BUNDLE, см. отчёт)
//	byte 27-31: 0x00           padding до 16 байт данных расширения
//
// 12B explicit nonce = 4B sessionID || 8B counter (BE). MSB sessionID
// кодирует направление. AAD = первые 44 байта (RTP hdr || ext || nonce).
package rtpopus3

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	KeyLen    = 32
	rtpHdrLen = 12
	rtpExtLen = 20
	nonceLen  = 12
	tagLen    = 16
	headerLen = rtpHdrLen + rtpExtLen + nonceLen // 44
	overhead  = headerLen + tagLen               // 60
	rtpVerExt = 0x90                             // V=2, P=0, X=1, CC=0
	rtpPT     = 0x6F                             // M=0, PT=111 (opus)
	rtpMarker = 0x80                             // M=1

	// Legacy v1 constants (клиенты до 01.09.2026: rtpExtLen=16, 3 words, без sdes:mid, без padding/маркера)
	legacyExtLen    = 16
	legacyHeaderLen = rtpHdrLen + legacyExtLen + nonceLen // 40
	legacyOverhead  = legacyHeaderLen + tagLen            // 56

	extAudioLevelHdr  = 0x10 // id=1, len=1
	extTransportHdr   = 0x21 // id=2, len=2
	extAbsSendTimeHdr = 0x32 // id=3, len=2
	extMidHdr         = 0x40 // id=4, len=1 (sdes:mid, RFC 8285)
	midValue          = '0'  // mid-тег; реальный VK BUNDLE использует "0" для первого m-line

	// markerLen - trailing 2-байтный маркер реальной длины payload внутри
	// зашифрованного блока (см. WrapInPlace). RED/dred в реальном WebRTC
	// живут ВНУТРИ SRTP-payload - снаружи не видна их структура, только
	// итоговый размер пакета. Раз наш AEAD тоже шифрует payload одним
	// блоком, воспроизводить RFC 2198 framing бессмысленно: наблюдателю
	// снаружи оно всё равно недоступно. Вместо этого - padTarget ниже.
	markerLen = 2
	// padTarget - ponytail: калибровочная ручка, не измерено живым тестом
	// против классификатора VK. Из живого капчура реального звонка
	// (docs/bandwidth-ceiling-investigation-2026-09-01.md §9.3): аудио с
	// RED даёт ~167Б/пакет на проводе; за вычетом настоящего RTP+SRTP
	// overhead (~32-36Б) - Opus+RED payload ≈130Б. Апгрейд: перекалибровать
	// после живого A/B на тестовом портал-акке, как делали для -obf-timing.
	padTarget = 130

	speechMinPkts  = 30
	speechMaxPkts  = 200
	silenceMinPkts = 5
	silenceMaxPkts = 30

	gapIntervalMin = 50
	gapIntervalMax = 150
	gapSizeMin     = 1
	gapSizeMax     = 3

	tsStep20ms = 960
	tsStep10ms = 480
	tsStep40ms = 1920
)

func MaxWire(payloadLen int) int { return overhead + max(payloadLen, padTarget) + markerLen }

type audioState int

const (
	stateSilence audioState = iota
	stateSpeech
)

// State хранит AEAD-экземпляр из общего ключа; разделяется многими Conn.
type State struct {
	aead cipher.AEAD
}

func NewState(key []byte) (*State, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("rtpopus3:key must be %d bytes (got %d)", KeyLen, len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("rtpopus3:aead init: %w", err)
	}
	return &State{aead: aead}, nil
}

// Conn несёт per-stream RTP-состояние. WrapInPlace/WrapInto могут зваться
// конкурентно (net.PacketConn-контракт), поэтому send-поля под mu;
// Unwrap* только читают AEAD.
type Conn struct {
	state     *State
	sessionID [4]byte   // префикс nonce; MSB кодирует направление
	ssrc      [4]byte   // SSRC для RTP header; полностью random
	startTime time.Time // база для abs-send-time; immutable после init

	mu        sync.Mutex
	isLegacy  bool
	counter   uint64
	seq       uint16
	timestamp uint32
	tcc       uint16

	audioState      audioState
	pktsInState     int
	nextStateSwitch int
	nextGapAt       int
	gapSize         int
}

func NewConn(key []byte, isServer bool) (*Conn, error) {
	s, err := NewState(key)
	if err != nil {
		return nil, err
	}
	return NewConnFromState(s, isServer)
}

func NewConnFromState(state *State, isServer bool) (*Conn, error) {
	if state == nil {
		return nil, errors.New("rtpopus3:nil state")
	}
	c := &Conn{
		state:           state,
		startTime:       time.Now(),
		audioState:      stateSpeech,
		nextStateSwitch: speechMinPkts + randRange(speechMaxPkts-speechMinPkts+1),
		nextGapAt:       gapIntervalMin + randRange(gapIntervalMax-gapIntervalMin+1),
		gapSize:         gapSizeMin + randRange(gapSizeMax-gapSizeMin+1),
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("rtpopus3:rand init: %w", err)
	}
	copy(c.sessionID[:], rnd[0:4])
	copy(c.ssrc[:], rnd[4:8])
	if isServer {
		c.sessionID[0] |= 0x80
	} else {
		c.sessionID[0] &^= 0x80
	}
	c.seq = binary.BigEndian.Uint16(rnd[8:10])
	c.timestamp = binary.BigEndian.Uint32(rnd[10:14])
	c.tcc = binary.BigEndian.Uint16(rnd[14:16])

	var cb [8]byte
	if _, err := rand.Read(cb[:]); err != nil {
		return nil, fmt.Errorf("rtpopus3:counter rand: %w", err)
	}
	c.counter = binary.BigEndian.Uint64(cb[:])
	return c, nil
}

func (c *Conn) HeaderLen() int {
	c.mu.Lock()
	legacy := c.isLegacy
	c.mu.Unlock()
	if legacy {
		return legacyHeaderLen
	}
	return headerLen
}

func (c *Conn) Overhead() int {
	c.mu.Lock()
	legacy := c.isLegacy
	c.mu.Unlock()
	if legacy {
		return legacyOverhead
	}
	return overhead
}

func (c *Conn) MaxWire(n int) int {
	c.mu.Lock()
	legacy := c.isLegacy
	c.mu.Unlock()
	if legacy {
		return legacyOverhead + n
	}
	return overhead + max(n, padTarget) + markerLen
}

func (c *Conn) IsLegacy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isLegacy
}

func (c *Conn) SetLegacy(legacy bool) {
	c.mu.Lock()
	c.isLegacy = legacy
	c.mu.Unlock()
}

func randRange(n int) int {
	if n <= 0 {
		return 0
	}
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("rtpopus3:rand: " + err.Error())
	}
	return int(b[0]) % n
}

func pickTsStep() uint32 {
	r := randRange(256)
	switch {
	case r < 10:
		return tsStep10ms
	case r < 230:
		return tsStep20ms
	default:
		return tsStep40ms
	}
}

// updateAudioState возвращает true на переходе silence->speech (RTP marker).
func (c *Conn) updateAudioState() bool {
	c.pktsInState++
	if c.pktsInState < c.nextStateSwitch {
		return false
	}
	c.pktsInState = 0
	if c.audioState == stateSilence {
		c.audioState = stateSpeech
		c.nextStateSwitch = speechMinPkts + randRange(speechMaxPkts-speechMinPkts+1)
		return true
	}
	c.audioState = stateSilence
	c.nextStateSwitch = silenceMinPkts + randRange(silenceMaxPkts-silenceMinPkts+1)
	return false
}

// audioLevel: speech несёт V-бит и низкий -dBov, silence наоборот.
func (c *Conn) audioLevel() byte {
	if c.audioState == stateSpeech {
		return 0x80 | byte(20+randRange(31)) //nolint:gosec // level 20..50, fits byte
	}
	return byte(100 + randRange(28)) //nolint:gosec // level 100..127, fits byte
}

// computeSeq возвращает текущий seq, периодически пропуская gapSize (имитация потерь).
func (c *Conn) computeSeq() uint16 {
	seq := c.seq
	c.seq++
	c.nextGapAt--
	if c.nextGapAt > 0 {
		return seq
	}
	c.seq += uint16(c.gapSize) //nolint:gosec // gapSize 1..3
	c.nextGapAt = gapIntervalMin + randRange(gapIntervalMax-gapIntervalMin+1)
	c.gapSize = gapSizeMin + randRange(gapSizeMax-gapSizeMin+1)
	return seq
}

func (c *Conn) absSendTime() uint32 {
	ms := max(time.Since(c.startTime).Milliseconds(), 0)
	sec := (ms / 1000) % 64
	frac := (ms % 1000) << 18 / 1000
	return uint32(sec)<<18 | uint32(frac) //nolint:gosec // sec<64, frac<2^18: укладывается в 24 бита
}

func (c *Conn) WrapInto(dst, payload []byte) (int, error) {
	if len(dst) < c.MaxWire(len(payload)) {
		return 0, errors.New("rtpopus3:dst buffer too small")
	}
	hLen := c.HeaderLen()
	copy(dst[hLen:], payload)
	return c.WrapInPlace(dst, len(payload))
}

// WrapInPlace кодирует plaintext из buf[HeaderLen:HeaderLen+plainLen] на месте.
// Для legacy-клиентов (isLegacy=true) использует v1 framing (rtpExtLen=16, headerLen=40,
// без padding/маркера). Для современных v2-клиентов дополняет до padTarget байт
// и trailing markerLen-байтным маркером реальной длины.
// Send-поля берутся под mu; запись в buf и Seal - без блокировки.
func (c *Conn) WrapInPlace(buf []byte, plainLen int) (int, error) {
	c.mu.Lock()
	legacy := c.isLegacy
	marker := c.updateAudioState()
	level := c.audioLevel()
	seq := c.computeSeq()
	ts := c.timestamp
	c.timestamp += pickTsStep()
	tcc := c.tcc
	c.tcc++
	ctr := c.counter
	c.counter++
	c.mu.Unlock()

	if legacy {
		wireLen := legacyOverhead + plainLen
		if len(buf) < wireLen {
			return 0, errors.New("rtpopus3:dst buffer too small")
		}

		buf[0] = rtpVerExt
		pt := byte(rtpPT)
		if marker {
			pt |= rtpMarker
		}
		buf[1] = pt
		binary.BigEndian.PutUint16(buf[2:4], seq)
		binary.BigEndian.PutUint32(buf[4:8], ts)
		copy(buf[8:12], c.ssrc[:])

		buf[12] = 0xBE
		buf[13] = 0xDE
		binary.BigEndian.PutUint16(buf[14:16], 3)
		buf[16] = extAudioLevelHdr
		buf[17] = level
		buf[18] = extTransportHdr
		binary.BigEndian.PutUint16(buf[19:21], tcc)
		buf[21] = extAbsSendTimeHdr
		ast := c.absSendTime()
		buf[22], buf[23], buf[24] = byte(ast>>16), byte(ast>>8), byte(ast)
		buf[25], buf[26], buf[27] = 0, 0, 0

		copy(buf[28:32], c.sessionID[:])
		binary.BigEndian.PutUint64(buf[32:legacyHeaderLen], ctr)

		nonce := buf[28:legacyHeaderLen]
		aad := buf[:legacyHeaderLen]
		c.state.aead.Seal(buf[legacyHeaderLen:legacyHeaderLen], nonce, buf[legacyHeaderLen:legacyHeaderLen+plainLen], aad)
		return wireLen, nil
	}

	effLen := max(plainLen, padTarget)
	sealedLen := effLen + markerLen
	wireLen := overhead + sealedLen
	if len(buf) < wireLen {
		return 0, errors.New("rtpopus3:dst buffer too small")
	}

	buf[0] = rtpVerExt
	pt := byte(rtpPT)
	if marker {
		pt |= rtpMarker
	}
	buf[1] = pt
	binary.BigEndian.PutUint16(buf[2:4], seq)
	binary.BigEndian.PutUint32(buf[4:8], ts)
	copy(buf[8:12], c.ssrc[:])

	buf[12] = 0xBE
	buf[13] = 0xDE
	binary.BigEndian.PutUint16(buf[14:16], 4)
	buf[16] = extAudioLevelHdr
	buf[17] = level
	buf[18] = extTransportHdr
	binary.BigEndian.PutUint16(buf[19:21], tcc)
	buf[21] = extAbsSendTimeHdr
	ast := c.absSendTime()
	buf[22], buf[23], buf[24] = byte(ast>>16), byte(ast>>8), byte(ast) //nolint:gosec // 24-bit abs-send-time
	buf[25] = extMidHdr
	buf[26] = midValue
	buf[27], buf[28], buf[29], buf[30], buf[31] = 0, 0, 0, 0, 0

	copy(buf[32:36], c.sessionID[:])
	binary.BigEndian.PutUint64(buf[36:headerLen], ctr)

	// Payload из buf[headerLen:headerLen+plainLen] уже на месте (контракт
	// caller'а); дописываем padding до effLen и маркер реальной длины сразу
	// за ним - оба внутри AEAD-блока, снаружи неотличимы от Opus-данных.
	for i := plainLen; i < effLen; i++ {
		buf[headerLen+i] = 0
	}
	binary.BigEndian.PutUint16(buf[headerLen+effLen:headerLen+sealedLen], uint16(plainLen)) //nolint:gosec // plainLen <= maxPayload(1600) << 65536

	nonce := buf[32:headerLen]
	aad := buf[:headerLen]
	c.state.aead.Seal(buf[headerLen:headerLen], nonce, buf[headerLen:headerLen+sealedLen], aad)
	return wireLen, nil
}

func (c *Conn) Unwrap(wire, dst []byte) (int, error) {
	plain, err := c.UnwrapInPlace(wire)
	if err != nil {
		return 0, err
	}
	if len(plain) > len(dst) {
		return 0, errors.New("rtpopus3:dst buffer too small")
	}
	copy(dst[:len(plain)], plain)
	return len(plain), nil
}

// UnwrapInPlace декодирует wire на месте и снимает padding/маркер (см.
// WrapInPlace), возвращая subslice РЕАЛЬНОГО plaintext внутри wire.
// Автоматически детектирует legacy v1 пакеты (RFC 8285 ext words == 3)
// и переключает соединение в legacy-режим для симметричных ответов.
func (c *Conn) UnwrapInPlace(wire []byte) ([]byte, error) {
	if len(wire) < legacyOverhead {
		return nil, errors.New("rtpopus3:packet too short")
	}

	// Проверяем RFC 8285 extension header (offset 12)
	if len(wire) >= 16 && wire[12] == 0xBE && wire[13] == 0xDE {
		extWords := binary.BigEndian.Uint16(wire[14:16])
		if extWords == 3 {
			// Legacy v1 клиент (rtpExtLen=16, headerLen=40, overhead=56, без padding/маркера)
			nonce := wire[28:legacyHeaderLen]
			aad := wire[:legacyHeaderLen]
			ct := wire[legacyHeaderLen:]

			plain, err := c.state.aead.Open(ct[:0], nonce, ct, aad)
			if err != nil {
				return nil, fmt.Errorf("rtpopus3:AEAD open: %w", err)
			}
			c.mu.Lock()
			c.isLegacy = true
			c.mu.Unlock()
			return plain, nil
		}
	}

	// Modern v2 клиент (rtpExtLen=20, headerLen=44, overhead=60, padTarget=130, marker=2)
	if len(wire) < overhead+padTarget+markerLen {
		return nil, errors.New("rtpopus3:packet too short")
	}
	nonce := wire[32:headerLen]
	aad := wire[:headerLen]
	ct := wire[headerLen:]

	sealed, err := c.state.aead.Open(ct[:0], nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("rtpopus3:AEAD open: %w", err)
	}
	realLen := int(binary.BigEndian.Uint16(sealed[len(sealed)-markerLen:]))
	if realLen > len(sealed)-markerLen {
		return nil, errors.New("rtpopus3:length marker exceeds sealed payload")
	}
	c.mu.Lock()
	c.isLegacy = false
	c.mu.Unlock()
	return sealed[:realLen], nil
}

func GenKeyHex() (string, error) {
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("rtpopus3:key gen: %w", err)
	}
	return hex.EncodeToString(key), nil
}

func DecodeKey(enabled bool, raw string) ([]byte, error) {
	if !enabled {
		return nil, nil
	}
	if raw == "" {
		return nil, errors.New("-obf-profile != none requires -obf-key")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("-obf-key invalid hex: %w", err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("-obf-key must decode to %d bytes (got %d)", KeyLen, len(key))
	}
	return key, nil
}
