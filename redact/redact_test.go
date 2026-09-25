package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestURLStripsPassword pins the fix for a startup connection failure that
// formats a raw NatsURL/ClickHouseDSN straight into an
// error that reaches slog.Error("fatal", ...), and both are userinfo URLs
// in general -- the dev stack's own default ClickHouseDSN literally is one.
// Without redaction, any failure to reach NATS or ClickHouse writes the
// password to stdout, exactly where an operator debugging a startup failure
// would copy it into a ticket.
func TestURLStripsPassword(t *testing.T) {
	for _, c := range []struct {
		name, in, want, password string
	}{
		{
			"clickhouse dsn with credentials",
			"clickhouse://vantage:vantage@clickhouse:9000/vantage",
			"clickhouse://vantage@clickhouse:9000/vantage",
			"", // username and password are both "vantage" in the dev
			// default, so a substring check would trivially "pass" by
			// finding the kept username; the exact-match assertion above
			// already proves the ":vantage@" (colon-prefixed, i.e. the
			// password position) is gone.
		},
		{
			"nats url with a password distinct from its username",
			"nats://user:s3cr3t@nats.internal:4222",
			"nats://user@nats.internal:4222",
			"s3cr3t",
		},
		{
			"no credentials at all",
			"nats://nats:4222",
			"nats://nats:4222",
			"",
		},
		{
			"username only, no password",
			"clickhouse://vantage@127.0.0.1:9000/vantage",
			"clickhouse://vantage@127.0.0.1:9000/vantage",
			"",
		},
		{
			// The fallback path must not leak the password if it is ever
			// hit. It used to render this as "not a url://host:1", echoing
			// the unparseable prefix back because a regexp had removed the
			// userinfo from it; URL now refuses to vouch for "not a url" as
			// a scheme (it is not one) and so declines to reproduce any of
			// this input, host included. Nothing here was ever a real
			// endpoint to be actionable about.
			"malformed url that still carries a userinfo-shaped prefix",
			"not a url://user:pass@host:1",
			"REDACTED",
			"pass",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := URL(c.in)
			if got != c.want {
				t.Errorf("URL(%q) = %q, want %q", c.in, got, c.want)
			}
			if c.password != "" && strings.Contains(got, c.password) {
				t.Errorf("URL(%q) = %q, still contains the password %q", c.in, got, c.password)
			}
		})
	}
}

// TestNatsURLRedactsAllSegments is the regression test for NATS's
// documented multi-server syntax -- a comma-separated list of full URLs in
// one Config.NatsURL string -- defeating single-URL redaction:
// URL(rawURL) parses the whole joined string as one URL, and net/url.Parse
// happily absorbs a second "nats://user:pass@host" fragment into the
// Host/Path of one bogus URL instead of failing, so only the first
// segment's password was ever actually removed. This asserts every segment
// in a 3-server value is redacted, not just the first, with a distinct
// password per segment so a redaction that only handled position 0 would be
// caught immediately.
func TestNatsURLRedactsAllSegments(t *testing.T) {
	raw := "nats://user:pass0@host0:4222,nats://user:pass1@host1:4222,nats://user:pass2@host2:4222"
	got := NatsURL(raw)
	want := "nats://user@host0:4222,nats://user@host1:4222,nats://user@host2:4222"
	if got != want {
		t.Errorf("NatsURL(%q) = %q, want %q", raw, got, want)
	}
	for _, pw := range []string{"pass0", "pass1", "pass2"} {
		if strings.Contains(got, pw) {
			t.Errorf("NatsURL(%q) = %q, still contains %q", raw, got, pw)
		}
	}
}

// TestCheckURLRejectsInvalidUserinfo pins CheckURL's whole reason for
// existing: a password containing '"' or '\' makes net/url.Parse itself
// reject the URL ("net/url: invalid userinfo") -- both clickhouse-go's DSN
// parser and nats.go's URL parser hit the identical failure internally,
// since both call net/url.Parse (or, for clickhouse-go, an internal fork
// that agrees on this input -- see CheckURL's doc comment) on the same
// string -- so CheckURL fails on exactly the same inputs those libraries
// would fail on, and does so before either library, or its error text, is
// ever reached.
func TestCheckURLRejectsInvalidUserinfo(t *testing.T) {
	for _, c := range []struct {
		name, in string
	}{
		{"clickhouse dsn, quote in password", `clickhouse://vantage:sup"secret@clickhouse:9000/vantage`},
		{"clickhouse dsn, backslash in password", `clickhouse://vantage:sup\secret@clickhouse:9000/vantage`},
		{"nats url, quote in password", `nats://user:sup"secret@nats.internal:4222`},
		{"nats url, backslash in password", `nats://user:sup\secret@nats.internal:4222`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckURL(c.in)
			if err == nil {
				t.Fatalf("CheckURL(%q) returned a nil error, want a rejection", c.in)
			}
			// CheckURL's error is built from URL(rawURL), and rawURL itself
			// fails to parse here (same as the input to CheckURL), so URL
			// falls back to stripping the whole userinfo, not just the
			// password -- "secret" (the distinguishing tail of every
			// password in this table) must not survive that in any form.
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("CheckURL(%q) = %q, still contains the password", c.in, err.Error())
			}
		})
	}
}

func TestCheckURLAcceptsWellFormedURLs(t *testing.T) {
	for _, in := range []string{
		"clickhouse://vantage:vantage@127.0.0.1:9000/vantage",
		"nats://nats:4222",
		"nats://user:pass@nats.internal:4222",
	} {
		if err := CheckURL(in); err != nil {
			t.Errorf("CheckURL(%q) = %v, want nil", in, err)
		}
	}
}

// TestCheckNatsURLRejectsBadSegmentAtAnyPosition is the regression test:
// an unsplit CheckURL never fires on a multi-server value, because it
// validates the whole comma-joined string as a single URL, which
// net/url.Parse accepts (folding later segments into the bogus URL's
// Host/Path) rather than rejects, so a malformed later segment's password
// reached nats.Connect and then its own error text unredacted. This covers
// the shape, not one instance: a bad segment (quote-in-password, the most
// direct trigger -- see TestCheckURLRejectsInvalidUserinfo) in first,
// middle and last position of a 3-server value must be rejected in every
// case, with no segment's password surviving in the rejection message.
func TestCheckNatsURLRejectsBadSegmentAtAnyPosition(t *testing.T) {
	good := []string{
		"nats://user:pass0@host0:4222",
		"nats://user:pass1@host1:4222",
		"nats://user:pass2@host2:4222",
	}
	bad := `nats://user:sup"secret@bad-host:4222`

	for pos := range 3 {
		t.Run(fmt.Sprintf("position %d of 3", pos), func(t *testing.T) {
			segs := append([]string{}, good...)
			segs[pos] = bad
			raw := strings.Join(segs, ",")

			err := CheckNatsURL(raw)
			if err == nil {
				t.Fatalf("CheckNatsURL(%q) returned a nil error, want a rejection", raw)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("CheckNatsURL(%q) = %q, still contains the password", raw, err.Error())
			}
			for _, pw := range []string{"pass0", "pass1", "pass2"} {
				if strings.Contains(err.Error(), pw) {
					t.Errorf("CheckNatsURL(%q) = %q, still contains a good segment's password %q",
						raw, err.Error(), pw)
				}
			}
		})
	}
}

