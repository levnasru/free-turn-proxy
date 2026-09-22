// SPDX-License-Identifier: MIT

// Package rtpvideo реализует wire-профиль обфускации под WebRTC VP8 Video:
// RTP PT=96 (VP8), 90 кГц timestamp clock, One-Byte Header Extensions (RFC 8285)
// с TWCC (Transport-Wide Congestion Control), abs-send-time и sdes:mid='1' (видео-поток
// в SDP BUNDLE), а также 1-байтный дескриптор полезной нагрузки VP8 (RFC 7741).
//
// Профиль переводит соединение на медиа-серверах и реле VK Calls из узкого
// голосового тира (Voice Tier, ~120-150 pps, ~64-128 kbps) в высокоскоростной
// видео-тир (Video Tier, 2.5-25 Mbps), снимая искусственные лимиты скорости.
//
// Wire-формат (HeaderLen=44, Overhead=60):
//
//	[12B RTP hdr | 16B one-byte ext | 4B VP8 desc+pad | 12B explicit nonce | AEAD(payload‖2B len-marker) | 16B tag]
//
// RTP header (RFC 3550):
//
//	byte 0:    0x90        V=2, P=0, X=1, CC=0
//	byte 1:    M<<7 | 0x60 M=1 на ключевом кадре, PT=96 (VP8 video)
//	byte 2-3:  seq16 BE    монотонный sequence number
//	byte 4-7:  ts32 BE     90 кГц video clock (шаг 3000 ticks при 30 fps)
//	byte 8-11: SSRC        полностью random per conn
//
// RTP extension (RFC 8285 one-byte, 12 байт данных -> 3 слова):
//
//	byte 12-13: 0xBE 0xDE      профиль one-byte
//	byte 14-15: 0x0003         длина = 3 слова (12 байт данных)
//	byte 16:    0x21           transport-wide-cc: id=2, len=1 (2 байта tccSeq)
//	byte 17-18: tccSeq16       монотонный transport-cc sequence
//	byte 19:    0x32           abs-send-time: id=3, len=2 (3 байта timestamp)
//	byte 20-22: abs_send_time  24-bit NTP timestamp (mod 64s)
//	byte 23:    0x40           sdes:mid: id=4, len=0 (1 байт mid)
//	byte 24:    '1'            mid-тег видео (VK Calls SDP BUNDLE использует "1" для m=video)
//	byte 25-27: 0x00           padding до 12 байт данных расширения
//
// VP8 descriptor (RFC 7741 Section 4.2):
//
//	byte 28:    0x10           X=0, R=0, N=0, S=1, R=0, PID=0 (начало раздела VP8)
//	byte 29-31: 0x00           padding до 4 байт
//
// 12B explicit nonce = 4B sessionID || 8B counter (BE). MSB sessionID
// кодирует направление. AAD = первые 44 байта (RTP hdr || ext || VP8 || nonce).
package rtpvideo

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
	rtpExtLen = 16 // 4B 0xBEDE container + 12B data (3 words)
	vp8HdrLen = 4  // 1B VP8 descriptor (0x10) + 3B padding
	nonceLen  = 12
	tagLen    = 16

	headerLen = rtpHdrLen + rtpExtLen + vp8HdrLen + nonceLen // 44
	overhead  = headerLen + tagLen                           // 60

	rtpVerExt   = 0x90 // V=2, P=0, X=1, CC=0
	rtpPTVideo  = 0x60 // PT=96 (VP8 video)
	rtpPTOpus   = 0x6F // PT=111 (Opus audio)
	rtpMarker   = 0x80 // M=1

	// Legacy Opus constants для обратной совместимости со старыми клиентами
	opusHeaderLen       = 44
	opusOverhead        = 60
	opusLegacyHeaderLen = 40
	opusLegacyOverhead  = 56
	opusPadTarget       = 130

	extAudioLevelHdr  = 0x10 // id=1, len=1
	extTransportHdr   = 0x21 // id=2, len=1 (2 байта данных)
	extAbsSendTimeHdr = 0x32 // id=3, len=2 (3 байта данных)
	extMidHdr         = 0x40 // id=4, len=0 (1 байт данных)

	midVideoValue = '1' // video stream mid в VK Calls BUNDLE
	midAudioValue = '0' // audio stream mid в VK Calls BUNDLE

	vp8DescKeyframe = 0x10 // X=0, R=0, N=0, S=1, R=0, PID=0

	markerLen = 2

	// Video clock 90 kHz: при 30 fps шаг составляет 3000 тиков на кадр.
	tsStepVideo = 3000
)

