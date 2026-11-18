package reconcile

import (
	"fmt"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// witnessCredential resolves the credential a file share witness declares, from
// the secrets delivered with this pull. Nil with no error when none is declared:
// the share is then reached as the cluster computer object.
//
// A declared credential that did not arrive is an error, not a quiet fallback to
// the CNO. Falling back would fail at the share with an access-denied that names
// the cluster account, and send the operator to fix permissions on a share whose
// real problem is a credential the centre never sent.
func witnessCredential(w types.WitnessSpec, secrets map[string]types.Secret) (*hyperv.ShareCredential, error) {
	if w.Type != types.WitnessFileShare || w.CredentialSecret == "" {
		return nil, nil
	}
	s, ok := secrets[w.CredentialSecret]
	if !ok {
		return nil, fmt.Errorf("the file share witness uses credential %q, but the centre did not send it to this host. If the credential exists in Settings, this is a delivery fault at the centre rather than anything to fix on this host", w.CredentialSecret)
	}
	user, pass := s.Data["username"], s.Data["password"]
	if user == "" || pass == "" {
		return nil, fmt.Errorf("credential %q has no username or password, so the file share witness cannot use it; edit it in Settings", w.CredentialSecret)
	}
	return &hyperv.ShareCredential{Username: user, Password: pass}, nil
}
