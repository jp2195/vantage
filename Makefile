.PHONY: help check chart-deps dev-api generate test test-nodb build fuzz images push-images ui ui-gen ui-test ui-fixtures standalone-check sync-helm-schema fetch-asnames restore-archive-backup

# Default target: an index that is DERIVED from the recipes rather than
# hand-maintained, so it cannot fall behind the targets it describes. A target
# added without a `##` comment is invisible here, which is the intended
# pressure -- document it or it does not exist.
.DEFAULT_GOAL := help

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?##' $(MAKEFILE_LIST) \
	 | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# The Go half of CI's gate -- `go build ./...`, `go vet ./...`, unit tests --
# behind one command that prints one line. Both halves of CI ran here with no
# local front door before this: `make build` builds three named binaries, which
# is a different question from "does every package still compile", and nothing
# ran vet at all.
#
# The script cd's to the repo root because `go build ./...` EXITS 0 from a
# directory holding no Go files -- it prints "matched no packages" to stderr
# and reports success. RACE=1 adds the race detector to match CI exactly.
check: ## Go gate: build every package, vet, unit tests (RACE=1 for the race detector)
	@./scripts/go-check.sh

# Pinned codegen tool versions (install with `go install <module>@<version>`):
#   buf:           github.com/bufbuild/buf/cmd/buf@v1.73.0
#   protoc-gen-go: google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
#
# Both are invoked from $GOBIN below by absolute path, and $GOBIN is
# prepended to PATH for the duration of the command, so this target works
# even when $GOPATH/bin is not on the caller's $PATH (buf shells out to
# protoc-gen-go by name and needs it on PATH to find it).
GOBIN := $(shell go env GOPATH)/bin

# Serve the real app on the host, against the dev stack's ClickHouse.
#
# `ui` is a PREREQUISITE, not a suggestion: webui/webui.go embeds webui/dist at
# COMPILE time, so rebuilding the UI without rebuilding the binary leaves a
# running daemon serving the previous bundle. That cost an afternoon once --
# a fix was verified as "not working" against a stale binary, and the hunt went
# looking for the bug in correct code. Depending on `ui` here makes the stale
# case unreachable rather than documented.
#
# The config is GENERATED rather than committed because deploy/dev/api.yaml is
# the container's, pointing at `clickhouse:9000` over the compose network --
# unreachable from the host. Four separate agents each hand-wrote a throwaway
# host config before this target existed. It honors VANTAGE_CH_NATIVE_PORT the
# same way `test` does, so a stack published on another port still works.
DEV_API_LISTEN ?= 127.0.0.1:9473

dev-api: ui ## Rebuild UI+daemon and serve the app (override DEV_API_LISTEN, default 127.0.0.1:9473)
	@mkdir -p bin
	@printf 'clickhouse_dsn: "clickhouse://vantage:vantage@127.0.0.1:%s/vantage"\nlisten: "%s"\ntokens:\n  - name: dev\n    token: "dev-token-not-a-secret"\n' \
	  "$(VANTAGE_CH_NATIVE_PORT)" "$(DEV_API_LISTEN)" > bin/api.local.yaml
	go build -ldflags "$(LDFLAGS)" -o bin/vantage-api ./cmd/vantage-api
	@echo "serving http://$(DEV_API_LISTEN)  (token: dev-token-not-a-secret)"
	./bin/vantage-api -config bin/api.local.yaml

generate: ## Regenerate protobuf code from .proto (pinned buf/protoc-gen-go)
	PATH="$(GOBIN):$$PATH" $(GOBIN)/buf generate
# VANTAGE_CH_NATIVE_PORT is the same variable docker-compose.dev.yml publishes
# ClickHouse's native port with, so `make test` reaches whatever the stack in
# this shell actually bound. Tying them together is not tidiness: chtest.Addr()
# falls back to 127.0.0.1:9000, and when nothing is listening there every
# live-ClickHouse test SKIPS -- inside a green "ok". On a stack published to
# 29000 that takes ./query from 4.2s to 0.004s and it still prints ok, so the
# suite reports success while asserting nothing about any SQL in the tree.
# VANTAGE_REQUIRE_CLICKHOUSE turns those skips into failures, which is what CI
# already does (.github/workflows/ci.yml) and what a local run needs too.
VANTAGE_CH_NATIVE_PORT ?= 9000
VANTAGE_TEST_CLICKHOUSE_ADDR ?= 127.0.0.1:$(VANTAGE_CH_NATIVE_PORT)

