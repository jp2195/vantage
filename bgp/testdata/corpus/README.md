# Vendor capture corpus

Real BMP bytes from real routers, one file per captured scenario. These are the
regression suite for everything in `bgp/` and `collector/`: if a decoder change
is safe, it is safe against these.

One edit was made to the bytes: a lab peer announced its host's name in the
BGP hostname capability, and that name is replaced by `lab-frr-host`, a
placeholder of the same length, so every length field and decoded structure
is unchanged.

## Why these files are not replaceable

The CML lab that produced them is decommissioned. A synthetic generator
(`vantage bmpgen`) can produce volume and churn, but it can only emit bytes we
wrote — which tests the decoder against its own beliefs rather than against a
shipping implementation.

That failure is on record: the five vendor anchors in `quirk.anchors` were
originally written from documentation and matched no real sender. Two tests now
enforce "no sample, no anchor" for exactly this reason. `bmpgen` has its own
version of the problem — it currently emits 4-byte router-IDs under IS-IS
protocol-IDs, a combination no real router produces.

Treat every file here as evidence, not as test scaffolding. Do not regenerate,
"clean up", or reformat one.

## Layout

    {vendor}/{platform}-{version}-{role}-{feature}.bmpcap
    {vendor}/{platform}-{version}-{role}-{feature}.bmpcap.json   (sidecar, when present)

`vendor` is one of `frr`, `iosxe`, `iosxr`, `nxos`, and the directory is what
makes a per-vendor coverage axis meaningful — a feature proven only on one
vendor is not proven on the others, and the axis is per-vendor by directory.

A `.bmpcap` is BMP messages concatenated with no delimiter. None is needed:
every message declares its own length in its header, and the collector rebuilds
that header from the parsed type and payload length, so a stored message's
declared length can never disagree with its bytes.

## The sidecar

Written by `vantage capture` beside the capture. It carries provenance — which
router and mode produced the file, the `sys_descr` banner, resolved
vendor/os/version, message count — plus `expected` and `complete`, which report
whether the replay that produced it consumed everything it was promised.

**`complete: false` means the file is a partial capture.** It is still useful;
it is not a whole scenario.

Not every capture has one. Some predate the sidecar. A missing sidecar is a gap
in provenance, not a defect in the bytes — nothing in the decode path reads it.

## Working with these files

Replay one through the current decoders and print what comes out:

    go run ./cmd/vantage reparse bgp/testdata/corpus/nxos/<file>.bmpcap
    go run ./cmd/vantage reparse -json <file>.bmpcap     # one JSON envelope per line

`reparse` is read-only and exits non-zero on a capture it cannot fully read,
returning whatever it recovered first. Replaying the whole corpus is the
cheapest check that a decoder change did not silently drop something.

Add a new one (needs a live collector and NATS):

    go run ./cmd/vantage capture -router <IP> -o <vendor>/<name>.bmpcap -window 5m
    go run ./cmd/vantage capture -router <IP> -o <vendor>/<name>.bmpcap -since 1h

Commit the `.bmpcap` **and** its `.json` together.

## Coverage

`TestCorpusFeatureCoverage` answers one question: which product features does
this corpus actually carry an example of? It reports per vendor as
`N covered, N n/a, N OPEN` across feature axes in nine families — `bmp`,
`rib`, `family`, `attr`, `route`, `asn`, `ls`, `peer` and `vendor`.

    go test ./collector/ -run TestCorpusFeatureCoverage -v

Read the numbers from that command, not from a document. Counts written down
here would be a snapshot of the day someone typed them, and this project has
repeatedly been misled by exactly that — a measured claim restated later as a
current one. The test is the authority; anything else is a memory of it.

Some axes are marked `n/a` per vendor because the platform cannot produce them,
which is a measured limit rather than a gap to fill. The distinction matters:
`OPEN` is work, `n/a` is closed.
