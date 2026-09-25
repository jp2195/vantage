package quirk

import (
	"reflect"
	"strings"
	"testing"
)

// TestProfileFromSysDescr and TestVersionBetween cover the baseline cases
// for this detector.

func TestProfileFromSysDescr(t *testing.T) {
	cases := []struct {
		descr, vendor, os, version string
		ok                         bool
	}{
		{"Cisco IOS XR Software, Version 7.9.2", "cisco", "iosxr", "7.9.2", true},
		{"Cisco Nexus Operating System (NX-OS) Software, nxos version 10.2(3)F", "cisco", "nxos", "10.2.3", true},
		// These three banners come from documentation, not from any BMP
		// Initiation this project has captured, and their anchors were
		// removed for exactly that reason -- see the anchors table. They are
		// kept here as rows expecting NO identification, so that the day a
		// real JUNOS/EOS/classic-IOS banner is captured and an anchor is
		// added back, this test fails and the addition is a deliberate act
		// rather than a silent one.
		{"Juniper Networks, Inc. mx480 internet router, kernel JUNOS 21.4R3-S2.6", "", "", "", false},
		{"Arista Networks EOS version 4.30.2F", "", "", "", false},
		{"Cisco IOS Software, Version 15.7(3)M", "", "", "", false},
		{"TotallyNewNOS v1", "", "", "", false},
	}
	for _, c := range cases {
		p := ProfileFromSysDescr(c.descr)
		if p.Vendor != c.vendor || p.OS != c.os || p.Version.OK != c.ok {
			t.Fatalf("%q -> %+v", c.descr, p)
		}
		if c.ok && p.Version.Raw != c.version {
			t.Fatalf("%q version %q want %q", c.descr, p.Version.Raw, c.version)
		}
	}
}

func TestVersionBetween(t *testing.T) {
	v := ProfileFromSysDescr("Cisco Nexus Operating System (NX-OS) Software, nxos version 10.2(3)F").Version
	if !VersionBetween(v, "10.0", "11.0") || VersionBetween(v, "10.3", "11.0") {
		t.Fatal("range check wrong")
	}
	if VersionBetween(Version{}, "0", "99") {
		t.Fatal("unparseable version must not match ranges")
	}
}

// TestProfileFromSysDescrRealWorldBanners pins detection against real vendor
// banner text, not just synthetic fixtures: the NX-OS detector previously
// required a lowercase "version", which real Nexus banners rarely use, so
// it matched only a synthetic test fixture and nothing a real router would
// send.
func TestProfileFromSysDescrRealWorldBanners(t *testing.T) {
	cases := []struct {
		name, descr, vendor, os, version string
		ok                               bool
	}{
		{
			"nxos classic Version capitalized, N-suffix maintenance number",
			"Cisco NX-OS(tm) n5000, Software (n5000-uk9), Version 5.0(3)N1(1a), RELEASE SOFTWARE Copyright (c) 2002-2011 by Cisco Systems, Inc.",
			"cisco", "nxos", "5.0.3", true,
		},
		{
			"nxos modern banner, Version capitalized, no lowercase nxos token",
			"Cisco Nexus Operating System (NX-OS) Software, Version 9.3(5)",
			"cisco", "nxos", "9.3.5", true,
		},
		{
			"nxos fixture form, lowercase version",
			"Cisco Nexus Operating System (NX-OS) Software, nxos version 10.2(3)F",
			"cisco", "nxos", "10.2.3", true,
		},
		{
			"iosxr real banner with trailing copyright text",
			"Cisco IOS XR Software, Version 7.3.2 Copyright (c) 2013-2021 by Cisco Systems, Inc.",
			"cisco", "iosxr", "7.3.2", true,
		},
		{
			// Observed on the wire, and the reason the Nexus anchor exists:
			// n9kv puts the CHASSIS line in its BMP Initiation and never the
			// token "NX-OS", which the anchor table was written against.
			"nxos as actually sent over BMP -- chassis line, no NX-OS token",
			"Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)",
			"cisco", "nxos", "10.6.2", true,
		},
		{
			"frr as actually sent over BMP",
			"FRRouting 10.3_git",
			"frr", "frr", "10.3", true,
		},
		// The next three are documentation banners for vendors this project
		// has never captured. Their anchors were removed; see the anchors
		// table for why a plausible-looking pattern with no sample behind it
		// is worse than none.
		{
			"classic ios documentation banner -- no anchor, not claimed",
			"Cisco IOS Software, C3560E Software (C3560E-UNIVERSALK9-M), Version 15.2(4)E10, RELEASE SOFTWARE (fc3)",
			"", "", "", false,
		},
		{
			"arista documentation banner -- no anchor, not claimed",
			"Arista Networks EOS version 4.29.2F running on an Arista Networks DCS-7050SX3-48YC8",
			"", "", "", false,
		},
		{
			"junos documentation banner -- no anchor, not claimed",
			"Juniper Networks, Inc. qfx5100 Ethernet Switch, kernel JUNOS 18.4R2.7",
			"", "", "", false,
		},
		{
			// A known vendor (IOS XR anchor matches) whose version
			// text is not in the expected numeric form. Vendor/OS must
			// survive the failed version-extraction stage, and Version.OK
			// must be false -- vendor-only quirk matching, not "no match at
			// all".
			"known vendor, unparseable version",
			"Cisco IOS XR Software, Version UNKNOWN-BUILD",
			"cisco", "iosxr", "", false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ProfileFromSysDescr(c.descr)
			if p.Vendor != c.vendor || p.OS != c.os || p.Version.OK != c.ok {
				t.Fatalf("%q -> %+v", c.descr, p)
			}
			if c.ok && p.Version.Raw != c.version {
				t.Fatalf("%q version %q want %q", c.descr, p.Version.Raw, c.version)
			}
			if !c.ok && p.Version.OK {
				t.Fatalf("%q -> Version.OK should be false, got %+v", c.descr, p.Version)
			}
		})
	}
}