# The chart's NATS dependency is a gitignored archive, so a fresh clone has
# none, and the Helm-rendering tests in sink fail under `make test` without it.
# The target is the archive itself, named from Chart.lock's pinned version,
# so helm only fetches when that version changes or the archive is missing.
NATS_CHART_VERSION := $(shell awk '/name: nats/ {f=1} f && /version:/ {print $$2; exit}' deploy/helm/vantage/Chart.lock)
CHART_DEPS := deploy/helm/vantage/charts/nats-$(NATS_CHART_VERSION).tgz

chart-deps: $(CHART_DEPS) ## Fetch the Helm chart's dependencies (the NATS subchart) into deploy/helm/vantage/charts

$(CHART_DEPS): deploy/helm/vantage/Chart.lock deploy/helm/vantage/Chart.yaml
	helm repo add nats https://nats-io.github.io/k8s/helm/charts/ --force-update >/dev/null
	helm dependency build deploy/helm/vantage
	@test -f $@ || { echo "chart-deps: helm did not write $@" >&2; exit 1; }
	@touch $@

test: chart-deps ## Full Go gate: race detector AND a live ClickHouse (see test-nodb)
	VANTAGE_REQUIRE_CLICKHOUSE=1 \
	VANTAGE_TEST_CLICKHOUSE_ADDR=$(VANTAGE_TEST_CLICKHOUSE_ADDR) \
	go test ./... -race -count=1

dashboard-coverage: ## Report how many shipped dashboard panels no test asserts anything about (add LIST=1 for the panels)
	@python3 scripts/dashboard-coverage.py $(if $(LIST),--list,)

# Unit tests only, for a machine with no dev stack running. Every test that
# needs ClickHouse SKIPS here inside a green "ok", so this is not a gate:
# `make check` runs the same tests and prints how many were skipped, and
# `make test` refuses to skip at all.
test-nodb: ## Go unit tests only, no database required (live-ClickHouse tests skip; make check counts them)
	go test ./... -count=1

# The version stamped into every binary and image: the nearest tag plus
# commits since it, or the short SHA when there is no tag, "-dirty" when the
# tree has uncommitted changes. REVISION is the full SHA. Both reach
# buildinfo through -X; a wrong -X path is silently ignored by the linker,
# which buildinfo's TestEveryBinaryPrintsItsVersion exists to catch.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -X github.com/jp2195/vantage/buildinfo.Version=$(VERSION) -X github.com/jp2195/vantage/buildinfo.Revision=$(REVISION)

build: ## Build the CLI and the three daemons into bin/, stamped with VERSION
	@for b in vantage vantage-collector vantage-writer vantage-api; do \
	  go build -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done
	@echo "built bin/{vantage,vantage-collector,vantage-writer,vantage-api} at $(VERSION)"
fuzz: ## Fuzz the BMP and BGP parsers for 30s each
	go test ./bmp -fuzz=FuzzReadMsg -fuzztime=30s
	go test ./bgp -fuzz=FuzzParseUpdate -fuzztime=30s

# The image tag is the git short SHA, never "latest": latest cannot be rolled
# back, gives no diff between two deploys, and makes "which code is running"
# unanswerable.
TAG ?= $(shell git rev-parse --short HEAD)
REGISTRY ?= ghcr.io/jp2195

# .dockerignore keeps .git out of the build context, so the Dockerfiles
# cannot work out VERSION and REVISION themselves; they arrive as build args.
IMAGE_BUILD_ARGS = --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION)

