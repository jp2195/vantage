package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// tokenSentinel is embedded in every token these tests configure and is the
// one thing the leak assertions grep for. As in secret's own tests, every
// such assertion is an assertion of absence: it passes only when the
// credential is really gone, however the rest of the line is spelled.
const tokenSentinel = "TOKENSECRETSENTINEL"

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeAndLoad writes content to a temp YAML file and loads it, failing the
// test immediately if LoadConfig errors. Use it where the test's subject is
// the loaded Config, not the error.
func writeAndLoad(t *testing.T, content string) Config {
	t.Helper()
	cfg, err := loadFrom(t, content)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// loadFrom is writeAndLoad without the failure: the caller inspects the
// error itself, for the tests whose subject is LoadConfig rejecting
// something.
func loadFrom(t *testing.T, content string) (Config, error) {
	t.Helper()
	return LoadConfig(writeTempYAML(t, content))
}

// TestAuthModeDefaultsToToken pins the backward-compatibility promise: a
// config file written before auth.mode existed -- which is every config in
// the field, and the one the Helm chart renders -- must behave exactly as
// it did.
func TestAuthModeDefaultsToToken(t *testing.T) {
	cfg := writeAndLoad(t, `
clickhouse_dsn: "clickhouse://u:p@h:9000/vantage"
tokens:
  - name: lab
    token: abcdefghijklmnopqrstuvwxyz012345
`)
	if cfg.Auth.Mode != AuthModeToken {
		t.Errorf("Auth.Mode = %q, want %q", cfg.Auth.Mode, AuthModeToken)
	}
}

// TestAuthModeNoneAllowsZeroTokens is the whole point of the mode. Without
// it, `mode: none` would still trip the empty-token-list startup error and
// the mode would be unreachable.
func TestAuthModeNoneAllowsZeroTokens(t *testing.T) {
	cfg := writeAndLoad(t, `
clickhouse_dsn: "clickhouse://u:p@h:9000/vantage"
auth:
  mode: none
`)
	if cfg.Auth.Mode != AuthModeNone {
		t.Errorf("Auth.Mode = %q, want %q", cfg.Auth.Mode, AuthModeNone)
	}
}

// TestAuthModeNoneStillValidatesEverythingElse pins the shape of the
// none-mode branch, not just its effect on tokens.
//
// The obvious way to write the branch is
// `case AuthModeNone: return cfg, nil` -- an early return that skips
// validate() outright. The code instead falls through to validate()
// and puts the token-list requirement behind a mode guard INSIDE it, so
// that everything in validate() unrelated to authentication still runs.
// Rewriting it in that early-return form left the whole Go suite green:
// nothing anywhere asserted that a none-mode config is still checked for
// anything at all.
//
// max_page is the case with teeth. Above query.MaxRIBPage this daemon
// accepts a limit= its own contract calls a 400 and then answers with
// fewer rows than it agreed to, which reads as a complete answer -- see
// validate()'s own comment on that ceiling. Skipping validate() in none
// mode would let exactly that config start.
func TestAuthModeNoneStillValidatesEverythingElse(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{
			name: "max_page above the query layer's ceiling",
			yaml: "auth:\n  mode: none\nmax_page: 999999\n",
			want: "largest page the query layer will return",
		},
		{
			name: "negative page size",
			yaml: "auth:\n  mode: none\ndefault_page: -1\n",
			want: "cannot be negative",
		},
		{
			name: "default above max",
			yaml: "auth:\n  mode: none\ndefault_page: 5000\nmax_page: 10\n",
			want: "above max_page",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadFrom(t, c.yaml)
			if err == nil {
				t.Fatal("LoadConfig accepted the value in auth.mode: none -- " +
					"the mode waives the token list, nothing else")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
	// The counterweight, and the reason the guard lives inside validate()
	// rather than around it: a none-mode config with no tokens and legal
	// page sizes must still load. Without this, refusing everything would
	// satisfy the cases above.
	if _, err := loadFrom(t, "auth:\n  mode: none\ndefault_page: 10\nmax_page: 100\n"); err != nil {
		t.Errorf("LoadConfig rejected a legal none-mode config: %v", err)
	}
}

// TestAuthModeOIDCIsRejectedForNow keeps the seam honest. The enum value
// exists so the config shape does not change when OIDC lands, but a daemon
// that accepted it today would serve every route unauthenticated while
// looking configured.
func TestAuthModeOIDCIsRejectedForNow(t *testing.T) {
	_, err := loadFrom(t, `
clickhouse_dsn: "clickhouse://u:p@h:9000/vantage"
auth:
  mode: oidc
`)
	if err == nil {
		t.Fatal("LoadConfig accepted mode: oidc, which is not implemented")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error %q does not say the mode is not implemented", err)
	}
}

// TestUnknownAuthModeIsRejected covers the typo. "tokens" instead of
// "token" must not silently fall back to an open daemon.
//
// The config carries a valid token entry so the empty-token-list check
// cannot be what produces the error: without one, this test passed for the
// wrong reason -- deleting LoadConfig's default: case entirely still left
// an unrecognized mode falling through to validate(), which rejects the
// zero tokens below regardless of whether the mode itself was ever
// checked. That left the branch this test exists to pin -- the one whose
// own comment says a typo must not silently open the daemon -- completely
// unverified. Asserting on the message, not merely on err != nil, is what
// ties the failure back to the switch's default: case specifically.
func TestUnknownAuthModeIsRejected(t *testing.T) {
	_, err := loadFrom(t, `
clickhouse_dsn: "clickhouse://u:p@h:9000/vantage"
tokens:
  - name: lab
    token: abcdefghijklmnopqrstuvwxyz012345
auth:
  mode: tokens
`)
	if err == nil {
		t.Fatal("LoadConfig accepted an unknown auth mode")
	}
	if !strings.Contains(err.Error(), "unknown auth mode") {
		t.Errorf("error %q does not say the mode is unknown", err)
	}
}

func TestLoadConfigFromYAML(t *testing.T) {
	path := writeTempYAML(t, `
clickhouse_dsn: "clickhouse://u:p@ch.internal:9000/vantage"
listen: "0.0.0.0:8080"
metrics_listen: "127.0.0.1:9999"
default_page: 25
max_page: 50
tokens:
  - name: grafana
    token: "grafana-`+tokenSentinel+`"
  - name: cron
    token: "cron-`+tokenSentinel+`"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.ClickHouseDSN.RevealSecret(), "clickhouse://u:p@ch.internal:9000/vantage"; got != want {
		t.Errorf("ClickHouseDSN = %q, want %q", got, want)
	}
	if cfg.Listen != "0.0.0.0:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.MetricsListen != "127.0.0.1:9999" {
		t.Errorf("MetricsListen = %q", cfg.MetricsListen)
	}
	if cfg.DefaultPage != 25 {
		t.Errorf("DefaultPage = %d, want 25", cfg.DefaultPage)
	}
	if cfg.MaxPage != 50 {
		t.Errorf("MaxPage = %d, want 50", cfg.MaxPage)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("Tokens has %d entries, want 2", len(cfg.Tokens))
	}
	// The token values matter here as much as the names: secret.APIToken's
	// UnmarshalYAML is the only thing that puts them in the struct, and
	// without it every field would be silently empty.
	for i, want := range []Token{
		{Name: "grafana", Token: secret.NewAPIToken("grafana-" + tokenSentinel)},
		{Name: "cron", Token: secret.NewAPIToken("cron-" + tokenSentinel)},
	} {
		if cfg.Tokens[i].Name != want.Name {
			t.Errorf("Tokens[%d].Name = %q, want %q", i, cfg.Tokens[i].Name, want.Name)
		}
		if got := cfg.Tokens[i].Token.RevealSecret(); got != want.Token.RevealSecret() {
			t.Errorf("Tokens[%d].Token did not round-trip through YAML", i)
		}
	}
}

// TestLoadConfigPartialFileStillDefaults is LoadConfig's documented
// precedence: file value per-field where the file sets one, built-in
// default otherwise. The listen default is loopback deliberately -- see
// Config.Listen -- so this pins it rather than merely observing it.
func TestLoadConfigPartialFileStillDefaults(t *testing.T) {
	path := writeTempYAML(t, "tokens:\n  - {name: only-this-is-set, token: t-"+tokenSentinel+"}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.ClickHouseDSN.RevealSecret(), "clickhouse://vantage:vantage@127.0.0.1:9000/vantage"; got != want {
		t.Errorf("ClickHouseDSN = %q, want default %q", got, want)
	}
	if cfg.Listen != "127.0.0.1:9473" {
		t.Errorf("Listen = %q, want default 127.0.0.1:9473 (loopback, deliberately)", cfg.Listen)
	}
	if cfg.MetricsListen != "0.0.0.0:9474" {
		t.Errorf("MetricsListen = %q, want default 0.0.0.0:9474", cfg.MetricsListen)
	}
	if cfg.DefaultPage != 1000 {
		t.Errorf("DefaultPage = %d, want default 1000", cfg.DefaultPage)
	}
	if cfg.MaxPage != 10000 {
		t.Errorf("MaxPage = %d, want default 10000", cfg.MaxPage)
	}
}

// TestLoadConfigRejectsNoTokens pins the decision that an empty token list
// is a startup error rather than a warning. This stack already contains one
// unauthenticated path to arbitrary ClickHouse SQL that arrived exactly this
// way -- a convenient default that was reasonable in isolation.
func TestLoadConfigRejectsNoTokens(t *testing.T) {
	path := writeTempYAML(t, "listen: 127.0.0.1:9473\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted a config with no tokens; it must refuse to start")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should name the missing tokens; got %v", err)
	}
}

// TestLoadConfigRefusesEveryShapeOfNoTokens covers the other three ways an
// operator arrives at an empty list, because "no tokens: key" is the only
// one the test above exercises and each of these reaches the check by a
// different route: an explicit empty sequence, an explicitly null key, and
// no config file at all. The last is the one worth stating: the other two
// daemons treat path == "" as "use defaults", and there is no default here.
func TestLoadConfigRefusesEveryShapeOfNoTokens(t *testing.T) {
	for _, c := range []struct{ name, yaml string }{
		{"key absent", "listen: 127.0.0.1:9473\n"},
		{"explicit empty sequence", "tokens: []\n"},
		{"explicit null", "tokens:\n"},
		{"empty file", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadConfig(writeTempYAML(t, c.yaml)); err == nil {
				t.Fatal("LoadConfig accepted a config with no tokens")
			}
		})
	}
	if _, err := LoadConfig(""); err == nil {
		t.Fatal(`LoadConfig("") returned pure defaults; with no file there are no tokens, so it must refuse`)
	}
}

func TestLoadConfigRejectsAMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("LoadConfig accepted a path that does not exist")
	}
}

// TestLoadConfigRejectsAnUnknownKey pins the KnownFields(true) decoder. The
// key here is one whose silent acceptance would matter: "max_pages" leaves
// max_page at its default, so a deployment that meant to raise its ceiling
// would run at 10000 and look healthy doing it.
func TestLoadConfigRejectsAnUnknownKey(t *testing.T) {
	body := "tokens:\n  - {name: a, token: t-" + tokenSentinel + "}\n"
	if _, err := LoadConfig(writeTempYAML(t, body)); err != nil {
		t.Fatalf("test setup: the same config without the typo does not load: %v", err)
	}
	_, err := LoadConfig(writeTempYAML(t, body+"max_pages: 500\n"))
	if err == nil {
		t.Fatal("LoadConfig silently ignored the unknown key max_pages")
	}
	if !strings.Contains(err.Error(), "max_pages") {
		t.Errorf("error should name the unknown field; got %v", err)
	}
}

// TestLoadConfigRejectsMalformedTokenEntries covers the entries that are
// present but cannot authenticate anyone, or cannot be attributed to anyone
// once they do.
func TestLoadConfigRejectsMalformedTokenEntries(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{
			"no name",
			"tokens:\n  - {token: t-" + tokenSentinel + "}\n",
			"name is required",
		},
		{
			"empty name",
			"tokens:\n  - {name: \"\", token: t-" + tokenSentinel + "}\n",
			"name is required",
		},
		{
			"duplicate name",
			"tokens:\n  - {name: dup, token: a-" + tokenSentinel + "}\n  - {name: dup, token: b-" + tokenSentinel + "}\n",
			"duplicate name",
		},
		{
			"no token",
			"tokens:\n  - {name: grafana}\n",
			"token is empty",
		},
		{
			"empty token",
			"tokens:\n  - {name: grafana, token: \"\"}\n",
			"token is empty",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempYAML(t, c.yaml))
			if err == nil {
				t.Fatal("LoadConfig accepted the entry")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestLoadConfigRejectsImpossiblePageSizes covers the two page settings
// that are accepted by YAML and meaningless downstream: a negative row
// count, and a default above the ceiling that clamps it.
func TestLoadConfigRejectsImpossiblePageSizes(t *testing.T) {
	base := "tokens:\n  - {name: a, token: t-" + tokenSentinel + "}\n"
	for _, c := range []struct{ name, yaml, want string }{
		{"negative default_page", base + "default_page: -1\n", "cannot be negative"},
		{"negative max_page", base + "max_page: -1\n", "cannot be negative"},
		{"default above max", base + "default_page: 20000\nmax_page: 10\n", "above max_page"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempYAML(t, c.yaml))
			if err == nil {
				t.Fatal("LoadConfig accepted the value")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
	// The counterweight: a default equal to the ceiling is legal, and so is
	// one below it. Failing closed is trivial if everything fails.
	for _, ok := range []string{base + "default_page: 10\nmax_page: 10\n", base + "default_page: 1\nmax_page: 2\n"} {
		if _, err := LoadConfig(writeTempYAML(t, ok)); err != nil {
			t.Errorf("LoadConfig rejected a legal pair: %v", err)
		}
	}
}

// TestConfigNeverPrintsAToken is the property api.Config inherits from
// secret.APIToken, asserted on the value an operator's file actually
// produces rather than on a hand-built token: a whole Config through every
// rendering a daemon puts one through at startup.
func TestConfigNeverPrintsAToken(t *testing.T) {
	cfg, err := LoadConfig(writeTempYAML(t, `
clickhouse_dsn: "clickhouse://vantage:dsn-`+tokenSentinel+`@127.0.0.1:9000/vantage"
tokens:
  - {name: grafana, token: "grafana-`+tokenSentinel+`"}
  - {name: cron, token: "cron-`+tokenSentinel+`"}
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	check := func(what, got string) {
		t.Helper()
		if strings.Contains(got, tokenSentinel) {
			t.Errorf("%s rendered %q, which still contains a credential", what, got)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%p"} {
		check("Sprintf("+verb+")", fmt.Sprintf(verb, cfg))
		check("Sprintf("+verb+") on Tokens", fmt.Sprintf(verb, cfg.Tokens))
	}
	// %w on a non-error is the misuse that reaches fmt's reflection dump;
	// the format string is a variable because vet -- correctly -- rejects a
	// constant one, and this is precisely about the contributor who writes
	// it anyway.
	wrapVerb := "cfg: %w"
	check("Errorf(%w)", fmt.Errorf(wrapVerb, cfg).Error())

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	check("json.Marshal", string(b))

	y, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	check("yaml.Marshal", string(y))

	for _, h := range []struct {
		name string
		new  func(*bytes.Buffer) slog.Handler
	}{
		{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
		{"json", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
	} {
		var buf bytes.Buffer
		log := slog.New(h.new(&buf))
		log.Info("starting", "cfg", cfg, "tokens", cfg.Tokens, "token", cfg.Tokens[0].Token)
		check("slog "+h.name, buf.String())
	}

	// The name is the other half of the contract: it must survive, or a
	// redacted log line says nothing about which caller it is about.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", "tokens", cfg.Tokens)
	if !strings.Contains(buf.String(), "grafana") {
		t.Errorf("slog rendered %q, which does not name the token", buf.String())
	}
}

// TestLoadConfigErrorsNeverPrintAToken covers the other rendering path an
// operator sees on every start: the error itself. A config file's token is
// operator-written text, and several of these errors are built from the
// entry that carries it.
//
// One shape is deliberately absent, because it is out of this package's
// reach rather than closed: `token: !!null <value>`, where yaml.v3 resolves
// the null before consulting an Unmarshaler and echoes the scalar in its
// own error. secret's TestYAMLNullTagWithAValueIsOutOfThisTypesReach pins
// it there; a plain string field behaves identically.
func TestLoadConfigErrorsNeverPrintAToken(t *testing.T) {
	for _, c := range []struct{ name, yaml string }{
		{"duplicate name", "tokens:\n  - {name: dup, token: a-" + tokenSentinel + "}\n  - {name: dup, token: b-" + tokenSentinel + "}\n"},
		{"unnamed entry", "tokens:\n  - {token: t-" + tokenSentinel + "}\n"},
		{"unknown key beside a good token", "bogus: 1\ntokens:\n  - {name: a, token: t-" + tokenSentinel + "}\n"},
		{"bad page size", "default_page: -1\ntokens:\n  - {name: a, token: t-" + tokenSentinel + "}\n"},
		{"token tagged as an int", "tokens:\n  - {name: a, token: !!int t-" + tokenSentinel + "}\n"},
		{"token given as a mapping", "tokens:\n  - {name: a, token: {v: t-" + tokenSentinel + "}}\n"},
		{"tokens given as a scalar", "tokens: t-" + tokenSentinel + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempYAML(t, c.yaml))
			if err == nil {
				t.Fatal("LoadConfig accepted the config; this table is all error paths")
			}
			if strings.Contains(err.Error(), tokenSentinel) {
				t.Errorf("error %q contains the token", err)
			}
		})
	}
}

// TestMaxPageCannotExceedTheCeilingQueryEnforces pins a defect, made a
// test.
//
// Three places carry this API's page ceiling: api/openapi.yaml types limit=
// as `maximum: 10000`, query.clampRIBLimit refuses to return more than
// query.MaxRIBPage rows whatever it is handed, and an operator sets
// max_page here. Nothing tied the third to the other two. An operator who
// set max_page: 50000 got a daemon that accepted limit=50000 -- a value the
// contract types as a 400 -- and then returned 10000 rows, which looks like
// a complete answer to the question asked and is not.
//
// It is a startup error rather than a runtime clamp because those fail in
// different places. Clamping at request time means the daemon runs happily
// with a configuration it cannot honor, and the disagreement surfaces only
// as a short page nobody attributes to the config file. Refusing at startup
// puts it in front of the operator who wrote it, once.
func TestMaxPageCannotExceedTheCeilingQueryEnforces(t *testing.T) {
	above := query.MaxRIBPage + 1
	path := writeTempYAML(t, fmt.Sprintf(
		"max_page: %d\ntokens:\n  - name: t\n    token: 0123456789abcdef\n", above))
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("max_page: %d was accepted. query.clampRIBLimit will return at "+
			"most %d rows regardless, so the daemon would advertise a page size "+
			"it cannot serve and silently answer with a smaller one",
			above, query.MaxRIBPage)
	}
	if !strings.Contains(err.Error(), "max_page") {
		t.Errorf("the refusal does not name max_page, so an operator cannot tell "+
			"which setting to change: %v", err)
	}
}

// TestPageDefaultsComeFromQueryNotFromALiteral pins the defaults to the
// package that enforces them. They were spelled 1000 and 10000 here, which
// is how the ceiling above came to be unchecked in the first place: a
// number copied is a number that drifts.
func TestPageDefaultsComeFromQueryNotFromALiteral(t *testing.T) {
	cfg, err := LoadConfig(writeTempYAML(t, "tokens:\n  - name: t\n    token: 0123456789abcdef\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPage != query.DefaultRIBPage {
		t.Errorf("DefaultPage = %d, want query.DefaultRIBPage (%d)",
			cfg.DefaultPage, query.DefaultRIBPage)
	}
	if cfg.MaxPage != query.MaxRIBPage {
		t.Errorf("MaxPage = %d, want query.MaxRIBPage (%d)",
			cfg.MaxPage, query.MaxRIBPage)
	}
}

// TestLoadConfigDefaultsMaxUnscopedSince pins the default to the widest
// window actually measured as cheap (docs/measurements.md, "Unscoped
// events"), so an operator who sets nothing gets the
// measured bound rather than an invented one.
func TestLoadConfigDefaultsMaxUnscopedSince(t *testing.T) {
	path := writeTempYAML(t, "tokens:\n  - {name: t, token: 0123456789abcdef}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxUnscopedSince != 24*time.Hour {
		t.Errorf("MaxUnscopedSince = %v, want 24h", cfg.MaxUnscopedSince)
	}
}

// TestLoadConfigReadsMaxUnscopedSinceFromTheFile proves the YAML key is
// wired, not merely declared. KnownFields(true) makes a typo an error, but a
// field whose tag never matched anything would default silently.
func TestLoadConfigReadsMaxUnscopedSinceFromTheFile(t *testing.T) {
	path := writeTempYAML(t, "max_unscoped_since: 6h\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxUnscopedSince != 6*time.Hour {
		t.Errorf("MaxUnscopedSince = %v, want 6h", cfg.MaxUnscopedSince)
	}
}

// TestLoadConfigRefusesAMaxUnscopedSinceBelowTheDefaultWindow is the check
// that keeps the daemon from starting in a state where its OWN default
// answer is refused. p.since resolves an absent ?since= to historyDefaultSince
// (1h); a maximum below that would 400 every unscoped request that named no
// window at all -- a configuration whose only symptom is a 400 nobody traces
// back to the config file. Refusing at startup puts it in front of the
// operator who wrote it, once. Same argument as the max_page ceiling above.
func TestLoadConfigRefusesAMaxUnscopedSinceBelowTheDefaultWindow(t *testing.T) {
	path := writeTempYAML(t, "max_unscoped_since: 30m\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted max_unscoped_since below the 1h default window")
	}
	if !strings.Contains(err.Error(), "max_unscoped_since") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// TestLoadConfigAcceptsMaxUnscopedSinceExactlyAtTheDefaultWindow pins the
// OTHER side of the boundary the test above checks: c.MaxUnscopedSince <
// historyDefaultSince refuses, so historyDefaultSince itself (1h), the
// smallest legal value, must load. events_test.go's own
// TestEventsUnscopedDefaultWindowExactlyAtTheConfiguredMaximumSucceeds
// reasons from exactly this premise -- "a value LoadConfig permits (it
// refuses only strictly BELOW historyDefaultSince, never equal to it)" --
// stated in a comment and, until this test, defended by nothing: a stray <=
// here would make max_unscoped_since: 1h a startup refusal while that
// events_test.go comment kept claiming otherwise, and the whole api suite
// would stay green because nothing else loads a config at exactly this
// value.
func TestLoadConfigAcceptsMaxUnscopedSinceExactlyAtTheDefaultWindow(t *testing.T) {
	path := writeTempYAML(t, "max_unscoped_since: 1h\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig refused max_unscoped_since at exactly the 1h default "+
			"window, which the boundary check's own comment says is inclusive: %v", err)
	}
	if cfg.MaxUnscopedSince != time.Hour {
		t.Errorf("MaxUnscopedSince = %v, want 1h", cfg.MaxUnscopedSince)
	}
}

// TestLoadConfigRefusesANegativeMaxUnscopedSince: a negative window reaches
// FORWARD from now and would refuse every request including the default,
// the same failure p.since already refuses a negative duration for.
func TestLoadConfigRefusesANegativeMaxUnscopedSince(t *testing.T) {
	path := writeTempYAML(t, "max_unscoped_since: -1h\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted a negative max_unscoped_since")
	}
	if !strings.Contains(err.Error(), "max_unscoped_since") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// TestLoadConfigRejectsMalformedCollectorEntries covers the entries that
// cannot be reached, or cannot be attributed to one collector once they
// are. The counterweight at the end -- two well-formed entries -- is the
// same shape TestLoadConfigRejectsImpossiblePageSizes uses: failing closed
// is trivial if everything fails, so a passing case belongs beside the
// failing ones.
func TestLoadConfigRejectsMalformedCollectorEntries(t *testing.T) {
	base := "tokens:\n  - {name: a, token: t-" + tokenSentinel + "}\n"
	for _, c := range []struct{ name, yaml, want string }{
		{
			"no id",
			base + "collectors:\n  - {url: http://c1:9469}\n",
			"id is required",
		},
		{
			"no url",
			base + "collectors:\n  - {id: dev-c1}\n",
			"url is required",
		},
		{
			"malformed url",
			base + "collectors:\n  - {id: dev-c1, url: \"http://[::1\"}\n",
			"does not parse",
		},
		{
			"wrong scheme",
			base + "collectors:\n  - {id: dev-c1, url: ftp://c1:9469}\n",
			"http or",
		},
		{
			"duplicate id",
			base + "collectors:\n  - {id: dup, url: http://c1:9469}\n  - {id: dup, url: http://c2:9469}\n",
			"duplicate id",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempYAML(t, c.yaml))
			if err == nil {
				t.Fatal("LoadConfig accepted the entry")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
	// The counterweight: two distinct, well-formed entries -- one http, one
	// https -- both load.
	ok := base + "collectors:\n  - {id: dev-c1, url: http://c1:9469}\n  - {id: dev-c2, url: https://c2:9469}\n"
	cfg, err := LoadConfig(writeTempYAML(t, ok))
	if err != nil {
		t.Fatalf("LoadConfig rejected two legal entries: %v", err)
	}
	if len(cfg.Collectors) != 2 {
		t.Fatalf("Collectors = %+v, want 2 entries", cfg.Collectors)
	}
	if cfg.Collectors[0] != (CollectorEndpoint{ID: "dev-c1", URL: "http://c1:9469"}) {
		t.Errorf("Collectors[0] = %+v, want {dev-c1 http://c1:9469}", cfg.Collectors[0])
	}
	if cfg.Collectors[1] != (CollectorEndpoint{ID: "dev-c2", URL: "https://c2:9469"}) {
		t.Errorf("Collectors[1] = %+v, want {dev-c2 https://c2:9469}", cfg.Collectors[1])
	}
}

// TestLoadConfigDefaultsCollectorsTimeout pins the 2s default
// CollectorsTimeout's own doc comment gives: this is a screen someone is
// waiting on, and a collector that cannot answer in two seconds is itself
// the finding.
func TestLoadConfigDefaultsCollectorsTimeout(t *testing.T) {
	path := writeTempYAML(t, "tokens:\n  - {name: t, token: 0123456789abcdef}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CollectorsTimeout != 2*time.Second {
		t.Errorf("CollectorsTimeout = %v, want 2s", cfg.CollectorsTimeout)
	}
}

// TestLoadConfigReadsCollectorsTimeoutFromTheFile proves the YAML key is
// wired, not merely declared -- the same concern
// TestLoadConfigReadsMaxUnscopedSinceFromTheFile checks for that field.
func TestLoadConfigReadsCollectorsTimeoutFromTheFile(t *testing.T) {
	path := writeTempYAML(t, "collectors_timeout: 500ms\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CollectorsTimeout != 500*time.Millisecond {
		t.Errorf("CollectorsTimeout = %v, want 500ms", cfg.CollectorsTimeout)
	}
}

// TestLoadConfigRefusesAShortToken is the length floor. A bearer token is
// the only thing between the network and every route in token mode, and
// nothing else in the path rate-limits a guess, so a short one is a
// credential an attacker can enumerate. 16 bytes is the floor; the boundary
// is tested from both sides so that neither an off-by-one nor a removed
// check passes.
func TestLoadConfigRefusesAShortToken(t *testing.T) {
	short := "short-" + tokenSentinel[:9] // 15 bytes
	if len(short) != 15 {
		t.Fatalf("fixture is %d bytes, want 15", len(short))
	}
	_, err := loadFrom(t, "tokens:\n  - {name: cron, token: "+short+"}\n")
	if err == nil {
		t.Fatal("LoadConfig accepted a 15-byte token")
	}
	if !strings.Contains(err.Error(), "16") || !strings.Contains(err.Error(), "cron") {
		t.Errorf("error %q should name the entry and the 16-byte floor", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Errorf("error %q echoes the token", err)
	}
	if _, err := loadFrom(t, "tokens:\n  - {name: cron, token: "+short+"x}\n"); err != nil {
		t.Errorf("LoadConfig refused a 16-byte token: %v", err)
	}
	// The Makefile's dev-api target writes this one; it must keep loading.
	if _, err := loadFrom(t, "tokens:\n  - {name: dev, token: dev-token-not-a-secret}\n"); err != nil {
		t.Errorf("LoadConfig refused the dev-api token: %v", err)
	}
}

// TestLoadConfigReadsAllowedHosts pins the normalization: allowed_hosts is
// compared against a request's Host with the port stripped, lowercased and
// without a trailing dot, so the entries have to be stored the same way or
// an operator's "Vantage.Example." would never match anything.
func TestLoadConfigReadsAllowedHosts(t *testing.T) {
	cfg := writeAndLoad(t, "auth: {mode: none}\nallowed_hosts: [Vantage.Example., \"[fd00::1]\", 10.0.0.5]\n")
	want := []string{"vantage.example", "fd00::1", "10.0.0.5"}
	if strings.Join(cfg.AllowedHosts, ",") != strings.Join(want, ",") {
		t.Errorf("AllowedHosts = %q, want %q", cfg.AllowedHosts, want)
	}
}

// TestLoadConfigRejectsMalformedAllowedHosts: an entry carrying a port can
// never match, because the port is stripped from the Host before the
// comparison, and an empty one is a half-finished edit. Both would leave an
// operator believing a name is allowed when it is not.
func TestLoadConfigRejectsMalformedAllowedHosts(t *testing.T) {
	for _, body := range []string{
		"auth: {mode: none}\nallowed_hosts: [\"vantage.example:9473\"]\n",
		"auth: {mode: none}\nallowed_hosts: [\"\"]\n",
		"auth: {mode: none}\nallowed_hosts: [\"http://vantage.example\"]\n",
	} {
		if _, err := loadFrom(t, body); err == nil {
			t.Errorf("LoadConfig accepted %q", body)
		}
	}
}

const minimalTokens = "tokens:\n  - name: t\n    token: 0123456789abcdef0123\n"

func TestLoadConfigLogKeys(t *testing.T) {
	cfg := writeAndLoad(t, minimalTokens+"log_level: warn\nlog_format: json\n")
	if cfg.LogLevel != "warn" || cfg.LogFormat != "json" {
		t.Errorf("LogLevel, LogFormat = %q, %q", cfg.LogLevel, cfg.LogFormat)
	}
	cfg = writeAndLoad(t, minimalTokens)
	if cfg.LogLevel != "info" || cfg.LogFormat != "text" {
		t.Errorf("defaults = %q, %q, want info, text", cfg.LogLevel, cfg.LogFormat)
	}
	for _, bad := range []string{"log_level: trace\n", "log_format: pretty\n"} {
		if _, err := loadFrom(t, minimalTokens+bad); err == nil {
			t.Errorf("LoadConfig accepted %q", bad)
		}
	}
}

// TestLoadConfigPasswordFile covers the api's use of the key; its full
// behavior is sink.ResolveClickHouseDSN's and is tested there.
func TestLoadConfigPasswordFile(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeAndLoad(t, minimalTokens+
		"clickhouse_dsn: \"clickhouse://reader@ch:9000/vantage\"\nclickhouse_password_file: "+pw+"\n")
	if got := cfg.ClickHouseDSN.RevealSecret(); got != "clickhouse://reader:fromfile@ch:9000/vantage" {
		t.Errorf("DSN = %q", got)
	}
	_, err := loadFrom(t, minimalTokens+
		"clickhouse_dsn: \"clickhouse://reader:inline@ch:9000/vantage\"\nclickhouse_password_file: "+pw+"\n")
	if err == nil || !strings.Contains(err.Error(), "clickhouse_password_file") {
		t.Errorf("a DSN password alongside the file = %v, want a refusal naming the key", err)
	}
}