// TestProfileFromSysDescrVersionScopedToAnchorLine pins version extraction
// being scoped to the anchor match's own line (see anchorLine), restoring
// the "version keyword follows the anchor, on the same line" ordering
// guarantee that the old per-vendor coupled regexes enforced implicitly
// and that splitting anchor matching from version extraction silently
// dropped, since FindStringSubmatch over the whole sysDescr is a
// leftmost-match with no line boundary.
func TestProfileFromSysDescrVersionScopedToAnchorLine(t *testing.T) {
	// Single-line regression: an unrelated "Version" token (a boot-ROM
	// version) appears before the real vendor banner on the very same line.
	// Before the fix, the leftmost FindStringSubmatch over the whole string
	// grabbed "2.0" from "ROM Version 2.0" instead of the real "7.9.2" that
	// belongs to the matched IOS XR anchor.
	iosxr := ProfileFromSysDescr("ROM Version 2.0; Cisco IOS XR Software, Version 7.9.2")
	if iosxr.Vendor != "cisco" || iosxr.OS != "iosxr" || !iosxr.Version.OK {
		t.Fatalf("ROM-Version-prefixed IOS XR banner -> %+v", iosxr)
	}
	if iosxr.Version.Raw != "7.9.2" {
		t.Fatalf("ROM-Version-prefixed IOS XR banner version = %q, want %q", iosxr.Version.Raw, "7.9.2")
	}

	// Multi-line regression: a real "show version"-style NX-OS capture where
	// a BIOS version line precedes the NXOS version line. The anchor
	// ("NX-OS") matches on the first line, which has no version keyword on
	// it at all, so version extraction must fail (Version.OK == false)
	// rather than picking up "07.69" from the unrelated BIOS line -- before
	// the fix, the leftmost case-insensitive "version" match anywhere in the
	// string won, which was worse than an honest failure: it produced a
	// confident but wrong Version.OK == true with the BIOS build number
	// instead of the real NXOS release. Vendor/OS must still be populated
	// (the whole point of the anchor/version-extraction split), so this is
	// the truthful vendor-known-version-unparsed outcome, not a total
	// detection failure. This is not a request for multi-line version
	// support -- leaving this blob unparsed is the correct, intended result.
	nxosBlob := ProfileFromSysDescr("Cisco Nexus Operating System (NX-OS) Software\n" +
		"TAC support: http://www.cisco.com/tac\n" +
		"  BIOS: version 07.69\n" +
		"  NXOS: version 9.3(5)\n")
	if nxosBlob.Vendor != "cisco" || nxosBlob.OS != "nxos" {
		t.Fatalf("multi-line NX-OS show-version blob -> %+v", nxosBlob)
	}
	if nxosBlob.Version.OK {
		t.Fatalf("multi-line NX-OS show-version blob must leave Version.OK false (unparsed), got %+v", nxosBlob.Version)
	}

	// CRLF line-ending test: IOS XR with Windows-style CRLF separators must
	// parse the version correctly, not treat the whole blob as one line.
	iosxrCRLF := ProfileFromSysDescr("Cisco IOS XR Software, Version 7.9.2\r\nCopyright (c) 2013-2021")
	if iosxrCRLF.Vendor != "cisco" || iosxrCRLF.OS != "iosxr" || !iosxrCRLF.Version.OK {
		t.Fatalf("CRLF-separated IOS XR banner -> %+v", iosxrCRLF)
	}
	if iosxrCRLF.Version.Raw != "7.9.2" {
		t.Fatalf("CRLF-separated IOS XR version = %q, want %q", iosxrCRLF.Version.Raw, "7.9.2")
	}

	// CRLF test for the Nexus anchor, which replaces the classic-IOS and
	// Arista cases that used to sit here: those anchors were removed, and
	// line-scoping is a property of anchorLine rather than of any one
	// vendor, so one surviving observed-banner case covers it.
	nexusCRLF := ProfileFromSysDescr("Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)\r\nCopyright (c) 2002-2026")
	if nexusCRLF.Vendor != "cisco" || nexusCRLF.OS != "nxos" || !nexusCRLF.Version.OK {
		t.Fatalf("CRLF-separated Nexus banner -> %+v", nexusCRLF)
	}
	if nexusCRLF.Version.Raw != "10.6.2" {
		t.Fatalf("CRLF-separated Nexus version = %q, want %q", nexusCRLF.Version.Raw, "10.6.2")
	}

	// Anchor at end of string with no trailing newline: IOS XR with version
	// keyword on the same line but no closing newline. Version extraction
	// fails because the version keyword is on a different line.
	iosxrNoTrail := ProfileFromSysDescr("router running Cisco IOS XR")
	if iosxrNoTrail.Vendor != "cisco" || iosxrNoTrail.OS != "iosxr" {
		t.Fatalf("IOS XR anchor at string end -> %+v", iosxrNoTrail)
	}
	if iosxrNoTrail.Version.OK {
		t.Fatalf("IOS XR anchor at string end must have Version.OK false, got %+v", iosxrNoTrail.Version)
	}

	// Anchor at end of string: NX-OS with no trailing newline.
	nxosNoTrail := ProfileFromSysDescr("router is Cisco NX-OS")
	if nxosNoTrail.Vendor != "cisco" || nxosNoTrail.OS != "nxos" {
		t.Fatalf("NX-OS anchor at string end -> %+v", nxosNoTrail)
	}
	if nxosNoTrail.Version.OK {
		t.Fatalf("NX-OS anchor at string end must have Version.OK false, got %+v", nxosNoTrail.Version)
	}

	// Bare \r as line separator (not CRLF, just \r): the bug this test pins.
	// The anchor matches on the first line, but the real version is on a
	// later "line" (separated by bare \r). Without the fix, the version
	// extraction would grab the BIOS version instead.
	nxosBareR := ProfileFromSysDescr("Cisco Nexus Operating System (NX-OS) Software\r  BIOS: version 07.69\r  NXOS: version 9.3(5)\r")
	if nxosBareR.Vendor != "cisco" || nxosBareR.OS != "nxos" {
		t.Fatalf("bare-\\r NX-OS banner -> %+v", nxosBareR)
	}
	if nxosBareR.Version.OK {
		t.Fatalf("bare-\\r NX-OS must leave Version.OK false (version on different line), got %+v", nxosBareR.Version)
	}

	// Anchor matches but the version format is one this package cannot read,
	// leaving Version.OK == false (vendor-only degradation). This used to be
	// tested with two Junos banners; that anchor was removed for having no
	// captured sample behind it, so the property is pinned here on an
	// observed one instead. It is the property that matters, not the vendor.
	nexusOddVersion := ProfileFromSysDescr("Nexus9000 C9300v Chassis, Software Version NIGHTLY-BUILD")
	if nexusOddVersion.Vendor != "cisco" || nexusOddVersion.OS != "nxos" {
		t.Fatalf("Nexus banner with unreadable version -> %+v", nexusOddVersion)
	}
	if nexusOddVersion.Version.OK {
		t.Fatalf("unreadable version must leave Version.OK false, got %+v", nexusOddVersion.Version)
	}
}