// TestCheckNatsURLRejectsBackslashSegment is TestCheckNatsURLRejectsBadSegmentAtAnyPosition's
// sibling for the second trigger character, at a middle position (the
// position most likely to be missed by an implementation that only checks
// the first or last segment).
func TestCheckNatsURLRejectsBackslashSegment(t *testing.T) {
	raw := `nats://user:pass0@host0:4222,nats://user:sup\secret@bad-host:4222,nats://user:pass2@host2:4222`
	err := CheckNatsURL(raw)
	if err == nil {
		t.Fatalf("CheckNatsURL(%q) returned a nil error, want a rejection", raw)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("CheckNatsURL(%q) = %q, still contains the password", raw, err.Error())
	}
}

// TestCheckNatsURLCatchesControlCharacterInNonFirstSegment pins a case that
// already worked before this package became segment-aware, and must not
// regress while fixing the userinfo-quoting class above: net/url.Parse (and
// churl.Parse) both reject any control byte anywhere in the input as their
// very first check, over the whole string, before any authority/scheme
// splitting -- so a control character was always caught regardless of
// position, even by the old whole-string CheckURL. This proves the
// segment-aware CheckNatsURL still catches it too, not merely that it now
// also splits.
func TestCheckNatsURLCatchesControlCharacterInNonFirstSegment(t *testing.T) {
	raw := "nats://user:pass0@host0:4222,nats://user:pass\x01bad@host1:4222,nats://user:pass2@host2:4222"
	if err := CheckNatsURL(raw); err == nil {
		t.Fatal("CheckNatsURL with a control character in the middle segment returned nil, want a rejection")
	}
}