func MaxWire(payloadLen int) int { return overhead + payloadLen + markerLen }

// State хранит общий ключ и AEAD; разделяется соединениями.
type State struct {
	aead cipher.AEAD
}

func NewState(key []byte) (*State, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("rtpvideo: key must be %d bytes (got %d)", KeyLen, len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("rtpvideo: aead init: %w", err)
	}
	return &State{aead: aead}, nil
}

// Conn управляет состоянием одного RTP-потока.
type Conn struct {
	state     *State
	sessionID [4]byte
	ssrc      [4]byte
	startTime time.Time

	mu        sync.Mutex
	isServer  bool
	isOpus    bool // true если входящий клиент говорит на rtpopus3
	isLegacy  bool // true если входящий клиент говорит на rtpopus3 v1
	counter   uint64
	seq       uint16
	timestamp uint32
	tcc       uint16

	framePackets int
}

func NewConn(key []byte, isServer bool) (*Conn, error) {
	s, err := NewState(key)
	if err != nil {
		return nil, err
	}
	return NewConnFromState(s, isServer)
}

func NewConnFromState(s *State, isServer bool) (*Conn, error) {
	var sessionID [4]byte
	if _, err := rand.Read(sessionID[:]); err != nil {
		return nil, fmt.Errorf("rtpvideo: rand sessionID: %w", err)
	}
	if isServer {
		sessionID[0] |= 0x80
	} else {
		sessionID[0] &= 0x7f
	}

	var ssrc [4]byte
	if _, err := rand.Read(ssrc[:]); err != nil {
		return nil, fmt.Errorf("rtpvideo: rand ssrc: %w", err)
	}

	var startSeq [2]byte
	_, _ = rand.Read(startSeq[:])
	var startTS [4]byte
	_, _ = rand.Read(startTS[:])
	var startTCC [2]byte
	_, _ = rand.Read(startTCC[:])
	var cb [8]byte
	if _, err := rand.Read(cb[:]); err != nil {
		return nil, fmt.Errorf("rtpvideo: rand counter: %w", err)
	}

	return &Conn{
		state:     s,
		sessionID: sessionID,
		ssrc:      ssrc,
		startTime: time.Now(),
		isServer:  isServer,
		seq:       binary.BigEndian.Uint16(startSeq[:]),
		timestamp: binary.BigEndian.Uint32(startTS[:]),
		tcc:       binary.BigEndian.Uint16(startTCC[:]),
		counter:   binary.BigEndian.Uint64(cb[:]),
	}, nil
}

func (c *Conn) HeaderLen() int {
	c.mu.Lock()
	legacy := c.isLegacy
	c.mu.Unlock()
	if legacy {
		return opusLegacyHeaderLen
	}
	return headerLen
}

func (c *Conn) Overhead() int {
	c.mu.Lock()
	legacy := c.isLegacy
	c.mu.Unlock()
	if legacy {
		return opusLegacyOverhead
	}
	return overhead
}

func (c *Conn) MaxWire(payloadLen int) int {
	c.mu.Lock()
	isOpus := c.isOpus
	legacy := c.isLegacy
	c.mu.Unlock()
	if isOpus {
		if legacy {
			return opusLegacyOverhead + payloadLen
		}
		return opusOverhead + max(payloadLen, opusPadTarget) + markerLen
	}
	return overhead + payloadLen + markerLen
}

func (c *Conn) absSendTime() uint32 {
	ms := max(time.Since(c.startTime).Milliseconds(), 0)
	sec := (ms / 1000) % 64
	frac := (ms % 1000) << 18 / 1000
	return uint32(sec)<<18 | uint32(frac)
}

func (c *Conn) WrapInto(dst, payload []byte) (int, error) {
	if len(dst) < c.MaxWire(len(payload)) {
		return 0, errors.New("rtpvideo: dst buffer too small")
	}
	hLen := c.HeaderLen()
	copy(dst[hLen:], payload)
	return c.WrapInPlace(dst, len(payload))
}

