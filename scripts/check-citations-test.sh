#!/usr/bin/env bash
# Tests scripts/check-citations.sh against its fixtures.
#
#   1. CI set: every line of must-fail.txt is reported, at its own line,
#      and must-pass.txt -- ordinary sentences, and near misses -- is clean.
#      Every CI pattern is load-bearing: with any one deleted, at least one
#      must-fail line goes unreported.
#   2. Sweep set (--sweep): the same, with sweep-must-fail.txt and
#      sweep-must-pass.txt, for every sweep pattern.
#   3. Wrapped citations (wrapped/): each is reported exactly at the line
#      where it starts, in //, /* */, <!-- -->, Markdown and YAML, and a
#      line with its own hit still reports a wrapped one that starts there.
#   4. A throwaway git repository: file names holding a tab, a newline, a
#      leading dash or an "=", the allow file, and every way the guard must
#      fail closed -- no git work tree, no files, an invalid pattern, no
#      internal paths.
set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)" || exit 2

fix=scripts/testdata/citations
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# reported_lines FILE OUTPUT: the line numbers OUTPUT reports for FILE.
reported_lines() {
  printf '%s\n' "$2" | sed -n "s|^  $1:\([0-9]*\):.*|\1|p"
}

# check_set NAME FAILFILE PASSFILE SETFILE ENVVAR [--sweep]
check_set() {
  local name=$1 failf=$2 passf=$3 setf=$4 var=$5 flag=${6:-}
  local total out rc got want before=$fail
  total=$(grep -cvE '^\s*$' "$failf")
  out=$(scripts/check-citations.sh $flag "$failf"); rc=$?
  got=$(reported_lines "$failf" "$out" | sort -n | tr '\n' ' ')
  want=$(seq 1 "$total" | tr '\n' ' ')
  if [ "$rc" -ne 1 ] || [ "$got" != "$want" ]; then
    echo "FAIL: $name: want exit 1 and lines $want reported once each, got exit $rc and lines $got:"
    printf '%s\n' "$out"; fail=1
  fi
  out=$(scripts/check-citations.sh $flag "$passf"); rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: $name: exit $rc on $passf, want 0:"; printf '%s\n' "$out"; fail=1
  fi
  local pats i
  mapfile -t pats < <(grep -vE '^\s*(#|$)' "$setf")
  for i in "${!pats[@]}"; do
    printf '%s\n' "${pats[@]}" | sed "$((i + 1))d" > "$tmp/pats"
    out=$(env "$var=$tmp/pats" scripts/check-citations.sh $flag "$failf")
    if [ "$(reported_lines "$failf" "$out" | wc -l)" -ge "$total" ]; then
      echo "FAIL: $name: pattern ${pats[$i]} can be deleted and every must-fail line is"
      echo "      still reported; add a must-fail line that only it matches"
      fail=1
    fi
  done
  [ "$fail" -eq "$before" ] && echo "ok  $name: $total must-fail lines caught at their own lines, must-pass clean, ${#pats[@]} patterns each load-bearing"
}

check_set "citations (CI)" "$fix/must-fail.txt" "$fix/must-pass.txt" \
  scripts/citation-patterns.txt CITATION_PATTERNS
check_set "citations (sweep)" "$fix/sweep-must-fail.txt" "$fix/sweep-must-pass.txt" \
  scripts/citation-sweep-patterns.txt CITATION_SWEEP_PATTERNS --sweep

# 3. Wrapped citations, compared as file:line with duplicates kept.
w=$fix/wrapped
mapfile -t wfiles < <(ls "$w" | grep -v '^expected.txt$')
out=$(scripts/check-citations.sh "${wfiles[@]/#/$w/}"); rc=$?
got=$(printf '%s\n' "$out" | sed -n "s|^  $w/\([^:]*\):\([0-9]*\):.*|\1:\2|p" | sort)
if [ "$rc" -ne 1 ] || [ "$got" != "$(sort "$w/expected.txt")" ]; then
  echo "FAIL: wrapped: want exit 1 and exactly $w/expected.txt, got exit $rc:"
  printf '%s\n' "$out"; fail=1
