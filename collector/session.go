// Package collector: BMP session state and envelope assembly.
//
// A Session owns the full mutable state for one BMP transport connection
// (per-peer sequence counters, negotiated capabilities, the router's quirk
// profile) and is not safe for concurrent use — exactly like quirk.Set, a
// Session belongs to exactly one goroutine (the one reading BMP messages off
// that connection). The collector runs many such connections concurrently,
// each with its own Session, which is what the package's race-detector
// requirement actually exercises: no accidental sharing of mutable state
// *across* sessions (the read-only quirk.Registry aside).
package collector

import (
	"fmt"
	"net/netip"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/quirk"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

// Event is one envelope ready to publish: the subject to publish it on, the
// NATS msg-id to dedupe it by, and the envelope itself.
type Event struct {
	Subject string
	MsgID   string
	Env     *vantagev1.Envelope
}

// peerState is the collector's per-peer record within one Session.
//
// seq is deliberately NOT reset by a Peer-Down: it is the counter half of the
// (router, peer, session_id, seq) ordering identity, and a peer flapping
// (Down then Up again) within the same session is still the same peer under
// that identity — router and peer token are unchanged, and session_id only
// changes across a collector restart. Resetting seq to 0 across the flap
// would make the post-flap Peer-Up reuse the exact msg-id
// ("router/peer/session_id/1") the original Peer-Up already used, so
// JetStream's msg-id dedup would silently drop the second, distinct event as
// a "duplicate" of the first — the same class of bug the subjects package's
// hex-token redesign exists to prevent. up and caps, by contrast, describe
// the *current* negotiated session and are genuinely gone on Peer-Down: up
// is cleared so a Route Monitoring message arriving before the next Peer-Up
// correctly latches QK_CAPS_MISSING again, and caps is cleared alongside it
// so a stale capability set from the prior session is never reused.
//
// up and capsKnown are deliberately separate. "The peer's session is up" and
// "this collector knows that peer's negotiated capabilities" are different
// facts, and a Peer-Up whose embedded OPENs fail to parse produces the first
// without the second. Keying QK_CAPS_MISSING off up alone would leave that
// peer's Route Monitoring silently unflagged while it is in fact parsed with
// an empty Caps -- exactly the "we are reading this peer's routes without
// knowing what it negotiated" condition the flag exists to announce.
//
// caps here is this stream's *own* Peer-Up. A stream that has none can still
// borrow its BGP session's negotiated view from the Session's openExchange
// map -- see handleRM -- because a peerState is one RIB view of a session,
// and there are up to four of those per session (RFC 8671 crossed with RFC
// 7854's L flag) but only one OPEN exchange.
type peerState struct {
	caps      bgp.Caps
	up        bool
	capsKnown bool
	seq       uint64

	// hdr is the peer header from this view's Peer-Up, kept for one reason:
	// Close has to name the peer in an envelope built from no wire message
	// at all. Only the identity fields are ever read from it (via peerID);
	// hdr.Timestamp belongs to the Peer-Up and must never reach a later
	// envelope's ts_router, which is why Close builds its envelope with a
	// nil header and attaches the PeerId itself rather than passing hdr
	// back to envelope.
	hdr bmp.PeerHeader
}

// openExchange is what one BGP session's Peer-Up OPEN pair negotiated,
// keyed by subjects.PeerBaseToken -- the peer's identity with the
// RIB-direction suffix stripped -- and therefore shared by every RIB view
// of that session. Negotiated capabilities are a property of the OPEN
// exchange, not of the RIB view: RFC 8671 §5 has a router send a Peer Up
// per RIB it mirrors, but all of them describe the same two OPENs, so a
// view whose own Peer Up has not arrived (or arrived before the collector
// connected) can take its capabilities from here rather than parse with an
// empty Caps. Subjects and sequence counters stay split; only this is
// shared.
//
// Both merges are stored, and neither is ever substituted for the other.
// inbound answers "do the UPDATEs the peer sends the router carry a Path
// Identifier?"; outbound asks the same of the UPDATEs the router sends the
// peer. Under an asymmetric ADD-PATH negotiation -- a route reflector
// advertising send-only to a client advertising receive-only, the common
// case -- those two answers differ, so a stream must take the merge
// computed for its own direction of travel. Handing an adj-RIB-out stream
// the inbound merge would read every NLRI in it four bytes short, with no
// flag raised, which is exactly what storing one merge and reusing it
// would invite. See bgp.Merge.
type openExchange struct {
	inbound  bgp.Caps // peer -> router: adj-RIB-in, pre- or post-policy
	outbound bgp.Caps // router -> peer: RFC 8671 adj-RIB-out
}

// forDirection returns the merge computed for the direction ph's messages
// travel.
func (e *openExchange) forDirection(ph bmp.PeerHeader) bgp.Caps {
	if adjRIBOut(ph) {
		return e.outbound
	}
	return e.inbound
}

// adjRIBOut reports whether the routes ph's message describes travel router
// -> peer (RFC 8671's O flag) rather than peer -> router.
//
// The Loc-RIB peer type is excluded: RFC 9069 §4.2 redefines the per-peer
// flags byte for it, leaving bit 0x10 reserved rather than the O flag, so
// junk in that bit must not flip a Loc-RIB dump's direction of travel. This
// mirrors subjects.ribDirection, which does not direction-suffix that peer
// type either -- the two must agree, since one keys the subject and the
// other keys the capabilities the routes on it are parsed with.
func adjRIBOut(ph bmp.PeerHeader) bool {
	return ph.Type != bmp.PeerTypeLocRIB && ph.AdjRIBOut()
}

// Session holds one BMP transport connection's collector-side state and
// turns each bmp.Msg into zero or more Events.
type Session struct {
	routerIP    netip.Addr
	routerToken string
	collectorID string
	sessionID   uint64
	now         func() time.Time
	force       []quirk.ID
	disable     []quirk.ID

	sysName string
	profile quirk.Profile
	// vendor/os are the operator-configured identity (Overrides), kept so
	// that a late Initiation is still resolved against them. bannerVendor is
	// what the banner itself claimed, kept only to detect a disagreement --
	// see IdentityConflict.
	vendor, os   string
	bannerVendor string
	quirks       *quirk.Set
	peers        map[string]*peerState    // key: subjects.PeerToken(ph)
	opens        map[string]*openExchange // key: subjects.PeerBaseToken(ph)
	rawSeq       uint64                   // router-scoped pseudo-peer counter (raw events)
	maxPeers     int                      // cap on len(peers); see maxPeersPerSession
}

// maxPeersPerSession caps the distinct peer views one Session tracks. Every
// view a router names costs a peerState held until the connection ends (a
// Peer Down keeps it, for seq) and a VIEW_LOST event at Close, and the router
// alone decides how many it names: a million Peer Ups held 238 MiB and made
// Close emit a million events. A view is one RIB stream of one peer, so this
// is 10,000 neighbors in the pre-policy view every captured router sends, or
// 2,500 mirrored in all four RIB views -- well past any real BMP speaker --
// at about 2.4 MB per session. Past it, a message for a new view is recorded
// as a raw event rather than tracked.
const maxPeersPerSession = 10000

// Overrides is everything an operator can say about one router, resolved from
// config.RouterOverride for the router's IP before its session starts.
//
// It is a struct rather than four parameters because this set grows: it began
// as the two quirk slices and gained identity when it became clear
// that a BMP banner frequently is not one. Callers with nothing to say pass
// the zero value, which is exactly the pre-config behavior.
type Overrides struct {
	// Force and Disable are per-router quirk overrides, applied on top of
	// whatever static profile the eventual Initiation message resolves to.
	Force, Disable []quirk.ID

	// Vendor and OS identify the router, ahead of anything its banner says.
	// Empty means "no configuration", not "unknown": the banner anchors are
	// then left in charge, so a zero-config deployment behaves as before.
	Vendor, OS string
}

// NewSession constructs a Session for one BMP connection from routerIP. now
// supplies collector-side wall-clock time (injected for tests); ov carries the
// per-router configuration.
func NewSession(routerIP netip.Addr, collectorID string, sessionID uint64,
	now func() time.Time, ov Overrides) *Session {
	return &Session{
		routerIP: routerIP, routerToken: subjects.EncodeIP(routerIP),
		collectorID: collectorID, sessionID: sessionID, now: now,
		force: ov.Force, disable: ov.Disable,
		vendor: ov.Vendor, os: ov.OS,
		// The identity applies from the first message, not from the
		// Initiation: a router that never sends one (or sends it late) is
		// still the device the operator says it is.
		profile:  quirk.Profile{}.WithIdentity(ov.Vendor, ov.OS),
		quirks:   quirk.Resolve(quirk.Profile{}.WithIdentity(ov.Vendor, ov.OS), ov.Force, ov.Disable),
		peers:    map[string]*peerState{},
		opens:    map[string]*openExchange{},
		maxPeers: maxPeersPerSession,
	}
}

// Profile returns the router's resolved vendor/OS/version profile: the
// operator's configured identity where there is one, the banner's anchor
// result otherwise, and in both cases whatever version the banner yielded.
// Zero-valued until an Initiation arrives, except for a configured identity,
// which applies from the first message.
func (s *Session) Profile() quirk.Profile { return s.profile }

// IdentityConflict reports whether this router's banner named a vendor and the
// operator's configuration named a different one. Config still wins; this only
// records that the two disagreed, which is worth an operator's attention
// because one of them is wrong and neither will complain on its own.
//
// A banner carrying no vendor at all -- the ordinary IOS-XR case -- is not a
// conflict. It contradicts nothing.
func (s *Session) IdentityConflict() bool {
	return s.bannerVendor != "" && s.vendor != "" && s.bannerVendor != s.vendor
}

// Handle processes one BMP message. It never fails: unparseable input is
// returned as a RawEvent so the session (and data) always survive.
func (s *Session) Handle(m bmp.Msg) []Event {
	switch m.Type {
	case bmp.TypeInitiation:
		info, err := bmp.ParseInit(m.Payload)
		if err != nil {
			return []Event{s.rawEvent(m, err)}
		}
		s.sysName = info.SysName
		banner := quirk.ProfileFromSysDescr(info.SysDescr)
		s.bannerVendor = banner.Vendor
		// Config outranks the banner: WithIdentity is a no-op when no vendor
		// was configured, so the anchor result stands for an unconfigured
		// router and is replaced for a configured one.
		s.profile = banner.WithIdentity(s.vendor, s.os)
		s.quirks = quirk.Resolve(s.profile, s.force, s.disable)
		return nil
	case bmp.TypePeerUp:
		return s.handlePeerUp(m)
	case bmp.TypePeerDown:
		return s.handlePeerDown(m)
	case bmp.TypeRouteMonitoring:
		return s.handleRM(m)
	case bmp.TypeStatsReport:
		return s.handleStats(m)
	default:
		// Termination and any message type this collector has no typed
		// event for (e.g. Route Mirroring): retention-cheap, lossless
		// RawEvent.
		return []Event{s.rawEvent(m, nil)}
	}
}

// envelope builds the fields common to every event. ph is nil for
// router-scoped events (RawEvent) that carry no peer identity at all.
func (s *Session) envelope(ph *bmp.PeerHeader, seq uint64, extra []vantagev1.ParseFlag) *vantagev1.Envelope {
	now := s.now()
	env := &vantagev1.Envelope{
		CollectorId: s.collectorID,
		Router:      &vantagev1.RouterId{Ip: s.routerIP.String(), SysName: s.sysName},
		SessionId:   s.sessionID,
		Seq:         seq,
		TsCollector: timestamppb.New(now),
		RouterInfo:  s.profile.RouterInfo(),
		ParseFlags:  extra,
	}
	// QK_VERSION_UNPARSED is a static, session-wide condition
	// (the router's sysDescr named a known vendor but not a version this
	// package can parse), so every envelope emitted while it is active must
	// carry the flag -- not just the Initiation message, which never emits
	// an event at all and so would otherwise never be attributed to any
	// envelope.
	if s.quirks.Active(quirk.QkVersionUnparsed) {
		env.ParseFlags = append(env.ParseFlags, quirk.FlagFor(quirk.QkVersionUnparsed))
	}
	if ph != nil {
		env.Peer = peerID(ph)
		// Latch records the observation (dynamic detection outranks a version
		// claim); Active is then what respects a per-router disable. Without
		// the Active check, `disable_quirks: ["QK_TS_ZERO"]` still
		// substituted collector time and still emitted the flag -- an ops
		// escape hatch the config validated and the README advertised, that
		// did nothing. Disabled means "trust the router's zero", so the wire
		// value passes through unaccommodated and unflagged.
		zeroTS := ph.Timestamp.IsZero()
		if zeroTS {
			s.quirks.Latch(quirk.QkTSZero)
		}
		if zeroTS && s.quirks.Active(quirk.QkTSZero) {
			env.TsRouter = timestamppb.New(now)
			env.ParseFlags = append(env.ParseFlags, quirk.FlagFor(quirk.QkTSZero))
		} else {
			env.TsRouter = timestamppb.New(ph.Timestamp)
		}
	} else {
		env.Peer = &vantagev1.PeerId{}
		env.TsRouter = timestamppb.New(now)
	}
	return env
}

// peerID projects a BMP peer header onto the wire PeerId. It is shared by
// envelope and by Close rather than inlined in each: Close builds an
// envelope for a peer with no message in front of it, and two spellings of
// "which peer is this" would be free to drift into naming the same peer two
// ways in one session's stream.
func peerID(ph *bmp.PeerHeader) *vantagev1.PeerId {
	return &vantagev1.PeerId{
		Type: vantagev1.PeerType(ph.Type), PostPolicy: ph.PostPolicy(),
		AdjRibOut: ph.AdjRIBOut(),
		Ip:        ph.Addr.String(), Asn: ph.AS, BgpId: ph.BGPID, Distinguisher: ph.Distinguisher,
	}
}

func (s *Session) msgID(peerToken string, seq uint64) string {
	return fmt.Sprintf("%s/%s/%d/%d", s.routerToken, peerToken, s.sessionID, seq)
}

// peerState returns the peerState for tok, creating a zero-value one (caps
// unknown, up == false) on first sight of this peer token. It fails rather
// than create one past s.maxPeers; the caller records the message raw.
func (s *Session) peerState(tok string) (*peerState, error) {
	ps, ok := s.peers[tok]
	if !ok {
		if len(s.peers) >= s.maxPeers {
			return nil, fmt.Errorf("collector: session already tracks %d peer views, the most it keeps; not tracking another", s.maxPeers)
		}
		ps = &peerState{}
		s.peers[tok] = ps
	}
	return ps, nil
}

func (s *Session) handlePeerUp(m bmp.Msg) []Event {
	ph, rest, err := bmp.ParsePeerHeader(m.Payload)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	// 16-byte local address + 2-byte local port + 2-byte remote port (RFC
	// 7854 §4.10), before the two OPEN messages.
	if len(rest) < 20 {
		return []Event{s.rawEvent(m, fmt.Errorf("bmp: peer-up truncated: %d bytes after peer header, need at least 20", len(rest)))}
	}
	var localIP netip.Addr
	if ph.IPv6() {
		localIP = netip.AddrFrom16([16]byte(rest[:16]))
	} else {
		localIP = netip.AddrFrom4([4]byte(rest[12:16]))
	}
	localPort := uint32(rest[16])<<8 | uint32(rest[17])
	remotePort := uint32(rest[18])<<8 | uint32(rest[19])
	opens := rest[20:]

	pe := &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP,
		LocalIp: localIP.String(), LocalPort: localPort, RemotePort: remotePort}

	tok := subjects.PeerToken(ph)
	ps, err := s.peerState(tok)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	sentOpen, recvOpen, oerr := splitOpens(opens)
	if oerr == nil {
		pe.SentOpen, pe.RecvOpen = sentOpen, recvOpen
		// sc is the monitored router's own OPEN, rc the OPEN it received
		// back from the peer. bgp.Merge's arguments are (the OPEN of the
		// side that receives the monitored UPDATEs, the OPEN of the side
		// that sends them), so the pairing depends on which way the routes
		// this stream carries travel -- and the two answers differ whenever
		// ADD-PATH was negotiated asymmetrically. Both are computed here,
		// once per Peer Up, and every RIB view of this session then takes
		// the one for its own direction.
		sc, e1 := bgp.ParseOpen(sentOpen)
		rc, e2 := bgp.ParseOpen(recvOpen)
		if e1 == nil && e2 == nil {
			ex := &openExchange{inbound: bgp.Merge(sc, rc), outbound: bgp.Merge(rc, sc)}
			s.opens[subjects.PeerBaseToken(ph)] = ex
			ps.caps = ex.forDirection(ph)
			ps.capsKnown = true
			pe.Caps = capsProto(ps.caps)
		}
	}
	ps.up = true
	ps.hdr = ph
	ps.seq++
	env := s.envelope(&ph, ps.seq, nil)
	env.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: pe}
	return []Event{{Subject: subjects.Peer(s.routerToken, tok), MsgID: s.msgID(tok, ps.seq), Env: env}}
}