// TestProfileFromSysDescrUnparsedVersionKeepsVendor further pins that
// behavior in isolation: a known-vendor anchor match with an unparseable
// version must retain Vendor/OS (not degrade to an entirely empty Profile)
// so that downstream can distinguish "unknown vendor" from "known vendor,
// unparsed version" -- the two cases QK_VERSION_UNPARSED exists to
// separate.
func TestProfileFromSysDescrUnparsedVersionKeepsVendor(t *testing.T) {
	p := ProfileFromSysDescr("Cisco IOS XR Software, Version UNKNOWN-BUILD")
	if p.Vendor != "cisco" || p.OS != "iosxr" {
		t.Fatalf("known vendor/OS must survive an unparseable version, got %+v", p)
	}
	if p.Version.OK {
		t.Fatalf("version must be unparsed, got %+v", p.Version)
	}
	if p.SysDescr != "Cisco IOS XR Software, Version UNKNOWN-BUILD" {
		t.Fatalf("SysDescr must be preserved verbatim, got %q", p.SysDescr)
	}
}

// --- additional tests beyond the baseline cases above, per the package's
// input-robustness and fuzz conventions (see bmp/tlv_test.go). ---

// TestRouterInfo pins Profile.RouterInfo()'s field-by-field projection onto
// the wire message. All four Profile/RouterInfo fields are plain strings,
// so a Vendor/Os transposition or a Version.Raw/SysDescr swap would compile
// cleanly and produce no type error -- only a field-value assertion like
// this one catches it.
func TestRouterInfo(t *testing.T) {
	p := Profile{
		Vendor:   "cisco",
		OS:       "iosxr",
		Version:  Version{Parts: []int{7, 9, 2}, Raw: "7.9.2", OK: true},
		SysDescr: "Cisco IOS XR Software, Version 7.9.2",
	}
	ri := p.RouterInfo()
	if ri.Vendor != "cisco" {
		t.Fatalf("Vendor = %q, want %q", ri.Vendor, "cisco")
	}
	if ri.Os != "iosxr" {
		t.Fatalf("Os = %q, want %q", ri.Os, "iosxr")
	}
	if ri.Version != "7.9.2" {
		t.Fatalf("Version = %q, want Version.Raw %q", ri.Version, "7.9.2")
	}
	if ri.SysDescr != "Cisco IOS XR Software, Version 7.9.2" {
		t.Fatalf("SysDescr = %q, want the original sysDescr string", ri.SysDescr)
	}
}

