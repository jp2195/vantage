package collector

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/bmp"
)

// FuzzSession drives a whole Session with arbitrary BMP bytes: every message
// bmp.ReadMsg can frame goes through Handle, then Close. Each envelope that
// comes out must marshal, because the publisher marshals it and a failure
// there loses the event with nothing but a log line -- a parser can decode
// input without panicking and still build an envelope proto.Marshal rejects,
// which is how an invalid-UTF-8 BGP-LS node name went unnoticed. The package
// fuzzers in bgp and bmp cannot see that; only the assembled envelope can.
//
// Seeded from the committed captures, small ones only, so the fuzzer starts
// from real Peer Up, Route Monitoring and Initiation framing instead of
// having to discover it.
func FuzzSession(f *testing.F) {
	files, err := filepath.Glob("../bgp/testdata/corpus/*/*.bmpcap")
	if err != nil {
		f.Fatal(err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		if len(b) < 16<<10 {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		s := newTestSession()
		r := bytes.NewReader(raw)
		for {
			m, err := bmp.ReadMsg(r)
			if err != nil {
				break
			}
			for _, ev := range s.Handle(m) {
				if _, err := proto.Marshal(ev.Env); err != nil {
					t.Fatalf("marshal %s: %v", ev.Subject, err)
				}
			}
		}
		for _, ev := range s.Close() {
			if _, err := proto.Marshal(ev.Env); err != nil {
				t.Fatalf("marshal close %s: %v", ev.Subject, err)
			}
		}
	})
}
