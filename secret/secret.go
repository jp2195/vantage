// Package secret holds the operator-supplied credentials this tree carries
// -- vantage-writer's ClickHouse DSN, both daemons' NATS URL, and
// vantage-api's bearer tokens -- in types that cannot be printed by
// accident.
//
// # Why a type, not just a renderer
//
// redact renders a connection string with its credentials removed,
// and it is good at it. It was still not enough, because it is a function:
// safety depended on every call site remembering to call it. Several
// successive fixes each closed the instance that had just been reported
// while leaving the class open. This package's rationale is the
// observation that closed the argument: "this is closed by convention,
// not by construction ... the structural fix would be to make the
// config fields a named type whose default rendering redacts, so the raw
// value cannot be formatted by accident."
//
// That is what these types are. The config fields hold a ClickHouseDSN or a
// NatsURL rather than a string, so
//
//	fmt.Errorf("connect clickhouse %s: %w", cfg.ClickHouseDSN, err)
//
// -- the line a future contributor writes without thinking about any of this
// -- renders the redacted form. Getting the real value out requires calling
// RevealSecret, which is greppable, and which appears in exactly the two
// functions that hand the string to a library -- natsutil.Connect and
// sink.NewClickHouse. (NewClickHouse calls it twice: once to pre-validate
// the DSN's "http_proxy" parameter, which is a second URL clickhouse-go
// would otherwise quote into an error redact.Err cannot reach, and once for
// the driver itself. Two call sites, one place.)
//
//	$ git grep -n RevealSecret -- '*.go' ':!*_test.go' ':!secret/*'
//
// # Which interfaces, and why String() is not enough
//
// A Stringer covers %s, %v and %q (fmt's handleMethods dispatches those
// verbs to Stringer), but it does not cover %#v, which goes to GoStringer,
// and it does not cover a verb the type has no business with at all -- %d
// on a string-ish value -- which fmt renders by reflecting over the operand
// and printing its fields. So these types implement:
//
//   - fmt.Formatter. This is the one that closes the class rather than a
//     verb: fmt consults Formatter *first*, for every verb, so there is no
//     verb -- known, unknown, or added to fmt later -- that reaches the
//     reflection-based printer with one of these values. Format renders the
//     redacted string through the verb the caller actually wrote, so widths
//     and flags (%-20s, %q, %#v) still behave.
//   - fmt.Stringer. Not for fmt's benefit (Formatter outranks it) but for
//     the many callers that type-assert Stringer themselves, and so that
//     v.String() at a call site reads as the ordinary thing to do.
//   - fmt.GoStringer, for anything that asks for %#v via GoString directly.
//   - encoding.TextMarshaler, which is what slog's TextHandler and
//     encoding/json both reach for before falling back to reflection.
//   - slog.LogValuer, so slog resolves the value once, identically, before
//     any handler sees it -- rather than each handler's fallback path
//     deciding separately.
//   - yaml.Marshaler / yaml.Unmarshaler, because the config files are YAML
//     and gopkg.in/yaml.v3 consults its own two interfaces first.
//     Unmarshaling is how the value gets in; marshaling deliberately emits
//     the *redacted* form, so round-tripping a config through YAML is not
//     a way to recover a password.
//
// That last entry used to end "(yaml.v3 honors neither TextMarshaler nor
// TextUnmarshaler; it looks only for its own two interfaces)", and that was
// false. It was found false by running the mutation that should have caught
// MarshalYAML's removal: deleting the method changed no output any test
// could see. yaml.v3 v3.0.1 checks Marshaler and then falls back to
// encoding.TextMarshaler (encode.go:150), and its decoder does the same
// with TextUnmarshaler (decode.go:592). So MarshalYAML is not what closes
// the marshal path -- MarshalText already would -- and it is kept as the
// interface yaml.v3 reaches for first.
//
// LogValue is in the same position for the same reason: with it deleted,
// both stock slog handlers fall back to TextMarshaler and print the
// identical redaction. Neither method's presence changes anything an
// encoder or a handler emits, so neither can be pinned by output. Both are
// pinned instead by being called directly and having their return values
// asserted -- which fails to compile if the method is deleted and fails as
// a test if it stops routing through String.
// TestEveryTypeIsPinnedForSlogAndYAML does that for all three types;
// before it existed, deleting any of those four methods left the whole
// suite green.
//
// UnmarshalYAML is load-bearing for a different reason: these types
// implement no UnmarshalText, so it is the only way in, and dropping it is
// caught by TestYAMLRoundTrip.
//
// # Why the raw value is behind a pointer
//
// The field is a *string rather than a string, and that is not an
// accident. fmt has two paths that bypass every interface above: %w applied
// to a non-error, and %p applied to a non-pointer. Both call fmt's badVerb,
// which sets an internal erroring flag and then re-prints the operand with
// interface dispatch *disabled*, dumping the struct's fields by reflection.
// With a string field that prints as
//
//	%!w(secret.ClickHouseDSN={clickhouse://vantage:hunter2@127.0.0.1:9000})
//
// -- the whole password, in the one code path that had been reasoned about
// least. (Verified against Go 1.26 rather than assumed; see
// TestNoVerbLeaksTheSecret, which pins %w and %p specifically.) A pointer
// field prints as its address instead, because fmt renders a pointer nested
// inside a struct as a hex address and never follows it.
//
// # What the pointer does not do, stated because the opposite reads as true
//
// Being unexported keeps ordinary Go code in other packages out. The
// pointer, on top of that, blocks *fmt's* reflection printer -- and only
// that. It works for one narrow, verified reason: fmt does not follow a
// pointer at depth > 0. It is not a barrier against reflection in general,
// and nothing in this package is.
//
// A generic reflective walker reaches the raw string with no unsafe, no
// linkname, and nothing exotic:
//
//	reflect.ValueOf(dsn).Field(0).Elem().String()  // the whole DSN
//
// Field(0) on an unexported field yields a Value with CanInterface()
// false, and that is where the intuition goes wrong: reflect.Value.String()
// does not require CanInterface, so the read succeeds. Verified by
// execution, not reasoned about.
//
// Whether that matters is a fact about the module graph, not a property of
// these types -- and the module graph is worse than it looks.
//
// # Do not call pretty.Compare on anything containing one of these types
//
// github.com/kylelemons/godebug is *already reachable*: prometheus/
// client_golang's prometheus/testutil imports godebug/diff, which puts the
// module in sink.test's own dependency graph today. Importing
// godebug/pretty therefore needs no go.mod change, no go.sum change, and
// no new requirement for anyone to notice in review.
//
// And godebug's headline API leaks in full. pretty.Compare uses
// CompareConfig, which sets IncludeUnexported: true (pretty/public.go,
// v1.1.0) and sets neither PrintStringers nor PrintTextMarshalers -- so it
// walks straight past String(), Format() and MarshalText(), reads the
// unexported field, follows the pointer, and prints the pointee. One
// ordinary line in a test:
//
//	pretty.Compare(gotCfg, wantCfg)
//	  ->  ClickHouseDSN: {
//	      -  raw: "clickhouse://u:hunter2@127.0.0.1:9000/db",
//
// Executed against a real sink.Config, not reasoned about. pretty.Sprint is
// the one that renders "{}" -- its DefaultConfig leaves IncludeUnexported
// false -- which is why a quick check of Sprint alone is misleading, and is
// exactly the check that produced the wrong conclusion the first time this
// paragraph was written.
//
// The rule is therefore: do not put a value containing one of these types
// through pretty.Compare, or through any differ configured to include
// unexported fields. That rule used to be enforced by reading alone, which
// was its weakest point -- the import adds no go.mod or go.sum line, so
// there is no diff for code review to notice. It is now enforced by
// secret/godebug_test.go's TestNoGodebugPrettyImport, which parses the
// imports of every Go file in the module and fails on this one wherever it
// appears; a differ configured the same way but reached by another name
// still needs a human to catch. go-spew,
// testify and go-cmp are genuinely absent from the module graph -- but if
// testify is ever added, note that its own differ configures spew with
// DisableMethods: true, the same setting by another name.
//
// One consequence, called out because it is silent: '==' on these types
// compares pointer identity, not the strings. Compare RevealSecret() values
// if you mean to compare configuration. reflect.DeepEqual is fine -- it
// follows pointers.
package secret

