// Package redact keeps a connection string's credentials out of the logs
// and errors both vantage-collector and vantage-writer produce when they
// fail to reach NATS or ClickHouse. Both daemons' configs (collector.Config.
// NatsURL; sink.Config.NatsURL, sink.Config.ClickHouseDSN) accept plain
// userinfo URLs -- the dev stack's own default ClickHouseDSN literally is
// one, clickhouse://vantage:vantage@127.0.0.1:9000/vantage -- and a startup
// connection failure is exactly the kind of error an operator copies
// verbatim into a ticket or a chat.
//
// # Design history: why Err does not scrub text
//
// The first two versions of this package tried to keep a raw connection
// string out of a wrapped library error by searching that error's rendered
// text for the string we knew we had passed in, and replacing it. That
// approach failed twice, for two different reasons (a literal match missed
// %q-escaping of '"'/'\' in a password; segmentation of a multi-server NATS
// URL meant the text that could leak was a substring of what we searched
// for, never the whole of it) before it was replaced with the design here.
//
// The design here does not search rendered text at all. Every place this
// package's callers can leak a credential goes through *url.Error: both
// clickhouse-go's DSN parser (lib/churl, an internal fork of net/url.Parse
// -- see CheckURL's doc comment) and nats.go's own URL parsing construct a
// literal, stdlib net/url.Error on failure -- confirmed by reading both
// sources directly, not inferred from behavior. *url.Error is a concrete
// stdlib type with exactly one Error() method, defined once, in net/url
// itself: func (e *Error) Error() string { return fmt.Sprintf("%s %q: %s",
// e.Op, e.URL, e.Err) }. Because that is the only place *url.Error ever
// renders itself, there are only two ways its URL field can appear in text
// derived from it: literally (if some wrapper independently includes e.URL
// itself, outside of calling Error()) or %q-quoted (via Error() itself).
// Err matches errors.As for *url.Error, reads e.URL directly out of the
// struct -- no guessing, no re-deriving what the "dangerous substring"
// might be from a config value that may not even be the same string the
// library actually tried to parse -- and redacts exactly that value.
//
// That enumeration was right about e.URL and wrong to stop there, which is
// worth recording precisely because it reads so convincingly. *url.Error
// has a second field, e.Err, holding an error net/url built separately --
// and net/url's messages quote fragments of the input: a password
// containing '/' produces `invalid port ":trailSECRET" after host`. The URL
// field was being redacted correctly while the password went to the log one
// field over. The lesson is not "also handle e.Err"; it is that enumerating
// the ways a known-dangerous value can be *rendered* is the wrong shape of
// argument, because it has to be complete to be sound, and completeness is
// exactly what nobody can check. Err now rebuilds a *url.Error's rendering
// from pieces that cannot carry a credential (safeURLError) instead of
// editing the text the library produced.
//
// # Design history: why URL fails closed
//
// Finding the right string to redact, above, is only half the problem;
// rendering it safely is the other half, and the renderer kept leaking
// after Err was fixed. Err, CheckURL, CheckNatsURL and NatsURL all funnel
// into URL, and URL used to end with a best-effort regexp over the raw
// input -- which returned that raw input, password included, whenever the
// regexp did not match. Two ordinary inputs made it not match ('/' in a
// password; NATS's schemeless "user:pass@host" shorthand), so all of Err's
// structural care was undone by its renderer.
//
// URL is now additive rather than subtractive: it emits only pieces it has
// positively identified (net/url components rebuilt via url.URL.String(),
// or a scheme and host recovered lexically and checked against a
// whitelist), and substitutes a fixed marker for everything else. The
// invariant is not "known bad patterns are removed" but "unrecognized bytes
// are never echoed" -- which is what makes it a property of the class
// rather than of the shapes anyone happened to test. See URL's own doc
// comment for the mechanism and the two leaks that motivated it.
//
// # Design history: why rendering alone was never going to be enough
//
// Everything above is about how a connection string is *rendered*. Round
// after round was spent closing one leak each while the class stayed open,
// and what ends the pattern is this:
// clickhouse-go accepts the password as a query *parameter*
// (clickhouse_options.go's fromDSN, v2.48.0, case "password"), so
// "clickhouse://host:9000/db?password=SECRET" carries a credential with no
// '@' anywhere in it -- and the renderer, having found no userinfo and no
// '@' to be suspicious of, echoed the query back whole.
//
// That is fixed below (see redactQuery), but the more important conclusion
// is that a renderer is only reached if a call site remembers to call it.
// secret now holds the config values in types whose *default*
// rendering -- every fmt verb, slog, JSON, YAML -- is the output of this
// package, and whose raw value can only be obtained through an explicitly
// named accessor. This package is still the rendering engine; it is no
// longer the thing standing between a credential and a log line all by
// itself.
package redact

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// redactedMarker stands in for every run of operator-supplied bytes this
// package could not positively classify as harmless. It is deliberately a
// fixed, obviously synthetic string and never any transformation of the
// input: the whole point of the rendering rules below is that a byte of the
// operator's connection string reaches a log line only by being recognized,
// never by failing to be recognized. See URL.
const redactedMarker = "REDACTED"

// natsDefaultScheme is the scheme nats.go's own parseServerURL prepends to a
// segment that contains no "://" before handing it to net/url.Parse
// (nats.go@v1.54.0), which is what makes a bare "user:pass@host:4222" legal
// NATS configuration. Both the validator (CheckNatsURL) and the renderer
// (NatsURL) prepend it, so the two agree on what they are looking at -- see
// natsSegmentURL for what went wrong when only one of them did.
const natsDefaultScheme = "nats"

