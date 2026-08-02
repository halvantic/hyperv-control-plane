package types

import "testing"

// The capture job's size travels inside its human-readable message, so the
// format and its parse have to stay in step — and an unrecognised message must
// come back as "unknown", never as a plausible-looking number.
func TestCaptureResultRoundTrip(t *testing.T) {
	const dest = `C:\ClusterStorage\Datastore 1\Templates\ws2025-gold.vhdx`
	msg := FormatCaptureResult("ws2025-gold", dest, 45097156608)
	if got := ParseCapturedBytes(msg); got != 45097156608 {
		t.Fatalf("round trip: got %d from %q", got, msg)
	}
	// A path with its own parentheses must not confuse the parse.
	msg = FormatCaptureResult("gold", `C:\Templates (old)\gold.vhdx`, 1024)
	if got := ParseCapturedBytes(msg); got != 1024 {
		t.Fatalf("parenthesised path: got %d from %q", got, msg)
	}
	if got := ParseCapturedBytes("zero"); got != 0 {
		t.Fatalf("zero size: got %d", got)
	}
}

func TestParseCapturedBytesUnknown(t *testing.T) {
	for _, msg := range []string{
		"",
		"captured ws2025-gold",
		"captured ws2025-gold to C:\\x.vhdx",
		"captured ws2025-gold to C:\\x.vhdx (lots bytes)",
		"captured ws2025-gold to C:\\x.vhdx (-1 bytes)",
		"captured ws2025-gold to C:\\x.vhdx 42 bytes)",
	} {
		if got := ParseCapturedBytes(msg); got != 0 {
			t.Fatalf("ParseCapturedBytes(%q) = %d, want 0 (unknown)", msg, got)
		}
	}
}

func TestTemplateDeployable(t *testing.T) {
	tests := []struct {
		name string
		tpl  VMTemplate
		want bool
	}{
		{"ready with an image", VMTemplate{SourceDiskPath: `C:\t.vhdx`, Status: VMTemplateStatus{Phase: TemplateReady}}, true},
		{"still capturing", VMTemplate{SourceDiskPath: `C:\t.vhdx`, Status: VMTemplateStatus{Phase: TemplateCapturing}}, false},
		{"capture failed", VMTemplate{SourceDiskPath: `C:\t.vhdx`, Status: VMTemplateStatus{Phase: TemplateFailed}}, false},
		{"ready but no image", VMTemplate{Status: VMTemplateStatus{Phase: TemplateReady}}, false},
		{"no phase at all", VMTemplate{SourceDiskPath: `C:\t.vhdx`}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tpl.Deployable(); got != tt.want {
				t.Fatalf("Deployable() = %v, want %v", got, tt.want)
			}
		})
	}
}
