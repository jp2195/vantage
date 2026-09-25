package natstest

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestRunTLSRequiresAClientCertificate proves the fixture is actually
// enforcing mTLS rather than merely offering TLS. Every rejection test in
// natsutil depends on this server refusing an unauthenticated client, so a
// fixture that quietly accepted one would turn all of them green for the
// wrong reason.
//
// The evidence is the SERVER's log line, not the client's error. Under TLS
// 1.3 the client's handshake finishes before the server has looked at the
// client certificate, so nats.go goes on to write CONNECT. The server then
// refuses the empty certificate with an alert and closes, and if CONNECT is
// already in its receive buffer the kernel answers with RST, which throws
// away the alert still unread on the client side. The client then reports
// "connection reset by peer" (1 connect in 300 without -race, far more
// often with it) instead of "tls: certificate required". Which of the two
// it sees is a race, and it says nothing about why the connection failed.
// The server always logs its reason before it closes.
func TestRunTLSRequiresAClientCertificate(t *testing.T) {
	m := RunTLS(t)

	// The control: the same server, the same CA, with the client keypair.
	ok, err := nats.Connect(m.URL, nats.RootCAs(m.CAFile), nats.ClientCert(m.CertFile, m.KeyFile))
	if err != nil {
		t.Fatalf("connect with valid material: %v", err)
	}
	ok.Close()
	if errs := m.serverLog.handshakeErrors(); len(errs) != 0 {
		t.Fatalf("the server logged a TLS handshake error for the connection that presented "+
			"a valid certificate, so nothing below can be pinned on the missing one: %q", errs)
	}

	// The same connection differing in exactly one thing: no client cert.
	bad, err := nats.Connect(m.URL, nats.RootCAs(m.CAFile))
	if err == nil {
		bad.Close()
		t.Fatal("connected with no client certificate; the fixture is not verifying clients")
	}

	got := waitForHandshakeError(t, m.serverLog, 5*time.Second)
	if !strings.Contains(got, "client didn't provide a certificate") {
		t.Errorf("the server refused the connection for another reason: %q (client saw %v)", got, err)
	}
}

// waitForHandshakeError returns the server's one TLS handshake error line,
// polling because the client can return before the server has logged.
func waitForHandshakeError(t *testing.T, l *serverLog, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if errs := l.handshakeErrors(); len(errs) > 0 {
			if len(errs) > 1 {
				t.Errorf("the server logged %d handshake errors for one refused connection: %q", len(errs), errs)
			}
			return errs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server logged no TLS handshake error within %s of refusing the connection", within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNewCAIsUnrelatedToRunTLS guards the other half of the fixture: the
// rejection tests need material from a DIFFERENT authority, and two CAs that
// happened to trust each other would make those tests vacuous.
func TestNewCAIsUnrelatedToRunTLS(t *testing.T) {
	m := RunTLS(t)
	foreign := NewCA(t)

	nc, err := nats.Connect(m.URL, nats.RootCAs(m.CAFile),
		nats.ClientCert(foreign.CertFile, foreign.KeyFile))
	if err == nil {
		nc.Close()
		t.Fatal("the server accepted a client certificate from NewCA's authority")
	}
}