// splitOpens slices two length-prefixed BGP messages out of a Peer-Up tail.
// bgp.ParseOpen does not do this itself: it parses one already-isolated OPEN
// and ignores any trailing bytes, so the two back-to-back OPENs a Peer-Up
// carries must be separated here first, using each one's own BGP header
// length field at offset 16-17 (RFC 4271 §4.1: 16-byte marker, then a 2-byte
// length).
func splitOpens(b []byte) (sent, recv []byte, err error) {
	next := func(b []byte) ([]byte, []byte, error) {
		if len(b) < 19 {
			return nil, nil, fmt.Errorf("bgp: open message truncated: %d bytes, need at least 19", len(b))
		}
		l := int(b[16])<<8 | int(b[17])
		if l < 19 || l > len(b) {
			return nil, nil, fmt.Errorf("bgp: open message declares length %d, have %d bytes available", l, len(b))
		}
		return b[:l], b[l:], nil
	}
	sent, rest, err := next(b)
	if err != nil {
		return nil, nil, err
	}
	recv, _, err = next(rest)
	if err != nil {
		return nil, nil, err
	}
	return sent, recv, nil
}

// capsProto projects negotiated capabilities onto the wire Capabilities
// message. Family lists are emitted sorted by AFI then SAFI so identical
// input always serializes identically, rather than depending on Go's
// randomized map iteration order.
func capsProto(c bgp.Caps) *vantagev1.Capabilities {
	return &vantagev1.Capabilities{
		FourByteAs:      c.FourByteAS,
		MpFamilies:      sortedFamilyProtos(c.MP),
		AddpathFamilies: sortedFamilyProtos(c.AddPathRecv),
		// Set together, always. A hold time of 0 is a real negotiated value
		// (RFC 4271: the timer never expires), and proto3 cannot distinguish
		// an unset uint32 from a zero one -- so the flag is what says an
		// OPEN was actually read, and everything downstream reads the flag
		// before the number.
		HoldTime:     uint32(c.HoldTime),
		HoldTimeSeen: true,
	}
}

