#!/usr/bin/env bash
# Builds the public repository's tree: the given ref (default main) minus
# every path in scripts/internal-paths.txt, as a fresh repository with one
# commit and no other history.
#
#   scripts/export-public-tree.sh <empty-output-dir> [ref]
#
# It only writes into the output directory, and it refuses a directory that
# is not empty. The output has no remote; publishing it is a separate,
# deliberate step. Run the checks it prints before pushing anything.
set -euo pipefail

out=${1:?usage: export-public-tree.sh <empty-output-dir> [ref]}
ref=${2:-main}
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

if [ -e "$out" ] && [ -n "$(ls -A "$out" 2>/dev/null)" ]; then
  echo "refusing: $out is not empty" >&2
  exit 2
fi
git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || { echo "no such ref: $ref" >&2; exit 2; }

mapfile -t internal < <(grep -vE '^\s*(#|$)' scripts/internal-paths.txt)
[ "${#internal[@]}" -gt 0 ] || { echo "scripts/internal-paths.txt lists no paths" >&2; exit 2; }

mkdir -p "$out"
git archive --format=tar "$ref" | tar -x -C "$out"
for p in "${internal[@]}"; do
  rm -rf -- "${out:?}/$p"
done

# A leftover path, or a published file still pointing at an excluded one,
# means the list and the tree disagree. Stop before creating any history.
for p in "${internal[@]}"; do
  [ ! -e "$out/$p" ] || { echo "still present after export: $p" >&2; exit 1; }
done

git -C "$out" init -q -b main
git -C "$out" add -A
git -C "$out" -c user.name=jp2195 \
  -c user.email=24376525+jp2195@users.noreply.github.com \
  commit -q -m "Initial public release"

echo "exported $(git rev-parse --short "$ref") ($ref) to $out as $(git -C "$out" rev-parse --short HEAD)"
echo "excluded: ${internal[*]}"
