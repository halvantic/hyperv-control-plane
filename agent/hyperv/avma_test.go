package hyperv

import (
	"strings"
	"testing"
)

/* AVMA failed on HVNEW06 with the Software Licensing Service's own sentence:

     The Software Licensing Service reported that the product SKU is not found.

   That names neither the key nor what is wrong with it, and sends an operator to
   look at a guest that is fine. Ballast held both halves of the answer. */

func TestTheHostsOwnKeyIsRefusedBeforeTheGuestIsTouched(t *testing.T) {
	s := avmaScript

	if !strings.Contains(s, "$hostProd = @(Get-CimInstance SoftwareLicensingProduct") {
		t.Fatalf("the host's own product key is never read, so it cannot be compared:\n%s", s)
	}
	if !strings.Contains(s, "[string]$hostProd.PartialProductKey -eq $keyTail") {
		t.Fatalf("the supplied key is not compared against the host's own:\n%s", s)
	}
	if !strings.Contains(s, "which is this host''s OWN product key") {
		t.Errorf("the refusal does not say what the key actually is:\n%s", s)
	}
	/* And it says the guest is fine. An operator told only that activation
	   failed goes to the guest, which is the one machine with nothing wrong
	   with it. */
	if !strings.Contains(s, "Nothing about ' + $vm + ' is wrong.") {
		t.Errorf("the refusal does not say the guest is not at fault:\n%s", s)
	}
}

/*
The check runs BEFORE the guest is touched, like the host-edition checks

	above it. Installing a doomed key and then explaining is worse than refusing:
	it leaves a wrong key in the guest's licensing store.
*/
func TestTheKeyCheckRunsBeforeTheGuestIsContacted(t *testing.T) {
	s := avmaScript
	check := strings.Index(s, "$hostProd = @(Get-CimInstance SoftwareLicensingProduct")
	touch := strings.Index(s, "Invoke-Command -VMName $vm")
	if check < 0 || touch < 0 || check > touch {
		t.Fatalf("the key is checked after the guest is contacted:\n%s", s)
	}
}

/*
A key that is not the host's but still does not match the guest gets the SKU

	error. It is caught and translated, because "product SKU is not found" is
	true and unusable: an AVMA key is per RELEASE and per EDITION, and the
	guest's own release is the one fact the message leaves out.
*/
func TestASKUMismatchNamesTheGuestsOwnRelease(t *testing.T) {
	s := avmaScript

	if !strings.Contains(s, "if ($m -match 'product SKU is not found')") {
		t.Fatalf("the SLC message is passed through raw:\n%s", s)
	}
	// The guest is asked what it is, so the message can say.
	if !strings.Contains(s, "Get-CimInstance Win32_OperatingSystem") {
		t.Errorf("the guest's own release is never read, so the error cannot name it:\n%s", s)
	}
	if !strings.Contains(s, "The guest reports itself as ") {
		t.Errorf("the failure does not tell the operator what the guest is running:\n%s", s)
	}
	// Both axes named: a 2022 key fails on a 2025 guest, and a Datacenter key
	// fails on a Standard one. Naming only the release sends half of them wrong.
	for _, want := range []string{"RELEASE and EDITION", "Datacenter key will not activate a Standard"} {
		if !strings.Contains(s, want) {
			t.Errorf("the remedy does not mention %q:\n%s", want, s)
		}
	}
}

// The existing host-side refusals stay: they are about the HOST and must not be
// reached by way of a guest that has already been changed.
func TestTheHostSideRefusalsAreStillFirst(t *testing.T) {
	s := avmaScript
	for _, want := range []string{"an evaluation edition", "AVMA is a Datacenter feature", "this host is not activated"} {
		if !strings.Contains(s, want) {
			t.Errorf("a host-side refusal was lost: %q", want)
		}
	}
}
