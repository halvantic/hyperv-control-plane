package hyperv

import (
	"strings"
	"testing"
)

/*
A credential on an ISO library share, and the trap it sets.

	The field existed everywhere and worked nowhere: the console offered it, the
	spec stored it, the proto carried the NAME to the agent — and the centre never
	sent the value, so the probe read the share as its service account exactly as
	if none were declared. Reported as "can't add creds on standalone hosts",
	which was true of clusters too.

	What matters as much as making it work is that it must not read as proof the
	share will boot. A credential authenticates BALLAST'S LISTING. Hyper-V
	attaches media as the node's computer account whatever Ballast authenticates
	as, so a library configured this way can list every image and still fail every
	boot.
*/
func TestTheCredentialAuthenticatesTheListingOnly(t *testing.T) {
	s := isoLibraryScript(`\\nas.lab.local\isos\win`, "LAB\\svc", "pw")

	for _, want := range []string{
		// Mapped for the agent's own read...
		"New-SmbMapping -RemotePath $root -UserName $user -Password $pass",
		// ...against the share ROOT, which is what New-SmbMapping takes.
		"New-SmbMapping wants the SHARE ROOT",
		// ...and taken down again, whatever the read did.
		"Remove-SmbMapping -RemotePath $mapped -Force",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in the probe script", want)
		}
	}

	// The computer-account probe is untouched by it. If a credential ever reached
	// the scheduled task, the probe would stop answering the question it exists
	// for — whether a VM can boot from this share.
	/* The task's command is ONE line — from `$probe = ` to the newline. Slicing
	   to the end of the script caught the credential-aware message further down
	   and failed for the wrong reason: the assertion has to be about what runs as
	   LocalSystem, not about everything printed after it. */
	from := strings.Index(s, "$probe = ")
	task := s[from : from+strings.Index(s[from:], "\n")]
	for _, forbidden := range []string{"$user", "$pass", "New-SmbMapping"} {
		if strings.Contains(task, forbidden) {
			t.Errorf("the computer-account probe references %s; it must run as LocalSystem with no credential", forbidden)
		}
	}
}

// Windows allows one identity per server per session. An existing connection is
// a fact about the session, and an operator told "the password is wrong" would
// go and change a password that was fine.
func TestAnExistingConnectionIsNotABadPassword(t *testing.T) {
	s := isoLibraryScript(`\\nas\isos`, "svc", "pw")
	if !strings.Contains(s, "multiple connections") {
		t.Error("the probe does not recognise Windows' one-identity-per-server refusal")
	}
	if !strings.Contains(s, "allows only one per server") {
		t.Error("the refusal is not explained in terms an operator can act on")
	}
}

// With no credential the script is what it always was: no mapping, no teardown.
func TestNoCredentialMeansNoMapping(t *testing.T) {
	s := isoLibraryScript(`\\nas\isos`, "", "")
	if !strings.Contains(s, "$user = ''") {
		t.Fatalf("expected an empty user in the script")
	}
	// The mapping is guarded on $user, so an empty one never maps. Assert the
	// guard rather than the absence of the call, which is what makes it safe.
	if !strings.Contains(s, "if ($user) {") {
		t.Error("the mapping is not guarded on a credential being present")
	}
}

/*
And the message that stops the credential from being a false comfort.

	A share the credential can list and the computer account cannot is the exact
	shape this feature invites: somebody adds a credential because the listing was
	empty, the listing fills, and every boot still fails.
*/
func TestListingWithACredentialDoesNotImplyItWillBoot(t *testing.T) {
	s := isoLibraryScript(`\\nas\isos`, "svc", "pw")
	if !strings.Contains(s, "A credential only affects this listing") {
		t.Error("the credential path does not warn that the attach is unaffected")
	}
}
