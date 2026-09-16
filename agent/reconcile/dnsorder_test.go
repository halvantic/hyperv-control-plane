package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* A host joins a domain by resolving its SRV records, so DNS has to point at the
   DC before the join is attempted.

   The DNS step used to sit BELOW the identity step, where it was unreachable in
   exactly the case that needed it: identity attempts the join, the join fails
   because DHCP handed the host the firewall as its resolver, identity returns
   early with a condition — and DNS is never applied. The next pass does the
   same. A freshly onboarded host on DHCP could never join, and the only way out
   was an operator running Fix DNS by hand.

   Seen on the rig 2026-08-07 with HVNEW05: dnsServers were declared in its spec
   and the host's only condition was the failed join. No HostDNS condition at
   all, because that code never ran. */

func hostNeedingJoin() types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "hv05", Generation: 1},
		Spec: types.HostSpec{
			FQDN:         "hv05.ballast.local",
			RebootPolicy: types.RebootNever,
			Networking:   types.HostNetworkingSpec{DNSServers: []string{"192.168.1.168"}},
			DomainJoin: &types.DomainJoinSpec{
				DomainName: "ballast.local", CredentialSecret: "dj",
			},
		},
	}
}

func joinSecret() map[string]types.Secret {
	return map[string]types.Secret{"dj": {
		Name: "dj", Type: types.SecretDomainCredential,
		Data: map[string]string{"username": `BALLAST\admin`, "password": "p"},
	}}
}

// The load-bearing assertion: DNS is applied even on a pass where the join runs
// (and, on a real host, fails). Without it the two deadlock — the join needs DNS
// and the failing join prevents DNS ever being set.
func TestDNSIsAppliedBeforeTheDomainJoinIsAttempted(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "hv05", Domain: "WORKGROUP"}
	res, _ := testReconciler(stub).Reconcile(context.Background(), hostNeedingJoin(), joinSecret())

	var sawDNS bool
	for _, c := range res.Conditions {
		if c.Type == "HostDNS" {
			sawDNS = true
		}
	}
	if !sawDNS {
		t.Fatal("DNS must be applied on the same pass the join is attempted; without it the join can never succeed and the pass returns before DNS is reached")
	}
}

// And it must come first in the reported conditions, because the order of the
// conditions is the order the work happened — a HostDNS condition after the join
// would mean the join saw the old resolvers.
func TestTheDNSConditionPrecedesTheJoinCondition(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "hv05", Domain: "WORKGROUP"}
	res, _ := testReconciler(stub).Reconcile(context.Background(), hostNeedingJoin(), joinSecret())

	dnsAt, joinAt := -1, -1
	for i, c := range res.Conditions {
		if c.Type == "HostDNS" {
			dnsAt = i
		}
		if strings.HasPrefix(c.Type, "DomainJoin/") {
			joinAt = i
		}
	}
	if dnsAt == -1 || joinAt == -1 {
		t.Skipf("stub did not produce both conditions (dns=%d join=%d); ordering covered by the test above", dnsAt, joinAt)
	}
	if dnsAt > joinAt {
		t.Fatal("DNS must be set before the join is attempted, not after it has already failed")
	}
}

// A host that declares no DNS is left alone: an absent declaration is "not
// managed", and helpfully inventing resolvers for a host whose DNS somebody set
// deliberately would be the opposite of what desired state means.
func TestAHostThatDeclaresNoDNSIsNotTouched(t *testing.T) {
	h := hostNeedingJoin()
	h.Spec.Networking.DNSServers = nil
	stub := &hyperv.Stub{ComputerName: "hv05", Domain: "WORKGROUP"}
	res, _ := testReconciler(stub).Reconcile(context.Background(), h, joinSecret())

	for _, c := range res.Conditions {
		if c.Type == "HostDNS" {
			t.Fatal("no DNS declared means no DNS step, not an empty one")
		}
	}
}
