package sink

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/natstls"
)

func TestLoadConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writer.yaml")
	yamlContent := `
nats_url: "nats://nats.internal:4222"
clickhouse_dsn: "clickhouse://u:p@ch.internal:9000/vantage"
metrics_listen: ":9999"
batch_rows: 1000
batch_wait: 500ms
fetch_batch: 50
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NatsURL.RevealSecret() != "nats://nats.internal:4222" {
		t.Errorf("NatsURL = %q", cfg.NatsURL.RevealSecret())
	}
	if cfg.ClickHouseDSN.RevealSecret() != "clickhouse://u:p@ch.internal:9000/vantage" {
		t.Errorf("ClickHouseDSN = %q", cfg.ClickHouseDSN.RevealSecret())
	}
	if cfg.MetricsListen != ":9999" {
		t.Errorf("MetricsListen = %q", cfg.MetricsListen)
	}
	if cfg.BatchRows != 1000 {
		t.Errorf("BatchRows = %d, want 1000", cfg.BatchRows)
	}
	if cfg.BatchWait != 500*time.Millisecond {
		t.Errorf("BatchWait = %v, want 500ms", cfg.BatchWait)
	}
	if cfg.FetchBatch != 50 {
		t.Errorf("FetchBatch = %d, want 50", cfg.FetchBatch)
	}
}

// TestLoadConfigPartialFileStillDefaults confirms a config file that only
// sets a subset of fields still gets defaults for the rest -- LoadConfig's
// documented precedence: file value wins per-field where the file sets one,
// built-in default otherwise.
func TestLoadConfigPartialFileStillDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writer.yaml")
	if err := os.WriteFile(path, []byte("nats_url: nats://only-this-is-set:4222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NatsURL.RevealSecret() != "nats://only-this-is-set:4222" {
		t.Errorf("NatsURL = %q", cfg.NatsURL.RevealSecret())
	}
	if cfg.ClickHouseDSN.RevealSecret() != "clickhouse://vantage:vantage@127.0.0.1:9000/vantage" {
		t.Errorf("ClickHouseDSN = %q, want default", cfg.ClickHouseDSN.RevealSecret())
	}
	if cfg.MetricsListen != "0.0.0.0:9472" {
		t.Errorf("MetricsListen = %q, want default 0.0.0.0:9472", cfg.MetricsListen)
	}
	if cfg.BatchRows != 5000 {
		t.Errorf("BatchRows = %d, want default 5000", cfg.BatchRows)
	}
	if cfg.BatchWait != 2*time.Second {
		t.Errorf("BatchWait = %v, want default 2s", cfg.BatchWait)
	}
	if cfg.FetchBatch != 500 {
		t.Errorf("FetchBatch = %d, want default 500", cfg.FetchBatch)
	}
}

func TestLoadConfigEmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NatsURL.RevealSecret() != "nats://127.0.0.1:4222" {
		t.Errorf("NatsURL = %q, want default", cfg.NatsURL.RevealSecret())
	}
	if cfg.ClickHouseDSN.RevealSecret() != "clickhouse://vantage:vantage@127.0.0.1:9000/vantage" {
		t.Errorf("ClickHouseDSN = %q, want default", cfg.ClickHouseDSN.RevealSecret())
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
	if err := os.WriteFile(path, []byte("nats_url: [this is not valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected a YAML parse error")
	}
}

func TestLoadConfigUnknownFieldErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.yaml")
	if err := os.WriteFile(path, []byte("nats_urls: nats://typo:4222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an unknown-field error for nats_urls (typo of nats_url)")
	}
}

// TestLoadConfigCarriesCredentialsWithoutRenderingThem is the config-level
// half of the credential-leak fix: the two fields that carry a password are
// secret types now (secret), which has to leave YAML loading
// working byte-for-byte -- the libraries parse the operator's string, not a
// rebuilt one -- while making the loaded Config unprintable.
//
// The values here use the shapes that have broken redaction before: a '"'
// and a '/' in a password, a comma-separated NATS list, and a password in a
// ClickHouse query parameter (which has no '@' in it at all).
func TestLoadConfigCarriesCredentialsWithoutRenderingThem(t *testing.T) {
	const (
		sentinel = "SECRET"
		natsURL  = `nats://user:sup"` + sentinel + `@n1.internal:4222,nats://user:ab/cd` + sentinel + `@n2.internal:4222`
		dsn      = `clickhouse://vantage:x` + sentinel + `@ch.internal:9000/vantage?password=q` + sentinel
	)
	path := filepath.Join(t.TempDir(), "writer.yaml")
	body, err := yaml.Marshal(map[string]string{"nats_url": natsURL, "clickhouse_dsn": dsn})
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
	// Round-trip: what nats.Connect and clickhouse-go receive must be what
	// the operator wrote, unmodified.
	if got := cfg.NatsURL.RevealSecret(); got != natsURL {
		t.Errorf("NatsURL = %q, want %q", got, natsURL)
	}
	if got := cfg.ClickHouseDSN.RevealSecret(); got != dsn {
		t.Errorf("ClickHouseDSN = %q, want %q", got, dsn)
	}
	// And the whole Config, formatted the way a debug log line would format
	// it, contains neither credential.
	for _, rendered := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%s %s", cfg.NatsURL, cfg.ClickHouseDSN),
	} {
		if strings.Contains(rendered, sentinel) {
			t.Errorf("a formatted Config contains the credential: %s", rendered)
		}
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
	path := filepath.Join(t.TempDir(), "writer.yaml")
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
	path := filepath.Join(t.TempDir(), "writer.yaml")
	if err := os.WriteFile(path, []byte("metrics_listen: \"0.0.0.0:9472\"\n"), 0o600); err != nil {
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
	path := filepath.Join(t.TempDir(), "writer.yaml")
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
	path := filepath.Join(t.TempDir(), "writer.yaml")
	if err := os.WriteFile(path, []byte("nats_tsl:\n  ca_file: /ca.crt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted the misspelled key nats_tsl")
	}
}

func writeWriterYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "writer.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigLogKeys(t *testing.T) {
	cfg, err := LoadConfig(writeWriterYAML(t, "log_level: debug\nlog_format: json\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("LogLevel, LogFormat = %q, %q", cfg.LogLevel, cfg.LogFormat)
	}

	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "text" {
		t.Errorf("defaults = %q, %q, want info, text", cfg.LogLevel, cfg.LogFormat)
	}

	for _, body := range []string{"log_level: verbose\n", "log_format: logfmt\n"} {
		if _, err := LoadConfig(writeWriterYAML(t, body)); err == nil {
			t.Errorf("LoadConfig accepted %q", body)
		}
	}
}

// writeSecretFile writes a password file the way a Kubernetes Secret volume
// or `echo pw > file` would: with a trailing newline.
func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigPasswordFileReplacesThePassword(t *testing.T) {
	for _, c := range []struct{ content, want string }{
		{"s3cret\n", "s3cret"},
		{"s3cret\r\n", "s3cret"},
		{"s3cret", "s3cret"},
		// Characters that must be escaped in userinfo: the result has to
		// be a DSN the driver parses back to the same password.
		{"p@ss:w/rd#?%\n", "p@ss:w/rd#?%"},
	} {
		pw := writeSecretFile(t, c.content)
		cfg, err := LoadConfig(writeWriterYAML(t,
			"clickhouse_dsn: \"clickhouse://writer@ch.internal:9000/vantage?dial_timeout=5s\"\n"+
				"clickhouse_password_file: "+pw+"\n"))
		if err != nil {
			t.Fatalf("%q: %v", c.content, err)
		}
		u, err := url.Parse(cfg.ClickHouseDSN.RevealSecret())
		if err != nil {
			t.Fatalf("%q: resulting DSN does not parse: %v", c.content, err)
		}
		got, ok := u.User.Password()
		if !ok || got != c.want || u.User.Username() != "writer" {
			t.Errorf("%q: userinfo = %q/%q, want writer/%q", c.content, u.User.Username(), got, c.want)
		}
		if u.Host != "ch.internal:9000" || u.Path != "/vantage" || u.RawQuery != "dial_timeout=5s" {
			t.Errorf("%q: rest of the DSN changed: %s", c.content, cfg.ClickHouseDSN)
		}
		if err := cfg.ClickHouseDSN.Validate(); err != nil {
			t.Errorf("%q: resulting DSN fails validation: %v", c.content, err)
		}
	}
}

// TestLoadConfigPasswordFileWithTheDefaultDSN: the built-in DSN carries the
// dev stack's password, so naming only a password file must not collide
// with it -- the file supplies the password to the default user instead.
func TestLoadConfigPasswordFileWithTheDefaultDSN(t *testing.T) {
	pw := writeSecretFile(t, "fromfile\n")
	cfg, err := LoadConfig(writeWriterYAML(t, "clickhouse_password_file: "+pw+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ClickHouseDSN.RevealSecret(); got != "clickhouse://vantage:fromfile@127.0.0.1:9000/vantage" {
		t.Errorf("DSN = %q", got)
	}
}

func TestLoadConfigPasswordFileConflicts(t *testing.T) {
	pw := writeSecretFile(t, "fromfile\n")
	for _, dsn := range []string{
		"clickhouse://writer:inline@ch:9000/vantage",
		"clickhouse://writer@ch:9000/vantage?password=inline",
	} {
		_, err := LoadConfig(writeWriterYAML(t,
			"clickhouse_dsn: \""+dsn+"\"\nclickhouse_password_file: "+pw+"\n"))
		if err == nil {
			t.Errorf("LoadConfig accepted a password in both %q and the file", dsn)
			continue
		}
		for _, want := range []string{"clickhouse_dsn", "clickhouse_password_file"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		for _, leak := range []string{"inline", "fromfile"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error %q leaks a password", err)
			}
		}
	}
}

func TestLoadConfigPasswordFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	for name, pw := range map[string]string{
		"missing": missing,
		"empty":   writeSecretFile(t, "\n"),
	} {
		_, err := LoadConfig(writeWriterYAML(t, "clickhouse_password_file: "+pw+"\n"))
		if err == nil {
			t.Errorf("%s password file accepted", name)
		} else if !strings.Contains(err.Error(), "clickhouse_password_file") {
			t.Errorf("%s: error %q does not name the key", name, err)
		}
	}
}

func TestLoadConfigCurrentCleanupInterval(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentCleanupInterval != time.Hour {
		t.Errorf("default CurrentCleanupInterval = %v, want 1h", cfg.CurrentCleanupInterval)
	}
	// A file that leaves the key out keeps the default, like every other key.
	cfg, err = LoadConfig(writeWriterYAML(t, "batch_rows: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentCleanupInterval != time.Hour {
		t.Errorf("unset CurrentCleanupInterval = %v, want 1h", cfg.CurrentCleanupInterval)
	}
	for body, want := range map[string]time.Duration{
		"current_cleanup_interval: 15m\n": 15 * time.Minute,
		// 0 disables cleanup, so it must survive defaulting rather than
		// being read as "unset".
		"current_cleanup_interval: 0s\n": 0,
	} {
		cfg, err := LoadConfig(writeWriterYAML(t, body))
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if cfg.CurrentCleanupInterval != want {
			t.Errorf("%q: CurrentCleanupInterval = %v, want %v", body, cfg.CurrentCleanupInterval, want)
		}
	}
	if _, err := LoadConfig(writeWriterYAML(t, "current_cleanup_interval: -1m\n")); err == nil {
		t.Error("LoadConfig accepted a negative current_cleanup_interval")
	}
}