// schemePattern is RFC 3986's scheme production. A "://" in the input only
// establishes a scheme if what precedes it really is one; otherwise those
// bytes are unclassified, and unclassified bytes are dropped rather than
// echoed.
var schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*$`)

// hostPortPattern is the whitelist of what this package will echo as a
// host: hostname and IPv4 characters, the brackets and colons of an IPv6
// literal, a port, and the commas of ClickHouse's multi-host DSN syntax --
// plus at least one alphanumeric, so pure punctuation is not "a host". It
// deliberately excludes every character that distinguishes a credential
// from a host: '"', '\', '%', '@', '/', whitespace and control bytes. This
// is defense in depth rather than the primary barrier (the choice of which
// '@' separates userinfo from host is what actually puts the password on
// the dropped side), but it is what lets redactUnparsed promise it never
// emits bytes it has not classified.
var hostPortPattern = regexp.MustCompile(`^[A-Za-z0-9._\-:\[\],]*[A-Za-z0-9][A-Za-z0-9._\-:\[\],]*$`)

// policy is the set of library-specific facts the renderer needs: the same
// bytes mean different things to nats.go and to clickhouse-go, and rendering
// them safely means following each library's own reading of them rather than
// a generic URL grammar. They are values rather than a flag so that a
// library added later has an obvious place to declare what it treats as
// secret -- which has since happened: there are three (clickHousePolicy,
// natsPolicy, and httpProxyPolicy for the second URL a ClickHouse DSN can
// carry inside its http_proxy parameter).
type policy struct {
	// bareUserinfoIsCredential says whether a userinfo block with no ':'
	// in it -- "scheme://word@host" -- is itself a credential.
	//
	// For a ClickHouse DSN it is not: clickhouse-go reads Auth.Username and
	// Auth.Password from the two halves of the userinfo (clickhouse_options
	// .go's fromDSN, v2.48.0), so a colonless userinfo is a bare username,
	// and this package's contract is to keep usernames.
	//
	// For a NATS URL it is. nats.go's connectProto (v1.54.0) does:
	//
	//	if _, ok := u.Password(); !ok { token = u.Username() }
	//
	// -- a colonless userinfo is an *auth token*, i.e. the entire
	// credential, which the keep-the-username rule would otherwise print in
	// full.
	bareUserinfoIsCredential bool

	// safeQueryParams names the query parameters whose *values* may be
	// echoed. Empty means none: a parameter is echoed only because it was
	// recognized, never because it was not recognized as dangerous. See
	// redactQuery.
	safeQueryParams map[string]bool

	// secretQueryParams names the query parameters the library reads as
	// credentials (or as URLs that may embed credentials). Their names are
	// echoed with the value replaced, so an operator can see *that* they
	// set a password without seeing it; every other unrecognized parameter
	// is dropped entirely, name included.
	secretQueryParams map[string]bool
}

// clickHousePolicy renders a ClickHouse DSN.
//
// The query-parameter sets are transcribed from clickhouse-go v2.48.0's
// Options.fromDSN (clickhouse_options.go), which switches on the exact
// parameter name -- so this list is case-sensitive in the same way, and
// "?PASSWORD=x" is deliberately *not* recognized as a password (fromDSN
// would treat it as an arbitrary ClickHouse setting, and this package
// therefore treats it as an unrecognized parameter and drops it).
//
// Two parameters fromDSN recognizes are credential-bearing rather than
// safe:
//
//   - "password" is the leak that motivated adding a secret-parameter
//     list. It is a complete alternative to the userinfo password, reached
//     by exactly the operator whose password broke the userinfo syntax.
//   - "http_proxy" is parsed as a URL of its own and can carry its own
//     userinfo ("http://user:pass@proxy:8080"), so echoing its value would
//     leak a different credential than the one everybody was looking for.
//
// "username" is on the safe list rather than the secret one for exactly the
// reason the userinfo username is kept (see URL): for clickhouse-go that
// field is a username, it identifies which account failed, and it is not a
// secret. If that judgment is ever revisited, both places must change
// together.
var clickHousePolicy = policy{
	safeQueryParams: map[string]bool{
		"block_buffer_size":        true,
		"client_info_product":      true,
		"compress":                 true,
		"compress_level":           true,
		"conn_max_lifetime":        true,
		"connection_open_strategy": true,
		"database":                 true,
		"debug":                    true,
		"dial_timeout":             true,
		"http_path":                true,
		"max_compression_buffer":   true,
		"max_idle_conns":           true,
		"max_open_conns":           true,
		"read_timeout":             true,
		"secure":                   true,
		"skip_verify":              true,
		"tls_server_name":          true,
		"username":                 true,
	},
	secretQueryParams: map[string]bool{
		"password":   true,
		"http_proxy": true,
	},
}

// natsPolicy renders one NATS server URL. nats.go reads no query parameter
// at all -- its URL handling looks only at the scheme, userinfo and host
// (parseServerURL and connectProto, v1.54.0) -- so no parameter is on the
// safe list and any query an operator wrote is rendered as the marker
// rather than echoed.
var natsPolicy = policy{bareUserinfoIsCredential: true}

// httpProxyPolicy renders the *value* of a ClickHouse DSN's "http_proxy"
// parameter, which is a URL in its own right and carries its own userinfo
// ("http://user:pass@proxy:8080"). It is what CheckClickHouseProxy renders
// with, and it is the third policy the type's doc comment anticipated.
//
// It is deliberately the zero policy:
//
//   - bareUserinfoIsCredential is false, as for a ClickHouse DSN. The
//     parsed URL is stored as Options.HTTPProxyURL and handed to net/http,
//     which reads its userinfo as Proxy-Authorization in the same
//     username:password shape, so a colonless userinfo is a username and
//     this package's contract is to keep usernames.
//   - No parameter is on the safe list, and none on the secret list.
//     clickhouse-go recognizes no query parameter *of the proxy URL* --
//     it keeps the whole *url.URL -- so there is nothing this package could
//     positively recognize, and an unrecognized parameter is dropped, name
//     and value, rather than echoed. Reusing clickHousePolicy here would
//     have echoed a "?username=" or "?database=" written on the proxy URL
//     purely because those names mean something on a *different* URL.
var httpProxyPolicy = policy{}

// URL strips the password (but not the username or host) from a single
// connection-string URL before it can reach an error or a log line. Only
// the password is removed: the host is what makes
// "connect clickhouse clickhouse://vantage@host:9000/vantage: ..."
// actionable, and the username costs nothing to keep.
//
// # Why this function fails closed
//
// The property that matters here is not "the password is removed from the
// strings we thought of" but "no byte of rawURL is ever echoed unless this
// function positively recognized it". A subtractive rule -- take the input
// and delete the dangerous part -- removes a password from the shapes its
// author thought of and leaves the class open, because a subtractive rule
// that does not fire returns the input intact. Ending in
// `return credentialInURLPattern.ReplaceAllString(rawURL, "//")`, for
// example, a regexp (`//[^/@]*@`) returns rawURL *verbatim, password and
// all* whenever it happens not to match, and two entirely ordinary inputs
// make it not match:
//
//   - A '/' in the password ("openssl rand -base64" emits these routinely).
//     net/url rejects the URL (the authority ends at that first '/', so the
//     port reads as ":ab"), and `[^/@]*` cannot cross the '/' either, so the
//     whole DSN is logged. Worse, such a config fails validation every
//     time, so the operator hits the leaking path 100% of the time.
//   - The schemeless "user:pass@host:4222" shorthand NATS accepts. There is
//     no "//" for the regexp to anchor on, and net/url.Parse *succeeds* on
//     it -- as an opaque URL (Scheme "user", Opaque "pass@host:4222") whose
//     User field is nil, so a parse-based branch finds no userinfo to strip
//     and falls through to the regexp.
//
// So this function is additive: it builds its result out of pieces it has
// identified, and anything left over is replaced by redactedMarker rather
// than passed through. Every byte of rawURL that reaches the output does so
// through one of exactly three recognitions:
//
//   - net/url parsed it into components (and it is not an opaque URL, the
//     shape that hid the second leak above). The result is rebuilt with
//     url.URL.String() from those components, with the password field
//     cleared -- so the password cannot survive, wherever else in the URL
//     its characters might also appear -- and with the query rebuilt from
//     an allowlist rather than echoed (see redactQuery: the query is a
//     second, '@'-free place clickhouse-go accepts a password).
//   - net/url could not parse it, and redactUnparsed recovered a scheme and
//     a host from it lexically, each checked against a whitelist pattern
//     (schemePattern, hostPortPattern). Everything else -- userinfo, path,
//     query, fragment -- is dropped, not inspected.
//   - the input contains an '@' that is not a userinfo separator, which
//     makes host and credential impossible to tell apart, and only the
//     pieces that are harmless under *both* readings are emitted (see
//     renderParsed's ambiguous branch and ambiguousHost).
//
// The host survives all three routes whenever it can be recovered at all,
// because an error that does not name the host it failed to reach just
// moves the operator's pain somewhere else. What is never preserved is a
// byte this package could not account for.
//
// URL operates on one URL. A ClickHouse DSN is always exactly one
// (ClickHouse's multi-host syntax lives inside a single URL's Host field,
// behind one userinfo block -- there is no second userinfo for a comma to
// smuggle past). A NATS URL is not: NATS's documented HA syntax is a
// comma-separated list of complete URLs in one config string, each with its
// own userinfo, and nats.Connect splits and parses them independently --
// redacting the joined string as a single URL leaves every server after the
// first unredacted. NatsURL below is the segment-aware equivalent for that
// shape; use it, not this function, for a NATS URL.
func URL(rawURL string) string {
	return redactOne(rawURL, clickHousePolicy)
}

