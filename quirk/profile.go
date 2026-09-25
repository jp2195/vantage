// Package quirk implements the vendor-quirk framework: RouterProfile
// detection, the central quirk registry, and per-session QuirkSets with
// static (profile-matched) and dynamic (runtime-latched) activation.
// Runtime evidence always outranks version claims.
package quirk

import (
	"regexp"
	"strconv"
	"strings"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// Version is a router's software version, both as comparable numeric parts
// and as the normalized display string it was derived from. A zero Version
// (OK == false) means the sysDescr text didn't match any known version
// scheme; range-scoped quirk matching (VersionBetween) always reports no
// match for such a Version, and Resolve records QK_VERSION_UNPARSED instead.
type Version struct {
	Parts []int  // comparable, e.g. 10.2(3)F -> [10 2 3]
	Raw   string // normalized display form, e.g. "10.2.3"
	OK    bool
}

// Profile is a router's identity as detected from its BMP-reported sysDescr:
// vendor/OS family plus parsed version, kept alongside the raw string that
// produced them.
type Profile struct {
	Vendor   string
	OS       string
	Version  Version
	SysDescr string
}

// RouterInfo projects a Profile onto the wire RouterInfo message attached to
// envelopes.
func (p Profile) RouterInfo() *vantagev1.RouterInfo {
	return &vantagev1.RouterInfo{Vendor: p.Vendor, Os: p.OS, Version: p.Version.Raw, SysDescr: p.SysDescr}
}

// anchors identifies a router's vendor/OS from its sysDescr banner, with no
// version requirement at all: detection is deliberately split into this
// vendor/OS anchor stage and the version-extraction stage below (see
// versionPatterns) so that a banner naming a known vendor in an
// unrecognized version format still yields a known Vendor/OS with
// Version.OK == false, rather than an entirely empty Profile (an
// unparseable version degrades matching to vendor-only, the safe
// direction, rather than to no match at all).
//
// This table is a FALLBACK, not the primary source of identity. Operator
// configuration (collector.RouterOverrides, keyed by router IP) is
// authoritative, because an operator knows what their devices are and a
// banner frequently does not say -- see ProfileFromSysDescr's doc comment.
// Anchors exist so that a zero-config deployment still identifies something.
//
// Every anchor here must correspond to a banner this project has actually
// captured off a BMP Initiation; quirk/profile_test.go's observedBanners is
// that list. This rule is not fussiness. The five anchors this package
// shipped with were written from documentation and `show version` output,
// and when three real senders were finally compared against them they
// matched NONE of the three. NX-OS is the clearest case: `show version`
// really does begin "Cisco Nexus Operating System (NX-OS) Software", but
// what the box puts on the BMP wire is built from the chassis line and never
// contains "NX-OS" at all -- hence the Nexus anchor below, which is what the
// wire actually carries. Juniper, Arista and classic-IOS anchors were
// removed rather than kept as decoration: no sample, no anchor.
//
// The NX-OS and IOS XR literals are kept alongside the observed ones. They
// have never been seen on a BMP Initiation either, but unlike the removed
// three they name platforms actually in this fleet, and real hardware
// plausibly sends a fuller banner than the XRd/n9kv virtual images do.
// Neither is load-bearing now that config supplies identity.
//
// Order matters: the more specific Cisco anchors are tested before anything
// generic. The patterns do not overlap on any observed banner.
var anchors = []struct {
	re     *regexp.Regexp
	vendor string
	os     string
}{
	{regexp.MustCompile(`NX-OS`), "cisco", "nxos"},
	// Observed: "Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)".
	// Real hardware sends the same shape with its own model, e.g.
	// "Nexus9000 C9336C-FX2 Chassis, ...".
	{regexp.MustCompile(`Nexus\d+`), "cisco", "nxos"},
	{regexp.MustCompile(`IOS[ -]?XR`), "cisco", "iosxr"},
	// Observed: "FRRouting 10.3_git".
	{regexp.MustCompile(`FRRouting`), "frr", "frr"},
}

// versionPatterns extracts the version substring for a vendor/OS pair
// already fixed by the anchor stage. Each vendor's version-keyword casing
// and value format is its own:
//
//   - NX-OS banners are observed in the wild with both "Version" and
//     lowercase "version" (e.g. classic "Cisco NX-OS(tm) ..., Version
//     5.0(3)N1(1a)" vs. the newer "Cisco Nexus Operating System (NX-OS)
//     Software, nxos version 10.2(3)F"), so its pattern matches either case.
//   - IOS XR banners capitalize "Version".
//   - FRR spells its version directly after the "FRRouting" token.
//
// A missing map entry, or a FindStringSubmatch miss against a real anchor
// match, both leave Version at its zero value (OK == false) while Vendor/OS
// are kept — that is the point of splitting anchor from version extraction.
var versionPatterns = map[string]*regexp.Regexp{
	"cisco/nxos":  regexp.MustCompile(`(?i)version ([0-9][0-9.()A-Za-z]*)`),
	"cisco/iosxr": regexp.MustCompile(`Version ([0-9][0-9.]*)`),
	// FRR appends a build suffix its own version string carries verbatim
	// ("10.3_git"), so the capture stops at the numeric run and
	// normalizeVersion never sees the suffix.
	"frr/frr": regexp.MustCompile(`FRRouting ([0-9][0-9.]*)`),
}

// numRe pulls the numeric runs out of an arbitrary vendor version string,
// discarding separators, parenthesized maintenance numbers, and trailing
// letter suffixes.
var numRe = regexp.MustCompile(`[0-9]+`)

// normalizeVersion extracts the numeric components of any vendor version
// scheme: "10.2(3)F" -> [10 2 3]; "21.4R3-S2.6" -> [21 4 3] (first three
// components identify the release train; S-suffixes are ignored for ranges).
func normalizeVersion(raw string) Version {
	nums := numRe.FindAllString(raw, 3)
	if len(nums) == 0 {
		// Unreachable today: every versionPatterns entry's capture group
		// begins with "[0-9]", so a real FindStringSubmatch match never
		// hands this function a digit-free raw string. Kept as a defensive
		// guard rather than deleted: if that invariant is ever broken by a
		// future pattern, returning the zero Version here (not
		// Version{Raw: raw}) preserves the contract documented on Version --
		// OK == false always means a fully zero-value Version, never one
		// that leaks a non-normalized raw string while claiming failure.
		return Version{}
	}
	v := Version{OK: true}
	for _, n := range nums {
		// strconv.Atoi on a pathologically long digit run (fuzz input, not a
		// real version string) returns strconv.ErrRange with a clamped
		// magnitude rather than panicking; the error is deliberately
		// ignored, since a clamped-but-still-ordered value is fine for
		// range comparisons and this path must never fail the session.
		i, _ := strconv.Atoi(n)
		v.Parts = append(v.Parts, i)
	}
	parts := make([]string, len(v.Parts))
	for i, p := range v.Parts {
		parts[i] = strconv.Itoa(p)
	}
	v.Raw = strings.Join(parts, ".")
	return v
}

// ProfileFromSysDescr identifies a router's vendor/OS/version from its BMP
// Initiation sysDescr TLV (RFC 7854 §4.3). The input is attacker-controlled
// router text: it may be empty, contain embedded NUL bytes, or otherwise not
// match any known banner format. ProfileFromSysDescr never panics.
//
// Detection is two-staged (see anchors/versionPatterns above):
//  1. Vendor/OS anchor — no version requirement.
//  2. Version extraction — only attempted once the anchor fixes vendor/OS.
//
// If no anchor matches, Vendor/OS/Version are all zero-value (an entirely
// unrecognized router). If an anchor matches but version extraction does
// not, Vendor/OS are populated and Version.OK is false: a known vendor with
// an unparseable version, which callers can rely on Resolve's
// QK_VERSION_UNPARSED accommodation to handle rather than a separate
// failure mode here.
//
// Version extraction (see anchorLine) is scoped to the anchor match's own
// line: the version pattern runs with FindStringSubmatch, which is a
// leftmost-match over whatever string it's given, so running it over the
// whole of s would let an unrelated "version"-shaped token anywhere in s --
// before the anchor, or on an earlier/later line -- win over the real one.
func ProfileFromSysDescr(s string) Profile {
	p := Profile{SysDescr: s}
	for _, a := range anchors {
		loc := a.re.FindStringIndex(s)
		if loc == nil {
			continue
		}
		p.Vendor, p.OS = a.vendor, a.os
		if re, ok := versionPatterns[a.vendor+"/"+a.os]; ok {
			if m := re.FindStringSubmatch(anchorLine(s, loc[0])); m != nil {
				p.Version = normalizeVersion(m[1])
			}
		}
		return p
	}
	return p
}

// bareVersionRe matches a sysDescr that is a version string and nothing else,
// e.g. XRd 26.1.1's entire BMP Initiation sysDescr: "26.1.1". Deliberately
// anchored at both ends -- a banner that merely CONTAINS a dotted number is
// not a bare version, and treating it as one would hand a confident, wrong
// version to anything with a digit in it.
var bareVersionRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)