func TestCheckNatsURLAcceptsWellFormedMultiServerURL(t *testing.T) {
	for _, raw := range []string{
		"nats://user:pass@host0:4222,nats://user:pass@host1:4222",
		"nats://host0:4222,nats://host1:4222",   // no credentials at all
		"host0:4222,host1:4222",                 // bare host:port, NATS shorthand -- parseServerURL prepends "nats://" before parsing, and CheckNatsURL must do the same or it over-rejects a legal config
		"nats://host0:4222, nats://host1:4222/", // NATS tolerates whitespace after the comma and a trailing slash; processUrlString trims both
	} {
		if err := CheckNatsURL(raw); err != nil {
			t.Errorf("CheckNatsURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestErrNilErrorReturnsNil(t *testing.T) {
	if got := Err(nil); got != nil {
		t.Errorf("Err(nil) = %v, want nil", got)
	}
}

// TestErrLeavesUnrelatedErrorsUnchanged confirms Err's scope boundary,
// stated in its doc comment: an error that does not wrap a *url.Error --
// a refused dial, a ping timeout -- passes through completely untouched,
// identity and all, rather than being needlessly rewritten.
//
// "Untouched" is the known residual as well as the design, and Err's doc
// comment now states its real size: a dial error can quote a fragment of
// the connection string (the whole password fragment between its first '@'
// and the next '/', '?' or '#', measured at 38 characters, on the NATS path
// as well as the ClickHouse one). This test pins the pass-through; it is
// not evidence that nothing passes through worth redacting.
func TestErrLeavesUnrelatedErrorsUnchanged(t *testing.T) {
	base := errors.New("dial tcp 1.2.3.4:9000: connect: connection refused")
	if got := Err(base); got != base {
		t.Errorf("Err(base) = %v (%p), want the same error value back, unchanged (%p)", got, got, base)
	}
}

// TestErrStripsMalformedHostURL is Err's basic case: a *url.Error from a
// plain malformed host (no credentials involved) must still have its URL
// field redacted -- URL's own fallback path can't safely split username
// from password in a string that failed to parse, so it strips the whole
// userinfo (there is none here) and the host/port must still be legible for
// debugging.
func TestErrStripsMalformedHostURL(t *testing.T) {
	raw := "clickhouse://vantage:supersecretpw@[::1:9000/vantage" // malformed: unterminated IPv6 literal
	_, parseErr := url.Parse(raw)
	if parseErr == nil {
		t.Fatal("test setup: expected url.Parse to fail on a malformed IPv6 host, got nil error")
	}

	got := Err(parseErr)
	if strings.Contains(got.Error(), "supersecretpw") {
		t.Errorf("Err(%v) = %q, still contains the raw password", parseErr, got.Error())
	}
	if !strings.Contains(got.Error(), "9000") {
		t.Errorf("Err(...) = %q, want the host/port still present for debuggability", got.Error())
	}
}

// TestErrStripsQuoteEscapedPassword is the regression test for the
// %q-escaping bypass that broke the first version of Err:
// net/url.Error.Error() renders its
// URL field via %q (strconv.Quote), which escapes '"' and '\' in the
// process. So when the raw string's password contains either character, the
// %q-rendered error text contains the *escaped* form -- e.g. sup\"secret --
// while the raw string has the *unescaped* form -- sup"secret -- and a
// plain strings.ReplaceAll(msg, raw, ...) finds no match at all. This
// reproduces that exact shape against the real net/url package (not a
// hand-built error) for both trigger characters.
func TestErrStripsQuoteEscapedPassword(t *testing.T) {
	for _, c := range []struct {
		name, raw, password string
	}{
		{"password contains a double quote", `clickhouse://vantage:sup"secret@clickhouse:9000/vantage`, `sup"secret`},
		{"password contains a backslash", `nats://user:sup\secret@nats.internal:4222`, `sup\secret`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, parseErr := url.Parse(c.raw)
			if parseErr == nil {
				t.Fatalf("test setup: expected url.Parse to reject %q, got nil error", c.raw)
			}
			// The %q-escaped form of the password -- e.g. sup\"secret for a
			// literal sup"secret -- is what url.Parse's error actually
			// contains (strconv.Quote inserts a backslash before the '"' or
			// '\', which breaks a *literal* substring match against the raw
			// password). Confirm that escaped form really is present before
			// asserting Err removes it, so this test can't silently degenerate
			// into asserting nothing.
			escapedPassword := strconv.Quote(c.password)
			escapedPassword = escapedPassword[1 : len(escapedPassword)-1] // strip Quote's own surrounding quotes
			if !strings.Contains(parseErr.Error(), escapedPassword) {
				t.Fatalf("test setup: url.Parse's error %q does not contain the %%q-escaped password %q",
					parseErr.Error(), escapedPassword)
			}

			got := Err(parseErr)
			if strings.Contains(got.Error(), escapedPassword) {
				t.Errorf("Err(%v) = %q, still contains the %%q-escaped password %q",
					parseErr, got.Error(), escapedPassword)
			}
			if strings.Contains(got.Error(), c.password) {
				t.Errorf("Err(%v) = %q, still contains the literal password %q",
					parseErr, got.Error(), c.password)
			}
		})
	}
}

// TestErrFindsURLErrorThroughWrapping confirms Err's errors.As traversal
// works through %w-wrapping the way NewClickHouse's real call chain does
// (clickhouse-go's churl.Parse returns a *url.Error unwrapped; NewClickHouse
// wraps it with fmt.Errorf("clickhouse: parse dsn: %w", err)) -- and that
// the outer wrapping context survives redaction, not just the inner
// message: Err substitutes only the dangerous fragment (uerr.URL, in both
// its renderings) within the full err.Error() text, so "clickhouse: parse
// dsn:" is still there to tell an operator *where* this failed.
func TestErrFindsURLErrorThroughWrapping(t *testing.T) {
	raw := `clickhouse://vantage:sup"secret@clickhouse:9000/vantage`
	_, parseErr := url.Parse(raw)
	if parseErr == nil {
		t.Fatal("test setup: expected url.Parse to reject a '\"' in userinfo, got nil error")
	}
	wrapped := fmt.Errorf("clickhouse: parse dsn: %w", parseErr)

	got := Err(wrapped)
	if strings.Contains(got.Error(), "secret") {
		t.Errorf("Err(%v) = %q, still contains the password", wrapped, got.Error())
	}
	if !strings.Contains(got.Error(), "clickhouse: parse dsn:") {
		t.Errorf("Err(%v) = %q, lost the outer wrapping context", wrapped, got.Error())
	}
}

// TestErrIsSoundRegardlessOfSegmentPosition documents why Err, unlike
// CheckNatsURL and NatsURL above, needs no segment-splitting of its own:
// it never inspects the original (possibly multi-segment) config value at
// all. It reads e.URL directly off whichever *url.Error surfaces, and
// nats.go's own parseServerURL calls net/url.Parse on exactly one segment
// at a time (confirmed by reading nats.go@v1.49.0's source) -- so e.URL is
// always already a single segment, never the joined list, regardless of
// where in a multi-server value that segment came from. This constructs the
// *url.Error the way nats.go's own code would for each of three positions
// and confirms Err's behavior is identical in every case -- proving the
// "position" question that matters for CheckNatsURL/NatsURL (which do
// iterate over segments, and could have a position-dependent bug) simply
// does not apply to Err's mechanism.
func TestErrIsSoundRegardlessOfSegmentPosition(t *testing.T) {
	for pos := range 3 {
		t.Run(fmt.Sprintf("position %d of 3", pos), func(t *testing.T) {
			// The exact per-segment string nats.go's parseServerURL would
			// have handed to net/url.Parse for the bad segment at this
			// position -- Err never sees the other two segments at all.
			segment := `nats://user:sup"secret@bad-host:4222`
			_, parseErr := url.Parse(segment)
			if parseErr == nil {
				t.Fatal("test setup: expected url.Parse to reject this segment, got nil error")
			}
			got := Err(parseErr)
			if strings.Contains(got.Error(), "secret") {
				t.Errorf("Err(%v) at simulated position %d = %q, still contains the password",
					parseErr, pos, got.Error())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Class coverage for the fail-closed renderer.
//
// A test that pins the exact string most recently reported broken leaves
// every other shape of the same leak unguarded. The tests below are built
// to fail for a *class* instead:
// every password in trickyPasswords is substituted into every URL shape in
// clickHouseShapes / the NATS forms, at every segment position, and the
// assertion is always the same one -- the sentinel "SECRET", which every
// tricky password contains and no username, host, port, scheme or database
// name in any of these tables does, must not appear anywhere in the rendered
// output.
//
// Asserting the *absence* of the secret rather than the presence of an
// expected string is deliberate: an expected-output assertion passes
// whenever the renderer changes in the way the test author imagined, so a
// future change to how a redacted URL is spelled cannot quietly turn these
// into assertions about nothing. An absence assertion only passes when the
// credential is really gone, however the rest of the line is formatted.
// ---------------------------------------------------------------------------

// secretSentinel is embedded in every password in trickyPasswords. It is the
// single thing every test below greps for.
const secretSentinel = "SECRET"

// trickyPasswords are password shapes that have broken a redactor before, or
// that break one of the structural assumptions a redactor can be tempted to
// make. Each one is a plausible output of a password generator or a
// human-chosen passphrase -- none of them is exotic.
var trickyPasswords = []struct{ name, password string }{
	// The shape that defeats a regexp-based renderer: an unencoded '/'
	// ends the URL's authority early, so net/url rejects the string
	// ("invalid port") and a fallback regexp's [^/@]* cannot cross the '/'
	// either. "openssl rand
	// -base64" emits '/' routinely, and such a config fails validation
	// every time, so this is a leak an operator hits on 100% of startups.
	{"slash", "ab/cd" + secretSentinel},
	{"leading slash", "/lead" + secretSentinel},
	{"trailing slash", "trail" + secretSentinel + "/"},
	{"multiple slashes", "a/b/c/d" + secretSentinel},
	// The quote/backslash shapes: strconv.Quote escapes these when *url.Error
	// renders itself, which broke a literal string substitution.
	{"double quote", `sup"` + secretSentinel},
	{"backslash", `sup\` + secretSentinel},
	{"quote and backslash", `s"p\` + secretSentinel},
	// Rejected by net/url before any authority splitting happens.
	{"control character", "pa\x01" + secretSentinel},
	{"newline", "pa\n" + secretSentinel},
	// '@' in a password makes the userinfo/host split ambiguous unless the
	// *last* '@' is chosen.
	{"at sign", "p@ss" + secretSentinel},
	{"at sign and slash", "a/b@c" + secretSentinel},
	{"two at signs", "a@b@c" + secretSentinel},
	// Legal, already-encoded passwords must survive redaction too -- they
	// parse fine, so they exercise the rebuild path rather than the
	// lexical one.
	{"percent encoded", "p%40ss" + secretSentinel},
	{"percent encoded slash", "p%2Fss" + secretSentinel},
	// A ':' in a password is legal and lands in the password half.
	{"colon", "a:b" + secretSentinel},
	// '?' and '#' end the authority just as '/' does.
	{"question mark", "a?b" + secretSentinel},
	{"hash", "a#b" + secretSentinel},
	{"space", "a b" + secretSentinel},
	// A password that is also a legal host string is the case where an
	// "is this fragment dangerous?" heuristic cannot tell the difference.
	{"host shaped", "host0" + secretSentinel},
}

// clickHouseShapes are the DSN shapes vantage-writer actually accepts,
// including the multi-host form that is the whole reason clickhouse-go
// forked net/url. %s takes the password.
var clickHouseShapes = []struct{ name, template, wantHost string }{
	{"single host", "clickhouse://vantage:%s@127.0.0.1:19000/vantage", "127.0.0.1:19000"},
	{"multi host", "clickhouse://vantage:%s@host1:9000,host2:9000/vantage", "host1:9000,host2:9000"},
	{"ipv6 host", "clickhouse://vantage:%s@[::1]:9000/vantage", "[::1]:9000"},
	{"no database", "clickhouse://vantage:%s@127.0.0.1:19000", "127.0.0.1:19000"},
	{"query parameters", "clickhouse://vantage:%s@127.0.0.1:19000/vantage?secure=true", "127.0.0.1:19000"},
}

// assertNoSecret is the single assertion every class test makes.
func assertNoSecret(t *testing.T, what, raw, got string) {
	t.Helper()
	if strings.Contains(got, secretSentinel) {
		t.Errorf("%s(%q) = %q, which still contains the password", what, raw, got)
	}
}

// TestURLNeverLeaksAClickHouseDSNPassword is the ClickHouse half of the
// class: every tricky password in every DSN shape, through the two functions
// that render a DSN into an operator-visible string (URL, used directly in
// cmd/vantage-writer's "connect clickhouse %s" message, and CheckURL, whose
// rejection message is what leaked on every single startup for a '/'
// password).
//
// It also asserts the host survives. That is not decoration: failing closed
// is trivially achievable by rendering every URL as a constant, and an error
// that does not say which endpoint failed just relocates the operator's
// problem. Both properties have to hold at once.
func TestURLNeverLeaksAClickHouseDSNPassword(t *testing.T) {
	for _, shape := range clickHouseShapes {
		for _, pw := range trickyPasswords {
			t.Run(shape.name+"/"+pw.name, func(t *testing.T) {
				raw := fmt.Sprintf(shape.template, pw.password)

				got := URL(raw)
				assertNoSecret(t, "URL", raw, got)
				if got == raw {
					t.Errorf("URL(%q) returned its input verbatim", raw)
				}
				if !strings.Contains(got, shape.wantHost) {
					t.Errorf("URL(%q) = %q, lost the host %q -- a connection error that "+
						"does not name the endpoint is not actionable", raw, got, shape.wantHost)
				}

				// CheckURL rejects most of these before either library is
				// reached; its message embeds URL(raw), and for a '/'
				// password that message is the one an operator sees on
				// every startup.
				if err := CheckURL(raw); err != nil {
					assertNoSecret(t, "CheckURL", raw, err.Error())
				}

				// And the library-error path: whatever net/url says about
				// this string must survive Err without the password.
				if _, parseErr := url.Parse(raw); parseErr != nil {
					assertNoSecret(t, "Err", raw, Err(parseErr).Error())
					wrapped := fmt.Errorf("clickhouse: parse dsn: %w", parseErr)
					assertNoSecret(t, "Err(wrapped)", raw, Err(wrapped).Error())
				}
			})
		}
	}
}

// TestNatsURLNeverLeaksAnySegmentsPassword is the NATS half of the class,
// and adds the two axes ClickHouse does not have: NATS accepts a
// comma-separated multi-server list, and it accepts a schemeless
// "user:pass@host:port" shorthand.
//
// Both axes are covered at every position of a three-server list because
// position is exactly where an earlier version failed: its renderer
// redacted only the first segment. Every segment here carries a password
// containing the
// sentinel -- the one under test and the two filler segments alike -- so a
// renderer that handles only some positions fails regardless of which ones.
//
// The schemeless form is the second leak this fix closes. CheckNatsURL
// already prepended "nats://" before validating a segment, so a schemeless
// segment passed validation; NatsURL did not prepend before rendering, so
// net/url read the same segment as an opaque URL with no userinfo, found
// nothing to strip, and logged the password as-is.
func TestNatsURLNeverLeaksAnySegmentsPassword(t *testing.T) {
	forms := []struct{ name, template string }{
		{"with scheme", "nats://user:%s@host%d:4222"},
		{"schemeless shorthand", "user:%s@host%d:4222"},
	}
	for _, form := range forms {
		for _, pw := range trickyPasswords {
			for pos := range 3 {
				t.Run(fmt.Sprintf("%s/%s/position %d of 3", form.name, pw.name, pos), func(t *testing.T) {
					segs := make([]string, 3)
					for i := range segs {
						segs[i] = fmt.Sprintf("nats://user:filler%d%s@host%d:4222", i, secretSentinel, i)
					}
					segs[pos] = fmt.Sprintf(form.template, pw.password, pos)
					raw := strings.Join(segs, ",")

					got := NatsURL(raw)
					assertNoSecret(t, "NatsURL", raw, got)
					if got == raw {
						t.Errorf("NatsURL(%q) returned its input verbatim", raw)
					}
					for i := range 3 {
						host := fmt.Sprintf("host%d:4222", i)
						if !strings.Contains(got, host) {
							t.Errorf("NatsURL(%q) = %q, lost server %q", raw, got, host)
						}
					}

					if err := CheckNatsURL(raw); err != nil {
						assertNoSecret(t, "CheckNatsURL", raw, err.Error())
					}
				})
			}
		}
	}
}

// TestNatsURLRedactsSingleSegmentShorthand is the direct, minimal
// reproduction of the schemeless leak as an operator would actually
// configure it -- one server, no commas.
// The multi-segment matrix above covers it too, but this pins the reported
// instance in a form that is obvious at a glance.
func TestNatsURLRedactsSingleSegmentShorthand(t *testing.T) {
	for _, raw := range []string{
		"user:s3cr3t" + secretSentinel + "@127.0.0.1:14222",
		"user:ab/cd" + secretSentinel + "@127.0.0.1:14222",
	} {
		got := NatsURL(raw)
		assertNoSecret(t, "NatsURL", raw, got)
		if !strings.Contains(got, "127.0.0.1:14222") {
			t.Errorf("NatsURL(%q) = %q, lost the host", raw, got)
		}
	}
}

// TestNatsURLRedactsATokenOnlyURL covers a credential that does not live in
// the password position at all. nats.go's connectProto (v1.49.0) reads:
//
//	if _, ok := u.Password(); !ok { token = u.Username() }
//
// -- so in a NATS URL a userinfo block with no ':' is not a username, it is
// the entire auth token. URL's documented contract is to keep usernames
// (they cost nothing and help identify which account failed), which for a
// NATS URL would mean printing the whole credential. NatsURL therefore
// redacts a colonless userinfo where URL does not; this pins that
// difference, at every position of a multi-server list.
func TestNatsURLRedactsATokenOnlyURL(t *testing.T) {
	for _, form := range []struct{ name, template string }{
		{"with scheme", "nats://token%s@host%d:4222"},
		{"schemeless shorthand", "token%s@host%d:4222"},
	} {
		for pos := range 3 {
			t.Run(fmt.Sprintf("%s/position %d of 3", form.name, pos), func(t *testing.T) {
				segs := []string{
					"nats://host0:4222", "nats://host1:4222", "nats://host2:4222",
				}
				segs[pos] = fmt.Sprintf(form.template, secretSentinel, pos)
				raw := strings.Join(segs, ",")

				got := NatsURL(raw)
				assertNoSecret(t, "NatsURL", raw, got)
				if !strings.Contains(got, fmt.Sprintf("host%d:4222", pos)) {
					t.Errorf("NatsURL(%q) = %q, lost the host", raw, got)
				}
			})
		}
	}
}

// TestURLKeepsAClickHouseUsername pins the deliberate other side of the
// token rule above: for a ClickHouse DSN a colonless userinfo really is just
// a username (clickhouse-go's fromDSN assigns the two halves of the userinfo
// to Auth.Username and Auth.Password, v2.48.0), and URL keeps it. This is
// here so that the NATS token change is not later "made consistent" by
// stripping ClickHouse usernames too, which would cost real diagnostic
// information for no security gain.
func TestURLKeepsAClickHouseUsername(t *testing.T) {
	raw := "clickhouse://vantage@127.0.0.1:9000/vantage"
	if got := URL(raw); got != raw {
		t.Errorf("URL(%q) = %q, want the username-only DSN kept intact", raw, got)
	}
}

// TestURLRedactsAPasswordIdenticalToItsHost is the case where an
// absence-of-substring assertion cannot be used naively: the password is
// "host0" and the host is also "host0", so the redacted output *must* still
// contain the bytes "host0". The meaningful assertion is positional -- the
// password must be gone from the credential-bearing part of the URL (the
// userinfo) while the host survives -- so this checks the userinfo section
// directly instead of the whole string.
func TestURLRedactsAPasswordIdenticalToItsHost(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{"parseable", "nats://user:host0@host0:4222"},
		{"unparseable, slash in password", "nats://user:a/host0@host0:4222"},
		{"schemeless", "user:host0@host0:4222"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := NatsURL(c.raw)
			if !strings.Contains(got, "host0:4222") {
				t.Errorf("NatsURL(%q) = %q, lost the host", c.raw, got)
			}
			if ui := userinfoOf(got); strings.Contains(ui, "host0") {
				t.Errorf("NatsURL(%q) = %q, whose userinfo %q still contains the password",
					c.raw, got, ui)
			}
		})
	}
}

// userinfoOf returns the userinfo section of a rendered URL -- everything
// between "://" and the last '@' of the authority -- or "" if there is none.
// The renderings this package produces are well-formed by construction, so
// this simple split is sufficient for a test helper.
func userinfoOf(rendered string) string {
	s := rendered
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if end := strings.IndexAny(s, "/?#"); end >= 0 {
		s = s[:end]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[:at]
	}
	return ""
}

// TestURLNeverEchoesUnrecognizedInput is the fail-closed invariant stated
// directly, independent of any credential: when URL cannot positively
// identify what it is looking at, it must not reproduce the input. These are
// strings with no recoverable structure -- there is no host in them to be
// actionable about -- so the correct output is the marker alone.
func TestURLNeverEchoesUnrecognizedInput(t *testing.T) {
	for _, raw := range []string{
		"not a url://user:pass" + secretSentinel + "@host:1",
		"://user:pass" + secretSentinel + "@host:1",
		"9scheme://user:pass" + secretSentinel + "@host:1",
		"user:ht://tp" + secretSentinel + "@host:1",
	} {
		got := URL(raw)
		assertNoSecret(t, "URL", raw, got)
		if got != redactedMarker {
			t.Errorf("URL(%q) = %q, want the bare marker %q for input with no recoverable structure",
				raw, got, redactedMarker)
		}
	}
}

// TestURLRedactsAPasswordNetURLParsedAsAPath pins one of two leaks a broad
// test matrix uncovered. An unencoded '/'
// at the *start* of a password ends the URL's authority there, so net/url
// parses this DSN successfully -- host "vantage:", with the rest of the
// password, the real '@' and the real host all in Path -- and a redactor
// that treats "parsed successfully with no userinfo" as "carries no
// credential" hands the whole Path straight back. The password is in the
// output even though nothing failed to parse.
func TestURLRedactsAPasswordNetURLParsedAsAPath(t *testing.T) {
	raw := "clickhouse://vantage:/lead" + secretSentinel + "@127.0.0.1:19000"

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("test setup: expected net/url to *accept* this DSN, got %v", err)
	}
	if u.User != nil {
		t.Fatalf("test setup: expected net/url to find no userinfo, got %v", u.User)
	}
	if !strings.Contains(u.Path, secretSentinel) {
		t.Fatalf("test setup: expected the password to land in Path, got %q", u.Path)
	}

	got := URL(raw)
	assertNoSecret(t, "URL", raw, got)
	if !strings.Contains(got, "127.0.0.1:19000") {
		t.Errorf("URL(%q) = %q, lost the host", raw, got)
	}
}

// TestErrRedactsPasswordFragmentsQuotedByTheInnerError pins the other one.
// *url.Error has two fields that can carry input: URL, which an earlier
// fix redacted, and Err, which it did not. net/url builds Err separately
// and its messages quote fragments of the input -- here a password
// containing '/' makes net/url report the fragment as a bad port:
//
//	invalid port ":trailSECRET" after host
//
// so the fully-redacted URL field sat right next to the password. This
// asserts the leak is really present in the raw error (so the test cannot
// degenerate into asserting nothing) and gone from Err's output, wrapped and
// unwrapped, while the host survives.
func TestErrRedactsPasswordFragmentsQuotedByTheInnerError(t *testing.T) {
	raw := "clickhouse://vantage:trail" + secretSentinel + "/@127.0.0.1:19000/vantage"

	_, parseErr := url.Parse(raw)
	if parseErr == nil {
		t.Fatal("test setup: expected net/url to reject this DSN, got nil error")
	}
	var uerr *url.Error
	if !errors.As(parseErr, &uerr) {
		t.Fatalf("test setup: expected a *url.Error, got %T", parseErr)
	}
	if !strings.Contains(uerr.Err.Error(), secretSentinel) {
		t.Fatalf("test setup: expected the inner error to quote the password, got %q", uerr.Err)
	}

	for _, c := range []struct {
		name string
		err  error
	}{
		{"unwrapped", parseErr},
		{"wrapped", fmt.Errorf("clickhouse: parse dsn: %w", parseErr)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Err(c.err)
			assertNoSecret(t, "Err", raw, got.Error())
			if !strings.Contains(got.Error(), "127.0.0.1:19000") {
				t.Errorf("Err(%v) = %q, lost the host", c.err, got.Error())
			}
		})
	}
}

// TestErrKeepsASafeReasonFromTheRedactedURL confirms safeURLError does not
// throw the diagnosis away wholesale: when the structural fault survives
// redaction (a malformed IPv6 literal is in the host, not the credential),
// re-parsing the already-redacted URL reproduces net/url's real explanation,
// and it is safe because every byte of it derives from a string that no
// longer contains a credential.
func TestErrKeepsASafeReasonFromTheRedactedURL(t *testing.T) {
	raw := "clickhouse://vantage:pw" + secretSentinel + "@[::1:9000/vantage"
	_, parseErr := url.Parse(raw)
	if parseErr == nil {
		t.Fatal("test setup: expected net/url to reject a malformed IPv6 host, got nil error")
	}

	got := Err(parseErr).Error()
	assertNoSecret(t, "Err", raw, got)
	if !strings.Contains(got, "9000") {
		t.Errorf("Err(...) = %q, lost the host/port", got)
	}
	if !strings.Contains(got, "']'") {
		t.Errorf("Err(...) = %q, want net/url's real explanation of the malformed host "+
			"(it survives redaction because the fault is not in the credential)", got)
	}
}

// ---------------------------------------------------------------------------
// The credential that is not in the userinfo, and the host that is not
// after the '@'.
// ---------------------------------------------------------------------------

// TestURLRedactsAPasswordInAQueryParameter is the fix for a password
// carried in a query parameter.
// clickhouse-go's Options.fromDSN (clickhouse_options.go, v2.48.0) reads
// "password" as a query parameter, so
//
//	clickhouse://host:9000/db?password=SECRET
//
// is a complete credential with no '@' in it anywhere -- and a redactor
// that looks only at userinfo renders it by handing back u.String() with
// RawQuery intact, having correctly determined that there was no userinfo
// to strip. It is also the form an operator reaches for *because* their
// password broke the userinfo syntax, so the population that hits it is the
// population whose passwords are hardest to redact.
//
// Each case asserts the leak is really present in the input first, so the
// test cannot degenerate into asserting nothing, and that the host survives.
func TestURLRedactsAPasswordInAQueryParameter(t *testing.T) {
	for _, c := range []struct{ name, raw, wantHost string }{
		{
			"password parameter",
			"clickhouse://127.0.0.1:19000/vantage?password=" + secretSentinel,
			"127.0.0.1:19000",
		},
		{
			// The same form with an '@' inside the password: legal here
			// precisely because a query value is not a userinfo, and the
			// shape an operator lands on when '@' broke their DSN.
			"password parameter containing an @",
			"clickhouse://127.0.0.1:19000/vantage?password=p@ss" + secretSentinel,
			"127.0.0.1",
		},
		{
			"password parameter alongside a username parameter",
			"clickhouse://127.0.0.1:19000/vantage?username=vantage&password=" + secretSentinel + "&secure=true",
			"127.0.0.1:19000",
		},
		{
			"password parameter on a multi-host DSN",
			"clickhouse://host1:9000,host2:9000/vantage?password=" + secretSentinel,
			"host1:9000,host2:9000",
		},
		{
			// http_proxy is parsed as a URL of its own by fromDSN and can
			// carry its own userinfo -- a different credential in the same
			// place nobody was looking.
			"http_proxy parameter carrying its own userinfo",
			"clickhouse://127.0.0.1:19000/vantage?http_proxy=http://u:" + secretSentinel + "@proxy:8080",
			"127.0.0.1",
		},
		{
			// An unrecognized parameter becomes an arbitrary ClickHouse
			// setting. Its name is as much operator input as its value, so
			// neither is echoed.
			"unrecognized parameter",
			"clickhouse://127.0.0.1:19000/vantage?my_" + secretSentinel + "=" + secretSentinel,
			"127.0.0.1:19000",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if u, err := url.Parse(c.raw); err != nil {
				t.Fatalf("test setup: expected net/url to accept this DSN, got %v", err)
			} else if !strings.Contains(u.String(), secretSentinel) {
				t.Fatal("test setup: expected the unredacted rendering to contain the credential")
			}
			got := URL(c.raw)
			assertNoSecret(t, "URL", c.raw, got)
			if !strings.Contains(got, c.wantHost) {
				t.Errorf("URL(%q) = %q, lost the host %q", c.raw, got, c.wantHost)
			}
			if err := CheckURL(c.raw); err != nil {
				assertNoSecret(t, "CheckURL", c.raw, err.Error())
			}
		})
	}
}

// TestURLKeepsRecognizedQueryParameters is the actionability half of the
// query rule: the parameters clickhouse-go reads that are not credentials
// are diagnostic (secure=true is the difference between a TLS handshake
// failure being mysterious and being obvious), so they survive by name and
// value, and a credential-bearing parameter survives by name only.
func TestURLKeepsRecognizedQueryParameters(t *testing.T) {
	raw := "clickhouse://127.0.0.1:19000/vantage?secure=true&dial_timeout=300ms&username=vantage&password=" + secretSentinel
	got := URL(raw)
	assertNoSecret(t, "URL", raw, got)
	for _, want := range []string{"secure=true", "dial_timeout=300ms", "username=vantage", "password=" + redactedMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("URL(%q) = %q, want it to contain %q", raw, got, want)
		}
	}
}

// TestQueryParameterSetsAreDisjoint pins the one way the two lists in
// clickHousePolicy could contradict each other: a name on both would be
// echoed or redacted depending on the order of the switch in redactQuery,
// which is not a decision anybody should make by accident.
func TestQueryParameterSetsAreDisjoint(t *testing.T) {
	for name := range clickHousePolicy.safeQueryParams {
		if clickHousePolicy.secretQueryParams[name] {
			t.Errorf("query parameter %q is on both the safe and the secret list", name)
		}
	}
	if !clickHousePolicy.secretQueryParams["password"] {
		t.Error(`"password" is not on the secret list -- it is the parameter this fix exists for`)
	}
}

// TestNatsURLDropsAnyQueryParameter pins the NATS side of the same rule.
// nats.go reads no query parameter at all (parseServerURL and connectProto,
// v1.49.0, look only at the scheme, userinfo and host), so there is no
// parameter whose value is known to be safe, and any query an operator
// wrote is rendered as the marker rather than echoed.
func TestNatsURLDropsAnyQueryParameter(t *testing.T) {
	raw := "nats://127.0.0.1:4222?token=" + secretSentinel
	got := NatsURL(raw)
	assertNoSecret(t, "NatsURL", raw, got)
	if !strings.Contains(got, "127.0.0.1:4222") {
		t.Errorf("NatsURL(%q) = %q, lost the host", raw, got)
	}
}

// TestURLKeepsTheRealHostWhenAnAtIsInAPathOrQuery is the regression test
// for the defect an earlier fix introduced. Closing the "password
// net/url parsed as a path" leak meant treating any unaccounted-for '@' as
// a userinfo separator, which fabricated a host out of whatever followed
// the last one:
//
//	clickhouse://127.0.0.1:19000/db@x   ->   clickhouse://REDACTED@x
//
// -- an error naming an endpoint the operator does not have, having dropped
// the one they do. Naming the wrong host is worse than naming none, and
// this shape needs no credential to occur: any '@' in a path or a query
// triggered it, including the "?password=p@ss" form.
//
// The renderer no longer picks a reading. It emits what is harmless under
// both, which keeps the parsed host (see renderParsed).
func TestURLKeepsTheRealHostWhenAnAtIsInAPathOrQuery(t *testing.T) {
	for _, c := range []struct{ name, raw, wantHost, wantAbsent string }{
		{"at in the database name", "clickhouse://127.0.0.1:19000/db@x", "127.0.0.1", ""},
		{"at in an ipv6-host DSN's path", "clickhouse://[::1]:19000/db@x", "[::1]", ""},
		{"at in a query value", "clickhouse://127.0.0.1:19000/db?opt=a@b", "127.0.0.1", ""},
		{"at in a fragment", "clickhouse://127.0.0.1:19000/db#a@b", "127.0.0.1", ""},
		{"at in a multi-host DSN's path", "clickhouse://host1:9000,host2:9000/db@x", "host1", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := URL(c.raw)
			if !strings.Contains(got, c.wantHost) {
				t.Errorf("URL(%q) = %q, lost the real host %q -- the renderer is naming "+
					"the wrong endpoint again", c.raw, got, c.wantHost)
			}
			// And it must not present the tail of the path as if it were
			// the host, which is precisely what the old rendering did.
			if strings.HasPrefix(got, "clickhouse://"+redactedMarker+"@") {
				t.Errorf("URL(%q) = %q, which fabricates a host out of the path tail", c.raw, got)
			}
		})
	}
}

// TestURLStillRecoversTheHostFromAPasswordSwallowedAuthority is the other
// side of that coin, and the reason the fix could not simply be "always
// believe net/url". When a '/' in the password ends the authority early,
// the true host really is the fragment after the last '@' -- the same
// lexical shape as the test above, with the opposite meaning -- and an
// operator whose password contains a '/' hits this on every startup. Both
// readings have to survive the same renderer.
func TestURLStillRecoversTheHostFromAPasswordSwallowedAuthority(t *testing.T) {
	for _, c := range []struct{ name, raw, wantHost string }{
		{"leading slash, parses as a path", "clickhouse://vantage:/lead" + secretSentinel + "@127.0.0.1:19000", "127.0.0.1:19000"},
		{"digits then slash, parses as a port", "clickhouse://vantage:2024/summer" + secretSentinel + "@127.0.0.1:19000", "127.0.0.1:19000"},
		{"trailing slash, net/url rejects it", "clickhouse://vantage:trail" + secretSentinel + "/@127.0.0.1:19000/vantage", "127.0.0.1:19000"},
		{"question mark in the password", "clickhouse://vantage:a?b" + secretSentinel + "@127.0.0.1:19000/vantage?secure=true", "127.0.0.1:19000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := URL(c.raw)
			assertNoSecret(t, "URL", c.raw, got)
			if !strings.Contains(got, c.wantHost) {
				t.Errorf("URL(%q) = %q, lost the host %q", c.raw, got, c.wantHost)
			}
		})
	}
}

