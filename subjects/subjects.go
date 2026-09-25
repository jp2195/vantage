// Package subjects builds the NATS subjects: the router/peer/family token
// vocabulary every BMP collector envelope is published under, and the
// taxonomy of subjects (route, ls, peer, stats, raw) built from those
// tokens.
//
// Collectors publish partition-free. The ROUTES and LS streams insert the
// partition token (the bare number `{{partition(32, router, peer)}}`
// emits, 0-31) server-side via a NATS subject transform; nothing in this
// package ever emits one.
//
// Tokens are built from structured values, never from free text, and each
// encoding is injective by construction rather than sanitized after the
// fact. That is the central design rule here, and it is what an earlier
// text-and-escape scheme got wrong: rendering address text and escaping
// whatever was unsafe collapsed two distinct addresses onto one token.
// Injectivity is not only about subject filtering -- collector msg-ids
// embed these tokens, so two peers sharing one would make JetStream drop
// one peer's message as a duplicate of the other's.
//
// Two trust levels meet in this package's API surface:
//
//   - EncodeIP, FamilyToken, PeerToken and PeerBaseToken derive a token
//     from wire-shaped data a remote, possibly misbehaving BMP router
//     influences: a netip.Addr built from raw wire bytes, a
//     bmp.PeerHeader, and a bgp.Family. Because those are structured
//     values rather than strings, each is encoded directly -- addresses
//     as hex of their bytes, families as a registry name or a numeric
//     fallback -- so no input can produce an unsafe token in the first
//     place. None of them ever fails or panics. See validToken for what
//     "safe" means, and FuzzEncodeIP, FuzzPeerToken, FuzzFamilyToken.
//
//   - Route, Ls, Peer, Stats, and Raw sit on the other side of that
//     boundary: they take plain strings that, in this system, are always
//     the *output* of the three functions above, never fresh wire bytes.
//     Per this repo's builder convention (bgp.AppendOpen,
//     bmp.PeerHeader.Append, bmp.AppendTLV), they guard that precondition
//     and panic rather than silently build a subject that could inject an
//     extra token, redirect data to the wrong stream, or collide two
//     distinct routers/peers onto one subject.
package subjects

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
)

// Prefix is the subject root every subject this package builds is under.
const Prefix = "vantage.v1"

// EncodeIP renders a as a single NATS subject token: the address bytes as
// lowercase hex, 8 characters for IPv4 and 32 for IPv6. Never panics and
// never returns an empty or unsafe token, for any netip.Addr whatsoever
// (see FuzzEncodeIP). DecodeIP inverts it.
//
// Hex of the raw bytes is deliberate, and replaced an earlier scheme that
// rendered address *text* and escaped whatever was unsafe. Because both
// tokens this package builds from addresses are always a netip.Addr and
// never free text, encoding the bytes is injective by construction: no
// escape scheme to reason about, no injection surface, no dependence on
// how Go happens to format an address, and a fixed width. The text scheme
// mapped both '.' and ':' to '-', which silently collapsed the distinct
// addresses ::ffff:1.2.3.4 and ::ffff:1:2:3:4 onto one token; injectivity
// matters beyond subject filtering, because collector msg-ids embed these
// tokens and JetStream would drop one peer's message as a duplicate of
// another's.
//
// Three cases need care, all covered by tests:
//
//   - An IPv4-mapped address is unmapped first, so ::ffff:10.0.0.1 and
//     10.0.0.1 share a token. They denote the same address, so this is a
//     deliberate merge. bmp.ParsePeerHeader keeps Is4In6 distinct in
//     PeerHeader.Addr and the envelope still carries that exact value --
//     only the routing key is normalized.
//   - The invalid zero Addr returns "invalid" rather than a run of zeros,
//     which would otherwise collide with "::".
//   - A zone is not part of the address bytes, so a "-z{8-hex FNV-1a}"
//     suffix is appended when one is present, keeping fe80::1%eth0 and
//     fe80::1%eth1 distinct. Wire-derived addresses never carry a zone
//     (bmp.ParsePeerHeader builds them with netip.AddrFrom16), so this
//     only arises for a collector-side address such as a link-local BMP
//     session endpoint.
func EncodeIP(a netip.Addr) string {
	if !a.IsValid() {
		// Distinct from any hex token: 'i', 'n', 'v', 'l' are not hex digits.
		return "invalid"
	}
	zone := a.Zone()
	if a.Is4In6() {
		a = a.Unmap()
	}
	var tok string
	if a.Is4() {
		b := a.As4()
		tok = hex.EncodeToString(b[:])
	} else {
		b := a.As16()
		tok = hex.EncodeToString(b[:])
	}
	if zone != "" {
		h := fnv.New32a()
		h.Write([]byte(zone))
		tok = fmt.Sprintf("%s-z%08x", tok, h.Sum32())
	}
	return tok
}

