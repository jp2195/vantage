package secret

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sentinel is embedded in every credential these tests use and is the one
// thing they grep for. Every assertion here is an assertion of *absence*:
// an expected-output assertion passes whenever the renderer changes the way
// its author imagined, so it pins an instance and misses the class.
// Absence only passes when the credential is really gone, however the rest
// of the line is spelled.
const sentinel = "SECRET"

// clickHouseSecrets are DSNs whose password contains the sentinel, one per
// shape known to defeat a redactor.
var clickHouseSecrets = []struct{ name, raw string }{
	{"ordinary userinfo password", "clickhouse://vantage:hunter" + sentinel + "@127.0.0.1:9000/vantage"},
	// The %q-escaping bypass: strconv.Quote escapes '"' and '\', so a literal
	// ReplaceAll(err.Error(), raw, ...) matched nothing and returned the
	// whole DSN.
	{"password with a double quote", `clickhouse://vantage:sup"` + sentinel + `@127.0.0.1:9000/vantage`},
	{"password with a backslash", `clickhouse://vantage:sup\` + sentinel + `@127.0.0.1:9000/vantage`},
	// The slash bypass: an unencoded '/' ends the authority early, so
	// net/url rejects the DSN and the regexp fallback returned it verbatim.
	{"password with a slash", "clickhouse://vantage:ab/cd" + sentinel + "@127.0.0.1:19000/vantage"},
	{"password with a leading slash", "clickhouse://vantage:/lead" + sentinel + "@127.0.0.1:19000"},
	// The query-parameter bypass: clickhouse-go
	// accepts the password as a query parameter, so there is no '@' in the
	// string at all.
	{"password in a query parameter", "clickhouse://127.0.0.1:9000/vantage?password=" + sentinel},
	{"password in a query parameter, with an @", "clickhouse://127.0.0.1:9000/vantage?password=p@ss" + sentinel},
	{"password in a query parameter of a multi-host DSN", "clickhouse://h1:9000,h2:9000/vantage?username=v&password=" + sentinel},
	{"password in an http_proxy parameter", "clickhouse://127.0.0.1:9000/vantage?http_proxy=http://u:" + sentinel + "@proxy:8080"},
	// The bypass a live test harness found: an '@' and then a '/' in the
	// password make net/url parse a userinfo that is not the real one, so
	// clearing the parsed password left the rest of it in the host and
	// path.
	{"password containing an @ and a slash", "clickhouse://u:p@ss/word" + sentinel + "@127.0.0.1:9000/vantage"},
	{"unparseable garbage", "clickhouse://vantage:" + sentinel + "\x01@127.0.0.1:9000"},
}

// natsSecrets are NATS URLs whose credential contains the sentinel, across
// the axes NATS has that ClickHouse does not: a comma-separated multi-server
// list, a schemeless shorthand, and a colonless userinfo that is an auth
// token rather than a username.
var natsSecrets = []struct{ name, raw string }{
	{"ordinary userinfo password", "nats://user:s3cr3t" + sentinel + "@127.0.0.1:4222"},
	// The schemeless-shorthand bypass: it parses as an opaque URL with a
	// nil User, so the parse-based branch found no userinfo to strip.
	{"schemeless shorthand", "user:s3cr3t" + sentinel + "@127.0.0.1:4222"},
	{"auth token", "nats://token" + sentinel + "@127.0.0.1:4222"},
	// The multi-server bypass: the leaking text is a substring of the
	// config value, never the whole of it.
	{"multi-server list", "nats://user:first" + sentinel + "@h1:4222,nats://user:second" + sentinel + "@h2:4222"},
	{"multi-server list, schemeless segment", "nats://h1:4222,user:second" + sentinel + "@h2:4222"},
	{"password with a slash", "nats://user:ab/cd" + sentinel + "@127.0.0.1:4222"},
	{"password containing an @ and a slash", "nats://u:p@ss/word" + sentinel + "@127.0.0.1:4222"},
	{"unparseable garbage", "nats://user:" + sentinel + "\x01@127.0.0.1:4222"},
}

// apiTokenSecrets are bearer tokens containing the sentinel, across the
// shapes an operator or a generator actually produces. Unlike the two URL
// types there is no grammar here to get wrong -- which is the point: a
// token is an opaque run of bytes, every one of them the credential, so
// these shapes exist to catch a renderer that tried to be clever about the
// ones that look like something else.
var apiTokenSecrets = []struct{ name, raw string }{
	{"opaque random string", "tok_" + sentinel + "_9f3a2b1c"},
	{"base64url with padding", "dG9rZW4" + sentinel + "=="},
	// A token that parses as a URL, so a renderer that reached for
	// redact.URL would keep the host and hand back most of the credential.
	{"looks like a URL", "https://user:" + sentinel + "@example.com/x"},
	// A token that parses as userinfo, which is what redact.NatsURL treats
	// as a bare auth token.
	{"looks like userinfo", "user:" + sentinel + "@host:4222"},
	{"jwt-shaped", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIx" + sentinel + "In0.c2ln" + sentinel},
	{"with a double quote", `sup"` + sentinel},
	{"with a backslash", `sup\` + sentinel},
	{"with a newline", "line1\n" + sentinel},
	{"non-printing bytes", sentinel + "\x01\x7f"},
	{"very long", strings.Repeat("a", 512) + sentinel},
	{"the sentinel alone", sentinel},
}

// verbs is every formatting directive these types could plausibly meet,
// including the ones that have nothing to do with strings. %d and %t are
// here because fmt renders a verb a type does not support by reflecting
// over the operand and printing its fields, and %w and %p are here because
// they reach that reflection path even when the type implements
// fmt.Formatter -- fmt's badVerb disables interface dispatch before
// re-printing the operand. Those two are the reason the raw value lives
// behind a pointer; without it they print the password in full.
var verbs = []string{
	"%v", "%+v", "%#v", "%s", "%q", "%x", "%X",
	"%d", "%b", "%o", "%c", "%U", "%e", "%f", "%g", "%t", "%T",
	"%p", "%w",
	"%-40s|", "%40s|", "%.4s", "%08.3q", "%[1]s %[1]v %[1]q",
}

// assertNoSentinel is the single assertion every test in this file makes.
func assertNoSentinel(t *testing.T, what, raw, got string) {
	t.Helper()
	if strings.Contains(got, sentinel) {
		t.Errorf("%s of %q rendered %q, which still contains the credential", what, raw, got)
	}
}

// TestNoVerbLeaksTheSecret is the class test for the type: every verb in
// verbs, applied to every credential shape, directly and nested inside the
// containers a config value actually turns up in.
//
// The nesting matters as much as the verbs. fmt dispatches to a Formatter
// for a struct field, a map value or a slice element as well as for a
// top-level operand, so `slog.Info("cfg", "cfg", cfg)` on a whole Config is
// covered by the same mechanism -- but only if the mechanism really is
// interface dispatch and not something that happens to work at depth 0.
func TestNoVerbLeaksTheSecret(t *testing.T) {
	for _, c := range clickHouseSecrets {
		t.Run("clickhouse/"+c.name, func(t *testing.T) {
			assertNoVerbLeaks(t, c.raw, NewClickHouseDSN(c.raw))
		})
	}
	for _, c := range natsSecrets {
		t.Run("nats/"+c.name, func(t *testing.T) {
			assertNoVerbLeaks(t, c.raw, NewNatsURL(c.raw))
		})
	}
	for _, c := range apiTokenSecrets {
		t.Run("apitoken/"+c.name, func(t *testing.T) {
			assertNoVerbLeaks(t, c.raw, NewAPIToken(c.raw))
		})
	}
}

func assertNoVerbLeaks(t *testing.T, raw string, v any) {
	t.Helper()

	// The container shapes a config value is actually printed in: bare, as
	// a pointer, inside a struct (a whole Config dumped with %+v), inside a
	// map and inside a slice.
	type wrapper struct {
		DSN   any
		Other string
	}
	operands := []struct {
		name string
		arg  any
	}{
		{"bare", v},
		{"struct field", wrapper{DSN: v, Other: "x"}},
		{"pointer to struct", &wrapper{DSN: v}},
		{"map value", map[string]any{"dsn": v}},
		{"slice element", []any{v}},
	}
	for _, op := range operands {
		for _, verb := range verbs {
			got := fmt.Sprintf(verb, op.arg)
			assertNoSentinel(t, fmt.Sprintf("Sprintf(%q) of %s", verb, op.name), raw, got)
		}
		assertNoSentinel(t, "Sprint of "+op.name, raw, fmt.Sprint(op.arg))
		assertNoSentinel(t, "Sprintln of "+op.name, raw, fmt.Sprintln(op.arg))
		assertNoSentinel(t, "Errorf %v of "+op.name, raw, fmt.Errorf("ctx: %v", op.arg).Error())
		// %w on a non-error is the misuse that reaches fmt's reflection
		// dump; a contributor reaching for the wrapping verb out of habit
		// must not be how a password gets logged. The format string is a
		// variable because vet -- correctly -- rejects a constant one, and
		// this test is precisely about what happens when someone writes it
		// anyway.
		wrapVerb := "ctx: %w"
		assertNoSentinel(t, "Errorf %w of "+op.name, raw, fmt.Errorf(wrapVerb, op.arg).Error())
	}

	// And the direct method calls, which is what a caller who knows the
	// type does.
	if s, ok := v.(fmt.Stringer); ok {
		assertNoSentinel(t, "String()", raw, s.String())
	} else {
		t.Errorf("%T does not implement fmt.Stringer", v)
	}
	if g, ok := v.(interface{ GoString() string }); ok {
		assertNoSentinel(t, "GoString()", raw, g.GoString())
	} else {
		t.Errorf("%T does not implement fmt.GoStringer", v)
	}
	if m, ok := v.(interface{ MarshalText() ([]byte, error) }); ok {
		b, err := m.MarshalText()
		if err != nil {
			t.Errorf("MarshalText: %v", err)
		}
		assertNoSentinel(t, "MarshalText()", raw, string(b))
	} else {
		t.Errorf("%T does not implement encoding.TextMarshaler", v)
	}
}

// TestSlogDoesNotLeak covers the exact call both binaries make on a fatal
// startup error -- slog.Error("fatal", "err", err) -- plus the value logged
// as an attribute in its own right, through both handlers. slog resolves a
// LogValuer before the handler sees it; the handlers' fallbacks
// (TextMarshaler for text, encoding/json for JSON) are covered too by
// logging a struct that contains the value, which is the path a whole
// Config takes.
func TestSlogDoesNotLeak(t *testing.T) {
	type cfg struct {
		DSN  ClickHouseDSN
		Nats NatsURL
		Tok  APIToken
	}
	for _, c := range clickHouseSecrets {
		for _, nc := range natsSecrets[:1] {
			value := cfg{DSN: NewClickHouseDSN(c.raw), Nats: NewNatsURL(nc.raw),
				Tok: NewAPIToken(apiTokenSecrets[0].raw)}
			for _, h := range []struct {
				name string
				new  func(*bytes.Buffer) slog.Handler
			}{
				{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
				{"json", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
			} {
				t.Run(h.name+"/"+c.name, func(t *testing.T) {
					var buf bytes.Buffer
					log := slog.New(h.new(&buf))
					log.Info("startup", "dsn", value.DSN, "nats", value.Nats,
						"token", value.Tok, "cfg", value)
					err := fmt.Errorf("connect clickhouse %s: %w", value.DSN,
						fmt.Errorf("connect nats %s: %w", value.Nats, fmt.Errorf("dial refused")))
					log.Error("fatal", "err", err)
					assertNoSentinel(t, "slog "+h.name, c.raw, buf.String())
					assertNoSentinel(t, "slog "+h.name, nc.raw, buf.String())
					assertNoSentinel(t, "slog "+h.name, value.Tok.RevealSecret(), buf.String())
				})
			}
		}
	}
}

// TestSerializationDoesNotLeak covers the two encoders a config struct is
// most likely to meet: encoding/json (a debug endpoint, a config dump) and
// yaml.Marshal (writing a config back out). Both must render the redacted
// form -- marshaling is deliberately lossy here.
func TestSerializationDoesNotLeak(t *testing.T) {
	type cfg struct {
		DSN  ClickHouseDSN `yaml:"clickhouse_dsn" json:"clickhouse_dsn"`
		Nats NatsURL       `yaml:"nats_url" json:"nats_url"`
		Tok  APIToken      `yaml:"token" json:"token"`
	}
	for _, c := range clickHouseSecrets {
		value := cfg{DSN: NewClickHouseDSN(c.raw), Nats: NewNatsURL(natsSecrets[0].raw),
			Tok: NewAPIToken(apiTokenSecrets[0].raw)}

		b, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		assertNoSentinel(t, "json.Marshal", c.raw, string(b))

		y, err := yaml.Marshal(value)
		if err != nil {
			t.Fatalf("yaml.Marshal: %v", err)
		}
		assertNoSentinel(t, "yaml.Marshal", c.raw, string(y))
	}
}

// TestYAMLRoundTrip is the load path both daemons' LoadConfig uses --
// yaml.NewDecoder with KnownFields(true) -- against the shape of both
// config structs at once. A defined string type would have made this work
// by accident; a struct with an unexported field only works because of the
// UnmarshalYAML method, so this is the test that would catch its removal.
//
// The values here contain the characters that break naive YAML quoting
// (a '"' and a '\' in a password) for the same reason redact's
// tests use them.
func TestYAMLRoundTrip(t *testing.T) {
	type cfg struct {
		NatsURL       NatsURL       `yaml:"nats_url"`
		ClickHouseDSN ClickHouseDSN `yaml:"clickhouse_dsn"`
		Token         APIToken      `yaml:"token"`
		BatchRows     int           `yaml:"batch_rows"`
	}
	const (
		nats  = `nats://user:sup"secret@127.0.0.1:4222,nats://user:b\ck@h2:4222`
		dsn   = `clickhouse://vantage:ab/cd@127.0.0.1:9000/vantage?password=p@ss`
		token = `sup"tok\en: with #yaml @characters`
	)
	body, err := yaml.Marshal(map[string]any{
		"nats_url": nats, "clickhouse_dsn": dsn, "token": token, "batch_rows": 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got cfg
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NatsURL.RevealSecret() != nats {
		t.Errorf("nats_url round-tripped as %q, want %q", got.NatsURL.RevealSecret(), nats)
	}
	if got.ClickHouseDSN.RevealSecret() != dsn {
		t.Errorf("clickhouse_dsn round-tripped as %q, want %q", got.ClickHouseDSN.RevealSecret(), dsn)
	}
	if got.Token.RevealSecret() != token {
		t.Errorf("token round-tripped as %q, want %q", got.Token.RevealSecret(), token)
	}
	if got.BatchRows != 7 {
		t.Errorf("batch_rows = %d, want 7 -- decoding the neighboring fields must be unaffected", got.BatchRows)
	}

	// KnownFields(true) must still reject a typo'd key: the custom
	// unmarshalers consume their own scalar nodes and must not disturb the
	// decoder's strictness for the mapping around them.
	var strict cfg
	dec = yaml.NewDecoder(strings.NewReader("nats_urls: nats://h:4222\n"))
	dec.KnownFields(true)
	if err := dec.Decode(&strict); err == nil {
		t.Error("decoding an unknown field returned nil, want KnownFields(true) to reject it")
	}

	// A non-scalar node must be an error rather than a silent zero value.
	var bad cfg
	dec = yaml.NewDecoder(strings.NewReader("nats_url:\n  a: 1\n"))
	dec.KnownFields(true)
	if err := dec.Decode(&bad); err == nil {
		t.Error("decoding a mapping into a NatsURL returned nil, want a type error")
	}
}

// TestRevealSecretReturnsTheRawValue pins the other half of the contract:
// the accessor must return the operator's string byte for byte, since the
// libraries have to parse the value the operator actually wrote (NATS's
// multi-server list and ClickHouse's multi-host authority are each that
// library's own grammar, not net/url's).
func TestRevealSecretReturnsTheRawValue(t *testing.T) {
	for _, c := range clickHouseSecrets {
		if got := NewClickHouseDSN(c.raw).RevealSecret(); got != c.raw {
			t.Errorf("RevealSecret() = %q, want %q", got, c.raw)
		}
	}
	for _, c := range natsSecrets {
		if got := NewNatsURL(c.raw).RevealSecret(); got != c.raw {
			t.Errorf("RevealSecret() = %q, want %q", got, c.raw)
		}
	}
	for _, c := range apiTokenSecrets {
		if got := NewAPIToken(c.raw).RevealSecret(); got != c.raw {
			t.Errorf("RevealSecret() = %q, want %q", got, c.raw)
		}
	}
}

// TestRenderingStaysActionable is the counterweight to every absence
// assertion above: failing closed is trivial if the answer is allowed to be
// a constant, and an error that does not name the endpoint just relocates
// the operator's problem.
func TestRenderingStaysActionable(t *testing.T) {
	for _, c := range []struct{ raw, wantHost string }{
		{"clickhouse://vantage:hunter" + sentinel + "@ch.internal:9000/vantage", "ch.internal:9000"},
		{"clickhouse://vantage:ab/cd" + sentinel + "@ch.internal:19000/vantage", "ch.internal:19000"},
		{"clickhouse://h1:9000,h2:9000/vantage?password=" + sentinel, "h1:9000,h2:9000"},
		{"clickhouse://ch.internal:9000/vantage?password=p@ss" + sentinel, "ch.internal"},
	} {
		got := fmt.Sprintf("%s", NewClickHouseDSN(c.raw))
		assertNoSentinel(t, "ClickHouseDSN", c.raw, got)
		if !strings.Contains(got, c.wantHost) {
			t.Errorf("ClickHouseDSN(%q) rendered %q, which does not name the host %q", c.raw, got, c.wantHost)
		}
	}
	for _, c := range []struct{ raw, wantHost string }{
		{"nats://user:s3cr3t" + sentinel + "@nats.internal:4222", "nats.internal:4222"},
		{"user:s3cr3t" + sentinel + "@nats.internal:4222", "nats.internal:4222"},
		{"nats://token" + sentinel + "@nats.internal:4222", "nats.internal:4222"},
		{"nats://user:a" + sentinel + "@h1:4222,nats://user:b" + sentinel + "@h2:4222", "h2:4222"},
	} {
		got := fmt.Sprintf("%s", NewNatsURL(c.raw))
		assertNoSentinel(t, "NatsURL", c.raw, got)
		if !strings.Contains(got, c.wantHost) {
			t.Errorf("NatsURL(%q) rendered %q, which does not name the host %q", c.raw, got, c.wantHost)
		}
	}
}

// TestZeroValueIsUsable pins that an unset field -- a Config decoded from a
// file that names neither key -- renders as empty and reports Empty rather
// than panicking on its nil pointer, since that is exactly the state
// LoadConfig inspects before substituting a default.
func TestZeroValueIsUsable(t *testing.T) {
	var dsn ClickHouseDSN
	var nats NatsURL
	var tok APIToken
	if !dsn.Empty() || !nats.Empty() || !tok.Empty() {
		t.Error("zero value is not Empty()")
	}
	if got := fmt.Sprintf("%v|%s|%q|%#v", dsn, dsn, dsn, dsn); got == "" {
		t.Error("formatting the zero value produced nothing")
	}
	if dsn.RevealSecret() != "" || nats.RevealSecret() != "" || tok.RevealSecret() != "" {
		t.Error("zero value does not reveal as the empty string")
	}
	if !NewNatsURL("").Empty() {
		t.Error(`NewNatsURL("") is not Empty()`)
	}
	if !NewAPIToken("").Empty() {
		t.Error(`NewAPIToken("") is not Empty()`)
	}
	// An unset token still renders as the marker rather than as nothing:
	// String never reads its receiver, so there is no state in which it
	// returns a different string. api.LoadConfig distinguishes the two
	// cases with Empty, which is the only thing that can.
	if got := fmt.Sprintf("%s", tok); got != apiTokenRedacted {
		t.Errorf("zero APIToken rendered %q, want %q", got, apiTokenRedacted)
	}
}

// TestValidateRejectsAndRedacts pins that the pre-flight check both
// binaries run reports a malformed value without quoting the credential in
// it -- the message an operator sees on every single startup when their
// password contains a '/'.
func TestValidateRejectsAndRedacts(t *testing.T) {
	dsn := NewClickHouseDSN("clickhouse://vantage:ab/cd" + sentinel + "@127.0.0.1:19000/vantage")
	err := dsn.Validate()
	if err == nil {
		t.Fatal("Validate returned nil for a DSN net/url rejects")
	}
	assertNoSentinel(t, "ClickHouseDSN.Validate", dsn.RevealSecret(), err.Error())

	nats := NewNatsURL(`nats://h1:4222,nats://user:sup"` + sentinel + `@h2:4222`)
	err = nats.Validate()
	if err == nil {
		t.Fatal("Validate returned nil for a multi-server value whose second segment is malformed")
	}
	assertNoSentinel(t, "NatsURL.Validate", nats.RevealSecret(), err.Error())

	if err := NewNatsURL("nats://127.0.0.1:4222").Validate(); err != nil {
		t.Errorf("Validate rejected a well-formed URL: %v", err)
	}
	if err := NewClickHouseDSN("clickhouse://vantage:pw@127.0.0.1:9000/vantage").Validate(); err != nil {
		t.Errorf("Validate rejected a well-formed DSN: %v", err)
	}
}

// TestVerbsRenderTheRedactedForm is the presence half of the contract, and
// the test that fails if fmt.Formatter is ever dropped in favor of "String
// is surely enough".
//
// Absence assertions alone cannot catch that: with only a Stringer, "%d"
// falls through to fmt's reflection printer, which prints the unexported
// pointer field as an address -- no credential, but no diagnosis either,
// and a call site whose error message has silently become "0xc000112233".
// Every verb must render the redaction, not merely fail to render the
// secret.
func TestVerbsRenderTheRedactedForm(t *testing.T) {
	dsn := NewClickHouseDSN("clickhouse://vantage:hunter" + sentinel + "@127.0.0.1:9000/vantage")
	want := dsn.String()
	if !strings.Contains(want, "127.0.0.1:9000") {
		t.Fatalf("test setup: String() = %q does not name the host", want)
	}
	for _, c := range []struct{ verb, want string }{
		{"%v", want},
		{"%+v", want},
		{"%s", want},
		{"%q", `"` + want + `"`},
		{"%#v", dsn.GoString()},
	} {
		if got := fmt.Sprintf(c.verb, dsn); got != c.want {
			t.Errorf("Sprintf(%q) = %q, want %q", c.verb, got, c.want)
		}
	}
	// A verb the type has no business with still renders the redaction,
	// wrapped in fmt's own bad-verb complaint, rather than a reflection
	// dump of the struct.
	if got := fmt.Sprintf("%d", dsn); !strings.Contains(got, want) {
		t.Errorf("Sprintf(%%d) = %q, want it to contain the redacted rendering %q", got, want)
	}

	nats := NewNatsURL("nats://user:s3cr3t" + sentinel + "@h1:4222,nats://user:x" + sentinel + "@h2:4222")
	if got, want := fmt.Sprintf("%s", nats), nats.String(); got != want {
		t.Errorf("Sprintf(%%s) on a NatsURL = %q, want %q", got, want)
	}

	// slog must log the same rendering, not a struct dump.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{})).Info("m", "dsn", dsn)
	if !strings.Contains(buf.String(), want) {
		t.Errorf("slog rendered %q, want it to contain %q", buf.String(), want)
	}

	// The same for APIToken, whose whole rendering is the marker. %d is the
	// verb that discriminates: with only a Stringer it would fall through
	// to fmt's reflection printer and render the unexported pointer as a
	// hex address -- no credential, and no diagnosis either.
	tok := NewAPIToken("tok_" + sentinel)
	for _, c := range []struct{ verb, want string }{
		{"%v", apiTokenRedacted},
		{"%+v", apiTokenRedacted},
		{"%s", apiTokenRedacted},
		{"%q", `"` + apiTokenRedacted + `"`},
		{"%#v", `secret.APIToken("` + apiTokenRedacted + `")`},
	} {
		if got := fmt.Sprintf(c.verb, tok); got != c.want {
			t.Errorf("Sprintf(%q) on an APIToken = %q, want %q", c.verb, got, c.want)
		}
	}
	if got := fmt.Sprintf("%d", tok); !strings.Contains(got, apiTokenRedacted) {
		t.Errorf("Sprintf(%%d) on an APIToken = %q, want it to contain %q", got, apiTokenRedacted)
	}
}

// TestAPITokenNeverPrints is the property secret.APIToken exists for: the
// token must have no printable form, so no error path, log line, or config
// dump can leak it by accident rather than by intent.
func TestAPITokenNeverPrints(t *testing.T) {
	const raw = "s3cr3t-token-value"
	tok := NewAPIToken(raw)
	for _, got := range []string{
		fmt.Sprintf("%s", tok), fmt.Sprintf("%v", tok), fmt.Sprintf("%#v", tok),
		fmt.Sprint(tok), tok.String(),
	} {
		if strings.Contains(got, raw) {
			t.Errorf("token leaked through a formatting verb: %q", got)
		}
	}
	b, err := json.Marshal(struct{ T APIToken }{tok})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte(raw)) {
		t.Errorf("token leaked through JSON: %s", b)
	}
	if tok.RevealSecret() != raw {
		t.Error("RevealSecret must still return the real value")
	}
}

// TestAPITokenRedactionPathsAreAllPresent is the presence half of the
// property above, and it is the half that has teeth. Absence alone is
// nearly vacuous for this type: APIToken's only field is unexported, so
// dropping MarshalText leaves encoding/json emitting "{}" -- no leak, it
// passes every leak assertion in this file, and it quietly turns a config
// dump into a lie about what is configured.
//
// LogValue and MarshalYAML are worse, because deleting either changes
// nothing observable at all. Both stock slog handlers fall back to
// TextMarshaler and print the identical marker without LogValue, and
// yaml.Marshal still emits "token: REDACTED" without MarshalYAML, because
// yaml.v3 falls back to TextMarshaler too (encode.go:150).
//
// A yaml.Marshal assertion can therefore pass on a mechanism other than
// the one it names. See this package's doc comment for the yaml.v3
// file:line evidence.
//
// So every method is asserted by what it returns when called directly. The
// two encoder-level assertions are kept for what they actually are: proof
// that the whole chain emits the right bytes, not proof of which method
// emitted them.
func TestAPITokenRedactionPathsAreAllPresent(t *testing.T) {
	tok := NewAPIToken("tok_" + sentinel)

	// encoding.TextMarshaler, via the encoder that uses it.
	b, err := json.Marshal(struct {
		T APIToken `json:"token"`
	}{tok})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if want := `{"token":"` + apiTokenRedacted + `"}`; string(b) != want {
		t.Errorf("json.Marshal = %s, want %s", b, want)
	}

	// The encoder-level YAML result. This passes with MarshalYAML deleted
	// -- see this test's doc comment -- so it is here as the chain check it
	// is, not as MarshalYAML's pin. That is the direct call below.
	y, err := yaml.Marshal(struct {
		T APIToken `yaml:"token"`
	}{tok})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if want := "token: " + apiTokenRedacted + "\n"; string(y) != want {
		t.Errorf("yaml.Marshal = %q, want %q", y, want)
	}

	// slog.LogValuer, checked at the Value rather than through a handler:
	// a handler's TextMarshaler fallback renders the same string, so only
	// the resolved Value distinguishes the type that implements LogValue
	// from the type that does not.
	v := slog.AnyValue(tok).Resolve()
	if v.Kind() != slog.KindString {
		t.Errorf("slog.AnyValue(APIToken).Resolve().Kind() = %v, want KindString -- "+
			"an unresolved value means LogValue is missing and every handler is "+
			"deciding for itself", v.Kind())
	}
	if v.String() != apiTokenRedacted {
		t.Errorf("resolved slog value = %q, want %q", v.String(), apiTokenRedacted)
	}

	// fmt.GoStringer, called directly rather than through %#v, which
	// Format would answer on its own.
	if got, want := tok.GoString(), `secret.APIToken("`+apiTokenRedacted+`")`; got != want {
		t.Errorf("GoString() = %q, want %q", got, want)
	}

	// yaml.Marshaler, called directly for the same reason: yaml.Marshal
	// above would answer for it. A direct call pins the method's deletion
	// at compile time exactly as an interface assertion would, and pins
	// what it returns as well.
	my, err := tok.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if got, ok := my.(string); !ok || got != apiTokenRedacted {
		t.Errorf("MarshalYAML() = %#v, want the string %q", my, apiTokenRedacted)
	}

	// The interfaces themselves, so that dropping one is a compile error
	// here rather than a behavior change somewhere else.
	var (
		_ fmt.Stringer           = tok
		_ fmt.Formatter          = tok
		_ fmt.GoStringer         = tok
		_ encoding.TextMarshaler = tok
		_ slog.LogValuer         = tok
		_ yaml.Unmarshaler       = &tok
	)
}

// TestEveryTypeIsPinnedForSlogAndYAML closes for ClickHouseDSN and NatsURL
// the blind spot the test above closes for APIToken, because the blind spot
// was never APIToken's: LogValue and MarshalYAML are the two methods on all
// three types whose deletion is invisible. log/slog's handlers fall back to
// TextMarshaler, and so does yaml.v3 (encode.go:150), so with either method
// gone every leak assertion in this file still passes, every handler in the
// tree still prints the identical redaction, and nothing anywhere notices.
// Only the resolved slog.Value and MarshalYAML's own return value tell the
// type that implements them from the type that does not.
//
// The redactable interface is half the point: naming these methods in an
// interface every type must satisfy makes deleting one a compile error
// here, which is the same instrument the direct GoString and MarshalYAML
// calls above use, applied to all three types at once.
func TestEveryTypeIsPinnedForSlogAndYAML(t *testing.T) {
	type redactable interface {
		String() string
		LogValue() slog.Value
		MarshalYAML() (any, error)
	}
	for _, c := range []struct {
		name string
		raw  string
		v    redactable
	}{
		{"ClickHouseDSN", clickHouseSecrets[0].raw, NewClickHouseDSN(clickHouseSecrets[0].raw)},
		{"NatsURL", natsSecrets[0].raw, NewNatsURL(natsSecrets[0].raw)},
		{"APIToken", apiTokenSecrets[0].raw, NewAPIToken(apiTokenSecrets[0].raw)},
	} {
		t.Run(c.name, func(t *testing.T) {
			// String() is the expectation the other two must reproduce.
			// Using it that way is not circular, because it is asserted to
			// be a redaction first: a String() that leaked would fail here
			// before it could become a passing expectation below.
			want := c.v.String()
			assertNoSentinel(t, c.name+".String", c.raw, want)

			v := slog.AnyValue(c.v).Resolve()
			if v.Kind() != slog.KindString {
				t.Errorf("slog.AnyValue(%s).Resolve().Kind() = %v, want KindString -- "+
					"a value that does not resolve means LogValue is missing and every "+
					"handler is deciding for itself", c.name, v.Kind())
			}
			if got := v.String(); got != want {
				t.Errorf("%s resolved slog value = %q, want %q", c.name, got, want)
			}

			y, err := c.v.MarshalYAML()
			if err != nil {
				t.Fatalf("%s.MarshalYAML: %v", c.name, err)
			}
			if got, ok := y.(string); !ok || got != want {
				t.Errorf("%s.MarshalYAML() = %#v, want the string %q", c.name, y, want)
			}
		})
	}
}

// TestAPITokenRenderingIsIndependentOfTheToken is what "no printable form"
// means beyond "does not contain the credential". A rendering that showed
// the first four characters, or the length, or merely whether a token was
// configured, would pass every absence assertion in this file while handing
// out exactly the information that shortens a search. String does not read
// its receiver, and this is the test that fails if it ever starts to.
func TestAPITokenRenderingIsIndependentOfTheToken(t *testing.T) {
	for _, c := range apiTokenSecrets {
		for _, got := range []string{
			NewAPIToken(c.raw).String(),
			fmt.Sprintf("%s", NewAPIToken(c.raw)),
			fmt.Sprintf("%v", NewAPIToken(c.raw)),
		} {
			if got != apiTokenRedacted {
				t.Errorf("%s rendered %q, want the same %q every other token renders as",
					c.name, got, apiTokenRedacted)
			}
		}
	}
	if NewAPIToken("").String() != NewAPIToken("x").String() {
		t.Error("an unset token renders differently from a set one, which discloses that one is configured")
	}
}

// TestYAMLTypeErrorDoesNotEchoTheValue closes a claim this package's own
// comment used to make and did not have: that "yaml.v3's own type errors
// name the node's type and line, never its contents".
//
// True for a wrong *kind* (a mapping into a string), false for a scalar
// carrying an explicit tag, where yaml.v3 echoes the whole value:
//
//	nats_url: !!int <secret>
//	  ->  yaml: cannot decode !!str `<secret>` as a !!int
//
// Not a regression -- a plain `string` field behaved identically, so it
// predates these types -- and it needs the operator to have written the tag
// themselves, which is why it was never the finding. decodeYAMLString now
// accepts only "!!str" and "!!null" and builds the rejection itself, so no
// tagged value reaches yaml.v3's resolver to be quoted.
//
// The tag is not echoed either unless it is one of YAML's own: a tag
// shorthand is "!" plus URI characters, and ':', '/' and '@' are all URI
// characters, so "!clickhouse://u:pw@h/db" is syntactically a legal tag.
// Nobody writes that; "nobody writes that" is not the standard here.
func TestYAMLTypeErrorDoesNotEchoTheValue(t *testing.T) {
	type cfg struct {
		NatsURL       NatsURL       `yaml:"nats_url"`
		ClickHouseDSN ClickHouseDSN `yaml:"clickhouse_dsn"`
	}
	for _, field := range []string{"nats_url", "clickhouse_dsn"} {
		// "!!null" is deliberately absent: it is the one tag yaml.v3
		// never gives this type a chance at. See
		// TestYAMLNullTagWithAValueIsOutOfThisTypesReach below.
		for _, tag := range []string{
			"!!int", "!!bool", "!!float", "!!timestamp", "!!binary",
			"!" + sentinel, "!nats://u:" + sentinel + "@h:4222",
		} {
			doc := field + ": " + tag + " nats://user:" + sentinel + "@h:4222\n"
			t.Run(field+" "+tag, func(t *testing.T) {
				var got cfg
				dec := yaml.NewDecoder(strings.NewReader(doc))
				dec.KnownFields(true)
				err := dec.Decode(&got)
				if err == nil {
					t.Fatalf("decoding %q returned nil, want a rejection -- an "+
						"explicit non-string tag on a connection string is a mistake", doc)
				}
				if strings.Contains(err.Error(), sentinel) {
					t.Errorf("decoding %q failed with %q, which echoes the value", doc, err)
				}
			})
		}
	}
}

// TestYAMLAcceptsEveryStringAnOperatorCanWrite is the other half: the tag
// check must not reject a configuration that worked before it existed.
// Every YAML scalar style resolves to "!!str" and must round-trip byte for
// byte, and an explicitly empty value must stay the silent "" that both
// daemons' LoadConfig reads as "unset, use the default" -- decodeYAMLString
// accepting "!!null" is what preserves that, and it is easy to drop.
func TestYAMLAcceptsEveryStringAnOperatorCanWrite(t *testing.T) {
	type cfg struct {
		NatsURL NatsURL `yaml:"nats_url"`
	}
	const raw = `nats://user:pw` + sentinel + `@h:4222`
	for _, c := range []struct{ name, doc, want string }{
		{"plain", "nats_url: " + raw + "\n", raw},
		{"single-quoted", "nats_url: '" + raw + "'\n", raw},
		{"double-quoted", `nats_url: "` + raw + "\"\n", raw},
		{"explicit !!str", "nats_url: !!str " + raw + "\n", raw},
		{"literal block", "nats_url: |-\n  " + raw + "\n", raw},
		{"folded block", "nats_url: >-\n  " + raw + "\n", raw},
		{"quoted with an escape", `nats_url: "a\tb` + sentinel + "\"\n", "a\tb" + sentinel},
		{"absent value", "nats_url:\n", ""},
		{"explicit null", "nats_url: ~\n", ""},
		{"explicit !!null", "nats_url: !!null\n", ""},
		{"empty string", `nats_url: ""` + "\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got cfg
			dec := yaml.NewDecoder(strings.NewReader(c.doc))
			dec.KnownFields(true)
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decoding %q: %v", c.doc, err)
			}
			if got.NatsURL.RevealSecret() != c.want {
				t.Errorf("decoding %q gave %q, want %q",
					c.doc, got.NatsURL.RevealSecret(), c.want)
			}
			if c.want == "" && !got.NatsURL.Empty() {
				t.Errorf("decoding %q left Empty() false -- LoadConfig would not "+
					"substitute the default", c.doc)
			}
		})
	}
}

