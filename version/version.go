// Package version holds the single Ballast release version string. Both the
// agent and centre import this so the centre can detect when a host is running
// a stale agent binary and surface an "update required" indicator.
package version

import (
	"strconv"
	"strings"
)

const Version = "1.0.0"

// ReleaseLine returns the release line Version belongs to, as "<major>.x" —
// "0.4.277-slice" and "0.3.90-slice" are both "0.x"; "1.4.2-slice" is "1.x".
//
// Major-only, deliberately. The published grandfathering commitment ("a
// deployment on a 0.x release keeps the entitlement it was installed under,
// for the life of that release line") is about major-version-zero as a whole,
// not any one minor version — minor bumps happen constantly during ordinary
// development and are not separate commercial lines. Licensing rules are
// compiled per release line (see centre/license), so this is the one place
// that boundary is decided.
//
// Falls back to "0.x" on anything unparseable rather than erroring: an
// unparseable version string must never be read as a stricter, unknown future
// line's rules when there is only one line's rules to fall back to safely.
func ReleaseLine() string { return releaseLineOf(Version) }

// releaseLineOf does the actual parsing, kept separate from ReleaseLine so it
// can be tested against version strings other than the one this binary was
// built with.
func releaseLineOf(v string) string {
	major := v
	if i := strings.IndexByte(major, '.'); i >= 0 {
		major = major[:i]
	}
	if _, err := strconv.Atoi(major); err != nil {
		return "0.x"
	}
	return major + ".x"
}
