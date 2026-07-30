package nodeclass

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// reconcileNetwork verifies that the NodeClass's NetworkID still resolves in
// hcloud. A missing network makes the NodeClass unusable for any server that
// needs to attach to it, so the condition stays False until the spec is
// fixed or the network is recreated.
func (r *Reconciler) reconcileNetwork(ctx context.Context, nc *apiv1.HCloudNodeClass) {
	cs := nc.StatusConditions()

	if nc.Spec.NetworkID == 0 {
		cs.SetFalse(
			apiv1.ConditionTypeNetworkReady,
			"NetworkIDMissing",
			"spec.networkID is required",
		)
		return
	}

	network, _, err := r.hcloud.Network.GetByID(ctx, nc.Spec.NetworkID)
	if err != nil {
		cs.SetFalse(
			apiv1.ConditionTypeNetworkReady,
			"NetworkLookupFailed",
			fmt.Sprintf("looking up network %d: %v", nc.Spec.NetworkID, err),
		)
		return
	}
	if network == nil {
		cs.SetFalse(
			apiv1.ConditionTypeNetworkReady,
			"NetworkNotFound",
			fmt.Sprintf("network %d does not exist", nc.Spec.NetworkID),
		)
		return
	}
	cs.SetTrueWithReason(apiv1.ConditionTypeNetworkReady, "NetworkResolved", fmt.Sprintf("network %d (%s) exists", network.ID, network.Name))
}

// reconcileResources validates the NodeClass's optional resource IDs
// (firewalls, SSH keys) and its Locations list against the live hcloud API.
// Any single failure rolls ResourcesReady to False with a reason naming the
// offending class so the operator can see at a glance which dependency is
// missing. Locations are validated in bulk (one All() call) to keep the
// reconcile cheap; firewalls and SSH keys are looked up individually because
// their counts are usually small.
func (r *Reconciler) reconcileResources(ctx context.Context, nc *apiv1.HCloudNodeClass) {
	cs := nc.StatusConditions()
	if len(nc.Spec.Locations) == 0 {
		cs.SetFalse(apiv1.ConditionTypeResourcesReady, "LocationsMissing", "spec.locations must contain at least one location")
		return
	}

	if missing := r.validateFirewallIDs(ctx, nc.Spec.FirewallIDs); len(missing) > 0 {
		cs.SetFalse(
			apiv1.ConditionTypeResourcesReady,
			"FirewallsNotFound",
			fmt.Sprintf("firewalls not found: %v", missing),
		)
		return
	}
	if missing := r.validateSSHKeyIDs(ctx, nc.Spec.SSHKeyIDs); len(missing) > 0 {
		cs.SetFalse(
			apiv1.ConditionTypeResourcesReady,
			"SSHKeysNotFound",
			fmt.Sprintf("ssh keys not found: %v", missing),
		)
		return
	}
	if missing := r.validateLocations(ctx, nc.Spec.Locations); len(missing) > 0 {
		cs.SetFalse(
			apiv1.ConditionTypeResourcesReady,
			"LocationsNotFound",
			fmt.Sprintf("locations not found: %v", missing),
		)
		return
	}
	cs.SetTrueWithReason(apiv1.ConditionTypeResourcesReady, "ResourcesResolved", "firewalls, ssh keys, and locations verified")
}

// validateFirewallIDs returns the IDs that could not be fetched. An empty
// input returns nil with no API calls.
func (r *Reconciler) validateFirewallIDs(ctx context.Context, ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	var missing []int64
	for _, id := range ids {
		fw, _, err := r.hcloud.Firewall.GetByID(ctx, id)
		if err != nil || fw == nil {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing
}

// validateSSHKeyIDs returns the IDs that could not be fetched.
func (r *Reconciler) validateSSHKeyIDs(ctx context.Context, ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	var missing []int64
	for _, id := range ids {
		key, _, err := r.hcloud.SSHKey.GetByID(ctx, id)
		if err != nil || key == nil {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing
}

// validateLocations fetches the full hcloud location list once and reports
// any spec.Locations entries that are not present. Locations rarely change,
// so the call is cheap relative to per-ID lookups.
func (r *Reconciler) validateLocations(ctx context.Context, locations []string) []string {
	if len(locations) == 0 {
		return nil
	}
	all, err := r.hcloud.Location.All(ctx)
	if err != nil {
		// On API failure report every spec location as missing — the
		// operator needs to know we could not validate them.
		missing := make([]string, len(locations))
		copy(missing, locations)
		sort.Strings(missing)
		return missing
	}
	known := make(map[string]struct{}, len(all))
	for _, loc := range all {
		if loc != nil {
			known[loc.Name] = struct{}{}
		}
	}
	var missing []string
	for _, name := range locations {
		if _, ok := known[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// reconcileUserData validates that the NodeClass has a usable user-data
// source: either a referenced Secret that exists with the named key and a
// non-empty value, or — when no Secret reference is set — an inline
// Spec.UserData (empty inline is permitted by the API).
//
// The Secret payload is intentionally not copied into the NodeClass status;
// HCloudNodeClassStatus has no userData field, so this is enforced by the
// schema. Callers must re-fetch the Secret at server-create time using the
// spec reference.
func (r *Reconciler) reconcileUserData(ctx context.Context, nc *apiv1.HCloudNodeClass) {
	cs := nc.StatusConditions()

	ref := nc.Spec.UserDataSecretRef
	if ref != nil {
		reason, msg := validateUserDataSecretRef(ctx, r.kubeClient, ref)
		if reason != "" {
			cs.SetFalse(apiv1.ConditionTypeUserDataReady, reason, msg)
			return
		}
		cs.SetTrueWithReason(apiv1.ConditionTypeUserDataReady, "UserDataFromSecret", "user data sourced from referenced Secret (content not stored on NodeClass)")
		return
	}

	// No Secret reference: inline Spec.UserData is the source. The API
	// declares it optional so empty inline is permitted.
	cs.SetTrueWithReason(apiv1.ConditionTypeUserDataReady, "UserDataInline", "user data sourced from spec.userData")
}

// validateUserDataSecretRef returns ("", "") when the referenced Secret
// resolves successfully and the named key has a non-empty value. Otherwise
// it returns the condition reason and message describing the failure.
func validateUserDataSecretRef(ctx context.Context, kubeClient client.Reader, ref *apiv1.UserDataSecretRef) (string, string) {
	if ref.Namespace == "" || ref.Name == "" || ref.Key == "" {
		return "UserDataSecretRefIncomplete",
			"spec.userDataSecretRef requires namespace, name, and key"
	}
	secret := &corev1.Secret{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return "UserDataSecretNotFound",
			fmt.Sprintf("fetching Secret %s/%s: %v", ref.Namespace, ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "UserDataSecretKeyMissing",
			fmt.Sprintf("Secret %s/%s has no key %q", ref.Namespace, ref.Name, ref.Key)
	}
	if len(value) == 0 {
		return "UserDataSecretValueEmpty",
			fmt.Sprintf("Secret %s/%s key %q is empty", ref.Namespace, ref.Name, ref.Key)
	}
	return "", ""
}
