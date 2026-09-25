package bmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func frame(typ uint8, payload []byte) []byte {
	return append(AppendHeader(nil, typ, len(payload)), payload...)
}

func TestReadMsg(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	buf.Write(frame(TypeInitiation, []byte{0xAA, 0xBB}))
	buf.Write(frame(TypeTermination, nil))

	m, err := ReadMsg(buf)
	if err != nil || m.Type != TypeInitiation || !bytes.Equal(m.Payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("got %+v err=%v", m, err)
	}
	m, err = ReadMsg(buf)
	if err != nil || m.Type != TypeTermination || len(m.Payload) != 0 {
		t.Fatalf("got %+v err=%v", m, err)
	}
	if _, err = ReadMsg(buf); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadMsgBadVersion(t *testing.T) {
	b := frame(TypeInitiation, nil)
	b[0] = 2
	if _, err := ReadMsg(bytes.NewReader(b)); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}

func TestReadMsgBadLength(t *testing.T) {
	// declared length smaller than header
	b := []byte{3, 0, 0, 0, 5, 0}
	if _, err := ReadMsg(bytes.NewReader(b)); !errors.Is(err, ErrBadLength) {
		t.Fatalf("want ErrBadLength, got %v", err)
	}
}

func TestReadMsgTruncated(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "zero_payload_bytes",
			// Header declares 4-byte payload, truncated to exactly HeaderLen.
			// io.ReadFull returns (0, io.EOF), unexpected() must convert to ErrUnexpectedEOF.
			data: AppendHeader(nil, TypePeerUp, 4)[:HeaderLen],
		},
		{
			name: "partial_payload_bytes",
			// Header declares 4-byte payload, but only 2 bytes provided.
			// io.ReadFull returns io.ErrUnexpectedEOF directly, unexpected() passes through.
			data: append(AppendHeader(nil, TypePeerUp, 4), 0xAA, 0xBB),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadMsg(bytes.NewReader(tt.data))
			if err == nil {
				t.Fatal("want error on truncated payload")
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("must not be io.EOF, got %v", err)
			}
		})
	}
}

func TestReadMsgOversizedLength(t *testing.T) {
	// declared length beyond MaxMsgLen must be rejected before any
	// payload allocation is attempted.
	b := AppendHeader(nil, TypeInitiation, 0)
	binary.BigEndian.PutUint32(b[1:5], MaxMsgLen+1)
	if _, err := ReadMsg(bytes.NewReader(b)); !errors.Is(err, ErrBadLength) {
		t.Fatalf("want ErrBadLength, got %v", err)
	}
	// No BMP message approaches this: a BGP message is at most 65535 bytes
	// (RFC 8654), and every BMP message type is that plus headers and TLVs.
	binary.BigEndian.PutUint32(b[1:5], 2<<20)
	if _, err := ReadMsg(bytes.NewReader(b)); !errors.Is(err, ErrBadLength) {
		t.Fatalf("a 2 MiB length: want ErrBadLength, got %v", err)
	}
}

// TestReadMsgAllocatesForBytesReceivedNotBytesClaimed is the header-only
// flood: a peer that sends a 6-byte header claiming the maximum length and
// then nothing. Allocating the claimed length up front let 128 such
// connections pin 2 GiB of heap without sending a single payload byte. Memory
// has to track what actually arrived.
//
// TotalAlloc is cumulative and unaffected by garbage collection, so the
// measurement is deterministic up to the runtime's own small allocations,
// which the bound leaves ample room for.
func TestReadMsgAllocatesForBytesReceivedNotBytesClaimed(t *testing.T) {
	hdr := AppendHeader(nil, TypeRouteMonitoring, MaxMsgLen-HeaderLen)
	const runs = 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		if _, err := ReadMsg(bytes.NewReader(hdr)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("want io.ErrUnexpectedEOF for a header with no body, got %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	perCall := (after.TotalAlloc - before.TotalAlloc) / runs
	if perCall > 2*readChunk {
		t.Fatalf("ReadMsg allocated %d bytes per call for a header claiming %d bytes "+
			"and delivering none; want at most %d", perCall, MaxMsgLen, 2*readChunk)
	}
}

// TestReadMsgLargeBody reads a body several chunks long, delivered one byte
// per Read, so the incremental read path is exercised across every chunk
// boundary rather than satisfied by one large read.
func TestReadMsgLargeBody(t *testing.T) {
	payload := make([]byte, 3*readChunk+17)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	b := append(frame(TypeRouteMirroring, payload), frame(TypeTermination, nil)...)
	r := &oneByteReader{b: b}
	m, err := ReadMsg(r)
	if err != nil || m.Type != TypeRouteMirroring || !bytes.Equal(m.Payload, payload) {
		t.Fatalf("large body: type=%d len=%d err=%v", m.Type, len(m.Payload), err)
	}
	if m, err := ReadMsg(r); err != nil || m.Type != TypeTermination {
		t.Fatalf("message after a large body: %+v err=%v", m, err)
	}
	// A truncated large body is still io.ErrUnexpectedEOF, not io.EOF.
	trunc := frame(TypeRouteMirroring, payload)[:HeaderLen+2*readChunk+1]
	if _, err := ReadMsg(bytes.NewReader(trunc)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated large body: want io.ErrUnexpectedEOF, got %v", err)
	}
}

type oneByteReader struct{ b []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.b[0]
	r.b = r.b[1:]
	return 1, nil
}

// TestMaxMsgLenFitsTheCorpus holds MaxMsgLen to real senders: every committed
// capture must still parse message by message under it. The corpus is BMP
// from four vendors' routers, so a limit that rejected any of it would be
// rejecting production traffic.
func TestMaxMsgLenFitsTheCorpus(t *testing.T) {
	root := filepath.Join("..", "bgp", "testdata", "corpus")
	files, largest := 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bmpcap") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		r := bytes.NewReader(raw)
		for n := 1; ; n++ {
			m, err := ReadMsg(r)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s: message %d: %v", path, n, err)
				break
			}
			largest = max(largest, HeaderLen+len(m.Payload))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("no .bmpcap files under %s", root)
	}
	t.Logf("%d captures; largest message %d bytes (MaxMsgLen %d)", files, largest, MaxMsgLen)
}

func FuzzReadMsg(f *testing.F) {
	f.Add(frame(TypeRouteMonitoring, []byte{1, 2, 3}))
	f.Fuzz(func(t *testing.T, data []byte) {
		ReadMsg(bytes.NewReader(data)) // must not panic
	})
}
