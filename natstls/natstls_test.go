package natstls

import (
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/jp2195/vantage/natsutil/natstest"
)

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     Config
		wantErr string // "" means valid
	}{
		{"nothing set", Config{}, ""},
		{"ca only", Config{CAFile: "/ca.crt"}, ""},
		{"full mutual set", Config{CAFile: "/ca.crt", CertFile: "/tls.crt", KeyFile: "/tls.key"}, ""},
		{"cert without key", Config{CertFile: "/tls.crt"}, "key_file"},
		{"key without cert", Config{KeyFile: "/tls.key"}, "cert_file"},
		{"cert and key, no ca", Config{CertFile: "/tls.crt", KeyFile: "/tls.key"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("Validate() = %q, which does not name %q -- an operator "+
					"cannot act on an error that does not say which field is missing",
					err, c.wantErr)
			}
		})
	}
}

// TestEmptyIsPlaintext is the guard on the row the whole dev stack rests on.
// A zero Config must produce NO options at all: nats.RootCAs and
// nats.ClientCert each set Options.Secure, so a single option added
// unconditionally turns every plaintext install into a TLS install.
func TestEmptyIsPlaintext(t *testing.T) {
	var c Config
	if !c.Empty() {
		t.Error("zero Config reports Empty() == false")
	}
	opts, err := c.Options()
	if err != nil {
		t.Fatalf("Options() on a zero Config: %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("Options() on a zero Config returned %d options, want 0", len(opts))
	}

	// Stronger than counting: apply whatever came back and assert the
	// connection would still be insecure.
	var o nats.Options
	for _, fn := range opts {
		if err := fn(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.Secure || o.TLSConfig != nil {
		t.Error("a zero Config produced options that request TLS")
	}
}

// TestOptionsLoadTheFilesNamed applies the options and then INVOKES the
// callbacks nats.go stored, which is what proves the right paths were wired
// rather than merely that two options were appended.
func TestOptionsLoadTheFilesNamed(t *testing.T) {
	m := natstest.NewCA(t)
	c := Config{CAFile: m.CAFile, CertFile: m.CertFile, KeyFile: m.KeyFile}

	opts, err := c.Options()
	if err != nil {
		t.Fatalf("Options(): %v", err)
	}
	var o nats.Options
	for _, fn := range opts {
		if err := fn(&o); err != nil {
			t.Fatalf("applying option: %v", err)
		}
	}
	if o.RootCAsCB == nil {
		t.Fatal("no RootCAs callback was set")
	}
	if o.TLSCertCB == nil {
		t.Fatal("no client certificate callback was set")
	}
	pool, err := o.RootCAsCB()
	if err != nil {
		t.Fatalf("RootCAs callback: %v", err)
	}
	if len(pool.Subjects()) == 0 { //nolint:staticcheck // Subjects is the only count available
		t.Error("the CA pool loaded no certificates")
	}
	cert, err := o.TLSCertCB()
	if err != nil {
		t.Fatalf("client certificate callback: %v", err)
	}
	if cert.Leaf == nil || cert.Leaf.Subject.CommonName != "vantage-test-client" {
		t.Errorf("loaded client certificate %+v, want the one NewCA issued", cert.Leaf)
	}
}

// TestOptionsRejectsAHalfKeypairBeforeDialing: Validate runs inside Options,
// so a misconfiguration cannot reach nats.Connect and come back as a
// confusing dial error.
func TestOptionsRejectsAHalfKeypair(t *testing.T) {
	_, err := Config{CertFile: "/tls.crt"}.Options()
	if err == nil {
		t.Fatal("Options() accepted cert_file with no key_file")
	}
}

// TestOptionsReportsAMissingFile: nats.RootCAs and nats.ClientCert each
// validate eagerly while being applied (nats.go v1.54.0), so a path that does
// not exist is a startup failure naming the path, not a handshake failure
// later.
func TestOptionsReportsAMissingFile(t *testing.T) {
	m := natstest.NewCA(t)
	var o nats.Options
	opts, err := Config{CAFile: m.CAFile + ".nope"}.Options()
	if err != nil {
		t.Fatalf("Options() itself failed: %v", err)
	}
	for _, fn := range opts {
		err = fn(&o)
	}
	if err == nil {
		t.Fatal("applying RootCAs for a nonexistent file succeeded")
	}
	if !strings.Contains(err.Error(), "rootCA") {
		t.Errorf("error %q does not identify the CA file as the problem", err)
	}
}
