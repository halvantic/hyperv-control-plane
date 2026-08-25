package hyperv

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

/* `(if ...)` without the $ sigil PARSES and fails at runtime.

   PowerShell reads "(if (...) {...} else {...})" as a command invocation named
   "if", so the parser accepts it and the host dies with "The term 'if' is not
   recognized as a name of a cmdlet". Confirmed on both Windows PowerShell 5.1
   and PowerShell 7.

   That makes it invisible to the parse checks used elsewhere in this package —
   two of these shipped, one of them in 0.4.146's duplicate-vNIC detection,
   where it would have failed on the host the first time a duplicate was found.

   An if-expression must be $(if ...). */
func TestNoUnsigiledIfExpressions(t *testing.T) {
	// A "(if (" not preceded by $, and not inside a PowerShell comment.
	bad := regexp.MustCompile(`[^$]\(if \(`)
	comment := regexp.MustCompile(`^\s*(#|//)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if comment.MatchString(line) || !bad.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d: an if-expression needs the $ sigil — $(if ...) — or PowerShell "+
				"treats it as a command called \"if\" and fails at runtime:\n\t%s",
				name, i+1, strings.TrimSpace(line))
		}
	}
}
