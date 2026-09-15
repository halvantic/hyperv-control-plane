package hyperv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"testing"
)

// TestJoinDomainCredentialNotInEnvironment proves the actual property task 2
// asks for: the credential JoinDomain hands to powershell.exe must never
// appear in that child process's own environment. It runs a real
// powershell.exe (skipped where one is not available) through the same
// execPowerShellStdin path JoinDomain uses, with a script that reads the
// credential from stdin exactly as JoinDomain's script does, then inspects
// its OWN environment from the inside — the most direct way to prove the
// negative, rather than inferring it from how the Go side happens to build
// the command.
//
// A random-per-run marker stands in for the password so this cannot pass by
// coincidentally matching something already present in the ambient
// environment (PATH, etc.). The marker is random test data generated fresh
// each run, not a real secret, so embedding it in the script text (to check
// for it) does not itself recreate the problem this test exists to catch.
func TestJoinDomainCredentialNotInEnvironment(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires a real powershell.exe")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH")
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	marker := "BALLAST-TEST-" + hex.EncodeToString(buf[:])
	user := "join-test-user"

	// Mirrors JoinDomain's actual script shape: read user then password from
	// stdin, one line each. Then the check this test exists to make.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$joinUser = [Console]::In.ReadLine()
$joinPass = [Console]::In.ReadLine()
if ($joinPass -ne '%s') { throw 'stdin did not deliver the expected value' }
foreach ($kv in [Environment]::GetEnvironmentVariables().GetEnumerator()) {
  if ([string]$kv.Value -like '*%s*') {
    throw ('credential leaked into environment variable ' + $kv.Key)
  }
}
'OK'
`, marker, marker)

	stdin := []byte(user + "\n" + marker + "\n")
	_, err := execPowerShellStdin(context.Background(), script, stdin)
	for i := range stdin {
		stdin[i] = 0
	}
	if err != nil {
		t.Fatalf("credential-not-in-environment check failed: %v", err)
	}
}

func TestOuFromDN(t *testing.T) {
	cases := []struct {
		name string
		dn   string
		want string
	}{
		{"nested OU", "CN=HV01,OU=BallastHosts,OU=Hyper-V,DC=ballast,DC=local", "OU=BallastHosts,OU=Hyper-V,DC=ballast,DC=local"},
		{"single OU", "CN=HV01,OU=BallastHosts,DC=ballast,DC=local", "OU=BallastHosts,DC=ballast,DC=local"},
		{"default Computers container", "CN=HV01,CN=Computers,DC=ballast,DC=local", "CN=Computers,DC=ballast,DC=local"},
		{"no comma at all", "CN=HV01", ""},
		{"trailing comma only", "CN=HV01,", ""},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ouFromDN(c.dn); got != c.want {
				t.Errorf("ouFromDN(%q) = %q, want %q", c.dn, got, c.want)
			}
		})
	}
}
