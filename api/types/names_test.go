package types

import (
	"strings"
	"testing"
)

func TestValidateFileSafeName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"ordinary", "ws2025-gold", false},
		{"spaces inside", "Windows Server 2025", false},
		{"dot inside", "ws2025.gold", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"backslash", `ws2025\gold`, true},
		{"forward slash", "ws2025/gold", true},
		{"colon", "C:gold", true},
		{"wildcard", "ws2025*", true},
		{"pipe", "ws|gold", true},
		{"trailing dot", "gold.", true},
		{"trailing space", "gold ", true},
		{"reserved device", "NUL", true},
		{"reserved device lowercase", "con", true},
		{"reserved device with extension", "PRN.image", true},
		{"reserved word as prefix only", "console", false},
		{"too long", strings.Repeat("a", MaxFileSafeName+1), true},
		{"at the limit", strings.Repeat("a", MaxFileSafeName), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFileSafeName("template", tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateFileSafeName(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