// WrapInPlace кодирует plaintext на месте. Если клиент определился как
// rtpopus3, отвечает в формате rtpopus3 для полной совместимости.
func (c *Conn) WrapInPlace(buf []byte, plainLen int) (int, error) {
	c.mu.Lock()
	isOpus := c.isOpus
	legacy := c.isLegacy
	seq := c.seq
	c.seq++
	ts := c.timestamp
	tcc := c.tcc
	c.tcc++
	ctr := c.counter
	c.counter++

	var marker bool
	if !isOpus {
		c.framePackets++
		if c.framePackets >= 8 { // эмуляция границ кадра видео (пачки по 8 пакетов)
			c.framePackets = 0
			c.timestamp += tsStepVideo
			marker = true
		}
	} else {
		c.timestamp += 960 // 20ms audio step
	}
	c.mu.Unlock()

	// Если определили Opus-клиент, используем Opus-форматирование
	if isOpus {
		return c.wrapOpus(buf, plainLen, legacy, marker, seq, ts, tcc, ctr)
	}

	// Стандартный rtpvideo формат (VP8 Video PT=96)
	sealedLen := plainLen + markerLen
	wireLen := overhead + sealedLen
	if len(buf) < wireLen {
		return 0, errors.New("rtpvideo: dst buffer too small")
	}

	buf[0] = rtpVerExt
	pt := byte(rtpPTVideo)
	if marker {
		pt |= rtpMarker
	}
	buf[1] = pt
	binary.BigEndian.PutUint16(buf[2:4], seq)
	binary.BigEndian.PutUint32(buf[4:8], ts)
	copy(buf[8:12], c.ssrc[:])

	// RTP extension: 0xBEDE + 3 words
	buf[12] = 0xBE
	buf[13] = 0xDE
	binary.BigEndian.PutUint16(buf[14:16], 3)
	buf[16] = extTransportHdr
	binary.BigEndian.PutUint16(buf[17:19], tcc)
	buf[19] = extAbsSendTimeHdr
	ast := c.absSendTime()
	buf[20], buf[21], buf[22] = byte(ast>>16), byte(ast>>8), byte(ast)
	buf[23] = extMidHdr
	buf[24] = midVideoValue
	buf[25], buf[26], buf[27] = 0, 0, 0 // padding

	// VP8 descriptor: 0x10 + 3B pad
	buf[28] = vp8DescKeyframe
	buf[29], buf[30], buf[31] = 0, 0, 0

	// 12B nonce
	copy(buf[32:36], c.sessionID[:])
	binary.BigEndian.PutUint64(buf[36:headerLen], ctr)

	// Маркер длины в конце полезной нагрузки
	binary.BigEndian.PutUint16(buf[headerLen+plainLen:headerLen+sealedLen], uint16(plainLen))

	nonce := buf[32:headerLen]
	aad := buf[:headerLen]
	c.state.aead.Seal(buf[headerLen:headerLen], nonce, buf[headerLen:headerLen+sealedLen], aad)
	return wireLen, nil
}

func (c *Conn) wrapOpus(buf []byte, plainLen int, legacy, marker bool, seq uint16, ts uint32, tcc uint16, ctr uint64) (int, error) {
	if legacy {
		wireLen := opusLegacyOverhead + plainLen
		if len(buf) < wireLen {
			return 0, errors.New("rtpvideo: dst buffer too small for legacy opus")
		}
		buf[0] = rtpVerExt
		pt := byte(rtpPTOpus)
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
		buf[17] = 0x80 | 30
		buf[18] = extTransportHdr
		binary.BigEndian.PutUint16(buf[19:21], tcc)
		buf[21] = extAbsSendTimeHdr
		ast := c.absSendTime()
		buf[22], buf[23], buf[24] = byte(ast>>16), byte(ast>>8), byte(ast)
		buf[25], buf[26], buf[27] = 0, 0, 0

		copy(buf[28:32], c.sessionID[:])
		binary.BigEndian.PutUint64(buf[32:opusLegacyHeaderLen], ctr)

		nonce := buf[28:opusLegacyHeaderLen]
		aad := buf[:opusLegacyHeaderLen]
		c.state.aead.Seal(buf[opusLegacyHeaderLen:opusLegacyHeaderLen], nonce, buf[opusLegacyHeaderLen:opusLegacyHeaderLen+plainLen], aad)
		return wireLen, nil
	}

	effLen := max(plainLen, opusPadTarget)
	sealedLen := effLen + markerLen
	wireLen := opusOverhead + sealedLen
	if len(buf) < wireLen {
		return 0, errors.New("rtpvideo: dst buffer too small for opus")
	}

	buf[0] = rtpVerExt
	pt := byte(rtpPTOpus)
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
	buf[17] = 0x80 | 30
	buf[18] = extTransportHdr
	binary.BigEndian.PutUint16(buf[19:21], tcc)
	buf[21] = extAbsSendTimeHdr
	ast := c.absSendTime()
	buf[22], buf[23], buf[24] = byte(ast>>16), byte(ast>>8), byte(ast)
	buf[25] = extMidHdr
	buf[26] = midAudioValue
	buf[27], buf[28], buf[29], buf[30], buf[31] = 0, 0, 0, 0, 0

	copy(buf[32:36], c.sessionID[:])
	binary.BigEndian.PutUint64(buf[36:opusHeaderLen], ctr)

	for i := plainLen; i < effLen; i++ {
		buf[opusHeaderLen+i] = 0
	}
	binary.BigEndian.PutUint16(buf[opusHeaderLen+effLen:opusHeaderLen+sealedLen], uint16(plainLen))

	nonce := buf[32:opusHeaderLen]
	aad := buf[:opusHeaderLen]
	c.state.aead.Seal(buf[opusHeaderLen:opusHeaderLen], nonce, buf[opusHeaderLen:opusHeaderLen+sealedLen], aad)
	return wireLen, nil
}

