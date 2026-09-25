package quirk

import (
	"testing"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// TestResolveAndLatch, TestResolveForceDisable, and TestRegistryFlags cover
// the baseline cases for Resolve and Latch.

func TestResolveAndLatch(t *testing.T) {
	s := Resolve(Profile{}, nil, nil)
	if s.Active(QkTSZero) {
		t.Fatal("nothing should be static-active for empty profile")
	}
	if newly := s.Latch(QkTSZero); !newly {
		t.Fatal("first latch must report newly=true")
	}
	if newly := s.Latch(QkTSZero); newly {
		t.Fatal("second latch must report newly=false")
	}
	if !s.Active(QkTSZero) {
		t.Fatal("latched quirk must be active")
	}
}

func TestResolveForceDisable(t *testing.T) {
	s := Resolve(Profile{}, []ID{QkAddPathHeuristic}, nil)
	if !s.Active(QkAddPathHeuristic) {
		t.Fatal("forced quirk must be active")
	}
	s2 := Resolve(Profile{}, nil, []ID{QkAddPathHeuristic})
	s2.Latch(QkAddPathHeuristic)
	if s2.Active(QkAddPathHeuristic) {
		t.Fatal("disabled quirk must stay inactive even after latch")
	}
}

func TestRegistryFlags(t *testing.T) {
	if FlagFor(QkTSZero).String() != "PARSE_FLAG_TS_COLLECTOR_FALLBACK" {
		t.Fatalf("got %v", FlagFor(QkTSZero))
	}
}

// --- additional tests beyond the cases above, covering completeness of
// the registry table and the precedence interactions among disable, force
// and static match (see bmp/tlv_test.go for the package's convention of
// testing beyond the baseline cases). ---

// TestRegistryAllQuirksMapToDistinctFlags pins every brief-specified quirk
// ID to its correct ParseFlag, and that FlagFor falls back to UNSPECIFIED
// for an ID the registry doesn't know about.
func TestRegistryAllQuirksMapToDistinctFlags(t *testing.T) {
	want := map[ID]vantagev1.ParseFlag{
		QkTSZero:           vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK,
		QkAddPathHeuristic: vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC,
		QkVersionUnparsed:  vantagev1.ParseFlag_PARSE_FLAG_VERSION_UNPARSED,
		QkCapsMissing:      vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING,
	}
	for id, flag := range want {
		if got := FlagFor(id); got != flag {
			t.Errorf("FlagFor(%s) = %v, want %v", id, got, flag)
		}
	}
	if len(Registry) != len(want) {
		t.Fatalf("Registry has %d entries, want exactly %d quirks", len(Registry), len(want))
	}
	if got := FlagFor(ID("QK_NOT_REGISTERED")); got != vantagev1.ParseFlag_PARSE_FLAG_UNSPECIFIED {
		t.Fatalf("FlagFor of an unknown ID = %v, want PARSE_FLAG_UNSPECIFIED", got)
	}

	// The name promises distinctness, which nothing above actually checks:
	// two entries sharing a Flag would pass every assertion up to this
	// point.
	seen := map[vantagev1.ParseFlag]ID{}
	for _, e := range Registry {
		if prior, ok := seen[e.Flag]; ok {
			t.Fatalf("Flag %v is shared by both %s and %s; every registry entry must map to a distinct flag", e.Flag, prior, e.ID)
		}
		seen[e.Flag] = e.ID
	}
}

// TestResolveVersionUnparsedOnlyWhenSysDescrPresent pins that
// QK_VERSION_UNPARSED fires only when the VENDOR is known and its version
// string is not readable — not for an altogether absent sysDescr, and no
// longer for a banner nobody could identify.
//
// The original form of this test asserted that any present-but-unparseable
// sysDescr raised the quirk. That matched the original intent but it made
// the flag useless in practice: every real sender observed to date fails every
// anchor, so the flag fired on every envelope from every router and
// distinguished nothing. It also overstated what was known — with no vendor
// there is no "vendor-only quirk matching" to be in effect. See Resolve.
func TestResolveVersionUnparsedOnlyWhenSysDescrPresent(t *testing.T) {
	if s := Resolve(Profile{}, nil, nil); s.Active(QkVersionUnparsed) {
		t.Fatal("empty (absent) sysDescr must not activate QK_VERSION_UNPARSED")
	}
	unrecognized := ProfileFromSysDescr("TotallyNewNOS v1")
	if s := Resolve(unrecognized, nil, nil); s.Active(QkVersionUnparsed) {
		t.Fatal("an unidentifiable sysDescr must not activate QK_VERSION_UNPARSED: " +
			"there is no vendor to degrade matching to, and firing here is what " +
			"made the flag fire on every envelope from every sender")
	}
	knownVendor := ProfileFromSysDescr("Nexus9000 C9300v Chassis, Software Version NIGHTLY-BUILD")
	if s := Resolve(knownVendor, nil, nil); !s.Active(QkVersionUnparsed) {
		t.Fatal("a known vendor with an unreadable version must activate QK_VERSION_UNPARSED")
	}
}

// withTestEntry temporarily appends e to the package-level Registry for the
// duration of the calling test, restoring the original slice on cleanup.
// None of the registry's four quirks has a version-scoped AppliesTo (all
// are runtime-only signatures), so the precedence tests below need a
// synthetic static entry to exercise the version-claim vs.
// dynamic-detection and override interactions.
//
// This mutates the package-level Registry at runtime, which is the only
// runtime mutation Registry's own doc comment permits in this package.
// That's fine for a single test's duration but means any test using this
// helper must not call t.Parallel() (nor otherwise run concurrently with
// another test that reads or reassigns Registry) -- otherwise this
// reassignment races with a concurrent Resolve/Registry read and only shows
// up intermittently under `go test -race`.
func withTestEntry(t *testing.T, e Entry) {
	t.Helper()
	orig := Registry
	Registry = append(append([]Entry(nil), orig...), e)
	t.Cleanup(func() { Registry = orig })
}

// TestPrecedenceDynamicOutranksVersionClaim: a router's version claim (from
// sysDescr) says a version-scoped quirk should NOT apply, but the quirk is
// observed directly in the byte stream during the session. Dynamic
// detection outranks version claims, so the runtime Latch must still take
// effect.
func TestPrecedenceDynamicOutranksVersionClaim(t *testing.T) {
	const testID ID = "QK_TEST_VERSION_RANGE"
	withTestEntry(t, Entry{
		ID:        testID,
		Layer:     "test",
		AppliesTo: func(p Profile) bool { return VersionBetween(p.Version, "10.0", "11.0") },
	})

	// Version claim: 12.0.0 is outside [10.0, 11.0), so this quirk must not
	// be statically active for this profile.
	p := Profile{Vendor: "cisco", OS: "nxos", Version: Version{Parts: []int{12, 0, 0}, Raw: "12.0.0", OK: true}}
	s := Resolve(p, nil, nil)
	if s.Active(testID) {
		t.Fatal("out-of-range version claim must not statically activate the quirk")
	}

	// Runtime evidence contradicts the version claim and must still win.
	if newly := s.Latch(testID); !newly {
		t.Fatal("Latch on a not-yet-active quirk must report newly=true")
	}
	if !s.Active(testID) {
		t.Fatal("dynamic detection must outrank a contradicting version claim")
	}
}

// TestPrecedenceOverrideBeatsVersionClaimAndDynamic: a per-router disable
// override sits above both a positive version-claim match and a runtime
// Latch attempt — both must be defeated by the disable.
func TestPrecedenceOverrideBeatsVersionClaimAndDynamic(t *testing.T) {
	const testID ID = "QK_TEST_VERSION_RANGE_2"
	withTestEntry(t, Entry{
		ID:        testID,
		Layer:     "test",
		AppliesTo: func(p Profile) bool { return VersionBetween(p.Version, "10.0", "11.0") },
	})

	// Version claim matches (10.5.0 is inside [10.0, 11.0)) but the router
	// is configured to disable this quirk regardless.
	p := Profile{Version: Version{Parts: []int{10, 5, 0}, Raw: "10.5.0", OK: true}}
	s := Resolve(p, nil, []ID{testID})
	if s.Active(testID) {
		t.Fatal("disable must override a positive version-claim match")
	}
	if newly := s.Latch(testID); newly {
		t.Fatal("Latch must not report newly=true for a disabled quirk")
	}
	if s.Active(testID) {
		t.Fatal("disable must override a subsequent dynamic Latch attempt too")
	}
}

// TestResolveForceOverridesVersionRangeMismatch: prior force coverage only
// ever exercised force against Profile{} with an AppliesTo: nil entry, which
// is vacuous -- there was nothing for force to override. This pins force
// actually overriding a static AppliesTo that would otherwise say "no"
// because the profile's version claim is out of the quirk's range.
func TestResolveForceOverridesVersionRangeMismatch(t *testing.T) {
	const testID ID = "QK_TEST_FORCE_VERSION_RANGE"
	withTestEntry(t, Entry{
		ID:        testID,
		Layer:     "test",
		AppliesTo: func(p Profile) bool { return VersionBetween(p.Version, "10.0", "11.0") },
	})

	// 12.0.0 is outside [10.0, 11.0), so AppliesTo alone would not activate
	// the quirk -- force must activate it anyway.
	p := Profile{Version: Version{Parts: []int{12, 0, 0}, Raw: "12.0.0", OK: true}}
	s := Resolve(p, []ID{testID}, nil)
	if !s.Active(testID) {
		t.Fatal("force must activate a quirk even when its version-range AppliesTo excludes the profile")
	}
}

// TestResolveForceAndDisableSameID: disable must win when both force and
// disable name the same ID in a single Resolve call. This follows from
// Resolve's fixed loop order -- the disable loop always runs after the force
// loop, deleting from s.active anything the force loop just set -- not from
// anything about element order within the force/disable slices themselves;
// only one slice arrangement is exercised here.
func TestResolveForceAndDisableSameID(t *testing.T) {
	s := Resolve(Profile{}, []ID{QkAddPathHeuristic}, []ID{QkAddPathHeuristic})
	if s.Active(QkAddPathHeuristic) {
		t.Fatal("disable must win over force for the same ID in one Resolve call")
	}
}

// TestVendorOnlyDegradationComposesWithAppliesTo pins the vendor-only
// degradation composing with AppliesTo. Making "known vendor, unparsed
// version" reachable was supposed to let quirk matching "degrade to
// vendor-only", but VersionBetween itself always returns false
// for an unparsed version -- by design, per its doc comment -- so a naively
// range-scoped AppliesTo (VersionBetween(p.Version, min, max) alone) would
// silently NOT apply to such a profile, the opposite of widening. This test
// uses the idiom VersionBetween's doc comment now points AppliesTo authors
// at -- `p.Vendor == "cisco" && (!p.Version.OK || VersionBetween(...))` --
// and confirms it actually does apply to a cisco profile whose version
// didn't parse, proving the degradation composes end to end when an
// AppliesTo is written correctly.
func TestVendorOnlyDegradationComposesWithAppliesTo(t *testing.T) {
	const testID ID = "QK_TEST_VENDOR_ONLY_WIDENING"
	withTestEntry(t, Entry{
		ID:    testID,
		Layer: "test",
		AppliesTo: func(p Profile) bool {
			return p.Vendor == "cisco" && (!p.Version.OK || VersionBetween(p.Version, "10.0", "11.0"))
		},
	})

	p := ProfileFromSysDescr("Cisco IOS XR Software, Version UNKNOWN-BUILD")
	if p.Vendor != "cisco" || p.Version.OK {
		t.Fatalf("test fixture assumption broken: want a cisco profile with Version.OK == false, got %+v", p)
	}
	s := Resolve(p, nil, nil)
	if !s.Active(testID) {
		t.Fatal("known vendor with an unparsed version must still statically match an AppliesTo written with the !Version.OK-widening idiom")
	}
}

// TestResolveVersionUnparsedRespectsDisable pins that s.active itself must
// not record a disabled quirk as active. Resolve used to write
// QK_VERSION_UNPARSED into s.active *after* the
// disable loop ran, so a disable of QK_VERSION_UNPARSED left s.active
// (not just Active()) reporting the quirk as on -- harmless only because
// Active re-checks s.disabled, but not a truthful record of state for any
// future code that iterates s.active directly.
func TestResolveVersionUnparsedRespectsDisable(t *testing.T) {
	p := ProfileFromSysDescr("TotallyNewNOS v1") // present sysDescr, unparseable version
	s := Resolve(p, nil, []ID{QkVersionUnparsed})
	if s.Active(QkVersionUnparsed) {
		t.Fatal("QK_VERSION_UNPARSED must stay inactive when disabled")
	}
	if s.active[QkVersionUnparsed] {
		t.Fatal("s.active must not record a disabled quirk as active; internal state must be truthful, not just Active()'s output")
	}
}

// TestZeroValueSetLatch pins that Set is exported with exported methods,
// and Resolve is only the *intended* constructor, not
// an enforced one -- a caller who writes quirk.Set{} directly must not get a
// nil-map write panic out of Latch.
func TestZeroValueSetLatch(t *testing.T) {
	var s Set
	if newly := s.Latch(QkTSZero); !newly {
		t.Fatal("first Latch on a zero-value Set must report newly=true")
	}
	if !s.Active(QkTSZero) {
		t.Fatal("latched quirk must be active on a zero-value Set")
	}

	var s2 Set
	if newly := (&s2).Latch(QkTSZero); !newly {
		t.Fatal("Latch via explicit pointer-to-zero-value Set must not panic and must report newly=true")
	}
}
