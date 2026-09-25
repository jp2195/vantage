# Parity with OpenBMP, and where vantage goes beyond it

vantage replaces OpenBMP. The bar is **at minimum feature parity, then well
past it**. This is the gap list: what OpenBMP does that vantage does not yet,
what vantage already does better, and which spec closes each gap.

Sources read 2026-08-10: `OpenBMP/obmp-collector` README and
`Server/src/bgp/*.cpp`, `OpenBMP/obmp-grafana`, `SNAS/obmp-grafana`.

## Gaps — things OpenBMP has that vantage does not

| Gap | OpenBMP | vantage today | Consequence | Closes in |
|---|---|---|---|---|
| **VPNv6 route targets (RFC 5701)** | supported | `FamilyVPNv6` is typed (`familyHasDecoder`, `bgp/nlri.go:149-156`) — prefixes, labels and RDs decode through the same labeled-NLRI path as vpn4 (`parseVpn6NLRI`). But a route target carried in RFC 5701's IPv6-address-specific extended community (attribute 25) does not: `parseAttr` (`bgp/update.go`) has a case for `attrExtComm` (type 16, RFC 4360's 8-byte communities), and none for type 25 | 6VPE prefixes are typed and queryable; a route target keyed to an IPv6 address (rather than an ASN or IPv4 address) stays raw on whatever route carries it | RFC 5701 decoder, deferred alongside the vpn4/lu4/EVPN extended-communities work |
| **RPKI / IRR integration** | RPKI and IRR enrichment for hijack and leak analysis | none | No route-security analysis. The single biggest feature gap by scope — needs external data feeds, not just a decoder | its own spec; largest of these |
| **BMP forwarder** | re-emits native BMP to another collector | none | Cannot chain collectors | YAGNI unless asked for |

IPv6 unicast and BGP-LS are not in this table: both are typed, and a table
titled "things OpenBMP has that vantage does not" is the wrong place to keep
something vantage has. See Ordering below for how each closed.

## Where vantage is already ahead

| | vantage | OpenBMP |
|---|---|---|
| **RFC 8671 adj-RIB-out** | O flag decoded, and the direction is part of *peer identity*: pre/post-policy adj-RIB-in and adj-RIB-out are four subjects, four sequence counters and four `rib` values in ClickHouse | supported, but as a flag on a shared peer |
| **EVPN** | types 2, 3 and 5 typed, with VNIs verified against real NX-OS | present in source, no VNI/MPLS distinction documented |
| **add-path on the VPN SAFI** | parsed (`VpnPrefix.path_id`) | **explicitly skipped** — `if (not isVPN and add_path_enabled ...)`. A PE running add-path on vpnv4 corrupts every route in the message for OpenBMP |
| **VPN next-hop validation** | requires the 8-byte RD to be zero, accepts 12/24/48, flags anything else | advances 8 bytes unconditionally, no validation |
| **Typing** | protobuf throughout; labels as `repeated uint32` | labels accumulated into a `std::string`; label decode via an endian- and compiler-dependent bitfield union |
| **Large communities** | typed message | not in the feature list |
| **Extended communities** | typed: numeric type and subtype, rendered value, and the wire bytes retained; route targets render identically to route distinguishers | decodes "roughly all of them", rendering not documented |
| **Test corpus** | real IOS-XR and NX-OS captures replayed in CI | none evident |
| **Quirk framework** | vendor/OS/version detection with per-router overrides | no vendor branches anywhere in the decode paths |

## Add-path on vpn4, against real traffic

**vpn4 with add-path is the code path that most distinguishes vantage from
OpenBMP.** `parseVpnNLRI` passes `addPath` into `parseLabeledCommon`, whose
contract states each entry begins with an optional 4-byte Path Identifier.
OpenBMP by contrast skips add-path for the VPN SAFI outright, so a PE running
add-path on vpnv4 corrupts every route in the message for it.

The corpus now exercises it. `iosxr/xrd-26.1.1-pe1-2byte-aspath.bmpcap`
carries an IOS-XR peer that negotiated add-path for vpnv4 (AFI 1, SAFI 128),
and its routes decode with a Path Identifier, RD and label; plain IPv4-unicast
add-path is captured from IOS-XE and NX-OS as well
(`*-addpath-stats.bmpcap`, `*-addpath-4byte.bmpcap`). Run
`go run ./cmd/vantage reparse -json` on any of them to see the `pathId`s.

What is still untested against real traffic:

- `Caps.AddPathRecv[family]` is learned from the OPEN messages carried in Peer
  Up. A BMP session that attaches mid-stream, or one whose Peer Up was missed,
  leaves the capability unknown. `update.go` carries an add-path heuristic for
  that case, but only for classic IPv4-unicast NLRI (`parsePrefixesV4Auto`),
  not for vpn4. Captured traffic raises that heuristic on IPv4 unicast
  (`PARSE_FLAG_ADDPATH_HEURISTIC` in several FRR, IOS-XR and NX-OS captures),
  but a vpn4 add-path session with its capabilities unknown has never been
  captured, and vantage has no heuristic for it.

