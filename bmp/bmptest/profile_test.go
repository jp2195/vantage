package bmptest

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bmp"
)

// TestProfilesAreDerivedFromRealCaptures is the guard that keeps the synthetic
// generator honest.
//
// vantage already has one instance of vendor strings written from
// documentation instead of measured off the wire: quirk.anchors looks for
// "NX-OS", "IOS[ -]?XR", "JUNOS" and friends, and matches NONE of the three
// implementations this project can actually run -- XRd sends a bare "26.1.1"
// with no vendor token at all, NX-OS sends a chassis line, FRR sends
// "FRRouting 10.3_git". Every vendor anchor in the product is therefore dead.
//
// A synthetic BMP generator is the obvious place to make that mistake a second
// time, because nothing stops someone typing a plausible-looking sysDescr into
// a profile table. So every profile's Initiation string must appear verbatim
// in a committed capture from that implementation. A profile with no capture
// behind it fails here rather than quietly becoming the thing the decoder is
// tested against.
//
// This is what makes the generator a safe substitute for live capture: it
// can produce any VOLUME needed, but it cannot invent an IDENTITY never
// observed.
func TestProfilesAreDerivedFromRealCaptures(t *testing.T) {
	corpus := filepath.Join("..", "..", "bgp", "testdata", "corpus")
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("corpus not present: %v", err)
	}

	// sysDescr strings actually observed, per vendor directory.
	observed := map[string]map[string]bool{}
	err := filepath.WalkDir(corpus, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".bmpcap") {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		vendorDir := filepath.Base(filepath.Dir(p))
		r := bytes.NewReader(raw)
		for {
			m, err := bmp.ReadMsg(r)
			if err != nil {
				return nil
			}
			if m.Type != bmp.TypeInitiation {
				continue
			}
			for _, tlv := range initTLVs(m.Payload) {
				if tlv.typ != 1 { // sysDescr
					continue
				}
				if observed[vendorDir] == nil {
					observed[vendorDir] = map[string]bool{}
				}
				observed[vendorDir][tlv.val] = true
			}
			return nil
		}
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}

	profiles := Profiles()
	if len(profiles) == 0 {
		t.Fatal("no vendor profiles defined")
	}
	for _, p := range profiles {
		got := observed[p.Corpus]
		if len(got) == 0 {
			t.Errorf("profile %q claims corpus dir %q, which has no capture with an Initiation sysDescr",
				p.Name, p.Corpus)
			continue
		}
		if !got[p.SysDescr] {
			t.Errorf("profile %q sysDescr %q was never sent by a real router;\n  captures under %s/ sent: %v",
				p.Name, p.SysDescr, p.Corpus, sortedStrings(got))
		}
	}
}

// initTLV is one Information TLV from an Initiation message.
type initTLV struct {
	typ int
	val string
}

// initTLVs splits an Initiation payload into its Information TLVs. Kept in the
// test rather than the package because nothing but the corpus check needs to
// read Initiation messages back -- bmptest writes them, it does not parse.
func initTLVs(payload []byte) []initTLV {
	var out []initTLV
	for len(payload) >= 4 {
		typ := int(payload[0])<<8 | int(payload[1])
		l := int(payload[2])<<8 | int(payload[3])
		if 4+l > len(payload) {
			return out
		}
		out = append(out, initTLV{typ: typ, val: string(payload[4 : 4+l])})
		payload = payload[4+l:]
	}
	return out
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
