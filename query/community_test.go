package query

import (
	"errors"
	"reflect"
	"testing"
)

// TestParseCommunityDispatch walks every shape ParseCommunity classifies.
// The assertion is on the COLUMNS searched, because that is what the caller
// sees in meta.community_columns and what decides whether an empty answer
// means "absent" or "looked in the wrong place".
//
// "70000:100" and "65000:70000" are here, not in
// TestParseCommunityRejectsWhatItCannotPlace: both parts are numeric and the
// value has exactly two of them, which is a route target's shape whether or
// not either half fits in 16 bits. A component above 65535 only rules out a
// STANDARD community -- it is exactly what a legal 4-byte-AS route target
// looks like ("70000:100" administered by AS 70000, a real 4-byte AS), and
// refusing it would make 4-byte-AS route targets unsearchable for precisely
// the networks most likely to use them.
func TestParseCommunityDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		cols     []string
	}{
		{"standard and rt share the 2-part integer form", "65000:100",
			[]string{"live_communities", "live_route_targets"}},
		{"an IPv4 administrator is a route target, never a standard community",
			"10.255.0.3:900",
			[]string{"live_ext_communities", "live_route_targets"}},
		{"3 numeric parts are ambiguous between large and unnamed extended",
			"65101:1:100",
			[]string{"live_ext_communities", "live_large_communities"}},
		{"4 numeric parts are an unnamed extended community", "128:0:0:256",
			[]string{"live_ext_communities"}},
		{"a named rt also searches the stripped projection", "rt:65101:1",
			[]string{"live_ext_communities", "live_route_targets"}},
		{"a named non-rt searches only extended", "soo:65000:777",
			[]string{"live_ext_communities"}},
		{"an administrator above 65535 is still a route target, not a refusal",
			"70000:100", []string{"live_route_targets"}},
		{"a value above 65535 is still a route target, not a refusal",
			"65000:70000", []string{"live_route_targets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseCommunity(tc.in)
			if err != nil {
				t.Fatalf("ParseCommunity(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(m.SearchedColumns(), tc.cols) {
				t.Errorf("columns = %v, want %v", m.SearchedColumns(), tc.cols)
			}
		})
	}
}

// TestParseCommunityRejectsWhatItCannotPlace: an unrecognized value must be a
// 400 and not an empty result, which would be indistinguishable from a real
// absence.
func TestParseCommunityRejectsWhatItCannotPlace(t *testing.T) {
	for _, in := range []string{"", "65000", "not-a-community", "65000:", ":100"} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseCommunity(in); !errors.Is(err, ErrBadFilter) {
				t.Errorf("ParseCommunity(%q) error = %v, want ErrBadFilter", in, err)
			}
		})
	}
}

// TestParseCommunityPacksAStandardCommunity: communities is Array(UInt32) and
// the caller types "high:low", so the two halves are packed. Getting the shift
// wrong yields a value that matches nothing, and an empty answer is the one
// failure this package refuses to return silently.
func TestParseCommunityPacksAStandardCommunity(t *testing.T) {
	m, err := ParseCommunity("65100:100")
	if err != nil {
		t.Fatal(err)
	}
	const want = uint32(65100)<<16 | 100 // 4266393700, seen in the archive
	var found bool
	for _, a := range m.args {
		if v, ok := a.(uint32); ok && v == want {
			found = true
		}
	}
	if !found {
		t.Errorf("args %v carry no packed value %d for 65100:100", m.args, want)
	}
}
