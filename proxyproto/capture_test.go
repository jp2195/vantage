package proxyproto

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The rest of this package's tests build their headers with v2Header and
// friends -- the same code path, in reverse, as the parser under test. That
// makes the suite self-consistent rather than correct: every fixture agrees
// with the parser because the same author wrote both, and a real proxy that
// lays its bytes out differently would be caught by nothing.
//
// The files under testdata/ are the answer to that. Each one is a byte-for-byte
// recording of what a real HAProxy, Traefik, NGINX or Envoy actually sent,
// taken off the wire with the proxy configured as an operator would configure
// it. The recording harness is not part of this repository; testdata/MANIFEST.md
// records which version of which proxy produced each capture and how the
// harness was set up, closely enough to rebuild it -- a captured fixture whose
// provenance is lost is just a hand-built fixture nobody can re-derive.

// captureClientV4 and captureClientV6 are the addresses the capture rig gives
// the client container, pinned on a dedicated Docker network (see
// testdata/MANIFEST.md). Every header a proxy sends about that client must
// name it, so a re-recording must pin the client to these same addresses.
const (
	captureClientV4 = "172.28.0.9"
	captureClientV6 = "fd00:beef:cafe::9"
)

// capturePayload is what the client writes after connecting, so that every
// recording has a known tail behind the header. It is what lets these cases
// assert exact consumption: whatever Read leaves in the reader must be this,
// byte for byte, or Read has either eaten into the payload or stopped short
// and left header bytes behind for the BMP decoder to choke on.
const capturePayload = "VANTAGE-CAPTURE-PAYLOAD\n"

func TestReadAcceptsRealSenderCaptures(t *testing.T) {
	cases := []struct {
		file string
		kind Kind
		// source is the address the header must yield, or "" when the kind
		// carries no source. For the v4-mapped case it is the IPv4 address,
		// NOT the ::ffff: form the proxy put on the wire -- unmapping is the
		// behavior that stops one router from acquiring two identities.
		source string
		// rest is what must remain in the reader once the header is consumed.
		// It is capturePayload for every recording driven by a client, and
		// empty for the LOCAL case: that header comes from HAProxy's own
		// health check, which has no client behind it and therefore sends
		// nothing after the header. An empty tail there is the honest shape,
		// not a truncated recording.
		rest string
	}{
		{file: "haproxy-v1-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "haproxy-v2-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "haproxy-v2-tlv.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "haproxy-v2-local.bin", kind: KindLocal, source: "", rest: ""},
		{file: "haproxy-v2-tcp6.bin", kind: KindProxy, source: captureClientV6, rest: capturePayload},
		{file: "haproxy-v2-v4mapped.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "traefik-v1-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "traefik-v2-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "nginx-v1-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
		{file: "envoy-v2-tcp4.bin", kind: KindProxy, source: captureClientV4, rest: capturePayload},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := readCapture(t, tc.file)
			r := bytes.NewReader(raw)

			h, err := Read(r)
			if err != nil {
				t.Fatalf("Read = %v -- this is a recording of what a real proxy "+
					"sent, so a parse error here is a defect in the parser, not "+
					"in the fixture", err)
			}
			if h.Kind != tc.kind {
				t.Errorf("Kind = %v, want %v", h.Kind, tc.kind)
			}
			switch tc.source {
			case "":
				if h.Source.IsValid() {
					t.Errorf("Source = %v, want none for kind %v", h.Source, tc.kind)
				}
			default:
				if h.Source.String() != tc.source {
					t.Errorf("Source = %v, want %v", h.Source, tc.source)
				}
			}

			// Exact consumption, against real bytes rather than built ones.
			rest, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reading the remainder: %v", err)
			}
			if string(rest) != tc.rest {
				t.Errorf("Read left %q, want %q -- Read consumed the wrong number "+
					"of bytes, so the connection would hand the BMP decoder a "+
					"stream that starts in the wrong place", rest, tc.rest)
			}
		})
	}
}

// readCapture returns one capture, failing loudly if it is missing or empty.
//
// The guard is the point of the helper. A table that silently skips absent
// files would report green on a checkout where testdata/ never landed, which
// is exactly the shape of the problem these captures exist to fix: a suite
// that passes while verifying nothing.
func readCapture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading capture: %v -- testdata/MANIFEST.md describes how it was recorded", err)
	}
	if len(b) == 0 {
		t.Fatalf("%s is empty: a zero-byte capture proves nothing and must not "+
			"be committed", path)
	}
	// 15 bytes is the shortest header that can exist at all: "PROXY UNKNOWN\r\n".
	// A recording below that caught a connection carrying no header, which
	// means the proxy was misconfigured when it was taken.
	if len(b) < 15 {
		t.Fatalf("%s is %d bytes, too short to hold any PROXY header: the "+
			"recording caught a connection that sent none", path, len(b))
	}
	return b
}
