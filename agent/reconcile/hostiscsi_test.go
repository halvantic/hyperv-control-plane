package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* The initiator side was always host-shaped — the service, the portals, the
   logins and MPIO are per-node, and a cluster's spec is that same configuration
   fanned to every member. Only its reachability differed: it could be declared on
   a cluster and nowhere else, so a standalone host had no way to reach an array
   at all, while the console showed shared storage under Storage for clusters and
   nothing equivalent for hosts. */

func standaloneWithISCSI() types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "hv09", Generation: 1},
		Spec: types.HostSpec{
			RebootPolicy: types.RebootNever,
			Storage: types.HostStorageSpec{
				ISCSI: &types.ISCSIStorageSpec{
					Portals: []string{"10.0.60.52"},
					Targets: []string{"iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2"},
				},
			},
		},
	}
}

func TestAStandaloneHostConnectsToItsOwnArray(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "hv09", Domain: "ballast.local"}
	r := testReconciler(stub)

	st, conds := r.reconcileHostISCSI(context.Background(), standaloneWithISCSI(), nil)

	if st == nil {
		t.Fatal("a standalone host that declares an array must report its connection state")
	}
	if st.InitiatorIQN == "" {
		t.Error("the initiator IQN is the first thing needed to grant the LUN; it must be reported")
	}
	if st.Node != "hv09" {
		t.Errorf("the status must name the host it came from, got %q", st.Node)
	}
	var ok bool
	for _, c := range conds {
		if c.Type == "HostISCSI" && c.Status {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("a successful connection must report a passing condition, got %+v", conds)
	}
}

// Two authorities over one initiator is how a node ends up logged in to targets
// nobody declared. Resolving it by precedence would mean an operator's edit
// silently doing nothing, so it is refused with the reason instead.
func TestAClusterMemberIsRefusedItsOwnISCSIConfiguration(t *testing.T) {
	h := standaloneWithISCSI()
	h.Spec.ClusterMembership = &types.ClusterMembershipSpec{ClusterName: "DRCluster"}
	stub := &hyperv.Stub{ComputerName: "hv09"}
	r := testReconciler(stub)

	st, conds := r.reconcileHostISCSI(context.Background(), h, nil)

	if st != nil {
		t.Fatal("a member's iSCSI belongs to the cluster; the host-level one must not be applied")
	}
	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("the refusal must be reported, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "DRCluster") {
		t.Errorf("the message must name the cluster whose configuration wins: %q", conds[0].Message)
	}
}

// A host declaring nothing is left entirely alone — an absent declaration is
// "not managed", never "disconnect from the array".
func TestAHostDeclaringNoArrayIsUntouched(t *testing.T) {
	h := standaloneWithISCSI()
	h.Spec.Storage.ISCSI = nil
	stub := &hyperv.Stub{ComputerName: "hv09"}
	r := testReconciler(stub)

	st, conds := r.reconcileHostISCSI(context.Background(), h, nil)
	if st != nil || len(conds) != 0 {
		t.Fatalf("nothing declared means nothing done, got st=%v conds=%+v", st, conds)
	}
}

func TestAnArrayWithNoPortalIsNamedAsTheProblem(t *testing.T) {
	h := standaloneWithISCSI()
	h.Spec.Storage.ISCSI.Portals = nil
	stub := &hyperv.Stub{ComputerName: "hv09"}
	r := testReconciler(stub)

	_, conds := r.reconcileHostISCSI(context.Background(), h, nil)
	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("a declaration with nothing to connect to must be reported, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "portal") {
		t.Errorf("the message must name what is missing: %q", conds[0].Message)
	}
}

// The CHAP credential is delivered with the pull. When it is missing the message
// must not send the operator to check things that are already true — the agent
// is holding the very spec that names it.
func TestAMissingHostCHAPCredentialBlamesDeliveryNotTheOperator(t *testing.T) {
	h := standaloneWithISCSI()
	h.Spec.Storage.ISCSI.CredentialSecret = "iSCSI Chap Auth"
	stub := &hyperv.Stub{ComputerName: "hv09"}
	r := testReconciler(stub)

	st, conds := r.reconcileHostISCSI(context.Background(), h, map[string]types.Secret{})

	if st != nil {
		t.Fatal("without the credential the node cannot log in, so there is no connection to report")
	}
	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("the absence must be reported, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "centre did not send it") {
		t.Errorf("the message must name where the fault is: %q", conds[0].Message)
	}
}

// CHAP that IS delivered has to reach the host call, or the login fails with an
// authentication error while everything above it looks correct.
func TestADeliveredHostCHAPCredentialIsUsed(t *testing.T) {
	h := standaloneWithISCSI()
	h.Spec.Storage.ISCSI.CredentialSecret = "chap"
	stub := &hyperv.Stub{ComputerName: "hv09"}
	r := testReconciler(stub)

	st, _ := r.reconcileHostISCSI(context.Background(), h, map[string]types.Secret{
		"chap": {Name: "chap", Type: types.SecretCHAPCredential, Data: map[string]string{"username": "u", "password": "p"}},
	})
	if st == nil {
		t.Fatal("with the credential present the connection must be attempted")
	}
}