// redactOne is URL's implementation, parameterized on the library-specific
// facts in policy.
func redactOne(rawURL string, p policy) string {
	// u.Opaque == "" is load-bearing, not incidental: net/url.Parse
	// "succeeds" on the schemeless "user:pass@host:4222" shorthand by
	// reading it as an opaque URL with a nil User, and treating that as a
	// successful parse with no userinfo is exactly how a raw NATS password
	// reached a log line. An opaque URL has no authority, so it has no
	// userinfo this branch can reason about; it belongs to redactUnparsed.
	if u, err := url.Parse(rawURL); err == nil && u.Opaque == "" {
		if u.User != nil {
			// net/url found the userinfo, so the password is entirely
			// inside u.User and nowhere else in the parse. Clearing it and
			// rebuilding is complete regardless of what the password
			// contains -- including a '@' (net/url splits the authority at
			// its *last* '@', so "user:p@ss@host" parses as password
			// "p@ss") and including characters that also appear in the
			// host.
			// The userinfo net/url found is only *the* userinfo if no
			// later '@' could have been the real separator. A password
			// containing an '@' and then a '/' produces both:
			//
			//	clickhouse://u:p@ss/wordSECRET@127.0.0.1:19000/db
			//
			// parses as userinfo "u:p", host "ss", path
			// "/wordSECRET@127.0.0.1:19000/db" -- so clearing the parsed
			// password removes "p" and echoes the rest of it as a host and
			// a path. The same ambiguity as the no-userinfo case, one
			// component to the right, and the same treatment: what net/url
			// called the host cannot be vouched for.
			//
			// The username can: under either reading it is the bytes
			// before the first ':' of the userinfo, which is what
			// url.Userinfo.Username() returns either way.
			_, hasPassword := u.User.Password()
			if hasPassword {
				u.User = url.User(u.User.Username())
			} else if p.bareUserinfoIsCredential {
				u.User = url.User(redactedMarker)
			}
			// A colonless ClickHouse userinfo is a username, and a username
			// is kept whichever reading applies, so a stray '@' costs
			// nothing there and the host is not in doubt.
			credentialInUserinfo := hasPassword || p.bareUserinfoIsCredential
			return renderParsed(u, p, credentialInUserinfo && hasStrayAt(u))
		}
		// No userinfo was parsed -- but "net/url found no userinfo" is not
		// the same as "there is no credential here", and assuming it was
		// is a leak in its own right. An unencoded '/' at the start of a
		// password ends the authority early, so
		//
		//	clickhouse://vantage:/leadSECRET@127.0.0.1:19000
		//
		// parses *successfully* as host "vantage:" with the remainder of
		// the password, the real '@' and the real host all sitting in
		// Path -- and u.String() would echo that Path back verbatim.
		//
		// A credential can only be in *userinfo* position if a literal '@'
		// is in the input at all (userinfo is what precedes one), so the
		// absence of '@' is a sound, complete test for "nothing in this
		// URL's structure is hiding a userinfo". It is not a test for "no
		// credential at all": a ClickHouse DSN can carry its password in a
		// query parameter with no '@' anywhere, which is why renderParsed
		// rebuilds the query on this path too. Testing rawURL rather than
		// u.Path also avoids over-redacting a percent-encoded "%40" in a
		// path, which u.Path would have already decoded to '@'.
		return renderParsed(u, p, strings.ContainsRune(rawURL, '@'))
	}
	return redactUnparsed(rawURL, p)
}