func TestProfileFromSysDescrEmpty(t *testing.T) {
	p := ProfileFromSysDescr("")
	if p.Vendor != "" || p.OS != "" || p.Version.OK || p.SysDescr != "" {
		t.Fatalf("empty sysDescr must yield a zero Profile, got %+v", p)
	}
}

// TestProfileFromSysDescrEmbeddedNUL guards an input-robustness property:
// InitInfo.SysDescr can contain embedded NUL bytes (strings.ToValidUTF8 only
// replaces invalid UTF-8 runs, and 0x00 is valid UTF-8). ProfileFromSysDescr
// must not panic on this input. Here the NUL falls inside the literal
// "IOS XR" the detector requires, so no detector matches at all and the
// result must degrade to an unparsed profile rather than misidentify the
// router or panic.
func TestProfileFromSysDescrEmbeddedNUL(t *testing.T) {
	descr := "Cisco IOS\x00XR Software, Version 7.9.2"
	p := ProfileFromSysDescr(descr)
	if p.SysDescr != descr {
		t.Fatalf("SysDescr must be preserved verbatim, got %q", p.SysDescr)
	}
	if p.Vendor != "" || p.OS != "" || p.Version.OK {
		t.Fatalf("NUL-split banner must not match any detector, got %+v", p)
	}
}