import (
	"fmt"
	"log/slog"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/redact"
)

// ClickHouseDSN is a clickhouse-go DSN
// ("clickhouse://user:password@host:9000/database", plus ClickHouse's
// multi-host and query-parameter forms). Its rendered form is
// redact.URL's.
type ClickHouseDSN struct{ raw *string }

// NatsURL is a nats.Connect URL, including NATS's comma-separated
// multi-server list and its schemeless "user:pass@host:4222" shorthand. Its
// rendered form is redact.NatsURL's, which redacts each server in the list
// separately and treats a colonless userinfo as an auth token rather than a
// username.
type NatsURL struct{ raw *string }

// APIToken is a vantage-api bearer token: a credential an operator
// generates, hands to a client, and lists in the config file under a name.
//
// It differs from the two types above in the one way that matters here: it
// has no printable form at all. Their renderings keep the host, because an
// error that names the endpoint an operator got wrong is actionable and the
// host is not the secret. A bearer token has no such part. Every byte of it
// is the credential, and a prefix is still a credential with a shorter
// search space -- so String returns a fixed marker and never reads its
// receiver. That is the property to preserve: the rendering cannot leak the
// token because it never looks at it.
//
// What keeps a redacted token line actionable is the name beside it.
// api.Config pairs every token with an operator-chosen name, and that name
// is an ordinary string that logs in full; the token reaches a log line in
// no form at all.
//
// One consequence for the code that will check these, spelled out because
// the obvious spelling is wrong twice over: comparing a presented token to
// a configured one is not "==". Equality on two APITokens compares the
// pointers, so it is false for two separately constructed values of the
// same string -- fail-closed, but never true. The comparison is a
// RevealSecret on both sides fed to subtle.ConstantTimeCompare.
type APIToken struct{ raw *string }

