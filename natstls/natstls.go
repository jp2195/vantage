// Package natstls carries the TLS material a vantage process presents to
// NATS, as file paths rather than inline PEM.
//
// It is a leaf package on purpose. natsutil is the obvious home -- Connect
// lives there, and the options need to be built in one place so the two
// daemons cannot drift -- but natsutil imports collector (for
// Publisher), so a collector.Config field of a natsutil type is an import
// cycle. Both daemon configs and all three NATS-touching CLI subcommands
// embed this type, so it has to sit below every one of them. The
// single-implementation property is what matters and it is preserved:
// nothing outside this file turns these paths into nats.Option values.
//
// Paths, not inline PEM, for three reasons that point the same way:
// cert-manager delivers a Secret which Kubernetes projects as files;
// rotation then replaces file contents in place, where inline PEM would mean
// re-rendering config and restarting both daemons on every renewal; and a
// private key in a config field is a key in whatever object carries that
// config.
//
// Rotation needs no code here. nats.RootCAs and nats.ClientCert each store a
// CALLBACK that reads its files when invoked (nats.go v1.54.0,
// Options.RootCAsCB and Options.TLSCertCB), and nats.go invokes it per
// connection attempt -- so a renewed certificate is picked up by the next
// reconnect with no reload plumbing, no SIGHUP handler and no file watcher.
// Probed against a real server on 2026-09-21 by replacing the CA, the client
// certificate and the server certificate with an unrelated authority's on
// the same paths and forcing a reconnect.
package natstls

import (
	"errors"

	"github.com/nats-io/nats.go"
)

// Config is the nats_tls block both daemons accept and the vantage CLI
// takes as flags.
//
// The zero value means plaintext -- exactly what every install before this
// type existed did, and what docker-compose.dev.yml still does. That is not
// an incidental property: the dev stack is deliberately plaintext, so no dev
// run will ever notice if this stops being true, and the tests are the only
// thing guarding it.
//
// CAFile alone is server-side TLS against a private CA. Adding CertFile and
// KeyFile makes it mutual. One of that pair without the other is a
// configuration error, rejected at load rather than at connect.
type Config struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Empty reports whether no TLS material was configured at all, which means
// plaintext.
func (c Config) Empty() bool {
	return c.CAFile == "" && c.CertFile == "" && c.KeyFile == ""
}

// Validate rejects a half-configured client keypair.
//
// A certificate without its key is the configuration that looks enabled and
// silently is not: nats.ClientCert is never built, the daemon connects
// without presenting anything, and a server that does not happen to require
// client certificates accepts it. The failure then surfaces on the day
// somebody turns verification on, far from the edit that caused it.
//
// Errors name the FIELD, never the file's contents, so they are safe to
// print. redact.Err still wraps whatever reaches a caller, which keeps that
// a guarantee rather than a property of this sentence.
func (c Config) Validate() error {
	var errs []error
	if c.CertFile != "" && c.KeyFile == "" {
		errs = append(errs, errors.New("cert_file is set without key_file: a client certificate needs both"))
	}
	if c.KeyFile != "" && c.CertFile == "" {
		errs = append(errs, errors.New("key_file is set without cert_file: a client certificate needs both"))
	}
	return errors.Join(errs...)
}

// Options returns the nats.Option values for this configuration, or an error
// if it does not make sense. A zero Config returns no options and no error.
//
// Both options nats.go builds here validate eagerly -- each reads its files
// once while being applied and returns the error -- so an unreadable or
// unparseable file fails inside nats.Connect at startup, naming the path.
// That is the loud failure this project wants: MaxReconnects governs
// reconnection after a successful connect, not the initial dial, so a
// misconfigured certificate cannot turn into a hot retry loop that presents
// as a network problem.
func (c Config) Options() ([]nats.Option, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var opts []nats.Option
	if c.CAFile != "" {
		opts = append(opts, nats.RootCAs(c.CAFile))
	}
	if c.CertFile != "" {
		opts = append(opts, nats.ClientCert(c.CertFile, c.KeyFile))
	}
	return opts, nil
}