func sortedFamilyProtos(m map[bgp.Family]bool) []*vantagev1.Family {
	fams := make([]bgp.Family, 0, len(m))
	for f := range m {
		fams = append(fams, f)
	}
	sort.Slice(fams, func(i, j int) bool {
		if fams[i].AFI != fams[j].AFI {
			return fams[i].AFI < fams[j].AFI
		}
		return fams[i].SAFI < fams[j].SAFI
	})
	out := make([]*vantagev1.Family, len(fams))
	for i, f := range fams {
		out[i] = &vantagev1.Family{Afi: uint32(f.AFI), Safi: uint32(f.SAFI)}
	}
	return out
}

func (s *Session) handlePeerDown(m bmp.Msg) []Event {
	ph, rest, err := bmp.ParsePeerHeader(m.Payload)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	tok := subjects.PeerToken(ph)
	ps, err := s.peerState(tok)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	ps.seq++

	pe := &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_DOWN}
	if len(rest) >= 1 {
		pe.DownReason = uint32(rest[0])
		pe.DownData = append([]byte{}, rest[1:]...)
	}
	env := s.envelope(&ph, ps.seq, nil)
	env.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: pe}
	ev := Event{Subject: subjects.Peer(s.routerToken, tok), MsgID: s.msgID(tok, ps.seq), Env: env}

	// Negotiated capabilities and "do we have a Peer-Up on record" both
	// belong to the session that just ended; seq does not (see peerState's
	// doc comment) and is deliberately left untouched. The shared OPEN
	// exchange goes with them: it describes the OPEN pair of the session
	// that just went down, so no RIB view of this peer may keep borrowing it
	// -- a view that does still hold its own Peer-Up keeps its own caps and
	// is unaffected.
	ps.caps = bgp.Caps{}
	ps.capsKnown = false
	ps.up = false
	delete(s.opens, subjects.PeerBaseToken(ph))
	return []Event{ev}
}