// apiTokenRedacted is the whole of APIToken's rendered form, in every verb
// and every encoder. It is redact's own marker spelled out a second time
// rather than imported: redact.redactedMarker is unexported, and nothing in
// redact's exported surface takes a bare credential -- every entry point
// there parses a URL, and a bearer token is not one.
const apiTokenRedacted = "REDACTED"

// NewClickHouseDSN wraps a raw DSN. Constructing one is not a disclosure;
// rendering it is, and every rendering path is redacted.
func NewClickHouseDSN(raw string) ClickHouseDSN { return ClickHouseDSN{raw: &raw} }

// NewNatsURL wraps a raw NATS URL.
func NewNatsURL(raw string) NatsURL { return NatsURL{raw: &raw} }

// NewAPIToken wraps a raw bearer token.
func NewAPIToken(raw string) APIToken { return APIToken{raw: &raw} }

// RevealSecret returns the operator's connection string verbatim. It is
// named to be conspicuous in a diff and greppable in a tree: every call is
// a decision to handle a credential, and there should never be more of them
// than there are libraries to hand the string to.
func (d ClickHouseDSN) RevealSecret() string { return deref(d.raw) }

// RevealSecret returns the operator's connection string verbatim. See
// ClickHouseDSN.RevealSecret.
func (u NatsURL) RevealSecret() string { return deref(u.raw) }

// RevealSecret returns the operator's bearer token verbatim. See
// ClickHouseDSN.RevealSecret; the one difference is that the caller here is
// not a library hand-off but a comparison, so what it is fed to must be
// constant-time.
func (t APIToken) RevealSecret() string { return deref(t.raw) }

// Empty reports whether no value was configured, which is how LoadConfig
// decides to substitute a default. It is not "== the zero value": a config
// that explicitly sets an empty string is equally unset.
func (d ClickHouseDSN) Empty() bool { return deref(d.raw) == "" }

// Empty reports whether no value was configured. See ClickHouseDSN.Empty.
func (u NatsURL) Empty() bool { return deref(u.raw) == "" }

// Empty reports whether no token was configured, which is how
// api.LoadConfig refuses to start on a token entry that names a caller and
// gives it nothing to present. There is no Validate beyond this: a bearer
// token has no grammar to be malformed against.
func (t APIToken) Empty() bool { return deref(t.raw) == "" }

