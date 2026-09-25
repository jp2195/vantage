package natstls

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/natsutil/natstest"
)

// runGenCerts runs the shipped script into a fresh directory and returns
// that directory plus everything the script printed -- the values block an
// operator copies into their values.yaml is part of the artifact, so it is
// part of what gets asserted.
//
// 127.0.0.1 goes into the server certificate's SAN list because this test
// dials the embedded server by address -- without it the TLS handshake fails
// on hostname verification before anything this test cares about is reached.
// It is the same flag an operator uses to debug through `kubectl port-forward`.
//
// The openssl skip stays for a laptop without it, but CI is the only place
// these tests are load-bearing: a silent skip there would retire the shipped
// script's ONLY coverage without printing a word. So in CI the missing
// binary is a failure, not a skip.
func runGenCerts(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("openssl is not installed, and in CI that silently retires "+
				"every assertion about deploy/nats-tls/gen-certs.sh: %v", err)
		}
		t.Skip("openssl is not installed")
	}
	script, err := filepath.Abs(filepath.Join("..", "deploy", "nats-tls", "gen-certs.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(script,
		"--release", "foo", "--namespace", "vtest",
		"--out", dir, "--extra-san", "IP:127.0.0.1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gen-certs.sh failed: %v\n%s", err, out)
	}
	return dir, string(out)
}

// The Secret names the script mints, and the three data keys the chart's
// nats_tls block names inside the mount (ca_file/cert_file/key_file under
// vantage.natsTLSDir, see statefulset-collector.yaml). They are spelled out
// here rather than read back out of the script so that a change to either
// side has to be made twice, deliberately.
const (
	serverSecret    = "foo-nats-server-tls"
	collectorSecret = "foo-vantage-nats-collector-tls"
	writerSecret    = "foo-vantage-nats-writer-tls"
)

var secretDataKeys = []string{"ca.crt", "tls.crt", "tls.key"}

