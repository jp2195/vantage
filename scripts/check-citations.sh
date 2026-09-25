#!/usr/bin/env bash
# Fails if a published file cites internal process material: a numbered
# task, a report or brief file, a ledger, a numbered ruling or finding, a
# review round, an internal phase, or a dated internal audit.
#
# check-standalone.sh catches internal PATHS and commit SHAs, and it cannot
# see these: "task-3-report.md" is under no path it lists and is not a dated
# file name, and "a Task 0 audit" names no file at all.
#
#   scripts/check-citations.sh [--sweep] [FILE...]
#
# With no FILE, every published file is scanned: git ls-files, minus the
# paths in scripts/internal-paths.txt and minus this guard's own files. A
# file not yet added to git is not scanned until it is, and CI sees it then.
#
# Two pattern sets:
#
#   scripts/citation-patterns.txt        run always, and in CI through make
#                                        standalone-check. Only patterns no
#                                        contributor writing ordinary prose
#                                        would plausibly type.
#   scripts/citation-sweep-patterns.txt  added by --sweep. Broad patterns for
#                                        a one-off pre-launch sweep, read by a
#                                        person: they flag ordinary English
#                                        too, and run in no CI path.
#
# A hit that is legitimate prose goes in scripts/citation-allow.txt as
# path:regex, where regex (grep -E) matches the reported line's text.
#
# Every pattern runs twice: over each line, and over each comment block
# joined across its line wraps, since a comment wrapped as "Task" / "3's
# review" matches on neither line alone. A match that spans a line break is
# reported at the line where it starts, as well as any match found within
# a single line, so no hit hides another.
#
# Requires bash 4+, GNU grep and GNU xargs; CI runs it on Linux.
#
# CITATION_PATTERNS, CITATION_SWEEP_PATTERNS and CITATION_ALLOW override the
# three files; check-citations-test.sh uses them.
set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)" || exit 2

sweep=0
if [ "${1:-}" = "--sweep" ]; then
  sweep=1
  shift
fi

if [ "$(git rev-parse --is-inside-work-tree 2>/dev/null)" != true ]; then
  echo "FAIL: $(pwd) is not inside a git work tree, so there is no file list to scan"
  exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Regular files, not process substitutions: xargs may run grep more than
# once, and a pipe can be read only once.
pats=$work/patterns
patfile=${CITATION_PATTERNS:-scripts/citation-patterns.txt}
sweepfile=${CITATION_SWEEP_PATTERNS:-scripts/citation-sweep-patterns.txt}
sets=("$patfile")
[ "$sweep" -eq 1 ] && sets+=("$sweepfile")
for f in "${sets[@]}"; do
  if [ ! -r "$f" ]; then
    echo "FAIL: cannot read pattern file $f"
    exit 2
  fi
  grep -vE '^\s*(#|$)' "$f" >> "$pats"
done
if [ ! -s "$pats" ]; then
  echo "FAIL: $patfile lists no patterns"
  exit 2
fi
# An invalid expression makes grep exit 2, which would otherwise read as
# "no match" and pass every file.
bad=0
while IFS= read -r p; do
  grep -qE -e "$p" /dev/null 2>/dev/null
  if [ "$?" -eq 2 ]; then
    echo "FAIL: invalid pattern: $p"
    bad=1
  fi
done < "$pats"
[ "$bad" -eq 0 ] || exit 2

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  mapfile -t internal < <(grep -vE '^\s*(#|$)' scripts/internal-paths.txt)
  # The same guard check-standalone.sh has: with no internal paths listed,
  # the internal documents themselves would be scanned as published files.
  if [ "${#internal[@]}" -eq 0 ]; then
    echo "FAIL: scripts/internal-paths.txt lists no paths"
    exit 2
  fi
  # The guard's own files and fixtures, and check-standalone.sh, describe
  # the citation categories on purpose. .gitignore/.dockerignore name
  # internal scratch ("ledgers, briefs") to keep it out of commits, which
  # points no reader anywhere.
  exclude=(':!scripts/citation-patterns.txt' ':!scripts/citation-sweep-patterns.txt'
           ':!scripts/citation-allow.txt' ':!scripts/check-citations.sh'
           ':!scripts/check-citations-test.sh' ':!scripts/testdata/citations'
           ':!scripts/check-standalone.sh' ':!.gitignore' ':!.dockerignore')
  for p in "${internal[@]}"; do exclude+=(":!$p"); done
  # NUL-separated, so a name holding a tab or a newline is one name.
  mapfile -d '' -t files < <(git ls-files -z -- . "${exclude[@]}")
