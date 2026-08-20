package hyperv

import (
	"os"
	"testing"
)

// TestDumpScriptsForParseCheck writes the generated scripts out so they can be
// checked with PowerShell's own parser — the only authority on whether a script
// built by string concatenation is valid.
func TestDumpScriptsForParseCheck(t *testing.T) {
	dir := os.Getenv("BALLAST_SCRIPT_DUMP")
	if dir == "" {
		t.Skip("set BALLAST_SCRIPT_DUMP to write the scripts out")
	}
	for name, body := range map[string]string{
		"capture.ps1": captureTemplateScript(true, true),
		"deploy.ps1":  deployFromTemplateScript(true),
	} {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
