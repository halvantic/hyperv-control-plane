package main

import "testing"

func TestDefaultBackend(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{"windows", "powershell"},
		{"linux", "stub"},
		{"darwin", "stub"},
	}
	for _, tt := range tests {
		if got := defaultBackend(tt.goos); got != tt.want {
			t.Errorf("defaultBackend(%q) = %q, want %q", tt.goos, got, tt.want)
		}
	}
}