// renderParsed rebuilds a URL net/url accepted, out of its parsed
// components. It is the only place a parsed URL is turned back into text.
//
// ambiguous says the input contains an '@' that net/url did not account for
// as userinfo. That is not a rare shape and it is not decidable: exactly the
// same bytes have two readings, and no lexical test separates them, because
// the difference is in what the operator meant.
//
//	clickhouse://vantage:/leadSECRET@127.0.0.1:19000   (A) '/' in password
//	clickhouse://127.0.0.1:19000/db@x                  (B) '@' in database
//
// Under reading (A) an unencoded character in the password ended the
// authority early, so what net/url called the host is really the userinfo
// and the true host follows the '@'. Under reading (B) net/url is right and
// the '@' is just a byte in a path or a query. Always assuming (A) --
// taking the fragment after the last '@' as the host -- renders (B) as
// "clickhouse://REDACTED@x": an error naming a host the operator does not
// have, having dropped the one they do. Naming the wrong endpoint is worse
// than naming none.
//
// So neither reading is assumed. Each piece is emitted only if it is
// harmless under both:
//
//   - Host: everything from the first ':' on is dropped (ambiguousHost).
//     Under (B) that costs the port; under (A) the bytes after that ':' are
//     the start of the password -- and they are not hypothetical, since
//     net/url only accepts an all-digit port, so a password like
//     "2024/summer" parses as port "2024" and would be logged as one.
//   - Path: a path is never a credential to either library (clickhouse-go
//     reads it as the database name), but under (A) it is the *middle of
//     the password*, so it is replaced by the marker.
//   - Path tail: under (A) the bytes after the last '@' in the path are the
//     true host, which is the actionable part of the message; under (B)
//     they are the tail of a database name. Neither is a credential, so the
//     tail is kept (host-pattern checked) -- but only when the query holds
//     no '@' of its own, because a '@' inside "?password=p@ss" makes the
//     tail a fragment of that password instead.
//   - Query: rebuilt from the allowlist by redactQuery, on this path and
//     the unambiguous one alike.
//   - Fragment: neither library reads one; it is replaced by the marker if
//     present so its bytes are never echoed.
func renderParsed(u *url.URL, p policy, ambiguous bool) string {
	query := redactQuery(u.RawQuery, p)
	if !ambiguous {
		// Nothing is in doubt: rebuild from net/url's own components, which
		// re-escapes exactly as net/url would have written the URL.
		v := *u
		v.RawQuery = query
		v.ForceQuery = false
		if v.Fragment != "" {
			v.Fragment, v.RawFragment = redactedMarker, ""
		}
		return v.String()
	}
	// The ambiguous rendering is assembled directly rather than through
	// url.URL.String(), because every piece of it has already been
	// classified and escaped by the function that produced it (a marker, a
	// hostPortPattern-checked fragment, or redactQuery's own QueryEscape
	// output) -- and because String() would percent-escape an IPv6 literal
	// recovered into the path position ("[::1]:9000" as "%5B::1%5D:9000"),
	// mangling the one part of the message the operator needs to read.
	var b strings.Builder
	if u.User != nil {
		// The userinfo was already reduced to what is safe under both
		// readings (see redactOne); the host was not, and here it cannot
		// be -- it is either the real host or the middle of the password,
		// with nothing to tell them apart. Only the path tail below can
		// recover an endpoint.
		b.WriteString(join(u.Scheme, u.User.String()+"@"+redactedMarker))
	} else {
		b.WriteString(join(u.Scheme, ambiguousHost(u.Host, p)))
	}
	if u.Path != "" {
		b.WriteString("/" + redactedMarker)
		if tail, ok := hostAfterLastAt(u.Path); ok &&
			!strings.ContainsRune(u.RawQuery, '@') &&
			!strings.ContainsRune(u.Fragment, '@') {
			b.WriteString("@" + tail)
		}
	}
	if query != "" {
		b.WriteString("?" + query)
	}
	if u.Fragment != "" {
		b.WriteString("#" + redactedMarker)
	}
	return b.String()
}

// ambiguousHost renders the authority of a URL whose '@' could not be
// accounted for -- see renderParsed. It keeps what is a host under one
// reading and a username under the other, and nothing else: everything from
// the first ':' onwards is replaced, because under the reading where the
// authority is really a userinfo, that is where the password starts.
//
// An IPv6 literal is the one shape where the port is unambiguous, since the
// brackets delimit the address; the port is still dropped, because the
// bytes after ']' are as unaccounted-for as any other.
func ambiguousHost(host string, p policy) string {
	if host == "" {
		return redactedMarker
	}
	if strings.HasPrefix(host, "[") {
		i := strings.Index(host, "]")
		if i < 0 {
			return redactedMarker
		}
		literal := host[:i+1]
		if !hostPortPattern.MatchString(literal) {
			return redactedMarker
		}
		if i+1 < len(host) {
			return literal + ":" + redactedMarker
		}
		return literal
	}
	name, _, hasPort := strings.Cut(host, ":")
	if !hostPortPattern.MatchString(name) {
		return redactedMarker
	}
	if !hasPort && p.bareUserinfoIsCredential {
		// A NATS authority with no ':' is, under the reading where this
		// really is a userinfo, a complete auth token rather than a
		// username -- so unlike the ClickHouse case there is nothing here
		// that is safe under both readings.
		return redactedMarker
	}
	if hasPort {
		return name + ":" + redactedMarker
	}
	return name
}

