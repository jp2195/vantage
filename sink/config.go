// Config: YAML-file + built-in defaults for vantage-writer, mirroring
// collector/config.go so the two daemons behave the same way when a
// field is left unset.
package sink

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/natstls"
	"github.com/jp2195/vantage/redact"
	"github.com/jp2195/vantage/secret"
)

// Config is vantage-writer's full runtime configuration, loaded from an
// optional YAML file (LoadConfig) and defaulted where the file (or its
// absence) leaves a field at its zero value.
type Config struct {
	// NatsURL and ClickHouseDSN are secret types, not strings: both carry
	// an operator's password, both are formatted into startup errors, and
	// several fixes to those errors, one call site at a time, is what
	// secret exists to end. Printing either one -- %s, %v, slog,
	// JSON -- yields the redacted rendering; reaching the real value is an
	// explicit RevealSecret call. The YAML tags are unchanged: the types
	// unmarshal from the same scalars they always did.
	NatsURL       secret.NatsURL       `yaml:"nats_url"`
	ClickHouseDSN secret.ClickHouseDSN `yaml:"clickhouse_dsn"`

	// ClickHousePasswordFile names a file whose contents are ClickHouse's
	// password; see ResolveClickHouseDSN. It lets a Kubernetes Secret be
	// mounted as a file instead of being rendered into this config.
	ClickHousePasswordFile string `yaml:"clickhouse_password_file"`

	// LogLevel (debug|info|warn|error) and LogFormat (text|json); see
	// package logging.
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`

	// NatsTLS is the TLS material this daemon presents to NATS. Absent
	// means plaintext, which is what every install before this field
	// existed did and what docker-compose.dev.yml still does.
	//
	// It is natstls.Config rather than a local struct so this daemon and
	// vantage-collector cannot drift: one type, one Validate, one place that
	// turns paths into nats.Option values.
	NatsTLS natstls.Config `yaml:"nats_tls"`

	MetricsListen string        `yaml:"metrics_listen"`
	BatchRows     int           `yaml:"batch_rows"`
	BatchWait     time.Duration `yaml:"batch_wait"`
	FetchBatch    int           `yaml:"fetch_batch"`

	// CurrentCleanupInterval is how often the writer deletes superseded
	// sessions' rows from the current tables, which have no TTL. 0 disables
	// it; with several writers, one holding the cleanup lease does the work.
	CurrentCleanupInterval time.Duration `yaml:"current_cleanup_interval"`
}

// LoadConfig reads the YAML file at path (path == "" skips reading and
// returns pure defaults) and fills in any field the file left unset. As in
// collector/config.go, there is no separate flag-based override
// layer: the only command-line input vantage-writer takes is which config
// file to load (-config), so file contents versus built-in defaults is the
// only precedence this function needs to resolve, field by field.
func LoadConfig(path string) (Config, error) {
	// Set before decoding, not defaulted after: 0 is a value (disabled), so
	// a zero after decoding cannot mean "unset".
	cfg := Config{CurrentCleanupInterval: time.Hour}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		// KnownFields(true) rather than yaml.Unmarshal: an unknown key is
		// otherwise silently ignored, so a typo like "batch_row:" leaves the
		// built-in default in place with no warning and the daemon starts
		// looking healthy while ignoring the operator's intent.
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, err
		}
	}
	if cfg.NatsURL.Empty() {
		cfg.NatsURL = secret.NewNatsURL("nats://127.0.0.1:4222")
	}
	dsn, err := ResolveClickHouseDSN(cfg.ClickHouseDSN, cfg.ClickHousePasswordFile)
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
	if cfg.MetricsListen == "" {
		cfg.MetricsListen = "0.0.0.0:9472"
	}
	if cfg.BatchRows == 0 {
		cfg.BatchRows = 5000
	}
	if cfg.BatchWait == 0 {
		cfg.BatchWait = 2 * time.Second
	}
	if cfg.FetchBatch == 0 {
		cfg.FetchBatch = 500
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects a config that would start a daemon which looks healthy
// and does the wrong thing, the way collector.Config.validate does. Until
// nats_tls there was nothing here to check, so LoadConfig returned whatever
// decoded; a half-configured client keypair is the first value that would
// otherwise start the writer connecting without the certificate its operator
// believes it is presenting.
func (c *Config) validate() error {
	var errs []error
	if err := c.NatsTLS.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("nats_tls: %w", err))
	}
	if err := logging.Validate(c.LogLevel, c.LogFormat); err != nil {
		errs = append(errs, err)
	}
	if c.CurrentCleanupInterval < 0 {
		errs = append(errs, fmt.Errorf("current_cleanup_interval: %v is negative; 0 disables cleanup", c.CurrentCleanupInterval))
	}
	return errors.Join(errs...)
}

// defaultClickHouseDSN is the DSN vantage-writer and vantage-api use when
// the config names none: the dev stack's ClickHouse on loopback.
const defaultClickHouseDSN = "clickhouse://vantage:vantage@127.0.0.1:9000/vantage"

// ResolveClickHouseDSN applies the default DSN and clickhouse_password_file,
// for both daemons that dial ClickHouse, so the two keys mean the same thing
// in writer.yaml and api.yaml.
//
// With passwordFile set, the file's contents -- one trailing newline
// trimmed, since `echo` and most editors add one and a Kubernetes Secret
// written from a shell usually carries it -- become the DSN's password, and
// the DSN keeps its user, host, database and parameters. A DSN that also
// carries a password, in its userinfo or as a password= parameter (which
// clickhouse-go reads and which would win over the userinfo), is refused:
// two sources for one credential means the operator believes one of them is
// in effect and cannot tell which.
//
// With no DSN in the config, the default's user and address are kept and its
// dev password is dropped, so naming only a password file is not itself a
// conflict.
//
// No error here quotes the file's contents or the DSN's password.
func ResolveClickHouseDSN(dsn secret.ClickHouseDSN, passwordFile string) (secret.ClickHouseDSN, error) {
	if passwordFile == "" {
		if dsn.Empty() {
			return secret.NewClickHouseDSN(defaultClickHouseDSN), nil
		}
		return dsn, nil
	}
	raw := dsn.RevealSecret()
	if dsn.Empty() {
		raw = "clickhouse://vantage@127.0.0.1:9000/vantage"
	}
	b, err := os.ReadFile(passwordFile)
	if err != nil {
		return secret.ClickHouseDSN{}, fmt.Errorf("clickhouse_password_file: %w", err)
	}
	pw := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if pw == "" {
		return secret.ClickHouseDSN{}, fmt.Errorf("clickhouse_password_file: %s is empty", passwordFile)
	}
	if err := secret.NewClickHouseDSN(raw).Validate(); err != nil {
		return secret.ClickHouseDSN{}, fmt.Errorf("clickhouse_dsn: %w", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return secret.ClickHouseDSN{}, fmt.Errorf("clickhouse_dsn: %w", redact.Err(err))
	}
	_, inUserinfo := u.User.Password()
	if inUserinfo || u.Query().Has("password") {
		return secret.ClickHouseDSN{}, errors.New("clickhouse_dsn carries a password and " +
			"clickhouse_password_file is set; remove the password from the DSN")
	}
	u.User = url.UserPassword(u.User.Username(), pw)
	return secret.NewClickHouseDSN(u.String()), nil
}
