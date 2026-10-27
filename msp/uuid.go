package msp

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// NewCommandID returns a fresh UUIDv7 (RFC 9562), the id MSP requires for
// every command. IDs from one process are strictly increasing, even within
// one millisecond, so the server's command log sorts in issue order.
func NewCommandID() string {
	var b [16]byte
	ms, seq := tick()
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = 0x70 | byte(seq>>8&0x0f) // version 7, 12-bit counter (rand_a)
	b[7] = byte(seq)
	_, _ = rand.Read(b[8:])
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

var clock struct {
	sync.Mutex
	ms  int64
	seq uint16
}

// tick returns the millisecond and a per-millisecond counter; when the
// counter overflows it borrows the next millisecond.
func tick() (int64, uint16) {
	clock.Lock()
	defer clock.Unlock()
	now := time.Now().UnixMilli()
	switch {
	case now > clock.ms:
		clock.ms, clock.seq = now, 0
	case clock.seq < 0x0fff:
		clock.seq++
	default:
		clock.ms++
		clock.seq = 0
	}
	return clock.ms, clock.seq
}