// TestURLNeverEchoesAPasswordPrefixAsAPort pins the one byte-level
// exposure the ambiguous branch is built to prevent, in the shape that
// makes it plausible: net/url accepts an all-digit port, so a password
// beginning with digits and containing a '/' parses as a port and would be
// logged as one. A date-prefixed passphrase does exactly that.
func TestURLNeverEchoesAPasswordPrefixAsAPort(t *testing.T) {
	raw := "clickhouse://vantage:2024/summer" + secretSentinel + "@127.0.0.1:19000"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("test setup: expected net/url to accept this DSN, got %v", err)
	}
	if u.Port() != "2024" {
		t.Fatalf("test setup: expected net/url to read the password prefix as the port, got %q", u.Port())
	}
	if got := URL(raw); strings.Contains(got, "2024") {
		t.Errorf("URL(%q) = %q, which echoes the password's first four characters as a port", raw, got)
	}
}

// clickHouseQueryShapes are the DSN shapes that carry a credential outside
// the userinfo. %s takes the password. They are a separate table from
// clickHouseShapes because the whole point is that this credential is not
// in the userinfo, where a redactor naturally looks.
var clickHouseQueryShapes = []struct{ name, template, wantHost string }{
	{"password parameter", "clickhouse://127.0.0.1:19000/vantage?password=%s", "127.0.0.1"},
	{"password parameter, no database", "clickhouse://127.0.0.1:19000?password=%s", "127.0.0.1"},
	{"password parameter with other options", "clickhouse://127.0.0.1:19000/vantage?secure=true&password=%s&dial_timeout=300ms", "127.0.0.1"},
	{"password parameter on a multi-host DSN", "clickhouse://host1:9000,host2:9000/vantage?password=%s", "host1"},
	{"password parameter and a userinfo username", "clickhouse://vantage@127.0.0.1:19000/vantage?password=%s", "127.0.0.1"},
	{"http_proxy parameter", "clickhouse://127.0.0.1:19000/vantage?http_proxy=http://u:%s@proxy:8080", "127.0.0.1"},
	{"unrecognized parameter", "clickhouse://127.0.0.1:19000/vantage?some_setting=%s", "127.0.0.1"},
}