images: ## Build the three container images, tagged with the git short SHA
	docker build $(IMAGE_BUILD_ARGS) -f Dockerfile.collector -t $(REGISTRY)/vantage-collector:$(TAG) .
	docker build $(IMAGE_BUILD_ARGS) -f Dockerfile.writer    -t $(REGISTRY)/vantage-writer:$(TAG) .
	docker build $(IMAGE_BUILD_ARGS) -f Dockerfile.api       -t $(REGISTRY)/vantage-api:$(TAG) .

push-images: images ## Push the three container images to $(REGISTRY)
	docker push $(REGISTRY)/vantage-collector:$(TAG)
	docker push $(REGISTRY)/vantage-writer:$(TAG)
	docker push $(REGISTRY)/vantage-api:$(TAG)

# The UI builds into webui/dist, which webui/webui.go embeds. `make ui` is
# therefore a prerequisite of any `go build` that is meant to serve the app;
# a build without it produces a working daemon that reports the UI as
# unbuilt, which is the intended behavior for a Go-only checkout.
ui: ## Build the UI into webui/dist (embedded by webui/webui.go)
	cd ui && npm ci && npm run build

# Regenerates the typed client from api/openapi.yaml. CI runs this and fails
# on any diff, so the committed client cannot drift from the contract.
ui-gen: ## Regenerate the typed API client from api/openapi.yaml
	cd ui && npm ci && npm run generate

ui-test: ## Run the UI test suite
	cd ui && npm ci && npm test

standalone-check: ## Fail if a published file cites an internal path, a commit SHA or internal process material
	@./scripts/check-citations-test.sh
	@./scripts/check-standalone.sh

# Fixtures are captured from a live deployment, never hand-written. See
# scripts/capture-ui-fixtures.sh for why, and ui/src/api/fixtures/README.md for
# what the committed set actually contains and where each file came from.
ui-fixtures: ## Capture UI fixtures from a live deployment
	./scripts/capture-ui-fixtures.sh

