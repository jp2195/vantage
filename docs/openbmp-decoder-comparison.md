# What OpenBMP's decoder tells us

`vantage` is a ground-up replacement for OpenBMP, and OpenBMP ran against this
class of gear (Cisco NX-OS and IOS-XR) for years. Its parsers therefore encode
real-world behavior that no vendor document states. Read on 2026-08-10 from
`OpenBMP/openbmp@master`, `Server/src/bgp/{EVPN,MPReachAttr,UpdateMsg}.cpp`.

## It independently confirms the raw 24-bit EVPN label decision

vantage stores the EVPN NLRI label as a **raw 24-bit value** because the field
is a VNI under VXLAN and a shifted 20-bit MPLS label otherwise. OpenBMP makes the
same distinction, in two different places:

**EVPN (`EVPN.cpp`)** -- raw 24-bit:

```c
memcpy(&tuple.mpls_label_1, data_pointer, 3);
bgp::SWAP_BYTES(&tuple.mpls_label_1);
tuple.mpls_label_1 >>= 8;          // undoes the swap alignment only
```

**vpn4 / lu4 (`MPReachAttr::decodeLabel`)** -- 20-bit MPLS label, via a bitfield:

```c
struct { uint8_t ttl:8; uint8_t bos:1; uint8_t exp:3; uint32_t value:20; } decode;
... convert << label.decode.value;
```

So the asymmetry is not a vantage invention: the reference implementation
encodes it too. This matches what vantage measured against NX-OS routers
in a lab: `bgp/testdata/corpus/nxos/n9kv-10.6.2F-leaf-evpn.bmpcap` carries
type-2 routes that decode as `labels: [10010]`, the L2VNI configured on that
router, unshifted.

## Three places vantage is ahead

**Add-path on the VPN SAFI.** OpenBMP skips add-path parsing for VPN entirely:

```c
// Only check for add-paths if not mpls/vpn
if (not isVPN and add_path_enabled and (len - read_size) >= 4) {
```

`VpnPrefix.path_id` means vantage parses it. If a PE runs add-path on vpnv4,
OpenBMP reads the 4-byte path-id as prefix and label bytes and corrupts every
route in the message. Worth confirming whether an operator's production
PEs enable add-path for vpnv4 -- if they do, this is a concrete
correctness gap in the tool being replaced, and a reason to trust its
historical output less.

**VPN next-hop validation.** OpenBMP advances past the 8-byte zero RD blindly:

```c
if (isIPv4) { nlri.next_hop += 8; nlri.nh_len -= 8; }
```

`nextHopForFamily` in `bgp/nlri.go` requires those 8 bytes to actually
be zero, accepts lengths 12, 24 and 48, and returns not-ok for anything else so
the caller flags rather than guesses.

**Typing.** OpenBMP accumulates labels into a `std::string` via `ostringstream`;
vantage carries `repeated uint32`. And OpenBMP's bitfield union over a
byte-swapped `uint32` is endian- and compiler-layout dependent, where vantage
uses explicit shifts.

## What it does not tell us

**There are no NX-OS-specific branches in these parsers at all** -- no vendor
name appears in any decode path. So OpenBMP offers no evidence about how a Nexus
renders vpn4 specifically. Two readings, and they have opposite consequences:
either NX-OS vpn4 is standard enough to need no special handling, or OpenBMP
mis-parsed it and nobody noticed because nothing checked. Nothing in the source
distinguishes them.

That leaves the NX-OS vpn4 gap where it was: only a capture from real hardware
closes it. Nexus 9300v 10.6(2) in a virtual lab cannot produce vpn4: a vpnv4
session establishes and advertises zero routes, because the BGP address family
exists but the L3VPN label-allocation path behind it does not.
