package query

import (
	"errors"
	"testing"
)

// TestProtocolNamesMatchTheRegistry pins the seven IDs RFC 9552 section 5.1
// defines. The archive carries four of them (1, 2, 3, 6); the other three
// are here because the registry defines them and a decoder that meets one
// should not have to guess.
func TestProtocolNamesMatchTheRegistry(t *testing.T) {
	for id, want := range map[uint8]string{
		1: "isis-l1",
		2: "isis-l2",
		3: "ospfv2",
		4: "direct",
		5: "static",
		6: "ospfv3",
		7: "bgp",
	} {
		if got := ProtocolName(id); got != want {
			t.Errorf("ProtocolName(%d) = %q, want %q", id, got, want)
		}
	}
}

// TestUnknownProtocolNameIsEmptyNotAGuess: an ID outside the registry has
// no name, and "" says so. Rendering "unknown-9" or falling back to the
// number would put a value on the wire that a client could mistake for a
// registry name.
func TestUnknownProtocolNameIsEmptyNotAGuess(t *testing.T) {
	for _, id := range []uint8{0, 8, 99, 255} {
		if got := ProtocolName(id); got != "" {
			t.Errorf("ProtocolName(%d) = %q, want the empty string", id, got)
		}
	}
}

// TestParseProtocolAcceptsBothNotations is the property the filter needs:
// protocol=2 and protocol=isis-l2 are the same question.
func TestParseProtocolAcceptsBothNotations(t *testing.T) {
	byNumber, err := ParseProtocol("2")
	if err != nil {
		t.Fatalf("ParseProtocol(\"2\"): %v", err)
	}
	byName, err := ParseProtocol("isis-l2")
	if err != nil {
		t.Fatalf("ParseProtocol(\"isis-l2\"): %v", err)
	}
	if byNumber != byName || byNumber != 2 {
		t.Errorf("ParseProtocol disagrees with itself: %d by number, %d by name",
			byNumber, byName)
	}
}

// TestParseProtocolRejectsWhatItCannotAnswer. 0 is refused for the reason
// AS 0 is: it is this package's "not asked" value, so an explicit 0 that
// reached a filter would return every protocol rather than none.
func TestParseProtocolRejectsWhatItCannotAnswer(t *testing.T) {
	for _, raw := range []string{"0", "256", "-1", "isis", "OSPFV2 ", "", "0x2"} {
		if _, err := ParseProtocol(raw); err == nil {
			t.Errorf("ParseProtocol(%q) was accepted", raw)
		} else if !errors.Is(err, ErrBadFilter) {
			t.Errorf("ParseProtocol(%q) returned %v, which does not wrap ErrBadFilter",
				raw, err)
		}
	}
}

// TestEveryNameParsesBackToItsOwnID is the round trip. A table with one
// entry mistyped on either side passes both tables above and fails here.
func TestEveryNameParsesBackToItsOwnID(t *testing.T) {
	for id := uint8(1); id <= 7; id++ {
		name := ProtocolName(id)
		if name == "" {
			t.Fatalf("ProtocolName(%d) is empty; the registry defines 1-7", id)
		}
		got, err := ParseProtocol(name)
		if err != nil {
			t.Fatalf("ParseProtocol(%q): %v", name, err)
		}
		if got != id {
			t.Errorf("%q round-tripped to %d, want %d", name, got, id)
		}
	}
}
