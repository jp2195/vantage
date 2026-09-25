// query.go implements `vantage query`: the operator's read path into the
// archive, over either transport.
//
// # Two sources, one interface
//
// Every subcommand here is written against `source`, and there are two
// implementations. apiSource speaks HTTP to cmd/vantage-api; directSource
// wraps a *query.Q and talks to ClickHouse itself. The API is the DEFAULT,
// and that is the operationally important half: ClickHouse lives in the
// cluster and is not reachable from a laptop, so the operator with no DSN
// is the normal case rather than the degraded one. -dsn exists for the
// operator who is on the box, and for debugging a disagreement between the
// two.
//
// They must be indistinguishable. TestBothSourcesProduceIdenticalResults
// compares them field by field over a real database, because a divergence
// would be a bug in the wire types rather than a property of a transport --
// the API marshals query/'s own values, and apiSource decodes them straight
// back.
//
// # The inverse converters
//
// apiSource has to turn api.WireX back into query.X, which is the mirror of
// api/types.go's NewWireX. The wire form is not the Go form in four places,
// each of them a decision that file argues for at length, and each of them
// something this file has to undo exactly:
//
//	session_id, seq   strings on the wire, because a u64 past 2^53 loses
//	                  precision as a JSON number.
//	next_hop          null rather than "invalid IP" when a route has none.
//	origin_asn        null when the AS path is empty, because AS 0 is
//	                  reserved and "no path" is not "originated in AS 0".
//	label             null when absent, because label 0 is implicit-null
//	                  and a real value.
//
// A converter that got any of those wrong would produce a CLI that
// disagrees with the API it is a client of, in a way no HTTP-level test
// would notice. That is what the parity test is for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/jp2195/vantage/api"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
	"github.com/jp2195/vantage/sink"
)

// defaultAPI is where the daemon listens in the dev stack. It is a
// last-resort default: VANTAGE_API overrides it, and -api overrides that.
const defaultAPI = "http://127.0.0.1:9473"

// RouteQuery is the narrowing `vantage query routes` accepts, and the
// argument to source.Routes. It is the CLI's own type rather than
// query.RouteFilter because it fans out to three filters, not one.
type RouteQuery struct {
	Prefix string
	Covers string
	Router netip.Addr
	Peer   netip.Addr
	RIB    string

	// The three wide filters, carried verbatim to the query/ filter structs
	// and to the API's own parameters of the same names. Zero means "not
	// asked" for both AS fields and "" for the community, exactly as
	// query.RouteFilter spells it -- so an explicit AS 0 must never reach
	// here. cmdQueryRoutes refuses one while parsing flags, for the reason
	// api/handlers.go's params.asn returns a 400 for it.
	OriginASN  uint32
	ThroughASN uint32
	Community  string
}

// RouteFanout is one answer per family, the same shape /v1/routes returns.
// The three are separate slices rather than one merged list for the reason
// the contract gives: the families have different columns, and a merged
// list would have to be the union of all three with most fields empty.
type RouteFanout struct {
	Unicast []query.Route
	VPN     []query.VPNRoute
	EVPN    []query.EVPNRoute
}

// normalized returns f with every family a non-nil slice, which is what
// source promises its callers. See source's doc comment.
func (f RouteFanout) normalized() RouteFanout {
	return RouteFanout{
		Unicast: nonNil(f.Unicast),
		VPN:     nonNil(f.VPN),
		EVPN:    nonNil(f.EVPN),
	}
}

// RIBQuery is the argument to source.RIB: one (router, peer), one family,
// and the page size to walk it at.
//
// Limit is a PAGE size, not a total. Both implementations follow the cursor
// to the end of the walk, because an operator who typed `vantage query rib`
// asked for the RIB and not for its first thousand rows; the flag is there
// to tune the round trips, not to truncate the answer.
type RIBQuery struct {
	Router netip.Addr
	Peer   netip.Addr
	RIB    string
	Family string
	Limit  int
	// Collector pins the walk to one collector's view of Router. It is
	// required when more than one collector watches Router, since each has
	// its own session and a walk cannot span two.
	Collector string
}

// ribFamilies is the closed set -family accepts, mapped to nothing: it is
// the vocabulary, and the three /v1/rib paths are named after its members.
var ribFamilies = []string{"unicast", "vpn", "evpn"}

// LSQuery is the narrowing every `vantage query ls` subcommand accepts, and
// the argument to source.LSNodes, source.LSLinks and source.LSPrefixes.
//
// Area, ASN, Node, LocalNode, RemoteNode and State are STRINGS, not the
// *uint32/*uint64/query.LSState the query/ filter structs eventually want,
// for the reason cmdQueryRoutes' AS flags are strings rather than
// fs.Uint64: flag.Uint's zero default cannot distinguish "not asked" from
// an explicit zero, and zero is a real, common value in every one of these
// columns -- area 0 is the OSPF backbone and 85% of the archive's ls_nodes
// rows, and a node with no AS descriptor stores asn 0. "" means the flag
// was not given; any other text is parsed -- by parseOptU32, parseOptU64
// and parseLSStateFlag, the SAME functions cmdQueryLS calls before f.source
// dials and the *directSource filter builders below call again to build a
// query.LSNodeFilter/LSLinkFilter/LSPrefixFilter. One function used at both
// call sites is what makes "parsed once, in one place" true of the parsing
// rather than of any single call to it: cmdQueryLS's call exists only for
// its error, so that a malformed -area or an unknown -state is refused
// while flags are still being parsed, before the -dsn path -- which has no
// handler in front of it -- would otherwise dial ClickHouse and only then
// discover the flag was bad.
//
// Router and Peer are already netip.Addr, unlike RouteQuery's own fields:
// addresses have no "zero is real" problem (the invalid Addr is genuinely
// never a value a caller wants to filter by), so they are parsed once, by
// lsFlags.query, exactly the way RouteQuery's are.
//
// apiSource does not parse Area/ASN/Node/LocalNode/RemoteNode/State at all:
// it sends the string straight through as the query parameter text via
// setIf, which is what makes "" the correct spelling for "omit the
// parameter" on that side too, with no special-casing for "0" the way
// setASN needs for RouteQuery's numeric fields. The API's own params.optU32
// and params.lsState then parse it exactly as parseOptU32 and
// parseLSStateFlag do here, which is what keeps a malformed value refused
// identically on both transports without this file validating twice.
//
// The two transports agree ONLY below the daemon's configured max_page.
// None of lsNodeFilter, lsLinkFilter or lsPrefixFilter sets Limit on the
// query.LS*Filter it builds, so -dsn is uncapped -- an operator on the box
// gets the whole answer, deliberately, the same choice cmdQueryRoutes
// already makes for RouteFilter. The API caps every /v1/ls/* response at
// max_page and says so in meta (apiSource.do prints the truncated warning
// to stderr), so the two sides can disagree in ROW COUNT once a live
// answer exceeds that cap, even though neither is wrong. The parity test in
// query_test.go cannot exercise this boundary: its fixture is a handful of
// rows against a default max_page of 10000, so every case it runs sits well
// under any cap a real daemon would configure. Do not read
// TestBothSourcesAgreeOnLinkState's pass as proof the two transports agree
// past the cap -- it was never in a position to test that.
type LSQuery struct {
	Router, Peer netip.Addr
	RIB          string

	Protocol string
	Area     string
	ASN      string

	Node       string
	LocalNode  string
	RemoteNode string

	Prefix string
	Covers string

	State string
}

// parseOptU32 parses -area or -asn: "" is absent, and anything else is a
// decimal 0-4294967295 -- INCLUDING "0", which api/handlers.go's
// params.optU32 accepts for the identical reason (its own doc comment
// makes the case at length): absence is decided by whether the flag was
// given, never by whether the parsed number is zero, because zero is the
// single most common value in these columns.
func parseOptU32(name, raw string) (*uint32, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("-%s %q is not a 32-bit unsigned integer "+
			"(a decimal 0-4294967295)", name, raw)
	}
	v := uint32(n)
	return &v, nil
}

// parseOptU64 is parseOptU32 for the u64 identity parameters -- -node,
// -local-node and -remote-node. They are decimal text on this side of the
// interface for the reason node_key leaves query/ as one on the wire: the
// value is a cityHash64 that routinely exceeds 2^53, so flag's own numeric
// parsing would be the wrong type even before "absent vs. zero" enters into
// it, and api/handlers.go's params.optU64 reads the same text for the same
// reason.
func parseOptU64(name, raw string) (*uint64, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("-%s %q is not a node key (a decimal "+
			"0-18446744073709551615 -- it is a 64-bit hash and does not "+
			"survive a JSON number, so the API takes the same text)", name, raw)
	}
	return &n, nil
}

// parseLSProtocol parses -protocol through query.ParseProtocol itself, so
// "2" and "isis-l2" resolve exactly as api/handlers.go's params.protocol
// resolves them -- one parser, read by both transports, rather than a
// second table of registry names that could drift from query/protocol.go's.
// "" is absent (0), which every ls_* filter reads as "not asked."
func parseLSProtocol(raw string) (uint8, error) {
	if raw == "" {
		return 0, nil
	}
	id, err := query.ParseProtocol(raw)
	if err != nil {
		return 0, fmt.Errorf("-protocol: %w", err)
	}
	return id, nil
}

// parseLSStateFlag validates -state against the three names query.LSState
// documents, including "" for "not given" (which every ls_* filter reads as
// live). It is api/handlers.go's params.lsState on this side of the
// interface, and it exists as its own function -- rather than being folded
// into lsCommon below -- because cmdQueryLS calls it, alone, before
// f.source dials: THE reason the two transports are required to agree, an
// unrecognized state, is refused before any connection is attempted, on
// -dsn exactly as it is on the API. See LSQuery's own doc comment for why
// that early call and the filter builders' later one are the same function
// rather than two copies of the same rule.
func parseLSStateFlag(raw string) (query.LSState, error) {
	switch s := query.LSState(raw); s {
	case "", query.LSStateLive, query.LSStateWithdrawn, query.LSStateAny:
		return s, nil
	default:
		return "", fmt.Errorf("-state %q must be one of live, withdrawn or any", raw)
	}
}