// DecodeIP inverts EncodeIP, recovering the address from a token built by
// it. Any suffix from the first '-' onward is ignored, so a PeerToken
// carrying a "-r"/"-z"/"-d" suffix can be passed directly; a zone is therefore
// not recoverable (it is hashed, not encoded). Returns an error for
// "invalid", "locrib", or anything that is not 8 or 32 hex digits.
//
// This exists so operator tooling can turn a subject back into a real
// address -- the round-trip that the earlier text-and-escape scheme could
// not provide, and a reason hex was chosen over it.
func DecodeIP(tok string) (netip.Addr, error) {
	if i := strings.IndexByte(tok, '-'); i >= 0 {
		tok = tok[:i]
	}
	if len(tok) != 8 && len(tok) != 32 {
		return netip.Addr{}, fmt.Errorf("subjects: DecodeIP: %q is not an 8- or 32-digit hex address token", tok)
	}
	b, err := hex.DecodeString(tok)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("subjects: DecodeIP: %q: %w", tok, err)
	}
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.Addr{}, fmt.Errorf("subjects: DecodeIP: %q decoded to %d bytes", tok, len(b))
	}
	return a, nil
}

// familyNames is the family-token registry:
// address families that get a typed subject name now, even though this
// repo's deeper BGP attribute parsing for several of them (VPNv4, EVPN,
// BGP-LS) came later. Any AFI/SAFI pair not listed here falls
// back to FamilyToken's "x{afi}-{safi}" form.
var familyNames = map[bgp.Family]string{
	{AFI: 1, SAFI: 1}:      "ipv4u",
	{AFI: 2, SAFI: 1}:      "ipv6u",
	{AFI: 1, SAFI: 4}:      "lu4",
	{AFI: 1, SAFI: 128}:    "vpn4",
	{AFI: 2, SAFI: 128}:    "vpn6",
	{AFI: 25, SAFI: 70}:    "evpn",
	{AFI: 16388, SAFI: 71}: "ls",
}

// FamilyToken returns f's subject token: the registered name if f is in
// the registry above, else the deferred-family fallback
// "x{afi}-{safi}". AFI is a uint16 and SAFI a uint8, so the fallback is
// always plain decimal digits plus a literal 'x' and '-' -- it never
// panics and never returns an unsafe token, for any Family whatsoever
// (see FuzzFamilyToken).
func FamilyToken(f bgp.Family) string {
	if n, ok := familyNames[f]; ok {
		return n
	}
	return fmt.Sprintf("x%d-%d", f.AFI, f.SAFI)
}

// IsLS reports whether f is a BGP-LS family (RFC 7752), identified by AFI
// 16388 regardless of SAFI (71 for BGP-LS NLRI, 72 for BGP-LS-VPN).
func IsLS(f bgp.Family) bool { return f.AFI == 16388 }

// PeerToken returns ph's subject token: PeerBaseToken (the BGP session's
// own identity -- "locrib" for PeerTypeLocRIB, else EncodeIP(ph.Addr), plus
// a Distinguisher suffix when there is one) followed by a ribDirection
// suffix naming which of the router's RIB views this message describes.
//
// The three forms cannot collide. EncodeIP returns 8 or 32 lowercase hex
// digits, or the literal "invalid"; "locrib" and "invalid" each contain
// letters outside 0-9a-f, and both differ from each other. A Distinguisher,
// zone or direction suffix introduces a '-', which a bare hex token never
// contains; the three suffixes are told apart by their marker letter ('z',
// 'r', 'd') and always appear in that order, so the concatenation stays
// injective. Never panics, for any PeerHeader whatsoever (see
// FuzzPeerToken).
func PeerToken(ph bmp.PeerHeader) string {
	return PeerBaseToken(ph) + ribDirection(ph)
}

// PeerBaseToken returns the part of PeerToken that identifies the BGP
// session rather than the RIB view: the peer address (or "locrib") plus any
// Peer Distinguisher suffix, with no direction suffix.
//
// The up-to-four RIB streams a router may mirror for one neighbor (see
// ribDirection) get four PeerTokens but share one PeerBaseToken, which is
// what lets a caller key state that is a property of the OPEN exchange
// rather than of the RIB view -- the collector's negotiated capabilities --
// across all four while subjects and sequence counters stay split.
//
// RFC 9069 §4.2 uses the Peer Distinguisher on the Loc-RIB peer type to say
// *which* VRF a Loc-RIB dump belongs to, so the Distinguisher suffix applies
// to Loc-RIB too. Returning a bare "locrib" for every Loc-RIB peer would
// merge every VRF's routes onto one subject.
func PeerBaseToken(ph bmp.PeerHeader) string {
	tok := "locrib"
	if ph.Type != bmp.PeerTypeLocRIB {
		tok = EncodeIP(ph.Addr)
	}
	if ph.Distinguisher != 0 {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], ph.Distinguisher)
		h := fnv.New32a()
		h.Write(b[:])
		tok = fmt.Sprintf("%s-r%08x", tok, h.Sum32())
	}
	return tok
}

