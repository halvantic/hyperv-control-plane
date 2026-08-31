package ballastpb

import (
	"reflect"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

// sampleResources populates every observed-resource field, so the round trip
// below can prove the proto carries each one.
func sampleResources() types.HostResources {
	return types.HostResources{
		Switches: []string{"ConvergedSwitch"},
		SwitchDetails: []types.VirtualSwitchInfo{{
			Name:              "ConvergedSwitch",
			NetAdapters:       []string{"Ethernet0", "Ethernet1"},
			AllowManagementOS: true,
			VLANID:            10,
		}},
		/* Both kinds, because a cluster member reports both and only Shared
		   tells them apart. A clustered VM placed on a local volume cannot fail
		   over anywhere, so a wire that dropped the flag would have every volume
		   read as ordinary storage. */
		Volumes: []types.StorageVolume{{
			Name: "Volume1", Path: `C:\ClusterStorage\Volume1`, SizeBytes: 1 << 40, UsedBytes: 1 << 39, Shared: true,
		}, {
			Name: "I:", Path: `I:\`, SizeBytes: 2 << 40, UsedBytes: 512 << 30,
		}},
		ISOs: []string{`C:\ClusterStorage\Volume1\ISOs\w2025.iso`},
		ManagementVNICs: []types.ManagementVNICInfo{{
			Name:       "Management",
			SwitchName: "ConvergedSwitch",
			VlanID:     10,
			DNSServers: []string{"192.168.1.168"},
			Profile:    "DomainAuthenticated",
			Addresses:  []types.VNICAddress{{Address: "192.168.1.74/24", Kind: "host"}},
			Gateway:    "192.168.1.1",
		}},
	}
}

func TestHostResourcesRoundTrip(t *testing.T) {
	in := types.HostStatus{Resources: sampleResources()}
	got := StatusFromProto(StatusToProto(in))
	if !reflect.DeepEqual(in.Resources, got.Resources) {
		t.Fatalf("round trip mismatch:\n in:  %#v\n got: %#v", in.Resources, got.Resources)
	}
}

// The same guard the host and cluster status already have, for the observed
// resources. ManagementVNICInfo.Gateway is why it is here: the vNIC set was
// reported without a default gateway, so nothing reconstructing a spec from what
// a host reports could tell a routable management vNIC from an isolated fabric
// one — and writing a management vNIC with no gateway is what takes a member off
// its default route. A field the proto does not carry round-trips perfectly as
// its zero value, so only this notices.
func TestSampleResourcesCoversEveryField(t *testing.T) {
	r := sampleResources()

	check := func(name string, v reflect.Value) {
		t.Helper()
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if v.Field(i).IsZero() {
				t.Errorf("sampleResources does not set %s.%s, so the round trip cannot prove the proto carries it — populate it", name, f.Name)
			}
		}
	}
	check("HostResources", reflect.ValueOf(r))
	check("VirtualSwitchInfo", reflect.ValueOf(r.SwitchDetails[0]))
	check("StorageVolume", reflect.ValueOf(r.Volumes[0]))
	check("ManagementVNICInfo", reflect.ValueOf(r.ManagementVNICs[0]))
	check("VNICAddress", reflect.ValueOf(r.ManagementVNICs[0].Addresses[0]))
}
