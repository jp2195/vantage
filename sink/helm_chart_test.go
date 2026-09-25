package sink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/collector"
)

// The Helm chart carries its own copy of every DDL file, because
// configmap-schema.yaml builds its ConfigMap with `.Files.Glob
// "files/schema/*.sql"` and .Files.Glob cannot read outside the chart
// directory -- so the chart cannot simply point at deploy/clickhouse/. The
// copies are therefore duplicates that drift silently, and the drift is not
// visible from either side: `helm template` renders a perfectly valid
// ConfigMap out of a stale set, and deploy/clickhouse/ has no idea a second
// copy exists.
//
// The drift is invisible on a FRESH install too, which is what makes it so
// quiet. 000-schema.sql creates every table at the current version in one
// shot, so a new cluster is correct whether or not the migrations were
// mirrored. Only an EXISTING cluster needs them, and there the schema Job is
// a no-op: schema.sql is all CREATE TABLE IF NOT EXISTS and its version row
// is guarded `WHERE (SELECT count() FROM schema_version) = 0`. A cluster
// that missed a migration therefore stays on the old version forever while
// reporting success, and vantage-writer and vantage-api then refuse to start
// against it (checkSchema, clickhouse.go) -- an upgrade that breaks only the
// clusters that already held data.
//
// This is the derived form on purpose: it reads whatever is in
// deploy/clickhouse/ rather than naming the files, so a migration 004 added
// without mirroring fails here the moment it lands, with no list to
// remember to update.
func TestHelmChartSchemaFilesMirrorDeployClickhouse(t *testing.T) {
	want := map[string][]byte{}

	// schema.sql travels under a 000- prefix because job-schema.yaml applies
	// /schema/*.sql in lexical order and the full DDL has to precede every
	// migration. That rename is the one piece of chart convention this test
	// hard-codes; everything else is derived.
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	want["000-schema.sql"] = ddl

	migs, err := filepath.Glob(filepath.Join("..", "deploy", "clickhouse", "migrations", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, m := range migs {
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		want[filepath.Base(m)] = body
	}

	// Without this the test would pass vacuously if the glob silently matched
	// nothing because the directory moved -- that would read as "the chart
	// mirrors everything", which is the exact reassurance this test exists to
	// deny. An EXISTING but empty directory is legitimate: it is the state at
	// the squashed version-1 baseline, and then the chart must hold
	// 000-schema.sql and nothing else, which assertChartDirMirrors checks.
	migDir := filepath.Join("..", "deploy", "clickhouse", "migrations")
	if fi, err := os.Stat(migDir); err != nil || !fi.IsDir() {
		t.Fatalf("%s is not a directory (%v), so this test is not asserting "+
			"what it claims", migDir, err)
	}

	assertChartDirMirrors(t,
		filepath.Join("..", "deploy", "helm", "vantage", "files", "schema"),
		want,
		"an already-deployed cluster would never receive it, would stay on the "+
			"old schema_version, and vantage-writer and vantage-api would then "+
			"refuse to start against it")
}

// The AS holder name dictionaries are the second copied tree, applied by the
// same Job from the same PodSpec (job-schema.yaml's second loop). They sit
// outside the schema_version sequence, so a stale copy here does not wedge
// the writer the way a missed migration does -- it degrades to AS numbers
// rendering without names, which is precisely the kind of quiet failure
// that has gone unnoticed here before.
func TestHelmChartDictionaryFilesMirrorDeployClickhouse(t *testing.T) {
	want := map[string][]byte{}
	for _, name := range []string{"asnames.sql", "asnames_meta.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", name))
		if err != nil {
			t.Fatalf("read %s: %v; both dictionary files have to be applied "+
				"together (see asnames_meta.sql's header)", name, err)
		}
		want[name] = body
	}
	assertChartDirMirrors(t,
		filepath.Join("..", "deploy", "helm", "vantage", "files", "dictionaries"),
		want,
		"the Job's second loop would apply only half the dictionary pair, and "+
			"AS numbers would render unnamed with nothing reporting an error")
}

