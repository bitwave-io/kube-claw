// Package version carries the build-stamped release version (DESIGN.md §24).
//
// Version is set at build time via
//
//	-ldflags "-X github.com/traego/kube-claw/internal/version.Version=v0.4.0"
//
// and defaults to "dev" for local builds. The self-update plane compares it
// (semver) against the release manifest to detect new releases, and the
// controller writes it into ControlPlane status as the startup-confirmed
// signal, so an unstamped binary must never masquerade as a release.
package version

import "golang.org/x/mod/semver"

// Version is the stamped release version ("v0.4.0"), or "dev" when unstamped.
var Version = "dev"

// Get returns the stamped version, canonicalized to a leading "v" when it
// parses as semver (build pipelines sometimes stamp "0.4.0").
func Get() string {
	if v := "v" + Version; Version != "" && Version[0] != 'v' && semver.IsValid(v) {
		return v
	}
	return Version
}

// IsRelease reports whether the running binary carries a valid semver release
// version (i.e. is upgrade-comparable). "dev" builds are not.
func IsRelease() bool { return semver.IsValid(Get()) }

// Newer reports whether candidate is a strictly newer release than current.
// Invalid semver on either side is never "newer" (fail closed).
func Newer(candidate, current string) bool {
	c, r := canon(candidate), canon(current)
	if !semver.IsValid(c) || !semver.IsValid(r) {
		return false
	}
	return semver.Compare(c, r) > 0
}

// Valid reports whether v is a usable release version (canonicalized semver).
func Valid(v string) bool { return semver.IsValid(canon(v)) }

// Same reports whether a and b are the same release (canonicalized semver
// equality — "0.4.0" and "v0.4.0" are the same). Invalid semver on either
// side is never "same".
func Same(a, b string) bool {
	ca, cb := canon(a), canon(b)
	return semver.IsValid(ca) && semver.IsValid(cb) && semver.Compare(ca, cb) == 0
}

// Max returns the semver-newer of a and b, preferring valid semver over
// invalid; two invalid inputs return a. Used for the desired-version rule
// (DESIGN.md §24.2): desired = max(helm-pinned, approved).
func Max(a, b string) string {
	ca, cb := canon(a), canon(b)
	switch {
	case !semver.IsValid(cb):
		return a
	case !semver.IsValid(ca):
		return b
	case semver.Compare(cb, ca) > 0:
		return b
	default:
		return a
	}
}

func canon(v string) string {
	if v == "" {
		return v
	}
	if v[0] != 'v' {
		return "v" + v
	}
	return v
}
