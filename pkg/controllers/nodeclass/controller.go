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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

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
// TODO: implement. Plan:
//  1. Fetch the HCloudNodeClass (Found/notFound/noChange).
//  2. List images matching the spec.ImageSelector; record ResolvedImages
//     keyed by arch.
//  3. Verify the spec.NetworkID exists in hcloud (Network.GetByID).
//  4. Verify each spec.FirewallIDs + spec.SSHKeyIDs exist (split ready/failed).
//  5. Patch status with new conditions + ResolvedImages.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log.FromContext(ctx).Info("reconcile hcloudnodeclass", "name", req.Name)

	if err := r.fetchAndMarkNotFound(ctx, req.NamespacedName); err != nil {
		return reconcile.Result{}, fmt.Errorf("checking hcloudnodeclass: %w", err)
	}

	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

// fetchAndMarkNotFound clears the Ready condition when the NodeClass has
// been deleted so any in-flight Karpenter operations can also see the
// deletion and refuse to provision against it.
func (r *Reconciler) fetchAndMarkNotFound(ctx context.Context, name types.NamespacedName) error {
	nc := &apiv1.HCloudNodeClass{}
	if err := r.kubeClient.Get(ctx, name, nc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		nc.StatusConditions().SetFalse(status.ConditionReady, "Deleted", "HCloudNodeClass is being deleted")
		return r.kubeClient.Status().Update(ctx, nc)
	}
	return nil
}
