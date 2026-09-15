package reconcile

import (
	"strings"
	"testing"
	"time"
)

func TestOuDriftCondition(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		observed   string
		desired    string
		wantStatus bool
		wantReason string
	}{
		{"exact match", "OU=BallastHosts,DC=ballast,DC=local", "OU=BallastHosts,DC=ballast,DC=local", true, "InDeclaredOU"},
		{"case-insensitive match", "ou=BallastHosts,dc=ballast,dc=local", "OU=BallastHosts,DC=ballast,DC=local", true, "InDeclaredOU"},
		{"whitespace-insensitive match", "  OU=BallastHosts,DC=ballast,DC=local ", "OU=BallastHosts,DC=ballast,DC=local", true, "InDeclaredOU"},
		{"in default Computers container instead", "CN=Computers,DC=ballast,DC=local", "OU=BallastHosts,DC=ballast,DC=local", false, "WrongOU"},
		{"in a sibling OU", "OU=OtherHosts,DC=ballast,DC=local", "OU=BallastHosts,DC=ballast,DC=local", false, "WrongOU"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ouDriftCondition(c.observed, c.desired, now)
			if got.Status != c.wantStatus {
				t.Errorf("Status = %v, want %v", got.Status, c.wantStatus)
			}
			if got.Reason != c.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.wantReason)
			}
			if got.Type != "DomainJoin/OU" {
				t.Errorf("Type = %q, want DomainJoin/OU", got.Type)
			}
			if !c.wantStatus {
				// The remedy must name a real, runnable one-step fix, not just
				// state the fact — CLAUDE.md's "name the cause and offer the
				// action" applies even when Ballast cannot perform the action
				// itself.
				if !strings.Contains(got.Message, "Move-ADObject") {
					t.Errorf("Message %q does not name the one-step remedy", got.Message)
				}
			}
		})
	}
}
