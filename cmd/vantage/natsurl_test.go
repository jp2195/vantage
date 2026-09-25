package main

import (
	"strings"
	"testing"

	"github.com/jp2195/vantage/natsutil/natstest"
)

// natsFlagSecret is the password planted in every -nats value below. It is
// long and distinctive so a substring assertion cannot pass by accident,
// and it contains no character that would change how nats.go or net/url
// segments the URL -- the shapes that need special segmentation are
// redact's own tests' job, not this file's.
const natsFlagSecret = "SUPERLONGPASSWORDSECRET"

// natsFlagValues are the -nats values each subcommand is driven with. Each
// carries a credential and each fails before doing any work, by one of the
// two routes an operator actually hits:
//
//   - a well-formed URL pointing at a port nothing listens on, which fails
//     in nats.go's dialer (no *url.Error involved -- the "%s" operand is
//     the only thing standing between the password and stderr); and
//   - a URL nats.go cannot parse, which fails in its parseServerURL and
//     returns a *url.Error carrying the raw segment, password included, so
//     the "%w" operand is exercised too. The '/' in the password is what
//     makes net/url reject it, and is exactly the shape a real generated
//     password takes: `openssl rand -base64` emits '/' routinely.
//
// The token form (a colonless userinfo, which nats.go reads as an auth
// token rather than a username) is third, because the policy of keeping
// the username visible in a redacted URL would print it in full if
// NatsURL were rendered with ClickHouse's policy instead of NATS's.
var natsFlagValues = []string{
	"nats://user:" + natsFlagSecret + "@127.0.0.1:1",
	"nats://user:pw/" + natsFlagSecret + "@127.0.0.1:1",
	"nats://" + natsFlagSecret + "@127.0.0.1:1",
}

// TestSubcommandsDoNotLeakTheNatsFlagCredential is the regression test for
// the third binary's copy of the original leak. `vantage streams init`,
// `vantage debug` and `vantage capture` each take an operator-supplied
// -nats flag, and each formatted it raw into
//
//	fmt.Errorf("connect nats %s: %w", *natsURL, err)
//
// -- a leak the config-side secret types did nothing about, because a flag
// is a second ingress. See natsFlag in main.go.
//
// The assertion is on the error each cmdXxx returns, which is precisely
// what main() writes to stderr ("vantage: " + err). Every subcommand is
// driven far enough to reach its NATS connect and no further, so no
// infrastructure is needed: -nats points at port 1.
func TestSubcommandsDoNotLeakTheNatsFlagCredential(t *testing.T) {
	out := t.TempDir() + "/capture.bmp"
	for _, c := range []struct {
		name string
		args func(natsURL string) []string
		run  func(args []string) error
	}{
		{
			name: "streams init",
			args: func(u string) []string { return []string{"init", "-nats", u} },
			run:  cmdStreams,
		},
		{
			name: "debug",
			args: func(u string) []string { return []string{"-nats", u} },
			run:  cmdDebug,
		},
		{
			// -router, -o and a mode are all required and are all
			// checked before the NATS connect, so they have to be
			// supplied for this test to reach the line under test at
			// all.
			name: "capture",
			args: func(u string) []string {
				return []string{"-nats", u, "-router", "10.0.0.1", "-o", out, "-since", "1m"}
			},
			run: cmdCapture,
		},
	} {
		for _, natsURL := range natsFlagValues {
			t.Run(c.name+" "+natsURL, func(t *testing.T) {
				err := c.run(c.args(natsURL))
				if err == nil {
					t.Fatalf("%s with -nats %q returned nil, want a connect failure",
						c.name, natsURL)
				}
				if strings.Contains(err.Error(), natsFlagSecret) {
					t.Errorf("%s error %q contains the -nats credential", c.name, err)
				}
				// Still actionable: an operator has to be able to see
				// which server failed, which is the whole reason the
				// redacted rendering keeps the host.
				if !strings.Contains(err.Error(), "127.0.0.1") {
					t.Errorf("%s error %q does not name the host it failed to reach",
						c.name, err)
				}
			})
		}
	}
}

