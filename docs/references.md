# External references worth reading

Implementations and documents to compare vantage against. Nothing here is a
commitment to act; it is the reading list.

## FRRouting's BMP implementation — an RFC conformance checklist

<https://docs.frrouting.org/en/latest/bmp.html>

Not read yet (noted 2026-08-10). **The reason to read it is that it enumerates
which BMP RFCs FRR implements**, which gives us something this repo does not
currently have: an external, independently-maintained list to audit our own RFC
coverage and adherence against.

We have no such audit today. What we have instead is `openbmp-parity.md`, which
compares us feature-by-feature against one other collector — useful, but it
measures us against an implementation's choices rather than against the
specifications. Those are different questions, and the second one is the one
that says whether we are correct.

Two live cases make the gap concrete, and both were found by accident rather
than by any systematic check:

- **RFC 8671 (adj-RIB-out)** went undecoded until 2026-08-10, and the bug was
  found by reading OpenBMP's feature list, not by checking the RFC.
- **RFC 9069 (Loc-RIB)** redefines the per-peer flags byte, and
  `ParsePeerHeader` still reads it as RFC 7854 does. Open; see
  `openbmp-parity.md`.

Both are the same failure mode: an RFC we partially implement, with nothing
telling us where the edges are. A conformance list is what turns that from
discovery-by-luck into a checklist.

The reading should produce, per RFC that FRR names: does vantage implement it,
partially or not at all; and if partially, what is missing. That belongs in its
own document rather than in the OpenBMP parity comparison, because it answers a
different question.

Secondary, once that is done: FRR is also a BMP *sender* that runs as a
container in seconds, where a hardware lab needs a rebuild and XRd needs a
4 GB override. That makes it a candidate for producing
fixtures for paths the corpus has no capture of yet — adj-RIB-out above
all, which we now support and have never seen real traffic for.
