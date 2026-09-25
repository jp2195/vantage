package main

import (
	"net/netip"
	"testing"
)

// TestLoadgenPrefixesAreDistinct is the test this command exists to have.
//
// Deriving a prefix from an index is easy to get wrong in a way that silently
// collapses distinct indices onto one prefix, and because repeats collapse
// under ReplacingMergeTree, a scale test then understates its own row count
// while reporting success. Two plausible mappings both do it:
//
//   - Masking an id to 24 bits and OR'ing it into an 11.0.0.0 base overflows
//     the first octet: 447,488 distinct prefixes out of 3,200,000.
//   - Writing the index into the FOURTH octet of a /24 -- which a /24 masks
//     off entirely -- gives 16 distinct prefixes out of 4,000.
//
// Neither is visible by reading the code, only by counting with uniqExact
// after a run. That is the argument for pinning it here instead.
func TestLoadgenPrefixesAreDistinct(t *testing.T) {
	// Enough to cross every boundary the mapping has: the third octet at 256,
	// the second at 65,536, and the prefix-length block at blockSize.
	const n = 3 * loadgenBlockSize / 2
	seen := make(map[netip.Prefix]int, n)
	for i := 0; i < n; i += 7 {
		p := loadgenPrefixAt(i)
		if prev, dup := seen[p]; dup {
			t.Fatalf("index %d and %d both map to %s", prev, i, p)
		}
		seen[p] = i
	}
	if len(seen) < n/7 {
		t.Fatalf("only %d distinct prefixes from %d indices", len(seen), n/7)
	}
}

// A prefix whose address has bits set below its own length is not canonical:
// netip renders it as given, but the BGP encoder writes only the leading
// ceil(len/8) bytes and the decoder zero-fills the rest, so what comes back
// out of the archive is a DIFFERENT prefix from the one that went in. A
// generator whose output does not survive its own round trip cannot be used to
// verify a row count.
func TestLoadgenPrefixesAreCanonical(t *testing.T) {
	for _, i := range []int{0, 1, 255, 256, 65535, 65536, loadgenBlockSize, 2*loadgenBlockSize + 9} {
		p := loadgenPrefixAt(i)
		if p.Masked() != p {
			t.Errorf("index %d maps to %s, which is not masked to its own "+
				"length (%s): the encoder drops those bits and the decoder "+
				"zero-fills them, so this prefix would not survive a round trip",
				i, p, p.Masked())
		}
	}
}

// The first octet must stay clear of 127/8, which the generator's own sessions
// dial from, and of 224+ (multicast) and 0/8. A route whose prefix collides
// with a loopback source address makes a test's own traffic ambiguous.
func TestLoadgenPrefixesAvoidReservedSpace(t *testing.T) {
	for _, i := range []int{0, loadgenBlockSize - 1, loadgenBlockSize, 4*loadgenBlockSize - 1} {
		first := loadgenPrefixAt(i).Addr().As4()[0]
		if first < 11 || first > 110 {
			t.Errorf("index %d maps to %s, whose first octet %d is outside "+
				"11..110", i, loadgenPrefixAt(i), first)
		}
	}
}
