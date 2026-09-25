// vantage: operator CLI — streams provisioning, wire debugging, synthetic
// BMP generation. One binary, stdlib flag subcommands.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nats-io/nats.go"

	"github.com/jp2195/vantage/buildinfo"
	"github.com/jp2195/vantage/natstls"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/secret"
)

// natsFlag converts a -nats flag value into the type that cannot be printed
// unredacted, at the point the flag is read.
//
// This exists because a flag is a *second ingress* for an operator
// credential, and the round that made the config fields secret types closed
// only the first. `vantage streams init`, `vantage debug` and `vantage
// capture` all take -nats, an operator can perfectly reasonably write
// "-nats nats://user:pass@host:4222" (or the schemeless token form nats.go
// accepts), and all three then formatted that raw string into
//
//	fmt.Errorf("connect nats %s: %w", *natsURL, err)
//
// -- the original leak, verbatim, three more times, going to this binary's
// stderr where an operator will copy it into a ticket. It is the concrete
// counterexample to "no call site can print it": no call site can print a
// *Config* value, which is a different claim.
//
// Converting here rather than patching those three fmt.Errorf calls is the
// point. Every one of them still reads exactly as a contributor would write
// it without thinking about any of this; they are safe in the "%s" operand
// because the value is a secret.NatsURL, and safe in the "%w" operand
// because natsutil.Connect has already put its error through redact.Err.
// A fourth subcommand that takes -nats gets the same protection by using
// the same two functions, not by remembering a rule.
func natsFlag(v *string) secret.NatsURL { return secret.NewNatsURL(*v) }

// natsURLFlag declares the -nats flag with the spelling, default and help
// text all three subcommands share, so a new one cannot drift.
func natsURLFlag(fs *flag.FlagSet) *string {
	return fs.String("nats", "nats://127.0.0.1:4222", "NATS URL")
}

// natsTLSFlags declares the -nats-ca / -nats-cert / -nats-key flags with the
// spelling and help text all three NATS-touching subcommands share, so a
// fourth one cannot drift -- the same reason natsURLFlag exists.
//
// These are paths, so unlike -nats they carry no credential and need no
// secret type. The private key they point AT is a credential; its path is
// not, and printing a path in an error is how an operator finds a typo.
func natsTLSFlags(fs *flag.FlagSet) *natstls.Config {
	c := &natstls.Config{}
	fs.StringVar(&c.CAFile, "nats-ca", "",
		"PEM CA bundle that signs the NATS server certificate")
	fs.StringVar(&c.CertFile, "nats-cert", "",
		"PEM client certificate to present to NATS (requires -nats-key)")
	fs.StringVar(&c.KeyFile, "nats-key", "",
		"PEM private key for -nats-cert")
	return c
}

// natsDial is the one place the CLI turns its two NATS flags into a
// connection. It exists so the validation cannot be present in two
// subcommands and missing from the third: a half-configured keypair has to
// fail here, before the dial, or the operator gets a connection timeout
// instead of the sentence naming the flag they forgot.
func natsDial(u secret.NatsURL, tc natstls.Config) (*nats.Conn, error) {
	if err := tc.Validate(); err != nil {
		return nil, fmt.Errorf("nats tls: %w", err)
	}
	return natsutil.Connect(u, tc)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "-version", "--version":
		fmt.Println(buildinfo.Line("vantage"))
		return
	case "streams":
		err = cmdStreams(os.Args[2:])
	case "debug":
		err = cmdDebug(os.Args[2:])
	case "bmpgen":
		err = cmdBmpgen(os.Args[2:])
	case "loadgen":
		err = cmdLoadgen(os.Args[2:])
	case "capture":
		err = cmdCapture(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "reparse":
		err = cmdReparse(os.Args[2:])
	case "purge":
		err = cmdPurge(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		reportError(os.Stderr, err)
		os.Exit(1)
	}
}

// reportError prints a command's error. An error can quote text a server or
// a router supplied, so it goes through escapeControl; tab and newline are
// kept because this CLI's own errors use them for layout, and the one place
// server text enters an error (apiSource.do) has already escaped those.
func reportError(w io.Writer, err error) {
	fmt.Fprintln(w, "vantage:", escapeControl(err.Error(), true))
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  vantage -version
  vantage streams init [-nats URL] [-nats-ca FILE] [-nats-cert FILE] [-nats-key FILE] [-partitions N] [-replicas N] [-ls-replicas N] [-allow-lower-replicas] [-routes-max-bytes N] [-raw-max-bytes N]
  vantage debug [-nats URL] [-nats-ca FILE] [-nats-cert FILE] [-nats-key FILE] [-filter SUBJ] [-from-start]
  vantage bmpgen [-target HOST:PORT] [-router NAME] [-peers N] [-updates N] [-vendor DESCR]
  vantage loadgen [-target HOST:PORT] [-routers N] [-peers N] [-prefixes N] [-nlri-per-update N] [-aspath N] [-hold DURATION]
  vantage capture -router IP -o FILE (-window DURATION | -since DURATION) [-nats URL] [-nats-ca FILE] [-nats-cert FILE] [-nats-key FILE] [-collector ADDR] [-max-bytes N] [-replay-timeout DURATION]
  vantage reparse [-json] FILE
  vantage query routers [-api URL] [-token T] [-dsn DSN] [-stale-after DURATION] [-o table|json]
  vantage query peers [-router IP] [...]
  vantage query routes (-prefix P | -covers ADDR | -origin-asn N | -through-asn N | -community C) [-router IP] [-peer IP] [-rib VIEW] [...]
  vantage query rib -router IP -peer IP [-family unicast|vpn|evpn] [-rib VIEW] [-limit N] [...]
  vantage query ls (nodes|links|prefixes) [-router IP] [-peer IP] [-rib VIEW] [-protocol P] [-area N] [-asn N] [-state live|withdrawn|any] [-node K] [-local-node K] [-remote-node K] [-prefix P] [-covers ADDR]
  vantage purge -dsn DSN -collector ID [-router IP] [-dry-run] [-force] [-stale-after DURATION]`)
}