// hasStrayAt reports whether a parsed URL carries an '@' anywhere net/url
// did not read as a userinfo separator -- in the path, the query or the
// fragment. Those are the positions from which an '@' could have been the
// *real* separator of a credential whose unencoded '/', '?' or '#' moved
// the authority boundary.
//
// It reads EscapedPath rather than Path so that a percent-encoded "%40",
// which net/url has already decoded into Path, is not mistaken for a
// literal '@' and does not cost an ordinary URL its host.
func hasStrayAt(u *url.URL) bool {
	return strings.ContainsRune(u.EscapedPath(), '@') ||
		strings.ContainsRune(u.RawQuery, '@') ||
		strings.ContainsRune(u.Fragment, '@')
}

// hostAfterLastAt returns the host-shaped fragment following the last '@'
// in s, if there is one and it passes hostPortPattern. It is how the true
// host is recovered from a string whose password swallowed the authority
// boundary; every byte it returns has been matched against the pattern.
func hostAfterLastAt(s string) (string, bool) {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return "", false
	}
	tail := s[at+1:]
	if end := strings.IndexAny(tail, "/?#"); end >= 0 {
		tail = tail[:end]
	}
	if !hostPortPattern.MatchString(tail) {
		return "", false
	}
	return tail, true
}

// redactQuery rebuilds a URL's query from the parameters policy recognizes,
// rather than echoing the operator's query string.
//
// This is the same shape as the userinfo leaks above: clickhouse-go's
// fromDSN accepts the password as a query parameter, so
//
//	clickhouse://host:9000/db?password=SECRET
//
// is a complete, working credential with no '@' in it anywhere -- and the
// renderer, having correctly determined that there was no userinfo to
// strip, handed back u.String() with RawQuery intact. It is also the form
// an operator reaches for precisely *because* their password broke the
// userinfo syntax, so the population hitting it is the population whose
// passwords are hardest to redact.
//
// The rule is the same additive one the rest of this package follows. A
// parameter's value is echoed only if the parameter is on the policy's safe
// list; a parameter the library reads as a credential keeps its name and
// loses its value; and anything else -- including a query net/url's own
// ParseQuery cannot make sense of -- collapses into a single marker, name
// included, since an unrecognized parameter name is as much operator input
// as its value.
func redactQuery(rawQuery string, p policy) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		// ParseQuery returns whatever it managed to parse alongside its
		// error. None of it has been accounted for, so none of it is
		// echoed.
		return redactedMarker
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names)+1)
	dropped := false
	for _, name := range names {
		switch {
		case p.safeQueryParams[name]:
			for _, v := range values[name] {
				parts = append(parts, url.QueryEscape(name)+"="+url.QueryEscape(v))
			}
		case p.secretQueryParams[name]:
			parts = append(parts, url.QueryEscape(name)+"="+redactedMarker)
		default:
			dropped = true
		}
	}
	if dropped {
		parts = append(parts, redactedMarker)
	}
	return strings.Join(parts, "&")
}

// redactUnparsed renders a connection string that net/url.Parse rejected.
// It never returns rawURL, and never returns a substring of rawURL that it
// has not matched against schemePattern or hostPortPattern; anything it
// cannot place is replaced by redactedMarker. A string it can make no sense
// of at all renders as redactedMarker alone.
func redactUnparsed(rawURL string, p policy) string {
	scheme, rest := "", rawURL
	if before, after, ok := strings.Cut(rawURL, "://"); ok {
		if !schemePattern.MatchString(before) {
			// Whatever precedes "://" is not a scheme, so this string has
			// no structure to reason about and nothing in it can be
			// vouched for.
			return redactedMarker
		}
		scheme, rest = before, after
	}

	// The authority is what precedes the first '/', '?' or '#', by RFC 3986
	// and by net/url's own reading of it.
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}

	switch {
	case strings.ContainsRune(authority, '@'):
		// A well-formed authority: the last '@' in it is the userinfo/host
		// separator ("last" so that an '@' inside the password lands on the
		// dropped side), so no password byte can follow it. Only what
		// follows it is a candidate for being echoed, and only if
		// hostPortPattern accepts it.
		host, ok := hostAfterLastAt(authority)
		if !ok {
			return join(scheme, redactedMarker)
		}
		return join(scheme, redactedMarker+"@"+host)

	case !strings.ContainsRune(rest, '@'):
		// No '@' anywhere: nothing in this string is in userinfo position,
		// so the authority is a host and the parse failed for some other
		// reason (a control character in the query, say). The query and
		// path are dropped either way -- this function echoes an authority
		// and nothing else.
		if !hostPortPattern.MatchString(authority) {
			return join(scheme, redactedMarker)
		}
		return join(scheme, authority)

	default:
		// There is an '@', but not in the authority: the same ambiguity
		// renderParsed documents, in a string that did not parse. The tie
		// is broken by asking net/url whether the authority is a
		// well-formed one *on its own*:
		//
		//   - If it is, then the string's structure is intact up to the end
		//     of the authority and the parse failed further right (in a
		//     query, say -- which is where a "?password=p@ss" lives). The
		//     authority is the host; the tail after the '@' is query bytes
		//     and may be a password fragment, so it is dropped.
		//   - If it is not -- "vantage:trailSECRET" is not a valid
		//     authority, and net/url will have said so ("invalid port") --
		//     then those bytes are not a host under any reading, and the
		//     '@' further right is the userinfo separator of a password
		//     that swallowed the boundary. The tail is the true host.
		//
		// Neither branch echoes anything it has not classified; the choice
		// is only about which side of the '@' is the host.
		if scheme != "" && authority != "" {
			if _, err := url.Parse(scheme + "://" + authority); err == nil {
				return join(scheme, ambiguousHost(authority, p))
			}
		}
		host, ok := hostAfterLastAt(rest)
		if !ok || atIsInsideAQueryValue(rest) {
			return join(scheme, ambiguousHost(authority, p))
		}
		return join(scheme, redactedMarker+"@"+host)
	}
}

