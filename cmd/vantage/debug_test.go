package main

import (
	"strings"
	"testing"

	"github.com/jp2195/vantage/subjects"
)

// TestHumanizeSubject covers the whole token vocabulary this system's
// subjects actually produce, including every suffix subjects.PeerToken can
// append -- "-r" (peer distinguisher), "-z" (IPv6 zone) and "-d" (RFC 8671
// RIB direction), alone and in combination. The display path previously had
// no tests at all, which matters because a mis-decode here is silent: the
// operator just reads a wrong address.
func TestHumanizeSubject(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"published route (6 tokens)",
			"vantage.v1.route.ipv4u.0a000001.0a000009",
			"vantage.v1.route.ipv4u.10.0.0.1.10.0.0.9"},
		{"stored route (7 tokens, partition inserted)",
			"vantage.v1.route.13.ipv4u.0a000001.0a000009",
			"vantage.v1.route.13.ipv4u.10.0.0.1.10.0.0.9"},
		{"ipv6 peer",
			"vantage.v1.peer.0a000001.20010db8000000000000000000000001",
			"vantage.v1.peer.10.0.0.1.2001:db8::1"},
		{"deferred-family fallback token is not an address",
			"vantage.v1.route.x16388-71.0a000001.0a000009",
			"vantage.v1.route.x16388-71.10.0.0.1.10.0.0.9"},
		{"locrib peer token passes through",
			"vantage.v1.peer.0a000001.locrib",
			"vantage.v1.peer.10.0.0.1.locrib"},
		{"invalid peer token passes through",
			"vantage.v1.peer.0a000001.invalid",
			"vantage.v1.peer.10.0.0.1.invalid"},
		{"distinguisher suffix preserved verbatim",
			"vantage.v1.peer.0a000001.0a000009-r81e14877",
			"vantage.v1.peer.10.0.0.1.10.0.0.9-r81e14877"},
		{"zone suffix preserved verbatim",
			"vantage.v1.peer.0a000001.fe800000000000000000000000000001-z1a2b3c4d",
			"vantage.v1.peer.10.0.0.1.fe80::1-z1a2b3c4d"},
		{"rib-direction suffix preserved verbatim",
			"vantage.v1.route.ipv4u.0a000001.0a000009-dout",
			"vantage.v1.route.ipv4u.10.0.0.1.10.0.0.9-dout"},
		{"distinguisher and rib-direction suffixes together",
			"vantage.v1.peer.0a000001.0a000009-r81e14877-doutpost",
			"vantage.v1.peer.10.0.0.1.10.0.0.9-r81e14877-doutpost"},
		{"raw subject",
			"vantage.v1.raw.0a000001",
			"vantage.v1.raw.10.0.0.1"},
		{"wildcards untouched",
			"vantage.v1.route.>",
			"vantage.v1.route.>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := humanizeSubject(c.in); got != c.want {
				t.Errorf("humanizeSubject(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestHumanizeSubjectDoesNotPanic pins that malformed input from the wire
// cannot crash the debug tool.
func TestHumanizeSubjectDoesNotPanic(t *testing.T) {
	for _, s := range []string{"", ".", "....", "----", "\xff\xfe\xfd", "vantage.v1.route.", ">"} {
		_ = humanizeSubject(s)
	}
}

// TestHeaderShowsPastableSubject pins the fix for a real usability trap:
// the humanized subject looks exactly like a subject and the flag is
// literally -filter SUBJ, but pasting the humanized form back subscribes
// successfully and then matches nothing forever -- silence that is
// indistinguishable from "no traffic". The header must therefore also carry
// the real subject.
func TestHeaderShowsPastableSubject(t *testing.T) {
	const real = "vantage.v1.route.ipv4u.0a000001.0a000009"
	got := header(real)
	if !strings.Contains(got, "10.0.0.1") {
		t.Errorf("header should humanize for readability: %q", got)
	}
	if !strings.Contains(got, real) {
		t.Errorf("header must include the real, pastable subject: %q", got)
	}
	// When there is nothing to decode, no bracketed duplicate.
	plain := "vantage.v1.stats.locrib.locrib"
	if h := header(plain); h != plain {
		t.Errorf("header(%q) = %q, want it unchanged", plain, h)
	}
}

// TestRoutesStreamFilterPreservesScope pins that the partition-token
// adjustment inserts a wildcard at the right position and leaves the
// operator's family/router/peer scoping intact. Collapsing everything to
// "vantage.v1.route.>" would also make the loop test pass while silently
// discarding the operator's filter.
func TestRoutesStreamFilterPreservesScope(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"vantage.v1.route.ipv4u.>", "vantage.v1.route.*.ipv4u.>"},
		{"vantage.v1.route.ipv4u.0a000001.0a000009", "vantage.v1.route.*.ipv4u.0a000001.0a000009"},
		// Not a literal "route" at token 2: no adjustment needed, because a
		// wildcard there already spans the partition token.
		{"vantage.v1.>", "vantage.v1.>"},
		{"vantage.v1.peer.>", "vantage.v1.peer.>"},
	} {
		if got := routesStreamFilter(c.in); got != c.want {
			t.Errorf("routesStreamFilter(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A beat's key is hex of the collector_id with a non-hex prefix, so the
// address decoder never mistakes a four-letter collector name for an IPv4
// address.
func TestHumanizeSubjectLeavesABeatKeyAlone(t *testing.T) {
	s := subjects.Beat("dev1")
	if got := humanizeSubject(s); got != s {
		t.Fatalf("humanizeSubject(%q) = %q; a collector key is not an address", s, got)
	}
}
