package types

import (
	"fmt"
	"strings"
)

// reservedDeviceNames are the DOS device names Windows still reserves at every
// level of every path. Creating "CON.vhdx" does not fail with a name error — the
// open is redirected to the device, so a capture would write a multi-gigabyte
// image into the console and report success.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// MaxFileSafeName bounds a name that becomes one path component. Windows caps a
// path at 260 characters by default, and a template's name has a library
// directory in front of it and ".vhdx" behind it, so a generous component limit
// still leaves room.
const MaxFileSafeName = 64

// ValidateFileSafeName reports why a name cannot be used as a filename
// component, or nil when it is fine. Template names become the captured VHDX's
// filename, so a name the operator can type but Windows cannot store is a
// failure discovered at the end of a multi-gigabyte copy rather than at the
// point of asking.
//
// This is the same category of defect as ValidateNetBIOSName: names that Windows
// accepts loosely and resolves into something other than what was asked for.
// The name is checked exactly as given, apart from the empty case: trimming it
// here would accept "gold " and then validate a different string from the one
// about to be stored, which is how the trailing-space collision below gets past
// a check that looks like it covers it. Callers trim before they validate.
func ValidateFileSafeName(kind, name string) error {
	n := name
	if strings.TrimSpace(n) == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(n) > MaxFileSafeName {
		return fmt.Errorf("%s name is %d characters; use %d or fewer, since the name becomes a filename under the library directory",
			kind, len(n), MaxFileSafeName)
	}
	if i := strings.IndexAny(n, `\/:*?"<>|`); i >= 0 {
		return fmt.Errorf("%s name %q contains %q, which is not valid in a filename", kind, n, n[i:i+1])
	}
	for _, r := range n {
		if r < 0x20 {
			return fmt.Errorf("%s name %q contains a control character", kind, n)
		}
	}
	// Windows silently strips a trailing dot or space from a path component, so
	// "gold " and "gold" become the same file while the centre holds them as two
	// records.
	if strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") {
		return fmt.Errorf("%s name %q ends with a dot or space, which Windows strips from a filename — two names that differ only by that would collide on disk", kind, n)
	}
	// The reservation applies to the stem, so "NUL.vhdx" is reserved too.
	stem := n
	if i := strings.Index(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	if reservedDeviceNames[strings.ToUpper(stem)] {
		return fmt.Errorf("%s name %q is a reserved Windows device name; a file cannot be created with it", kind, n)
	}
	return nil
}
