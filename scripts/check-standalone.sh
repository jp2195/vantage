#!/usr/bin/env bash
# Fails if a published file points at something a reader of the public
# repository cannot see.
#
# The public repository is built from main minus the paths in
# scripts/internal-paths.txt, as a single squashed commit. So two kinds of
# pointer dangle there, and both used to be routine in this codebase:
#
#   1. a reference to an internal path ("see docs/superpowers/ledgers/...")
#   2. a commit SHA ("fixed in d821758"), which names a commit that exists
#      only in the private history
#
# Check 2 treats any 7-40 character hex token that resolves to a commit in
# the current repository as a citation. A hex value that collides with a
# commit prefix by chance can be allowed in scripts/standalone-allow.txt.
set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)" || exit 2

mapfile -t internal < <(grep -vE '^\s*(#|$)' scripts/internal-paths.txt)
if [ "${#internal[@]}" -eq 0 ]; then
  echo "FAIL: scripts/internal-paths.txt lists no paths"
  exit 1
fi

# Published files only: skip the internal paths themselves, and the two files
# that name them on purpose.
exclude=(':!scripts/internal-paths.txt' ':!scripts/check-standalone.sh')
for p in "${internal[@]}"; do exclude+=(":!$p"); done

# A distinctive path (docs/superpowers, design_handoff_vantage) is matched
# anywhere. A plain word (lab) is matched only as a path, "lab/...", with no
# path characters before it: otherwise "label" and "the lab" would count.
pattern=$(printf '%s\n' "${internal[@]}" | while read -r p; do
  esc=$(printf '%s' "$p" | sed 's/[.[\*^$/]/\\&/g')
  if [[ "$p" =~ ^[a-z]+$ ]]; then
    printf '(^|[^A-Za-z0-9_./-])%s/\n' "$esc"
  else
    printf '%s\n' "$esc"
  fi
done | paste -sd'|')
# .gitignore and .dockerignore may name an internal path: ignoring a
# directory points no reader anywhere, and it keeps contributors' own tooling
# out of commits and out of image build contexts.
refs=$(git grep -nE "$pattern" -- . "${exclude[@]}" ':!.gitignore' ':!.dockerignore')

# Hex tokens in text files, minus checksums, lockfiles and captured data,
# where hex is content rather than citation.
allow=$(grep -vE '^\s*(#|$)' scripts/standalone-allow.txt 2>/dev/null || true)
# Hex tokens in text files. Checksums, lockfiles and captured test data are
# skipped, since hex there is content rather than citation. Prose (*.md) is
# always checked, even inside testdata/ or a fixtures directory, because a
# README beside the data is exactly where a citation turns up.
cites=""
while IFS=: read -r file line tok; do
  [ -n "$tok" ] || continue
  case "$file" in
    *.md) ;;
    *.sum|*package-lock.json|*/testdata/*|ui/src/api/fixtures/*) continue ;;
  esac
  if [ "$(git cat-file -t "$tok" 2>/dev/null)" = commit ] \
     && ! grep -qxF "$file:$tok" <<<"$allow"; then
    cites+="$file:$line: $tok"$'\n'
  fi
done < <(git grep -I -noE '\b[0-9a-f]{7,40}\b' -- . "${exclude[@]}")

# A bare filename cites an internal document just as surely as its path does
# ("see 2026-08-29-lab-session-2.md"), and the path check cannot see it. So
# every dated file under an internal path is also searched for by name. Names
# too generic to be a citation (README.md, a lone image) are left to the path
# check.
names=$(git ls-files -- "${internal[@]}" | xargs -r -n1 basename | sort -u \
  | grep -E '^[0-9]{4}-[0-9]{2}-[0-9]{2}-' || true)
if [ -n "$names" ]; then
  bare=$(git grep -nF -f <(printf '%s\n' "$names") -- . "${exclude[@]}" ':!.gitignore' ':!.dockerignore')
  if [ -n "$bare" ]; then
    refs+="${refs:+$'\n'}$bare"
  fi
fi

status=0
if [ -n "$refs" ]; then
  echo "FAIL: published files refer to internal paths (${internal[*]}):"
  echo "$refs" | sed 's/^/  /'
  status=1
fi
if [ -n "$cites" ]; then
  echo "FAIL: published files cite commits that will not exist publicly:"
  printf '%s' "$cites" | sed 's/^/  /'
  echo "  (a coincidental hex value can be listed as file:token in scripts/standalone-allow.txt)"
  status=1
fi
# Internal process citations -- a task number, a report or brief file, a
# ledger -- point at nothing a public reader can open, and none of the path
# checks above can see them. scripts/check-citations.sh has the patterns.
if ! scripts/check-citations.sh; then
  status=1
fi
if [ "$status" -eq 0 ]; then
  echo "ok  standalone: no references to ${#internal[@]} internal paths, no commit citations"
fi
exit "$status"
