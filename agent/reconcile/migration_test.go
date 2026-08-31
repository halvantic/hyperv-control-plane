package reconcile

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Reading a job's migration parameters.

   All of these are refusals, and they are refusals for the same reason: every
   one of them has a plausible-looking default that would produce a migration
   which appears to work. Copying a whole disk and calling it a delta is not an
   error anybody notices until the bill for the maintenance window arrives. */

func TestUnreadableChangeMarkersAreRefusedRatherThanTreatedAsNone(t *testing.T) {
	if _, err := parseMarkers(`{"2000": broken`); err == nil {
		t.Fatal("malformed markers were accepted")
	} else if !strings.Contains(err.Error(), "re-copy every disk in full") {
		t.Errorf("the refusal does not say what defaulting would cost: %v", err)
	}
	if _, err := parseMarkers(`{"sda": "52 aa/1"}`); err == nil {
		t.Error("a marker keyed by something that is not a disk key was accepted")
	}
}

// No markers is a base copy and is perfectly normal — it must not be confused
// with markers that could not be read.
func TestNoMarkersIsABaseCopyNotAFailure(t *testing.T) {
	for _, s := range []string{"", "  ", "{}"} {
		m, err := parseMarkers(s)
		if err != nil {
			t.Errorf("parseMarkers(%q) failed: %v", s, err)
		}
		if len(m) != 0 {
			t.Errorf("parseMarkers(%q) produced %v", s, m)
		}
	}
	m, err := parseMarkers(`{"2000":"52 aa/1","2001":"52 aa/2"}`)
	if err != nil {
		t.Fatalf("valid markers failed: %v", err)
	}
	if m[2000] != "52 aa/1" || m[2001] != "52 aa/2" {
		t.Errorf("markers came back as %v", m)
	}
}

/*
A missing credential and a missing address are different problems with

	different fixes, and an operator told to check a hostname that is perfectly
	correct will check it twice before doubting the message.
*/
func TestAMissingCredentialIsNamedSeparatelyFromAMissingAddress(t *testing.T) {
	if _, err := migrationEndpoint(map[string]string{"username": "u", "password": "p"}); err == nil ||
		!strings.Contains(err.Error(), "address") {
		t.Errorf("a job with no address gave: %v", err)
	}
	_, err := migrationEndpoint(map[string]string{"address": "vcsa-02.nuclear.home"})
	if err == nil {
		t.Fatal("a job with no credential was accepted")
	}
	if !strings.Contains(err.Error(), "secret it names has been deleted") {
		t.Errorf("the refusal does not name the likely cause: %v", err)
	}
	if !strings.Contains(err.Error(), "vcsa-02.nuclear.home") {
		t.Errorf("the refusal does not say which source: %v", err)
	}
}

func TestTheEndpointCarriesWhatTheJobSent(t *testing.T) {
	e, err := migrationEndpoint(map[string]string{
		"address": " vcsa-02.nuclear.home ", "username": "svc", "password": "s3cret", "insecure": "true",
	})
	if err != nil {
		t.Fatalf("a complete endpoint was refused: %v", err)
	}
	if e.Address != "vcsa-02.nuclear.home" || e.Username != "svc" || e.Password != "s3cret" || !e.InsecureTLS {
		t.Errorf("the endpoint came out as %+v", e)
	}
	// Absent means verify, not skip. A default of "insecure" would silently
	// accept any certificate on every migration anybody ever ran.
	e2, _ := migrationEndpoint(map[string]string{"address": "a", "username": "u", "password": "p"})
	if e2.InsecureTLS {
		t.Error("certificate verification was skipped by default")
	}
}

/*
Only the LAST pass per migration is kept. A host that has migrated two

	hundred VMs must not report two hundred results on every heartbeat.
*/
func TestOnlyTheLatestPassPerMigrationIsReported(t *testing.T) {
	r := &Reconciler{}
	r.recordMigrationPass(types.MigrationPassResult{Migration: "mig-a", CopiedBytes: 10})
	r.recordMigrationPass(types.MigrationPassResult{Migration: "mig-b", CopiedBytes: 20})
	r.recordMigrationPass(types.MigrationPassResult{Migration: "mig-a", CopiedBytes: 30})

	got := r.MigrationPasses()
	if len(got) != 2 {
		t.Fatalf("%d passes reported, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Migration == "mig-a" && p.CopiedBytes != 30 {
			t.Errorf("mig-a reported the older pass: %+v", p)
		}
	}

	// Cleanup is the last thing every migration does, so it is what stops the
	// host reporting one nothing is working on any more.
	r.forgetMigrationPass("mig-a")
	if got := r.MigrationPasses(); len(got) != 1 || got[0].Migration != "mig-b" {
		t.Errorf("after cleanup the host still reports %+v", got)
	}
	r.forgetMigrationPass("mig-b")
	if got := r.MigrationPasses(); got != nil {
		t.Errorf("with nothing in flight the host reports %+v rather than nothing", got)
	}
}

// The reported slice is a copy. Handing out the live one lets a status report
// being marshalled race with a pass finishing.
func TestTheReportedPassesAreACopy(t *testing.T) {
	r := &Reconciler{}
	r.recordMigrationPass(types.MigrationPassResult{Migration: "mig-a", CopiedBytes: 10})
	got := r.MigrationPasses()
	got[0].CopiedBytes = 999
	if again := r.MigrationPasses(); again[0].CopiedBytes != 10 {
		t.Error("a caller mutating the reported passes changed what the agent holds")
	}
}