// atIsInsideAQueryValue reports whether the last '@' in rest sits where a
// query parameter's *value* would be: after a '?', with a '=' between that
// '?' and the '@'.
//
// It is the second half of redactUnparsed's tie-break, for the case its
// first half cannot decide: a string whose authority is malformed *and*
// whose password lives in a query parameter -- "clickhouse://ho st:9000/db
// ?password=p@ssSECRET" -- has an unparseable authority (so the authority
// is not a host under any reading) and an '@' that is nonetheless part of a
// credential rather than a userinfo separator. Echoing the fragment after
// it would echo the tail of that password.
//
// The condition is a positive one, in the direction of *not* echoing: the
// tail is emitted only when this function can see that the '@' is not in a
// value position. A password that itself contains "?...=" declines the tail
// and costs the host, which is the safe way to be wrong.
func atIsInsideAQueryValue(rest string) bool {
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return false
	}
	before := rest[:at]
	q := strings.LastIndex(before, "?")
	if q < 0 {
		return false
	}
	return strings.ContainsRune(before[q:], '=')
}

// join renders a scheme and an already-redacted authority as a URL. A
// segment with no scheme (NATS's schemeless shorthand reaches this function
// only through natsSegmentURL, which prepends one, but URL's own callers
// may not) renders as the authority alone rather than growing a "://" the
// operator never wrote.
func join(scheme, authority string) string {
	if scheme == "" {
		return authority
	}
	return scheme + "://" + authority
}

// natsServerSegments splits a Config.NatsURL value into the individual
// server URLs nats.Connect will actually parse, replicating nats.go's own
// processUrlString exactly (split on ',', trim whitespace and a trailing
// '/', drop empty segments) so validation and redaction operate on the same
// units the library does. A single-server value (the common case) degrades
// to a one-element slice.
func natsServerSegments(rawURL string) []string {
	parts := strings.Split(rawURL, ",")
	segs := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSuffix(strings.TrimSpace(s), "/")
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// NatsURL is URL's segment-aware equivalent for Config.NatsURL: it splits
// on the NATS multi-server comma syntax (see natsServerSegments), redacts
// each segment independently with URL, and rejoins with ",". A value with
// no comma redacts the same as a direct URL call, modulo natsServerSegments'
// own whitespace/trailing-slash trimming.
func NatsURL(rawURL string) string {
	segs := natsServerSegments(rawURL)
	if len(segs) == 0 {
		return natsSegmentURL(rawURL)
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = natsSegmentURL(s)
	}
	return strings.Join(out, ",")
}

// natsSegmentURL redacts exactly one NATS server URL -- one element of
// natsServerSegments' output -- as opposed to URL, which knows nothing about
// NATS. It differs from URL in the two ways nats.go's own parsing does:
//
//   - It applies withNatsScheme first. This is the half of the fix that
//     closes the schemeless-shorthand leak. CheckNatsURL already prepended
//     "nats://" before *validating* a segment, but NatsURL did not prepend
//     it before *redacting* one, so the validator and the renderer were
//     looking at different strings: the validator saw a well-formed
//     "nats://user:pass@host:4222" and passed it, while the renderer saw
//     the raw "user:pass@host:4222", which net/url reads as an opaque URL
//     with no userinfo -- nothing to strip -- and the password went to the
//     log verbatim. The two now prepend identically, so a segment can no
//     longer be judged safe in one shape and rendered in another.
//   - It treats a colonless userinfo as a credential, because nats.go does:
//     connectProto uses the username as an auth token when the URL carries
//     no password (see redactOne). "nats://sometoken@host:4222" is a
//     complete NATS credential, not a username.
func natsSegmentURL(seg string) string {
	if strings.TrimSpace(seg) == "" {
		// Nothing configured, nothing to redact. Without this, the prepend
		// below would turn "" into the noise "nats:" (net/url renders a
		// scheme-only URL that way) instead of the empty value the operator
		// actually set.
		return ""
	}
	return redactOne(withNatsScheme(seg), natsPolicy)
}

// withNatsScheme mirrors nats.go's parseServerURL: a segment with no "://"
// gets the default "nats" scheme before it is parsed. The prepended scheme
// is kept in the rendered output rather than stripped back off, because it
// is what nats.Connect will actually use -- a log line naming
// "nats://host:4222" for a configured "host:4222" describes the connection
// that was really attempted.
func withNatsScheme(seg string) string {
	if strings.Contains(seg, "://") {
		return seg
	}
	return natsDefaultScheme + "://" + seg
}

// CheckURL parses rawURL with net/url.Parse and returns a redaction-safe
// error if that fails, *before* the caller ever hands rawURL to
// nats.Connect or sink.NewClickHouse. This exists for two reasons that are
// no longer about safety alone:
//
//   - Fast, clear failure. A malformed connection string otherwise surfaces
//     as a confusing dial/TLS/timeout error only after a real network
//     attempt, once it gets far enough into the library to actually try
//     connecting with whatever garbage its parser salvaged from the input.
//   - Defense in depth alongside Err (below), which is now the mechanism
//     that actually guarantees no credential reaches a log line: Err works
//     by recognizing *url.Error structurally, regardless of what any
//     library's parser accepts or rejects, so CheckURL's own grammar no
//     longer needs to agree with a library's for that guarantee to hold.
//
// For NATS this validator's grammar genuinely is nats.go's: nats.go's own
// URL parsing calls net/url.Parse directly (confirmed by reading
// nats.go@v1.54.0's parseServerURL), the same function this validator
// calls, so CheckURL (used per-segment via CheckNatsURL below) accepts and
// rejects exactly what nats.Connect would.
//
// For ClickHouse it is not identical: clickhouse-go/v2's Options.fromDSN
// parses with lib/churl.Parse, an internal fork of net/url.Parse that the
// package documents as modified to support ClickHouse's multi-host DSN
// syntax. Diffed against this validator for '"', '\', control characters,
// and combinations of them under the pinned clickhouse-go v2.48.0: no
// divergence found. That is a fact about this version, not a guarantee
// about the next one -- churl could accept or reject some future input
// differently than net/url does, and if it ever does, CheckURL alone could
// pass a string through to clickhouse-go that clickhouse-go then rejects
// (or vice versa, rejecting one clickhouse-go would accept -- an
// availability annoyance, not a leak). Either way, that divergence is not a
// credential-safety hole: churl.Parse's own source constructs a literal
// *net/url.Error on every failure path (verified directly, not inferred),
// so whatever churl's grammar decides, a failure still reaches Err below in
// a form it handles soundly.
func CheckURL(rawURL string) error {
	if _, err := url.Parse(rawURL); err != nil {
		return fmt.Errorf("%s: invalid URL", URL(rawURL))
	}
	return nil
}

// CheckClickHouseDSN is the validator for a ClickHouse DSN: CheckURL, plus
// the one parameter whose *value* is a second URL that clickhouse-go will
// parse and quote back at us (CheckClickHouseProxy). Use it, not CheckURL,
// for a DSN -- secret.ClickHouseDSN.Validate does.
func CheckClickHouseDSN(rawURL string) error {
	if err := CheckURL(rawURL); err != nil {
		return err
	}
	return CheckClickHouseProxy(rawURL)
}

// CheckClickHouseProxy parses the "http_proxy" parameter of a ClickHouse
// DSN itself and returns a redacted error if it is malformed, so that
// clickhouse-go is never handed a value it would echo. sink.NewClickHouse
// calls it immediately before clickhouse.ParseDSN for exactly that reason:
// this is a pre-validation, not a scrub of library text, because a scrub
// of library text catches only the shapes its author anticipated.
//
// It is needed because clickhouse-go's Options.fromDSN (v2.48.0) does
//
//	proxyURL, err := url.Parse(params.Get(v))
//	if err != nil {
//	    return fmt.Errorf("clickhouse [dsn parse]: http_proxy: %s", err)
//	}
//
// -- "%s", not "%w". That flattens the *url.Error into text and wraps
// nothing, so Err's errors.As finds no *url.Error and returns the message
// untouched, complete with the proxy's password:
//
//	clickhouse: parse dsn: clickhouse [dsn parse]: http_proxy:
//	  parse "http://u:PASSWORD@proxy:8o80": invalid port ":8o80" after host
//
// Two ordinary inputs reach it, both reproduced against the pinned
// clickhouse-go before this function existed: a port that is not a number
// ("proxy:8o80"), and a space in the proxy's host. Neither is caught by
// CheckURL, because the DSN *containing* them is perfectly well-formed.
//
// The irony worth not repeating: "http_proxy" was already on
// clickHousePolicy's secretQueryParams list, so the renderer knew the
// parameter carries a credential from when it was added. Only the
// error path did not.
func CheckClickHouseProxy(rawURL string) error {
	proxy := dsnParam(rawURL, "http_proxy")
	if proxy == "" {
		return nil
	}
	_, err := url.Parse(proxy)
	if err == nil {
		return nil
	}
	// url.Parse's failures are always *url.Error, so safeURLError -- which
	// rebuilds the message out of the *redacted* URL rather than quoting
	// the library's text -- normally applies, and keeps the reason ("invalid
	// port", "invalid character in host name") that makes the error
	// actionable. The fallback is for a future net/url that returns some
	// other error type: it names the parameter and the redacted value, and
	// says no more.
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("http_proxy: %s", safeURLError(uerr, httpProxyPolicy))
	}
	return fmt.Errorf("http_proxy %s: invalid URL", redactOne(proxy, httpProxyPolicy))
}

