// Package natstest runs an embedded JetStream server for tests. It exists so
// natsutil's tests exercise a real nats-server binary (subject
// transforms, partition functions, dedup windows) rather than a mock —
// nothing here should be taken as a substitute for testing against the
// server's actual behavior.
package natstest

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// RunJSConn starts an embedded nats-server with JetStream enabled on a random
// port and a temporary store directory, connects to it, and returns both the
// raw *nats.Conn (for tests that need to publish/subscribe directly, e.g. a
// future debug-CLI test) and the jetstream.JetStream context built from it.
// The server and connection are shut down via t.Cleanup.
func RunJSConn(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	srv := RunServer(t)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return nc, js
}

// RunJS is the common case: an embedded JetStream context with no need for
// the underlying connection.
func RunJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, js := RunJSConn(t)
	return js
}