# RIPE publishes the AS-holder-name list at a fixed URL, refreshed daily, with
# no auth and no date inside the file itself -- its first line IS data (`1
# LVLT-1 - Level 3 Parent, LLC, US`), not a header. The only publication date
# available is the response's Last-Modified header, so this target is the one
# moment that date can be captured; reconstructing it later from the file's
# mtime would record when someone copied the file, not when RIPE published it.
#
# This is a `make` target, run by a person, rather than code any daemon
# calls: the tool runs on a management network with no route to the
# internet, and ftp.ripe.net is the one host nothing here talks to except
# this recipe.
#
# It also does the TSV parse, not just the download, because ClickHouse's
# dictionary FILE source wants columns, and this is the one place that both
# has the raw bytes and knows the row count they must survive parsing with.
# ^(\d+) (.*), ([A-Z]{2})$ matches all 122,442 known lines and keeps the name
# field WHOLE -- 31.8% of lines have no " - " separator and 1,530 have more
# than one, so splitting on it would truncate real holder names.
#
# All four outputs -- raw file, parsed TSV, the one-row date TSV, and the
# human-readable sidecar -- are staged as temp files in the destination
# directory and moved into place with a same-filesystem rename, which is
# atomic PER FILE: a download that fails partway, or a parse that does not
# account for every line, never overwrites a good file with a bad one. The
# four renames are NOT one atomic unit, and this target does not claim
# otherwise -- a process killed between them can still leave a mismatched
# set (say, a new asn.txt beside a stale asn.meta). They run back-to-back
# with nothing else between them to keep that window as narrow as the shell
# allows; closing it completely would need a lock file or a manifest a
# reader checks before trusting the set, which is more machinery than an
# operator-run fetch justifies.
#
# The one-row date TSV (asnames_meta.tsv) exists for deploy/clickhouse/
# asnames_meta.sql, a companion dictionary: the names dictionary's own DDL
# reads only asn.tsv, and nothing else carries RIPE's Last-Modified date
# into ClickHouse, but the API's contract has to state that date (a stale
# holder name that looks authoritative is the one way this dataset can
# mislead). One row, "0\t<Last-Modified value>", is all that companion
# dictionary's schema needs -- see that file's own header for the key.
#
# Every numeric value that gates a decision below (raw_lines, tsv_lines,
# malformed) is read through a pipeline, and pipeline failures are not
# reflected in `&&` chaining -- the trailing `tr`/`wc` masks an upstream
# `wc -l` or `awk` failure. raw_lines and tsv_lines are genuinely covered
# by the ''|*[!0-9]*) case guard restore-archive-backup already uses below,
# rather than by `set -o pipefail` (not portable to every /bin/sh this
# Makefile might run under, and this file already has an established,
# working pattern for the same problem): a failed `wc -l` prints nothing,
# the guard sees an empty string, and the target stops.
#
# malformed is NOT, and that is the one worth knowing about. A failed `awk`
# prints nothing, `wc -l` then counts zero lines, and the value reaching
# the guard is a well-formed `0` -- which the guard accepts and which
# `[ "$$malformed" != "0" ]` reads as "no malformed rows". It fails OPEN,
# silently, on the check most worth having.
#
# It is worse than one guard being weaker than its neighbors, because the
# neighbor is nearly tautological: `sed` prints every input line whether or
# not the substitution matched, so a parse that matched NOTHING still
# produces a TSV with exactly raw_lines lines and `tsv_lines != raw_lines`
# still passes. malformed (awk's NF != 3) is the only check here that
# validates FORMAT at all, and it is the one that fails open. Wrapping that
# awk in `set -o pipefail`, or having it emit its own count, is the fix;
# until then this is a measured limit of this target, not a guarantee.
# The Helm chart cannot read outside its own directory: configmap-schema.yaml
# builds its ConfigMap with `.Files.Glob "files/schema/*.sql"`, and .Files.Glob
# is chart-rooted by design. So the chart carries COPIES of the DDL, and copies
# drift. This target is the one-call way to re-derive them; sink's
# TestHelmChartSchemaFilesMirrorDeployClickhouse is the guard that notices when
# nobody ran it.
#
# The drift is invisible from both ends, which is why it needs a command rather
# than a convention. A stale chart still renders a valid ConfigMap, and a fresh
# install is correct regardless -- 000-schema.sql creates every table at the
# current version in one shot. Only an ALREADY-DEPLOYED cluster needs the
# migrations, and there the Job is a silent no-op: schema.sql is all CREATE
# TABLE IF NOT EXISTS and its version row is guarded on the table being empty.
# A cluster that missed a migration stays on the old version reporting success,
# and vantage-writer and vantage-api then refuse to start against it.
#
# Removing a chart file with no counterpart is part of syncing, not a separate
# destructive mode: job-schema.yaml applies every file its globs match, so a
# leftover copy of a deleted migration would be re-applied to every cluster
# forever. Each removal is printed.
#
# An empty migrations directory is a normal state (it is empty at a squashed
# baseline), and sh leaves an unmatched glob as the literal pattern, so the
# migrations loop skips a name that does not exist rather than failing on it.
CHART_FILES := deploy/helm/vantage/files

