package query

import (
	"fmt"
	"strconv"
)

// protocolNames is the IANA "BGP-LS Protocol-IDs" registry as RFC 9552
// section 5.1 Table 2 defines it. The names are lowercase kebab tokens
// rather than the registry's prose ("IS-IS Level 1") because they are
// filter values as well as output: ?protocol=isis-l2 has to be typeable
// without quoting, and a name with a space in it is not.
//
// The archive carries 1, 2, 3 and 6. The other three are here because the
// registry defines them, not because anything has produced one -- a table
// that covered only what has been seen would silently render a Direct or
// Static node's protocol as unknown the first time one arrived.
//
// It stops at 7. IANA has assigned values above that since RFC 9552, and
// deliberately guessing at them is the failure ProtocolName's own test
// forbids: an ID this table does not know renders "", which is unambiguous,
// rather than a name that might be wrong.
var protocolNames = map[uint8]string{
	1: "isis-l1",
	2: "isis-l2",
	3: "ospfv2",
	4: "direct",
	5: "static",
	6: "ospfv3",
	7: "bgp",
}

// ProtocolName renders a BGP-LS Protocol-ID as its registry name, or "" for
// an ID outside the table. "" is a positive answer -- "this project does not
// know a name for that ID" -- and the numeric protocol travels beside it on
// the wire, so nothing is lost when it is empty.
func ProtocolName(id uint8) string { return protocolNames[id] }

// ParseProtocol accepts either notation a caller might type: a decimal ID or
// a registry name. Both resolve to the same uint8, which is what makes
// ?protocol=2 and ?protocol=isis-l2 the same question.
//
// Base 10 only, and no surrounding space. The looser readings are all
// silent: strconv.ParseUint with base 0 would make "0x2" mean 2 here and a
// parse error over HTTP, and a trimmed " isis-l2" would accept from one
// transport what another rejects.
//
// 0 is refused rather than treated as absent, for the reason params.asn
// refuses AS 0: 0 is how this package's filters spell "not asked", so an
// explicit 0 reaching one would return every protocol instead of none.
func ParseProtocol(s string) (uint8, error) {
	if id, ok := nameToProtocol(s); ok {
		return id, nil
	}
	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return 0, fmt.Errorf("%w: protocol %q is neither a BGP-LS Protocol-ID "+
			"(a decimal 1-255) nor a name this project knows (%s)",
			ErrBadFilter, s, KnownProtocolNames())
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: protocol 0 is not assigned, and 0 is how this "+
			"filter spells \"not asked\" -- an explicit 0 would return every "+
			"protocol rather than none", ErrBadFilter)
	}
	return uint8(n), nil
}

// nameToProtocol is the reverse of protocolNames, built by scanning rather
// than kept as a second map: seven entries make the scan free, and a second
// map is a second place for the table to be wrong.
func nameToProtocol(s string) (uint8, bool) {
	for id, name := range protocolNames {
		if name == s {
			return id, true
		}
	}
	return 0, false
}

// KnownProtocolNames renders the accepted registry names, in ID order so the
// list is stable across runs -- ranging a map for user-visible text would
// reorder it on every call.
//
// Exported because it is part of the HTTP surface as well as this package's
// own: api/handlers.go's protocol() builds its 400 body from this call
// rather than a second, hand-copied list, so a name added to protocolNames
// reaches the error message a caller actually sees without a second edit --
// and, before that call existed, the registry names were unreachable from
// any 400 body at all, because api/openapi.yaml's ?protocol= parameter
// documents one example and no enumeration.
func KnownProtocolNames() string {
	out := ""
	for id := uint8(1); id <= 7; id++ {
		if name := protocolNames[id]; name != "" {
			if out != "" {
				out += ", "
			}
			out += name
		}
	}
	return out
}