// TestSubcommandsValidateNatsTLSFlags: each of the three subcommands that
// take -nats also takes -nats-cert and -nats-key, and each must reject a
// half-configured keypair BEFORE dialing. The -nats value below points at a
// port nothing listens on, so if validation ran late the error would be a
// dial failure instead -- which is exactly the assertion below.
func TestSubcommandsValidateNatsTLSFlags(t *testing.T) {
	out := t.TempDir() + "/capture.bmp"
	for _, c := range []struct {
		name string
		args []string
		run  func(args []string) error
	}{
		{
			name: "streams init",
			args: []string{"init", "-nats", "nats://127.0.0.1:1", "-nats-cert", "/tls.crt"},
			run:  cmdStreams,
		},
		{
			name: "debug",
			args: []string{"-nats", "nats://127.0.0.1:1", "-nats-cert", "/tls.crt"},
			run:  cmdDebug,
		},
		{
			name: "capture",
			args: []string{"-nats", "nats://127.0.0.1:1", "-nats-cert", "/tls.crt",
				"-router", "10.0.0.1", "-o", out, "-since", "1m"},
			run: cmdCapture,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(c.args)
			if err == nil {
				t.Fatalf("%s accepted -nats-cert with no -nats-key", c.name)
			}
			if !strings.Contains(err.Error(), "key_file") {
				t.Errorf("%s failed with %q, want the configuration error naming "+
					"key_file -- a dial error means the flags are not being validated",
					c.name, err)
			}
		})
	}
}

// TestStreamsInitAppliesNatsTLSFlags is the partner to
// TestSubcommandsValidateNatsTLSFlags: rather than a complete keypair
// against a dead port -- which cannot tell a TLS config that reached
// natsutil.Connect apart from one silently dropped, since the TCP dial is
// refused before any TLS handshake could start either way -- this dials a
// REAL TLS NATS server that requires a client certificate
// (natstest.RunTLS). Only a keypair that actually reaches the dial can
// complete a handshake with it, so success here is proof the flags were
// wired through, not just declared.
//
// streams init is the vehicle because it is the only one of the three
// NATS-touching subcommands that connects, does its work and returns --
// debug subscribes and blocks, capture waits for messages, and neither
// would complete inside a test against a live server.
func TestStreamsInitAppliesNatsTLSFlags(t *testing.T) {
	m := natstest.RunTLS(t)
	// Small stream limits: nats-server validates MaxBytes against the JetStream
	// store directory's free disk space (see streams.go's -routes-max-bytes
	// doc), and this test should not depend on how much disk the machine
	// running it happens to have free.
	tlsFlags := []string{"-nats-ca", m.CAFile, "-nats-cert", m.CertFile, "-nats-key", m.KeyFile,
		"-routes-max-bytes", "16777216", "-raw-max-bytes", "16777216"}

	t.Run("full keypair reaches the dial and succeeds", func(t *testing.T) {
		args := append([]string{"init", "-nats", m.URL}, tlsFlags...)
		if err := cmdStreams(args); err != nil {
			t.Fatalf("streams init against a TLS server with a complete keypair: %v", err)
		}
	})

	t.Run("no TLS flags fails the handshake, not a dial timeout", func(t *testing.T) {
		err := cmdStreams([]string{"init", "-nats", m.URL,
			"-routes-max-bytes", "16777216", "-raw-max-bytes", "16777216"})
		if err == nil {
			t.Fatal("streams init connected to a server requiring a client certificate with no TLS flags at all")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "certificate") &&
			!strings.Contains(strings.ToLower(err.Error()), "tls") {
			t.Errorf("streams init failed with %q, which names neither a certificate nor a tls problem -- "+
				"want proof this failed the TLS handshake, not a network-level dial error", err)
		}
	})
}
