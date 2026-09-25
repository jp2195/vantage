package bmp

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParseInit, TestParseInitNonUTF8, and TestParseTLVsTruncated cover
// baseline TLV parsing.

func TestParseInit(t *testing.T) {
	p := AppendTLV(nil, 1, []byte("Cisco IOS XR Software, Version 7.9.2"))
	p = AppendTLV(p, 2, []byte("rr1"))
	p = AppendTLV(p, 0, []byte("free-form"))
	info, err := ParseInit(p)
	if err != nil || info.SysName != "rr1" || info.SysDescr != "Cisco IOS XR Software, Version 7.9.2" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestParseInitNonUTF8(t *testing.T) {
	info, err := ParseInit(AppendTLV(nil, 2, []byte{0xff, 'r', 'r'}))
	if err != nil || info.SysName != "�rr" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestParseTLVsTruncated(t *testing.T) {
	if _, err := ParseTLVs([]byte{0, 1, 0, 5, 'x'}); err == nil {
		t.Fatal("want error")
	}
}

// --- error identity and edge cases, following the conventions in
// msg_test.go and peerheader_test.go ---

func TestParseTLVsEmpty(t *testing.T) {
	out, err := ParseTLVs(nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	out, err = ParseTLVs([]byte{})
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestParseTLVsHeaderTruncated(t *testing.T) {
	// 3 bytes: not even a full 4-byte type+length header.
	_, err := ParseTLVs([]byte{0, 1, 0})
	if !errors.Is(err, ErrTLVTruncated) {
		t.Fatalf("want ErrTLVTruncated, got %v", err)
	}
}

func TestParseTLVsValueTruncated(t *testing.T) {
	// Declares a 5-byte value but only 1 byte follows, asserted here for
	// sentinel-error identity.
	_, err := ParseTLVs([]byte{0, 1, 0, 5, 'x'})
	if !errors.Is(err, ErrTLVTruncated) {
		t.Fatalf("want ErrTLVTruncated, got %v", err)
	}
}

func TestParseTLVsZeroLength(t *testing.T) {
	// A zero-length TLV must parse (not loop forever) and yield an empty value.
	p := AppendTLV(nil, 7, nil)
	p = AppendTLV(p, 8, []byte("x"))
	out, err := ParseTLVs(p)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []TLV{{Type: 7, Value: []byte{}}, {Type: 8, Value: []byte("x")}}
	if len(out) != len(want) || out[0].Type != want[0].Type || len(out[0].Value) != 0 ||
		out[1].Type != want[1].Type || !bytes.Equal(out[1].Value, want[1].Value) {
		t.Fatalf("out=%+v", out)
	}
}

func TestParseTLVsDeclaredLengthExceedsBuffer(t *testing.T) {
	// Header declares a 65535-byte value but the buffer holds none of it.
	// Must error, not allocate 65535 bytes or read out of range.
	b := []byte{0, 1, 0xff, 0xff}
	_, err := ParseTLVs(b)
	if !errors.Is(err, ErrTLVTruncated) {
		t.Fatalf("want ErrTLVTruncated, got %v", err)
	}
}

func TestParseTLVsTrailingGarbage(t *testing.T) {
	// One well-formed TLV followed by 2 stray bytes: not enough for another
	// header, so parsing must fail rather than silently drop the remainder.
	p := AppendTLV(nil, 1, []byte("ok"))
	p = append(p, 0xAA, 0xBB)
	_, err := ParseTLVs(p)
	if !errors.Is(err, ErrTLVTruncated) {
		t.Fatalf("want ErrTLVTruncated, got %v", err)
	}
}

func TestParseTLVsRoundTrip(t *testing.T) {
	p := AppendTLV(nil, 1, []byte("descr"))
	p = AppendTLV(p, 2, []byte("name"))
	out, err := ParseTLVs(p)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []TLV{
		{Type: 1, Value: []byte("descr")},
		{Type: 2, Value: []byte("name")},
	}
	if len(out) != len(want) {
		t.Fatalf("out=%+v", out)
	}
	for i := range want {
		if out[i].Type != want[i].Type || !bytes.Equal(out[i].Value, want[i].Value) {
			t.Fatalf("out[%d]=%+v want=%+v", i, out[i], want[i])
		}
	}
}

func TestParseInitEmptyPayload(t *testing.T) {
	info, err := ParseInit(nil)
	if err != nil || !reflect.DeepEqual(info, InitInfo{}) {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestParseInitTruncated(t *testing.T) {
	if _, err := ParseInit([]byte{0, 1, 0, 5, 'x'}); !errors.Is(err, ErrTLVTruncated) {
		t.Fatalf("want ErrTLVTruncated, got %v", err)
	}
}

// sysName and sysDescr are copied into every envelope a session publishes,
// so a router sending 64 KiB of each would multiply every event it sends by
// that much. Both are capped, and a cap that splits a multi-byte rune must
// back off to the rune boundary rather than leave half a character behind.
func TestParseInitCapsBannerLengths(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 65535)
	info, err := ParseInit(AppendTLV(AppendTLV(nil, 1, big), 2, big))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.SysName) != maxSysName || len(info.SysDescr) != maxSysDescr {
		t.Fatalf("sysName %d bytes, sysDescr %d bytes; want %d and %d",
			len(info.SysName), len(info.SysDescr), maxSysName, maxSysDescr)
	}
	if maxSysName != 255 || maxSysDescr != 1024 {
		t.Fatalf("caps are %d/%d, want 255/1024", maxSysName, maxSysDescr)
	}

	// "é" is two bytes. maxSysName-1 of "A" then "éé" puts the cap in the
	// middle of the first "é".
	name := append(bytes.Repeat([]byte("A"), maxSysName-1), "éé"...)
	// The invalid byte becomes a three-byte U+FFFD, which is what then
	// straddles the sysDescr cap.
	descr := append(bytes.Repeat([]byte("B"), maxSysDescr-1), 0xff, 'x')
	info, err = ParseInit(AppendTLV(AppendTLV(nil, 1, descr), 2, name))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(info.SysName) || info.SysName != strings.Repeat("A", maxSysName-1) {
		t.Fatalf("sysName = %d bytes, valid=%v; want %d bytes of A",
			len(info.SysName), utf8.ValidString(info.SysName), maxSysName-1)
	}
	if !utf8.ValidString(info.SysDescr) || info.SysDescr != strings.Repeat("B", maxSysDescr-1) {
		t.Fatalf("sysDescr = %d bytes, valid=%v; want %d bytes of B",
			len(info.SysDescr), utf8.ValidString(info.SysDescr), maxSysDescr-1)
	}

	// Under the caps nothing changes.
	info, _ = ParseInit(AppendTLV(nil, 2, []byte("r\u00e9")))
	if info.SysName != "r\u00e9" {
		t.Fatalf("short sysName changed: %q", info.SysName)
	}
}

func TestAppendTLVOversized(t *testing.T) {
	// AppendTLV must panic when len(v) > math.MaxUint16, as the uint16 length
	// field cannot encode it. Verify panic happens for an oversized value.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic, but AppendTLV returned normally")
		}
	}()
	AppendTLV(nil, 1, make([]byte, 65536))
}

func TestAppendTLVBoundary(t *testing.T) {
	// Verify the boundary: exactly 65535 bytes (max uint16) must work and
	// round-trip, without panic. This guards against off-by-one errors.
	maxVal := make([]byte, 65535)
	for i := range maxVal {
		maxVal[i] = byte(i % 256)
	}
	p := AppendTLV(nil, 42, maxVal)

	tlvs, err := ParseTLVs(p)
	if err != nil {
		t.Fatalf("ParseTLVs failed: %v", err)
	}
	if len(tlvs) != 1 {
		t.Fatalf("want 1 TLV, got %d", len(tlvs))
	}
	if tlvs[0].Type != 42 {
		t.Fatalf("want Type=42, got %d", tlvs[0].Type)
	}
	if len(tlvs[0].Value) != 65535 {
		t.Fatalf("want Value length=65535, got %d", len(tlvs[0].Value))
	}
	if !bytes.Equal(tlvs[0].Value, maxVal) {
		t.Fatalf("Value mismatch")
	}
}

// FuzzParseTLVs mirrors FuzzReadMsg and FuzzParsePeerHeader: ParseTLVs reads
// attacker-controlled wire bytes (the type-length-value loop is the classic
// place for a malformed length to cause an out-of-range slice or an
// infinite/unbounded loop) and must never panic, regardless of what error it
// returns.
func FuzzParseTLVs(f *testing.F) {
	f.Add(AppendTLV(nil, 1, []byte("Cisco IOS XR Software, Version 7.9.2")))
	f.Add(AppendTLV(AppendTLV(nil, 1, []byte("d")), 2, []byte("n")))
	f.Add([]byte{0, 1, 0, 5, 'x'})  // declared length exceeds buffer
	f.Add([]byte{0, 1, 0})          // header truncated
	f.Add([]byte{})                 // empty
	f.Add([]byte{0, 1, 0xff, 0xff}) // declared length far exceeds buffer
	f.Fuzz(func(t *testing.T, data []byte) {
		ParseTLVs(data) // must not panic
	})
}