sync-helm-schema: ## Re-copy deploy/clickhouse DDL into the Helm chart's files/ tree (the chart cannot read outside itself); run by a person after adding a migration
	@set -e; \
	changed=0; \
	sync_one() { \
	  if [ ! -f "$$1" ]; then echo "sync-helm-schema: missing source $$1" >&2; exit 1; fi; \
	  if ! cmp -s "$$1" "$$2"; then cp "$$1" "$$2"; echo "  updated $$2"; changed=1; fi; \
	}; \
	mkdir -p $(CHART_FILES)/schema $(CHART_FILES)/dictionaries; \
	sync_one deploy/clickhouse/schema.sql $(CHART_FILES)/schema/000-schema.sql; \
	for m in deploy/clickhouse/migrations/*.sql; do \
	  [ -e "$$m" ] || continue; \
	  sync_one "$$m" "$(CHART_FILES)/schema/$$(basename $$m)"; \
	done; \
	for d in asnames asnames_meta; do \
	  sync_one "deploy/clickhouse/$$d.sql" "$(CHART_FILES)/dictionaries/$$d.sql"; \
	done; \
	for f in $(CHART_FILES)/schema/*.sql; do \
	  b=$$(basename $$f); \
	  if [ "$$b" != "000-schema.sql" ] && [ ! -f "deploy/clickhouse/migrations/$$b" ]; then \
	    rm "$$f"; echo "  removed $$f (no counterpart in deploy/clickhouse/migrations)"; changed=1; \
	  fi; \
	done; \
	for f in $(CHART_FILES)/dictionaries/*.sql; do \
	  b=$$(basename $$f); \
	  if [ ! -f "deploy/clickhouse/$$b" ]; then \
	    rm "$$f"; echo "  removed $$f (no counterpart in deploy/clickhouse)"; changed=1; \
	  fi; \
	done; \
	if [ $$changed -eq 0 ]; then echo "sync-helm-schema: already in sync"; \
	else echo "sync-helm-schema: chart updated -- commit the files above"; fi

ASNAMES_DIR      := deploy/dev/asnames
ASNAMES_URL      := https://ftp.ripe.net/ripe/asnames/asn.txt
ASNAMES_RAW      := $(ASNAMES_DIR)/asn.txt
ASNAMES_TSV      := $(ASNAMES_DIR)/asn.tsv
ASNAMES_META     := $(ASNAMES_DIR)/asn.meta
ASNAMES_META_TSV := $(ASNAMES_DIR)/asnames_meta.tsv

fetch-asnames: ## Fetch RIPE's AS holder names into deploy/dev/asnames (raw file, parsed TSV, one-row date TSV, fetch-date sidecar); run by a person, no daemon calls this
	@mkdir -p $(ASNAMES_DIR)
	@old_lines=""; old_meta=""; \
	if [ -f $(ASNAMES_RAW) ]; then old_lines=$$(wc -l < $(ASNAMES_RAW) | tr -d ' '); fi; \
	if [ -f $(ASNAMES_META) ]; then old_meta=$$(awk '{printf "%s; ", $$0}' $(ASNAMES_META) | sed 's/; $$//'); fi; \
	tmp_raw=$$(mktemp -p $(ASNAMES_DIR) asn.txt.XXXXXX) && \
	tmp_tsv=$$(mktemp -p $(ASNAMES_DIR) asn.tsv.XXXXXX) && \
	tmp_meta=$$(mktemp -p $(ASNAMES_DIR) asn.meta.XXXXXX) && \
	tmp_meta_tsv=$$(mktemp -p $(ASNAMES_DIR) asnames_meta.tsv.XXXXXX) && \
	tmp_hdr=$$(mktemp) && \
	trap 'rm -f "$$tmp_raw" "$$tmp_tsv" "$$tmp_meta" "$$tmp_meta_tsv" "$$tmp_hdr"' EXIT && \
	if ! curl -fsS -D "$$tmp_hdr" -o "$$tmp_raw" $(ASNAMES_URL); then \
	  echo "fetch-asnames: download from $(ASNAMES_URL) failed -- $(ASNAMES_RAW) left untouched" >&2; \
	  exit 1; \
	fi && \
	fetched_at=$$(date -u +%Y-%m-%dT%H:%M:%SZ) && \
	raw_lines=$$(wc -l < "$$tmp_raw" | tr -d ' ') && \
	case "$$raw_lines" in ''|*[!0-9]*) \
	  echo "fetch-asnames: could not read a line count for the download -- got \"$$raw_lines\"" >&2; \
	  exit 1;; \
	esac && \
	if [ "$$raw_lines" -lt 100000 ]; then \
	  echo "fetch-asnames: downloaded file has only $$raw_lines lines (expected 120,000+) -- refusing what looks like a truncated dataset" >&2; \
	  exit 1; \
	fi && \
	last_modified=$$(grep -i '^Last-Modified:' "$$tmp_hdr" | tail -1 | tr -d '\r' | cut -d' ' -f2-) && \
	if [ -z "$$last_modified" ]; then \
	  echo "fetch-asnames: response carried no Last-Modified header -- refusing, since that header is the only publication date this file has" >&2; \
	  exit 1; \
	fi && \
	printf '0\t%s\n' "$$last_modified" > "$$tmp_meta_tsv" && \
	sed -E 's/^([0-9]+) (.*), ([A-Z]{2})$$/\1\t\2\t\3/' "$$tmp_raw" > "$$tmp_tsv" && \
	tsv_lines=$$(wc -l < "$$tmp_tsv" | tr -d ' ') && \
	case "$$tsv_lines" in ''|*[!0-9]*) \
	  echo "fetch-asnames: could not read a line count for the parsed TSV -- got \"$$tsv_lines\"" >&2; \
	  exit 1;; \
	esac && \
	malformed=$$(awk -F'\t' 'NF != 3' "$$tmp_tsv" | wc -l | tr -d ' ') && \
	case "$$malformed" in ''|*[!0-9]*) \
	  echo "fetch-asnames: could not count malformed rows in the parsed TSV -- got \"$$malformed\"" >&2; \
	  exit 1;; \
	esac && \
	if [ "$$tsv_lines" != "$$raw_lines" ] || [ "$$malformed" != "0" ]; then \
	  echo "fetch-asnames: parse produced $$tsv_lines rows ($$malformed malformed) from $$raw_lines raw lines -- refusing to write a mismatched TSV" >&2; \
	  exit 1; \
	fi && \
	printf 'Last-Modified: %s\nFetched: %s\n' "$$last_modified" "$$fetched_at" > "$$tmp_meta" && \
	if [ -n "$$old_lines" ]; then \
	  echo "fetch-asnames: replacing $(ASNAMES_RAW) ($$old_lines lines; previously $$old_meta)"; \
	fi && \
	chmod 644 "$$tmp_raw" "$$tmp_tsv" "$$tmp_meta" "$$tmp_meta_tsv" && \
	mv "$$tmp_raw" $(ASNAMES_RAW) && \
	mv "$$tmp_tsv" $(ASNAMES_TSV) && \
	mv "$$tmp_meta" $(ASNAMES_META) && \
	mv "$$tmp_meta_tsv" $(ASNAMES_META_TSV) && \
	echo "fetch-asnames: wrote $$raw_lines lines to $(ASNAMES_RAW) and $(ASNAMES_TSV) (published $$last_modified, fetched $$fetched_at); $(ASNAMES_META_TSV) carries the same published date for deploy/clickhouse/asnames_meta.sql"

# restore-archive-backup copies vantage_archive_backup into vantage.
#
# Not listed in "make help" (its own target line below carries a single #,
# not the ## help greps for): vantage_archive_backup is the maintainer's own
# database, holding one retired lab's only surviving capture, and this
# target has nothing to offer a contributor who does not have it. Still
# defined and runnable by name -- it is not a secret, just not a suggestion.
#
# It exists because the copy is three statements, two of them long enough that
# pasting them is its own failure mode -- one was pasted into a chat and never
# reached the server at all, and nothing noticed for a day because the symptom
# is a screen that renders its empty state correctly.
#
# The two long ones are long because the schema moved after the backup was
# taken. peer_events gained `view_lost` in its `kind` enum on 2026-09-04, so
# the column round-trips through String and lets the target enum parse BY NAME
# rather than by ordinal; route_unicast DROPPED an `end_of_rib` column the
# backup still carries, so naming the live columns skips it and makes column
# ORDER drift irrelevant too. The other seven tables are byte-identical and
# take SELECT *.
#
# Every statement is an INSERT. Nothing here drops, truncates or writes to
# vantage_archive_backup, which is the only surviving capture of a retired
# multi-vendor CML lab -- 139 ls_nodes / 229 ls_links / 125 ls_prefixes of
# OSPFv2 from routers that no longer exist. It cannot be recaptured.
#
# The guard refuses a second run rather than duplicating rows. These are
# ReplacingMergeTree tables, so duplicates would collapse on merge and the
# counts would be right EVENTUALLY -- which is worse than an error, because
# every count in between is wrong and nothing says so.
#
# It guards on ALL NINE destination tables, not on one of them. It used to
# read vantage.ls_nodes alone, which is filled SIXTH: peer_events and
# route_unicast are inserted before it, so a run that died partway through
# those two left ls_nodes at 0 and a re-run duplicated everything already
# copied -- the exact silent-miscount case the paragraph above argues is
# worse than an error, introduced by the guard meant to prevent it.
#
# It also distinguishes "cannot read the table" from "the table has rows".
# A wrong CH_EXEC (a renamed container, a stopped one) makes the command
# substitution empty, and `test "" = "0"` is false, so the old guard refused
# with "vantage.ls_nodes is not empty" -- safe, and the wrong cause. A count
# that is not a non-negative integer is now its own refusal, naming CH_EXEC.
CH_EXEC ?= docker exec vantage-clickhouse-1 clickhouse-client
RESTORE_SIMPLE_TABLES = route_vpn route_evpn stats_events ls_nodes ls_links ls_prefixes ls_events
# Every table this target writes to, in insertion order. One list, so the
# guard, the loop and the summary below cannot describe different sets.
RESTORE_ALL_TABLES = peer_events route_unicast $(RESTORE_SIMPLE_TABLES)

restore-archive-backup: # Copy vantage_archive_backup into vantage (INSERT only, refuses if already restored); maintainer-only, deliberately absent from "make help"
	@for t in $(RESTORE_ALL_TABLES); do \
	  n="$$($(CH_EXEC) -q "SELECT count() FROM vantage.$$t")"; \
	  case "$$n" in ''|*[!0-9]*) \
	    echo "refusing: cannot read vantage.$$t -- CH_EXEC ($(CH_EXEC)) answered \"$$n\""; \
	    exit 1;; \
	  esac; \
	  test "$$n" = "0" || \
	    { echo "refusing: vantage.$$t holds $$n rows, so this has already run at least in part"; \
	      exit 1; }; \
	done
	@n="$$($(CH_EXEC) -q 'SELECT count() FROM vantage_archive_backup.ls_nodes')"; \
	case "$$n" in ''|*[!0-9]*) \
	  echo "refusing: cannot read vantage_archive_backup.ls_nodes -- CH_EXEC ($(CH_EXEC)) answered \"$$n\""; \
	  exit 1;; \
	esac; \
	test "$$n" != "0" || \
	  { echo "refusing: vantage_archive_backup.ls_nodes is empty -- nothing to restore"; exit 1; }
	@echo "restoring peer_events (kind enum via String, by name not ordinal)..."
	@$(CH_EXEC) -q "INSERT INTO vantage.peer_events SELECT collector_id, router_ip, router_sysname, peer_ip, rib, peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, parse_flags, stream_seq, toString(kind) AS kind, local_ip, local_port, remote_port, down_reason, cap_four_byte_as FROM vantage_archive_backup.peer_events"
	@echo "restoring route_unicast (live columns named, skipping the dropped end_of_rib)..."
	@$(CH_EXEC) -q "INSERT INTO vantage.route_unicast SELECT collector_id, router_ip, router_sysname, peer_ip, rib, peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, parse_flags, stream_seq, family, prefix, path_id, is_withdraw, origin, as_path, next_hop, med, local_pref, communities, ext_communities, route_targets, large_communities FROM vantage_archive_backup.route_unicast"
	@for t in $(RESTORE_SIMPLE_TABLES); do \
	  echo "restoring $$t..."; \
	  $(CH_EXEC) -q "INSERT INTO vantage.$$t SELECT * FROM vantage_archive_backup.$$t" || exit 1; \
	done
	@echo "--- restored ---"
	@for t in $(RESTORE_ALL_TABLES); do \
	  printf '%-16s %s\n' "$$t" "$$($(CH_EXEC) -q "SELECT count() FROM vantage.$$t")"; \
	done