// Validate parses the DSN and returns a redacted error if it is malformed,
// so a typo fails at startup with a clear message instead of surfacing as a
// dial or TLS error once clickhouse-go gets hold of whatever its parser
// salvaged. See redact.CheckClickHouseDSN, which is CheckURL plus the
// "http_proxy" parameter -- a second URL, with its own userinfo, that
// clickhouse-go parses and quotes back into an error it formats with "%s"
// rather than "%w", where redact.Err cannot reach it.
func (d ClickHouseDSN) Validate() error { return redact.CheckClickHouseDSN(d.RevealSecret()) }

// Validate parses every server in the NATS URL -- the value may be a
// comma-separated list, and nats.Connect parses each element separately --
// and returns a redacted error if any is malformed. See redact.CheckNatsURL.
func (u NatsURL) Validate() error { return redact.CheckNatsURL(u.RevealSecret()) }

// String renders the DSN with its credentials removed, keeping the host so
// that an error naming it stays actionable.
func (d ClickHouseDSN) String() string { return redact.URL(d.RevealSecret()) }

// String renders the NATS URL with every server's credentials removed.
func (u NatsURL) String() string { return redact.NatsURL(u.RevealSecret()) }

// String renders the fixed marker. The receiver is deliberately unused --
// see APIToken -- so this rendering is identical for every token, carrying
// neither the value nor its length nor whether one was configured at all.
func (t APIToken) String() string { return apiTokenRedacted }

// Format implements fmt.Formatter for every verb. See this package's doc
// comment for why this, and not String, is what closes the class.
func (d ClickHouseDSN) Format(f fmt.State, verb rune) {
	format(f, verb, "secret.ClickHouseDSN", d.String())
}

// Format implements fmt.Formatter for every verb. See ClickHouseDSN.Format.
func (u NatsURL) Format(f fmt.State, verb rune) {
	format(f, verb, "secret.NatsURL", u.String())
}

// Format implements fmt.Formatter for every verb. See ClickHouseDSN.Format.
func (t APIToken) Format(f fmt.State, verb rune) {
	format(f, verb, "secret.APIToken", t.String())
}

// GoString renders the %#v form.
func (d ClickHouseDSN) GoString() string { return goString("secret.ClickHouseDSN", d.String()) }

// GoString renders the %#v form.
func (u NatsURL) GoString() string { return goString("secret.NatsURL", u.String()) }

// GoString renders the %#v form.
func (t APIToken) GoString() string { return goString("secret.APIToken", t.String()) }

// MarshalText renders the redacted form for encoding/json and for slog's
// TextHandler.
func (d ClickHouseDSN) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// MarshalText renders the redacted form. See ClickHouseDSN.MarshalText.
func (u NatsURL) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// MarshalText renders the marker. Without it encoding/json would fall back
// to reflection and emit "{}" for this struct -- no leak, but no honesty
// either: a config dump would show a token field that looks like it holds
// nothing.
func (t APIToken) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// LogValue renders the redacted form for log/slog, before any handler sees
// the value.
func (d ClickHouseDSN) LogValue() slog.Value { return slog.StringValue(d.String()) }

// LogValue renders the redacted form for log/slog.
func (u NatsURL) LogValue() slog.Value { return slog.StringValue(u.String()) }