// dsnParam reads one query parameter out of a connection string the way the
// library that will consume it does, without depending on this package's
// parser accepting the string as a whole.
//
// clickhouse-go reads its parameters from Query() on the URL churl.Parse
// returned -- and churl.Parse returns a *net/url.URL, so that is stdlib
// url.URL.Query(): url.ParseQuery(RawQuery) with the error discarded, which
// is why this function discards it too. Discarding matters rather than
// merely matching: ParseQuery returns every pair it did parse alongside its
// error, so a malformed pair elsewhere in the query must not be allowed to
// hide a well-formed http_proxy from this check when it would not hide it
// from the driver.
//
// RawQuery is split lexically, and identically, by net/url and by churl:
// cut at the first '#', then cut at the first '?' (net/url's parse and
// churl.Parse both, verified by reading churl@v2.48.0). Splitting here
// rather than calling url.Parse on the whole DSN is what makes this check
// independent of whether *our* parser accepts the DSN: churl is a fork, and
// a DSN churl accepts and net/url does not must still be checked, not
// silently skipped.
func dsnParam(rawURL, name string) string {
	if i := strings.IndexByte(rawURL, '#'); i >= 0 {
		rawURL = rawURL[:i]
	}
	i := strings.IndexByte(rawURL, '?')
	if i < 0 {
		return ""
	}
	q, _ := url.ParseQuery(rawURL[i+1:])
	return q.Get(name)
}

// CheckNatsURL is CheckURL's segment-aware equivalent for Config.NatsURL --
// see natsServerSegments and NatsURL's doc comments for why NATS's
// comma-separated multi-server syntax needs one. A segment with no "://" is
// given nats.go's own default scheme ("nats") before validation, mirroring
// parseServerURL's identical prepend, so a bare "host:port" segment (valid
// NATS shorthand) is not rejected here only because this package validates
// it more strictly than nats.Connect actually would.
func CheckNatsURL(rawURL string) error {
	for _, s := range natsServerSegments(rawURL) {
		if _, err := url.Parse(withNatsScheme(s)); err != nil {
			// natsSegmentURL, not URL: the rejected segment is a NATS
			// segment, and must be rendered by the same rules (default
			// scheme, token-shaped userinfo) that were used to judge it.
			return fmt.Errorf("%s: invalid URL", natsSegmentURL(s))
		}
	}
	return nil
}

