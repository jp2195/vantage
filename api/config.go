// Config: YAML-file + built-in defaults for vantage-api, mirroring
// sink/config.go and collector/config.go so all three daemons behave the
// same way when a field is left unset -- with one deliberate exception,
// documented on LoadConfig: this daemon refuses to start rather than
// defaulting its way into an unauthenticated one.
package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
	"github.com/jp2195/vantage/sink"
)

// defaultMaxUnscopedSince is Config.MaxUnscopedSince's built-in default. See
// that field's doc comment for where the number comes from.
const defaultMaxUnscopedSince = 24 * time.Hour

// defaultCollectorsTimeout is Config.CollectorsTimeout's built-in default.
// collectorstatus.go also falls back to it directly, for a *Server built by
// a test that sets cfg by literal rather than through LoadConfig: a zero
// time.Duration must mean "not configured," never "already expired," the
// same distinction Config.Collectors's doc comment draws for zero counts.
const defaultCollectorsTimeout = 2 * time.Second

// Config is vantage-api's full runtime configuration, loaded from a YAML
// file (LoadConfig) and defaulted where the file leaves a field at its zero
// value.
type Config struct {
	// ClickHouseDSN is a secret type, not a string, for the reason
	// sink.Config gives: it carries the operator's password, it is
	// formatted into startup errors, and printing it -- %s, %v, slog, JSON
	// -- yields the redacted rendering. Reaching the real value is an
	// explicit RevealSecret call.
	ClickHouseDSN secret.ClickHouseDSN `yaml:"clickhouse_dsn"`

	// ClickHousePasswordFile names a file holding the ClickHouse password,
	// with the same meaning as vantage-writer's key of that name (see
	// sink.ResolveClickHouseDSN).
	ClickHousePasswordFile string `yaml:"clickhouse_password_file"`

	// LogLevel (debug|info|warn|error) and LogFormat (text|json); see
	// package logging.
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`

	// Listen defaults to loopback, unlike MetricsListen. This is the same
	// call collector/config.go makes for its admin port and for the same
	// reason: the read API answers arbitrary questions about the whole
	// database, so the default has to fail closed. An operator who wants it
	// reachable off-box sets listen explicitly and takes on gating it.
	Listen string `yaml:"listen"`

	// MetricsListen serves /metrics only, which is why it may default to
	// all interfaces the way the other two daemons' does.
	MetricsListen string `yaml:"metrics_listen"`

	// DefaultPage is the page size a request that names none gets; MaxPage
	// is the ceiling a request that names one is clamped to. Both are row
	// counts handed to query as a LIMIT.
	DefaultPage int `yaml:"default_page"`
	MaxPage     int `yaml:"max_page"`

	// MaxUnscopedSince is the widest window an UNSCOPED /v1/events request
	// may ask for. A request past it is a 400 that names this limit; a
	// request naming no window at all gets historyDefaultSince (1h) and is
	// never affected by this value.
	//
	// It exists because the scoped and unscoped modes have different costs,
	// not different semantics. Scoped, a wide since is still a seek and
	// refusing it would be gratuitous -- which is why /v1/events does NOT
	// clamp a scoped request, a decision that has not changed.
	// Unscoped, the 2026-09-08 measurement (docs/measurements.md,
	// "Unscoped events") put a wide window at 237-289ms and 2.4-3.6 GB
	// resident on a two-million-row archive, growing with the archive,
	// reachable from one query string. That is a resource hazard rather than a slow answer.
	//
	// The default is the widest window that measurement found cheap (a
	// 24-hour bound: 174,262 rows, 14-20ms, 24-30MB resident with the argMax
	// dedup included), not a round number chosen for looking reasonable. An
	// operator with a bigger machine raises it deliberately rather than
	// discovering it.
	//
	// Both figures above are for ONE of the two queries eventsUnscoped
	// actually runs. rowsAndTotal (see its own doc comment) runs
	// query.FleetEvents and query.CountFleetEvents CONCURRENTLY, and the
	// count wraps the identical peerEventsSQL -- same GROUP BY, same argMax
	// dedup, same window -- so it pays the same resident memory, at the same
	// time, not the same wall-clock cost paid twice in sequence. Peak
	// resident memory for one unscoped request is therefore roughly DOUBLE
	// the measured per-query number: ~48-60MB at the 24-hour default,
	// ~4.8-7.2GB at the measured wide-window figure. An operator
	// sizing a raised limit from the measured numbers alone would size
	// for half the actual ceiling.
	MaxUnscopedSince time.Duration `yaml:"max_unscoped_since"`

	// Tokens is the list of bearer tokens that may call this API. It has no
	// default and an empty one is a startup error; see LoadConfig.
	Tokens []Token `yaml:"tokens"`

	// Auth selects how a caller proves who it is. It is a nested block
	// rather than a top-level key so that the OIDC settings that arrive
	// with the next slice have somewhere to live without moving anything.
	//
	// Omitting it entirely is the common case and means mode: token, which
	// is what every config written before this field existed already does.
	Auth AuthConfig `yaml:"auth"`

	// AllowedHosts is the host names this daemon answers to in auth.mode:
	// none, compared against each request's Host header with the port
	// stripped, case folded and any trailing dot removed. A request naming
	// any other host gets 421 Misdirected Request before it reaches the UI
	// or the API. The loopback literals -- 127.0.0.1, ::1 and localhost --
	// are always allowed and need not be listed.
	//
	// It exists because of DNS rebinding. With no token to present, the
	// only thing that tells this daemon's own UI apart from a web page on
	// another site that has re-pointed its own name at this listener's
	// address is the Host header, which still carries that site's name. A
	// daemon that answered any Host would hand such a page every route,
	// even one bound to loopback or reachable only over a VPN, because the
	// victim's browser is the thing inside the boundary.
	//
	// Entries are bare names or IP literals, without a port or scheme; an
	// IPv6 literal may be written with or without brackets. Every name a
	// browser uses to reach this daemon in none mode -- the VPN address,
	// the fronting proxy's public name -- has to be listed here.
	//
	// It is ignored in token mode: a rebinding page cannot present a
	// token, so there the Host header is left unchecked.
	AllowedHosts []string `yaml:"allowed_hosts"`

	// Collectors is how this daemon reaches each collector's /status. Empty is
	// a supported deployment, not a broken one: /v1/collectors still answers
	// from the archive alone, and each card says that its process facts are
	// unconfigured rather than showing them as zeros.
	//
	// The id here is NOT the source of truth. A collector states its own
	// collector_id and that is what the archive stores; this one exists for the
	// single case where the daemon cannot state anything, because it is down --
	// without it an outage renders as a card that silently disappeared. When
	// both exist and disagree, /v1/collectors reports the disagreement.
	//
	// Each URL must resolve to exactly one collector process. deploy/helm's
	// collector Service is headless (clusterIP: None) over a StatefulSet
	// precisely so that per-pod DNS exists to put here -- something like
	// http://vantage-collector-0.vantage-collector:9469 for pod 0. A bare
	// headless service name (http://vantage-collector:9469) resolves to ALL
	// pod IPs round-robin, so with more than one replica it would hit a
	// different pod on every poll and the card behind that entry would flip
	// identity between refreshes. That mistake is exactly what IDMismatch
	// below exists to catch cheaply -- two consecutive polls of the same
	// configured id would disagree with each other and eventually with the
	// id itself -- so a card stuck in IDMismatch is the first place to look
	// for a URL that names a Service instead of a pod.
	Collectors []CollectorEndpoint `yaml:"collectors"`

	// CollectorsTimeout bounds the whole fan-out, not each request. Defaults
	// to 2s: this is a screen someone is waiting on, and a collector that
	// cannot answer in two seconds is itself the finding.
	CollectorsTimeout time.Duration `yaml:"collectors_timeout"`

	// StaleAfter is how long a collector may go without a heartbeat before
	// its peers read "stale". Defaults to query.DefaultStaleAfter, the same
	// default `vantage query -dsn` reads with, so the two read paths agree
	// unless an operator changes one. It may not be below
	// query.MinStaleAfter, two heartbeats, nor above query.MaxStaleAfter,
	// one day. The Grafana "Peer status" panel cannot read this file and
	// always uses the default.
	StaleAfter time.Duration `yaml:"stale_after"`
}

// CollectorEndpoint is one entry in Config.Collectors: the id this daemon
// expects to find at URL, and the address of its /status.
type CollectorEndpoint struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

// AuthMode is how callers authenticate. It is a named string type so an
// unknown value is caught by validation rather than silently compared away
// somewhere downstream.
type AuthMode string

const (
	// AuthModeToken compares a bearer token against the configured list.
	// It is the default and the API's native mode.
	AuthModeToken AuthMode = "token"

	// AuthModeNone serves every route unauthenticated. It exists for
	// deployments behind an authenticating proxy or a VPN, and it exists
	// EXPLICITLY: this stack already carries one accidental unauthenticated
	// path (Grafana's anonymous access, which reaches ClickHouse with full
	// SQL), and it arrived as a convenience default nobody chose. A mode an
	// operator has to type, that warns on every startup, is a different
	// thing from one that is inherited.
	AuthModeNone AuthMode = "none"

	// AuthModeOIDC validates a JWT against an identity provider. The value
	// is defined now so the configuration shape does not change when the
	// implementation lands; LoadConfig rejects it until then.
	AuthModeOIDC AuthMode = "oidc"
)

// AuthConfig is the auth block. Mode is the only field today.
type AuthConfig struct {
	Mode AuthMode `yaml:"mode"`
}

// Token is one entry in the tokens list: a bearer token plus the name that
// stands in for it everywhere the token itself must not appear.
//
// The name exists because secret.APIToken has no printable form at all --
// deliberately, see its doc comment -- so "REDACTED" is all a log line
// could otherwise say about which caller made a request or which entry an
// error is about. Name is an ordinary string and is logged in full; it is
// operator-chosen, so do not put a credential in it.
type Token struct {
	Name  string          `yaml:"name"`
	Token secret.APIToken `yaml:"token"`
}

// LoadConfig reads the YAML file at path and fills in any field the file
// left unset, then validates what it ended up with.
//
// As in collector/config.go and sink/config.go there is no flag-based
// override layer: the only command-line input vantage-api takes is which
// config file to load (-config), so file contents versus built-in defaults
// is the only precedence to resolve, field by field.
//
// It differs from those two in one way, and the difference is the point of
// this function. They accept path == "" and return pure defaults, because a
// collector with no config file is a collector listening on its default
// port -- an operator's problem, not a security one. There is no equivalent
// default for tokens, and there is no auth_disabled flag to reach for
// either: in AuthModeToken -- the default, and what every config file
// written before auth.mode existed already gets -- a vantage-api that
// starts with an empty token list is an unauthenticated path to every
// route, peer and prefix the collector has ever stored. This stack already
// carries one of those -- Grafana's anonymous access, which reaches
// ClickHouse with full SQL -- and it arrived exactly this way, as a
// convenience default that was reasonable in isolation. So in token mode an
// empty list is still an error, and path == "" still reaches that same
// error rather than being special-cased. The only road to an
// unauthenticated daemon is auth.mode: none, a value an operator has to
// type -- there is no flag that flips this daemon open by accident -- and
// one that logs a warning on every startup (see NewServer's AuthModeNone
// branch) rather than arriving silently.
func LoadConfig(path string) (Config, error) {
	cfg := Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		// KnownFields(true) rather than yaml.Unmarshal: an unknown key is
		// otherwise silently ignored, so a typo like "token:" for "tokens:"
		// leaves the built-in default in place with no warning and the
		// daemon starts looking healthy while ignoring the operator's
		// intent. Here that particular typo is caught twice over -- the
		// empty-list check below would reject it too -- but "max_pages:"
		// would not be, and it would silently uncap the API's page size.
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, err
		}
	}
	dsn, err := sink.ResolveClickHouseDSN(cfg.ClickHouseDSN, cfg.ClickHousePasswordFile)
	if err != nil {
		return Config{}, err
	}
	cfg.ClickHouseDSN = dsn
	if cfg.LogLevel == "" {
		cfg.LogLevel = logging.LevelInfo
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = logging.FormatText
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:9473"
	}
	if cfg.MetricsListen == "" {
		cfg.MetricsListen = "0.0.0.0:9474"
	}
	// From query rather than as literals: these are the same two numbers
	// api/openapi.yaml types limit= with, and query is the package that
	// enforces them on every page it returns. A copy here would be a third
	// spelling of a bound whose whole purpose is that the three agree.
	if cfg.DefaultPage == 0 {
		cfg.DefaultPage = query.DefaultRIBPage
	}
	if cfg.MaxPage == 0 {
		cfg.MaxPage = query.MaxRIBPage
	}
	if cfg.MaxUnscopedSince == 0 {
		cfg.MaxUnscopedSince = defaultMaxUnscopedSince
	}
	if cfg.CollectorsTimeout == 0 {
		cfg.CollectorsTimeout = defaultCollectorsTimeout
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = query.DefaultStaleAfter
	}
	if cfg.Auth.Mode == "" {
		cfg.Auth.Mode = AuthModeToken
	}
	switch cfg.Auth.Mode {
	case AuthModeToken:
		// Falls through to the existing token-list validation below.
	case AuthModeNone:
		// Deliberately skips the token-list check, in validate() below: an
		// empty list is the expected shape here, not an error. The
		// page-size checks in validate() are unrelated to authentication
		// and still apply -- see the guard inside validate() itself.
	case AuthModeOIDC:
		return Config{}, fmt.Errorf("api: auth mode %q is not implemented yet; use %q or %q", AuthModeOIDC, AuthModeToken, AuthModeNone)
	default:
		return Config{}, fmt.Errorf("api: unknown auth mode %q; want one of %q, %q, %q", cfg.Auth.Mode, AuthModeToken, AuthModeNone, AuthModeOIDC)
	}
	for i, h := range cfg.AllowedHosts {
		n, err := normalizeAllowedHost(h)
		if err != nil {
			return cfg, fmt.Errorf("allowed_hosts[%d]: %w", i, err)
		}
		cfg.AllowedHosts[i] = n
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// minTokenBytes is the shortest bearer token LoadConfig accepts. Nothing in
// front of this daemon rate-limits a guess, so a token short enough to
// enumerate is no gate at all; 16 bytes is the floor, and a generated token
// (openssl rand -hex 32) is far above it.
const minTokenBytes = 16

// normalizeAllowedHost puts one allowed_hosts entry in the form hostOf
// reduces a request's Host to, so the two compare with plain equality. An
// entry with a port or a scheme is refused rather than stripped: the port
// is dropped from the Host before comparison, so such an entry could never
// match, and an operator who wrote one believes a name is allowed that is
// not.
func normalizeAllowedHost(h string) (string, error) {
	if h == "" {
		return "", errors.New("entry is empty")
	}
	if strings.ContainsAny(h, "/ ") {
		return "", fmt.Errorf("%q is not a host name; write the bare name, without a scheme or path", h)
	}
	if _, _, err := net.SplitHostPort(h); err == nil {
		return "", fmt.Errorf("%q carries a port; the port is stripped from the Host before it is compared, so write the name alone", h)
	}
	return hostOf(h), nil
}

// validate is the half of LoadConfig the other two daemons have no
// counterpart for. Its token-list messages name the offending entry by
// index and by name, and never by token: an error printed at startup is a
// log line like any other. The three page-size messages below have no
// entry to name and quote the offending numbers instead, which is the same
// rule -- print what identifies the mistake, never the credential.
func (c Config) validate() error {
	// The token-list checks below apply only in AuthModeToken. AuthModeNone
	// is the one mode where an empty list is the expected shape rather than
	// a half-finished config, and LoadConfig's switch has already rejected
	// AuthModeOIDC and any unknown mode before validate() is ever called --
	// so this is the mode a config reaching here can also be in.
	if c.Auth.Mode != AuthModeNone {
		if len(c.Tokens) == 0 {
			return errors.New("no tokens configured: vantage-api answers for the whole " +
				"database, so token mode needs at least one entry of the form " +
				"{name: <who>, token: <secret>} -- or set auth.mode: none if this " +
				"daemon should run unauthenticated on purpose")
		}
		seen := make(map[string]int, len(c.Tokens))
		for i, t := range c.Tokens {
			if t.Name == "" {
				return fmt.Errorf("tokens[%d]: name is required; it is what identifies "+
					"the caller in logs and errors, since the token itself appears in "+
					"neither", i)
			}
			if prev, ok := seen[t.Name]; ok {
				return fmt.Errorf("tokens[%d]: duplicate name %q, already used by "+
					"tokens[%d]; a name has to identify one caller for logging it to "+
					"mean anything", i, t.Name, prev)
			}
			seen[t.Name] = i
			// An entry with a name and no token is the shape a half-finished
			// edit leaves behind, and it must not be allowed to authenticate a
			// request that presents nothing.
			if t.Token.Empty() {
				return fmt.Errorf("tokens[%d] (%q): token is empty", i, t.Name)
			}
			// The length is the only property of the value read here; the
			// value itself goes nowhere.
			if n := len(t.Token.RevealSecret()); n < minTokenBytes {
				return fmt.Errorf("tokens[%d] (%q): token is %d bytes, shorter than the "+
					"%d-byte minimum; generate one with `openssl rand -hex 32`",
					i, t.Name, n, minTokenBytes)
			}
		}
	}
	// Duplicate token *values* are checked in newAuth (api/auth.go), not
	// here, and they are rejected there for the same reason a duplicate
	// name is rejected above. The check needs the revealed value, and this
	// package's secret types are worth exactly as much as the shortness of
	// the RevealSecret grep: newAuth's loop has to reveal every token
	// anyway to compare against it, so putting the check there keeps the
	// value comparison in the one place that holds every revealed token.
	// The length floor above reveals too, but reads only len() of what it
	// gets, and belongs here because it is a property of one entry that
	// LoadConfig's own tests can see. Both are startup errors either way --
	// newAuth returns an error the daemon cannot start past.
	if err := logging.Validate(c.LogLevel, c.LogFormat); err != nil {
		return err
	}
	if c.DefaultPage < 0 || c.MaxPage < 0 {
		return fmt.Errorf("default_page (%d) and max_page (%d) are row counts and "+
			"cannot be negative", c.DefaultPage, c.MaxPage)
	}
	if c.DefaultPage > c.MaxPage {
		return fmt.Errorf("default_page (%d) is above max_page (%d), so every "+
			"request that names no page size would be clamped below the default",
			c.DefaultPage, c.MaxPage)
	}
	// The ceiling, checked here and not per request, because the two fail in
	// different places. query.clampRIBLimit returns at most query.MaxRIBPage
	// rows whatever it is handed, and api/openapi.yaml types limit= with the
	// same maximum -- so a max_page above it makes this daemon accept a limit
	// its own contract calls a 400 and then answer with fewer rows than it
	// agreed to, which reads as a complete answer and is not. Clamping at
	// request time would leave the daemon running on a configuration it
	// cannot honor, surfacing only as a short page nobody traces back to the
	// config file. Refusing at startup puts it in front of the operator who
	// wrote it, once.
	if c.MaxPage > query.MaxRIBPage {
		return fmt.Errorf("max_page (%d) is above %d, the largest page the query "+
			"layer will return and the maximum api/openapi.yaml types limit= with; "+
			"this daemon would accept a limit its own contract refuses and then "+
			"answer with fewer rows than it agreed to",
			c.MaxPage, query.MaxRIBPage)
	}
	// Below historyDefaultSince, this daemon would refuse its own default
	// answer: p.since resolves an absent ?since= to that duration, so every
	// unscoped request naming no window would 400. The failure surfaces only
	// as a 400 nobody traces back to the config file, which is the same
	// reason the max_page ceiling above is a startup error rather than a
	// per-request clamp.
	if c.MaxUnscopedSince < historyDefaultSince {
		return fmt.Errorf("max_unscoped_since (%v) is below %v, the window an "+
			"unscoped /v1/events request gets when it names none; this daemon "+
			"would refuse its own default answer",
			c.MaxUnscopedSince, historyDefaultSince)
	}
	// The same floor query.WithStaleAfter enforces, checked here so the
	// operator who wrote the value hears about it at startup.
	if c.StaleAfter < query.MinStaleAfter {
		return fmt.Errorf("stale_after (%v) is below %v, two collector heartbeats; "+
			"a healthy collector would read stale between beats", c.StaleAfter, query.MinStaleAfter)
	}
	// And the ceiling, for the same reason and with the same number.
	if c.StaleAfter > query.MaxStaleAfter {
		return fmt.Errorf("stale_after (%v) is above %v; a collector never heard "+
			"from could read up", c.StaleAfter, query.MaxStaleAfter)
	}
	// Every entry needs both fields, the url has to parse and carry an
	// http/https scheme so a typo'd address fails at startup rather than as
	// an opaque dial error the first time collectorStatuses polls it, and
	// ids must be unique -- a duplicate would have two cards' process facts
	// land in collectorStatuses's result map under the same key, with
	// whichever poll finished last silently overwriting the other.
	seenCollectorIDs := make(map[string]int, len(c.Collectors))
	for i, ce := range c.Collectors {
		if ce.ID == "" {
			return fmt.Errorf("collectors[%d]: id is required", i)
		}
		if ce.URL == "" {
			return fmt.Errorf("collectors[%d] (%q): url is required", i, ce.ID)
		}
		u, err := url.Parse(ce.URL)
		if err != nil {
			return fmt.Errorf("collectors[%d] (%q): url %q does not parse: %w",
				i, ce.ID, ce.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("collectors[%d] (%q): url %q must have an http or "+
				"https scheme, got %q", i, ce.ID, ce.URL, u.Scheme)
		}
		if prev, ok := seenCollectorIDs[ce.ID]; ok {
			return fmt.Errorf("collectors[%d]: duplicate id %q, already used by "+
				"collectors[%d]; two cards would overwrite each other's process "+
				"facts in the result map", i, ce.ID, prev)
		}
		seenCollectorIDs[ce.ID] = i
	}
	return nil
}