func TestVersionBetweenBoundaries(t *testing.T) {
	v := Version{Parts: []int{10, 2, 3}, Raw: "10.2.3", OK: true}
	if !VersionBetween(v, "10.2.3", "10.2.4") {
		t.Fatal("v must be included when minIncl equals v exactly")
	}
	if VersionBetween(v, "0", "10.2.3") {
		t.Fatal("v must be excluded when maxExcl equals v exactly")
	}
	// Shorter range bound than v's parts: missing trailing components
	// compare as zero, so v=[10,2,3] is still >= minIncl=[10,2].
	if !VersionBetween(v, "10.2", "11") {
		t.Fatal("shorter minIncl must still compare correctly against a longer version")
	}
}

// TestVersionBetweenEmptyBoundsAreUnbounded pins an empty maxExcl meaning
// unbounded, not "matches nothing": it must mean "no upper bound" (matching
// every version at or above minIncl), symmetric with an empty minIncl
// already meaning "no lower bound". Previously, parseRange("") returned nil
// and cmp(parts, nil) < 0 was never true, so an empty upper bound silently
// matched nothing -- not the natural "no upper bound" idiom an AppliesTo
// author would reach for.
func TestVersionBetweenEmptyBoundsAreUnbounded(t *testing.T) {
	v := Version{Parts: []int{10, 2, 3}, Raw: "10.2.3", OK: true}

	if !VersionBetween(v, "7.0", "") {
		t.Fatal("empty maxExcl must mean unbounded-above, not \"matches nothing\"")
	}
	if !VersionBetween(v, "", "99.0") {
		t.Fatal("empty minIncl must mean unbounded-below (already correct pre-fix)")
	}
	if !VersionBetween(v, "", "") {
		t.Fatal("both bounds empty must match every parseable version")
	}
	// Sanity: a non-empty upper bound that v is genuinely past must still
	// exclude it, so the fix didn't accidentally make maxExcl a no-op.
	if VersionBetween(v, "7.0", "10.2.3") {
		t.Fatal("non-empty maxExcl must still exclude a version at or past it")
	}
	// Unparseable version must still never match, empty bounds or not.
	if VersionBetween(Version{}, "", "") {
		t.Fatal("unparseable version must not match even fully unbounded range")
	}
}

// TestParseRangeKeepsFourthComponent replaces the old (buggy)
// TestParseRangeDropsFourthComponent: parseRange previously capped at 3
// numeric components, silently
// dropping a bound's trailing ".4". That was verifiably wrong, not a
// deliberate simplification: cmp([10,2,3], [10,2,3,4]) == -1 (10.2.3 is less
// than 10.2.3.4, so v should be INCLUDED by a maxExcl of "10.2.3.4"), but
// truncating the bound to [10,2,3] made cmp == 0 instead, which EXCLUDED v --
// backwards. parseRange no longer caps, so the 4th component now
// discriminates correctly.
func TestParseRangeKeepsFourthComponent(t *testing.T) {
	v := Version{Parts: []int{10, 2, 3}, Raw: "10.2.3", OK: true}
	if !VersionBetween(v, "0", "10.2.3.4") {
		t.Fatal("v=10.2.3 must be included when maxExcl is 10.2.3.4, since 10.2.3 < 10.2.3.4")
	}
	if VersionBetween(v, "0", "10.2.3") {
		t.Fatal("sanity: v must still be excluded when the (3-component) maxExcl equals v exactly")
	}
	// Additional exclusion directions: maxExcl with only 3 components must
	// still exclude v when they match exactly.
	if VersionBetween(v, "0", "10.2.3") {
		t.Fatal("v must be excluded when a 3-component maxExcl equals v exactly")
	}
	// And a 4-component minIncl must exclude a 3-component v when the 4-component
	// is greater.
	if VersionBetween(v, "10.2.3.4", "99.0") {
		t.Fatal("v=10.2.3 must be excluded when minIncl is 10.2.3.4, since 10.2.3 < 10.2.3.4")
	}
}