// WithIdentity returns p with an operator-supplied vendor/OS substituted for
// whatever the banner stage concluded, re-running version extraction for the
// new vendor/OS. An empty vendor returns p unchanged, so a router with no
// configuration keeps its anchor result.
//
// Configuration outranks the banner because the banner is frequently not an
// identity at all. IOS-XR is the proof: XRd's entire sysDescr is "26.1.1", a
// bare version with no vendor token, and no regex can match that without
// matching everything. An operator can see the device. So config supplies
// identity, the banner supplies version, and between them XR is fully
// identified -- which neither achieves alone.
//
// Version extraction here is scoped to the FIRST line rather than to an
// anchor match's own line, because a config-supplied identity has no match
// position to scope to. Every banner observed to date is a single line, so
// the two are the same in practice; the first-line rule keeps a multi-line
// "show version"-style blob from donating an unrelated BIOS version, the same
// hazard anchorLine exists to prevent.
func (p Profile) WithIdentity(vendor, os string) Profile {
	if vendor == "" {
		return p
	}
	p.Vendor, p.OS = vendor, os
	// The previous vendor's version, if any, was extracted with a pattern
	// belonging to a vendor the operator has just contradicted. Drop it
	// rather than carry it across.
	p.Version = Version{}
	line := firstLine(p.SysDescr)
	if re, ok := versionPatterns[vendor+"/"+os]; ok {
		if m := re.FindStringSubmatch(line); m != nil {
			p.Version = normalizeVersion(m[1])
			return p
		}
	}
	if bareVersionRe.MatchString(line) {
		p.Version = normalizeVersion(line)
	}
	return p
}