// LogValue renders the marker for log/slog, before any handler sees the
// value -- so every handler, including one added later that consults
// neither TextMarshaler nor Stringer, logs the same thing.
func (t APIToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

// MarshalYAML renders the redacted form. Deliberately lossy: writing a
// config back out is not a supported way to recover a password, and a
// marshaler that emitted the raw value would reopen the class through
// yaml.Marshal.
func (d ClickHouseDSN) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalYAML renders the redacted form. See ClickHouseDSN.MarshalYAML.
func (u NatsURL) MarshalYAML() (any, error) { return u.String(), nil }

// MarshalYAML renders the marker. See ClickHouseDSN.MarshalYAML: writing a
// config back out is not a supported way to recover a token either. As
// noted in this package's doc comment, MarshalText would cover this path on
// its own -- yaml.v3 falls back to it -- so this method is the belt to that
// braces, and nothing an encoder emits distinguishes its presence. It is
// pinned by a direct call instead.
func (t APIToken) MarshalYAML() (any, error) { return t.String(), nil }

// UnmarshalYAML reads the raw value out of a config file. This is the one
// direction that carries the real string, and it is why the config structs
// can keep their `yaml:"clickhouse_dsn"` tags and their KnownFields(true)
// decoder untouched.
func (d *ClickHouseDSN) UnmarshalYAML(node *yaml.Node) error {
	raw, err := decodeYAMLString(node)
	if err != nil {
		return err
	}
	d.raw = &raw
	return nil
}

// UnmarshalYAML reads the raw value out of a config file. See
// ClickHouseDSN.UnmarshalYAML.
func (u *NatsURL) UnmarshalYAML(node *yaml.Node) error {
	raw, err := decodeYAMLString(node)
	if err != nil {
		return err
	}
	u.raw = &raw
	return nil
}

// UnmarshalYAML reads the raw token out of a config file. This is the only
// direction that carries the real value, and it is the method whose absence
// would be silent in the worst way: the field would simply stay unset, and
// api.LoadConfig would reject every configured token as empty.
func (t *APIToken) UnmarshalYAML(node *yaml.Node) error {
	raw, err := decodeYAMLString(node)
	if err != nil {
		return err
	}
	t.raw = &raw
	return nil
}

// deref reads a raw value, treating the zero value (no pointer at all) as
// the empty string rather than panicking: a Config field nobody set is a
// perfectly ordinary thing to render.
func deref(raw *string) string {
	if raw == nil {
		return ""
	}
	return *raw
}

// format renders redacted through whatever verb the caller wrote.
//
// fmt.FormatString reconstructs that verb along with its flags, width and
// precision, so "%-20s", "%q" and "%8.3s" all behave as they would on a
// plain string, and a verb that makes no sense for a string ("%d") produces
// fmt's own "%!d(string=...)" complaint -- carrying the redacted rendering,
// never the raw value. %#v is handled separately because FormatString would
// hand "%#v" to a string and get a quoted string rather than something that
// names the type.
func format(f fmt.State, verb rune, typeName, redacted string) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(goString(typeName, redacted)))
		return
	}
	_, _ = fmt.Fprintf(f, fmt.FormatString(f, verb), redacted)
}

// goString is the shared %#v rendering: Go syntax that names the type and
// quotes the redacted string, so a debug dump is honest about being a
// redaction rather than looking like a literal an operator could paste back
// into a config.
func goString(typeName, redacted string) string {
	return typeName + "(" + strconv.Quote(redacted) + ")"
}