// Close ends this session's view of the router and returns one
// KIND_VIEW_LOST PeerEvent per peer this session could still see -- the
// peers whose last word was a Peer-Up.
//
// It exists because a BMP transport ending is not a BMP message. The router
// sends Peer Up, Peer Down and Route Monitoring; it sends nothing at all
// when the TCP session drops, and it cannot -- BMP is unidirectional and has
// no close notification. So without this, a collector that died, was
// rescheduled, or simply lost the connection left every peer of that session
// recorded up. Because the query layer scopes current state to
// max(session_id) per (collector_id, router_ip), and no newer session
// exists to displace it, that dead view is served as current forever, with
// no error anywhere: routes withdrawn since are still returned, and peers
// long gone still read "up".
//
// What it emits is deliberately NOT a Peer Down. Peer Down is the router
// stating that a BGP session dropped, and it carries a reason code the
// archive keeps and the dashboards decode. This event is the collector
// stating it stopped being able to see the peer. Those are different facts
// about different subjects, and the archive already separates them almost
// perfectly -- one fleet's routers turned over 354 BMP sessions and sent 0
// Peer Downs, while the NX-OS routers held 6 sessions and sent 17. Emitting
// KIND_DOWN here would have fixed the staleness by fabricating 354 router
// statements, which is this project's most recurring defect (a collection
// artifact recorded as a fact about the network) introduced into the very
// table that distinction is drawn in.
//
// A peer the router already reported down is skipped. Its final state is on
// record with a reason, and replacing that with a synthetic event would
// overwrite a better answer with a worse one.
//
// Close is idempotent: it clears up as it goes, so the second call has
// nothing to report. handleConn calls it from a defer on a path that can
// also be reached after an explicit close, and a second view-lost event
// under a fresh seq would read as the peer having been lost twice.
//
// Peers are walked in token order rather than map order so one dropped
// session publishes the same sequence of events every time. Each peer
// carries its own seq counter, so ordering cannot change any msg-id; it is
// the reproducibility of the published stream that is worth having.
func (s *Session) Close() []Event {
	toks := make([]string, 0, len(s.peers))
	for tok, ps := range s.peers {
		if ps.up {
			toks = append(toks, tok)
		}
	}
	sort.Strings(toks)

	evs := make([]Event, 0, len(toks))
	for _, tok := range toks {
		ps := s.peers[tok]
		ps.up = false
		ps.seq++
		// nil header, then the PeerId attached directly: there is no wire
		// message here, so ts_router can only be the collector's clock (see
		// envelope's nil branch). It is not left zero -- ts_collector drives
		// PARTITION BY and TTL, and a zero stamp lands the row in partition
		// 197001, already 90 days past the TTL, so the next merge deletes
		// the very event this function exists to record.
		//
		// No PARSE_FLAG_TS_COLLECTOR_FALLBACK either. That flag means "the
		// router's clock read zero and ours was substituted", and this
		// router did not send a zero clock, it sent nothing. Setting it
		// would attribute a quirk to a router that does not have it, in the
		// flag set the Parse anomalies dashboard counts per router.
		env := s.envelope(nil, ps.seq, nil)
		env.Peer = peerID(&ps.hdr)
		env.Payload = &vantagev1.Envelope_PeerEvent{
			PeerEvent: &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_VIEW_LOST},
		}
		evs = append(evs, Event{
			Subject: subjects.Peer(s.routerToken, tok),
			MsgID:   s.msgID(tok, ps.seq),
			Env:     env,
		})
	}
	return evs
}