// firstLine returns s up to its first CR or LF, or all of s if it has none.
func firstLine(s string) string {
	if nl := strings.IndexAny(s, "\r\n"); nl >= 0 {
		return s[:nl]
	}
	return s
}

// anchorLine returns the substring of s running from the anchor match's own
// start position to the next newline, or to the end of s if there is none.
// Scoping version extraction to this substring restores the "version
// keyword follows the vendor/OS anchor, on the same line" ordering guarantee
// that the old per-vendor coupled regexes (e.g. "NX-OS.*?version …") enforced
// implicitly and the anchor/version-extraction split silently dropped: a
// boot-ROM "ROM Version 2.0" preceding the real vendor banner on the same
// line, or a BIOS version line preceding a multi-line NX-OS "show version"
// capture, must not be picked up in place of the version that actually
// belongs to the matched vendor/OS. This is deliberately single-line only --
// a multi-line vendor banner whose version keyword is not on the anchor's
// own line is left with Version.OK == false (vendor known, version
// unparsed), which is the truthful outcome, not a gap to be closed here.
func anchorLine(s string, start int) string {
	if nl := strings.IndexAny(s[start:], "\r\n"); nl >= 0 {
		return s[start : start+nl]
	}
	return s[start:]
}

// parseRange parses a "10.2" / "11" / "10.2.3.4" style range bound into
// comparable parts, keeping every numeric run the string contains. Unlike
// normalizeVersion, parseRange does NOT cap at 3 components.
//
// The two must not share a cap. normalizeVersion's 3-component limit governs
// Version.Parts as produced from a sysDescr banner (the first three
// components identify a vendor release train), but Version is exported with
// an exported Parts []int field, and VersionBetween accepts any Version a
// caller constructs directly -- not only ones ProfileFromSysDescr produced --
// so Version.Parts can legitimately be longer than 3 components. A range
// bound must be able to discriminate against all of them: truncating the
// bound to 3 components made cmp compare a longer Version's extra
// components against nothing, and by cmp's missing-component-is-zero rule
// that reliably favored the untruncated side, silently widening or
// narrowing the range depending on which side got cut. It also broke the
// simpler 3-part-Version case: cmp([10,2,3], [10,2,3,4]) == -1 (v is less,
// so a maxExcl of "10.2.3.4" should include v), but truncating the bound to
// [10,2,3] made cmp == 0 instead, excluding v -- backwards, since 10.2.3 is
// in fact less than 10.2.3.4.
func parseRange(s string) []int {
	var out []int
	for _, n := range numRe.FindAllString(s, -1) {
		i, _ := strconv.Atoi(n)
		out = append(out, i)
	}
	return out
}