// k8sSecret is only the part of a Secret this test has an opinion about.
type k8sSecret struct {
	Kind     string `yaml:"kind"`
	Type     string `yaml:"type"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

// parseSecrets decodes every document in secrets.yaml, keyed by Secret name.
func parseSecrets(t *testing.T, path string) map[string]k8sSecret {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("gen-certs.sh wrote no secrets.yaml, which is the file the "+
			"README tells operators to kubectl apply: %v", err)
	}
	defer f.Close()

	got := map[string]k8sSecret{}
	dec := yaml.NewDecoder(f)
	for {
		var sec k8sSecret
		err := dec.Decode(&sec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("secrets.yaml does not parse as YAML: %v", err)
		}
		if sec.Kind == "" {
			continue // the leading `---` decodes as an empty document
		}
		if sec.Kind != "Secret" {
			t.Errorf("secrets.yaml carries a %q document; kubectl apply would "+
				"create something the chart does not mount", sec.Kind)
			continue
		}
		got[sec.Metadata.Name] = sec
	}
	return got
}

// writeMount base64-decodes one Secret into a fresh directory named exactly
// as the kubelet projects it -- ca.crt, tls.crt, tls.key as files. Every
// later assertion runs against THAT, so the test reads what the pod reads.
func writeMount(t *testing.T, sec k8sSecret) string {
	t.Helper()
	dir := t.TempDir()
	for key, b64 := range sec.Data {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("%s: %s is not valid base64: %v", sec.Metadata.Name, key, err)
		}
		if err := os.WriteFile(filepath.Join(dir, key), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestGenCertsSecretsManifestIsWhatTheChartMounts closes the
// script -> Secret -> chart -> daemon seam, which nothing else in this
// repo touches: every other test here reads the loose .crt/.key files the
// script leaves behind, and NOTHING reads secrets.yaml -- the primary
// artifact of the default issuance path, the one file the README says to
// kubectl apply.
//
// Three defects live in that gap and none of them has a symptom before the
// cluster: a renamed data key (ca.crt -> ca.pem) CrashLoops both daemons on
// a file that is not there; a Secret name drifting from the values block the
// script prints CrashLoops nothing and leaves both pods in
// ContainerCreating forever; and the wrong file base64'd into a field fails
// only at the handshake. So the assertions are names, exact key sets, and
// then a real mTLS handshake driven from the DECODED Secret contents rather
// than from the loose files beside them.
func TestGenCertsSecretsManifestIsWhatTheChartMounts(t *testing.T) {
	dir, printed := runGenCerts(t)
	secrets := parseSecrets(t, filepath.Join(dir, "secrets.yaml"))

	want := []string{serverSecret, collectorSecret, writerSecret}
	if names := slices.Sorted(maps.Keys(secrets)); !slices.Equal(names, slices.Sorted(slices.Values(want))) {
		t.Fatalf("secrets.yaml creates %v, want exactly %v -- the chart mounts "+
			"the names it was given and waits forever for anything else", names, want)
	}

	for _, name := range want {
		sec := secrets[name]
		if keys := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(keys, secretDataKeys) {
			t.Errorf("Secret %s carries data keys %v, want exactly %v -- the "+
				"chart's nats_tls block names those three paths inside the "+
				"mount, and a daemon pointed at a file the Secret does not "+
				"carry CrashLoops", name, keys, secretDataKeys)
		}
		if sec.Metadata.Namespace != "vtest" {
			t.Errorf("Secret %s lands in namespace %q, not the --namespace it was given", name, sec.Metadata.Namespace)
		}
		// The values block the script prints is what an operator pastes; if
		// it drifts from the manifest the script just wrote, the chart mounts
		// a Secret that does not exist and both pods sit in
		// ContainerCreating with no error anywhere.
		if !strings.Contains(printed, name) {
			t.Errorf("gen-certs.sh mints Secret %s but never names it in the "+
				"values block it prints, so nothing would mount it:\n%s", name, printed)
		}
	}
	if t.Failed() {
		return
	}

	// The handshake, from the mounts and nothing else. A wrong file in a
	// field -- server.key under tls.key on the collector, say -- parses as
	// PEM, satisfies every assertion above, and fails only here.
	srvDir := writeMount(t, secrets[serverSecret])
	url := natstest.RunTLSWithFiles(t,
		filepath.Join(srvDir, "ca.crt"),
		filepath.Join(srvDir, "tls.crt"),
		filepath.Join(srvDir, "tls.key"))

	for name, svc := range map[string]string{collectorSecret: "collector", writerSecret: "writer"} {
		t.Run(svc, func(t *testing.T) {
			mount := writeMount(t, secrets[name])
			cfg := Config{
				CAFile:   filepath.Join(mount, "ca.crt"),
				CertFile: filepath.Join(mount, "tls.crt"),
				KeyFile:  filepath.Join(mount, "tls.key"),
			}
			opts, err := cfg.Options()
			if err != nil {
				t.Fatalf("Options(): %v", err)
			}
			nc, err := nats.Connect(url, opts...)
			if err != nil {
				t.Fatalf("Secret %s does not hold material a NATS server built "+
					"from Secret %s will accept: %v", name, serverSecret, err)
			}
			nc.Close()
		})
	}

	// The falsifying partner, the same one the loose-file test uses: a second
	// run's CA is unrelated, so its collector Secret must be refused. Without
	// this row every assertion above would hold against a server verifying
	// nothing.
	otherDir, _ := runGenCerts(t)
	otherMount := writeMount(t, parseSecrets(t, filepath.Join(otherDir, "secrets.yaml"))[collectorSecret])
	cfg := Config{
		CAFile:   filepath.Join(srvDir, "ca.crt"),
		CertFile: filepath.Join(otherMount, "tls.crt"),
		KeyFile:  filepath.Join(otherMount, "tls.key"),
	}
	opts, err := cfg.Options()
	if err != nil {
		t.Fatalf("Options(): %v", err)
	}
	if nc, err := nats.Connect(url, opts...); err == nil {
		nc.Close()
		t.Fatal("a client Secret from a different CA was accepted")
	}
}

// TestGenCertsMaterialWorksOverMTLS is the whole point of the script: a NATS
// server running its server certificate, requiring client certificates,
// accepting the collector's and the writer's. Nothing short of a handshake
// proves that -- a wrong key usage or a missing SAN parses perfectly and
// fails only in the cluster.
func TestGenCertsMaterialWorksOverMTLS(t *testing.T) {
	dir, _ := runGenCerts(t)
	url := natstest.RunTLSWithFiles(t,
		filepath.Join(dir, "ca.crt"),
		filepath.Join(dir, "server.crt"),
		filepath.Join(dir, "server.key"))

	for _, svc := range []string{"collector", "writer"} {
		t.Run(svc, func(t *testing.T) {
			cfg := Config{
				CAFile:   filepath.Join(dir, "ca.crt"),
				CertFile: filepath.Join(dir, svc+".crt"),
				KeyFile:  filepath.Join(dir, svc+".key"),
			}
			opts, err := cfg.Options()
			if err != nil {
				t.Fatalf("Options(): %v", err)
			}
			nc, err := nats.Connect(url, opts...)
			if err != nil {
				t.Fatalf("%s material was refused by a server using the same CA: %v", svc, err)
			}
			nc.Close()
		})
	}

	// The falsifying partner. A second run mints an unrelated CA, so its
	// client certificate must be refused by the first run's server. Without
	// this row the test above would pass just as happily against a server
	// that verified nothing at all.
	other, _ := runGenCerts(t)
	cfg := Config{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(other, "collector.crt"),
		KeyFile:  filepath.Join(other, "collector.key"),
	}
	opts, err := cfg.Options()
	if err != nil {
		t.Fatalf("Options(): %v", err)
	}
	if nc, err := nats.Connect(url, opts...); err == nil {
		nc.Close()
		t.Fatal("a client certificate from a different CA was accepted")
	}
}

// TestGenCertsServerSANsCoverTheServiceNames: the names the daemons actually
// dial in a cluster. A missing entry here is invisible to every test that
// connects by address, and shows up only as a handshake failure in the
// cluster -- so it is asserted against the parsed certificate directly.
func TestGenCertsServerSANsCoverTheServiceNames(t *testing.T) {
	dir, _ := runGenCerts(t)
	b, err := os.ReadFile(filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatal("server.crt is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"foo-nats",
		"foo-nats.vtest.svc",
		"foo-nats.vtest.svc.cluster.local",
		"*.foo-nats-headless.vtest.svc.cluster.local",
	} {
		if !slices.Contains(cert.DNSNames, want) {
			t.Errorf("server certificate SANs %v do not cover %q", cert.DNSNames, want)
		}
	}
}

// loadCert parses one PEM certificate file from a gen-certs.sh run.
func loadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s is not PEM", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// TestGenCertsServerCertVerifiesAsARoute checks the server certificate the
// way a cluster route uses it. The chart's cluster routes are
// tls://<release>-nats-<i>.<release>-nats-headless.<ns>.svc.cluster.local
// (routeURLs.useFQDN), and the server certificate is also the route
// certificate, so each end of a route verifies it: the dialing server as a
// server certificate for that name, the accepting server as a client
// certificate. Both are verified here against the route names the chart
// renders for release "foo" in namespace "vtest", with Go's verifier --
// the one nats-server runs. A name outside the headless Service is the
// falsifying row: without it a certificate valid for any name would pass.
func TestGenCertsServerCertVerifiesAsARoute(t *testing.T) {
	dir, _ := runGenCerts(t)
	roots := x509.NewCertPool()
	roots.AddCert(loadCert(t, filepath.Join(dir, "ca.crt")))
	srv := loadCert(t, filepath.Join(dir, "server.crt"))

	for i := range 3 {
		name := fmt.Sprintf("foo-nats-%d.foo-nats-headless.vtest.svc.cluster.local", i)
		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
			if _, err := srv.Verify(x509.VerifyOptions{
				DNSName: name, Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage},
			}); err != nil {
				t.Errorf("the server certificate does not verify for route %s with usage %v: %v "+
					"-- the NATS servers could not form a cluster", name, usage, err)
			}
		}
	}
	if _, err := srv.Verify(x509.VerifyOptions{
		DNSName: "foo-nats-0.other-headless.vtest.svc.cluster.local", Roots: roots,
	}); err == nil {
		t.Error("the server certificate verifies for a name outside foo-nats-headless; the SAN check above proves nothing")
	}
}

// startRouteServer runs one nats-server whose cluster block is the one the
// chart renders into nats.conf -- cluster.tls with ca_file (tlsCA), the
// server certificate, and verify: true -- parsed by nats-server's own config
// loader, which is where route TLS picks up its strict two-way verification.
// certDir holds the gen-certs.sh output to use. seed is the route to dial,
// empty for the first server.
func startRouteServer(t *testing.T, certDir, name, seed string) *server.Server {
	t.Helper()
	routes := ""
	if seed != "" {
		routes = fmt.Sprintf("routes: [%q]", seed)
	}
	conf := fmt.Sprintf(`
server_name: %q
listen: "127.0.0.1:-1"
cluster {
  name: "foo-nats"
  listen: "127.0.0.1:-1"
  %s
  tls {
    ca_file: %q
    cert_file: %q
    key_file: %q
    verify: true
  }
}
`, name, routes,
		filepath.Join(certDir, "ca.crt"),
		filepath.Join(certDir, "server.crt"),
		filepath.Join(certDir, "server.key"))
	path := filepath.Join(t.TempDir(), "nats.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("nats-server refuses the route TLS config: %v\n%s", err, conf)
	}
	opts.NoSigs = true
	opts.NoLog = os.Getenv("NATS_TEST_LOG") == ""
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.NoLog {
		srv.ConfigureLogger()
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatalf("nats server %s not ready", name)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// routedPeers returns the distinct servers s has a route to. Not NumRoutes:
// nats-server pools several connections per peer (pool_size, plus a pinned
// route for the system account), so the connection count is a multiple of
// the peer count and says nothing on its own.
func routedPeers(t *testing.T, s *server.Server) []string {
	t.Helper()
	rz, err := s.Routez(nil)
	if err != nil {
		t.Fatal(err)
	}
	var peers []string
	for _, r := range rz.Routes {
		if !slices.Contains(peers, r.RemoteName) {
			peers = append(peers, r.RemoteName)
		}
	}
	slices.Sort(peers)
	return peers
}

// TestGenCertsServerCertFormsARouteCluster forms a real three-server
// cluster whose routes run over mutual TLS with the server certificate --
// the configuration the chart renders -- and checks that every server
// reaches the other two. A route needs clientAuth on the certificate as
// well as serverAuth, because the dialing server presents it as a client
// certificate; issued serverAuth-only, no route here ever comes up.
//
// Routes dial 127.0.0.1, which runGenCerts puts in the SAN list; the names
// the chart dials are TestGenCertsServerCertVerifiesAsARoute's job. The
// falsifying row is a fourth server holding a certificate from an unrelated
// CA: it must never be admitted, or the cluster would take any peer.
func TestGenCertsServerCertFormsARouteCluster(t *testing.T) {
	dir, _ := runGenCerts(t)

	first := startRouteServer(t, dir, "s0", "")
	seed := "tls://" + first.ClusterAddr().String()
	servers := []*server.Server{first,
		startRouteServer(t, dir, "s1", seed),
		startRouteServer(t, dir, "s2", seed)}

	deadline := time.Now().Add(15 * time.Second)
	for {
		ok := true
		for _, s := range servers {
			if len(routedPeers(t, s)) != 2 {
				ok = false
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			for _, s := range servers {
				t.Errorf("%s routes to %v, want the other two", s.Name(), routedPeers(t, s))
			}
			t.Fatal("the three servers never formed a cluster over mutual TLS with the gen-certs.sh server certificate")
		}
		time.Sleep(100 * time.Millisecond)
	}

	otherDir, _ := runGenCerts(t)
	intruder := startRouteServer(t, otherDir, "intruder", seed)
	time.Sleep(3 * time.Second)
	if peers := routedPeers(t, intruder); len(peers) != 0 {
		t.Fatalf("a server holding a certificate from another CA routed into the cluster, to %v", peers)
	}
	for _, s := range servers {
		if peers := routedPeers(t, s); slices.Contains(peers, "intruder") || len(peers) != 2 {
			t.Errorf("%s routes to %v after the intruder tried to join, want the other two only", s.Name(), peers)
		}
	}
}

// TestGenCertsClientCertsAreNotServerCerts: a client certificate that also
// carries serverAuth is a certificate that can impersonate the broker. The
// script issues them separately, and this is what keeps that true.
func TestGenCertsClientCertsAreClientOnly(t *testing.T) {
	dir, _ := runGenCerts(t)
	for _, svc := range []string{"collector", "writer"} {
		b, err := os.ReadFile(filepath.Join(dir, svc+".crt"))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(b)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
			t.Errorf("%s certificate carries serverAuth; it could impersonate the NATS server", svc)
		}
		if !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
			t.Errorf("%s certificate does not carry clientAuth", svc)
		}
	}
}
