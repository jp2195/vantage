#!/usr/bin/env bash
# Validates the artifact `make fetch-asnames` already wrote, against the
# invariants our parse rests on -- invariants that belong to RIPE's asn.txt,
# not to us, and that were measured rather than assumed: every line ends
# ", XX" with a two-letter country code, there are zero duplicate ASNs,
# every first field is a bare integer, and the parse carries every raw line
# through rather than dropping any (measured against the live file:
# 122,442 of 122,442 lines carried through, and this validator's own runs
# have caught four deliberate invariant breaks).
#
# This reads ONLY the two files fetch-asnames already produced -- asn.txt
# (raw) and asn.tsv (parsed) -- and never re-derives what a correct parse
# SHOULD produce from the raw text. A validator that reimplements the parse
# would agree with itself and pass while the shipped path (the Makefile's
# sed) broke, which is the defect class this project keeps finding. Where a
# check below extracts something from the raw file (the row-count check's
# "which line went missing" diagnostic), it does so only to print a helpful
# offending line, never to decide pass or fail.
#
# Usage: check-asnames-format.sh [raw-file] [tsv-file]
#   defaults to deploy/dev/asnames/asn.txt and deploy/dev/asnames/asn.tsv,
#   the paths `make fetch-asnames` writes (see the Makefile's ASNAMES_* vars).
set -uo pipefail

ASNAMES_DIR="${ASNAMES_DIR:-deploy/dev/asnames}"
RAW="${1:-$ASNAMES_DIR/asn.txt}"
TSV="${2:-$ASNAMES_DIR/asn.tsv}"

if [ ! -f "$RAW" ]; then
  echo "asnames canary: $RAW not found -- run 'make fetch-asnames' first" >&2
  exit 2
fi
if [ ! -f "$TSV" ]; then
  echo "asnames canary: $TSV not found -- run 'make fetch-asnames' first" >&2
  exit 2
fi

failures=0

report() {
  # $1 = invariant name, $2 = what broke, $3 = offending line(s) (may be multi-line)
  failures=$((failures + 1))
  echo "asnames canary: INVARIANT VIOLATED -- $1" >&2
  echo "  $2" >&2
  if [ -n "${3:-}" ]; then
    echo "  offending line(s):" >&2
    echo "$3" | sed 's/^/    /' >&2
  fi
}

# Invariant 1: no line was silently dropped. The shipped parse is a plain
# `sed -E 's/.../.../'` over the raw file, which prints every line once
# whether it matched or not -- a non-matching line survives as unparsed
# pass-through (caught by invariant 4 below), not as a gap. A row-count
# mismatch therefore means some OTHER transform ran -- e.g. a future rewrite
# that filters with `awk '/pattern/'` and drops what doesn't match, which is
# exactly the silent failure mode this canary exists to catch before an
# operator meets it as names that quietly stopped appearing.
raw_lines=$(wc -l < "$RAW" | tr -d ' ')
tsv_lines=$(wc -l < "$TSV" | tr -d ' ')
if [ "$raw_lines" != "$tsv_lines" ]; then
  # Diagnostic only, not the verdict above: find one raw ASN that has no
  # match in the TSV's ASN column, using only the leading-digit shape every
  # line's first field has -- not the anchored parse regex -- so this stays
  # a line-finder, not a second implementation of the parse.
  offending=""
  missing_asn=$(comm -23 \
    <(grep -oE '^[0-9]+' "$RAW" | sort -n -u) \
    <(cut -f1 "$TSV" | sort -n -u) 2>/dev/null | head -1)
  if [ -n "$missing_asn" ]; then
    offending=$(grep -m1 -E "^${missing_asn} " "$RAW")
  fi
  report "row count (no silently skipped lines)" \
    "raw file $RAW has $raw_lines lines but $TSV has $tsv_lines rows -- expected equal" \
    "${offending:-raw line count and TSV row count differ by $((raw_lines - tsv_lines)); could not isolate a single missing ASN}"
fi

# Invariants 2-4 are per-row checks over the TSV. One awk pass finds all
# three kinds of violation and prints, for each, the invariant it belongs to,
# the 1-based TSV line number, and the row itself -- so a failure is
# actionable without re-running anything.
#
# The wrapper record below is joined with \037 (unit separator), not a tab:
# the row itself is tab-separated, so splitting the wrapper on tab again
# would cut the offending row down to just its first field instead of
# showing it whole. \037 cannot appear in the source data (RIPE's asn.txt is
# plain text) so it never collides with real content.
US=$(printf '\037')
awk_out=$(mktemp)
trap 'rm -f "$awk_out"' EXIT

awk -F'\t' -v US="$US" '
  {
    asn = $1; name = $2; country = $3
    if (NF < 3 || country !~ /^[A-Z]{2}$/) {
      print "COUNTRY" US NR US $0
    }
    if (asn !~ /^[0-9]+$/) {
      print "ASNINT" US NR US $0
    } else {
      seen[asn]++
      if (seen[asn] == 2) {
        print "DUPASN" US NR US $0
      }
    }
  }
' "$TSV" > "$awk_out"

dup_lines=$(awk -F"$US" '$1 == "DUPASN" {print $2": "$3}' "$awk_out" | head -5)
if [ -n "$dup_lines" ]; then
  report "zero duplicate ASNs" \
    "an ASN in $TSV appears more than once (showing up to 5)" \
    "$dup_lines"
fi

asnint_lines=$(awk -F"$US" '$1 == "ASNINT" {print $2": "$3}' "$awk_out" | head -5)
if [ -n "$asnint_lines" ]; then
  report "ASN parses as an integer" \
    "a row's first field in $TSV is not a bare integer (showing up to 5)" \
    "$asnint_lines"
fi

country_lines=$(awk -F"$US" '$1 == "COUNTRY" {print $2": "$3}' "$awk_out" | head -5)
if [ -n "$country_lines" ]; then
  report "country code is two letters" \
    "a row's third field in $TSV is missing or is not exactly two uppercase letters (showing up to 5)" \
    "$country_lines"
fi

if [ "$failures" -gt 0 ]; then
  echo "asnames canary: FAILED -- $failures invariant(s) broken, see above" >&2
  exit 1
fi

echo "asnames canary: OK -- $raw_lines lines, $tsv_lines rows, 0 duplicate ASNs, every ASN an integer, every country code two letters"
