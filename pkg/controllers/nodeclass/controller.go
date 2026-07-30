// Package nodeclass reconciles HCloudNodeClass objects and aggregates the
// derived state (resolved images, network existence, resource existence)
// into a Ready condition so Karpenter's drift detection has a stable view of
// "is this NodeClass usable right now".
package nodeclass

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// requeueInterval is the cadence at which the controller re-evaluates a
// NodeClass that is already in a stable Ready state. Karpenter only invokes
// the controller on object changes; this fallback exists so a NodeClass that
// flips Ready=True still gets re-checked before long in case an upstream
// resource (image, network, firewall, ssh key, secret) is deleted behind the
// scenes.
const requeueInterval = 5 * time.Minute

// Reconciler is the controller-runtime Reconciler for HCloudNodeClass.
type Reconciler struct {
	kubeClient client.Client
	hcloud     *hcloud.Client
}

// New builds a Reconciler. kubeClient must be a cache-backed reader for the
// types this controller watches, and hcloud must be a configured client.
func New(kubeClient client.Client, hcloud *hcloud.Client) *Reconciler {
	return &Reconciler{kubeClient: kubeClient, hcloud: hcloud}
}

// Reconcile re-evaluates the HCloudNodeClass status.
//
// Behaviour:
//
//   - NotFound: returns immediately without requeue; the object is gone.
//   - DeletionTimestamp set: marks Ready=False/Deleted and returns without
//     requeue; in-flight Karpenter operations should refuse to provision
//     against a NodeClass that is going away.
//   - Otherwise: runs the image, network, resource, and user-data sub-checks,
//     aggregates the resulting dependent conditions into Ready via the
//     operatorpkg ConditionSet, updates ResolvedImages, and patches status.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("name", req.Name)
	logger.V(1).Info("reconcile hcloudnodeclass")

	nc := &apiv1.HCloudNodeClass{}
	if err := r.kubeClient.Get(ctx, req.NamespacedName, nc); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("getting hcloudnodeclass: %w", err)
		}
		return reconcile.Result{}, nil
	}

	// Snapshot the freshly-loaded object before any in-memory mutations so
	// the merge patch always has a non-empty diff. Patches against an empty
	// diff are correctly accepted as no-ops by the real apiserver but are
	// observed to drop the in-memory status in the controller-runtime fake
	// client, so capturing original before mutation is the safe pattern.
	original := nc.DeepCopy()

	if !nc.DeletionTimestamp.IsZero() {
		nc.StatusConditions().SetFalse(status.ConditionReady, "Deleted", "HCloudNodeClass is being deleted")
		if err := r.patchStatus(ctx, original, nc); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// Each sub-check writes its own dependent condition; the ConditionSet
	// recomputes the aggregate Ready condition after every Set so we do not
	// have to wire that aggregation here.
	r.reconcileImages(ctx, nc)
	r.reconcileNetwork(ctx, nc)
	r.reconcileResources(ctx, nc)
	r.reconcileUserData(ctx, nc)

	if err := r.patchStatus(ctx, original, nc); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: requeueInterval}, nil
}

// patchStatus applies the in-memory status changes to the API server via a
// merge patch computed against the pre-update snapshot. original must be a
// deep copy of the NodeClass as it was returned by Get, before any local
// modifications were made; using a post-modification snapshot produces an
// empty patch and is observed to clobber status in the fake client.
func (r *Reconciler) patchStatus(ctx context.Context, original, nc *apiv1.HCloudNodeClass) error {
	if equality.Semantic.DeepEqual(original.Status, nc.Status) {
		return nil
	}
	if err := r.kubeClient.Status().Patch(ctx, nc, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching hcloudnodeclass status: %w", err)
	}
	return nil
}

// archLabel is the ResolvedImage.Architecture string for a given hcloud
// architecture. Kept private to the package since the value space ("amd64",
// "arm64") is part of the CRD's public schema and changes would be breaking.
func archLabel(a hcloud.Architecture) string {
	if a == hcloud.ArchitectureARM {
		return "arm64"
	}
	return "amd64"
}