// TestURLNeverLeaksAQueryParameterCredential is the class test for the
// query-parameter-credential shape: every tricky password from the table
// above, in every query-carrying DSN shape. The passwords are the same
// ones that broke the userinfo path, because there is no reason a
// password in a query parameter should be tamer than one in a userinfo --
// and because a redactor that special-cases "password=" without treating
// the value as unclassified bytes would pass the simple cases and fail
// these.
func TestURLNeverLeaksAQueryParameterCredential(t *testing.T) {
	for _, shape := range clickHouseQueryShapes {
		for _, pw := range trickyPasswords {
			t.Run(shape.name+"/"+pw.name, func(t *testing.T) {
				raw := fmt.Sprintf(shape.template, pw.password)

				got := URL(raw)
				assertNoSecret(t, "URL", raw, got)
				if got == raw {
					t.Errorf("URL(%q) returned its input verbatim", raw)
				}
				if !strings.Contains(got, shape.wantHost) {
					t.Errorf("URL(%q) = %q, lost the host %q -- a connection error that "+
						"does not name the endpoint is not actionable", raw, got, shape.wantHost)
				}
				if err := CheckURL(raw); err != nil {
					assertNoSecret(t, "CheckURL", raw, err.Error())
				}
				if _, parseErr := url.Parse(raw); parseErr != nil {
					assertNoSecret(t, "Err", raw, Err(parseErr).Error())
					wrapped := fmt.Errorf("clickhouse: parse dsn: %w", parseErr)
					assertNoSecret(t, "Err(wrapped)", raw, Err(wrapped).Error())
				}
			})
		}
	}
}