An open item for a contributor with a vpnv4 route reflector that runs
add-path: a capture of a BMP session that attaches after the Peer Up, so the
capabilities are unknown, would show what vantage does with those routes.

The general point generalizes past this one case: a feature verified by reading
code is not the same as a feature verified by a router. The corpus exists to
close that gap.

## Ordering

The gaps are not equally urgent, and one of them is a correctness bug rather
than a missing feature.

1. ~~**`flagO` / RFC 8671.**~~ **Done.** The O flag is decoded, and RIB
   direction was folded into `subjects.PeerToken` rather than left as a bare
   envelope field: a router mirroring both directions of one neighbor would
   otherwise have published routes it *sent* onto the subject carrying routes
   it *received*, and collapsed them together in ClickHouse under
   `ReplacingMergeTree`. Pre-policy adj-RIB-in keeps the bare token, so every
   existing subject is unchanged.
2. ~~**Extended communities.**~~ **Done.** Route targets decode, and the L3VPN
   dashboards are unblocked for vpn4. RFC 5701's IPv6-address-specific
   communities (attribute 25) remain raw, so a 6VPE route's route target does
   not decode — tracked as its own row in the Gaps table above, now that the
   vpn6 prefix it attaches to decodes too.
3. ~~**IPv6 unicast.**~~ **Done.** `FamilyIPv6U` decodes through
   `ParsePrefixesV6`, the same shape ipv4 unicast uses. No known gap remains.
4. ~~**VPNv6.**~~ **Done, with one residual gap.** `FamilyVPNv6` decodes
   prefixes, labels and RDs. Route targets carried in RFC 5701's
   IPv6-address-specific extended community (attribute 25) do not — see the
   Gaps table above.
5. ~~**BGP-LS.**~~ **Done.** Node, Link and Prefix NLRI — both the IPv4 and
   IPv6 topology-prefix types (RFC 9552 types 3 and 4) — decode into
   `ls_nodes`, `ls_links` and `ls_prefixes`, including SRGB and adjacency/node
   SIDs off the BGP-LS attribute. This is what the link-state topology graph
   on the README's front page is built from.
6. **RPKI/IRR.** Largest by far, needs external feeds, and is the one that would
   take vantage past parity into something OpenBMP users would switch for.

Nothing here changes the sink's persist-whatever-the-collector-produces
design, so a family that becomes typed later lands in its table without a
sink change. The one coupling there ever was — **the L3VPN dashboards
needed extended communities decoded first** — is closed: that decoder
landed (item 2 above), and the sink's own design is written against it.
Nothing in this list now blocks anything else in it.

## Found while closing the RFC 8671 gap, not yet fixed

**RFC 9069 redefines the same flags byte for Loc-RIB peers.** For peer type 3
the high bit is not V (peer address is IPv6) but F (the Loc-RIB is filtered),
and the remaining bits are reserved. `ParsePeerHeader` applies the RFC 7854
interpretation unconditionally, so a Loc-RIB peer that sets F is read as having
an IPv6 peer address and its address is parsed from the wrong 16 bytes.

Impact today is small and bounded, but only because the two places where it
would not have been bounded take an explicit peer-type branch. `PeerToken`
returns `locrib` for that peer type regardless of address, *and* skips the
RFC 8671 direction suffix for it; the collector likewise never treats a
Loc-RIB peer as adj-RIB-out when choosing which direction's negotiated
capabilities to parse its UPDATEs with. Without those two branches the
reserved 0x40 and 0x10 bits would be read as RFC 7854's L flag and RFC 8671's
O flag, and junk left in them would split one Loc-RIB peer across up to four
subjects, four peer states and four sequence counters. With them, no subject
or msg-id is affected, and the corpus's Loc-RIB captures (FRR) do not set F.

What is still wrong is on the envelope: `PeerId.ip` — `::` instead of
`0.0.0.0` — and the `IPv6()` accessor, plus `PeerId.post_policy` and
`PeerId.adj_rib_out`, which report reserved bits as if they were flags. The
fix is one peer-type branch in `ParsePeerHeader` (making `IPv6()`,
`PostPolicy()` and `AdjRIBOut()` peer-type aware at the source, so the two
branches above become redundant rather than load-bearing) plus a
`LocRIBFiltered()` accessor, and it wants a fixture from a router that sends
a filtered Loc-RIB with F set.

See also `references.md` for FRRouting's BMP implementation, a third sender and
a far cheaper source of fixtures than a hardware lab.
