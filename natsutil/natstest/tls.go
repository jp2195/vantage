package natstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TLSMaterial is a certificate authority, and the client keypair it signed,
// written to PEM files under a test's temporary directory. The file names
// match the keys cert-manager puts in the Secret it issues -- ca.crt,
// tls.crt, tls.key -- so a test reads the same shape the chart mounts.
//
// Everything here is generated per test. Nothing is committed: a committed
// certificate expires, and a test that starts failing on a date nobody chose
// is worse than no test.
type TLSMaterial struct {
	// URL is the embedded server's client URL. It is empty for material
	// from NewCA, which has no server attached.
	URL string

	CAFile   string
	CertFile string
	KeyFile  string

	// serverLog is what RunTLS's server logged at error level. It is nil for
	// NewCA material, which has no server, and RunTLSWithFiles returns only
	// a URL.
	serverLog *serverLog
}

// NewCA generates a self-signed authority and one client keypair it signed,
// with no server. It exists for the rejection cases: material from an
// authority the server under test has never heard of.
func NewCA(t *testing.T) TLSMaterial {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey := selfSignedCA(t)

	m := TLSMaterial{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
	}
	writePEM(t, m.CAFile, "CERTIFICATE", caCert.Raw)
	issueLeaf(t, caCert, caKey, m.CertFile, m.KeyFile, "vantage-test-client", nil)
	return m
}

// RunTLS starts an embedded JetStream nats-server that REQUIRES a client
// certificate, and returns its URL along with a CA bundle and client keypair
// that satisfy it. Unlike RunJSConn it connects nothing: most of what this
// fixture exists for is connections that must be REFUSED, and those need the
// server without a client.
//
// The server and its store directory are torn down via t.Cleanup.
func RunTLS(t *testing.T) TLSMaterial {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey := selfSignedCA(t)

	m := TLSMaterial{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
	}
	writePEM(t, m.CAFile, "CERTIFICATE", caCert.Raw)
	issueLeaf(t, caCert, caKey, m.CertFile, m.KeyFile, "vantage-test-client", nil)

	srvCert := filepath.Join(dir, "server.crt")
	srvKey := filepath.Join(dir, "server.key")
	// 127.0.0.1 has to be in the server certificate's SAN list and in
	// Options.Host below, and the two have to agree: the client dials
	// srv.ClientURL(), and a certificate that does not cover that address
	// fails hostname verification before anything about client certificates
	// is reached -- which would make every mTLS case here pass for the wrong
	// reason.
	issueLeaf(t, caCert, caKey, srvCert, srvKey, "localhost", []net.IP{net.ParseIP("127.0.0.1")})

	m.URL, m.serverLog = runTLSServer(t, m.CAFile, srvCert, srvKey)
	return m
}

// RunTLSWithFiles starts an embedded JetStream nats-server using certificate
// material already on disk, requiring and verifying a client certificate
// signed by caFile. It returns the client URL.
//
// It exists so a test can run the server against material this package did
// NOT generate -- specifically, deploy/nats-tls/gen-certs.sh's output, which
// is the only way to find out whether the shipped script produces a
// certificate a NATS server will actually accept.
func RunTLSWithFiles(t *testing.T, caFile, certFile, keyFile string) string {
	t.Helper()
	url, _ := runTLSServer(t, caFile, certFile, keyFile)
	return url
}

// runTLSServer is RunTLSWithFiles plus the server's error log.
func runTLSServer(t *testing.T, caFile, certFile, keyFile string) (string, *serverLog) {
	t.Helper()
	tlsCfg, err := server.GenTLSConfig(&server.TLSConfigOpts{
		CertFile: certFile,
		KeyFile:  keyFile,
		CaFile:   caFile,
		Verify:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := &server.Options{
		Host:       "127.0.0.1",
		Port:       -1,
		JetStream:  true,
		StoreDir:   t.TempDir(),
		TLS:        true,
		TLSConfig:  tlsCfg,
		TLSVerify:  true,
		TLSTimeout: 5,
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	log := &serverLog{}
	srv.SetLogger(log, false, false)
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL(), log
}

// serverLog is a nats-server Logger that keeps error-level lines and drops
// the rest. A test reads it to learn why the SERVER refused a connection,
// which is the only deterministic side of a TLS 1.3 client-certificate
// failure: see TestRunTLSRequiresAClientCertificate.
type serverLog struct {
	mu     sync.Mutex
	errors []string
}

func (l *serverLog) Errorf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf(format, v...))
}

func (l *serverLog) Fatalf(format string, v ...any) { l.Errorf(format, v...) }
func (*serverLog) Noticef(string, ...any)           {}
func (*serverLog) Warnf(string, ...any)             {}
func (*serverLog) Debugf(string, ...any)            {}
func (*serverLog) Tracef(string, ...any)            {}

// handshakeErrors returns every "TLS handshake error" line logged so far.
func (l *serverLog) handshakeErrors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.errors {
		if strings.Contains(e, "TLS handshake error") {
			out = append(out, e)
		}
	}
	return out
}

// RunJSConnTLS is RunTLS plus a connected client and a JetStream context --
// the happy path, for tests that need to publish and consume over mTLS
// rather than watch a connection be refused.
func RunJSConnTLS(t *testing.T) (*nats.Conn, jetstream.JetStream, TLSMaterial) {
	t.Helper()
	m := RunTLS(t)
	nc, err := nats.Connect(m.URL, nats.RootCAs(m.CAFile), nats.ClientCert(m.CertFile, m.KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return nc, js, m
}

func selfSignedCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "vantage-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func issueLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey,
	certFile, keyFile, cn string, ips []net.IP) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// Both usages on every leaf: the server certificate needs
		// ServerAuth, the client certificates need ClientAuth, and giving
		// each leaf both keeps one issuing path instead of two.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: ips,
		DNSNames:    []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, certFile, "CERTIFICATE", der)

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
