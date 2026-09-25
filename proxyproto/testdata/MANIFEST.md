# What these captures are, and how each one was made

Every file here is a byte-for-byte recording of what a real proxy put on the
wire, with the proxy configured the way an operator would configure it. They exist because the rest of this package's
fixtures are built by `v2Header` and friends -- the same understanding that
`Read` implements, expressed twice -- so they can only ever confirm that the
package agrees with itself.

The harness that recorded them is not part of this repository; the section
below describes it closely enough to rebuild. The client was pinned at
`172.28.0.9` / `fd00:beef:cafe::9`, and `proxyproto/capture_test.go` asserts
those addresses, so a re-recording must reuse them: the subnet is part of the
contract rather than an incidental detail.

## How they were recorded

- **One Docker network**, dual stack, `172.28.0.0/24` and `fd00:beef:cafe::/64`,
  with every container at a fixed address: the recorder at `.2`, HAProxy `.3`,
  NGINX `.4`, Envoy `.5`, Traefik `.6`, and the client at `.9` (the same host
  number in both families).
- **The recorder** is a small Python script listening dual stack on port 9999.
  It accepts ONE connection, writes every byte it receives to a file, and
  exits. Each capture is a fresh recorder, so a file is always evidence about
  a single sender in a single configuration, never two connections appended
  together. Every proxy forwards to it by IP (`172.28.0.2:9999`), not by name,
  because a fresh container per capture may not exist yet when the proxy
  resolves names at startup.
- **The client** connects to one proxy frontend, writes
  `VANTAGE-CAPTURE-PAYLOAD\n`, and half-closes, which gives the recorder a
  clean EOF instead of an idle timeout. The IPv6 capture connects to HAProxy
  over IPv6; the v4-mapped capture connects over IPv4 to a `bind :::PORT v4v6`
  listener.
- **Each proxy** is a plain TCP pass-through to the recorder, differing only in
  how it announces the client (the "Sender configuration" column below):
  HAProxy in `mode tcp` with one backend per variant; NGINX's `stream` module
  with `proxy_protocol on`; Envoy's `tcp_proxy` filter with an
  `upstream_proxy_protocol` transport socket (`version: V2`) wrapping
  `raw_buffer`; Traefik's TCP routers (`` HostSNI(`*`) ``) with a load-balancer
  service setting `proxyProtocol.version`.
- **The LOCAL capture drives no client.** It comes from HAProxy's own health
  check (`server ... send-proxy-v2 check`) against the recorder.

Every capture except the LOCAL one ends in the 24 bytes
`VANTAGE-CAPTURE-PAYLOAD\n`, written by the client after connecting. That tail
is what lets the test assert *exact* consumption: whatever `Read` leaves must
be precisely those bytes, so a parser that ate one byte too many or stopped one
short fails rather than passing quietly.

## Versions recorded on 2026-09-06

| Proxy | Version | Image |
| --- | --- | --- |
| HAProxy | 3.0.27-a2b09cd | `haproxy:3.0-alpine` |
| Traefik | 3.1.7 (comte) | `traefik:v3.1` |
| NGINX | 1.27.5 | `nginx:1.27-alpine` |
| Envoy | 1.31.10 | `envoyproxy/envoy:v1.31-latest` |

## The captures

| File | Sender configuration | Bytes | What it pins |
| --- | --- | --- | --- |
| `haproxy-v1-tcp4.bin` | `server ... send-proxy` | 69 | The v1 text line as HAProxy writes it |
| `haproxy-v2-tcp4.bin` | `send-proxy-v2` | 52 | v2 AF_INET: `21 11 000c` + 12-byte address block |
| `haproxy-v2-tlv.bin` | `send-proxy-v2-ssl` | 60 | v2 with an 8-byte SSL TLV after the address block -- the case that fails if the parser assumes the address block is the whole body instead of honoring the declared length |
| `haproxy-v2-local.bin` | health check, `send-proxy-v2 check` | 16 | `20 00 0000`: a LOCAL header with no client behind it, and no payload. This is how a health check reaches the collector, and it must not be read as a router |
| `haproxy-v2-tcp6.bin` | `send-proxy-v2`, v6 client | 76 | v2 AF_INET6: `21 21 0024` + 36-byte address block |
| `haproxy-v2-v4mapped.bin` | `send-proxy-v2`, v4 client on a `v4v6` listener | 76 | **The identity-collapse guard.** HAProxy does NOT normalize to TCP4 here: it emits a genuine AF_INET6 header carrying `::ffff:172.28.0.9`. `Header.Source` must come back as `172.28.0.9`, or one router acquires two identities depending on which listener its proxy used |
| `traefik-v1-tcp4.bin` | TCP service, `proxyProtocol.version: 1` | 69 | Traefik's v1 |
| `traefik-v2-tcp4.bin` | TCP service, `proxyProtocol.version: 2` | 52 | Traefik's v2 |
| `nginx-v1-tcp4.bin` | stream, `proxy_protocol on` | 69 | NGINX's v1. There is no version switch in the stream module, so an operator behind NGINX gets v1 whether or not they chose it |
| `envoy-v2-tcp4.bin` | upstream proxy protocol transport socket, `V2` | 52 | Envoy's v2 |

## What is NOT captured here, and why

Two rows of the matrix could not be produced from a real sender, and the
constructed fixtures in `proxyproto_test.go` remain the only coverage for them.
Recording that plainly is the point of this file: a gap named is a gap someone
can close, and a gap counted as covered is a gap nobody looks at again.

- **AF_UNSPEC / `PROXY UNKNOWN`.** A proxy emits it when it genuinely does not
  know the source. None of these four could be made to do so on demand.
- **AWS NLB.** A v2 sender, and not runnable locally. Its TLV layout in particular is
  unverified against this parser.

## What the captures established

All ten parse correctly, with the right `Kind` and the right `Source`, and
`Read` consumes exactly the header in every case. No parser defect was found.
The v1 line grammar is identical across HAProxy, Traefik and NGINX, and the two
v2 senders that announce an IPv4 client produce byte-identical 16-byte headers.
