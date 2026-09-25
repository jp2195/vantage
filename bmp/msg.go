// Package bmp implements RFC 7854 BMP wire parsing. Parsers are pure and
// stdlib-only; they never panic on wire input.
package bmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

const (
	TypeRouteMonitoring = 0
	TypeStatsReport     = 1
	TypePeerDown        = 2
	TypePeerUp          = 3
	TypeInitiation      = 4
	TypeTermination     = 5
	TypeRouteMirroring  = 6
)

const (
	Version   = 3
	HeaderLen = 6
	// MaxMsgLen bounds a single BMP message, guarding against hostile length
	// fields. The largest legitimate message is set by the BGP message it
	// carries: at most 65535 bytes even with RFC 8654 extended messages. A
	// Route Monitoring or Route Mirroring message is that plus a 6-byte
	// common header, a 42-byte per-peer header and a few TLVs; a Peer Up
	// carries two OPENs (4096 bytes each) plus information TLVs; Initiation,
	// Termination and Stats Reports are TLV lists with 2-byte lengths. 1 MiB
	// is sixteen times the largest BGP message, so it leaves room for
	// several TLVs of maximum size on any message type while still refusing
	// a length no sender produces.
	MaxMsgLen = 1 << 20
)

// readChunk is the most ReadMsg allocates for a message body ahead of the
// bytes that fill it. A header only claims a length; allocating the claim up
// front let a peer pin MaxMsgLen of heap per connection by sending 6 bytes and
// stalling. Reading in chunks that at most double what has already arrived
// keeps a body's memory proportional to the bytes actually received, while a
// real message -- nearly always under 64 KiB -- still takes one allocation.
const readChunk = 64 << 10

var (
	ErrBadVersion = errors.New("bmp: unsupported version")
	ErrBadLength  = errors.New("bmp: bad message length")
)

type Msg struct {
	Type    uint8
	Payload []byte
}

// ReadMsg reads exactly one BMP message from r.
//
// On success, returns a complete message and nil error.
//
// On error:
//   - If err is io.EOF with no bytes consumed, the peer closed cleanly at a
//     message boundary — the normal end of a session.
//   - Any other error (ErrBadVersion, ErrBadLength, io.ErrUnexpectedEOF, or a
//     transport error) leaves the reader positioned at an arbitrary offset inside
//     a partially-consumed message. The byte stream cannot be resynchronized; the
//     caller must close the connection rather than calling ReadMsg again on it.
func ReadMsg(r io.Reader) (Msg, error) {
	var hdr [HeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:1]); err != nil {
		return Msg{}, err
	}
	if hdr[0] != Version {
		return Msg{}, fmt.Errorf("%w: %d", ErrBadVersion, hdr[0])
	}
	if _, err := io.ReadFull(r, hdr[1:]); err != nil {
		return Msg{}, unexpected(err)
	}
	l := binary.BigEndian.Uint32(hdr[1:5])
	if l < HeaderLen || l > MaxMsgLen {
		return Msg{}, fmt.Errorf("%w: %d", ErrBadLength, l)
	}
	p, err := readBody(r, int(l-HeaderLen))
	if err != nil {
		return Msg{}, unexpected(err)
	}
	return Msg{Type: hdr[5], Payload: p}, nil
}

// readBody reads exactly n bytes from r, growing its buffer only as bytes
// arrive; see readChunk. Each step reads at most max(readChunk, bytes so
// far), so the buffer never runs more than readChunk ahead of the stream or
// more than double the bytes received.
func readBody(r io.Reader, n int) ([]byte, error) {
	p := make([]byte, 0, min(n, readChunk))
	for len(p) < n {
		step := min(n-len(p), max(len(p), readChunk))
		p = slices.Grow(p, step)
		k, err := io.ReadFull(r, p[len(p):len(p)+step])
		p = p[:len(p)+k]
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// AppendHeader appends a BMP common header for a message of the given type
// and payload length. Used by tests and bmpgen.
func AppendHeader(dst []byte, typ uint8, payloadLen int) []byte {
	dst = append(dst, Version)
	dst = binary.BigEndian.AppendUint32(dst, uint32(HeaderLen+payloadLen))
	return append(dst, typ)
}
