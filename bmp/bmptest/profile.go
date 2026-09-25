package bmptest

// Profile is one implementation's BMP identity: what it puts in the
// Initiation message that a collector uses to tell vendors apart.
//
// Profiles exist so the synthetic generator can impersonate a specific
// implementation when producing volume, rather than emitting one invented
// identity for everything. They are deliberately thin -- identity only, not
// behavior -- because identity is the part most prone to going wrong and
// the part a generator cannot discover for itself.
type Profile struct {
	// Name is the token used to select this profile on the command line.
	Name string
	// Corpus is the directory under bgp/testdata/corpus holding captures from
	// this implementation. TestProfilesAreDerivedFromRealCaptures uses it to
	// prove SysDescr against real bytes, so a profile cannot name an
	// implementation the corpus has never seen.
	Corpus string
	// SysDescr is the Initiation sysDescr TLV (type 1) verbatim as this
	// implementation sends it. NOT what its CLI prints, and NOT what its
	// documentation says -- those disagree, which is exactly how
	// quirk.anchors ended up matching nothing.
	SysDescr string
	// SysName is a representative sysName TLV (type 2). Unlike SysDescr this
	// is per-device rather than per-implementation, so it is a sample, and
	// the generator overrides it per simulated router.
	SysName string
}

// Profiles returns the vendor identities that have been measured off committed
// captures. Every entry is enforced against real bytes by
// TestProfilesAreDerivedFromRealCaptures; adding one without a capture behind
// it fails the build.
//
// Notably absent: IOS-XE. No lab IOS-XE device has sent a BMP message --
// configuration is accepted on Cat8000v 17.18.02 and the session never
// initiates (measured 2026-08-27: BGP reached Established and TCP to the
// collector opened, yet BMP made zero connection attempts; cause
// unresolved), so there is no capture to derive an IOS-XE identity from
// and inventing one is the mistake this whole mechanism exists to
// prevent. Arista, Junos and SR
// Linux are absent for the same reason.
func Profiles() []Profile {
	return []Profile{
		{
			Name:   "iosxr",
			Corpus: "iosxr",
			// A bare version string with no vendor token whatsoever. This is
			// why quirk.anchors' `IOS[ -]?XR` pattern never fires: XR does
			// not put "IOS-XR" anywhere in its Initiation message.
			SysDescr: "26.1.1",
			SysName:  "xr-pe1",
		},
		{
			Name:   "nxos",
			Corpus: "nxos",
			// Built from the CHASSIS line, not the "Cisco Nexus Operating
			// System (NX-OS) Software" line that `show version` starts with
			// -- so the `NX-OS` anchor misses real hardware too.
			SysDescr: "Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)",
			SysName:  "nx-spine1",
		},
		{
			Name:     "frr",
			Corpus:   "frr",
			SysDescr: "FRRouting 10.3_git",
			// FRR in a container reports the container ID as its hostname.
			SysName: "9dd321a572fe",
		},
	}
}