else
  echo "ok  wrapped: $(wc -l < "$w/expected.txt") hits at the lines they start on"
fi

# 4. A throwaway repository. The guard resolves its root from its own
# location, so it is copied in with its pattern files.
repo=$tmp/repo
mkdir -p "$repo/scripts"
cp scripts/check-citations.sh scripts/citation-patterns.txt \
   scripts/citation-sweep-patterns.txt "$repo/scripts/"
printf 'internal\n' > "$repo/scripts/internal-paths.txt"
: > "$repo/scripts/citation-allow.txt"
mkdir -p "$repo/internal"
printf '// Task 1 is internal and never scanned.\n' > "$repo/internal/x.go"
printf '// Done in Task 1.\n' > "$repo/tab	name.go"
printf '// See widget-report.md.\n' > "$repo/new
line.go"
printf '// Written for Task\n// 2 and nothing else.\n' > "$repo/x=y.go"
printf '// Measured on the dev archive.\n' > "$repo/-dash.go"
printf '// Kept per the k8s notes.\n' > "$repo/allowed.go"
printf 'plain text, nothing cited\n' > "$repo/clean.txt"
(cd "$repo" && git init -q && git add -A && git -c user.name=t -c user.email=t@t commit -qm t) \
  || { echo "FAIL: could not build the throwaway repository"; exit 1; }
g() { (cd "$repo" && scripts/check-citations.sh "$@"); }

out=$(g); rc=$?
want=$(printf '%s\n' '-dash.go:1' 'allowed.go:1' 'new\nline.go:1' 'tab\tname.go:1' 'x=y.go:1' | sort)
got=$(printf '%s\n' "$out" | sed -n 's|^  \(.*\):\([0-9]*\):.*|\1:\2|p' | sort)
if [ "$rc" -ne 1 ] || [ "$got" != "$want" ]; then
  echo "FAIL: odd file names: want exit 1 and"; printf '    %s\n' $want
  echo "  got exit $rc:"; printf '%s\n' "$out"; fail=1
else
  echo "ok  odd file names: tab, newline, leading dash and '=' each reported once, escaped"
fi

printf 'allowed.go:per the k8s notes\n' > "$repo/scripts/citation-allow.txt"
out=$(g); rc=$?
if [ "$rc" -ne 1 ] || printf '%s\n' "$out" | grep -q 'allowed.go' \
   || [ "$(printf '%s\n' "$out" | grep -c '^  ')" -ne 4 ]; then
  echo "FAIL: allow file: want allowed.go suppressed and the other 4 hits kept:"
  printf '%s\n' "$out"; fail=1
else
  echo "ok  allow file: the listed hit is suppressed and nothing else"
fi

# fail_closed NAME COMMAND...: the guard must exit non-zero and not say ok.
fail_closed() {
  local name=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [ "$rc" -eq 0 ] || printf '%s\n' "$out" | grep -q '^ok'; then
    echo "FAIL: $name: want a non-zero exit, got $rc:"; printf '%s\n' "$out"; fail=1
  else
    echo "ok  fails closed: $name"
  fi
}
printf '(unclosed\n' > "$tmp/badpats"
fail_closed "invalid pattern" env CITATION_PATTERNS="$tmp/badpats" bash -c "cd '$repo' && scripts/check-citations.sh"
fail_closed "missing sweep file" env CITATION_SWEEP_PATTERNS="$tmp/nope" bash -c "cd '$repo' && scripts/check-citations.sh --sweep"
printf '# nothing\n' > "$repo/scripts/internal-paths.txt"
fail_closed "no internal paths" g
printf '*\n' > "$repo/scripts/internal-paths.txt"
fail_closed "no files to scan" g
mkdir -p "$tmp/nogit/scripts"
cp "$repo/scripts/"* "$tmp/nogit/scripts/"
fail_closed "outside a git work tree" "$tmp/nogit/scripts/check-citations.sh"

exit "$fail"
