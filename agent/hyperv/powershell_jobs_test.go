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
		"$rt.Wait($idleMs)",                        // the body read has a deadline
		"transfer stalled",                         // and a stall is an error, not a hang
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
	// Stream.CopyTo has no read deadline — HttpClient.Timeout stops covering the
	// body once headers are read, so a half-open connection blocks forever and
	// the resume logic never gets to run. This was observed live: a transfer
	// frozen at 183MB while the server had finished the request 8 minutes prior.
	if strings.Contains(script, "CopyTo(") {
		t.Error("body copy is back on Stream.CopyTo; a stalled connection will hang forever")
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

// The VM an operator most needs to delete is the one deletion refused to
// attempt. A VM whose configuration storage has gone sits in SavedCritical, and
// Stop-VM cannot work because Hyper-V has nothing to read — so under
// $ErrorActionPreference='Stop' its failure aborted the script before Remove-VM
// ever ran.
//
// Observed 2026-08-06: 'Windows Temp Test' on HVNEW03, SavedCritical with
// "Cannot connect to virtual machine configuration storage" after its CSV was
// deleted, and every removal failing on Stop-VM. Remove-VM does not need the
// storage — it removes the REGISTRATION — so stopping must be a courtesy, not a
// precondition.
func TestRemoveVMDoesNotLetAFailedStopAbortTheRemoval(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	if err := newTestPS(f).RemoveVM(context.Background(), "Windows Temp Test"); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]

	// The stop must be inside a catch, or a VM that cannot be stopped can never
	// be removed.
	stop := strings.Index(s, "Stop-VM")
	remove := strings.Index(s, "Remove-VM")
	if stop == -1 || remove == -1 {
		t.Fatal("both the stop and the remove must be present")
	}
	if remove < stop {
		t.Fatal("the removal must follow the stop attempt")
	}
	if !strings.Contains(s, "try { Stop-VM") || !strings.Contains(s, "-ErrorAction Stop } catch {}") {
		t.Error("Stop-VM must be attempted and its failure tolerated, not allowed to abort the script")
	}

	// Judged by outcome, not by whether a cmdlet complained: Remove-VM can emit a
	// trailing error for files it could not tidy while having deregistered the VM,
	// and calling that a failure leaves the operator deleting something already gone.
	if !strings.Contains(s, "$still = Get-VM") {
		t.Error("the removal must be verified by re-reading, not trusted")
	}
	// And when it genuinely did not go, the message must name the cause rather
	// than passing the cmdlet's sentence through.
	if !strings.Contains(s, "configuration storage") {
		t.Error("a VM whose storage is unreachable must have that named as the cause")
	}
}

// RepairHostDNS is offered as "fix DNS on all nodes", and it used to mean that
// literally: every NIC got the DC as its DNS server. An isolated fabric vNIC
// (storage, live migration) must have none — DNS there publishes an unreachable
// A record and makes clustering classify the network as client-facing — and the
// reconcile loop clears it on the very next pass, so the job's own work is undone
// while the operator is told it succeeded.
func TestRepairHostDNSLeavesIsolatedNetworksAlone(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("no change")}}
	if _, err := newTestPS(f).RepairHostDNS(context.Background(), "192.168.1.168"); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]

	if !strings.Contains(s, `$routed = @(Get-NetRoute`) {
		t.Fatal("each NIC's routability must be established before its DNS is touched")
	}
	if !strings.Contains(s, "if (-not $routed) {") {
		t.Fatal("a non-routed NIC needs its own branch, not the DC-DNS treatment")
	}
	if !strings.Contains(s, "-ResetServerAddresses") {
		t.Error("an isolated network's DNS must be cleared, not set")
	}

	// The management NIC is the routed one. "First NIC with a static IP" picks
	// whatever Get-NetAdapter happens to return first, which on a converged host
	// can be the storage vNIC — handing DNS registration to a network that must
	// never publish an A record.
	mgmt := strings.Index(s, "$mgmtIdx = [int]$n.ifIndex")
	if mgmt == -1 {
		t.Fatal("management NIC selection not found")
	}
	if !strings.Contains(s[:mgmt], `DestinationPrefix '0.0.0.0/0'`) {
		t.Error("management NIC selection must require a default route, not a static IP alone")
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