// TestVersionBetweenMalformedBoundsAreUnbounded pins that a non-empty bound
// string that contains no digits at all
// (parseRange returns zero components) is now treated the same as an empty
// bound -- unbounded in that direction -- symmetrically in both directions.
// Before the fix, a garbage minIncl was (already, incidentally) unbounded
// via cmp's missing-component-is-zero rule, since cmp(v.Parts, nil) is never
// negative for a non-negative Version.Parts; but a garbage maxExcl matched
// nothing, since cmp(v.Parts, nil) >= 0 is (almost) always true. Chosen fix:
// treat "parses to no components" as unbounded in both directions, the same
// as an empty string, rather than documenting the asymmetry -- an
// AppliesTo author who typos a bound should get the same "no real
// constraint" behavior regardless of which side they typo'd, not a silent
// full exclusion on one side only.
func TestVersionBetweenMalformedBoundsAreUnbounded(t *testing.T) {
	v := Version{Parts: []int{10, 2, 3}, Raw: "10.2.3", OK: true}
	if !VersionBetween(v, "garbage", "") {
		t.Fatal("a digit-free minIncl must be treated as unbounded-below")
	}
	if !VersionBetween(v, "", "garbage") {
		t.Fatal("a digit-free maxExcl must be treated as unbounded-above, not matching nothing")
	}
	if !VersionBetween(v, "garbage", "garbage") {
		t.Fatal("both bounds digit-free must match every parseable version, same as both bounds empty")
	}
}

// FuzzProfileFromSysDescr feeds ProfileFromSysDescr attacker-controlled
// router text (the BMP Initiation sysDescr TLV). Beyond "must never panic",
// it asserts two free invariants any real mutation bug (a swapped field, a
// lost byte) would break: the input string is always preserved verbatim in
// SysDescr, and a parsed version always carries both comparable Parts and a
// non-empty Raw (the naive "just don't panic" assertion would miss a
// mutation that set Version.OK without actually populating Parts/Raw).
// Seeded with real vendor banners plus hostile inputs — empty, NUL-laden,
// very long, invalid UTF-8, multi-line, and version-like strings with
// absurdly large numbers.
func FuzzProfileFromSysDescr(f *testing.F) {
	seeds := []string{
		"Cisco IOS XR Software, Version 7.9.2",
		"Cisco Nexus Operating System (NX-OS) Software, nxos version 10.2(3)F",
		"Juniper Networks, Inc. mx480 internet router, kernel JUNOS 21.4R3-S2.6",
		"Arista Networks EOS version 4.30.2F",
		"Cisco IOS Software, Version 15.7(3)M",
		"",
		"\x00",
		"\x00\x00\x00Cisco IOS XR Software, Version 7.9.2\x00",
		"Cisco IOS XR Software, Version 7\x009.2",
		strings.Repeat("A", 1<<20),
		"Version " + strings.Repeat("9", 500) + ".1.1",
		"JUNOS " + strings.Repeat("1", 300) + "R" + strings.Repeat("2", 300),
		"\xff\xfe\xfd invalid utf8 JUNOS 1.2R3",
		"TotallyNewNOS v1",
		// Multi-line banner, as a real "show version"-style capture might
		// produce. Explicitly NOT exercising multi-line matching semantics
		// (out of scope: a naive (?s) fix would wrongly let a lazy .*? cross
		// into an unrelated line, e.g. a NX-OS BIOS version line preceding
		// the NXOS one) -- this seed only pins that such input still
		// doesn't panic and still round-trips SysDescr.
		"Cisco Nexus Operating System (NX-OS) Software\nBIOS: version 1.2.3\nnxos: version 10.2(3)F\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p := ProfileFromSysDescr(s) // must not panic
		if p.SysDescr != s {
			t.Fatalf("SysDescr must be preserved verbatim: input %q, got %q", s, p.SysDescr)
		}
		if p.Version.OK && (len(p.Version.Parts) == 0 || p.Version.Raw == "") {
			t.Fatalf("Version.OK implies non-empty Parts and Raw, got %+v for input %q", p.Version, s)
		}
	})
}