// assertChartDirMirrors fails unless dir holds exactly want: no file missing,
// no file extra, and every one byte-identical. missingCosts states what a
// missing file actually breaks, which differs per tree -- a missed migration
// wedges the daemons, a missed dictionary half only loses the names. An extra file matters as much
// as a missing one -- job-schema.yaml applies everything the glob matches, so
// a leftover copy of a deleted migration would be re-applied to every cluster.
func assertChartDirMirrors(t *testing.T, dir string, want map[string][]byte, missingCosts string) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	got := map[string][]byte{}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		got[filepath.Base(p)] = body
	}

	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		body, ok := got[n]
		if !ok {
			t.Errorf("%s/%s is missing from the chart: %s. Run `make sync-helm-schema`",
				dir, n, missingCosts)
			continue
		}
		if !bytes.Equal(body, want[n]) {
			t.Errorf("%s/%s has drifted from its deploy/clickhouse/ original "+
				"(%d bytes in the chart, %d in the source): two clusters on "+
				"the same chart version would hold different shapes. Run "+
				"`make sync-helm-schema`",
				dir, n, len(body), len(want[n]))
		}
	}

	extra := make([]string, 0)
	for n := range got {
		if _, ok := want[n]; !ok {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		t.Errorf("%s/%s has no counterpart in deploy/clickhouse/: the schema "+
			"Job applies every file the glob matches, so a file left behind "+
			"here is re-applied to every cluster forever. Run "+
			"`make sync-helm-schema`", dir, n)
	}
}

// natsConfBare matches a nats.conf value that is not JSON: a $VARIABLE or a
// number with a unit suffix.
var natsConfBare = regexp.MustCompile(`(?m)(": )(\$[A-Z_]+|[0-9]+[A-Za-z]+)(,?)$`)

// natsRender is what the NATS-clustering assertions below read out of one
// `helm template` of the chart.
type natsRender struct {
	// natsReplicas is the NATS StatefulSet's spec.replicas.
	natsReplicas int
	// spread is its pod template's topologySpreadConstraints.
	spread []map[string]any
	// conf is nats.conf, decoded; nil without the bundled NATS.
	conf map[string]any
	// streams is the collector's stream configuration as the collector
	// itself computes it from the rendered collector.yaml: LoadConfig's
	// defaulting included, so LSReplicas is what the LS stream gets.
	streams collector.StreamsConfig
	// streamsRendered is whether collector.yaml carries a replicas line at
	// all, which distinguishes "the chart said 1" from "the chart said
	// nothing and the collector defaulted".
	replicasRendered bool
}