// foldTreatAsWithdraw applies the RFC 7606 treat-as-withdraw outcome to
// every one of Update's independent NLRI representations, not only the
// classic ipv4u Announced/Withdrawn pair: vpn4/lu4/EVPN and BGP-LS as well.
// Each is a second, independent NLRI representation that survives a
// malformed *other* attribute in the same UPDATE -- e.g. a malformed
// ORIGIN alongside a cleanly-decoded MP_REACH -- since RawReach is empty
// on a clean typed parse, that second representation is the only place
// the NLRI still exists. Without this fold each would reach the wire as
// an announcement even though RFC 7606 requires the whole route be
// treated as withdrawn.
//
// Extracted from handleRM (rather than left inline) so it can be exercised
// directly against a *bgp.Update in tests, matching every other
// foldTreatAsWithdraw... test in this file. LsEvent carries LsNodes/
// LsLinks, so the BGP-LS half of this fold is observable through
// Session.Handle too (see TestSessionPublishesTypedLinkState and
// TestSessionTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn); this function
// is still exercised directly here as well, for the same reason every
// sibling fold test in this file is: it isolates the fold itself from
// everything else Session.Handle does on the way to an envelope.
func foldTreatAsWithdraw(u *bgp.Update) {
	u.Withdrawn = append(u.Withdrawn, u.Announced...)
	u.Announced = nil
	u.VpnWithdrawn = append(u.VpnWithdrawn, u.VpnAnnounced...)
	u.VpnAnnounced = nil
	u.EvpnWithdrawn = append(u.EvpnWithdrawn, u.EvpnAnnounced...)
	u.EvpnAnnounced = nil
	u.LsNodesWithdrawn = append(u.LsNodesWithdrawn, u.LsNodes...)
	u.LsNodes = nil
	u.LsLinksWithdrawn = append(u.LsLinksWithdrawn, u.LsLinks...)
	u.LsLinks = nil
	u.LsPrefixesWithdrawn = append(u.LsPrefixesWithdrawn, u.LsPrefixes...)
	u.LsPrefixes = nil
}

func (s *Session) handleRM(m bmp.Msg) []Event {
	ph, rest, err := bmp.ParsePeerHeader(m.Payload)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	if len(rest) < 19 {
		return []Event{s.rawEvent(m, fmt.Errorf("bmp: route monitoring: bgp header truncated: %d bytes, need at least 19", len(rest)))}
	}
	// The BGP header's own length field (RFC 4271 §4.1, offset 16) is what
	// bounds the UPDATE, not the BMP message around it. An UPDATE's IPv4
	// NLRI runs to the end of whatever it is handed, and each 0x00 byte
	// there is one /0 prefix, so parsing to the end of the BMP message let
	// a 16 MiB message with a lying header become 16 million prefixes and
	// 2.4 GB of heap. RFC 7854 §4.6 carries exactly one BGP PDU here, and
	// every Route Monitoring message in the committed captures fills its BMP
	// message exactly, so any disagreement -- short or long -- is a parse
	// error, recorded raw with every byte kept, rather than a guess at which
	// of the two lengths to believe. The field is 16 bits, so this also
	// holds an RFC 8654 extended message to 65535 bytes.
	if l := int(rest[16])<<8 | int(rest[17]); l != len(rest) {
		return []Event{s.rawEvent(m, fmt.Errorf("bmp: route monitoring: bgp header declares length %d, message carries %d bytes", l, len(rest)))}
	}
	tok := subjects.PeerToken(ph)
	ps, err := s.peerState(tok)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	caps, capsKnown := ps.caps, ps.capsKnown
	if !capsKnown {
		// This RIB view has no Peer-Up of its own. Before giving up, fall
		// back to the BGP session's OPEN exchange, which every RIB view of
		// this peer shares: a router mirroring adj-RIB-out sends a second,
		// O-flagged Peer Up (RFC 8671 §5), but it may not have arrived yet,
		// and capabilities are a property of the OPEN exchange rather than
		// of the RIB view, so taking them from there is not a guess.
		// forDirection is what keeps it honest -- this view gets the merge
		// computed for its own direction of travel, never the other one.
		if ex, ok := s.opens[subjects.PeerBaseToken(ph)]; ok {
			caps, capsKnown = ex.forDirection(ph), true
		}
	}
	var extra []vantagev1.ParseFlag
	if !capsKnown {
		// No negotiated capabilities on record for this peer at all -- not
		// for this RIB view, not for its BGP session: this collector never
		// saw a Peer-Up, the peer's most recent state was a Peer-Down (a
		// flap), or a Peer-Up did arrive but its embedded OPENs were
		// unparseable. In every case the UPDATE below is parsed with an
		// empty Caps, which is exactly what QK_CAPS_MISSING exists to flag.
		// Keyed off capsKnown rather than up so the third case -- peer
		// demonstrably up, capabilities unknown -- is not silently treated
		// as fully known.
		// Active gates the flag so a per-router disable actually takes
		// effect; Latch still records that the condition was observed.
		s.quirks.Latch(quirk.QkCapsMissing)
		if s.quirks.Active(quirk.QkCapsMissing) {
			extra = append(extra, quirk.FlagFor(quirk.QkCapsMissing))
		}
	}
	u, perr := bgp.ParseUpdate(rest[19:], caps)
	if perr != nil {
		return []Event{s.rawEvent(m, perr)}
	}
	if u.TreatAsWithdraw {
		foldTreatAsWithdraw(u)
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC {
			// The envelope already carries this flag via u.Flags below
			// regardless; latching QK_ADDPATH_HEURISTIC in the session's own
			// Set additionally makes it visible to s.quirks.Active for any
			// session-level logging/metrics, mirroring QK_TS_ZERO and
			// QK_CAPS_MISSING, which are both latched at their emission
			// points rather than left as envelope-only signals.
			s.quirks.Latch(quirk.QkAddPathHeuristic)
		}
	}
	ps.seq++
	env := s.envelope(&ph, ps.seq, append(extra, u.Flags...))
	fam := &vantagev1.Family{Afi: uint32(u.Family.AFI), Safi: uint32(u.Family.SAFI)}

	if subjects.IsLS(u.Family) {
		env.Payload = &vantagev1.Envelope_Ls{Ls: buildLsEvent(fam, u)}
		return []Event{{Subject: subjects.Ls(s.routerToken, tok), MsgID: s.msgID(tok, ps.seq), Env: env}}
	}

	re := &vantagev1.RouteEvent{
		Family: fam, Attrs: u.Attrs, EndOfRib: u.EndOfRIB,
		// Raw MP_REACH/MP_UNREACH value bytes for every non-ipv4u family,
		// not only BGP-LS. For FamilyIPv4U these are always nil
		// (bgp.ParseUpdate never populates them for that family), so this
		// assignment is a no-op in the common case and the lossless
		// fallback for every other family that hasn't grown a typed
		// decoder yet.
		RawReach: u.RawReach, RawUnreach: u.RawUnreach,
	}
	for _, p := range u.Announced {
		re.Announced = append(re.Announced, &vantagev1.Prefix{Prefix: p.Prefix.String(), PathId: p.PathID})
	}
	for _, p := range u.Withdrawn {
		re.Withdrawn = append(re.Withdrawn, &vantagev1.Prefix{Prefix: p.Prefix.String(), PathId: p.PathID})
	}
	// Typed VPN/labeled/EVPN NLRI: carried through verbatim, in lockstep
	// with the raw-bytes policy above -- u.RawReach/RawUnreach are already
	// empty on a fully clean typed parse and non-empty whenever any part
	// went untyped, and this layer does not re-derive that distinction,
	// only forwards it.
	re.VpnAnnounced = vpnProto(u.VpnAnnounced)
	re.VpnWithdrawn = vpnProto(u.VpnWithdrawn)
	re.EvpnAnnounced = evpnProto(u.EvpnAnnounced)
	re.EvpnWithdrawn = evpnProto(u.EvpnWithdrawn)
	env.Payload = &vantagev1.Envelope_Route{Route: re}
	subj := subjects.Route(subjects.FamilyToken(u.Family), s.routerToken, tok)
	return []Event{{Subject: subj, MsgID: s.msgID(tok, ps.seq), Env: env}}
}