// observedBanners is every sysDescr this project has actually captured off a
// BMP Initiation, transcribed from bgp/testdata/corpus/*/*.json. It is the
// only trustworthy input to anchor design: the five anchors this package
// shipped with were written from documentation and `show version` output, and
// matched none of these three. Add a row here when a new sender is captured,
// and never add an anchor without one.
//
// The distinction that makes this table worth having: NX-OS's `show version`
// really does begin "Cisco Nexus Operating System (NX-OS) Software", but what
// it puts on the BMP wire is built from the chassis line and never contains
// the token "NX-OS" at all. The CLI string and the BMP string are different
// strings, and only the second one is here.
var observedBanners = []struct {
	name       string
	sysDescr   string
	vendor, os string
	version    string // normalized Version.Raw; "" means Version.OK false
}{
	{
		name:     "nxos n9kv 10.6(2) -- chassis line, never says NX-OS",
		sysDescr: "Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)",
		vendor:   "cisco", os: "nxos", version: "10.6.2",
	},
	{
		name:     "frr 10.3",
		sysDescr: "FRRouting 10.3_git",
		vendor:   "frr", os: "frr", version: "10.3",
	},
	{
		// XRd sends a bare version with no vendor token whatsoever. There is
		// nothing here to anchor on, and inventing a pattern that matches a
		// bare dotted-quad would match anything. This row asserts the honest
		// outcome -- unidentified -- so that a future "fix" that guesses at
		// it fails this test rather than quietly claiming every unknown
		// sender is IOS-XR.
		name:     "iosxr XRd 26.1.1 -- no vendor token at all",
		sysDescr: "26.1.1",
		vendor:   "", os: "", version: "",
	},
}

func TestProfileFromSysDescrMatchesObservedBanners(t *testing.T) {
	for _, tt := range observedBanners {
		t.Run(tt.name, func(t *testing.T) {
			got := ProfileFromSysDescr(tt.sysDescr)
			if got.Vendor != tt.vendor || got.OS != tt.os {
				t.Errorf("vendor/os = %q/%q, want %q/%q", got.Vendor, got.OS, tt.vendor, tt.os)
			}
			if tt.version == "" {
				if got.Version.OK {
					t.Errorf("Version.OK = true (raw %q), want false", got.Version.Raw)
				}
				return
			}
			if !got.Version.OK {
				t.Fatalf("Version.OK = false, want %q", tt.version)
			}
			if got.Version.Raw != tt.version {
				t.Errorf("Version.Raw = %q, want %q", got.Version.Raw, tt.version)
			}
		})
	}
}

// QK_VERSION_UNPARSED means what its Registry description says: "sysDescr
// version string not parseable; vendor-only quirk matching in effect". That
// sentence presupposes a vendor. When no anchor matched there is no
// vendor-only matching to fall back to -- there is no matching at all -- so
// raising it there overstates what the collector knows.
//
// It also destroyed the flag's signal. Every sender observed to date failed
// every anchor, so the flag fired on every envelope from every router and
// could not distinguish an odd router from the normal case. Gating it on a
// known vendor is what makes a set bit mean something again.
func TestVersionUnparsedNeedsAKnownVendor(t *testing.T) {
	t.Run("unidentified sender does not raise it", func(t *testing.T) {
		s := Resolve(ProfileFromSysDescr("26.1.1"), nil, nil)
		if s.Active(QkVersionUnparsed) {
			t.Error("QK_VERSION_UNPARSED active for a banner with no vendor token; " +
				"there is no vendor to fall back to, so the flag claims more than is known")
		}
	})
	t.Run("known vendor with an unparseable version still raises it", func(t *testing.T) {
		// A real NX-OS anchor with a version format this package cannot read:
		// vendor/os are known, the version is not, and that is exactly the
		// case the flag exists to report.
		s := Resolve(ProfileFromSysDescr("Nexus9000 C9300v Chassis, Software Version wombat"), nil, nil)
		if !s.Active(QkVersionUnparsed) {
			t.Error("QK_VERSION_UNPARSED not active for a known vendor with an " +
				"unparseable version -- that is the flag's entire purpose")
		}
	})
	t.Run("empty sysDescr does not raise it", func(t *testing.T) {
		if Resolve(ProfileFromSysDescr(""), nil, nil).Active(QkVersionUnparsed) {
			t.Error("QK_VERSION_UNPARSED active for an absent Initiation TLV")
		}
	})
	t.Run("a fully identified sender does not raise it", func(t *testing.T) {
		if Resolve(ProfileFromSysDescr("FRRouting 10.3_git"), nil, nil).Active(QkVersionUnparsed) {
			t.Error("QK_VERSION_UNPARSED active for a sender whose version parsed fine")
		}
	})
}

