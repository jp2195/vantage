// Config: YAML-file + built-in defaults for vantage-collector.
package collector

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/natstls"
	"github.com/jp2195/vantage/quirk"
	"github.com/jp2195/vantage/secret"
)

// RouterOverride is a per-router quirk override, keyed in
// Config.Routers by the router's IP address as a string (e.g. "10.0.0.1").
// Both slices name quirk.ID values verbatim (e.g. "QK_TS_ZERO"); the string
// form -> quirk.ID conversion happens where the override is consumed
// (server.go's quirkIDs), not here, so this type stays free of any
// dependency on the quirk package.
//
// Vendor and OS are the authoritative source of a router's identity, ahead of
// anything its BMP Initiation banner says. A banner frequently is not an
// identity: XRd's entire sysDescr is "26.1.1" -- a bare version with no vendor
// token anywhere in it -- and no pattern can match that without matching
// everything. The operator can see the device. Set these and quirk matching
// works for a router whose banner could never identify it; leave them unset
// and the banner anchors still apply, so a zero-config deployment is not
// worse off than before.
//
// The banner is still read for the VERSION, which config deliberately does
// not carry: a version in a config file goes stale at the next upgrade and
// nothing notices, whereas the router reports its own on every session.
type RouterOverride struct {
	ForceQuirks   []string `yaml:"force_quirks"`
	DisableQuirks []string `yaml:"disable_quirks"`
	Vendor        string   `yaml:"vendor"`
	OS            string   `yaml:"os"`
}

// StreamsConfig configures the five JetStream streams EnsureStreams
// provisions (natsutil.StreamOpts).
//
// LSReplicas is its own field, not folded into Replicas, for the same reason
// natsutil.StreamOpts keeps it separate (see that type's doc comment):
// natsutil's own default for a zero LSReplicas is 3, which a single, non-
// clustered NATS server -- the common dev/laptop and even many production
// deployments' starting point -- rejects outright when EnsureStreams tries to
// create the LS stream. LoadConfig's defaulting below therefore fills an
// unset LSReplicas from Replicas (falling back to 1), so a single-node dev
// install provisions successfully while a cluster configured with
// `replicas: 3` gets LS at 3 rather than being silently downgraded on the
// next collector start. Set "ls_replicas" explicitly to diverge from
// Replicas in either direction.
type StreamsConfig struct {
	Partitions     int   `yaml:"partitions"`
	Replicas       int   `yaml:"replicas"`
	LSReplicas     int   `yaml:"ls_replicas"`
	RoutesMaxBytes int64 `yaml:"routes_max_bytes"`
	RawMaxBytes    int64 `yaml:"raw_max_bytes"`
}

// ProxyProtocol's two accepted values. There is deliberately no third mode
// that parses a header when one happens to be present and falls back to the
// socket otherwise: that would let any sender able to reach the BMP port
// assert any router's identity by writing 28 bytes, which is far cheaper than
// spoofing a TCP source address.
const (
	ProxyProtocolOff      = "off"
	ProxyProtocolRequired = "required"
)