fi
if [ "${#files[@]}" -eq 0 ]; then
  echo "FAIL: no files to scan"
  exit 2
fi

# Text files only, as awk operands. A relative name gets "./" so that awk
# cannot read "a=b.md" as a variable assignment or "-" as standard input.
mapfile -d '' -t text < <(printf '%s\0' "${files[@]}" \
  | LC_ALL=C xargs -0 -r grep -lIZ -e '' -- 2>/dev/null)
operands=()
for f in "${text[@]}"; do
  case "$f" in /*) operands+=("$f") ;; *) operands+=("./$f") ;; esac
done

# One awk pass writes two files, each one record per line (appending, since
# xargs may run awk more than once):
#   lines:  name TAB line TAB text            every line of every file
#   blocks: name TAB first-line TAB len,len,... TAB joined text
# A block is a run of two or more comment lines: lines starting with //, #,
# --, * or /*, any line inside a /* */ or <!-- --> block opened at the start
# of a line (a "/*" inside a Go string is not a comment), and in Markdown
# and YAML every non-blank line. Markers are stripped and the lines joined
# by one space, so a match's byte offset maps back to the line it starts
# on. Names are escaped (\\, \t, \n) so that a record is always one line.
# LC_ALL=C keeps awk's lengths and grep's offsets both in bytes.
split='
function esc(s,   o, i, c) {
  o = ""
  for (i = 1; i <= length(s); i++) {
    c = substr(s, i, 1)
    if (c == "\\") o = o "\\\\"; else if (c == "\t") o = o "\\t"
    else if (c == "\n") o = o "\\n"; else o = o c
  }
  return o
}
function flush(   i, lens, joined) {
  if (n >= 2) {
    lens = len[1]; joined = seg[1]
    for (i = 2; i <= n; i++) { lens = lens "," len[i]; joined = joined " " seg[i] }
    printf "%s\t%d\t%s\t%s\n", name, start, lens, joined >> blocks
  }
  n = 0
}
FNR == 1 { flush(); name = FILENAME; if (substr(name, 1, 2) == "./") name = substr(name, 3)
  name = esc(name); incb = 0; inh = 0; prose = (FILENAME ~ /\.(md|ya?ml)$/) }
{
  line = $0; gsub(/\t/, " ", line)
  printf "%s\t%d\t%s\n", name, FNR, line >> lines
  iscomment = 0
  if (incb || inh) iscomment = 1
  else if (line ~ /^ *(\/\/+|#+|--|\*+|\/\*+)( |$)/ || line ~ /^ *<!--/) iscomment = 1
  else if (prose && line !~ /^ *$/) iscomment = 1
  if (line ~ /^ *\/\*/ && line !~ /\*\//) incb = 1
  if (incb && line ~ /\*\//) incb = 0
  if (line ~ /^ *<!--/ && line !~ /-->/) inh = 1
  if (inh && line ~ /-->/) inh = 0
  if (!iscomment) { flush(); next }
  sub(/^ *(\/\/+|#+|--|\*+\/?|\/\*+|<!--)? */, "", line)
  sub(/ *(\*\/|-->) *$/, "", line)
  sub(/ +$/, "", line)
  if (line == "") { flush(); next }
  if (n == 0) start = FNR
  seg[++n] = line; len[n] = length(line)
}
END { flush() }'
: > "$work/lines"
: > "$work/blocks"
if [ "${#operands[@]}" -eq 0 ]; then
  echo "FAIL: none of the ${#files[@]} files to scan is a readable text file"
  exit 2
fi
if ! printf '%s\0' "${operands[@]}" | LC_ALL=C xargs -0 -r awk \
    -v lines="$work/lines" -v blocks="$work/blocks" -- "$split"; then
  echo "FAIL: could not read the files to scan"
  exit 2
fi
# Every text file has at least one line, so an empty result means nothing
# was read -- which must not pass as "nothing cited".
if [ ! -s "$work/lines" ]; then
  echo "FAIL: read no lines from ${#operands[@]} text files"
  exit 2
fi

# Hits, one per record: name TAB line TAB text.
# Pass 1: line by line.
cut -f3- "$work/lines" > "$work/text1"
LC_ALL=C grep -nE -f "$pats" "$work/text1" | cut -d: -f1 > "$work/hit1"
LC_ALL=C awk -F'\t' -v hits="$work/hit1" '
  BEGIN { while ((getline r < hits) > 0) want[r] = 1 }
  (NR in want) { print }' "$work/lines" > "$work/hits"

# Pass 2: matches that span a line break in a joined block.
cut -f4- "$work/blocks" > "$work/text2"
LC_ALL=C grep -nobE -f "$pats" "$work/text2" \
  | LC_ALL=C awk -F'\t' -v meta="$work/blocks" '
    BEGIN {
      off = 0
      while ((getline l < meta) > 0) {
        r++; split(l, f, "\t")
        name[r] = f[1]; first[r] = f[2]; lens[r] = f[3]; recoff[r] = off
        t = l; for (i = 1; i <= 3; i++) sub(/^[^\t]*\t/, "", t)
        text[r] = t; off += length(t) + 1
      }
    }
    {
      # grep -nob prints record:offset:match, and the match may hold colons.
      i1 = index($0, ":"); rec = substr($0, 1, i1 - 1); rest = substr($0, i1 + 1)
      i2 = index(rest, ":"); o = substr(rest, 1, i2 - 1) - recoff[rec]
      m = substr(rest, i2 + 1)
      e = o + length(m) - 1
      k = split(lens[rec], L, ",")
      pos = 0; sl = el = 0
      for (i = 1; i <= k; i++) {
        if (!sl && o < pos + L[i] + 1) sl = i
        if (!el && e < pos + L[i] + 1) el = i
        pos += L[i] + 1
      }
      # A match inside one line is pass 1'"'"'s.
      if (sl == el) next
      s = 0; for (i = 1; i < sl; i++) s += L[i] + 1
      w = 0; for (i = sl; i <= el; i++) w += L[i] + 1
      printf "%s\t%d\t%s\n", name[rec], first[rec] + sl - 1, substr(text[rec], s + 1, w - 1)
    }' >> "$work/hits"

# Allowed hits: path:regex, the regex matched against the hit's text.
allowfile=${CITATION_ALLOW:-scripts/citation-allow.txt}
allowed=()
if [ -f "$allowfile" ]; then
  mapfile -t allowed < <(grep -vE '^\s*(#|$)' "$allowfile")
fi
out=()
while IFS=$'\t' read -r name line txt; do
  skip=0
  for a in "${allowed[@]}"; do
    if [ "${a%%:*}" = "$name" ] && printf '%s\n' "$txt" | grep -qE -e "${a#*:}"; then
      skip=1
      break
    fi
  done
  [ "$skip" -eq 1 ] || out+=("$name:$line:$txt")
done < <(sort -t$'\t' -k1,1 -k2,2n "$work/hits")

mode=""
[ "$sweep" -eq 1 ] && mode=" (sweep)"
if [ "${#out[@]}" -gt 0 ]; then
  echo "FAIL$mode: ${#out[@]} published lines cite internal process material a public reader cannot open:"
  printf '  %s\n' "${out[@]}"
  # Not indented, so that counting "^  " counts hits and nothing else.
  echo "Say what the cited material established, in the comment itself;"
  echo "scripts/citation-patterns.txt says what each pattern is for. A hit that is"
  echo "ordinary prose can be listed as path:regex in scripts/citation-allow.txt."
  exit 1
fi
echo "ok  citations$mode: ${#files[@]} files, no internal process citations"