func (s *Session) handleStats(m bmp.Msg) []Event {
	ph, rest, err := bmp.ParsePeerHeader(m.Payload)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	// RFC 7854 §4.8: a 4-byte "Number of TLVs" field precedes the TLVs
	// themselves; this collector reads TLVs to the end of the message
	// rather than trusting the count, so a mismatched count degrades
	// gracefully instead of desynchronizing the TLV walk.
	if len(rest) < 4 {
		return []Event{s.rawEvent(m, fmt.Errorf("bmp: stats report truncated: %d bytes after peer header, need at least 4", len(rest)))}
	}
	st := &vantagev1.StatsEvent{Counters: map[uint32]uint64{}}
	b := rest[4:]
	for len(b) >= 4 {
		typ := uint32(b[0])<<8 | uint32(b[1])
		l := int(b[2])<<8 | int(b[3])
		if len(b) < 4+l {
			return []Event{s.rawEvent(m, fmt.Errorf("bmp: stats report: tlv type %d declares %d bytes, %d available", typ, l, len(b)-4))}
		}
		v := b[4 : 4+l]
		switch l {
		case 4:
			st.Counters[typ] = uint64(uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3]))
		case 8:
			var x uint64
			for _, c := range v {
				x = x<<8 | uint64(c)
			}
			st.Counters[typ] = x
		}
		// Any other Stat Len is a wire form this collector doesn't have a
		// counter width for; the TLV is skipped (not stored, not an
		// error) rather than failing the whole message.
		b = b[4+l:]
	}

	tok := subjects.PeerToken(ph)
	ps, err := s.peerState(tok)
	if err != nil {
		return []Event{s.rawEvent(m, err)}
	}
	ps.seq++
	env := s.envelope(&ph, ps.seq, nil)
	env.Payload = &vantagev1.Envelope_Stats{Stats: st}
	return []Event{{Subject: subjects.Stats(s.routerToken, tok), MsgID: s.msgID(tok, ps.seq), Env: env}}
}

// rawEvent wraps m verbatim (reconstructing its BMP common header) as a
// lossless, router-scoped RawEvent. perr, if non-nil, is recorded as the
// human-readable reason this message couldn't be turned into a typed event;
// a nil perr means m parsed fine at the BMP layer but simply has no typed
// event yet (Termination, Route Mirroring, or any future type).
func (s *Session) rawEvent(m bmp.Msg, perr error) Event {
	s.rawSeq++
	env := s.envelope(nil, s.rawSeq, nil)
	re := &vantagev1.RawEvent{
		// The generated field is BmpMsg, not Bmp_Msg.
		BmpMsg: append(bmp.AppendHeader(nil, m.Type, len(m.Payload)), m.Payload...),
	}
	if perr != nil {
		re.ParseError = perr.Error()
	}
	env.Payload = &vantagev1.Envelope_Raw{Raw: re}
	return Event{Subject: subjects.Raw(s.routerToken), MsgID: s.msgID("raw", s.rawSeq), Env: env}
}

// MirrorEvent wraps m as a RawEvent marked mirrored, for republication by
// mirror mode. It reuses rawEvent's framing so a capture file is byte-identical
// to what arrived on the wire, then sets Mirrored so a consumer can tell a
// deliberate copy from a parse failure -- both land on the same subject, and
// an empty parse_error is also true of Termination.
//
// It consumes a rawSeq like any other raw event, so mirrored messages sit in
// the same monotonic sequence and cannot collide on msg-id.
//
// Callers must not invoke this for a message that Handle already turned into
// a RawEvent of its own (a parse failure, or a message type with no typed
// event) -- that message has already reached the raw stream, and a mirror
// copy on top of it would be a second rawSeq-distinct entry for the same
// bytes, not a capture of something that would otherwise have been lost.
func (s *Session) MirrorEvent(m bmp.Msg) Event {
	ev := s.rawEvent(m, nil)
	if r := ev.Env.GetRaw(); r != nil {
		r.Mirrored = true
	}
	return ev
}

// vpnProto converts decoded VPN prefixes (vpn4/lu4) to their wire
// form. Labels is forwarded unshifted -- it already holds the shifted 20-bit
// MPLS label values bgp.VpnPrefix.Labels defines, including the RFC 3107/8277
// withdraw sentinel (0x800000 >> 4 == 524288) verbatim when present; this
// layer does not special-case it.
func vpnProto(in []bgp.VpnPrefix) []*vantagev1.VpnPrefix {
	if len(in) == 0 {
		return nil
	}
	out := make([]*vantagev1.VpnPrefix, 0, len(in))
	for _, p := range in {
		out = append(out, &vantagev1.VpnPrefix{
			Prefix: p.Prefix.String(),
			PathId: p.PathID,
			Rd:     p.RD,
			Labels: p.Labels,
		})
	}
	return out
}

