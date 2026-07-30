package types

import "strings"

import "testing"

// The 15-character limit is the whole point: Windows does not reject a longer
// name, it truncates it, and every downstream check then reports healthy while
// the truncated name has no DNS record. These are the real names from the live
// cluster where that happened.
func TestValidateNetBIOSName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr bool
		why     string
	}{
		{"Newer-Broker", false, "12 chars — this one always worked on the rig"},
		{"bc2-Broker", false, "10 chars"},
		{"exactly-15-char", false, "15 chars is the limit, not over it"},
		{"bcluster2-Broker", true, "16 chars — truncates to bcluster2-Broke"},
		{"bcluster2-Broker5", true, "17 chars — same truncation, failed identically"},
		{"bcluster2-Broker6", true, "17 chars — the one observed failing"},
		{"", true, "required"},
		{"has space", false, "spaces are legal in a NetBIOS name"},
		{`back\slash`, true, "invalid character"},
		{"comma,name", true, "invalid character"},
	} {
		err := ValidateNetBIOSName("broker", tc.name)
		if tc.wantErr && err == nil {
			t.Errorf("%q (%s): expected an error, got none", tc.name, tc.why)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%q (%s): unexpected error: %v", tc.name, tc.why, err)
		}
	}
}

// The message has to name the truncated form, because that is the name the
// operator will not find in DNS and would otherwise never think to look for.
func TestValidateNetBIOSNameNamesTheTruncation(t *testing.T) {
	err := ValidateNetBIOSName("broker", "bcluster2-Broker6")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"bcluster2-Broke", "No such host is known", "15"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q, got: %v", want, err)
		}
	}
}