// validateLSQuery runs every parse cmdQueryLS's three subcommands will
// eventually need against q's string fields, and discards the results: it
// exists ONLY for its error, so that a bad flag is refused while flags are
// still being parsed and before f.source has dialed anything. The -dsn path
// has no handler in front of it to do this refusing otherwise -- see
// TestQueryLSRefusesAnUnknownState, which is the reason this function
// exists rather than leaving parsing to the *directSource filter builders
// that would otherwise be the first code to run it.
func validateLSQuery(q LSQuery) error {
	if _, err := parseLSProtocol(q.Protocol); err != nil {
		return err
	}
	if _, err := parseOptU32("area", q.Area); err != nil {
		return err
	}
	if _, err := parseOptU32("asn", q.ASN); err != nil {
		return err
	}
	if _, err := parseOptU64("node", q.Node); err != nil {
		return err
	}
	if _, err := parseOptU64("local-node", q.LocalNode); err != nil {
		return err
	}
	if _, err := parseOptU64("remote-node", q.RemoteNode); err != nil {
		return err
	}
	if _, err := parseLSStateFlag(q.State); err != nil {
		return err
	}
	return nil
}

// lsCommon parses the four parameters every ls_* filter shares -- protocol,
// area, asn and state -- through the same functions validateLSQuery already
// ran for their error. Router, Peer, RIB, Prefix and Covers need no
// parsing: they are either already typed (Router, Peer) or read verbatim by
// query/'s own filters (RIB, Prefix, Covers).
func lsCommon(q LSQuery) (protocol uint8, area, asn *uint32, state query.LSState, err error) {
	if protocol, err = parseLSProtocol(q.Protocol); err != nil {
		return
	}
	if area, err = parseOptU32("area", q.Area); err != nil {
		return
	}
	if asn, err = parseOptU32("asn", q.ASN); err != nil {
		return
	}
	state, err = parseLSStateFlag(q.State)
	return
}

func lsNodeFilter(q LSQuery) (query.LSNodeFilter, error) {
	protocol, area, asn, state, err := lsCommon(q)
	if err != nil {
		return query.LSNodeFilter{}, err
	}
	node, err := parseOptU64("node", q.Node)
	if err != nil {
		return query.LSNodeFilter{}, err
	}
	return query.LSNodeFilter{
		Router: q.Router, Peer: q.Peer, RIB: q.RIB,
		Protocol: protocol, Area: area, ASN: asn, NodeKey: node,
		State: state,
	}, nil
}

func lsLinkFilter(q LSQuery) (query.LSLinkFilter, error) {
	protocol, area, asn, state, err := lsCommon(q)
	if err != nil {
		return query.LSLinkFilter{}, err
	}
	local, err := parseOptU64("local-node", q.LocalNode)
	if err != nil {
		return query.LSLinkFilter{}, err
	}
	remote, err := parseOptU64("remote-node", q.RemoteNode)
	if err != nil {
		return query.LSLinkFilter{}, err
	}
	return query.LSLinkFilter{
		Router: q.Router, Peer: q.Peer, RIB: q.RIB,
		Protocol: protocol, Area: area, ASN: asn,
		LocalNode: local, RemoteNode: remote,
		State: state,
	}, nil
}

func lsPrefixFilter(q LSQuery) (query.LSPrefixFilter, error) {
	protocol, area, asn, state, err := lsCommon(q)
	if err != nil {
		return query.LSPrefixFilter{}, err
	}
	node, err := parseOptU64("node", q.Node)
	if err != nil {
		return query.LSPrefixFilter{}, err
	}
	return query.LSPrefixFilter{
		Router: q.Router, Peer: q.Peer, RIB: q.RIB,
		Protocol: protocol, Area: area, ASN: asn, NodeKey: node,
		Prefix: q.Prefix, Covers: q.Covers,
		State: state,
	}, nil
}

// source is what every subcommand is written against. Neither
// implementation holds state between calls, so nothing here has a Close.
//
// Every slice a source returns is NON-NIL, empty rather than nil when there
// is nothing to report, and that is part of the interface rather than an
// accident of either implementation. apiSource gets it for free -- the
// contract types every array field as an array and api.arrayOf guarantees
// it, for the reason /v1/routes' three required keys exist: an empty answer
// has to be distinguishable from an absent one. query/ makes no such
// promise, so directSource adopts it explicitly via nonNil below.
//
// It was not free to discover. TestBothSourcesProduceIdenticalResults ran
// against an empty database and reported "direct: [] / api: []" -- two
// renderings of the same text, one nil and one not, which reflect.DeepEqual
// distinguishes and a person reading the failure cannot.
type source interface {
	Routers(context.Context) ([]query.Router, error)
	Peers(context.Context, netip.Addr) ([]query.Peer, error)
	Routes(context.Context, RouteQuery) (RouteFanout, error)
	RIB(context.Context, RIBQuery) (RouteFanout, error)
	LSNodes(context.Context, LSQuery) ([]query.LSNode, error)
	LSLinks(context.Context, LSQuery) ([]query.LSLink, error)
	LSPrefixes(context.Context, LSQuery) ([]query.LSPrefix, error)
}

// directSource answers from ClickHouse, with no daemon in the way.
type directSource struct {
	q *query.Q
	// warn receives the warnings the API would put in meta.warnings; nil
	// means os.Stderr, where apiSource prints them.
	warn io.Writer
}

// nonNil is api.arrayOf's counterpart on this side of the interface. See
// source's doc comment for why the two have to agree.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// warnStale prints the collector_stale warning the API would attach to an
// answer of n rows, with the API's own wording and threshold. stale reports
// whether any row came from a stale (collector, router); an answer with no
// rows costs no statement and never warns. Routers and peers need none of
// this: they carry the stale state in their own columns.
func (d *directSource) warnStale(ctx context.Context, n int, stale func(query.StaleSet) bool) error {
	if n == 0 {
		return nil
	}
	set, err := d.q.StalePairs(ctx)
	if err != nil {
		return err
	}
	w := d.warn
	if w == nil {
		w = os.Stderr
	}
	printWarnings(w, api.StaleWarning(stale(set), d.q.StaleAfter()))
	return nil
}

// warnStaleFanout is warnStale for a route answer's three families.
func (d *directSource) warnStaleFanout(ctx context.Context, f RouteFanout) error {
	return d.warnStale(ctx, len(f.Unicast)+len(f.VPN)+len(f.EVPN), func(set query.StaleSet) bool {
		return query.AnyStale(set, f.Unicast, query.Route.StaleKey) ||
			query.AnyStale(set, f.VPN, query.VPNRoute.StaleKey) ||
			query.AnyStale(set, f.EVPN, query.EVPNRoute.StaleKey)
	})
}

func (d *directSource) Routers(ctx context.Context) ([]query.Router, error) {
	rows, err := d.q.Routers(ctx)
	return nonNil(rows), err
}

func (d *directSource) Peers(ctx context.Context, router netip.Addr) ([]query.Peer, error) {
	rows, err := d.q.Peers(ctx, query.PeerFilter{Router: router})
	return nonNil(rows), err
}

func (d *directSource) Routes(ctx context.Context, rq RouteQuery) (RouteFanout, error) {
	var out RouteFanout
	var err error
	// Sequential, unlike the API handler's errgroup. This is one operator
	// waiting at a terminal rather than a server serving many, so the
	// latency saved is not worth a second concurrent implementation of a
	// fan-out to keep in agreement with the first.
	if out.Unicast, err = d.q.Routes(ctx, query.RouteFilter{
		Prefix: rq.Prefix, Covers: rq.Covers,
		Router: rq.Router, Peer: rq.Peer, RIB: rq.RIB,
		OriginASN: rq.OriginASN, ThroughASN: rq.ThroughASN,
		Community: rq.Community,
	}); err != nil {
		return RouteFanout{}, err
	}
	if out.VPN, err = d.q.VPNRoutes(ctx, query.VPNRouteFilter{
		Prefix: rq.Prefix, Covers: rq.Covers,
		Router: rq.Router, Peer: rq.Peer, RIB: rq.RIB,
		OriginASN: rq.OriginASN, ThroughASN: rq.ThroughASN,
		Community: rq.Community,
	}); err != nil {
		return RouteFanout{}, err
	}
	if out.EVPN, err = d.q.EVPNRoutes(ctx, query.EVPNRouteFilter{
		Prefix: rq.Prefix, Covers: rq.Covers,
		Router: rq.Router, Peer: rq.Peer, RIB: rq.RIB,
		OriginASN: rq.OriginASN, ThroughASN: rq.ThroughASN,
		Community: rq.Community,
	}); err != nil {
		return RouteFanout{}, err
	}
	out = out.normalized()
	if err := d.warnStaleFanout(ctx, out); err != nil {
		return RouteFanout{}, err
	}
	return out, nil
}