// evpnProto converts decoded EVPN routes to their wire form. Prefix is only
// meaningful for route type 5; an unset netip.Prefix would render as "invalid
// Prefix", so it is emitted only when valid. Labels is forwarded unshifted,
// as raw 24-bit values (VNI or 20-bit-label-in-high-bits, per
// bgp.EvpnRoute.Labels) -- the opposite convention from vpnProto's Labels,
// deliberately: see bgp.EvpnRoute's doc comment for why the asymmetry is not
// a bug.
func evpnProto(in []bgp.EvpnRoute) []*vantagev1.EvpnRoute {
	if len(in) == 0 {
		return nil
	}
	out := make([]*vantagev1.EvpnRoute, 0, len(in))
	for _, r := range in {
		e := &vantagev1.EvpnRoute{
			RouteType:     uint32(r.RouteType),
			Rd:            r.RD,
			PathId:        r.PathID,
			Mac:           r.MAC,
			Ip:            r.IP,
			OriginatingIp: r.OriginatingIP,
			GatewayIp:     r.GatewayIP,
			EthernetTag:   r.EthernetTag,
			Esi:           r.ESI,
			Labels:        r.Labels,
			Raw:           r.Raw,
		}
		if r.Prefix.IsValid() {
			e.Prefix = r.Prefix.String()
		}
		out = append(out, e)
	}
	return out
}

// lsDescriptor converts a decoded node descriptor to its wire form. Every
// field (ASN, BGP-LS ID, area, router-id) is part of a node's identity --
// see bgp.LsNodeDescriptor's doc comment -- so all four are forwarded, not
// just RouterID.
func lsDescriptor(d bgp.LsNodeDescriptor) *vantagev1.LsNodeDescriptor {
	return &vantagev1.LsNodeDescriptor{
		Asn: d.ASN, BgplsId: d.BGPLSID, Area: d.Area, RouterId: d.RouterID,
	}
}

// lsNodeMessage builds one LsNode from a decoded NLRI and the BGP-LS
// Attribute that describes it. attrs is shared by every node/link NLRI in
// the same UPDATE (RFC 9552 §5.3 has exactly one BGP-LS Attribute per
// message) and may be nil -- an UPDATE can carry NLRI with no attribute at
// all, e.g. a pure withdrawal, so every attrs-derived field is conditional
// on it being present rather than dereferenced unconditionally.
//
// Shared by both the announced and withdrawn paths in handleRM: is_withdraw
// is the only thing that differs between them, and the caller sets it after
// this returns.
func lsNodeMessage(n bgp.LsNodeNLRI, attrs *bgp.LsAttrs) *vantagev1.LsNode {
	pn := &vantagev1.LsNode{
		Protocol: uint32(n.Protocol), Identifier: n.Identifier,
		Local: lsDescriptor(n.Local),
	}
	if attrs != nil {
		pn.Name = attrs.NodeName
		pn.SrgbBase, pn.SrgbSize = attrs.SRGBBase, attrs.SRGBSize
		pn.SrlbBase, pn.SrlbSize = attrs.SRLBBase, attrs.SRLBSize
		for _, a := range attrs.SRAlgorithms {
			pn.SrAlgorithms = append(pn.SrAlgorithms, uint32(a))
		}
		if attrs.LocalRouterIDv4.IsValid() {
			b := attrs.LocalRouterIDv4.As4()
			pn.RouterIdV4 = b[:]
		}
	}
	// This Node NLRI's own unrecognized TLVs (its own top-level TLVs plus
	// its node descriptor's nested sub-TLVs -- see bgp.LsNodeNLRI.Unknown).
	// Kept as its own wire field, distinct from the shared BGP-LS
	// Attribute's LsEvent.unknown_tlvs; sink/rows.go merges the two into
	// ls_nodes' single unknown_tlvs column.
	if len(n.Unknown) > 0 {
		pn.UnknownTlvs = make(map[uint32][]byte, len(n.Unknown))
		for k, v := range n.Unknown {
			pn.UnknownTlvs[uint32(k)] = v
		}
	}
	return pn
}

// lsPrefixMessage builds one LsPrefix from a decoded NLRI and the BGP-LS
// Attribute that describes it. See lsNodeMessage for why attrs may be nil and
// why is_withdraw is left to the caller.
//
// The prefix goes on the wire as the packed address bytes plus a length,
// rather than as text: bytes are what the router sent, and rendering is the
// sink's job (sink/rows.go turns them into "10.255.0.5" for the archive).
func lsPrefixMessage(p bgp.LsPrefixNLRI, attrs *bgp.LsAttrs) *vantagev1.LsPrefix {
	pp := &vantagev1.LsPrefix{
		Protocol: uint32(p.Protocol), Identifier: p.Identifier,
		Local:         lsDescriptor(p.Local),
		OspfRouteType: uint32(p.OSPFRouteType),
	}
	if p.Prefix.IsValid() {
		pp.Prefix = p.Prefix.Addr().AsSlice()
		pp.PrefixLen = uint32(p.Prefix.Bits())
	}
	if attrs != nil {
		pp.PrefixMetric = attrs.PrefixMetric
		pp.PrefixAttrFlags = uint32(attrs.PrefixAttrFlags)
		// HasPrefixSID rather than "SID != 0": index 0 is a legal SID, and
		// the two cases mean different things to anyone reading the archive.
		if attrs.HasPrefixSID {
			pp.PrefixSid = attrs.PrefixSID
			pp.PrefixSidFlags = uint32(attrs.PrefixSIDFlags)
			pp.HasPrefixSid = true
		}
	}
	if len(p.Unknown) > 0 {
		pp.UnknownTlvs = map[uint32][]byte{}
		for k, v := range p.Unknown {
			pp.UnknownTlvs[uint32(k)] = v
		}
	}
	return pp
}