func (c *Conn) Unwrap(wire, dst []byte) (int, error) {
	plain, err := c.UnwrapInPlace(wire)
	if err != nil {
		return 0, err
	}
	if len(plain) > len(dst) {
		return 0, errors.New("rtpvideo: dst buffer too small")
	}
	copy(dst[:len(plain)], plain)
	return len(plain), nil
}

// UnwrapInPlace автоматически распознаёт профиль (VP8 video PT=96 или Opus PT=111)
// и расшифровывает полезную нагрузку. При обнаружении Opus переключает соединение
// в симметричный Opus-режим, гарантируя 100% совместимость со старыми клиентами.
func (c *Conn) UnwrapInPlace(wire []byte) ([]byte, error) {
	if len(wire) < opusLegacyOverhead {
		return nil, errors.New("rtpvideo: packet too short")
	}

	pt := wire[1] & 0x7F
	if pt == rtpPTOpus {
		return c.unwrapOpus(wire)
	}

	// VP8 Video (PT=96)
	if len(wire) < overhead+markerLen {
		return nil, errors.New("rtpvideo: packet too short for video")
	}
	nonce := wire[32:headerLen]
	aad := wire[:headerLen]
	ct := wire[headerLen:]

	sealed, err := c.state.aead.Open(ct[:0], nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("rtpvideo: AEAD open: %w", err)
	}
	if len(sealed) < markerLen {
		return nil, errors.New("rtpvideo: length marker exceeds sealed payload")
	}
	realLen := int(binary.BigEndian.Uint16(sealed[len(sealed)-markerLen:]))
	if realLen > len(sealed)-markerLen {
		return nil, errors.New("rtpvideo: length marker exceeds sealed payload")
	}
	c.mu.Lock()
	c.isOpus = false
	c.mu.Unlock()
	return sealed[:realLen], nil
}

func (c *Conn) unwrapOpus(wire []byte) ([]byte, error) {
	if len(wire) >= 16 && wire[12] == 0xBE && wire[13] == 0xDE {
		extWords := binary.BigEndian.Uint16(wire[14:16])
		if extWords == 3 {
			// Legacy v1 Opus клиент
			nonce := wire[28:opusLegacyHeaderLen]
			aad := wire[:opusLegacyHeaderLen]
			ct := wire[opusLegacyHeaderLen:]

			plain, err := c.state.aead.Open(ct[:0], nonce, ct, aad)
			if err != nil {
				return nil, fmt.Errorf("rtpvideo: opus v1 AEAD open: %w", err)
			}
			c.mu.Lock()
			c.isOpus = true
			c.isLegacy = true
			c.mu.Unlock()
			return plain, nil
		}
	}

	// Modern v2 Opus клиент
	if len(wire) < opusOverhead+opusPadTarget+markerLen {
		return nil, errors.New("rtpvideo: packet too short for opus v2")
	}
	nonce := wire[32:opusHeaderLen]
	aad := wire[:opusHeaderLen]
	ct := wire[opusHeaderLen:]

	sealed, err := c.state.aead.Open(ct[:0], nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("rtpvideo: opus v2 AEAD open: %w", err)
	}
	if len(sealed) < markerLen {
		return nil, errors.New("rtpvideo: opus payload shorter than length marker")
	}
	realLen := int(binary.BigEndian.Uint16(sealed[len(sealed)-markerLen:]))
	if realLen > len(sealed)-markerLen {
		return nil, errors.New("rtpvideo: opus length marker exceeds payload")
	}
	c.mu.Lock()
	c.isOpus = true
	c.isLegacy = false
	c.mu.Unlock()
	return sealed[:realLen], nil
}

func GenKeyHex() (string, error) {
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("rtpvideo: key gen: %w", err)
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