// cmp orders two version-part slices component-wise, treating a missing
// trailing component as 0 (so [10, 2] == [10, 2, 0]).
func cmp(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// VersionBetween reports minIncl <= v < maxExcl. An empty bound means
// unbounded in that direction: VersionBetween(v, "", "8.0") means "any
// version before 8.0"; VersionBetween(v, "7.0", "") means "7.0 and every
// later version, with no upper bound"; VersionBetween(v, "", "") matches
// every parseable version. This holds symmetrically for both bounds — an
// empty minIncl has always meant unbounded-below here; an empty maxExcl now
// means unbounded-above too, rather than matching nothing. A malformed,
// non-empty bound that contains no digits at all (parseRange returns zero
// components) degrades the same way as an empty bound -- unbounded in that
// direction -- rather than the pre-fix asymmetry where a digit-free minIncl
// was (already, incidentally) unbounded via cmp's missing-component-is-zero
// rule while a digit-free maxExcl matched nothing at all: both are simply
// "no real bound was given", so both directions now degrade identically.
//
// Unparseable versions (v.OK == false) never match a range regardless of
// bounds, by design, so a naively range-scoped AppliesTo silently does not
// apply to a profile with a known vendor but an unparsed version -- the
// opposite of the intended "degrade to vendor-only" widening. An
// AppliesTo that wants that widening must say so explicitly, e.g.
// `p.Vendor == "cisco" && (!p.Version.OK || VersionBetween(p.Version,
// minIncl, maxExcl))`; VersionBetween itself cannot do this on the author's
// behalf since it has no way to know which vendor(s) the caller intends the
// widening to cover.
func VersionBetween(v Version, minIncl, maxExcl string) bool {
	if !v.OK {
		return false
	}
	if lo := parseRange(minIncl); len(lo) > 0 && cmp(v.Parts, lo) < 0 {
		return false
	}
	if hi := parseRange(maxExcl); len(hi) > 0 && cmp(v.Parts, hi) >= 0 {
		return false
	}
	return true
}