// helmTemplate renders the chart under release "foo" -- not "vantage", whose
// name collapses vantage.fullname and hides constructed-versus-literal name
// mismatches -- with the stand-in values every render needs plus args, and
// returns the manifests. A render failure is returned, not fatal, so a test
// can assert that a combination is refused.
func helmTemplate(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	base := []string{"template", "foo", filepath.Join("..", "deploy", "helm", "vantage"),
		"--namespace", "vantage",
		"--set", "clickhouse.externalHost=ch", "--set", "clickhouse.auth.password=ci",
		"--set", "nats.tls.collectorSecret=c-tls", "--set", "nats.tls.writerSecret=w-tls"}
	cmd := exec.CommandContext(t.Context(), "helm", append(base, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, lookErr := exec.LookPath("helm"); lookErr != nil {
			skipWithoutTool(t, fmt.Sprintf("helm is not installed: %v", lookErr))
		}
		if strings.Contains(stderr.String(), "found in Chart.yaml, but missing in charts/") {
			skipWithoutTool(t, "the chart's dependencies are not built: run "+
				"`helm dependency build deploy/helm/vantage`")
		}
		return nil, fmt.Errorf("%v: %s", err, stderr.String())
	}
	return out, nil
}

// renderNATS decodes the pieces natsRender names. A render failure is
// returned, not fatal, so a test can assert that a combination is refused.
func renderNATS(t *testing.T, args ...string) (natsRender, error) {
	t.Helper()
	out, err := helmTemplate(t, args...)
	if err != nil {
		return natsRender{}, err
	}

	var r natsRender
	var sawCollector bool
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
			Spec struct {
				Replicas int `yaml:"replicas"`
				Template struct {
					Spec struct {
						TopologySpreadConstraints []map[string]any `yaml:"topologySpreadConstraints"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the render is not YAML: %v", err)
		}
		switch {
		case doc.Kind == "StatefulSet" && doc.Metadata.Name == "foo-nats":
			r.natsReplicas = doc.Spec.Replicas
			r.spread = doc.Spec.Template.Spec.TopologySpreadConstraints
		case doc.Kind == "ConfigMap" && doc.Metadata.Name == "foo-nats-config":
			// nats.conf is JSON apart from the values the subchart leaves
			// bare for nats-server's own parser: a variable ($SERVER_NAME)
			// and sizes with a unit (20Gi). Quote those and it decodes.
			raw := natsConfBare.ReplaceAllString(doc.Data["nats.conf"], `$1"$2"$3`)
			if err := json.Unmarshal([]byte(raw), &r.conf); err != nil {
				t.Fatalf("nats.conf does not decode: %v\n%s", err, raw)
			}
		case doc.Kind == "ConfigMap" && doc.Metadata.Name == "foo-vantage-collector":
			sawCollector = true
			body := doc.Data["collector.yaml"]
			r.replicasRendered = strings.Contains(body, "\n  replicas:")
			path := filepath.Join(t.TempDir(), "collector.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := collector.LoadConfig(path)
			if err != nil {
				t.Fatalf("the collector refuses the rendered collector.yaml: %v\n%s", err, body)
			}
			r.streams = cfg.Streams
		}
	}
	if !sawCollector {
		t.Fatal("the render has no foo-vantage-collector ConfigMap; this test is reading the wrong names")
	}
	return r, nil
}

// mustRenderNATS is renderNATS for a render that has to succeed.
func mustRenderNATS(t *testing.T, args ...string) natsRender {
	t.Helper()
	r, err := renderNATS(t, args...)
	if err != nil {
		t.Fatalf("helm template %v failed: %v", args, err)
	}
	return r
}

// dig walks nested JSON objects by key.
func dig(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

// The default render is a three-server NATS cluster that survives losing
// any one server: three pods, forced onto three different nodes, routes
// over mutual TLS, and streams with three copies. Each is asserted on its
// own because each fails alone into something that looks resilient and is
// not: two pods on one node, or three servers holding one-copy streams,
// lose data on one node's restart exactly as a single server does.
func TestHelmNATSDefaultIsAThreeServerCluster(t *testing.T) {
	r := mustRenderNATS(t)

	if r.natsReplicas != 3 {
		t.Errorf("the NATS StatefulSet runs %d replicas, want 3", r.natsReplicas)
	}

	var hard bool
	for _, c := range r.spread {
		if c["topologyKey"] == "kubernetes.io/hostname" &&
			c["whenUnsatisfiable"] == "DoNotSchedule" && c["maxSkew"] == 1 {
			hard = true
			if c["minDomains"] != 3 {
				t.Errorf("the hostname spread has minDomains %v, want 3: without it a two-node "+
					"cluster stacks two servers on one node", c["minDomains"])
			}
			sel, _ := c["labelSelector"].(map[string]any)
			ml, _ := sel["matchLabels"].(map[string]any)
			if ml["app.kubernetes.io/component"] != "nats" || ml["app.kubernetes.io/instance"] != "foo" {
				t.Errorf("the hostname spread selects %v, not this release's NATS pods", ml)
			}
		}
	}
	if !hard {
		t.Errorf("no hard (DoNotSchedule, maxSkew 1) spread on kubernetes.io/hostname: %v -- "+
			"the scheduler is free to put all three servers on one node", r.spread)
	}

	if dig(r.conf, "cluster") == nil {
		t.Fatal("nats.conf has no cluster block")
	}
	routes, _ := dig(r.conf, "cluster", "routes").([]any)
	if len(routes) != 3 {
		t.Errorf("nats.conf lists %d routes, want 3: %v", len(routes), routes)
	}
	for i, rt := range routes {
		want := fmt.Sprintf("tls://foo-nats-%d.foo-nats-headless.vantage.svc.cluster.local:6222", i)
		if rt != want {
			t.Errorf("route %d is %v, want %s: the server certificate's route SAN is "+
				"*.foo-nats-headless.vantage.svc.cluster.local, so any other form fails verification", i, rt, want)
		}
	}
	// The route TLS block, not the client one: both carry the same keys, so
	// reading the top-level "tls" here would pass with routes in plaintext.
	if v := dig(r.conf, "cluster", "tls", "verify"); v != true {
		t.Errorf("cluster.tls.verify is %v, want true", v)
	}
	if v := dig(r.conf, "cluster", "tls", "cert_file"); v != "/etc/nats-certs/cluster/tls.crt" {
		t.Errorf("cluster.tls.cert_file is %v: the routes present no certificate", v)
	}
	if v := dig(r.conf, "cluster", "tls", "ca_file"); v != "/etc/nats-ca-cert/ca.crt" {
		t.Errorf("cluster.tls.ca_file is %v: the routes verify against no CA", v)
	}
	if v := dig(r.conf, "tls", "verify"); v != true {
		t.Errorf("the client-facing tls.verify is %v, want true", v)
	}

	if !r.replicasRendered || r.streams.Replicas != 3 {
		t.Errorf("collector streams.replicas is %d (rendered: %v), want 3 rendered: a cluster "+
			"with one-copy streams loses them when their one server restarts",
			r.streams.Replicas, r.replicasRendered)
	}
	if r.streams.LSReplicas != 3 {
		t.Errorf("the collector computes ls_replicas %d, want 3 (it follows replicas)", r.streams.LSReplicas)
	}
}

// The single-server opt-out is one value, and it has to take the stream
// copies back to 1 with it: three-copy streams on one server fail the
// collector at startup.
func TestHelmNATSSingleServerOptOut(t *testing.T) {
	r := mustRenderNATS(t, "--set", "nats.config.cluster.enabled=false")

	if r.natsReplicas != 1 {
		t.Errorf("the NATS StatefulSet runs %d replicas, want 1", r.natsReplicas)
	}
	if dig(r.conf, "cluster") != nil {
		t.Errorf("nats.conf still has a cluster block: %v", dig(r.conf, "cluster"))
	}
	// The spread rule stays in the pod template and cannot hold the pod: one
	// pod's skew is at most 1 on any number of nodes, and with fewer
	// eligible nodes than minDomains the global minimum is 0, so 1-0 is
	// still within maxSkew. What would block it is a larger maxSkew-violating
	// rule, which the chart does not render.
	for _, c := range r.spread {
		if c["whenUnsatisfiable"] == "DoNotSchedule" {
			if skew, _ := c["maxSkew"].(int); skew < 1 {
				t.Errorf("a DoNotSchedule spread with maxSkew %v could hold even one pod Pending: %v", c["maxSkew"], c)
			}
		}
	}
	if r.replicasRendered {
		t.Errorf("collector.yaml renders streams.replicas on a single server; want it left to the collector's default")
	}
	if r.streams.Replicas != 0 && r.streams.Replicas != 1 {
		t.Errorf("collector streams.replicas is %d on a single server, want the default (1)", r.streams.Replicas)
	}
	if r.streams.LSReplicas != 1 {
		t.Errorf("the collector computes ls_replicas %d on a single server, want 1", r.streams.LSReplicas)
	}

	// Stream copies the single server cannot hold are refused at render
	// time, not left to a collector that never starts.
	// Each pair sets the other field to 1, so that only the check for the
	// field at 3 can refuse it: replicas alone at 3 would also fail the LS
	// check, since ls_replicas follows it.
	for _, v := range [][2]string{
		{"collector.streams.replicas=3", "collector.streams.lsReplicas=1"},
		{"collector.streams.lsReplicas=3", "collector.streams.replicas=1"},
	} {
		if _, err := renderNATS(t, "--set", "nats.config.cluster.enabled=false",
			"--set", v[0], "--set", v[1]); err == nil {
			t.Errorf("rendered %s against a single NATS server", v[0])
		}
	}
}

// An explicit collector.streams.replicas wins over the derived 3, in both
// directions, and lsReplicas stays independent of it.
func TestHelmNATSExplicitStreamReplicasWin(t *testing.T) {
	r := mustRenderNATS(t, "--set", "collector.streams.replicas=1")
	if r.streams.Replicas != 1 || !r.replicasRendered {
		t.Errorf("explicit replicas 1 rendered as %d (rendered: %v)", r.streams.Replicas, r.replicasRendered)
	}
	if r.streams.LSReplicas != 1 {
		t.Errorf("ls_replicas is %d with replicas 1 and no lsReplicas, want 1", r.streams.LSReplicas)
	}
	if r.natsReplicas != 3 {
		t.Errorf("setting stream replicas changed the NATS server count to %d", r.natsReplicas)
	}

	r = mustRenderNATS(t, "--set", "collector.streams.replicas=1", "--set", "collector.streams.lsReplicas=3")
	if r.streams.Replicas != 1 || r.streams.LSReplicas != 3 {
		t.Errorf("replicas 1 / lsReplicas 3 rendered as %d / %d", r.streams.Replicas, r.streams.LSReplicas)
	}

	// A larger cluster still derives three copies: enough to survive one
	// server's loss, without five copies of every stream on disk.
	r = mustRenderNATS(t, "--set", "nats.config.cluster.replicas=5",
		"--set", `nats.podTemplate.topologySpreadConstraints.kubernetes\.io/hostname.minDomains=5`)
	if r.natsReplicas != 5 || r.streams.Replicas != 3 {
		t.Errorf("a five-server cluster runs %d servers and derives replicas %d, want 5 and 3",
			r.natsReplicas, r.streams.Replicas)
	}
}

// A clustered NATS must be able to lose a server: an odd size of at least
// 3. Two servers need both for quorum, and the PodDisruptionBudget would
// let a drain take one. And minDomains, a literal, must keep up with the
// size. Each refused case is paired with the nearest accepted one, so a
// guard that refused everything would fail too.
func TestHelmNATSClusterSizeGuards(t *testing.T) {
	minDomains := func(n int) []string {
		return []string{"--set", fmt.Sprintf(`nats.podTemplate.topologySpreadConstraints.kubernetes\.io/hostname.minDomains=%d`, n)}
	}
	for _, n := range []int{1, 2, 4, 6} {
		args := append([]string{"--set", fmt.Sprintf("nats.config.cluster.replicas=%d", n)}, minDomains(n)...)
		if _, err := renderNATS(t, args...); err == nil {
			t.Errorf("rendered a clustered NATS of %d servers", n)
		} else if !strings.Contains(err.Error(), "odd number of servers") {
			t.Errorf("%d servers refused for the wrong reason: %v", n, err)
		}
	}
	for _, n := range []int{3, 5} {
		mustRenderNATS(t, append([]string{"--set", fmt.Sprintf("nats.config.cluster.replicas=%d", n)}, minDomains(n)...)...)
	}

	// cluster.replicas above minDomains is refused; equal is fine (above).
	_, err := renderNATS(t, "--set", "nats.config.cluster.replicas=5")
	if err == nil {
		t.Error("rendered five servers with minDomains still 3")
	} else if !strings.Contains(err.Error(), "minDomains") {
		t.Errorf("five servers with minDomains 3 refused for the wrong reason: %v", err)
	}

	// The single-server opt-out ignores cluster.replicas, as the subchart does.
	r := mustRenderNATS(t, "--set", "nats.config.cluster.enabled=false", "--set", "nats.config.cluster.replicas=2")
	if r.natsReplicas != 1 {
		t.Errorf("the opt-out with cluster.replicas=2 runs %d servers, want 1", r.natsReplicas)
	}
}

// External NATS: the chart cannot see the cluster, so it renders no
// replicas and the collector keeps its default of 1 -- the operator sets
// collector.streams.replicas to match their own servers.
func TestHelmNATSExternalLeavesReplicasAlone(t *testing.T) {
	ext := []string{"--set", "nats.enabled=false", "--set", "nats.externalURL=nats://nats.example:4222"}
	r := mustRenderNATS(t, ext...)
	if r.natsReplicas != 0 || r.conf != nil {
		t.Errorf("external NATS still rendered a bundled server (replicas %d)", r.natsReplicas)
	}
	if r.replicasRendered {
		t.Error("external NATS rendered streams.replicas; the chart cannot know the external cluster's size")
	}
	if r.streams.LSReplicas != 1 {
		t.Errorf("external NATS: the collector computes ls_replicas %d, want 1", r.streams.LSReplicas)
	}
	r = mustRenderNATS(t, append(ext, "--set", "collector.streams.replicas=3")...)
	if r.streams.Replicas != 3 {
		t.Errorf("external NATS with explicit replicas 3 rendered %d", r.streams.Replicas)
	}
}

// Route TLS is guarded like client TLS: with the cluster on and client mTLS
// on, routes in plaintext, or on a Secret other than the server
// certificate's, are refused; the plaintext opt-out takes the route switch
// with the other three. Each refused case supplies everything else a render
// needs, so only the check under test can fail it.
func TestHelmNATSRouteTLSGuards(t *testing.T) {
	for _, bad := range [][]string{
		{"--set", "nats.config.cluster.tls.enabled=false"},
		{"--set", "nats.config.cluster.tls.secretName=other"},
		{"--set", "nats.tls.enabled=false", "--set", "nats.tlsCA.enabled=false",
			"--set", "nats.config.nats.tls.enabled=false"},
	} {
		if _, err := renderNATS(t, bad...); err == nil {
			t.Errorf("rendered with %v", bad)
		}
	}
	plaintext := []string{"--set", "nats.tls.enabled=false", "--set", "nats.tlsCA.enabled=false",
		"--set", "nats.config.nats.tls.enabled=false", "--set", "nats.config.cluster.tls.enabled=false"}
	r := mustRenderNATS(t, plaintext...)
	if tls := dig(r.conf, "cluster", "tls"); tls != nil {
		t.Errorf("the plaintext opt-out still renders route TLS: %v", tls)
	}
	// On a single server the route switches are inert, so neither the
	// cluster TLS setting nor its Secret name is checked.
	mustRenderNATS(t, "--set", "nats.config.cluster.enabled=false",
		"--set", "nats.config.cluster.tls.enabled=false")
}

// podSpec is one pod template the render would create, by the object that
// owns it ("StatefulSet/foo-nats").
type podSpec struct {
	owner string
	spec  map[string]any
}

// renderPodSpecs renders the chart and returns every pod template in it.
func renderPodSpecs(t *testing.T, args ...string) []podSpec {
	t.Helper()
	out, err := helmTemplate(t, args...)
	if err != nil {
		t.Fatalf("helm template %v failed: %v", args, err)
	}
	var pods []podSpec
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the render is not YAML: %v", err)
		}
		kind, _ := doc["kind"].(string)
		name, _ := dig(doc, "metadata", "name").(string)
		var spec any
		switch kind {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
			spec = dig(doc, "spec", "template", "spec")
		case "CronJob":
			spec = dig(doc, "spec", "jobTemplate", "spec", "template", "spec")
		case "Pod":
			spec = doc["spec"]
		default:
			continue
		}
		m, ok := spec.(map[string]any)
		if !ok {
			t.Fatalf("%s/%s has no pod spec", kind, name)
		}
		// A StatefulSet's volumeClaimTemplates never appear in its pod
		// template's own volumes -- the StatefulSet controller adds the
		// matching persistentVolumeClaim volume to each Pod it creates, not
		// to spec.template.spec here -- so restrictedViolations would never
		// see them. Synthesize that volume here, as the kind it always is,
		// so the render's disk storage is covered by the same check as
		// every other volume instead of being invisible to it.
		if vcts, _ := dig(doc, "spec", "volumeClaimTemplates").([]any); vcts != nil {
			vols, _ := m["volumes"].([]any)
			for _, vct := range vcts {
				vcm, _ := vct.(map[string]any)
				vname, _ := dig(vcm, "metadata", "name").(string)
				vols = append(vols, map[string]any{"name": vname, "persistentVolumeClaim": map[string]any{}})
			}
			m["volumes"] = vols
		}
		pods = append(pods, podSpec{owner: kind + "/" + name, spec: m})
	}
	return pods
}

// restrictedVolumeTypes is the "restricted" Pod Security Standard's
// allow-list for pod.spec.volumes[*], by the one non-"name" key a volume
// entry carries to say what kind it is. Anything else -- hostPath, nfs,
// gitRepo, and so on -- is refused.
var restrictedVolumeTypes = map[string]bool{
	"configMap": true, "csi": true, "downwardAPI": true, "emptyDir": true,
	"ephemeral": true, "persistentVolumeClaim": true, "projected": true, "secret": true,
}

// restrictedViolations lists every way pod falls short of the Kubernetes
// "restricted" Pod Security Standard, as the admission controller checks a
// pod template:
// https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
// It checks every restricted-level rule (privileged; allowPrivilegeEscalation;
// capabilities.drop ALL; capabilities.add beyond NET_BIND_SERVICE;
// runAsNonRoot; runAsUser not 0; seccompProfile RuntimeDefault or Localhost)
// plus the baseline-level rules restricted still carries forward (host
// namespaces; volume types outside restrictedVolumeTypes; host ports), and
// returns nil for a compliant pod. A container's own setting wins over the
// pod's, as it does in admission.
func restrictedViolations(pod map[string]any) []string {
	var v []string
	for _, k := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if pod[k] == true {
			v = append(v, "pod: "+k+" is true")
		}
	}
	vols, _ := pod["volumes"].([]any)
	for _, vol := range vols {
		m, _ := vol.(map[string]any)
		if m == nil {
			continue
		}
		var badTypes []string
		for k := range m {
			if k == "name" || restrictedVolumeTypes[k] {
				continue
			}
			badTypes = append(badTypes, k)
		}
		sort.Strings(badTypes)
		for _, k := range badTypes {
			v = append(v, fmt.Sprintf("pod: volume %v is type %s, which restricted does not allow", m["name"], k))
		}
	}
	psc, _ := pod["securityContext"].(map[string]any)
	effective := func(sc map[string]any, key string) (any, bool) {
		if val, ok := sc[key]; ok {
			return val, true
		}
		val, ok := psc[key]
		return val, ok
	}
	for _, list := range []string{"initContainers", "containers", "ephemeralContainers"} {
		cs, _ := pod[list].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			name := fmt.Sprint(cm["name"])
			sc, _ := cm["securityContext"].(map[string]any)
			if sc["privileged"] == true {
				v = append(v, name+": privileged is true")
			}
			if sc["allowPrivilegeEscalation"] != false {
				v = append(v, name+": allowPrivilegeEscalation is not false")
			}
			caps, _ := sc["capabilities"].(map[string]any)
			drop, _ := caps["drop"].([]any)
			if !slices.Contains(drop, any("ALL")) {
				v = append(v, name+": capabilities.drop does not include ALL")
			}
			add, _ := caps["add"].([]any)
			for _, a := range add {
				if a != "NET_BIND_SERVICE" {
					v = append(v, fmt.Sprintf("%s: adds capability %v", name, a))
				}
			}
			if nonRoot, _ := effective(sc, "runAsNonRoot"); nonRoot != true {
				v = append(v, name+": runAsNonRoot is not true")
			}
			if uid, ok := effective(sc, "runAsUser"); ok && uid == 0 {
				v = append(v, name+": runAsUser is 0")
			}
			seccomp, _ := effective(sc, "seccompProfile")
			sm, _ := seccomp.(map[string]any)
			if typ := sm["type"]; typ != "RuntimeDefault" && typ != "Localhost" {
				v = append(v, name+": seccompProfile is not RuntimeDefault or Localhost")
			}
			ports, _ := cm["ports"].([]any)
			for _, p := range ports {
				pm, _ := p.(map[string]any)
				if hp, ok := pm["hostPort"].(int); ok && hp != 0 {
					v = append(v, fmt.Sprintf("%s: hostPort %d", name, hp))
				}
			}
		}
	}
	return v
}

// TestRestrictedViolationsNamesEachRule probes the checker before trusting
// it with the chart: a bare container fails exactly the four rules the NATS
// pods failed on a live cluster, a compliant pod fails none, a container
// can undo a compliant pod's settings, and a table below exercises every
// remaining rule -- privileged, capabilities.add beyond NET_BIND_SERVICE,
// each host namespace, host ports and the volume-type allow-list -- on
// both its failing and its passing side. A rule with no case here can be
// deleted with nothing to notice.
func TestRestrictedViolationsNamesEachRule(t *testing.T) {
	bare := map[string]any{"containers": []any{map[string]any{"name": "c"}}}
	want := []string{
		"c: allowPrivilegeEscalation is not false",
		"c: capabilities.drop does not include ALL",
		"c: runAsNonRoot is not true",
		"c: seccompProfile is not RuntimeDefault or Localhost",
	}
	if got := restrictedViolations(bare); !slices.Equal(got, want) {
		t.Errorf("bare container: got %q, want %q", got, want)
	}

	compliant := func() map[string]any {
		return map[string]any{
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": 1000,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{"name": "c", "securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"capabilities":             map[string]any{"drop": []any{"ALL"}},
			}}},
		}
	}
	if got := restrictedViolations(compliant()); len(got) != 0 {
		t.Errorf("compliant pod: got %q, want none", got)
	}

	undone := compliant()
	sc := undone["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
	sc["runAsNonRoot"] = false
	sc["runAsUser"] = 0
	want = []string{"c: runAsNonRoot is not true", "c: runAsUser is 0"}
	if got := restrictedViolations(undone); !slices.Equal(got, want) {
		t.Errorf("container overriding a compliant pod: got %q, want %q", got, want)
	}

	// Each case starts from a compliant pod and breaks exactly one more
	// rule (or, for the two accept-side cases, adds something the profile
	// allows), so the want list is either that one rule's message or none.
	for _, tc := range []struct {
		name   string
		mutate func(pod map[string]any)
		want   []string
	}{
		{"privileged", func(pod map[string]any) {
			cs := pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
			cs["privileged"] = true
		}, []string{"c: privileged is true"}},
		{"capabilities.add beyond NET_BIND_SERVICE", func(pod map[string]any) {
			caps := pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["capabilities"].(map[string]any)
			caps["add"] = []any{"SYS_ADMIN"}
		}, []string{"c: adds capability SYS_ADMIN"}},
		{"capabilities.add NET_BIND_SERVICE only (accept side)", func(pod map[string]any) {
			caps := pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["capabilities"].(map[string]any)
			caps["add"] = []any{"NET_BIND_SERVICE"}
		}, nil},
		{"hostNetwork", func(pod map[string]any) { pod["hostNetwork"] = true }, []string{"pod: hostNetwork is true"}},
		{"hostPID", func(pod map[string]any) { pod["hostPID"] = true }, []string{"pod: hostPID is true"}},
		{"hostIPC", func(pod map[string]any) { pod["hostIPC"] = true }, []string{"pod: hostIPC is true"}},
		{"hostPort", func(pod map[string]any) {
			c := pod["containers"].([]any)[0].(map[string]any)
			c["ports"] = []any{map[string]any{"hostPort": 8080}}
		}, []string{"c: hostPort 8080"}},
		{"nfs volume", func(pod map[string]any) {
			pod["volumes"] = []any{map[string]any{"name": "v", "nfs": map[string]any{"server": "x", "path": "/"}}}
		}, []string{"pod: volume v is type nfs, which restricted does not allow"}},
		{"hostPath volume", func(pod map[string]any) {
			pod["volumes"] = []any{map[string]any{"name": "v", "hostPath": map[string]any{"path": "/x"}}}
		}, []string{"pod: volume v is type hostPath, which restricted does not allow"}},
		{"allowed volume types (accept side)", func(pod map[string]any) {
			pod["volumes"] = []any{
				map[string]any{"name": "a", "configMap": map[string]any{}},
				map[string]any{"name": "b", "csi": map[string]any{}},
				map[string]any{"name": "c", "downwardAPI": map[string]any{}},
				map[string]any{"name": "d", "emptyDir": map[string]any{}},
				map[string]any{"name": "e", "ephemeral": map[string]any{}},
				map[string]any{"name": "f", "persistentVolumeClaim": map[string]any{}},
				map[string]any{"name": "g", "projected": map[string]any{}},
				map[string]any{"name": "h", "secret": map[string]any{}},
			}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := compliant()
			tc.mutate(pod)
			if got := restrictedViolations(pod); !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHelmChartPodsMeetRestrictedPodSecurity: every pod the chart renders,
// the NATS subchart's included, passes the restricted profile, so the
// release namespace can enforce it. The three renders are the default, the
// single-server opt-out, and the NATS exporter that deploy/helm/README.md's
// Prometheus section tells operators to turn on, which adds a third
// container to the NATS pod.
func TestHelmChartPodsMeetRestrictedPodSecurity(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		nats []string
	}{
		{"default", nil, []string{"nats", "reloader"}},
		{"single NATS server", []string{"--set", "nats.config.cluster.enabled=false"},
			[]string{"nats", "reloader"}},
		{"NATS exporter on", []string{"--set", "nats.promExporter.enabled=true"},
			[]string{"nats", "reloader", "prom-exporter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := renderPodSpecs(t, tc.args...)
			var nats map[string]any
			for _, p := range pods {
				for _, v := range restrictedViolations(p.spec) {
					t.Errorf("%s: %s", p.owner, v)
				}
				if p.owner == "StatefulSet/foo-nats" {
					nats = p.spec
				}
			}
			if nats == nil {
				t.Fatalf("no StatefulSet/foo-nats among %d pod specs; this test is reading the wrong names", len(pods))
			}
			cs, _ := nats["containers"].([]any)
			var names []string
			for _, c := range cs {
				names = append(names, fmt.Sprint(c.(map[string]any)["name"]))
			}
			if !slices.Equal(names, tc.nats) {
				t.Errorf("the NATS pod runs %v, want %v: a container this test does not know about "+
					"was checked by nothing above", names, tc.nats)
			}

			psc, _ := nats["securityContext"].(map[string]any)
			uid, _ := psc["runAsUser"].(int)
			if uid == 0 {
				t.Errorf("the NATS pod sets no non-zero runAsUser (%v): the images carry no USER "+
					"and would run as root", psc["runAsUser"])
			}
			if psc["fsGroup"] == nil {
				t.Error("the NATS pod sets no fsGroup, so its non-root uid cannot write the JetStream volume")
			}
			for _, c := range cs {
				cm := c.(map[string]any)
				if own, ok := dig(cm, "securityContext", "runAsUser").(int); ok && own != uid {
					t.Errorf("container %v runs as %d and the pod as %d: the reloader signals "+
						"nats-server over the shared process namespace, which needs one uid",
						cm["name"], own, uid)
				}
			}

			// The JetStream store is a volumeClaimTemplate, not a volumes
			// entry, so it never goes through restrictedViolations unless
			// renderPodSpecs folds it in as a persistentVolumeClaim volume.
			// It is always that type by construction and so can never fail
			// the allow-list check itself; this instead confirms
			// renderPodSpecs actually performed the fold, which nothing
			// above would notice if it silently stopped.
			var sawPVC bool
			vols, _ := nats["volumes"].([]any)
			for _, vol := range vols {
				vm, _ := vol.(map[string]any)
				if vm != nil && vm["persistentVolumeClaim"] != nil {
					sawPVC = true
				}
			}
			if !sawPVC {
				t.Error("the NATS pod's volumes carry no persistentVolumeClaim entry: " +
					"renderPodSpecs did not fold in the JetStream volumeClaimTemplate, " +
					"so the disk store's volume type goes unchecked")
			}
		})
	}
}

// TestHelmStaleAfterMustBeADuration: api.staleAfter is decoded by the API as
// a Go duration, so a value without a unit is refused at render rather than
// crashlooping the API. A duration with several units renders.
func TestHelmStaleAfterMustBeADuration(t *testing.T) {
	for _, bad := range []string{"90", "90 s", "ninety", "1.5"} {
		_, err := helmTemplate(t, "--set-string", "api.staleAfter="+bad)
		if err == nil {
			t.Errorf("api.staleAfter=%q rendered", bad)
		} else if !strings.Contains(err.Error(), "api.staleAfter") {
			t.Errorf("api.staleAfter=%q refused for the wrong reason: %v", bad, err)
		}
	}
	for _, good := range []string{"90s", "1h30m", "1.5m", "2h"} {
		out, err := helmTemplate(t, "--set-string", "api.staleAfter="+good)
		if err != nil {
			t.Errorf("api.staleAfter=%q: %v", good, err)
			continue
		}
		if want := fmt.Sprintf("stale_after: %q", good); !strings.Contains(string(out), want) {
			t.Errorf("api.staleAfter=%q did not render %s", good, want)
		}
	}
}
