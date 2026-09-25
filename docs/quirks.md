# Vendor quirk matrix

Canonical registry documentation. **This table and
`quirk.Registry` (the actual Go table) must agree** — the registry's
own doc comment says a new quirk is "one `Entry` (+ hook use at its layer +
corpus sample + docs/quirks.md row)"; a quirk PR that adds an `Entry` without
updating this file (or vice versa) is incomplete.

Detection: **S** = static (vendor/version match via `Entry.AppliesTo`), **D**
= dynamic (runtime evidence via `Set.Latch`) — dynamic always outranks a
static version claim: a router whose version string says "this
is fixed" but behaves broken gets the quirk latched anyway. All four quirks
below are dynamic-only today (`Entry.AppliesTo == nil` in the registry); none
has a known vendor/version signature yet, so every session evaluates for
them from runtime evidence alone. Static entries can be added as vendor
captures, or reports in the openbmp, gobmp and pmacct issue trackers and on
the IETF GROW list, establish a version signature.

| ID | Layer | Symptom | Detection | Handling | ParseFlag | Status |
|---|---|---|---|---|---|---|
| `QK_TS_ZERO` | per-peer-header | Router sends an all-zero timestamp in the BMP per-peer header (RFC 7854 §4.2), seen historically on IOS-XR | D | Collector substitutes its own receive time for the event's router-side timestamp | `PARSE_FLAG_TS_COLLECTOR_FALLBACK` | handled; corpus pending |
| `QK_ADDPATH_HEURISTIC` | peer-up | NLRI parse fails under the capabilities negotiated in Peer-Up's OPENs — a router with missing or self-contradictory add-path capabilities | D | Add-path wire format decided **structurally** (by inspecting the NLRI bytes) rather than trusted from negotiated capabilities alone | `PARSE_FLAG_ADDPATH_HEURISTIC` | handled; corpus pending |
| `QK_VERSION_UNPARSED` | init-tlv | Initiation's sysDescr TLV is present but doesn't match any of this repo's per-vendor version-parsing schemes (IOS-XR `7.9.2`-style, NX-OS `10.2(3)F`-style, FRR `FRRouting 10.3`-style; no other vendor has a scheme) | D | Matching degrades to **vendor-only** for the rest of the session — the safe direction, since it widens which quirks may apply rather than narrowing | `PARSE_FLAG_VERSION_UNPARSED` | handled; no corpus needed (this quirk fires on the *absence* of a recognized version scheme, not a specific vendor sample) |
| `QK_CAPS_MISSING` | peer-up | Route Monitoring arrives for a peer with no negotiated capabilities on record right now: the collector never saw a Peer-Up for it, its most recent state was a Peer-Down (a flap), or a Peer-Up did arrive but its embedded OPENs were unparseable | D | Session proceeds with a **zero-value capability set** (no add-path, no 4-byte-ASN, no MP) rather than refusing to parse | `PARSE_FLAG_CAPS_MISSING` | handled; corpus pending |

## What "handled" means here

Every quirk above already has working code at its layer (`quirk`,
`bmp`, `bgp`, `collector`) — "corpus pending" refers
only to the **corpus reproduction** (a captured byte sample proving the
quirk against a real router's actual bytes), not a gap in the handling.
A quirk without a test is a rumor, and the pending corpus entries are the
one place that rule is knowingly not yet satisfied, tracked here on purpose
rather than silently.

## What a consumer should do about each flag

Every envelope carries the `ParseFlag`s active on it (`vantage.v1.Envelope`).
None of them mean "this event is untrustworthy" — they mean "this event was
produced under a documented accommodation, not a straight read of the wire."
A downstream consumer (writer, dashboard, analyst) that cares about data
provenance should:

- Treat `PARSE_FLAG_TS_COLLECTOR_FALLBACK` events' router-side timestamp as
  collector-observed, not router-observed, when computing propagation delay
  or similar router-clock-dependent metrics.
- Treat `PARSE_FLAG_ADDPATH_HEURISTIC` and `PARSE_FLAG_CAPS_MISSING` as a
  signal that add-path/path-ID interpretation for that peer is a best-effort
  reconstruction, not a negotiated fact — most relevant for consumers that
  key on path ID.
- Treat `PARSE_FLAG_VERSION_UNPARSED` as "this router's quirk profile is
  wider than necessary" — a signal for humans (is a new sysDescr format from
  a NOS upgrade worth a registry entry?) more than for automated consumers.