// TestURLRedactsAPasswordThatSwallowsTheAuthorityAfterItsOwnAt is a leak
// this fix's own live test matrix turned up, in the same class as the other
// findings before it and present in every version of this package. A
// password containing an '@' and then a '/' makes net/url parse a userinfo
// that is not the real one:
//
//	clickhouse://u:p@ss/wordSECRET@127.0.0.1:19000/db
//
// parses as userinfo "u:p", host "ss", path "/wordSECRET@127.0.0.1:19000/db"
// -- so clearing the parsed password removed "p" and echoed the rest of the
// password back as a host and a path. "net/url found a userinfo" is no more
// a guarantee that the credential is fully accounted for than "net/url
// found no userinfo" was.
//
// Both readings are asserted: the credential is gone, and the endpoint that
// can still be recovered (the fragment after the last '@') is named.
func TestURLRedactsAPasswordThatSwallowsTheAuthorityAfterItsOwnAt(t *testing.T) {
	for _, c := range []struct {
		name, raw, wantHost string
		render              func(string) string
	}{
		{"slash after the at", "clickhouse://u:p@ss/word" + secretSentinel + "@127.0.0.1:19000/db", "127.0.0.1:19000", URL},
		{"question mark after the at", "clickhouse://u:p@ss?word" + secretSentinel + "@127.0.0.1:19000/db", "", URL},
		{"hash after the at", "clickhouse://u:p@ss#word" + secretSentinel + "@127.0.0.1:19000/db", "", URL},
		{"nats, slash after the at", "nats://u:p@ss/word" + secretSentinel + "@127.0.0.1:4222", "127.0.0.1:4222", NatsURL},
		{"nats token, slash after the at", "nats://tok@en/word" + secretSentinel + "@127.0.0.1:4222", "127.0.0.1:4222", NatsURL},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatalf("test setup: expected net/url to accept this URL, got %v", err)
			}
			if u.User == nil {
				t.Fatal("test setup: expected net/url to find a (wrong) userinfo")
			}
			if !strings.Contains(u.String(), secretSentinel) {
				t.Fatal("test setup: expected the unredacted rendering to contain the credential")
			}

			got := c.render(c.raw)
			assertNoSecret(t, "render", c.raw, got)
			if c.wantHost != "" && !strings.Contains(got, c.wantHost) {
				t.Errorf("render(%q) = %q, lost the host %q", c.raw, got, c.wantHost)
			}
		})
	}
}