func (d *directSource) RIB(ctx context.Context, rq RIBQuery) (RouteFanout, error) {
	var out RouteFanout
	// The same pin api/ builds from collector=, so the two transports answer
	// a -collector walk identically.
	cur, err := d.q.RIBStart(ctx, rq.Router, rq.Peer, rq.RIB, rq.Collector)
	if err != nil {
		return RouteFanout{}, err
	}
	for {
		var err error
		var next *query.RIBCursor
		switch rq.Family {
		case "unicast":
			var page []query.Route
			page, next, err = d.q.RIBPageUnicast(ctx, rq.Router, rq.Peer, rq.RIB, cur, rq.Limit)
			out.Unicast = append(out.Unicast, page...)
		case "vpn":
			var page []query.VPNRoute
			page, next, err = d.q.RIBPageVPN(ctx, rq.Router, rq.Peer, rq.RIB, cur, rq.Limit)
			out.VPN = append(out.VPN, page...)
		case "evpn":
			var page []query.EVPNRoute
			page, next, err = d.q.RIBPageEVPN(ctx, rq.Router, rq.Peer, rq.RIB, cur, rq.Limit)
			out.EVPN = append(out.EVPN, page...)
		default:
			return RouteFanout{}, fmt.Errorf("unknown family %q, want one of %s",
				rq.Family, strings.Join(ribFamilies, ", "))
		}
		if err != nil {
			return RouteFanout{}, err
		}
		if next == nil {
			out = out.normalized()
			if err := d.warnStaleFanout(ctx, out); err != nil {
				return RouteFanout{}, err
			}
			return out, nil
		}
		cur = next
	}
}

func (d *directSource) LSNodes(ctx context.Context, q LSQuery) ([]query.LSNode, error) {
	f, err := lsNodeFilter(q)
	if err != nil {
		return nil, err
	}
	rows, err := d.q.LSNodes(ctx, f)
	if err != nil {
		return nil, err
	}
	if err := d.warnStale(ctx, len(rows), func(set query.StaleSet) bool {
		return query.AnyStale(set, rows, query.LSNode.StaleKey)
	}); err != nil {
		return nil, err
	}
	return nonNil(rows), nil
}

func (d *directSource) LSLinks(ctx context.Context, q LSQuery) ([]query.LSLink, error) {
	f, err := lsLinkFilter(q)
	if err != nil {
		return nil, err
	}
	rows, err := d.q.LSLinks(ctx, f)
	if err != nil {
		return nil, err
	}
	if err := d.warnStale(ctx, len(rows), func(set query.StaleSet) bool {
		return query.AnyStale(set, rows, query.LSLink.StaleKey)
	}); err != nil {
		return nil, err
	}
	return nonNil(rows), nil
}

func (d *directSource) LSPrefixes(ctx context.Context, q LSQuery) ([]query.LSPrefix, error) {
	f, err := lsPrefixFilter(q)
	if err != nil {
		return nil, err
	}
	rows, err := d.q.LSPrefixes(ctx, f)
	if err != nil {
		return nil, err
	}
	if err := d.warnStale(ctx, len(rows), func(set query.StaleSet) bool {
		return query.AnyStale(set, rows, query.LSPrefix.StaleKey)
	}); err != nil {
		return nil, err
	}
	return nonNil(rows), nil
}

// apiSource answers over HTTP, through cmd/vantage-api.
type apiSource struct {
	base  string
	token string
	// client is nil in the zero value and defaultAPIClient is used
	// instead; a test sets it to reach a server of its own.
	client *http.Client
}

// apiRequestTimeout bounds ONE request to vantage-api, from dial to the last
// byte of the body. It is per request rather than per command because a RIB
// walk is one request per page and may run far longer than any one of them;
// what it catches is a daemon (or anything between) that accepts the
// connection and then never answers, which with no bound at all hangs the
// command until someone kills it. Two minutes is well above the slowest
// single page the API is measured to serve.
const apiRequestTimeout = 2 * time.Minute

// defaultAPIClient is what an apiSource with no client of its own uses. It is
// a variable only so a test can shorten the bound; http.DefaultClient, which
// this replaced, has no timeout at all.
var defaultAPIClient = newAPIClient(apiRequestTimeout)

func newAPIClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// envelope is the response shape every path shares.
type envelope struct {
	Data json.RawMessage `json:"data"`
	Meta api.Meta        `json:"meta"`
}

