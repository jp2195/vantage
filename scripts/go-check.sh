#!/usr/bin/env bash
# The Go half of CI's gate -- compile every package, vet, and run the unit
# tests -- as one command with one line of output.
#
# It exists because `go build ./...` is a command that reports success for
# doing nothing. Run from a subdirectory with no Go files in it (ui/, deploy/,
# docs/) it prints `go: warning: "./..." matched no packages` and EXITS 0. On
# this repo that has fooled four different agents, three of them after being
# warned about it in writing, because the warning goes to stderr and the exit
# code -- the thing everyone actually checks -- says the build passed.
#
# Two defenses, because being right by construction is not the same as staying
# right: this script cd's to the repo root so the wrong-directory case cannot
# arise, AND it asserts the package count is non-zero, so if that ever becomes
# false again the run FAILS instead of congratulating you.
set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)" || exit 2

LOG="${TMPDIR:-/tmp}/vantage-check-$(date +%Y%m%d-%H%M%S).log"
: > "$LOG"

# RACE=1 mirrors CI's `go test -race ./...`; the default omits it so the common
# case stays fast. `make test` is the full gate (race AND a live ClickHouse).
TEST_FLAGS=(-count=1)
[ "${RACE:-0}" = "1" ] && TEST_FLAGS+=(-race)

fail() {
  printf 'FAIL: %s\n\n' "$1"
  tail -n 25 "$LOG"
  printf '\nfull log: %s\nNext: %s\n' "$LOG" "$2"
  exit 1
}

PKGS=$(go list ./... 2>>"$LOG" | wc -l | tr -d ' ')
if [ "$PKGS" -eq 0 ]; then
  fail "go list ./... matched no packages in $(pwd)" \
    "if an error is shown above, the module is broken -- fix it; if not, this directory is not the vantage Go module"
fi

go build ./... >>"$LOG" 2>&1 || fail "go build ./..." "fix the compile errors above, then re-run 'make check'"
go vet   ./... >>"$LOG" 2>&1 || fail "go vet ./..."   "fix the vet findings above, then re-run 'make check'"

# -v so every skip is on a line of its own in the log. Without it `go test`
# prints nothing for a skipped test, and a machine with no ClickHouse reports
# the same "ok" for every package whether its SQL was exercised or not.
if ! go test ./... -v "${TEST_FLAGS[@]}" >>"$LOG" 2>&1; then
  printf 'FAIL: go test\n\n'
  grep -E '^(FAIL|--- FAIL|\s+--- FAIL)' "$LOG" | head -n 20
  printf '\nfull log: %s\nNext: %s\n' "$LOG" \
    "re-run one package with 'go test ./<pkg> -run <TestName> -v' from the repo root"
  exit 1
fi

TESTED=$(grep -c '^ok' "$LOG")
SKIPPED=$(grep -cE '^\s*--- SKIP' "$LOG")
# chtest's skip message ("    x_test.go:12: chtest: ...", printed just above
# its --- SKIP line) is the one that means "no ClickHouse": count those
# apart, since they are the skips a developer can do something about.
CH_SKIPPED=$(grep -cE '^\s+[^ ]+\.go:[0-9]+: chtest:' "$LOG")
printf 'ok  packages=%s  build=clean  vet=clean  tested=%s  skipped=%s%s%s\n' \
  "$PKGS" "$TESTED" "$SKIPPED" \
  "$([ "$CH_SKIPPED" -gt 0 ] && echo " ($CH_SKIPPED need ClickHouse: start the dev stack or run 'make test')")" \
  "$([ "${RACE:-0}" = "1" ] && echo '  race=on')"
rm -f "$LOG"
