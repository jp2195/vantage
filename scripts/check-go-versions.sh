#!/usr/bin/env bash
# The Go version is written in one place: go.mod's `toolchain goX.Y.Z` line.
# Everything that builds Go reads it from there, so a Go bump (Renovate's
# included) is a one-line change that nothing else can drift from:
#
#   - workflows: actions/setup-go with `go-version-file: go.mod`, which
#     installs exactly the toolchain line's version.
#   - Dockerfiles: each golang build stage clears the image's
#     GOTOOLCHAIN=local and runs `go env -w GOTOOLCHAIN=<toolchain line>`,
#     so go fetches and uses exactly that version whatever Go the base
#     image holds.
#
# This script fails if that stops being true: a hard-coded `go-version:`
# in a workflow, a setup-go step without go-version-file: go.mod, or a
# golang stage that does not read its toolchain from go.mod. Before this,
# the version lived in seven places, and v0.1.1's images were built on
# go1.27.3 while go.mod and the CLI were on the go1.27.4 security release.
#
# Text only, no network. Usage: scripts/check-go-versions.sh (exit 1 = drift)
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

bad=0
fail() { printf '  FAIL  %s\n' "$1"; bad=1; }
pass() { printf '  ok    %s\n' "$1"; }

tc=$(awk '$1 == "toolchain" { print $2 }' go.mod)
if [ -z "$tc" ]; then
  fail "go.mod has no toolchain line; the workflows and Dockerfiles read the Go version from it"
else
  pass "go.mod toolchain $tc"
fi

# Workflows: every setup-go step reads go.mod, none hard-codes a version.
for wf in .github/workflows/*.yml .github/workflows/*.yaml; do
  [ -f "$wf" ] || continue
  while IFS=: read -r line _; do
    fail "$wf:$line hard-codes go-version; use go-version-file: go.mod"
  done < <(grep -nE '^[[:space:]]*go-version:' "$wf" || true)
  uses=$(grep -cE 'uses:[[:space:]]*actions/setup-go@' "$wf" || true)
  [ "$uses" = 0 ] && continue
  reads=$(grep -cE '^[[:space:]]*go-version-file:[[:space:]]*["'\'']?go\.mod["'\'']?[[:space:]]*$' "$wf" || true)
  if [ "$reads" -lt "$uses" ]; then
    fail "$wf: $uses setup-go step(s), only $reads with go-version-file: go.mod"
  else
    pass "$wf: $uses setup-go step(s) read go.mod"
  fi
done

# Dockerfiles: every golang build stage takes its toolchain from go.mod.
found=0
while IFS= read -r df; do
  stages=$(awk 'toupper($1) == "FROM" { for (i = 2; i <= NF; i++) if ($i !~ /^--/) { if ($i ~ /^(docker\.io\/)?(library\/)?golang[:@]/) n++; break } } END { print n + 0 }' "$df")
  [ "$stages" = 0 ] && continue
  found=1
  clears=$(grep -cE '^ENV[[:space:]]+GOTOOLCHAIN=[[:space:]]*$' "$df" || true)
  reads=$(grep -cF "awk '\$1 == \"toolchain\" { print \$2 }' go.mod" "$df" || true)
  sets=$(grep -cF 'go env -w GOTOOLCHAIN="$tc"' "$df" || true)
  if [ "$clears" -ge "$stages" ] && [ "$reads" -ge "$stages" ] && [ "$sets" -ge "$stages" ]; then
    pass "$df: $stages golang stage(s) take the toolchain from go.mod"
  else
    fail "$df: $stages golang stage(s), but ENV GOTOOLCHAIN= x$clears, toolchain read x$reads, go env -w x$sets (each stage needs all three)"
  fi
done < <(git ls-files 'Dockerfile*' '*/Dockerfile*')

if [ "$found" = 0 ]; then
  fail "no Dockerfile has a golang build stage; this check's pattern is stale"
fi

if [ "$bad" != 0 ]; then
  cat >&2 <<'EOF'

The Go version must come only from go.mod's toolchain line.
- Workflows: actions/setup-go with `go-version-file: go.mod`, no `go-version:`.
- Dockerfiles: after `COPY go.mod go.sum ./` in each golang stage,
    ENV GOTOOLCHAIN=
    RUN tc=$(awk '$1 == "toolchain" { print $2 }' go.mod) && test -n "$tc" && \
        go env -w GOTOOLCHAIN="$tc" && go mod download && go version
EOF
  exit 1
fi
echo "The Go version comes only from go.mod ($tc)."
