package natsutil

import (
	"github.com/nats-io/nats.go"

	"github.com/jp2195/vantage/natstls"
	"github.com/jp2195/vantage/redact"
	"github.com/jp2195/vantage/secret"
)

// Connect dials NATS from an operator-supplied URL.
//
// It exists so that neither daemon's main has to call RevealSecret itself:
// this function and sink.NewClickHouse are the only two places in the tree
// that unwrap a secret, which is what makes `git grep RevealSecret` a
// complete audit of where credentials are handled.
//
// Every error it returns has been through redact.Err, so a caller writing
// the obvious
//
//	fmt.Errorf("connect nats %s: %w", cfg.NatsURL, err)
//
// is safe in both operands. nats.go parses each server URL with
// net/url.Parse and returns the resulting *url.Error (parseServerURL,
// v1.54.0), which carries the raw segment -- password included -- and is
// what redact.Err recognizes structurally, at any depth of wrapping.
//
// The URL is passed to nats.Connect exactly as the operator wrote it. It is
// never rebuilt from a parse: NATS's multi-server list and its schemeless
// shorthand are nats.go's grammar to interpret, not ours, and a round-trip
// through net/url would risk reshaping a value that works today.
//
// TLS comes in as a natstls.Config rather than as another variadic option
// because this function has five callers -- two daemons and three vantage
// subcommands -- and a variadic can be forgotten at any one of them. A
// positional parameter makes the compiler ask each caller what it does about
// TLS. natstls.Config{} is the answer that means plaintext, which is what
// every caller did before this parameter existed.
func Connect(u secret.NatsURL, tc natstls.Config, opts ...nats.Option) (*nats.Conn, error) {
	tlsOpts, err := tc.Options()
	if err != nil {
		return nil, redact.Err(err)
	}
	// TLS options first, so an explicit option from a caller wins. Nothing
	// passes one today; the ordering is so that if something ever does, it
	// overrides rather than is overridden.
	nc, err := nats.Connect(u.RevealSecret(), append(tlsOpts, opts...)...)
	if err != nil {
		return nil, redact.Err(err)
	}
	return nc, nil
}
