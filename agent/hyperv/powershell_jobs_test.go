package hyperv

import (
	"context"
	"strings"
	"testing"
)

// A clean transfer reports no note — nothing notable happened.
func TestFetchISOQuietOnCleanTransfer(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	note, err := newTestPS(f).FetchISO(context.Background(), "http://centre/isos/w.iso", `C:\ClusterStorage\DS1\ISOs\w.iso`)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("expected no note on a clean transfer, got %q", note)
	}
}

// A transfer that dropped and resumed still succeeds, but says so: a link that
// keeps cutting multi-GB transfers is worth surfacing even when the retry wins.
func TestFetchISOReportsResume(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte("NOTE=transfer resumed 2 time(s) after a dropped connection\r\n"),
	}}
	note, err := newTestPS(f).FetchISO(context.Background(), "http://centre/isos/w.iso", `C:\ClusterStorage\DS1\ISOs\w.iso`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "resumed 2 time(s)") {
		t.Fatalf("note should report the resume count, got %q", note)
	}
	if strings.HasPrefix(note, "NOTE=") {
		t.Fatalf("marker prefix should be stripped, got %q", note)
	}
}

// The generated script must keep the properties the job depends on: skip when
// already present, stage through a temp file, treat a locked temp file as a
// concurrent transfer, and stream/resume rather than buffer.
func TestFetchISOScriptShape(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	if _, err := newTestPS(f).FetchISO(context.Background(), "http://centre/isos/w.iso", `C:\ClusterStorage\DS1\ISOs\w.iso`); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected one script call, got %d", len(f.calls))
	}
	script := f.calls[0]
	for _, want := range []string{
		"if (Test-Path $dest) { return }",          // idempotent
		"$tmp=$dest + [char]46 + 'download'",       // staged
		"already in progress on this host",         // locked temp file is explained
		"Move-Item -Force -Path $tmp -Destination", // atomic-ish publish
		"ResponseHeadersRead",                      // streamed, not buffered
		"RangeHeaderValue",                         // resumable
		"[int]$resp.StatusCode -eq 206",            // resume only on a real partial response
		"download failed after",                    // retries are exhausted, then reported
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
	// Start-BitsTransfer cannot work from the agent's Session 0 service context
	// (ERROR_NOT_LOGGED_ON) — reintroducing it silently reinstates the bug.
	if strings.Contains(script, "Start-BitsTransfer") {
		t.Error("Start-BitsTransfer is back; it always fails from a Session 0 service")
	}
	// Invoke-WebRequest buffers and cannot resume, which is what broke multi-GB
	// fetches in the first place.
	if strings.Contains(script, "Invoke-WebRequest") {
		t.Error("Invoke-WebRequest is back; it buffers and cannot resume")
	}
}

// A NIC is only management when it has a static IP AND a default gateway.
// Storage and live-migration NICs are static too, on isolated fabric subnets
// with no gateway — flagging one as management yields a host address (used to
// prefill the agent-update WinRM target) that nothing can reach.
func TestInventoryScriptRequiresGatewayForManagement(t *testing.T) {
	script := inventoryScript()
	if !strings.Contains(script, `$isMgmt = $static -and $gw -ne ''`) {
		t.Error("isManagement must require a default gateway, not a static IP alone")
	}
	if strings.Contains(script, "isManagement = $static;") {
		t.Error("isManagement is still reported straight from $static")
	}
	// The IP must still be reported for a static fabric NIC: callers use
	// "carries an IP" to refuse teaming it into a vSwitch.
	if !strings.Contains(script, "if ($static) { $ip = [string]$a.IPAddress") {
		t.Error("a static fabric NIC must still report its IP so teaming guards fire")
	}
	// The cluster-IP exclusion is load-bearing and must survive edits here.
	if !strings.Contains(script, "$clusterIps -notcontains $_.IPAddress") {
		t.Error("cluster IPs must stay excluded by address")
	}
}

// A local or UNC source is copied, never fetched over HTTP.
func TestFetchISOLocalSourceUsesCopy(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	if _, err := newTestPS(f).FetchISO(context.Background(), `\\fileserver\media\w.iso`, `C:\ClusterStorage\DS1\ISOs\w.iso`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[0], "Copy-Item -LiteralPath $url") {
		t.Errorf("UNC source should branch to Copy-Item:\n%s", f.calls[0])
	}
}
