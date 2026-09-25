#!/usr/bin/env bash
# Captures real API responses as UI test fixtures.
#
# Fixtures are captured rather than written because a hand-authored fixture
# encodes what someone THOUGHT the server returns. Every field name, null,
# empty array, warning code and dump state in ui/src/api/fixtures came off the
# wire from a running deployment.
#
# Two phases, and the order is load-bearing. /v1/rib/unicast requires `router`
# and `peer`, and /v1/routes/history requires `prefix` -- all three are
# `required: true` in api/openapi.yaml, so a fixed URL would 400. Rather than
# hardcode identifiers that rot the moment the lab changes, the second phase
# derives them from what the first phase actually captured. That also makes the
# fixture set internally consistent: the RIB page is a page of the very peer
# the peers fixture describes.
#
# Requires a reachable vantage-api and a token. Defaults target a local
# port-forward of the deployment.
set -euo pipefail

BASE="${VANTAGE_API:-http://127.0.0.1:9473}"
TOKEN="${VANTAGE_TOKEN:?set VANTAGE_TOKEN to a bearer token}"
OUT="$(dirname "$0")/../ui/src/api/fixtures"
mkdir -p "$OUT"

capture() {
  local name="$1" path="$2"
  echo "capturing $name <- $path"
  curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE$path" \
    | python3 -m json.tool > "$OUT/$name.json"
}

# Phase 1: the two endpoints that need no parameters.
capture routers "/v1/routers"
capture peers   "/v1/peers"

# Phase 2: identifiers taken from phase 1's real responses.
ROUTER=$(python3 -c "
import json;d=json.load(open('$OUT/routers.json'))['data']
assert d, 'routers is empty -- the collector has no routers to derive a peer or rib request from'
print(d[0]['ip'])")
# The peer worth paging is one carrying routes. A loc_rib peer at 0.0.0.0 is a
# real row but an empty page, which teaches a component nothing.
PEER=$(python3 -c "
import json;d=json.load(open('$OUT/peers.json'))['data']
best=sorted(d,key=lambda p:-p.get('routes',0))[0]
print(best['peer_ip'])")
echo "derived router=$ROUTER peer=$PEER"

capture rib-unicast "/v1/rib/unicast?router=$ROUTER&peer=$PEER&limit=50"

PREFIX=$(python3 -c "
import json,urllib.parse
d=json.load(open('$OUT/rib-unicast.json'))['data']
print(urllib.parse.quote(d[0]['prefix'],safe='') if d else '')")
if [ -n "$PREFIX" ]; then
  capture routes-history "/v1/routes/history?prefix=$PREFIX&limit=50"
else
  echo "SKIP routes-history: the RIB page was empty, so there is no real prefix to ask about" >&2
fi

# /v1/auth/config is public, so it is captured without the token -- which also
# proves it really is reachable unauthenticated, the property the SPA's boot
# depends on. A deployment older than that endpoint returns 404; say so rather
# than writing a fixture nobody captured.
echo "capturing auth-config <- /v1/auth/config"
if curl -fsS "$BASE/v1/auth/config" | python3 -m json.tool > "$OUT/auth-config.json" 2>/dev/null; then
  :
else
  rm -f "$OUT/auth-config.json"
  echo "SKIP auth-config: $BASE does not serve /v1/auth/config -- the deployment predates it. See ui/src/api/fixtures/README.md" >&2
fi

echo "captured $(ls -1 "$OUT"/*.json 2>/dev/null | wc -l) fixtures into $OUT"