// decodeYAMLString reads a YAML scalar as a string.
//
// This function used to hand every node straight to node.Decode and claim,
// in this comment, that "yaml.v3's own type errors name the node's type and
// line, never its contents". That was false, and verified false by
// execution. It holds for a node whose *kind* is wrong --
//
//	nats_url:
//	  a: 1        ->  line 1: cannot unmarshal !!map into string
//
// -- and not for a scalar carrying an explicit tag, where yaml.v3 echoes
// the whole value while complaining that it cannot resolve it:
//
//	nats_url: !!int <secret>
//	  ->  yaml: cannot decode !!str `<secret>` as a !!int
//
// The same holds for !!bool, !!float, !!null and !!timestamp. It is not a
// regression -- a plain `string` field behaved identically, so this is as
// old as the config struct -- and it needs the operator to have written an
// explicit tag on their own connection string to trigger, which is why it
// was never the finding. It was still a comment asserting a property the
// code did not have, which is how the preceding rounds happened.
//
// So: the node's tag is checked here, before yaml.v3 is asked to resolve
// anything, and only two tags are accepted.
//
//   - "!!str" is every string an operator can write, in every style
//     (plain, single- and double-quoted, literal and folded block). It is
//     passed to node.Decode exactly as before, so the value this function
//     returns for every input that was accepted before is byte-identical.
//   - "!!null" is accepted, but be clear about what that does and does not
//     do, because the obvious reading is wrong. This branch is *dead code
//     under yaml.v3 v3.0.1*: the decoder short-circuits every null before
//     it looks for an Unmarshaler, so decodeYAMLString is never called for
//     `nats_url:`, `~`, `null` or `!!null` -- verified by instrumenting a
//     custom unmarshaler and observing that it does not run for any of the
//     four. The empty-value default that both daemons' LoadConfig depends
//     on is therefore preserved by something else entirely: the field is
//     simply never assigned, so raw stays nil and deref renders "", which
//     is what makes Empty() true. The branch is kept, not deleted, as the
//     guard for the day yaml.v3 drops that short-circuit -- on that day it
//     becomes the thing that keeps `nats_url:` from being rejected -- and
//     it is documented as unreachable so nobody credits it with a job it
//     is not currently doing.
//
// One shape stays open and is not closeable from here, recorded rather
// than glossed: "nats_url: !!null <value>", an explicit null tag on a
// non-empty scalar, still echoes the value. It is the same short-circuit:
// yaml.v3's decoder starts d.unmarshal with
// `if n.ShortTag() == nullTag { return d.null(out) }`, before it looks for
// an Unmarshaler, and the resolve inside ShortTag() is what raises the
// error -- so this function never sees that input either. A plain `string`
// field behaves identically. See
// TestYAMLNullTagWithAValueIsOutOfThisTypesReach, which pins it.
//
// Everything else is rejected with an error built here rather than by
// yaml.v3. That closes the echo for a tag the operator wrote by hand, at
// the cost of three shapes yaml.v3 used to accept, all recorded rather
// than discovered later:
//
//   - An *untagged* scalar that YAML resolves to a non-"!!str" tag.
//     "nats_url: 123" decoded to "123" before and is now rejected as
//     "cannot unmarshal !!int into a string"; likewise `true` (!!bool),
//     `1.5` (!!float) and `2026-08-10` (!!timestamp). No connection string
//     resolves to any of those tags -- a NATS URL or ClickHouse DSN always
//     resolves to !!str -- so this costs nothing real, and it is the
//     fail-closed direction. Quote the value if a future field ever needs
//     one of those literal forms.
//   - A non-standard tag on a scalar ("nats_url: !mytag nats://h:4222",
//     which yaml.v3 decoded as a plain string).
//   - An explicit "!!str" on a non-scalar.
//
// None of the three is a configuration anyone writes for a connection
// string, and an explicit non-string tag on one of these fields is a
// mistake worth saying so about rather than resolving.
func decodeYAMLString(node *yaml.Node) (string, error) {
	switch node.ShortTag() {
	case "!!null":
		// Unreachable under yaml.v3 v3.0.1 -- it resolves every null
		// before consulting an Unmarshaler, so this function is not
		// called for one. Kept as the guard for a yaml.v3 that stops
		// doing that; see this function's doc comment. What actually
		// preserves the empty-value default today is raw staying nil.
		return "", nil
	case "!!str":
		if node.Kind != yaml.ScalarNode {
			break
		}
		var raw string
		if err := node.Decode(&raw); err != nil {
			// Unreachable in practice -- a !!str scalar always decodes
			// into a string -- but if yaml.v3 ever disagrees, its error
			// for this case names the kinds, not the value.
			return "", err
		}
		return raw, nil
	}
	return "", fmt.Errorf("line %d: cannot unmarshal %s into a string",
		node.Line, describeYAMLTag(node.ShortTag()))
}

// describeYAMLTag names a node's tag in an error message, and echoes it
// only if it is one of YAML's own.
//
// A tag is not the node's value, but it is still operator-written text: a
// YAML tag shorthand is "!" followed by URI characters, and ':', '/' and
// '@' are all URI characters, so "!clickhouse://u:pw@h/db" is a
// syntactically legal tag. Nobody writes that, and the point of the
// preceding rounds is that "nobody writes that" is not the standard this
// code is held to. An unrecognized tag is therefore described, not quoted
// -- the same additive rule redact applies to a URL's bytes.
func describeYAMLTag(tag string) string {
	switch tag {
	case "!!str", "!!null", "!!bool", "!!int", "!!float", "!!timestamp",
		"!!binary", "!!seq", "!!map", "!!merge":
		return tag
	}
	return "a non-standard tag"
}
