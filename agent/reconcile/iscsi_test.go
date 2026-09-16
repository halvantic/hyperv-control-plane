package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

func iscsiAssignment(former bool, cfg *types.ISCSIStorageSpec) ClusterAssignment {
	return ClusterAssignment{
		IsMember: true, IsFormer: former,
		Cluster: types.Cluster{
			Meta: types.ObjectMeta{Name: "c1"},
			Spec: types.ClusterSpec{
				Members: []string{"n1", "n2"},
				Storage: &types.ClusterStorageSpec{Kind: types.StorageKindISCSI, ISCSI: cfg},
			},
		},
	}
}

// Unlike every other cluster-wide setting, a login is per-NODE: there is no
// shared object for members to race over, and gating it on the former would
// leave the other members unable to see the storage at all.
func TestEveryMemberConnectsToTheArrayNotJustTheFormer(t *testing.T) {
	cfg := &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}, Targets: []string{"iqn.test:t1"}}
	for _, former := range []bool{true, false} {
		stub := &hyperv.Stub{}
		st, conds, _ := testReconciler(stub).reconcileISCSI(context.Background(), iscsiAssignment(former, cfg), nil)
		if st == nil {
			t.Fatalf("former=%v: a member must connect and report", former)
		}
		if len(conds) == 0 {
			t.Errorf("former=%v: the attempt must be reported as a condition", former)
		}
	}
}

// An S2D cluster reports NOTHING rather than an empty iSCSI state. "Not
// applicable" and "connected to nothing" must not look the same.
func TestAnS2DClusterReportsNoISCSIState(t *testing.T) {
	a := iscsiAssignment(true, &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}})
	a.Cluster.Spec.Storage = &types.ClusterStorageSpec{Kind: types.StorageKindS2D}
	st, conds, changed := testReconciler(&hyperv.Stub{}).reconcileISCSI(context.Background(), a, nil)
	if st != nil || len(conds) != 0 || changed {
		t.Fatalf("an S2D cluster must not report iSCSI: st=%v conds=%d changed=%v", st, len(conds), changed)
	}
}

// Declared iSCSI-backed with no portal is a spec that cannot possibly work, and
// it must say so rather than quietly connecting to nothing.
func TestNoPortalIsReportedAsAFault(t *testing.T) {
	st, conds, _ := testReconciler(&hyperv.Stub{}).reconcileISCSI(context.Background(),
		iscsiAssignment(true, &types.ISCSIStorageSpec{}), nil)
	if st != nil {
		t.Error("nothing was connected, so nothing should be reported as state")
	}
	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("a portal-less iSCSI cluster must fail its condition, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "portal") {
		t.Errorf("the message must name what is missing, got %q", conds[0].Message)
	}
}

// A CHAP credential the centre never delivered cannot be guessed at. Attempting
// the login anyway would fail on the array with an authentication error, sending
// the operator to check the array's CHAP settings rather than the credential
// that never arrived.
func TestAMissingCHAPCredentialNamesItself(t *testing.T) {
	cfg := &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}, CredentialSecret: "nas-chap"}
	_, conds, _ := testReconciler(&hyperv.Stub{}).reconcileISCSI(context.Background(),
		iscsiAssignment(true, cfg), map[string]types.Secret{})
	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("a missing credential must fail the condition, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "nas-chap") {
		t.Errorf("the message must name the credential, got %q", conds[0].Message)
	}
}

// A delivered credential is used and not stored — the reconcile takes it from
// the pull each pass, like every other secret.
func TestADeliveredCHAPCredentialIsUsed(t *testing.T) {
	cfg := &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}, CredentialSecret: "nas-chap"}
	secrets := map[string]types.Secret{
		"nas-chap": {Name: "nas-chap", Data: map[string]string{"username": "hv", "password": "s3cret"}},
	}
	st, conds, _ := testReconciler(&hyperv.Stub{}).reconcileISCSI(context.Background(),
		iscsiAssignment(true, cfg), secrets)
	if st == nil {
		t.Fatal("with the credential delivered the login must be attempted")
	}
	if len(conds) == 0 || !conds[0].Status {
		t.Fatalf("want a healthy condition, got %+v", conds)
	}
}

// A target discovered but not logged in has a different remedy from an
// unreachable array, and both otherwise present as simply having no disks. The
// message names the initiator IQN because that is what the array's allowed list
// has to contain.
func TestADiscoveredButUnconnectedTargetNamesTheInitiator(t *testing.T) {
	stub := &hyperv.Stub{ISCSIUnconnected: []string{"iqn.test:t1"}}
	_, conds, _ := testReconciler(stub).reconcileISCSI(context.Background(),
		iscsiAssignment(true, &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}, Targets: []string{"iqn.test:t1"}}), nil)

	var found bool
	for _, c := range conds {
		if c.Reason == "TargetNotConnected" {
			found = true
			if !strings.Contains(c.Message, "iqn.1991-05.com.microsoft") {
				t.Errorf("the remedy needs the initiator IQN in it, got %q", c.Message)
			}
		}
	}
	if !found {
		t.Fatalf("a discovered-but-not-connected target must be reported, got %+v", conds)
	}
}
