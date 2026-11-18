package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

func witnessStub() *hyperv.Stub {
	return &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
}

// A share on a NAS outside the domain cannot be reached as the cluster computer
// object, so the declared credential has to reach Set-ClusterQuorum.
func TestAFileShareWitnessIsAppliedWithItsCredential(t *testing.T) {
	stub := witnessStub()
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\nas01\witness`, CredentialSecret: "nas-witness"}
	secrets := map[string]types.Secret{"nas-witness": {Name: "nas-witness", Data: map[string]string{"username": `nas01\cluster`, "password": "pw"}}}

	if _, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, spec), secrets); err != nil {
		t.Fatal(err)
	}
	if len(stub.WitnessCreds) != 1 || stub.WitnessCreds[0] == nil {
		t.Fatal("the declared credential must be passed to the witness apply")
	}
	if c := stub.WitnessCreds[0]; c.Username != `nas01\cluster` || c.Password != "pw" {
		t.Fatalf("wrong credential passed: %+v", c)
	}
}

// No credential declared: the cluster computer object, exactly as before.
func TestADomainShareWitnessUsesNoCredential(t *testing.T) {
	stub := witnessStub()
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\fs01\witness`}
	if _, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, spec), nil); err != nil {
		t.Fatal(err)
	}
	if len(stub.WitnessCreds) != 1 || stub.WitnessCreds[0] != nil {
		t.Fatal("a witness with no credential declared must be applied as the cluster computer object")
	}
}

// A declared credential that did not arrive must not fall back to the CNO: that
// fails at the share with an access-denied naming the wrong account.
func TestAMissingWitnessCredentialIsReportedNotWorkedAround(t *testing.T) {
	stub := witnessStub()
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\nas01\witness`, CredentialSecret: "nas-witness"}
	res, _ := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, spec), nil)
	if len(stub.WitnessCalls) != 0 {
		t.Fatal("the witness must not be applied without the credential it declares")
	}
	var msg string
	for _, c := range res.Conditions {
		if c.Type == "ClusterWitness" {
			msg = c.Message
		}
	}
	if !strings.Contains(msg, `"nas-witness"`) || !strings.Contains(msg, "did not send") {
		t.Fatalf("the condition must name the credential and say it was not delivered, got %q", msg)
	}
}
