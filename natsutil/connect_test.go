package natsutil_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/natstls"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/natsutil/natstest"
	"github.com/jp2195/vantage/secret"
)

// TestConnectTLS is a falsification set, run as one table against
// ONE server so every rejection differs from the accepted row in exactly one
// field. A rejection test that stands alone proves only that something went
// wrong; paired with its control it proves what.
func TestConnectTLS(t *testing.T) {
	good := natstest.RunTLS(t)
	foreign := natstest.NewCA(t)

	for _, c := range []struct {
		name        string
		cfg         natstls.Config
		wantConnect bool
	}{
		{
			name:        "valid CA and valid client certificate",
			cfg:         natstls.Config{CAFile: good.CAFile, CertFile: good.CertFile, KeyFile: good.KeyFile},
			wantConnect: true,
		},
		{
			name:        "no client certificate",
			cfg:         natstls.Config{CAFile: good.CAFile},
			wantConnect: false,
		},
		{
			name:        "client certificate from a different CA",
			cfg:         natstls.Config{CAFile: good.CAFile, CertFile: foreign.CertFile, KeyFile: foreign.KeyFile},
			wantConnect: false,
		},
		{
			// The client's own trust store is what refuses here: the
			// server's certificate was signed by good's authority, and
			// this row trusts only foreign's.
			name:        "server certificate our CA did not sign",
			cfg:         natstls.Config{CAFile: foreign.CAFile, CertFile: good.CertFile, KeyFile: good.KeyFile},
			wantConnect: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			nc, err := natsutil.Connect(secret.NewNatsURL(good.URL), c.cfg)
			if c.wantConnect {
				if err != nil {
					t.Fatalf("Connect() = %v, want a connection", err)
				}
				nc.Close()
				return
			}
			if err == nil {
				nc.Close()
				t.Fatal("Connect() succeeded, want it refused")
			}
			t.Logf("refused with: %v", err)
		})
	}
}

// TestConnectTLSPublishAndConsume: a handshake that completes but cannot
// publish is not a working transport.
func TestConnectTLSPublishAndConsume(t *testing.T) {
	_, _, m := natstest.RunJSConnTLS(t)

	nc, err := natsutil.Connect(secret.NewNatsURL(m.URL),
		natstls.Config{CAFile: m.CAFile, CertFile: m.CertFile, KeyFile: m.KeyFile})
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TLSTEST",
		Subjects: []string{"tlstest.>"},
	}); err != nil {
		t.Fatalf("create stream over mTLS: %v", err)
	}
	if _, err := js.Publish(ctx, "tlstest.one", []byte("hello")); err != nil {
		t.Fatalf("publish over mTLS: %v", err)
	}
	s, err := js.Stream(ctx, "TLSTEST")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("stream holds %d messages after one publish, want 1", info.State.Msgs)
	}
}

// TestConnectWithoutTLSConfigIsPlaintext pins the row most likely to be
// quietly broken, and the one the dev stack rests on. A
// zero natstls.Config against a server with no TLS at all must behave exactly
// as it did before this parameter existed.
//
// It is also the test that catches the most plausible mistake in natstls:
// building nats.RootCAs unconditionally. That option sets Options.Secure, so
// the connection below would demand TLS from a server that offers none.
func TestConnectWithoutTLSConfigIsPlaintext(t *testing.T) {
	plain, _ := natstest.RunJSConn(t)
	url := plain.ConnectedUrl()

	nc, err := natsutil.Connect(secret.NewNatsURL(url), natstls.Config{})
	if err != nil {
		t.Fatalf("Connect() with no TLS config against a plaintext server: %v", err)
	}
	defer nc.Close()

	// NOT nc.TLSRequired(): that returns nc.info.TLSRequired (nats.go
	// v1.49.0:6167), which is what the SERVER advertised -- false against a
	// plaintext server whatever Connect did with the config, so the assertion
	// could never fail. TLSConnectionState inspects the connection this
	// client actually holds and returns ErrConnectionNotTLS when it is not a
	// *tls.Conn (nats.go:2352), which is the question being asked.
	if _, err := nc.TLSConnectionState(); !errors.Is(err, nats.ErrConnectionNotTLS) {
		t.Errorf("TLSConnectionState() returned %v, want ErrConnectionNotTLS -- "+
			"the connection negotiated TLS against a plaintext server", err)
	}
	if err := nc.Publish("plain.one", []byte("hello")); err != nil {
		t.Fatalf("publish over plaintext: %v", err)
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush over plaintext: %v", err)
	}
}

// TestConnectRejectsAHalfKeypair: a misconfiguration must not reach the
// network. The URL below points at a port nothing listens on, so a dial
// error would be the failure if Validate did not run first.
func TestConnectRejectsAHalfKeypair(t *testing.T) {
	_, err := natsutil.Connect(secret.NewNatsURL("nats://127.0.0.1:1"),
		natstls.Config{CertFile: "/tls.crt"})
	if err == nil {
		t.Fatal("Connect() accepted cert_file with no key_file")
	}
	if !strings.Contains(err.Error(), "key_file") {
		t.Errorf("Connect() failed with %q, which is a dial error rather than "+
			"the configuration error -- validation is running too late", err)
	}
}