// ribDirection returns the token suffix naming which RIB ph's message
// describes: RFC 8671's O flag (adj-RIB-out) crossed with RFC 7854's L
// flag (post-policy). A router may mirror all four of these streams for
// one neighbor at once, and they are four different sets of routes, so
// they are four different peers as far as subjects and collector peer
// state are concerned -- without this, routes a router *sent* would be
// published onto the subject carrying routes it *received*.
//
// Pre-policy adj-RIB-in encodes as the empty suffix. It is what every
// router in the corpus sends and the overwhelmingly common case, so the
// bare address token keeps meaning what it always meant; direction is
// still injective, since absence denotes exactly one of the four.
//
// The Loc-RIB peer type is excluded entirely: RFC 9069 §4.2 redefines the
// per-peer flags byte for it, where the only defined bit is F (0x80,
// address family of the Loc-RIB instance) and everything else is reserved.
// Bits 0x40 and 0x10 therefore carry no L or O meaning there, and reading
// them would let junk a sender left in reserved bits split one Loc-RIB peer
// across up to four subjects, peer states and sequence counters. A Loc-RIB
// dump is neither adj-RIB-in nor adj-RIB-out -- it is the router's own
// table -- so the empty suffix is not a default here, it is the only
// correct answer.
func ribDirection(ph bmp.PeerHeader) string {
	if ph.Type == bmp.PeerTypeLocRIB {
		return ""
	}
	switch {
	case ph.AdjRIBOut() && ph.PostPolicy():
		return "-doutpost"
	case ph.AdjRIBOut():
		return "-dout"
	case ph.PostPolicy():
		return "-dpost"
	default:
		return ""
	}
}

// validToken reports whether s is safe to place, unescaped, into a single
// NATS subject token: non-empty, and every byte is printable ASCII
// (0x21-0x7e) excluding '.' (the token separator) and the wildcard tokens
// '*' and '>'. This is what Route, Ls, Peer, Stats, and Raw require of
// every router/peer/family argument -- see the package doc comment.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '!' || c > '~' || c == '.' || c == '*' || c == '>' {
			return false
		}
	}
	return true
}

// mustToken panics if s is not a valid subject token (see validToken),
// naming fn and field in the panic message.
func mustToken(fn, field, s string) {
	if !validToken(s) {
		panic(fmt.Sprintf("subjects: %s: %s %q is not a valid NATS subject token", fn, field, s))
	}
}

// Route returns the subject a route/withdraw envelope for the given
// family, router, and peer publishes on: 6 tokens,
// "vantage.v1.route.{family}.{router}.{peer}".
// Partition-free -- see the package doc comment.
func Route(family, router, peer string) string {
	mustToken("Route", "family", family)
	mustToken("Route", "router", router)
	mustToken("Route", "peer", peer)
	return Prefix + ".route." + family + "." + router + "." + peer
}

// Ls returns the subject a BGP-LS envelope for router/peer publishes on:
// "vantage.v1.ls.{router}.{peer}".
func Ls(router, peer string) string {
	mustToken("Ls", "router", router)
	mustToken("Ls", "peer", peer)
	return Prefix + ".ls." + router + "." + peer
}

// Peer returns the subject a peer up/down envelope for router/peer
// publishes on: "vantage.v1.peer.{router}.{peer}".
func Peer(router, peer string) string {
	mustToken("Peer", "router", router)
	mustToken("Peer", "peer", peer)
	return Prefix + ".peer." + router + "." + peer
}

// Stats returns the subject a stats-report envelope for router/peer
// publishes on: "vantage.v1.stats.{router}.{peer}".
func Stats(router, peer string) string {
	mustToken("Stats", "router", router)
	mustToken("Stats", "peer", peer)
	return Prefix + ".stats." + router + "." + peer
}

// Raw returns the subject a raw/unparseable-message envelope for router
// publishes on: "vantage.v1.raw.{router}".
func Raw(router string) string {
	mustToken("Raw", "router", router)
	return Prefix + ".raw." + router
}

// Beat returns the subject a collector's heartbeat publishes on:
// "vantage.v1.stats.beat.id{hex}", where {hex} is the collector_id's bytes as
// lowercase hex. It sits under the STATS stream's "vantage.v1.stats.>", so no
// stream changes.
//
// collector_id is operator-supplied free text -- a hostname by default, dots
// included -- so it is encoded rather than validated: hex of its bytes is
// injective and always a safe token. The "id" prefix keeps the token from
// ever being all hex digits, so humanizeSubject in `vantage debug`, which
// decodes any 8- or 32-hex-digit token as an address, never renders a
// collector named "dev1" as 100.101.118.49.
//
// The "beat" token cannot collide with a stats report's router token: that is
// always EncodeIP output -- hex digits, or "invalid" -- and "beat" contains a
// 't'.
//
// It panics on an empty collector_id, per this package's builder convention:
// an empty id names no collector, and LoadConfig never produces one.
func Beat(collectorID string) string {
	if collectorID == "" {
		panic("subjects: Beat: collector_id is empty")
	}
	tok := "id" + hex.EncodeToString([]byte(collectorID))
	mustToken("Beat", "collector_id", tok)
	return Prefix + ".stats.beat." + tok
}