// Config is vantage-collector's full runtime configuration, loaded from an
// optional YAML file (LoadConfig) and defaulted where the file (or its
// absence) leaves a field at its zero value.
type Config struct {
	Listen string `yaml:"listen"`
	// ProxyProtocol is "off" (the default) or "required". When required, every
	// connection must open with a PROXY protocol header and its source address
	// becomes the router's identity; a connection without one is closed.
	//
	// A string rather than a bool because "required" says out loud what "true"
	// only implies, and because enabling this is a hard cutover: it breaks
	// every directly connected router at once, which is intended -- it fails
	// loudly at rollout instead of mis-attributing routes quietly afterward.
	ProxyProtocol string `yaml:"proxy_protocol"`
	// AllowedSources lists the CIDRs a BMP sender may connect from. The
	// address checked is the router's: the TCP peer with proxy_protocol off,
	// the PROXY header's source with it required. A connection from outside
	// is closed before any of its BMP is parsed. Empty accepts any source,
	// which the daemon warns about once at startup: BMP is unauthenticated,
	// so a reachable port with no allowlist lets anyone write routes into the
	// archive under whatever router address they connect from.
	AllowedSources []string `yaml:"allowed_sources"`
	// TrustedProxies lists the CIDRs a PROXY header may come from. With
	// proxy_protocol required, the header is the router's identity, so
	// anything that can reach the port and is not the proxy could otherwise
	// assert any router's identity in 28 bytes. Empty trusts any peer, which
	// is only safe when the network already guarantees that the proxy is the
	// only thing that can reach the port. Setting it with proxy_protocol off
	// is a config error: there is no proxy for it to check.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// MaxConnections caps concurrent BMP connections; connections past it are
	// closed on accept. Each connection holds a goroutine, an fd and up to
	// bmp.MaxMsgLen of buffer, so without a cap the number of connections a
	// sender can open is the collector's memory ceiling.
	MaxConnections int `yaml:"max_connections"`
	// NatsURL is a secret type, not a string: it carries an operator's
	// password (or auth token) and is formatted into this daemon's startup
	// errors. See secret and sink.Config's identical
	// field.
	NatsURL secret.NatsURL `yaml:"nats_url"`
	// NatsTLS is the TLS material this daemon presents to NATS. Absent
	// means plaintext, which is what every install before this field
	// existed did and what docker-compose.dev.yml still does.
	//
	// It is natstls.Config rather than a local struct so this daemon and
	// vantage-writer cannot drift: one type, one Validate, one place that
	// turns paths into nats.Option values.
	NatsTLS     natstls.Config `yaml:"nats_tls"`
	CollectorID string         `yaml:"collector_id"`
	// CollectorIDDerived records that CollectorID came from the hostname
	// rather than from the file, so the daemon can say so at startup. It is
	// not a YAML field: setting it would be claiming something about where a
	// value came from, which only LoadConfig knows.
	//
	// It exists because collector_id is half of session identity -- the query
	// layer scopes current state to max(session_id) per (collector_id,
	// router_ip) -- so an ID that changes when the process restarts strands
	// the previous session's view in the archive permanently, under a pairing
	// no future session will ever carry. The hostname is a stable identity on
	// a VM, a laptop and a StatefulSet pod, and is NOT one under a Kubernetes
	// Deployment or anything else that names a replacement process
	// differently from the process it replaced. Nothing here can tell those
	// apart, so the daemon reports what it did and lets the operator judge.
	CollectorIDDerived bool                      `yaml:"-"`
	MetricsListen      string                    `yaml:"metrics_listen"`
	AdminListen        string                    `yaml:"admin_listen"`
	Streams            StreamsConfig             `yaml:"streams"`
	Routers            map[string]RouterOverride `yaml:"routers"` // keyed by router IP
	// LogLevel (debug|info|warn|error) and LogFormat (text|json); empty
	// means info and text. See package logging.
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`
}

// LoadConfig reads the YAML file at path (path == "" skips reading and
// returns pure defaults) and fills in any field the file left unset. There is
// no separate flag-based override layer: the only command-line input
// vantage-collector takes is which config file to load (-config), so file
// contents versus built-in defaults is the only precedence this function
// needs to resolve, field by field, taking the file's value whenever it set
// one and the default otherwise.
func LoadConfig(path string) (Config, error) {
	cfg := Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		// KnownFields(true) rather than yaml.Unmarshal: an unknown key is
		// otherwise silently ignored, so a typo like "nats_urls:" or
		// "collector-id:" leaves the built-in default in place with no
		// warning and the daemon starts looking healthy while ignoring the
		// operator's intent.
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, err
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = ":11019"
	}
	if cfg.ProxyProtocol == "" {
		cfg.ProxyProtocol = ProxyProtocolOff
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = defaultMaxConnections
	}
	if cfg.NatsURL.Empty() {
		cfg.NatsURL = secret.NewNatsURL("nats://127.0.0.1:4222")
	}
	if cfg.MetricsListen == "" {
		cfg.MetricsListen = ":9469"
	}
	if cfg.AdminListen == "" {
		// Loopback-only, unlike MetricsListen above. The admin API arms mirror
		// mode, which republishes a router's BMP traffic into the shared raw
		// stream, and it has no authentication, no cap on how many mirrors may
		// be armed, and no ceiling on max_bytes -- so anyone who reaches this
		// port can flood the stream that production capture depends on.
		// Binding to all interfaces was considered and rejected;
		// loopback-only keeps the default closed and matches what
		// admin.go's own package comment prescribes. An operator who wants
		// it reachable off-box sets admin_listen explicitly and takes on
		// gating it.
		cfg.AdminListen = "127.0.0.1:9470"
	}
	if cfg.CollectorID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "vantage-collector"
		}
		cfg.CollectorID = host
		cfg.CollectorIDDerived = true
	}
	if cfg.Streams.LSReplicas == 0 {
		// natsutil's own zero-value default for LSReplicas is 3, which a
		// single-node NATS rejects outright, so an unset value cannot simply
		// be passed through. But defaulting it to a flat 1 is worse than it
		// looks: EnsureStreams uses CreateOrUpdateStream and runs on every
		// collector start, so deploying against a cluster where LS was
		// provisioned R3 would silently *rewrite it down* to R1 -- inverting
		// the design intent that LS is the more replicated
		// stream because topology data is high-value. Following Replicas
		// instead means a single-node dev install gets 1 and a 3-node
		// production cluster gets 3, which is what an operator setting
		// `replicas: 3` and saying nothing about LS plainly intends.
		if cfg.Streams.Replicas > 0 {
			cfg.Streams.LSReplicas = cfg.Streams.Replicas
		} else {
			cfg.Streams.LSReplicas = 1
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects a config that would start a daemon which looks healthy and
// does the wrong thing. Router overrides are the sharp edge: they are matched
// by exact string equality against netip.Addr.String() at connection time, so
// a key written as "2001:DB8::1", "::ffff:10.0.0.1", or "10.0.0.1/32" never
// matches any router and the override is silently inert -- invisible in
// production, and precisely the case someone editing this file is trying to
// affect. Keys are therefore parsed and re-keyed into canonical form here,
// and unknown quirk IDs are rejected rather than converted and ignored.
func (c *Config) validate() error {
	var errs []error
	if len(c.Routers) > 0 {
		canon := make(map[string]RouterOverride, len(c.Routers))
		for k, v := range c.Routers {
			addr, err := netip.ParseAddr(k)
			if err != nil {
				errs = append(errs, fmt.Errorf("routers: %q is not an IP address: %w", k, err))
				continue
			}
			key := addr.String()
			if _, dup := canon[key]; dup {
				errs = append(errs, fmt.Errorf("routers: %q duplicates another key that canonicalizes to %s", k, key))
				continue
			}
			for _, id := range append(append([]string{}, v.ForceQuirks...), v.DisableQuirks...) {
				if !knownQuirk(id) {
					errs = append(errs, fmt.Errorf("routers %s: unknown quirk %q", key, id))
				}
			}
			// quirk.Entry.AppliesTo and quirk.versionPatterns both key on
			// lowercase vendor/OS tokens, so an operator's "Cisco" must be
			// folded here. Accepting the capitalization and then matching
			// nothing would be a config that looks applied and silently is
			// not.
			v.Vendor = strings.ToLower(strings.TrimSpace(v.Vendor))
			v.OS = strings.ToLower(strings.TrimSpace(v.OS))
			// An OS with no vendor cannot match anything: both lookups are
			// keyed on the pair. Rejecting it turns a one-second typo fix
			// into exactly that, instead of an inert config nobody can
			// explain.
			if v.OS != "" && v.Vendor == "" {
				errs = append(errs, fmt.Errorf("routers %s: os %q set without a vendor", key, v.OS))
			}
			canon[key] = v
		}
		c.Routers = canon
	}
	// "" is accepted because validate is a method on Config, not only a step
	// inside LoadConfig: tests and any future caller construct a Config
	// literal and never pass through the defaulting above. An unset value
	// means off, which is what LoadConfig fills in.
	switch c.ProxyProtocol {
	case "", ProxyProtocolOff, ProxyProtocolRequired:
	default:
		errs = append(errs, fmt.Errorf("proxy_protocol: %q is neither %q nor %q",
			c.ProxyProtocol, ProxyProtocolOff, ProxyProtocolRequired))
	}
	if _, err := parsePrefixes(c.AllowedSources); err != nil {
		errs = append(errs, fmt.Errorf("allowed_sources: %w", err))
	}
	if _, err := parsePrefixes(c.TrustedProxies); err != nil {
		errs = append(errs, fmt.Errorf("trusted_proxies: %w", err))
	}
	if len(c.TrustedProxies) > 0 && c.ProxyProtocol != ProxyProtocolRequired {
		errs = append(errs, fmt.Errorf("trusted_proxies is set but proxy_protocol is not %q: "+
			"with no PROXY header there is no proxy to trust", ProxyProtocolRequired))
	}
	if c.MaxConnections < 0 {
		errs = append(errs, fmt.Errorf("max_connections: %d is negative", c.MaxConnections))
	}
	if err := c.NatsTLS.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("nats_tls: %w", err))
	}
	if err := logging.Validate(c.LogLevel, c.LogFormat); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// defaultMaxConnections is max_connections when unset: well above any single
// collector's router count, and low enough that the connections a sender can
// hold open cannot exhaust the process's fds or memory.
const defaultMaxConnections = 1024

// parsePrefixes parses a list of CIDRs, returning each masked to its network.
// A bare address is rejected rather than read as a /32 or /128: the key is
// documented as a CIDR list, and guessing the width of an entry is how an
// allowlist ends up wider than its author meant. An IPv4-mapped IPv6 prefix
// is rejected too, because the address it is checked against is always
// unmapped first, so such an entry could never match anything.
func parsePrefixes(ss []string) ([]netip.Prefix, error) {
	var errs []error
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			errs = append(errs, fmt.Errorf("%q is not a CIDR: %w", s, err))
			continue
		}
		if p.Addr().Is4In6() {
			errs = append(errs, fmt.Errorf("%q is an IPv4-mapped prefix; write the IPv4 CIDR instead", s))
			continue
		}
		out = append(out, p.Masked())
	}
	return out, errors.Join(errs...)
}

// knownQuirk reports whether id names a quirk in the registry. An unknown id
// converts to a quirk.ID cleanly and is then simply never matched, so without
// this a typo'd override is silently inert.
func knownQuirk(id string) bool {
	for _, e := range quirk.Registry {
		if string(e.ID) == id {
			return true
		}
	}
	return false
}
