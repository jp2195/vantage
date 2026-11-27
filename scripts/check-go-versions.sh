#!/usr/bin/env bash
# Every Go toolchain this repo builds with must be the one go.mod names.
#
# go.mod's `toolchain goX.Y.Z` line is the version the project says it
# compiles with, and it is the one Renovate moves (including for Go security
# releases). Three other kinds of place pin a toolchain on their own, and
# nothing made them follow it:
#
#   - `go-version:` in the workflows (setup-go). Renovate does not update
#     these; they have been bumped by hand.
#   - each Dockerfile's `FROM golang:<tag>@sha256:<digest>`. The tag (1.27)
#     names no patch release, the digest fixes one, and the golang image sets
#     GOTOOLCHAIN=local, so go.mod's toolchain line is ignored inside the
#     build. v0.1.1 shipped images on go1.27.3 this way while go.mod and the
#     CLI were on the go1.27.4 security release.
#
# For a Dockerfile, the patch release is read from the pinned image's own
# GOLANG_VERSION env var, fetched from the registry with
# `docker buildx imagetools inspect` -- the image config only, no layers.
#
# Usage: scripts/check-go-versions.sh   (exit 0 = all agree, 1 = drift)
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

want=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' go.mod)
if [ -z "$want" ]; then
  echo "go.mod has no toolchain line; nothing to check against." >&2
  exit 1
fi
echo "go.mod toolchain: go$want"

bad=0
report() { # file, what, got
  if [ "$3" = "$want" ]; then
    printf '  ok    %-40s %s\n' "$1" "$3"
  else
    printf '  DRIFT %-40s %s (want %s) -- %s\n' "$1" "$3" "$want" "$2"
    bad=1
  fi
}

# setup-go pins.
while IFS=: read -r file line rest; do
  got=$(printf '%s' "$rest" | sed -E 's/.*go-version:[[:space:]]*["'\'']?([^"'\'' #]+).*/\1/')
  report "$file:$line" "setup-go go-version" "$got"
done < <(grep -nE '^[[:space:]]*go-version:' .github/workflows/*.yml || true)

# image_go_version REF sets $go_version to the GOLANG_VERSION baked into REF's config.
# Every Dockerfile usually pins the same digest, so each ref is fetched once
# (a newline-separated "ref version" cache; bash 3.2 on macOS has no
# associative arrays). Docker Hub rate-limits anonymous requests (429), so a
# failed fetch is retried with backoff, and a final failure says so instead
# of passing as "unknown". It sets a variable rather than printing, because
# $(...) would run it in a subshell and lose the cache.
cache=""
image_go_version() {
  local ref=$1 hit out attempt
  hit=$(printf '%s\n' "$cache" | awk -v r="$ref" '$1 == r { print $2; exit }')
  if [ -n "$hit" ]; then go_version=$hit; return 0; fi
  # Odd attempts ask Docker Hub, even ones Google's Docker Hub mirror. The
  # lookup is by digest, which names the content, so the mirror cannot hand
  # back a different image -- it only gets around Hub's rate limit.
  local mirror="mirror.gcr.io/library/golang@${ref#*@}" src
  for attempt in 1 2 3 4; do
    src=$ref; [ $((attempt % 2)) = 0 ] && src=$mirror
    if out=$(docker buildx imagetools inspect "$src" --format '{{json .Image}}' 2>&1); then
      out=$(printf '%s' "$out" | jq -r '[.[]?.config.Env[]?, .config.Env[]?]
               | map(select(startswith("GOLANG_VERSION=")) | ltrimstr("GOLANG_VERSION="))
               | unique | join(",")')
      if [ -z "$out" ]; then
        echo "error: $ref has no GOLANG_VERSION in its config; is it an official golang image?" >&2
        return 1
      fi
      cache="$cache$ref $out"$'\n'
      go_version=$out
      return 0
    fi
    echo "  (lookup of $src failed, attempt $attempt/4: ${out##*: })" >&2
    [ "$attempt" -lt 4 ] && sleep $((attempt * 10))
  done
  echo "error: could not read $ref from the registry; the check cannot tell which Go it holds." >&2
  return 1
}

# Dockerfile golang base images.
found=0
while IFS= read -r file; do
  while IFS=: read -r line ref; do
    found=1
    if [[ "$ref" != *@sha256:* ]]; then
      report "$file:$line" "golang image is not pinned by digest: $ref" "unpinned"
      continue
    fi
    image_go_version "$ref" || exit 1
    report "$file:$line" "$ref" "$go_version"
  done < <(awk '
    toupper($1) == "FROM" {
      for (i = 2; i <= NF; i++) if ($i !~ /^--/) { if ($i ~ /^(docker\.io\/)?(library\/)?golang:/) print FNR ":" $i; break }
    }' "$file")
done < <(git ls-files 'Dockerfile*' '*/Dockerfile*')

if [ "$found" = 0 ]; then
  echo "No golang base image found in any Dockerfile; the pattern above is stale." >&2
  exit 1
fi

if [ "$bad" != 0 ]; then
  cat >&2 <<EOF

Go toolchain drift: every pin above must be go$want, the toolchain go.mod names.
- setup-go: set go-version to "$want".
- Dockerfile: move the golang digest to the one for $want, e.g.
    docker buildx imagetools inspect golang:$want --format '{{json .Manifest.Digest}}'
  (keep the minor tag so Renovate goes on tracking it).
EOF
  exit 1
fi
echo "All Go toolchain pins match go$want."