// Operator configuration is the authoritative source of a router's identity,
// because a banner frequently does not carry one. IOS-XR is the case that
// forces this: XRd's entire BMP sysDescr is "26.1.1" -- a bare version with
// no vendor token anywhere in it. No anchor can ever match that without
// matching everything, but an operator knows perfectly well what the box is.
//
// The two sources are complementary rather than competing: config supplies
// identity, the banner supplies version. Together they identify XR
// completely, which neither does alone.
func TestWithIdentityUsesConfigVendorAndBannerVersion(t *testing.T) {
	// The XR case, end to end.
	p := ProfileFromSysDescr("26.1.1").WithIdentity("cisco", "iosxr")
	if p.Vendor != "cisco" || p.OS != "iosxr" {
		t.Fatalf("vendor/os = %q/%q, want cisco/iosxr", p.Vendor, p.OS)
	}
	if !p.Version.OK || p.Version.Raw != "26.1.1" {
		t.Errorf("Version = %+v, want 26.1.1 -- a banner that is nothing but a "+
			"version string is unambiguous once config supplies the vendor", p.Version)
	}
	if p.SysDescr != "26.1.1" {
		t.Errorf("SysDescr = %q, want it preserved verbatim", p.SysDescr)
	}
}

func TestWithIdentityOverridesTheAnchor(t *testing.T) {
	// An anchor said cisco/nxos; the operator says otherwise. The operator
	// wins: they can see the device, the regex is guessing from text.
	p := ProfileFromSysDescr("Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)").
		WithIdentity("acme", "acmeos")
	if p.Vendor != "acme" || p.OS != "acmeos" {
		t.Fatalf("vendor/os = %q/%q, want acme/acmeos", p.Vendor, p.OS)
	}
	// No version pattern is registered for acme/acmeos and the banner is not
	// a bare version, so the version is honestly unknown rather than carried
	// over from the vendor the operator just contradicted.
	if p.Version.OK {
		t.Errorf("Version = %+v, want unparsed: the nxos pattern must not be "+
			"applied to a vendor the operator overrode it to", p.Version)
	}
}

func TestWithIdentityEmptyVendorChangesNothing(t *testing.T) {
	// No config for this router: the anchor result stands untouched.
	banner := "FRRouting 10.3_git"
	want := ProfileFromSysDescr(banner)
	got := ProfileFromSysDescr(banner).WithIdentity("", "")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WithIdentity(\"\",\"\") = %+v, want it unchanged: %+v", got, want)
	}
}

func TestWithIdentityBareVersionOnlyWhenTheWholeBannerIsOne(t *testing.T) {
	// The bare-version fallback must not fire on a banner that merely
	// contains digits, or every unrecognized string with a number in it
	// would acquire a confident, wrong version.
	for _, descr := range []string{
		"router-7 uptime 4 days",
		"26.1.1 (build 9)",
		"v26.1.1",
		"",
	} {
		t.Run(descr, func(t *testing.T) {
			p := ProfileFromSysDescr(descr).WithIdentity("cisco", "iosxr")
			if p.Version.OK {
				t.Errorf("Version = %+v, want unparsed for %q", p.Version, descr)
			}
		})
	}
}

// A profile whose identity came from config, with a version the banner did
// carry, must not raise QK_VERSION_UNPARSED -- and one whose version stayed
// unreadable must. This is the flag becoming reachable for IOS-XR at all:
// before config supplied a vendor, XR could never satisfy the vendor gate.
func TestVersionUnparsedRespectsConfigSuppliedVendor(t *testing.T) {
	full := Resolve(ProfileFromSysDescr("26.1.1").WithIdentity("cisco", "iosxr"), nil, nil)
	if full.Active(QkVersionUnparsed) {
		t.Error("QK_VERSION_UNPARSED active for a fully identified router")
	}
	partial := Resolve(ProfileFromSysDescr("no version here").WithIdentity("cisco", "iosxr"), nil, nil)
	if !partial.Active(QkVersionUnparsed) {
		t.Error("QK_VERSION_UNPARSED not active for a config-identified vendor " +
			"whose banner carries no readable version")
	}
}
