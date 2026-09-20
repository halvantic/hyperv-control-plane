package version

import "testing"

func TestReleaseLine(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		// Real strings this repo has actually shipped.
		{"0.4.277-slice", "0.x"},
		{"0.3.90-slice", "0.x"},
		{"0.4.164", "0.x"},
		// A future major line.
		{"1.0.0", "1.x"},
		{"1.4.2-slice", "1.x"},
		// Unparseable: falls back to the one line whose rules exist, rather
		// than erroring or being read as some unknown stricter line.
		{"", "0.x"},
		{"not-a-version", "0.x"},
	}
	for _, c := range cases {
		if got := releaseLineOf(c.version); got != c.want {
			t.Errorf("releaseLineOf(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestReleaseLineOfTheActualShippedVersion(t *testing.T) {
	// Version itself, unmodified -- this is what every other package that
	// calls ReleaseLine() actually gets today, so it earns its own case.
	if got := ReleaseLine(); got != "1.x" {
		t.Errorf("ReleaseLine() for the real Version %q = %q, want 1.x", Version, got)
	}
}
