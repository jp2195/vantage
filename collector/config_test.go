package collector

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/natstls"
)

// parseConfigForTest writes yamlContent to a temp file and loads it, so the
// tests below exercise the real LoadConfig path (defaults, canonicalization
// and validation) rather than a hand-built Config that skips all three.
func parseConfigForTest(t *testing.T, yamlContent string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.yaml")
	yamlContent := `
listen: ":12345"
nats_url: "nats://nats.internal:4222"
collector_id: "rtr-collector-1"
metrics_listen: ":9999"
admin_listen: ":9998"
streams:
  partitions: 16
  replicas: 3
  ls_replicas: 3
  routes_max_bytes: 1073741824
  raw_max_bytes: 536870912
routers:
  10.0.0.1:
    force_quirks: ["QK_TS_ZERO"]
    disable_quirks: ["QK_CAPS_MISSING"]
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != ":12345" {
		t.Errorf("Listen = %q, want :12345", cfg.Listen)
	}
	if cfg.NatsURL.RevealSecret() != "nats://nats.internal:4222" {
		t.Errorf("NatsURL = %q", cfg.NatsURL.RevealSecret())
	}
	if cfg.CollectorID != "rtr-collector-1" {
		t.Errorf("CollectorID = %q", cfg.CollectorID)
	}
	if cfg.MetricsListen != ":9999" {
		t.Errorf("MetricsListen = %q", cfg.MetricsListen)
	}
	if cfg.AdminListen != ":9998" {
		t.Errorf("AdminListen = %q", cfg.AdminListen)
	}
	if cfg.Streams.Partitions != 16 || cfg.Streams.Replicas != 3 || cfg.Streams.LSReplicas != 3 ||
		cfg.Streams.RoutesMaxBytes != 1073741824 || cfg.Streams.RawMaxBytes != 536870912 {
		t.Errorf("Streams = %+v", cfg.Streams)
	}
	ov, ok := cfg.Routers["10.0.0.1"]
	if !ok {
		t.Fatalf("Routers[10.0.0.1] missing; got %+v", cfg.Routers)
	}
	if len(ov.ForceQuirks) != 1 || ov.ForceQuirks[0] != "QK_TS_ZERO" {
		t.Errorf("ForceQuirks = %v", ov.ForceQuirks)
	}
	if len(ov.DisableQuirks) != 1 || ov.DisableQuirks[0] != "QK_CAPS_MISSING" {
		t.Errorf("DisableQuirks = %v", ov.DisableQuirks)
	}
}

// TestLoadConfigPartialFileStillDefaults confirms a config file that only
// sets a subset of fields still gets defaults for the rest -- LoadConfig's
// documented precedence: file value wins per-field where the file sets one,
// built-in default otherwise.
func TestLoadConfigPartialFileStillDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.yaml")
	if err := os.WriteFile(path, []byte("collector_id: only-this-is-set\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CollectorID != "only-this-is-set" {
		t.Errorf("CollectorID = %q", cfg.CollectorID)
	}
	if cfg.Listen != ":11019" {
		t.Errorf("Listen = %q, want default :11019", cfg.Listen)
	}
	if cfg.MetricsListen != ":9469" {
		t.Errorf("MetricsListen = %q, want default :9469", cfg.MetricsListen)
	}
	if cfg.AdminListen != "127.0.0.1:9470" {
		t.Errorf("AdminListen = %q, want default 127.0.0.1:9470", cfg.AdminListen)
	}
	if cfg.NatsURL.RevealSecret() != "nats://127.0.0.1:4222" {
		t.Errorf("NatsURL = %q, want default", cfg.NatsURL.RevealSecret())
	}
	if cfg.Streams.LSReplicas != 1 {
		t.Errorf("Streams.LSReplicas = %d, want default 1", cfg.Streams.LSReplicas)
	}
}

func TestLoadConfigMissingFileErrors(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error for a nonexistent config path")
	}
}

func TestLoadConfigInvalidYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("listen: [this is not valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected a YAML parse error")
	}
}

// TestLoadConfigLSReplicasFollowsReplicas pins that an unset ls_replicas
// tracks replicas rather than collapsing to 1. EnsureStreams uses
// CreateOrUpdateStream and runs on every collector start, so a flat default
// of 1 would silently rewrite a cluster's LS stream down to R1 on deploy --
// inverting the design intent that LS is the *more* replicated
// stream because topology data is high-value.
func TestLoadConfigLSReplicasFollowsReplicas(t *testing.T) {
	for _, c := range []struct {
		name string
		yaml string
		want int
	}{
		{"cluster, ls unset", "streams:\n  replicas: 3\n", 3},
		{"single node, both unset", "listen: \":1\"\n", 1},
		{"explicit ls wins", "streams:\n  replicas: 3\n  ls_replicas: 5\n", 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(c.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Streams.LSReplicas != c.want {
				t.Fatalf("LSReplicas=%d want %d", cfg.Streams.LSReplicas, c.want)
			}
		})
	}
}

// TestLoadConfigRejectsUnknownKey pins KnownFields(true): a mistyped key
// otherwise leaves the built-in default in place with no warning, so the
// daemon starts looking healthy while ignoring what the operator wrote.
func TestLoadConfigRejectsUnknownKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("nats_urls: nats://x:4222\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("want an error for a mistyped key, got nil")
	}
}

// TestLoadConfigCanonicalizesRouterKeys pins that router overrides are keyed
// the way the server looks them up (netip.Addr.String()). The server matches
// by exact string equality, so an uppercase or IPv4-mapped key would parse
// fine and then silently never match any router.
func TestLoadConfigCanonicalizesRouterKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "routers:\n  \"2001:DB8::1\":\n    force_quirks: [\"QK_TS_ZERO\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParseAddr("2001:DB8::1").String()
	if _, ok := cfg.Routers[want]; !ok {
		t.Fatalf("router key not canonicalized to %q: %v", want, cfg.Routers)
	}
}

// TestLoadConfigRejectsBadRouterOverride covers the two ways a router
// override is silently inert: a key that is not an address at all, and a
// quirk id that converts cleanly to quirk.ID and is then never matched.
func TestLoadConfigRejectsBadRouterOverride(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"not an address", "routers:\n  \"10.0.0.1/32\":\n    force_quirks: [\"QK_TS_ZERO\"]\n"},
		{"unknown quirk", "routers:\n  \"10.0.0.1\":\n    force_quirks: [\"QK_NOPE\"]\n"},
		{"unknown disable quirk", "routers:\n  \"10.0.0.1\":\n    disable_quirks: [\"QK_NOPE\"]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(p); err == nil {
				t.Fatal("want a validation error, got nil")
			}
		})
	}
}

// TestLoadConfigAdminListenDefault pins the admin API's default listen
// address: loopback-only, port 9470. The admin API arms a real capability
// (mirroring another router's traffic) with no authentication, no cap on how
// many mirrors can be armed at once, and no upper bound on max_bytes -- an
// unauthenticated bind-all default would let anything that can reach the
// host at all arm as many maximal mirrors as it likes. Binding to
// 127.0.0.1 fails closed: an operator who wants it reachable off-box (behind
// a NetworkPolicy, a reverse proxy that adds auth, etc.) sets admin_listen
// explicitly. This also keeps it off the metrics listener's exposure surface
// (:9469, routinely scraped from far more broadly than an admin control
// plane should be reachable from) without relying on the two ports merely
// happening to differ.
func TestLoadConfigAdminListenDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("listen: \":1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminListen != "127.0.0.1:9470" {
		t.Fatalf("AdminListen = %q, want \"127.0.0.1:9470\"", cfg.AdminListen)
	}
	host, _, err := net.SplitHostPort(cfg.AdminListen)
	if err != nil {
		t.Fatalf("AdminListen %q: %v", cfg.AdminListen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("AdminListen host = %q, want a loopback address -- an unauthenticated, "+
			"resource-consuming API must not default to binding every interface", host)
	}
}

// TestLoadConfigCarriesTheNatsCredentialWithoutRenderingIt mirrors
// sink/config_test.go's test of the same shape, for this daemon's
// one credential-bearing field. See secret for why NatsURL is not
// a string.
func TestLoadConfigCarriesTheNatsCredentialWithoutRenderingIt(t *testing.T) {
	const (
		sentinel = "SECRET"
		natsURL  = `nats://user:sup"` + sentinel + `@n1.internal:4222,user:ab/cd` + sentinel + `@n2.internal:4222`
	)
	path := filepath.Join(t.TempDir(), "collector.yaml")
	body, err := yaml.Marshal(map[string]string{"nats_url": natsURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.NatsURL.RevealSecret(); got != natsURL {
		t.Errorf("NatsURL = %q, want %q", got, natsURL)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%s", cfg.NatsURL),
	} {
		if strings.Contains(rendered, sentinel) {
			t.Errorf("a formatted Config contains the credential: %s", rendered)
		}
	}
}

// A router's vendor and OS come from operator configuration, because the BMP
// Initiation frequently does not carry an identity at all -- XRd's entire
// sysDescr is "26.1.1", a bare version with no vendor token. The operator
// knows what the box is; the collector can only read text.
func TestRouterOverrideCarriesVendorAndOS(t *testing.T) {
	c, err := parseConfigForTest(t, `
nats_url: "nats://127.0.0.1:4222"
collector_id: "c1"
routers:
  "10.0.103.62":
    vendor: cisco
    os: iosxr
  "10.0.103.74":
    vendor: Cisco
    os: NXOS
    force_quirks: ["QK_TS_ZERO"]
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	xr := c.Routers["10.0.103.62"]
	if xr.Vendor != "cisco" || xr.OS != "iosxr" {
		t.Errorf("10.0.103.62 vendor/os = %q/%q, want cisco/iosxr", xr.Vendor, xr.OS)
	}
	// Vendor and OS are matched against quirk.Entry.AppliesTo, which compares
	// lowercase tokens. Accepting an operator's capitalization and then never
	// matching anything would be a silent, unexplainable no-op.
	nx := c.Routers["10.0.103.74"]
	if nx.Vendor != "cisco" || nx.OS != "nxos" {
		t.Errorf("10.0.103.74 vendor/os = %q/%q, want them lowercased to cisco/nxos", nx.Vendor, nx.OS)
	}
	if len(nx.ForceQuirks) != 1 {
		t.Errorf("force_quirks alongside vendor/os = %v, want it preserved", nx.ForceQuirks)
	}
}

// An OS with no vendor cannot be matched: quirk.Profile keys version patterns
// and static entries on the vendor/OS pair, so a bare OS is silently inert.
// Rejecting it at load is the difference between a typo the operator fixes in
// a second and a config that looks applied and does nothing.
func TestRouterOverrideRejectsOSWithoutVendor(t *testing.T) {
	_, err := parseConfigForTest(t, `
nats_url: "nats://127.0.0.1:4222"
collector_id: "c1"
routers:
  "10.0.103.62":
    os: iosxr
`)
	if err == nil {
		t.Fatal("an os with no vendor must be rejected")
	}
	if !strings.Contains(err.Error(), "vendor") {
		t.Errorf("error should name the missing vendor, got: %v", err)
	}
}

// TestCollectorIDDerivedFromHostnameIsWarnedAbout covers the one config
// default in this file whose failure mode is silent and permanent.
//
// collector_id is half of session identity: the query layer scopes current
// state to max(session_id) per (collector_id, router_ip). So the ID has to be
// STABLE across restarts of the same logical collector, or a restart's session
// lands under a pairing the old one never used and cannot displace it. The old
// view is then stranded in the archive forever -- no future session will ever
// carry that collector_id again -- and if the process died hard enough to skip
// Session.Close, its peers stay recorded up and its routes keep being served
// as current state.
//
// os.Hostname() satisfies stability on a VM, a laptop and a StatefulSet pod,
// and does NOT satisfy it under a Kubernetes Deployment, ECS, Nomad, or
// anything else that names a replacement process differently from the process
// it replaced. LoadConfig cannot tell which of those it is running in, so it
// says what it did and what that costs, and leaves the judgment to whoever
// reads the log.
func TestCollectorIDDerivedFromHostnameIsWarnedAbout(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.CollectorIDDerived {
		t.Fatal("a config that set no collector_id reports CollectorIDDerived " +
			"false, so nothing downstream can warn about an identity that may " +
			"not survive a restart")
	}
	if cfg.CollectorID == "" {
		t.Fatal("collector_id is empty; the hostname fallback did not run")
	}
}

// The converse: an operator who set the ID has answered the question, and
// must not be warned at them on every start.
func TestConfiguredCollectorIDIsNotReportedAsDerived(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.yaml")
	if err := os.WriteFile(path, []byte("collector_id: \"shard-3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CollectorID != "shard-3" {
		t.Fatalf("CollectorID = %q, want shard-3", cfg.CollectorID)
	}
	if cfg.CollectorIDDerived {
		t.Error("an explicitly configured collector_id is reported as derived")
	}
}

func TestProxyProtocolDefaultsToOff(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyProtocol != ProxyProtocolOff {
		t.Fatalf("ProxyProtocol = %q, want %q", cfg.ProxyProtocol, ProxyProtocolOff)
	}
}

func TestProxyProtocolAcceptsRequired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte("proxy_protocol: required\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyProtocol != ProxyProtocolRequired {
		t.Fatalf("ProxyProtocol = %q, want %q", cfg.ProxyProtocol, ProxyProtocolRequired)
	}
}

// A value that is neither "off" nor "required" must fail the load rather than
// start a daemon that silently ignores the operator's intent. "on" and "true"
// are the two an operator will actually type by mistake -- both would read as
// "enabled" and both would leave the collector wide open if accepted and then
// compared against "required".
func TestProxyProtocolRejectsAnythingElse(t *testing.T) {
	for _, v := range []string{"on", "true", "yes", "Required", "optional"} {
		t.Run(v, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "collector.yaml")
			if err := os.WriteFile(path, []byte("proxy_protocol: "+v+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("LoadConfig accepted proxy_protocol: %q", v)
			}
		})
	}
}

// natsTLSYAML is the block an operator writes and the block the chart
// renders. Kept as one string so both the collector and writer tests are
// asserting against the same text an operator would paste.
const natsTLSYAML = `
nats_tls:
  ca_file: /etc/nats-tls/ca.crt
  cert_file: /etc/nats-tls/tls.crt
  key_file: /etc/nats-tls/tls.key
`

func TestLoadConfigNatsTLS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte(natsTLSYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := natstls.Config{
		CAFile:   "/etc/nats-tls/ca.crt",
		CertFile: "/etc/nats-tls/tls.crt",
		KeyFile:  "/etc/nats-tls/tls.key",
	}
	if cfg.NatsTLS != want {
		t.Errorf("NatsTLS = %+v, want %+v", cfg.NatsTLS, want)
	}
}

// TestLoadConfigWithoutNatsTLSIsPlaintext: the partner row. A config file
// that says nothing about TLS must leave the field zero, which natstls
// defines as plaintext.
func TestLoadConfigWithoutNatsTLSIsPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte("listen: \":11019\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.NatsTLS.Empty() {
		t.Errorf("NatsTLS = %+v on a file that never mentions it, want the zero value", cfg.NatsTLS)
	}
}

// TestLoadConfigRejectsAHalfKeypair pins the requirement: fails at config
// load, not at connect. The daemon must refuse to start, not start and connect
// without presenting the certificate the operator thought they configured.
func TestLoadConfigRejectsAHalfKeypair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	body := "nats_tls:\n  cert_file: /etc/nats-tls/tls.crt\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted cert_file with no key_file")
	}
	for _, want := range []string{"nats_tls", "key_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestLoadConfigRejectsAMisspelledNatsTLSKey: KnownFields(true) is what
// turns a typo into a loud failure instead of a daemon that starts plaintext
// while its operator believes it is using TLS.
func TestLoadConfigRejectsAMisspelledNatsTLSKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte("nats_tsl:\n  ca_file: /ca.crt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted the misspelled key nats_tsl")
	}
}

// TestLoadConfigSourceControls loads the three connection-admission keys as an
// operator writes them and checks they arrive intact.
func TestLoadConfigSourceControls(t *testing.T) {
	cfg, err := parseConfigForTest(t, `
proxy_protocol: required
allowed_sources: ["10.0.0.0/8", "2001:db8::/32"]
trusted_proxies: ["192.0.2.10/32"]
max_connections: 64
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AllowedSources, ","); got != "10.0.0.0/8,2001:db8::/32" {
		t.Errorf("AllowedSources = %q", got)
	}
	if got := strings.Join(cfg.TrustedProxies, ","); got != "192.0.2.10/32" {
		t.Errorf("TrustedProxies = %q", got)
	}
	if cfg.MaxConnections != 64 {
		t.Errorf("MaxConnections = %d, want 64", cfg.MaxConnections)
	}
}

func TestLoadConfigMaxConnectionsDefault(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConnections != defaultMaxConnections || defaultMaxConnections != 1024 {
		t.Fatalf("MaxConnections = %d, want 1024", cfg.MaxConnections)
	}
	if len(cfg.AllowedSources) != 0 || len(cfg.TrustedProxies) != 0 {
		t.Fatalf("source controls default to %v / %v, want empty", cfg.AllowedSources, cfg.TrustedProxies)
	}
}

// TestLoadConfigRejectsBadSourceControls: each of these would otherwise start
// a collector whose admission rule is not the one the operator wrote. An
// unparsable CIDR dropped from an allowlist can empty it, and an empty
// allowlist accepts everything; trusted_proxies with proxy_protocol off would
// check a proxy that is not there.
func TestLoadConfigRejectsBadSourceControls(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"allowed_sources not a CIDR", `allowed_sources: ["10.0.0.300/8"]`, "allowed_sources"},
		{"allowed_sources bare address", `allowed_sources: ["10.0.0.1"]`, "allowed_sources"},
		{"allowed_sources v4-mapped", `allowed_sources: ["::ffff:10.0.0.0/104"]`, "allowed_sources"},
		{"trusted_proxies not a CIDR", "proxy_protocol: required\ntrusted_proxies: [\"proxy.example\"]", "trusted_proxies"},
		{"trusted_proxies with proxy off", `trusted_proxies: ["192.0.2.10/32"]`, "trusted_proxies"},
		{"trusted_proxies with proxy explicitly off", "proxy_protocol: \"off\"\ntrusted_proxies: [\"192.0.2.10/32\"]", "trusted_proxies"},
		{"negative max_connections", `max_connections: -1`, "max_connections"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfigForTest(t, tc.yaml)
			if err == nil {
				t.Fatalf("LoadConfig accepted %q", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// TestLoadConfigLogKeys: the collector takes the same two logging keys as
// the writer and the api, validated by the same package.
func TestLoadConfigLogKeys(t *testing.T) {
	cfg, err := parseConfigForTest(t, "log_level: debug\nlog_format: json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("LogLevel, LogFormat = %q, %q", cfg.LogLevel, cfg.LogFormat)
	}
	for _, bad := range []string{"log_level: loud", "log_format: xml"} {
		if _, err := parseConfigForTest(t, bad); err == nil {
			t.Errorf("LoadConfig accepted %q", bad)
		}
	}
}
