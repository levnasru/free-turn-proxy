// Package reseq implements an adaptive sliding-window packet resequencing
// engine for multi-path UDP encapsulation. It eliminates TCP duplicate ACKs,
// false fast-retransmits, and congestion window collapse caused by arrival
// jitter across heterogeneous parallel TURN relay streams.
package reseq

import (
	"encoding/binary"
)

const (
	// Magic byte identifying encapsulated resequencing frames.
	// WireGuard packets always start with 0x01..0x04, so 0xD5 is unambiguous.
	Magic = 0xD5

	// HeaderLen is the fixed size of the MP-UDP resequencing shim header in bytes.
	HeaderLen = 8

	// FlagData indicates standard payload data.
	FlagData = 0x00
)

// IsReseq returns true if the packet starts with the resequencing magic byte
// and meets the minimum header length requirement.
func IsReseq(pkt []byte) bool {
	return len(pkt) >= HeaderLen && pkt[0] == Magic
}

// Wrap prepends an 8-byte resequencing header to payload using dst buffer.
// Bytes 2..3 store the session epoch (uint16 BE).
func Wrap(dst []byte, payload []byte, seq uint32, epoch uint16) []byte {
	needed := HeaderLen + len(payload)
	if cap(dst) >= needed {
		dst = dst[:needed]
	} else {
		dst = make([]byte, needed)
	}

	dst[0] = Magic
	dst[1] = FlagData
	binary.BigEndian.PutUint16(dst[2:4], epoch)
	binary.BigEndian.PutUint32(dst[4:8], seq)
	copy(dst[HeaderLen:], payload)
	return dst
}

// Unwrap extracts the sequence number, session epoch, and underlying payload.
// Returns ok=false if the packet does not contain a valid resequencing header.
func Unwrap(pkt []byte) (seq uint32, epoch uint16, payload []byte, ok bool) {
	if len(pkt) < HeaderLen || pkt[0] != Magic {
		return 0, 0, nil, false
	}
	epoch = binary.BigEndian.Uint16(pkt[2:4])
	seq = binary.BigEndian.Uint32(pkt[4:8])
	payload = pkt[HeaderLen:]
	return seq, epoch, payload, true
}
