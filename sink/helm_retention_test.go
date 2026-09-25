package sink

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jp2195/vantage/chtest"
)

// retentionJobTestDB is the database the schema Job's retention step is run
// against here. The Job itself only ever targets `vantage`; the step names
// its database through $CH_DB, which is what lets a test point it anywhere.
const retentionJobTestDB = "vantage_sink_retention_job_test"

// The markers job-schema.yaml puts around its retention step, so this test
// runs that step and nothing else: the rest of the Job applies files that
// name the `vantage` database outright.
const (
	retentionBeginMarker = "# BEGIN history retention"
	retentionEndMarker   = "# END history retention"
)

// retentionChangeRe matches the line the retention step logs for each table
// whose TTL it changes.
var retentionChangeRe = regexp.MustCompile(`(?m)^retention: set \S+ to \d+ days$`)

// TestSchemaJobRetentionStepIsIdempotent renders the chart with
// retention.days=30 and runs the rendered retention step, in the image and
// under the user and read-only root the Job runs with, against a scratch
// database built from the shipped schema. The first run changes the TTL of
// all ten history tables and of no other table; the second run changes
// nothing, because a table already at the configured retention is skipped.
// An upgrade re-runs the Job, so a step that rewrote every TTL each time
// would issue ten ALTERs on every upgrade for no change.
//
// It needs helm (with the chart's dependencies built), docker, and the dev
// ClickHouse; under VANTAGE_REQUIRE_CLICKHOUSE any of them missing fails.
func TestSchemaJobRetentionStepIsIdempotent(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, retentionJobTestDB)

	image, script := renderRetentionStep(t, ctx, 30)

	engines := func() map[string]string {
		t.Helper()
		rows, err := conn.Query(ctx,
			"SELECT name, engine_full FROM system.tables WHERE database = ?", retentionJobTestDB)
		if err != nil {
			t.Fatalf("read system.tables: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, engine string
			if err := rows.Scan(&name, &engine); err != nil {
				t.Fatalf("scan system.tables: %v", err)
			}
			out[name] = engine
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("system.tables: %v", err)
		}
		return out
	}

	before := engines()
	var history []string
	for name, engine := range before {
		if strings.Contains(engine, "TTL toDateTime(ts_collector) + toIntervalDay(90)") {
			history = append(history, name)
		}
	}
	if len(history) != 10 {
		t.Fatalf("the shipped schema has %d tables with a 90-day history TTL, want 10: %v",
			len(history), history)
	}

	first := runRetentionStep(t, ctx, image, script)
	if n := len(retentionChangeRe.FindAllString(first, -1)); n != 10 {
		t.Fatalf("first run logged %d TTL changes, want 10:\n%s", n, first)
	}
	after := engines()
	for name, engine := range after {
		want := before[name]
		if strings.Contains(want, "toIntervalDay(90)") {
			want = strings.Replace(want, "toIntervalDay(90)", "toIntervalDay(30)", 1)
		}
		if engine != want {
			t.Errorf("%s after the first run:\n got %s\nwant %s", name, engine, want)
		}
	}

	second := runRetentionStep(t, ctx, image, script)
	if n := len(retentionChangeRe.FindAllString(second, -1)); n != 0 {
		t.Fatalf("second run logged %d TTL changes, want 0: a rerun must be a no-op\n%s", n, second)
	}
	if again := engines(); fmt.Sprint(again) != fmt.Sprint(after) {
		t.Errorf("the second run changed a table:\n got %v\nwant %v", again, after)
	}
}

// skipWithoutTool ends the test when a tool it needs besides ClickHouse is
// missing: a skip naming the tool, or a failure under
// VANTAGE_REQUIRE_CLICKHOUSE, which CI sets so that this test cannot pass
// by not running.
func skipWithoutTool(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("VANTAGE_REQUIRE_CLICKHOUSE") != "" {
		t.Fatalf("%s -- VANTAGE_REQUIRE_CLICKHOUSE is set, so this is a failure, not a skip", msg)
	}
	t.Skip(msg)
}

// renderRetentionStep renders job-schema.yaml with retention.days=days and
// returns the Job's image and its retention step, cut out between the
// markers.
func renderRetentionStep(t *testing.T, ctx context.Context, days int) (image, script string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "helm", "template", "foo",
		filepath.Join("..", "deploy", "helm", "vantage"), "--namespace", "vantage",
		"--set", "clickhouse.externalHost=ch", "--set", "clickhouse.auth.password=ci",
		"--set", "nats.tls.collectorSecret=c-tls", "--set", "nats.tls.writerSecret=w-tls",
		"--set", fmt.Sprintf("retention.days=%d", days),
		"--show-only", "templates/job-schema.yaml",
	).CombinedOutput()
	if err != nil {
		skipWithoutTool(t, fmt.Sprintf("helm could not render the chart (install helm and run "+
			"`helm dependency build deploy/helm/vantage`): %v\n%s", err, out))
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string   `yaml:"image"`
						Args  []string `yaml:"args"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &job); err != nil {
		t.Fatalf("parse the rendered Job: %v\n%s", err, out)
	}
	cs := job.Spec.Template.Spec.Containers
	if len(cs) != 1 || len(cs[0].Args) != 1 {
		t.Fatalf("the rendered Job has %d containers, want 1 with one script argument", len(cs))
	}
	full := cs[0].Args[0]
	begin := strings.Index(full, retentionBeginMarker)
	end := strings.Index(full, retentionEndMarker)
	if begin < 0 || end < begin {
		t.Fatalf("the Job script has no %q ... %q block:\n%s", retentionBeginMarker, retentionEndMarker, full)
	}
	return cs[0].Image, "set -e\n" + full[begin:end]
}

// runRetentionStep runs script in image the way the Job's pod does -- uid
// 65532, read-only root, no capabilities -- on the host network so it
// reaches chtest's ClickHouse, with the Job's CH_* environment aimed at
// retentionJobTestDB. It returns the combined output and fails the test on a
// non-zero exit.
func runRetentionStep(t *testing.T, ctx context.Context, image, script string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(chtest.Addr())
	if err != nil {
		t.Fatalf("split %s: %v", chtest.Addr(), err)
	}
	dsn, err := url.Parse(chtest.DSN(retentionJobTestDB))
	if err != nil {
		t.Fatalf("parse chtest DSN: %v", err)
	}
	pass, _ := dsn.User.Password()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"--network", "host", "--user", "65532", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-e", "CH_HOST="+host, "-e", "CH_PORT="+port,
		"-e", "CH_USER="+dsn.User.Username(), "-e", "CH_PASS="+pass,
		"-e", "CH_DB="+retentionJobTestDB,
		"--entrypoint", "/bin/sh", image, "-s",
	)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, lookErr := exec.LookPath("docker"); lookErr != nil {
			skipWithoutTool(t, fmt.Sprintf("docker is not installed, and it is needed to run "+
				"the schema Job's image: %v", lookErr))
		}
		t.Fatalf("the retention step failed: %v\n%s", err, out)
	}
	return string(out)
}