// lsLinkMessage builds one LsLink from a decoded NLRI and the BGP-LS
// Attribute that describes it. See lsNodeMessage for why attrs may be nil
// and why this is shared between the announced and withdrawn paths.
func lsLinkMessage(l bgp.LsLinkNLRI, attrs *bgp.LsAttrs) *vantagev1.LsLink {
	pl := &vantagev1.LsLink{
		Protocol: uint32(l.Protocol), Identifier: l.Identifier,
		Local: lsDescriptor(l.Local), Remote: lsDescriptor(l.Remote),
	}
	// TLV 258 (Link Local/Remote Identifiers) arrives in either of two
	// containers -- see bgp.LsLinkNLRI.HasLinkID and bgp.LsAttrs.HasLinkID
	// for which router was observed putting it where. The NLRI-side value
	// wins when both are present: there is no known case of a router
	// sending both, but the Link NLRI's own descriptor is the more
	// authoritative of the two containers if it ever happened, and the
	// attribute-side value is XRd 26.1.1's only source today. Both
	// link_local_id and link_remote_id feed ls_links' ORDER BY identity
	// tail, so leaving both at 0 (the previous behavior whenever only the
	// attribute-side carried them) silently collapsed two parallel
	// adjacencies between one router pair onto one sort tuple.
	switch {
	case l.HasLinkID:
		pl.LinkLocalId, pl.LinkRemoteId = l.LinkLocalID, l.LinkRemoteID
	case attrs != nil && attrs.HasLinkID:
		pl.LinkLocalId, pl.LinkRemoteId = attrs.LinkLocalID, attrs.LinkRemoteID
	}
	// Is4(), not IsValid(): netip.Addr.As4() panics on any valid-but-non-IPv4
	// address ("As4 called on IPv6 address"), and Is4() is the check that
	// actually rules that out -- IsValid() alone does not. decodeLsLink
	// already rejects a non-IPv4 value for these two fields at the wire
	// boundary (bgp/linkstate.go, TLV 259/260), but this guard is kept here
	// too: it is the exact call site a malformed BGP-LS UPDATE from one
	// monitored router was able to crash the whole collector process
	// through (server.go promises the collector never panics on wire
	// input), and a single upstream fix is one refactor away from a second
	// producer of LsLinkNLRI reopening the same panic.
	if l.LocalIfAddr.Is4() {
		b := l.LocalIfAddr.As4()
		pl.LocalIfaddr = b[:]
	}
	if l.RemoteIfAddr.Is4() {
		b := l.RemoteIfAddr.As4()
		pl.RemoteIfaddr = b[:]
	}
	if attrs != nil {
		// RFC 9085 §2.2.1 allows several Adjacency SID TLVs per link (e.g. a
		// protected/unprotected pair); AdjSIDs already keeps every one
		// decoded, so all of them are forwarded rather than just the last.
		for _, sid := range attrs.AdjSIDs {
			pl.AdjSids = append(pl.AdjSids, &vantagev1.LsAdjacencySid{
				Sid: sid.SID, Flags: uint32(sid.Flags), Weight: uint32(sid.Weight),
			})
		}
		pl.TeMetric = attrs.TEMetric
		pl.IgpMetric = attrs.IGPMetric
		pl.AdminGroup = attrs.AdminGroup
		pl.MaxBandwidth = attrs.MaxBandwidth
	}
	// See lsNodeMessage's identical comment -- this Link
	// NLRI's own unrecognized TLVs, distinct from the shared attribute's.
	if len(l.Unknown) > 0 {
		pl.UnknownTlvs = make(map[uint32][]byte, len(l.Unknown))
		for k, v := range l.Unknown {
			pl.UnknownTlvs[uint32(k)] = v
		}
	}
	return pl
}

// buildLsEvent assembles one LsEvent from a decoded BGP-LS UPDATE. Extracted
// from handleRM's IsLS branch so it can be unit tested directly against a
// hand-built *bgp.Update -- in particular so a withdrawn node/link can be
// driven through to is_withdraw without constructing real MP_UNREACH wire
// bytes just to reach this code.
func buildLsEvent(fam *vantagev1.Family, u *bgp.Update) *vantagev1.LsEvent {
	ls := &vantagev1.LsEvent{
		Family: fam, Attrs: u.Attrs,
		RawReach: u.RawReach, RawUnreach: u.RawUnreach,
		// An LS End-of-RIB (RFC 4724 §2 -- an MP_UNREACH
		// with AFI 16388/SAFI 71 and no NLRI) has no nodes, no links and no
		// raw bytes at all, so without this field it decoded "successfully"
		// into an envelope sink.RowsFor then had no reason to emit any row
		// for -- "the LS RIB finished converging" was unrepresentable in
		// ClickHouse even though u.EndOfRIB was already computed correctly.
		EndOfRib: u.EndOfRIB,
	}
	// Announced and withdrawn are separate slices on Update (mirroring
	// the Vpn/Evpn pairs). Both become
	// LsNode/LsLink messages; is_withdraw is what tells them apart, and
	// dropping the withdrawn set would make a topology that can only
	// ever grow edges.
	for _, n := range u.LsNodesWithdrawn {
		pn := lsNodeMessage(n, u.LsAttrs)
		pn.IsWithdraw = true
		ls.Nodes = append(ls.Nodes, pn)
	}
	for _, l := range u.LsLinksWithdrawn {
		pl := lsLinkMessage(l, u.LsAttrs)
		pl.IsWithdraw = true
		ls.Links = append(ls.Links, pl)
	}
	for _, p := range u.LsPrefixesWithdrawn {
		pp := lsPrefixMessage(p, u.LsAttrs)
		pp.IsWithdraw = true
		ls.Prefixes = append(ls.Prefixes, pp)
	}
	for _, n := range u.LsNodes {
		ls.Nodes = append(ls.Nodes, lsNodeMessage(n, u.LsAttrs))
	}
	for _, l := range u.LsLinks {
		ls.Links = append(ls.Links, lsLinkMessage(l, u.LsAttrs))
	}
	for _, p := range u.LsPrefixes {
		ls.Prefixes = append(ls.Prefixes, lsPrefixMessage(p, u.LsAttrs))
	}
	if u.LsAttrs != nil && len(u.LsAttrs.Unknown) > 0 {
		ls.UnknownTlvs = map[uint32][]byte{}
		for k, v := range u.LsAttrs.Unknown {
			ls.UnknownTlvs[uint32(k)] = v
		}
	}
	return ls
}