// Err makes err safe to log by finding a *url.Error anywhere in its chain
// (via errors.As, so any depth of %w-wrapping is followed) and replacing
// every occurrence of that error's own URL field -- both literal and
// strconv.Quote-escaped, the only two forms *url.Error.Error() can ever
// render it in, see this package's doc comment -- with its redacted form,
// everywhere in err's full message. The rest of the message (an outer
// "clickhouse: parse dsn:" prefix, for instance) is left intact, so
// diagnostic context survives; only the credential-bearing fragment does
// not.
//
// If err does not wrap a *url.Error, it is returned unchanged. For almost
// every failure path this package's callers hit -- a refused dial, a ping
// timeout, ClickHouse's own schema-version mismatch -- that is exactly
// right: none embeds the connection string, so there is nothing to redact.
//
// # The known residual, stated at its real size
//
// There is one exception, and it is deliberately described here in full,
// because an understated residual is one that stops getting fixed. When a
// password contains an '@', the parser splits the authority at the *last*
// '@' inside it -- and the authority ends at the first '/', '?' or '#' --
// so the bytes between the password's first '@' and that delimiter are read
// as the host, and handed to the dialer. net.Dial then quotes them in an
// ordinary dial error that wraps no *url.Error at all:
//
//	clickhouse: ping: dial tcp: address <fragment>: missing port in address
//
// What escapes is the entire password fragment between its first '@' and
// the next '/', '?' or '#' -- not a couple of bytes. Measured at 38
// characters, unchanged across all three delimiters, and the length is a
// property of the password, not of this code: a longer fragment leaks
// proportionally more. It applies to the NATS path in both daemons exactly
// as it applies to the ClickHouse path in vantage-writer, since nats.go
// parses each server segment with the same net/url and dials what it gets.
// Both were reproduced directly, not reasoned about.
//
// It is unreachable from here by design, not by oversight: the message
// carries no *url.Error, so there is nothing to recognize structurally, and
// the only ways to close it are to scrub library text (unsound -- see this
// package's design history) or to discard the dial diagnosis for every
// error, which would cost every operator the one line that tells them what
// actually failed. Closing it properly means not building an authority the
// dialer can misread in the first place, i.e. rejecting an '@' in a
// password at config load. That is a behavior change to argue on its own
// merits, not something to slip in here.
//
// Deliberately not parameterized on the raw config value: a config value
// is not reliably the exact string that failed to parse (Config.NatsURL can
// be a multi-segment value; only one segment is what any single parse call
// ever sees), so matching against it misses the failing segment. e.URL is,
// unconditionally, the exact string net/url.Parse (or churl.Parse) was
// actually asked to parse -- reading it from the error is what makes this
// correct regardless of how many servers Config.NatsURL names or which one
// failed.
func Err(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}

	safe := safeURLError(uerr, clickHousePolicy)
	msg := err.Error()

	// Replace the *url.Error's entire rendering, not just its URL field.
	// Substituting only the URL field leaves uerr.Err's own text in place,
	// and that text quotes fragments of the input: net/url reports a bad
	// authority as
	//
	//	invalid port ":trailSECRET" after host
	//
	// where ":trailSECRET" is part of a password that contained a '/'. The
	// URL field would be correctly redacted and the password would go to the
	// log anyway, one field over.
	inner := uerr.Error()
	if !strings.Contains(msg, inner) {
		// A wrapper rendered the *url.Error some way other than by calling
		// its Error() method, so we cannot say which part of msg came from
		// it. Return the rebuilt rendering alone rather than reproduce text
		// we cannot account for; the outer context is worth less than the
		// guarantee.
		return errors.New(safe)
	}
	msg = strings.ReplaceAll(msg, inner, safe)

	// And, as before, a wrapper may have embedded uerr.URL itself
	// independently of calling Error() -- in either of the two forms it can
	// take, literal or strconv.Quote-escaped.
	redacted := URL(uerr.URL)
	msg = strings.ReplaceAll(msg, uerr.URL, redacted)
	msg = strings.ReplaceAll(msg, strconv.Quote(uerr.URL), strconv.Quote(redacted))
	return errors.New(msg)
}

// safeURLError renders a *url.Error from scratch, out of pieces that cannot
// contain a credential, rather than editing the error's own text.
//
// The Op is the library's own constant ("parse"), never operator input. The
// URL is redactOne(uerr.URL, p), which fails closed -- p is the policy for
// whichever library's URL this is; Err passes clickHousePolicy and
// CheckClickHouseProxy passes httpProxyPolicy. The reason is the only
// interesting part: uerr.Err's message may quote arbitrary fragments of the
// input (see Err), so it cannot be reproduced. Instead the *already
// redacted* URL is handed back to net/url.Parse:
//
//   - If it still fails, the resulting message is derived entirely from a
//     string that is already credential-free, so it is safe by
//     construction, and it explains the same structural fault the operator
//     needs to know about -- a malformed IPv6 literal still reports
//     "missing ']' in host".
//   - If it now parses cleanly, the fault was in something redaction
//     removed (the userinfo, or a path/query dropped with it). Saying so
//     is both safe and accurate; quoting the reason would mean quoting the
//     credential.
//
// This keeps the diagnosis without ever passing library-authored text
// derived from the raw input through to a log line.
func safeURLError(uerr *url.Error, p policy) string {
	redacted := redactOne(uerr.URL, p)
	reason := "invalid URL (reason omitted: it quotes the redacted part of the URL)"
	if _, err := url.Parse(redacted); err != nil {
		if reparsed, ok := errors.AsType[*url.Error](err); ok {
			reason = reparsed.Err.Error()
		} else {
			reason = err.Error()
		}
	}
	return fmt.Sprintf("%s %q: %s", uerr.Op, redacted, reason)
}
