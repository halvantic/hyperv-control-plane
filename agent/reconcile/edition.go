package reconcile

import (
	"context"
	"fmt"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// reconcileWindowsEdition converts the host to its declared Windows edition.
//
// It runs BEFORE the Hyper-V role and everything after it, and returns done ==
// true when a restart is owed, because a staged conversion changes the operating
// system on the next boot: configuring a host and then converting the edition
// underneath it means reconciling against something that is about to be replaced.
//
// IRREVERSIBLE, so an absent declaration converts nothing. Nil means "Ballast does
// not manage the edition", never "keep the current one" — the difference matters
// because there is no way back from a conversion, and a schema that treats
// silence as consent would convert a host nobody meant to touch.
func (r *Reconciler) reconcileWindowsEdition(ctx context.Context, desired types.Host, secrets map[string]types.Secret) (Result, bool, error) {
	spec := desired.Spec.WindowsLicence
	if spec == nil || spec.Edition == "" {
		return Result{}, false, nil
	}

	var key string
	if spec.ProductKeySecret != "" {
		s, ok := secrets[spec.ProductKeySecret]
		if !ok {
			// Not an error the host can fix, and not a reason to degrade it: the
			// host is running perfectly on the edition it has.
			return Result{Conditions: []types.Condition{{
				Type: "WindowsEdition", Status: false, Reason: "NotApplied",
				Message:            fmt.Sprintf("this host declares edition %s, but the product key %q was not sent to it. If the key exists in Settings this is a delivery fault at the centre rather than anything to fix here.", spec.Edition, spec.ProductKeySecret),
				LastTransitionTime: r.now(),
			}}}, false, nil
		}
		key = s.Data["productKey"]
		if key == "" {
			key = s.Data["password"] // tolerated: an operator storing it as a credential
		}
	}

	out, rebootRequired, err := r.hv.EnsureWindowsEdition(ctx, spec.Edition, key)
	if err != nil {
		// Advisory. A conversion that cannot happen leaves the host exactly as it
		// was and working; degrading the whole host for it would hold back every
		// other piece of desired state for something that is not broken.
		r.log.Warn("windows edition conversion failed (advisory, retries next pass)", "err", err)
		return Result{Conditions: []types.Condition{
			r.advisoryCondition("WindowsEdition", hyperv.OutcomeUnchanged, err),
		}}, false, nil
	}
	if out == hyperv.OutcomeUnchanged {
		return Result{Conditions: []types.Condition{
			r.condition("WindowsEdition", out, nil),
		}}, false, nil
	}

	conds := []types.Condition{{
		Type: "WindowsEdition", Status: true, Reason: "Updated",
		Message:            fmt.Sprintf("converted to %s; the change takes effect on restart", spec.Edition),
		LastTransitionTime: r.now(),
	}}
	r.log.Info("windows edition conversion staged", "edition", spec.Edition)
	if !rebootRequired {
		return Result{Conditions: conds, Changed: true}, false, nil
	}
	// Governed by RebootPolicy, exactly as the domain join and the role install
	// are. A conversion is not more sensitive than those — it is the same kind of
	// pending change — and giving it its own rule would mean an operator's policy
	// meant something different here than everywhere else.
	return r.rebootResult(ctx, desired.Spec.RebootPolicy, conds, "convert windows edition")
}

// reconcileWindowsActivation activates the host by its declared method.
//
// Runs AFTER the edition step and only when that step did not stop the pass: a
// host with a conversion staged is about to become a different edition, and
// activating the one it is leaving would consume a MAK seat for an installation
// that ceases to exist at the next restart.
//
// Advisory throughout. An unactivated host runs, serves VMs and reconciles
// everything else perfectly — Windows nags and eventually restricts, but that is
// not a reason to hold back a host's entire desired state.
func (r *Reconciler) reconcileWindowsActivation(ctx context.Context, desired types.Host, secrets map[string]types.Secret) []types.Condition {
	spec := desired.Spec.WindowsLicence
	if spec == nil || spec.Activation == "" {
		return nil
	}

	var key string
	if spec.ActivationKeySecret != "" {
		s, ok := secrets[spec.ActivationKeySecret]
		if !ok {
			return []types.Condition{{
				Type: "WindowsActivation", Status: false, Reason: "NotApplied",
				Message:            fmt.Sprintf("this host activates by %s with key %q, but that key was not sent to it. If it exists in Settings this is a delivery fault at the centre rather than anything to fix here.", spec.Activation, spec.ActivationKeySecret),
				LastTransitionTime: r.now(),
			}}
		}
		key = s.Data["productKey"]
		if key == "" {
			key = s.Data["password"]
		}
	}

	out, err := r.hv.EnsureWindowsActivation(ctx, spec.Activation, key, spec.KMSServer)
	if err != nil {
		r.log.Warn("windows activation failed (advisory, retries next pass)", "err", err)
		return []types.Condition{r.advisoryCondition("WindowsActivation", hyperv.OutcomeUnchanged, err)}
	}
	if out != hyperv.OutcomeUnchanged {
		r.log.Info("windows activated", "method", spec.Activation)
	}
	return []types.Condition{r.advisoryCondition("WindowsActivation", out, nil)}
}