// TestYAMLNullTagWithAValueIsOutOfThisTypesReach pins the one tagged-scalar
// shape the fix above does *not* close, so that it is a recorded fact with a
// test on it rather than a silent gap.
//
// "nats_url: !!null <value>" -- an explicit null tag on a non-empty scalar
// -- still echoes the value. Not for want of trying: yaml.v3's decoder
// begins d.unmarshal with
//
//	if n.ShortTag() == nullTag { return d.null(out) }
//
// which runs *before* it looks for an Unmarshaler, and the resolve inside
// ShortTag() is what raises "cannot decode !!str `<value>` as a !!null".
// UnmarshalYAML is never called, so decodeYAMLString cannot intervene --
// confirmed by execution: with a custom unmarshaler installed the method
// does not run for this input, while it does run for every other tag.
// A plain `string` field produces the identical error, so this predates
// these types and is not a regression.
//
// Closing it would mean either scrubbing yaml.v3's error text (unsound: a
// scrub catches only the shapes its author anticipated) or pre-scanning
// the YAML for explicit tags before decoding. Neither is worth it for a
// shape that requires the operator to write "!!null" in front of their own
// connection string.
//
// If a future yaml.v3 stops echoing here, this test fails -- which is the
// signal to delete it and the caveat in decodeYAMLString's comment.
func TestYAMLNullTagWithAValueIsOutOfThisTypesReach(t *testing.T) {
	type cfg struct {
		NatsURL NatsURL `yaml:"nats_url"`
	}
	var got cfg
	doc := "nats_url: !!null nats://user:" + sentinel + "@h:4222\n"
	err := yaml.Unmarshal([]byte(doc), &got)
	if err == nil {
		t.Fatalf("decoding %q returned nil; yaml.v3's behavior changed", doc)
	}
	if !strings.Contains(err.Error(), sentinel) {
		t.Fatalf("decoding %q no longer echoes the value (err = %q) -- yaml.v3 "+
			"has changed; delete this test and the caveat in decodeYAMLString's "+
			"doc comment", doc, err)
	}
}