// do issues one authenticated GET and returns the decoded envelope.
//
// A non-200 is decoded as the contract's ErrorResponse and its MESSAGE is
// what surfaces, not the status alone. The API's 400s say which parameter
// was wrong and what the rule is; throwing that away for "unexpected status
// 400" would make this CLI strictly worse to use than curl, which is the
// tool it exists to replace.
//
// meta.warnings go to stderr rather than being dropped or mixed into
// stdout. session_dumping is the field that says an answer is partial, and
// a CLI that swallowed it would present a half-loaded RIB as the whole one
// -- while stdout stays clean for a pipe.
func (a *apiSource) do(ctx context.Context, path string, q url.Values) (envelope, error) {
	u := strings.TrimRight(a.base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	client := a.client
	if client == nil {
		client = defaultAPIClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bounded, because this is an error path reading from a server
		// this process does not control, and an unbounded read of an error
		// body is a way to be hung by one.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var er api.ErrorResponse
		if json.Unmarshal(body, &er) == nil && er.Error.Message != "" {
			// Escaped in full, newlines included: this is server text on
			// its way to a terminal, and a newline in it would print as a
			// line of this CLI's own.
			return envelope{}, fmt.Errorf("%s: %d %s: %s",
				path, resp.StatusCode, escapeControl(er.Error.Code, false),
				escapeControl(er.Error.Message, false))
		}
		return envelope{}, fmt.Errorf("%s: %d %s", path, resp.StatusCode,
			http.StatusText(resp.StatusCode))
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return envelope{}, fmt.Errorf("%s: decode: %w", path, err)
	}
	printWarnings(os.Stderr, env.Meta.Warnings)
	return env, nil
}

// getJSON is do for the six paths THIS FILE fetches whole -- routers, peers,
// routes and the three ls_* resources -- not for every cursorless path the
// contract has. Eleven of api/openapi.yaml's fourteen paths carry no cursor
// (the three /v1/rib/* keyset walks are the exception), and one of those
// eleven is /v1/openapi.yaml, the document itself, which no source method
// fetches. Six is a count of the call sites below, not a property of /v1.
func (a *apiSource) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	env, err := a.do(ctx, path, q)
	if err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

func (a *apiSource) Routers(ctx context.Context) ([]query.Router, error) {
	var wire []api.WireRouter
	if err := a.getJSON(ctx, "/v1/routers", nil, &wire); err != nil {
		return nil, err
	}
	out := make([]query.Router, len(wire))
	for i, w := range wire {
		r, err := routerFromWire(w)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func (a *apiSource) Peers(ctx context.Context, router netip.Addr) ([]query.Peer, error) {
	q := url.Values{}
	if router.IsValid() {
		q.Set("router", router.String())
	}
	var wire []api.WirePeer
	if err := a.getJSON(ctx, "/v1/peers", q, &wire); err != nil {
		return nil, err
	}
	out := make([]query.Peer, len(wire))
	for i, w := range wire {
		p, err := peerFromWire(w)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func (a *apiSource) Routes(ctx context.Context, rq RouteQuery) (RouteFanout, error) {
	q := url.Values{}
	setIf(q, "prefix", rq.Prefix)
	setIf(q, "covers", rq.Covers)
	setIf(q, "rib", rq.RIB)
	setAddr(q, "router", rq.Router)
	setAddr(q, "peer", rq.Peer)
	setASN(q, "origin_asn", rq.OriginASN)
	setASN(q, "through_asn", rq.ThroughASN)
	setIf(q, "community", rq.Community)
	var wire api.WireRouteFanout
	if err := a.getJSON(ctx, "/v1/routes", q, &wire); err != nil {
		return RouteFanout{}, err
	}
	return fanoutFromWire(wire)
}

func (a *apiSource) RIB(ctx context.Context, rq RIBQuery) (RouteFanout, error) {
	path := "/v1/rib/" + rq.Family
	var out RouteFanout
	cursor := ""
	for {
		q := url.Values{}
		setAddr(q, "router", rq.Router)
		setAddr(q, "peer", rq.Peer)
		setIf(q, "rib", rq.RIB)
		// Sent on every page, not just the first: the API accepts a
		// collector= that agrees with the cursor's own, and refuses one that
		// contradicts it, so repeating it costs nothing and catches a cursor
		// that somehow belongs to another collector's walk.
		setIf(q, "collector", rq.Collector)
		setIf(q, "cursor", cursor)
		if rq.Limit > 0 {
			q.Set("limit", strconv.Itoa(rq.Limit))
		}

		// ribPage rather than getJSON: this is the only path that needs
		// meta.next_cursor as well as data, and following the cursor to the
		// end is what makes `vantage query rib` answer with the RIB rather
		// than with its first page.
		next, err := a.ribPage(ctx, path, q, rq.Family, &out)
		if err != nil {
			return RouteFanout{}, err
		}
		if next == "" {
			return out.normalized(), nil
		}
		cursor = next
	}
}

// ribPage fetches one page, appends it to out, and returns the next cursor
// or "" when the walk is done.
func (a *apiSource) ribPage(ctx context.Context, path string, q url.Values,
	family string, out *RouteFanout) (string, error) {
	env, err := a.do(ctx, path, q)
	if err != nil {
		return "", err
	}
	switch family {
	case "unicast":
		var wire []api.WireUnicastRoute
		if err := json.Unmarshal(env.Data, &wire); err != nil {
			return "", err
		}
		for _, w := range wire {
			r, err := unicastFromWire(w)
			if err != nil {
				return "", err
			}
			out.Unicast = append(out.Unicast, r)
		}
	case "vpn":
		var wire []api.WireVPNRoute
		if err := json.Unmarshal(env.Data, &wire); err != nil {
			return "", err
		}
		for _, w := range wire {
			r, err := vpnFromWire(w)
			if err != nil {
				return "", err
			}
			out.VPN = append(out.VPN, r)
		}
	case "evpn":
		var wire []api.WireEVPNRoute
		if err := json.Unmarshal(env.Data, &wire); err != nil {
			return "", err
		}
		for _, w := range wire {
			r, err := evpnFromWire(w)
			if err != nil {
				return "", err
			}
			out.EVPN = append(out.EVPN, r)
		}
	default:
		return "", fmt.Errorf("unknown family %q, want one of %s",
			family, strings.Join(ribFamilies, ", "))
	}
	if env.Meta.NextCursor == nil {
		return "", nil
	}
	return *env.Meta.NextCursor, nil
}

// lsParams sets the parameters every /v1/ls/* path shares -- router, peer,
// rib, protocol, area, asn and state. Callers add their own resource-
// specific key parameters (node, local_node/remote_node, prefix/covers) on
// top of what this returns.
//
// Every value here is sent as the SAME text LSQuery already carries: "" is
// omitted by setIf, and anything else -- "0" included -- is sent verbatim.
// That is what makes area=0 arrive on the wire as exactly the question a
// caller asked, with no special-casing for zero the way setASN needs for
// RouteQuery's numeric fields: the zero-vs-absent distinction already lives
// entirely in whether the string is empty, so there is nothing left for
// this function to get wrong.
func lsParams(q LSQuery) url.Values {
	v := url.Values{}
	setAddr(v, "router", q.Router)
	setAddr(v, "peer", q.Peer)
	setIf(v, "rib", q.RIB)
	setIf(v, "protocol", q.Protocol)
	setIf(v, "area", q.Area)
	setIf(v, "asn", q.ASN)
	setIf(v, "state", q.State)
	return v
}

func (a *apiSource) LSNodes(ctx context.Context, q LSQuery) ([]query.LSNode, error) {
	v := lsParams(q)
	setIf(v, "node", q.Node)
	var wire []api.WireLSNode
	if err := a.getJSON(ctx, "/v1/ls/nodes", v, &wire); err != nil {
		return nil, err
	}
	out := make([]query.LSNode, len(wire))
	for i, w := range wire {
		n, err := lsNodeFromWire(w)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func (a *apiSource) LSLinks(ctx context.Context, q LSQuery) ([]query.LSLink, error) {
	v := lsParams(q)
	setIf(v, "local_node", q.LocalNode)
	setIf(v, "remote_node", q.RemoteNode)
	var wire []api.WireLSLink
	if err := a.getJSON(ctx, "/v1/ls/links", v, &wire); err != nil {
		return nil, err
	}
	out := make([]query.LSLink, len(wire))
	for i, w := range wire {
		l, err := lsLinkFromWire(w)
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}

func (a *apiSource) LSPrefixes(ctx context.Context, q LSQuery) ([]query.LSPrefix, error) {
	v := lsParams(q)
	setIf(v, "node", q.Node)
	setIf(v, "prefix", q.Prefix)
	setIf(v, "covers", q.Covers)
	var wire []api.WireLSPrefix
	if err := a.getJSON(ctx, "/v1/ls/prefixes", v, &wire); err != nil {
		return nil, err
	}
	out := make([]query.LSPrefix, len(wire))
	for i, w := range wire {
		p, err := lsPrefixFromWire(w)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func setIf(q url.Values, name, v string) {
	if v != "" {
		q.Set(name, v)
	}
}

func setAddr(q url.Values, name string, a netip.Addr) {
	if a.IsValid() {
		q.Set(name, a.String())
	}
}

// setASN is setIf for an AS number. Zero is omitted rather than sent, which
// is the same "not asked" this side already spells with "" and the invalid
// Addr -- and sending it would be worse than useless, since the contract
// answers origin_asn=0 with a 400 rather than with the unfiltered result the
// omission produces. An explicit 0 never reaches here: parseASNFlag refuses
// it while flags are still being parsed.
func setASN(q url.Values, name string, n uint32) {
	if n != 0 {
		q.Set(name, strconv.FormatUint(uint64(n), 10))
	}
}

// ---- the inverse converters ----

// addrFromText parses a plain address field. An empty string is the invalid
// Addr rather than an error: api.addrText renders the zero Addr as "" for
// fields the contract types as a plain string, so "" is how "no address"
// arrives and it is not malformed.
func addrFromText(s string) (netip.Addr, error) {
	if s == "" {
		return netip.Addr{}, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("address %q: %w", s, err)
	}
	return a.Unmap(), nil
}

// addrFromNullable is the inverse of api.addrOrNull: null means the route
// has no next hop, which is the ordinary case on a withdrawal.
func addrFromNullable(s *string) (netip.Addr, error) {
	if s == nil {
		return netip.Addr{}, nil
	}
	return addrFromText(*s)
}

// u64FromText is the inverse of api.u64. The error is not decorative: a
// session_id that failed to parse would otherwise become 0, which is a
// value no collector's clock issues and which would silently compare
// unequal to every real session.
func u64FromText(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a base-10 uint64: %w", s, err)
	}
	return n, nil
}

// originFromNullable is the inverse of api.originASN. null means the AS
// path was empty, and query's own originASN returns 0 for that case, so 0
// is the faithful value rather than a placeholder.
func originFromNullable(v *uint32) uint32 {
	if v == nil {
		return 0
	}
	return *v
}

func routerFromWire(w api.WireRouter) (query.Router, error) {
	ip, err := addrFromText(w.IP)
	if err != nil {
		return query.Router{}, err
	}
	sid, err := u64FromText(w.SessionID)
	if err != nil {
		return query.Router{}, err
	}
	return query.Router{
		SysName:       w.SysName,
		IP:            ip,
		Collector:     w.Collector,
		SessionID:     sid,
		PeersUp:       w.PeersUp,
		PeersDown:     w.PeersDown,
		PeersViewLost: w.PeersViewLost,
		PeersStale:    w.PeersStale,
		LastSeen:      w.LastSeen,
	}, nil
}

func peerFromWire(w api.WirePeer) (query.Peer, error) {
	router, err := addrFromText(w.RouterIP)
	if err != nil {
		return query.Peer{}, err
	}
	peer, err := addrFromText(w.PeerIP)
	if err != nil {
		return query.Peer{}, err
	}
	sid, err := u64FromText(w.SessionID)
	if err != nil {
		return query.Peer{}, err
	}
	return query.Peer{
		RouterIP:   router,
		PeerIP:     peer,
		Collector:  w.Collector,
		RIB:        w.RIB,
		ASN:        w.ASN,
		State:      w.State,
		DumpStates: w.DumpStates,
		SessionID:  sid,
		Routes:     w.Routes,
		// The session facts. hold_time is nullable on the wire precisely so
		// that a negotiated 0 stays distinct from "no OPEN was observed"; the
		// pointer being non-nil IS the flag, and collapsing it to a plain 0
		// here would put that distinction back exactly where the API went to
		// the trouble of removing it.
		HoldTime:        holdTimeValue(w.HoldTime),
		HoldTimeSeen:    w.HoldTime != nil,
		MPFamilies:      familiesFromWire(w.MPFamilies),
		AddPathFamilies: familiesFromWire(w.AddPathFamilies),
		SysDescr:        w.SysDescr,
		// Null on the wire is "this session never came up", and the zero
		// instant is what query.Peer carries for it -- so a nil pointer
		// decodes to the zero value rather than to a date. The parity test
		// is what found this missing: the direct path read up_since from
		// ClickHouse while this one silently dropped it, which is the
		// value-loaded-but-not-plumbed gap that test exists for.
		UpSince: upSinceValue(w.UpSince),
	}, nil
}

func upSinceValue(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func holdTimeValue(p *uint16) uint16 {
	if p == nil {
		return 0
	}
	return *p
}

// familiesFromWire keeps a decoded family list non-nil, matching what
// query.Peers itself produces. nil and []string{} print identically under
// %+v and differ under reflect.DeepEqual, which is how the direct-vs-API
// parity test reports this: "peers disagree" above two structs that are
// character-for-character the same.
func familiesFromWire(f []string) []string {
	if f == nil {
		return []string{}
	}
	return f
}

// commonFromWire decodes the fields all three route types share, so the
// three converters below cannot drift on them.
func commonFromWire(c api.WireRouteCommon) (routerIP, peerIP, nextHop netip.Addr, err error) {
	if routerIP, err = addrFromText(c.RouterIP); err != nil {
		return
	}
	if peerIP, err = addrFromText(c.PeerIP); err != nil {
		return
	}
	nextHop, err = addrFromNullable(c.NextHop)
	return
}

func unicastFromWire(w api.WireUnicastRoute) (query.Route, error) {
	routerIP, peerIP, nextHop, err := commonFromWire(w.WireRouteCommon)
	if err != nil {
		return query.Route{}, err
	}
	return query.Route{
		RouterSysName:    w.RouterSysName,
		RouterIP:         routerIP,
		PeerIP:           peerIP,
		NextHop:          nextHop,
		Collector:        w.Collector,
		RIB:              w.RIB,
		Family:           w.Family,
		Prefix:           w.Prefix,
		PathID:           w.PathID,
		ASPath:           w.ASPath,
		OriginASN:        originFromNullable(w.OriginASN),
		MED:              w.MED,
		LocalPref:        w.LocalPref,
		Communities:      w.Communities,
		LargeCommunities: w.LargeCommunities,
		ExtCommunities:   w.ExtCommunities,
		RouteTargets:     w.RouteTargets,
		DumpState:        w.DumpState,
	}, nil
}

func vpnFromWire(w api.WireVPNRoute) (query.VPNRoute, error) {
	routerIP, peerIP, nextHop, err := commonFromWire(w.WireRouteCommon)
	if err != nil {
		return query.VPNRoute{}, err
	}
	var label uint32
	if w.Label != nil {
		label = *w.Label
	}
	return query.VPNRoute{
		RouterSysName: w.RouterSysName,
		RouterIP:      routerIP,
		PeerIP:        peerIP,
		NextHop:       nextHop,
		Collector:     w.Collector,
		RIB:           w.RIB,
		Family:        w.Family,
		RD:            w.RD,
		Prefix:        w.Prefix,
		PathID:        w.PathID,
		Label:         label,
		// HasLabel is the presence of the JSON null, not the value: label 0
		// is the real implicit-null label an MPLS router advertises, so
		// `label: 0` and `label: null` are two different facts.
		HasLabel:         w.Label != nil,
		RouteTargets:     w.RouteTargets,
		ASPath:           w.ASPath,
		OriginASN:        originFromNullable(w.OriginASN),
		MED:              w.MED,
		LocalPref:        w.LocalPref,
		Communities:      w.Communities,
		LargeCommunities: w.LargeCommunities,
		ExtCommunities:   w.ExtCommunities,
		DumpState:        w.DumpState,
	}, nil
}

func evpnFromWire(w api.WireEVPNRoute) (query.EVPNRoute, error) {
	routerIP, peerIP, nextHop, err := commonFromWire(w.WireRouteCommon)
	if err != nil {
		return query.EVPNRoute{}, err
	}
	return query.EVPNRoute{
		RouterSysName:    w.RouterSysName,
		RouterIP:         routerIP,
		PeerIP:           peerIP,
		NextHop:          nextHop,
		Collector:        w.Collector,
		RIB:              w.RIB,
		RouteType:        w.RouteType,
		RD:               w.RD,
		Prefix:           w.Prefix,
		MAC:              w.MAC,
		IP:               w.IP,
		GatewayIP:        w.GatewayIP,
		EthernetTag:      w.EthernetTag,
		ESI:              w.ESI,
		PathID:           w.PathID,
		Labels:           w.Labels,
		RouteTargets:     w.RouteTargets,
		ASPath:           w.ASPath,
		OriginASN:        originFromNullable(w.OriginASN),
		MED:              w.MED,
		LocalPref:        w.LocalPref,
		Communities:      w.Communities,
		LargeCommunities: w.LargeCommunities,
		ExtCommunities:   w.ExtCommunities,
		DumpState:        w.DumpState,
	}, nil
}

// lsCommonFromWire decodes the fields WireLSNode and WireLSPrefix share
// through api.WireLSCommon, so the two converters below cannot drift on
// them the way commonFromWire keeps the three route converters in step.
func lsCommonFromWire(c api.WireLSCommon) (routerIP, peerIP netip.Addr, identifier, nodeKey uint64, err error) {
	if routerIP, err = addrFromText(c.RouterIP); err != nil {
		return
	}
	if peerIP, err = addrFromText(c.PeerIP); err != nil {
		return
	}
	if identifier, err = u64FromText(c.Identifier); err != nil {
		return
	}
	nodeKey, err = u64FromText(c.NodeKey)
	return
}

// srAlgorithmsFromWire is the inverse of api.wireBytes: the wire carries
// []int because encoding/json marshals []uint8 as a base64 STRING rather
// than an array (see wireBytes' own doc comment), so this file has to
// convert back to []uint8 rather than assigning w.SRAlgorithms directly.
func srAlgorithmsFromWire(in []int) []uint8 {
	out := make([]uint8, len(in))
	for i, v := range in {
		out[i] = uint8(v)
	}
	return out
}

func lsNodeFromWire(w api.WireLSNode) (query.LSNode, error) {
	routerIP, peerIP, identifier, nodeKey, err := lsCommonFromWire(w.WireLSCommon)
	if err != nil {
		return query.LSNode{}, err
	}
	return query.LSNode{
		RouterSysName: w.RouterSysName,
		RouterIP:      routerIP,
		PeerIP:        peerIP,
		Collector:     w.Collector,
		RIB:           w.RIB,
		Protocol:      w.Protocol,
		Identifier:    identifier,
		ASN:           w.ASN,
		BGPLSID:       w.BGPLSID,
		Area:          w.Area,
		RouterID:      w.RouterID,
		NodeKey:       nodeKey,
		RouterIDv4:    w.RouterIDv4,
		Name:          w.Name,
		SRGBBase:      w.SRGBBase,
		SRGBSize:      w.SRGBSize,
		SRLBBase:      w.SRLBBase,
		SRLBSize:      w.SRLBSize,
		SRAlgorithms:  srAlgorithmsFromWire(w.SRAlgorithms),
		IsWithdraw:    w.IsWithdraw,
		DumpState:     w.DumpState,
	}, nil
}

func lsPrefixFromWire(w api.WireLSPrefix) (query.LSPrefix, error) {
	routerIP, peerIP, identifier, nodeKey, err := lsCommonFromWire(w.WireLSCommon)
	if err != nil {
		return query.LSPrefix{}, err
	}
	return query.LSPrefix{
		RouterSysName:  w.RouterSysName,
		RouterIP:       routerIP,
		PeerIP:         peerIP,
		Collector:      w.Collector,
		RIB:            w.RIB,
		Protocol:       w.Protocol,
		Identifier:     identifier,
		ASN:            w.ASN,
		BGPLSID:        w.BGPLSID,
		Area:           w.Area,
		RouterID:       w.RouterID,
		NodeKey:        nodeKey,
		Prefix:         w.Prefix,
		PrefixSID:      w.PrefixSID,
		PrefixSIDFlags: w.PrefixSIDFlags,
		HasPrefixSID:   w.HasPrefixSID,
		PrefixMetric:   w.PrefixMetric,
		OSPFRouteType:  w.OSPFRouteType,
		IsWithdraw:     w.IsWithdraw,
		DumpState:      w.DumpState,
	}, nil
}

// lsEndpointFromWire is the inverse of api's newWireLSEndpoint, used for
// both the local and the remote end of a WireLSLink -- one function, used
// twice, for the reason newWireLSEndpoint's own doc comment gives: two
// hand-written converters is how the remote end ends up carrying the local
// end's parsing bug and nothing catches it.
func lsEndpointFromWire(w api.WireLSEndpoint) (query.LSEndpoint, error) {
	nodeKey, err := u64FromText(w.NodeKey)
	if err != nil {
		return query.LSEndpoint{}, err
	}
	return query.LSEndpoint{
		ASN:         w.ASN,
		BGPLSID:     w.BGPLSID,
		Area:        w.Area,
		RouterID:    w.RouterID,
		NodeKey:     nodeKey,
		IfAddr:      w.IfAddr,
		InterfaceID: w.InterfaceID,
		Label:       w.Label,
		LabelSource: w.LabelSource,
	}, nil
}

func lsLinkFromWire(w api.WireLSLink) (query.LSLink, error) {
	routerIP, err := addrFromText(w.RouterIP)
	if err != nil {
		return query.LSLink{}, err
	}
	peerIP, err := addrFromText(w.PeerIP)
	if err != nil {
		return query.LSLink{}, err
	}
	identifier, err := u64FromText(w.Identifier)
	if err != nil {
		return query.LSLink{}, err
	}
	local, err := lsEndpointFromWire(w.Local)
	if err != nil {
		return query.LSLink{}, err
	}
	remote, err := lsEndpointFromWire(w.Remote)
	if err != nil {
		return query.LSLink{}, err
	}
	return query.LSLink{
		RouterSysName: w.RouterSysName,
		RouterIP:      routerIP,
		PeerIP:        peerIP,
		Collector:     w.Collector,
		RIB:           w.RIB,
		Protocol:      w.Protocol,
		Identifier:    identifier,
		Local:         local,
		Remote:        remote,
		AdjSIDs:       w.AdjSIDs,
		TEMetric:      w.TEMetric,
		IGPMetric:     w.IGPMetric,
		AdminGroup:    w.AdminGroup,
		MaxBandwidth:  w.MaxBandwidth,
		IsWithdraw:    w.IsWithdraw,
		DumpState:     w.DumpState,
	}, nil
}

func fanoutFromWire(w api.WireRouteFanout) (RouteFanout, error) {
	var out RouteFanout
	for _, r := range w.Unicast {
		v, err := unicastFromWire(r)
		if err != nil {
			return RouteFanout{}, err
		}
		out.Unicast = append(out.Unicast, v)
	}
	for _, r := range w.VPN {
		v, err := vpnFromWire(r)
		if err != nil {
			return RouteFanout{}, err
		}
		out.VPN = append(out.VPN, v)
	}
	for _, r := range w.EVPN {
		v, err := evpnFromWire(r)
		if err != nil {
			return RouteFanout{}, err
		}
		out.EVPN = append(out.EVPN, v)
	}
	return out.normalized(), nil
}

// ---- the subcommands ----

// queryFlags are the five flags every `vantage query` subcommand shares.
type queryFlags struct {
	api    *string
	token  *string
	dsn    *string
	output *string
	// staleAfter is the -dsn path's stale threshold. Through the API the
	// daemon's own stale_after applies; both default to
	// query.DefaultStaleAfter, so the two transports agree unless an operator
	// changes one of them.
	staleAfter *time.Duration
	// fs is the set these flags were declared on, so source can tell a
	// flag given on the command line from its default.
	fs *flag.FlagSet
	// warn is where source reports a flag it ignores; nil means os.Stderr.
	warn io.Writer
}

// register declares the shared flags on fs. The defaults come from the
// environment so that an operator sets VANTAGE_API_TOKEN once per shell
// instead of pasting a credential into every command line -- where it would
// land in the shell history and in the process table, which is the same
// argument natsFlag makes about -nats.
func registerQueryFlags(fs *flag.FlagSet) queryFlags {
	return queryFlags{
		api: fs.String("api", envOr("VANTAGE_API", defaultAPI),
			"vantage-api base URL (env VANTAGE_API)"),
		token: fs.String("token", os.Getenv("VANTAGE_API_TOKEN"),
			"bearer token (env VANTAGE_API_TOKEN)"),
		dsn: fs.String("dsn", "",
			"ClickHouse DSN; queries the database directly instead of the API"),
		output: fs.String("o", "table", "output format: table or json"),
		staleAfter: fs.Duration("stale-after", query.DefaultStaleAfter,
			"with -dsn, how long a collector may go unheard before its peers read stale"),
		fs: fs,
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// source builds the transport the flags select, and closes over what has to
// be cleaned up.
//
// -dsn wins when set, and the API is used otherwise. The direct path dials
// through sink.NewClickHouse rather than clickhouse.Open for the reason
// cmd/vantage-api does: that constructor owns the DSN redaction and the
// schema-version check, and a CLI that skipped the latter would print
// confident answers out of a database whose columns had moved.
func (f queryFlags) source(ctx context.Context) (source, func(), error) {
	switch *f.output {
	case "table", "json":
	default:
		return nil, nil, fmt.Errorf("unknown output format %q: -o takes table or json",
			*f.output)
	}
	if *f.dsn == "" {
		if *f.token == "" {
			// A vantage-api instance configured with auth.mode: none would
			// answer without one, but this flag has no way to ask a daemon
			// which mode it is running before deciding whether to send a
			// token -- doing that would be a second network round trip on
			// every invocation, for a case most deployments are not in --
			// so -token (or -dsn, to skip the API and its auth entirely)
			// stays required here regardless of what the target turns out
			// to accept.
			return nil, nil, errors.New("no token: set VANTAGE_API_TOKEN or pass " +
				"-token, or use -dsn to query ClickHouse directly. If the target " +
				"vantage-api is configured with auth.mode: none, a token is not " +
				"required, but this command has no way to know that in advance")
		}
		f.fs.Visit(func(fl *flag.Flag) {
			if fl.Name == "stale-after" {
				w := f.warn
				if w == nil {
					w = os.Stderr
				}
				fmt.Fprintln(w, "vantage: warning: -stale-after applies only with -dsn; "+
					"the API uses its own stale_after")
			}
		})
		return &apiSource{base: *f.api, token: *f.token}, func() {}, nil
	}
	dsn := secret.NewClickHouseDSN(*f.dsn)
	ch, err := sink.NewClickHouse(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect clickhouse %s: %w", dsn, err)
	}
	q, err := query.New(ch.Conn(), ch.DB())
	if err != nil {
		ch.Close()
		return nil, nil, err
	}
	if q, err = q.WithStaleAfter(*f.staleAfter); err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("-stale-after: %w", err)
	}
	return &directSource{q: q}, func() { ch.Close() }, nil
}

func cmdQuery(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vantage query (routers|peers|routes|rib|ls) [flags]")
	}
	switch args[0] {
	case "routers":
		return cmdQueryRouters(args[1:])
	case "peers":
		return cmdQueryPeers(args[1:])
	case "routes":
		return cmdQueryRoutes(args[1:])
	case "rib":
		return cmdQueryRIB(args[1:])
	case "ls":
		return cmdQueryLS(args[1:])
	default:
		return fmt.Errorf("unknown query subcommand %q: want routers, peers, "+
			"routes, rib or ls", args[0])
	}
}

func cmdQueryRouters(args []string) error {
	fs := flag.NewFlagSet("query routers", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := src.Routers(ctx)
	if err != nil {
		return err
	}
	if *f.output == "json" {
		return emitJSON(mapSlice(rows, api.NewWireRouter))
	}
	return table(func(w io.Writer) {
		// LOST is peers the collector stopped being able to see, which is
		// not the same as peers the router reported down -- see
		// query.Router.PeersViewLost. It is a column rather than a footnote
		// because a router whose collector died has every peer in it, and
		// without it that router reads as having no peers at all.
		// STALE is peers whose collector has not been heard from within the
		// stale threshold -- see query.Router.PeersStale. Their routes are
		// still served, which is exactly why they need their own column.
		fmt.Fprintln(w, "SYSNAME\tIP\tCOLLECTOR\tSESSION\tUP\tDOWN\tLOST\tSTALE\tLAST SEEN")
		for _, r := range rows {
			rowf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
				orDash(r.SysName), r.IP, r.Collector, r.SessionID,
				r.PeersUp, r.PeersDown, r.PeersViewLost, r.PeersStale,
				r.LastSeen.UTC().Format(time.RFC3339))
		}
	})
}

func cmdQueryPeers(args []string) error {
	fs := flag.NewFlagSet("query peers", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	router := fs.String("router", "", "restrict to one router IP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr, err := addrFromText(*router)
	if err != nil {
		return fmt.Errorf("-router: %w", err)
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := src.Peers(ctx, addr)
	if err != nil {
		return err
	}
	if *f.output == "json" {
		return emitJSON(mapSlice(rows, api.NewWirePeer))
	}
	return table(func(w io.Writer) {
		fmt.Fprintln(w, "ROUTER\tPEER\tRIB\tASN\tSTATE\tROUTES\tDUMP STATES")
		for _, p := range rows {
			rowf(w, "%s\t%s\t%s\t%d\t%s\t%d\t%s\n",
				p.RouterIP, p.PeerIP, p.RIB, p.ASN, p.State, p.Routes,
				orDash(joinDumpStates(p.DumpStates)))
		}
	})
}

// parseASNFlag parses -origin-asn or -through-asn. It is api/handlers.go's
// params.asn on this side of the interface, and it is deliberately the same
// three rules rather than a looser CLI-flavored version of them: the -dsn
// transport has no handler in front of it, so whatever this function lets
// through is what query/ receives, and the two transports are required to
// answer the same question identically.
//
//   - Empty is 0, meaning "not asked" -- the value query/'s filter structs
//     already spell that way.
//   - An explicit 0 is REFUSED. AS 0 is reserved by RFC 7607 and originates
//     nothing, so the honest answers are "that is not a question" or an
//     empty set indistinguishable from a real absence -- and since 0 is also
//     the unset value, one that reached a filter would not even produce the
//     empty set. It would produce every route in the fleet.
//   - Anything above 4294967295 is refused rather than truncated, because a
//     uint32 conversion turns 4294967296 into exactly the 0 above.
func parseASNFlag(name, raw string) (uint32, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("-%s %q is not an AS number (a decimal 1-4294967295)",
			name, raw)
	}
	if n == 0 {
		return 0, fmt.Errorf("-%s is 0, which RFC 7607 reserves and no route "+
			"originates; an empty answer for it would be indistinguishable from "+
			"a real absence, and 0 is how this filter spells \"not asked\" -- so "+
			"the answer you would get is every route instead of none", name)
	}
	return uint32(n), nil
}

func cmdQueryRoutes(args []string) error {
	fs := flag.NewFlagSet("query routes", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	prefix := fs.String("prefix", "", "exact prefix match")
	covers := fs.String("covers", "", "address the route must contain")
	router := fs.String("router", "", "restrict to one router IP")
	peer := fs.String("peer", "", "restrict to one peer IP")
	rib := fs.String("rib", "", "restrict to one BMP RIB view")
	// The three wide filters. They are strings rather than fs.Uint64 so
	// that "not asked" is the empty string and an explicit 0 stays
	// distinguishable from it -- fs.Uint64 collapses the two, and 0 is the
	// one AS number that has to be refused rather than ignored. Parsing
	// them here also keeps the accepted notation identical to the API's:
	// flag's own numeric parsers read base 0, so -origin-asn 0x10 would be
	// AS 16 on this path and a 400 on the other.
	originASN := fs.String("origin-asn", "",
		"AS that originated the route (as_path's last element)")
	throughASN := fs.String("through-asn", "",
		"AS anywhere in the route's AS path, origin and neighbor included")
	community := fs.String("community", "",
		"community the route carries: 65000:100, 4200000000:1:2, rt:65000:100 or a bare 32-bit value")
	if err := fs.Parse(args); err != nil {
		return err
	}
	routerAddr, err := addrFromText(*router)
	if err != nil {
		return fmt.Errorf("-router: %w", err)
	}
	peerAddr, err := addrFromText(*peer)
	if err != nil {
		return fmt.Errorf("-peer: %w", err)
	}
	origin, err := parseASNFlag("origin-asn", *originASN)
	if err != nil {
		return err
	}
	through, err := parseASNFlag("through-asn", *throughASN)
	if err != nil {
		return err
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	out, err := src.Routes(ctx, RouteQuery{
		Prefix: *prefix, Covers: *covers,
		Router: routerAddr, Peer: peerAddr, RIB: *rib,
		OriginASN: origin, ThroughASN: through, Community: *community,
	})
	if err != nil {
		return err
	}
	return emitFanout(out, *f.output)
}

func cmdQueryRIB(args []string) error {
	fs := flag.NewFlagSet("query rib", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	router := fs.String("router", "", "router IP (required)")
	peer := fs.String("peer", "", "peer IP (required)")
	rib := fs.String("rib", "", "restrict to one BMP RIB view")
	collector := fs.String("collector", "",
		"walk this collector's view of the router; required when more than one collector watches it")
	family := fs.String("family", "unicast",
		"one of unicast, vpn, evpn")
	limit := fs.Int("limit", 0,
		"rows per page; the walk follows every page regardless")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *router == "" || *peer == "" {
		return errors.New("-router and -peer are both required: a RIB walk is " +
			"scoped to one (router, peer)")
	}
	routerAddr, err := addrFromText(*router)
	if err != nil {
		return fmt.Errorf("-router: %w", err)
	}
	peerAddr, err := addrFromText(*peer)
	if err != nil {
		return fmt.Errorf("-peer: %w", err)
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	out, err := src.RIB(ctx, RIBQuery{
		Router: routerAddr, Peer: peerAddr, RIB: *rib,
		Family: *family, Limit: ribWalkLimit(*limit), Collector: *collector,
	})
	if err != nil {
		return ribCollectorHint(err, *collector)
	}
	return emitFanout(out, *f.output)
}

// ribCollectorHint adds the CLI's own remedy to the refusal a RIB walk gets
// on a router more than one collector watches. The query layer's message is
// written for API callers ("start the walk with a RIBCursor"), which a CLI
// user cannot act on; -collector is the answer they can.
func ribCollectorHint(err error, collector string) error {
	if collector == "" && strings.Contains(err.Error(), "is monitored by") {
		return fmt.Errorf("%w\n\npass -collector with one of the collectors named above", err)
	}
	return err
}

// ribWalkLimit resolves the page size a walk runs at: whatever -limit named,
// or MaxRIBPage when it named nothing.
//
// The unset case does NOT fall through to the server's own default, and the
// difference is not cosmetic. A RIB page costs a whole-peer argMax recompute
// regardless of limit -- measured 2026-09-04 against a 1,000,000-route peer at
// 487ms per page at limit=1000 and 495ms at limit=10000, the same work for ten
// times the rows (docs/measurements.md, "Collector and writer load test").
// Since this command follows every page to the end of the walk by design (see
// RIBQuery.Limit), a smaller page buys nothing and costs one extra whole-RIB
// aggregation per page it adds: about eight minutes for a full-table peer at
// 1000, about fifty seconds at 10000.
//
// The server's interactive default stays at DefaultRIBPage, which is the right
// answer for the caller it serves -- someone asking /v1/rib one question should
// not be handed ten thousand rows unasked. Only the walker, which has already
// said it wants the whole table, opts up.
func ribWalkLimit(flagValue int) int {
	if flagValue <= 0 {
		return query.MaxRIBPage
	}
	return flagValue
}

// ---- link-state ----

// lsFlags are the parameters every `vantage query ls` subcommand shares:
// /v1/ls/nodes, /v1/ls/links and /v1/ls/prefixes all document router, peer,
// rib, protocol, area, asn and state, and api/openapi.yaml's own parameter
// components (area, lsASN, lsState) are shared across the same three paths
// for the identical reason -- one definition rather than three that could
// drift apart.
type lsFlags struct {
	router, peer *string
	rib          *string
	protocol     *string
	area, asn    *string
	state        *string
}

// registerLSFlags declares lsFlags on fs. Area, ASN and State are fs.String,
// not fs.Uint or a closed-set flag.Var, for LSQuery's own reason: the
// parsing that would reject a bad value happens once, in validateLSQuery,
// not in the flag package's own error path -- so every one of these fields
// fails the same way -state does, before f.source dials.
func registerLSFlags(fs *flag.FlagSet) lsFlags {
	return lsFlags{
		router: fs.String("router", "", "restrict to one router IP"),
		peer:   fs.String("peer", "", "restrict to one peer IP"),
		rib:    fs.String("rib", "", "restrict to one BMP RIB view"),
		protocol: fs.String("protocol", "", "BGP-LS Protocol-ID: a decimal "+
			"1-255 or a registry name (isis-l2, ospfv2, direct, static, ...)"),
		area: fs.String("area", "", "OSPF/IS-IS area, decimal; 0 is a real "+
			"area (the OSPF backbone) and stays askable -- omit the flag "+
			"entirely for \"any area\""),
		asn: fs.String("asn", "", "AS number, decimal; 0 is a real value "+
			"some node descriptors carry -- omit the flag entirely for "+
			"\"any AS\""),
		state: fs.String("state", "", "live, withdrawn or any (default live)"),
	}
}

// query builds an LSQuery from the shared flags plus the caller's own key
// parameters. node, localNode, remoteNode, prefix and covers are "" from
// whichever subcommand does not register that flag -- cmdQueryLSNodes
// passes "" for localNode, remoteNode, prefix and covers, and so on -- so
// LSQuery always carries all seven even though only the relevant three ever
// hold anything.
//
// -router and -peer are parsed HERE, unlike every other field: LSQuery
// types them as netip.Addr rather than string (see its own doc comment for
// why only the numeric fields and state stay strings), so this is the one
// place in the ls_* path that mirrors what cmdQueryRoutes does for its own
// -router and -peer.
func (f lsFlags) query(node, localNode, remoteNode, prefix, covers string) (LSQuery, error) {
	router, err := addrFromText(*f.router)
	if err != nil {
		return LSQuery{}, fmt.Errorf("-router: %w", err)
	}
	peer, err := addrFromText(*f.peer)
	if err != nil {
		return LSQuery{}, fmt.Errorf("-peer: %w", err)
	}
	return LSQuery{
		Router: router, Peer: peer, RIB: *f.rib,
		Protocol: *f.protocol, Area: *f.area, ASN: *f.asn,
		Node: node, LocalNode: localNode, RemoteNode: remoteNode,
		Prefix: prefix, Covers: covers,
		State: *f.state,
	}, nil
}

// lsStateLabel renders LSNode/LSLink/LSPrefix's own IsWithdraw as the same
// three words -state accepts, so a table's STATE column and the flag that
// narrows it use one vocabulary.
func lsStateLabel(isWithdraw bool) string {
	if isWithdraw {
		return "withdrawn"
	}
	return "live"
}

func cmdQueryLS(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vantage query ls (nodes|links|prefixes) [flags]")
	}
	switch args[0] {
	case "nodes":
		return cmdQueryLSNodes(args[1:])
	case "links":
		return cmdQueryLSLinks(args[1:])
	case "prefixes":
		return cmdQueryLSPrefixes(args[1:])
	default:
		return fmt.Errorf("unknown query ls subcommand %q: want nodes, links "+
			"or prefixes", args[0])
	}
}

func cmdQueryLSNodes(args []string) error {
	fs := flag.NewFlagSet("query ls nodes", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	lsf := registerLSFlags(fs)
	node := fs.String("node", "", "restrict to one node key (decimal; see "+
		"the NODE KEY column of an unfiltered query for a value to pass)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q, err := lsf.query(*node, "", "", "", "")
	if err != nil {
		return err
	}
	// Validated BEFORE f.source dials: see validateLSQuery's own doc
	// comment for why this call exists purely for its error.
	if err := validateLSQuery(q); err != nil {
		return err
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := src.LSNodes(ctx, q)
	if err != nil {
		return err
	}
	if *f.output == "json" {
		return emitJSON(mapSlice(rows, api.NewWireLSNode))
	}
	return table(func(w io.Writer) {
		fmt.Fprintln(w, "SYSNAME\tROUTER\tPEER\tPROTOCOL\tAREA\tASN\tNODE KEY\tNAME\tROUTER ID\tSTATE")
		for _, n := range rows {
			rowf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
				orDash(n.RouterSysName), n.RouterIP, n.PeerIP,
				protocolLabel(n.Protocol), n.Area, n.ASN, n.NodeKey,
				orDash(n.Name), orDash(n.RouterID), lsStateLabel(n.IsWithdraw))
		}
	})
}

func cmdQueryLSLinks(args []string) error {
	fs := flag.NewFlagSet("query ls links", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	lsf := registerLSFlags(fs)
	// No backquotes in a usage string: package flag takes the first
	// backquoted word as the flag's value name in -h.
	localNode := fs.String("local-node", "", "restrict to one LOCAL end node "+
		"key (decimal; the table prints labels rather than keys, so take one "+
		"from the NODE KEY column of 'vantage query ls nodes' or from -o json) -- not "+
		"either end")
	remoteNode := fs.String("remote-node", "", "restrict to one REMOTE end "+
		"node key (decimal; same sources as -local-node) -- not either end")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q, err := lsf.query("", *localNode, *remoteNode, "", "")
	if err != nil {
		return err
	}
	if err := validateLSQuery(q); err != nil {
		return err
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := src.LSLinks(ctx, q)
	if err != nil {
		return err
	}
	if *f.output == "json" {
		return emitJSON(mapSlice(rows, api.NewWireLSLink))
	}
	// LOCAL and REMOTE carry the resolved LABEL, not the node_key, and
	// LABEL SRC carries both ends' tiers. That is the whole point of
	// the three-tier chain: this table is the default output of the
	// command, so it is what a human or an LLM actually reads, and it
	// used to print two 19-20 digit decimal cityHash64 values under
	// headings that promised nodes. A topology labeled in hex defeats
	// the point of a human-readable link table; a topology labeled in
	// longer decimal defeats it further.
	//
	// The node keys are not printed alongside, because -o json carries
	// them and the row does not have space for both: four identity columns
	// per end is a line no terminal wraps well. -local-node and
	// -remote-node's own flag help points at where a key still comes from.
	//
	// LABEL SRC is one column rather than two, and it is not dropped for
	// width. LSEndpoint.LabelSource exists precisely so that "this router
	// named the node" and "another router did" are distinguishable rather
	// than blended -- a table showing the name without the tier IS the
	// blend that type was added to prevent.
	//
	// It is routine rather than theoretical: in one measured production
	// archive, 25% of the endpoints THIS TABLE PRINTS resolve to the raw
	// identifier rather than to any name -- 56 of the 222 endpoints
	// across 111 links at state=live, the CLI's own default. Naming the
	// scope is not pedantry. A different, wider count puts the figure at
	// 27%, which is the non-observer share of a wider, ungated census
	// (639 of 2,333 raw local ends); at that scope the identifier tier's
	// own share is 10.8%, so the number was neither the right population
	// nor the right tier. See lsNodeLabelsCTE, which carries both
	// censuses with their scopes attached for exactly this reason.
	return table(func(w io.Writer) {
		fmt.Fprintln(w, "SYSNAME\tROUTER\tPEER\tPROTOCOL\tLOCAL\tLOCAL AREA\tLOCAL ASN\tREMOTE\tREMOTE AREA\tREMOTE ASN\tLABEL SRC\tSTATE")
		for _, l := range rows {
			rowf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%d\t%d\t%s\t%s\n",
				orDash(l.RouterSysName), l.RouterIP, l.PeerIP,
				protocolLabel(l.Protocol),
				orDash(l.Local.Label), l.Local.Area, l.Local.ASN,
				orDash(l.Remote.Label), l.Remote.Area, l.Remote.ASN,
				l.Local.LabelSource+"/"+l.Remote.LabelSource,
				lsStateLabel(l.IsWithdraw))
		}
	})
}

func cmdQueryLSPrefixes(args []string) error {
	fs := flag.NewFlagSet("query ls prefixes", flag.ContinueOnError)
	f := registerQueryFlags(fs)
	lsf := registerLSFlags(fs)
	node := fs.String("node", "", "restrict to one node key (decimal)")
	prefix := fs.String("prefix", "", "exact prefix match")
	covers := fs.String("covers", "", "address the prefix must contain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q, err := lsf.query(*node, "", "", *prefix, *covers)
	if err != nil {
		return err
	}
	if err := validateLSQuery(q); err != nil {
		return err
	}
	ctx := context.Background()
	src, closeFn, err := f.source(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := src.LSPrefixes(ctx, q)
	if err != nil {
		return err
	}
	if *f.output == "json" {
		return emitJSON(mapSlice(rows, api.NewWireLSPrefix))
	}
	return table(func(w io.Writer) {
		// ROUTER ID sits beside NODE KEY for the reason the links table
		// prints labels instead of keys: the key is a 19-20 digit hash and
		// the row already carries the readable identity the originating
		// node advertised. ls_prefixes has no name column at all -- there
		// is no three-tier label to resolve here, and this table does not
		// invent one by joining -- so router_id is the most readable node
		// identity a prefix row honestly has. `ls nodes` prints the same
		// pair, and its NAME column beside them is where a name comes
		// from.
		fmt.Fprintln(w, "SYSNAME\tROUTER\tPEER\tPROTOCOL\tAREA\tASN\tNODE KEY\tROUTER ID\tPREFIX\tSTATE")
		for _, p := range rows {
			rowf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
				orDash(p.RouterSysName), p.RouterIP, p.PeerIP,
				protocolLabel(p.Protocol),
				p.Area, p.ASN, p.NodeKey, orDash(p.RouterID), p.Prefix,
				lsStateLabel(p.IsWithdraw))
		}
	})
}

// ---- output ----

// emitFanout renders whichever families the answer holds.
//
// In json it emits all three keys unconditionally, exactly as /v1/routes
// does and for the same reason: a caller iterating the output must not have
// to tell "no results" from "not searched". In table form the empty
// families are skipped, because a person reading a terminal is not
// iterating and three empty headers is noise.
func emitFanout(out RouteFanout, format string) error {
	if format == "json" {
		return emitJSON(api.NewWireRouteFanout(
			mapSlice(out.Unicast, api.NewWireUnicastRoute),
			mapSlice(out.VPN, api.NewWireVPNRoute),
			mapSlice(out.EVPN, api.NewWireEVPNRoute),
		))
	}
	return table(func(w io.Writer) {
		// PATH is path_id, and it is a column rather than a detail because
		// of what the archive actually holds: ADD-PATH means one peer can
		// advertise the same prefix more than once, and without this the
		// two rows are byte-identical on screen. That is not a hypothetical
		// -- `vantage query routes -covers 10.255.0.2` printed exactly that
		// pair the first time it was run against a populated database.
		if len(out.Unicast) > 0 {
			fmt.Fprintln(w, "FAMILY\tPREFIX\tPATH\tROUTER\tPEER\tRIB\tNEXT HOP\tAS PATH\tDUMP")
			for _, r := range out.Unicast {
				rowf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Family, r.Prefix, r.PathID, r.RouterIP, r.PeerIP, r.RIB,
					orDash(addrOrDash(r.NextHop)), asPath(r.ASPath), r.DumpState)
			}
		}
		if len(out.VPN) > 0 {
			if len(out.Unicast) > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "FAMILY\tRD\tPREFIX\tPATH\tLABEL\tROUTER\tPEER\tRIB\tNEXT HOP\tDUMP")
			for _, r := range out.VPN {
				// "-" rather than 0, because label 0 is the real
				// implicit-null label an MPLS router advertises. HasLabel
				// is what tells the two apart; see query.VPNRoute.
				label := "-"
				if r.HasLabel {
					label = strconv.FormatUint(uint64(r.Label), 10)
				}
				rowf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Family, orDash(r.RD), r.Prefix, r.PathID, label, r.RouterIP,
					r.PeerIP, r.RIB, orDash(addrOrDash(r.NextHop)), r.DumpState)
			}
		}
		if len(out.EVPN) > 0 {
			if len(out.Unicast) > 0 || len(out.VPN) > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "TYPE\tRD\tPREFIX\tMAC\tIP\tROUTER\tPEER\tRIB\tDUMP")
			for _, r := range out.EVPN {
				rowf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.RouteType, orDash(r.RD), orDash(r.Prefix), orDash(r.MAC),
					orDash(r.IP), r.RouterIP, r.PeerIP, r.RIB, r.DumpState)
			}
		}
	})
}

// emitJSON writes one document to stdout.
//
// The value passed in is always an api.WireX, never a query.X, and that is
// the point: this is the same JSON cmd/vantage-api serves, produced by the
// same converters, so `vantage query -o json` and `curl` agree whether or
// not -dsn was used. See api/types.go's note above WireRouter.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table runs fn against a tabwriter aimed at stdout and flushes it, the
// same shape `vantage debug` uses.
func table(fn func(io.Writer)) error {
	return tableTo(os.Stdout, fn)
}

// tableTo is table with the destination named.
//
// Table cells carry strings a router chose -- sysName and sysDescr from the
// BMP Initiation, IS-IS hostnames -- and the collector stores them byte for
// byte, so an escape sequence in one reaches this terminal intact unless
// something here stops it. Two layers do: rowf escapes every control byte in
// each string argument, tab and newline included, so a value cannot shift
// columns or forge a row; and fn writes through controlSafeWriter, which
// escapes whatever reaches the table by any other route while leaving the
// tabs and newlines the table's own format strings lay it out with.
func tableTo(out io.Writer, fn func(io.Writer)) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fn(controlSafeWriter{w})
	return w.Flush()
}

// rowf is fmt.Fprintf for one table row: every string-kinded argument is
// escaped in full before formatting, while the format string -- this
// file's own tabs and newline -- is left alone. Non-string arguments are
// untouched; an integer or an address cannot carry a control byte.
func rowf(w io.Writer, format string, args ...any) {
	for i, a := range args {
		v := reflect.ValueOf(a)
		if !v.IsValid() || v.Kind() != reflect.String {
			continue
		}
		s := v.String()
		if st, ok := a.(fmt.Stringer); ok {
			s = st.String()
		}
		args[i] = escapeControl(s, false)
	}
	fmt.Fprintf(w, format, args...)
}

// controlSafeWriter passes writes through escapeControl, keeping tab and
// newline. It reports the length it was handed rather than what it wrote,
// as io.Writer requires of a filter that expands its input.
type controlSafeWriter struct{ w io.Writer }

func (c controlSafeWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(c.w, escapeControl(string(p), true)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// printWarnings prints meta.warnings, one per line. Both fields are escaped
// in full: a warning's message can quote router text.
func printWarnings(w io.Writer, ws []api.Warning) {
	for _, wa := range ws {
		fmt.Fprintf(w, "vantage: warning: %s: %s\n",
			escapeControl(wa.Code, false), escapeControl(wa.Message, false))
	}
}

// escapeControl makes s safe to print on a terminal by replacing every byte
// a terminal would act on with a visible escape: C0 controls and DEL as
// \xNN, C1 controls (U+0080-U+009F, which some terminals honor as CSI, OSC
// and the rest) as \uNNNN, and a byte that is not valid UTF-8 as \xNN, since
// a terminal may decode it as C1 too. With keepLayout, tab and newline pass
// through; without it they are escaped like the rest.
//
// It is for terminal output only. -o json does not use it: encoding/json
// already escapes every control character, and a program reading the JSON
// needs the value the router sent, not a rendering of it.
func escapeControl(s string, keepLayout bool) string {
	clean := true
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case keepLayout && (r == '\t' || r == '\n'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func mapSlice[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}

// protocolLabel renders a BGP-LS Protocol-ID for a table the way
// api/openapi.yaml's protocol_name documents it on the wire: the IANA
// registry name where this project has one, and the NUMBER where it does
// not.
//
// It is not orDash(query.ProtocolName(id)), which is what these three tables
// used to print, because a dash is the wrong answer here. orDash exists for
// a column that is legitimately EMPTY; an unrecognized Protocol-ID is not
// empty, it is a value this project cannot name, and the number beside
// the missing name is the whole truth. Rendering it as "-" throws away
// the only fact the row had -- and the wire form does not: it carries
// protocol and protocol_name side by side, so the table was strictly
// less informative than the JSON it summarizes.
//
// ParseProtocol accepts a decimal ID as well as a name, so the number this
// prints is also a value the reader can hand straight back to -protocol.
func protocolLabel(id uint8) string {
	if name := query.ProtocolName(id); name != "" {
		return name
	}
	return strconv.FormatUint(uint64(id), 10)
}

// orDash renders an empty string as "-", so a column that is legitimately
// empty is visibly empty rather than looking like a rendering bug.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func addrOrDash(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func asPath(path []uint32) string {
	if len(path) == 0 {
		return "-"
	}
	parts := make([]string, len(path))
	for i, as := range path {
		parts[i] = strconv.FormatUint(uint64(as), 10)
	}
	return strings.Join(parts, " ")
}

// joinDumpStates renders Peer.DumpStates, which is per family and therefore
// cannot be one word. Sorted, so two runs of the same command produce the
// same line -- Go map iteration order is deliberately random, and an
// unsorted rendering would make a diff of two outputs unreadable.
func joinDumpStates(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	fams := make([]string, 0, len(m))
	for f := range m {
		fams = append(fams, f)
	}
	slices.Sort(fams)
	parts := make([]string, len(fams))
	for i, f := range fams {
		parts[i] = f + "=" + m[f]
	}
	return strings.Join(parts, " ")
}