// TestURLKeepsTheHostWhenOnlyTheQueryHoldsAnAt pins the deliberate limit of
// the rule above: a colonless ClickHouse userinfo is a username, which this
// package keeps under either reading, so a "?password=p@ss" alongside it
// costs nothing. The host must survive -- this is the ordinary shape of an
// operator whose password broke the userinfo syntax, and it is the one they
// hit on every startup.
func TestURLKeepsTheHostWhenOnlyTheQueryHoldsAnAt(t *testing.T) {
	raw := "clickhouse://vantage@127.0.0.1:19000/db?password=p@ss" + secretSentinel
	got := URL(raw)
	assertNoSecret(t, "URL", raw, got)
	for _, want := range []string{"vantage", "127.0.0.1:19000", "password=" + redactedMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("URL(%q) = %q, want it to contain %q", raw, got, want)
		}
	}
}

// TestCheckClickHouseProxy pins the critical defect: the DSN
// parameter whose value is a second URL, with a second credential, that
// clickhouse-go parses and then formats with "%s" rather than "%w".
//
// That formatting is what puts it out of Err's reach: "%s" flattens the
// *url.Error to text and wraps nothing, so errors.As finds no *url.Error
// and Err returns the message -- complete with the proxy's password --
// untouched. The fix is a pre-validation, so clickhouse-go is never given
// the value; TestNewClickHouseRejectsAMalformedHTTPProxy in sink
// is the other half, driving the same inputs through the real driver.
//
// Both triggers found are covered. Neither is exotic: a mistyped port
// ("8o80", the letter 'o') and a space in the proxy host are the two ways
// an ordinary proxy URL is malformed, and CheckURL passes every one of
// these DSNs, because the DSN itself is well-formed. The value appears in
// percent-encoded and literal form because an operator writes it either
// way and clickhouse-go decodes both.
func TestCheckClickHouseProxy(t *testing.T) {
	const s = secretSentinel
	for _, c := range []struct {
		name     string
		raw      string
		wantErr  bool
		wantHost string
	}{
		{"invalid port, percent-encoded",
			"clickhouse://127.0.0.1:9000/db?dial_timeout=100ms&http_proxy=http%3A%2F%2Fu%3Apw" + s + "%40proxy%3A8o80",
			true, "proxy:8o80"},
		{"invalid port, literal",
			"clickhouse://127.0.0.1:9000/db?http_proxy=http://u:pw" + s + "@proxy:8o80",
			true, "proxy:8o80"},
		{"space in host, percent-encoded",
			"clickhouse://127.0.0.1:9000/db?http_proxy=http://u:pw" + s + "@pro%20xy:8080",
			true, ""},
		{"space in host, plus-encoded",
			"clickhouse://127.0.0.1:9000/db?http_proxy=http://u:pw" + s + "@pro+xy:8080",
			true, ""},
		// The happy paths. A proxy this package must not reject: the
		// credential in it is none of our business as long as the URL
		// parses, because clickhouse-go will never quote it.
		{"valid proxy with credentials",
			"clickhouse://127.0.0.1:9000/db?http_proxy=http://u:pw" + s + "@proxy:8080", false, ""},
		{"valid proxy without credentials",
			"clickhouse://127.0.0.1:9000/db?http_proxy=http://proxy:8080", false, ""},
		{"no proxy parameter at all",
			"clickhouse://vantage:pw" + s + "@127.0.0.1:9000/db?dial_timeout=1s", false, ""},
		{"no query at all", "clickhouse://127.0.0.1:9000/db", false, ""},
		{"empty proxy parameter", "clickhouse://127.0.0.1:9000/db?http_proxy=", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// CheckURL passes every one of these: the leak is in a
			// parameter's value, and the DSN around it is well-formed.
			// If this ever stopped holding the test would be proving
			// something else.
			if err := CheckURL(c.raw); err != nil {
				t.Fatalf("test setup: CheckURL(%q) = %v, want nil -- these DSNs are "+
					"well-formed, which is the whole point", c.raw, err)
			}
			err := CheckClickHouseProxy(c.raw)
			if !c.wantErr {
				if err != nil {
					t.Fatalf("CheckClickHouseProxy(%q) = %v, want nil", c.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckClickHouseProxy(%q) = nil, want a rejection", c.raw)
			}
			assertNoSecret(t, "CheckClickHouseProxy", c.raw, err.Error())
			// CheckClickHouseDSN is what secret.ClickHouseDSN.Validate
			// calls, and it must reach the same verdict.
			if dsnErr := CheckClickHouseDSN(c.raw); dsnErr == nil {
				t.Errorf("CheckClickHouseDSN(%q) = nil, want the same rejection", c.raw)
			} else {
				assertNoSecret(t, "CheckClickHouseDSN", c.raw, dsnErr.Error())
			}
			if !strings.Contains(err.Error(), "http_proxy") {
				t.Errorf("CheckClickHouseProxy(%q) = %q, which does not name the "+
					"parameter at fault", c.raw, err)
			}
			// Actionable: the proxy host survives wherever it can be
			// recovered at all, for the same reason the DSN's own host
			// does. A host that is unrecoverable (the space cases, where
			// hostPortPattern refuses it) is left blank above rather
			// than asserted, because failing closed outranks it.
			if c.wantHost != "" && !strings.Contains(err.Error(), c.wantHost) {
				t.Errorf("CheckClickHouseProxy(%q) = %q, lost the proxy host %q",
					c.raw, err, c.wantHost)
			}
		})
	}
}

// TestCheckClickHouseProxyReadsTheParameterTheDriverWillRead pins dsnParam
// against the two ways it could silently stop matching clickhouse-go and
// leave the check looking green while doing nothing.
//
// The driver reads Query() on the URL churl.Parse returned -- stdlib
// url.URL.Query(), i.e. ParseQuery(RawQuery) with the error discarded.
// So: a malformed pair elsewhere in the query must not hide a well-formed
// http_proxy (ParseQuery returns the pairs it did parse *and* an error, and
// honoring that error would skip the check on a DSN the driver still
// parses), and a '?' inside a fragment is not a query at all.
func TestCheckClickHouseProxyReadsTheParameterTheDriverWillRead(t *testing.T) {
	const bad = "http://u:pw" + secretSentinel + "@proxy:8o80"
	if err := CheckClickHouseProxy(
		"clickhouse://127.0.0.1:9000/db?%zz=1&http_proxy=" + bad); err == nil {
		t.Error("a query with an undecodable pair alongside http_proxy was not " +
			"checked; ParseQuery's error must be discarded, as the driver discards it")
	} else {
		assertNoSecret(t, "CheckClickHouseProxy", bad, err.Error())
	}
	// Everything from the first '#' is the fragment, for churl exactly as
	// for net/url, so this DSN has no query and no http_proxy.
	if err := CheckClickHouseProxy(
		"clickhouse://127.0.0.1:9000/db#frag?http_proxy=" + bad); err != nil {
		t.Errorf("a '?' inside the fragment was read as a query: %v", err)
	}
}