- Six more flags are not quirks and have no registry entry. They record
  what the parser did with bytes it could not take at face value:
  - `PARSE_FLAG_TREAT_AS_WITHDRAW_7606`: an UPDATE carried a malformed
    attribute that RFC 7606 says makes the route a withdrawal (ORIGIN,
    AS_PATH, NEXT_HOP, MED, the community attributes, MP_REACH or
    MP_UNREACH), so its prefixes were archived as withdrawn. A route that
    disappears with this flag was withdrawn by vantage's reading of a
    malformed UPDATE, not by the router's intent; check the router.
  - `PARSE_FLAG_ATTR_DISCARDED_7606`: a malformed attribute that does not
    affect route selection was discarded under RFC 7606 and the route kept.
    The discarded bytes stay on the event as an unknown attribute.
  - `PARSE_FLAG_UNKNOWN_FAMILY`: an AFI/SAFI this build has no decoder for.
    Its NLRI is not decoded into routes, and the MP_REACH/MP_UNREACH bytes
    stay on the event. Expect no route rows for that family.
  - `PARSE_FLAG_NLRI_UNTYPED`: a family vantage decodes, but some NLRI in
    the event could not be typed (an EVPN route type this build does not
    type, or NLRI that failed to decode). Those NLRI are missing from the
    route tables; the raw bytes stay on the event.
  - `PARSE_FLAG_LS_NLRI_UNDECODED`: a BGP-LS NLRI type this build does not
    decode (anything but node, link, and IPv4 and IPv6 prefix NLRI, types 1
    to 4). Its bytes are kept on the event, and it is absent from
    `ls_nodes`, `ls_links` and `ls_prefixes`.
  - `PARSE_FLAG_LS_TLV_UNKNOWN`: a BGP-LS NLRI or attribute decoded, but
    one of its TLVs was a type this build does not recognize. The TLV's
    bytes are kept in the row's `unknown_tlvs` column.

  None of these needs action from a consumer that only reads decoded
  routes, other than knowing the decoded view is incomplete for that event.
  A steady count of any of them from one platform is worth a bug report.
- Every flag increments `vantage_collector_parse_flags_total{flag=...}`
  (`collector/metrics.go`), and every archived row carries its flags in a
  `parse_flags` column. The Parse anomalies Grafana dashboard
  (`docs/grafana.md`) breaks those down by platform and by flag, which
  is how a fleet-wide "did a new quirk just show up" question gets
  answered.

## Ops escape hatch

Per-router `force_quirks`/`disable_quirks` in `vantage-collector`'s config
(`routers:` keyed by router IP; see the `vantage-collector` section of
`docs/architecture.md`, or "Per-router overrides" in
`deploy/helm/README.md`) let an operator force a quirk
active or suppress it immediately, without a code change, for gear
misbehaving right now. Unknown quirk IDs in either list are
rejected at config load (`collector/config.go`'s `validate`) rather
than silently ignored.

**Override coverage.** Overrides take effect for `QK_TS_ZERO`,
`QK_CAPS_MISSING` and `QK_VERSION_UNPARSED`: disabling one suppresses both
its accommodation and its flag, so the wire value passes through untouched.
`QK_ADDPATH_HEURISTIC` is the exception — the decision is made inside
`bgp`'s UPDATE parser, which deliberately has no dependency on the
quirk package, so listing it in either list is currently accepted and
inert. Threading an override down into the parser is not yet done. Nothing
else in the registry has this gap.

Precedence (`quirk.Resolve`), highest to
lowest: **disable always wins** (cannot be overridden by force or a later
dynamic latch) > **force / dynamic latch** (both set a quirk active
regardless of any version claim) > **static profile match** (weakest signal,
never overrides a later disable).

## Router behavior outside the registry

The table above lists only what the collector detects and accommodates in
code. Some vendor behavior decides what reaches the archive without anything
on the wire to detect or correct: the router simply never sends the data.
These have no quirk ID, no `ParseFlag` and no `quirk.Registry` entry, and
they are recorded here so an operator configures around them.

### IOS-XR: no table dump without `initial-refresh`

| | |
|---|---|
| **Platform** | IOS-XR, tested on XRd 26.1.1, 2026-08-17 |
| **Symptom** | With no `initial-refresh` line under the BMP server, XR sent only incremental changes across four observed BMP session cycles and never its table: the same three link-state NLRI every time, never the 48 routes the routers demonstrably held. Sessions up, peers up, no errors, and an archive holding a fraction of the network. |
| **Configuration** | `initial-refresh delay <N>` under `bmp server <id>`; the one router that sent node NLRI from the start carried `initial-refresh delay 5`. Adding the line to the two routers without it was immediately followed by node and prefix NLRI arriving for the first time (`ls_nodes` 1 to 9). |
| **Detection** | None in vantage: the collector cannot tell a router that is not dumping from one with nothing to send. Compare a new router's route count against the router's own `show bgp` after its first session. |

What this does not establish: adding the line also bounced the BMP
session, so the result is strong correlation across several cycles rather
than an isolated experiment, and XR's own help text claims a default of 1.
A later session (2026-08-28) found that after a BMP-only reconnect, XR sent
Initiation and a Peer Up per monitored peer and then nothing, with
`initial-refresh` configured, on every node tested; routes arrived only when
BGP updates did. What reliably refilled the stream there was bouncing the
BGP neighbor; a soft refresh (`clear bgp ... soft in`) produced no mirrored
messages. So configure `initial-refresh` on every XR router, and do not rely
on a BMP reconnect alone to re-dump its tables.

NX-OS (n9kv 10.6(2)) does not need this: it reports `initial-refresh delay
30` and `initial-delay 45` without either being configured, and
`initial-refresh skip` turns it off.

## Adding a quirk

1. Add an `Entry` to `quirk.Registry` (ID, layer, `ParseFlag`,
   `AppliesTo` if it has a known static signature, description).
2. Wire the handling hook at that layer.
3. Add a corpus sample (a captured reproduction). The four existing quirks
   still owe theirs, but a *new* quirk ships its corpus sample and its
   matrix row with the PR, or it doesn't merge.
4. Add a row to this table.

Registry and table must describe the same four (or, after your PR, five)
entries; a mismatch between them is a bug in whichever one is stale.
