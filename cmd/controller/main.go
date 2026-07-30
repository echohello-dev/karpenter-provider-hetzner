// Command karpenter-provider-hetzner runs Karpenter with the Hetzner Cloud provider.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	cp "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/controllers/nodeclass"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return fmt.Errorf("HCLOUD_TOKEN environment variable is required")
	}
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		return fmt.Errorf("CLUSTER_NAME environment variable is required")
	}
	if err := apiv1.AddToScheme(clientgoscheme.Scheme); err != nil {
		return fmt.Errorf("adding HCloudNodeClass API to scheme: %w", err)
	}

	ctx, op := operator.NewOperator()
	hcloudClient := hcloud.NewClient(hcloud.WithToken(token))
	instanceProvider, err := instance.New(hcloudClient, clusterName)
	if err != nil {
		return fmt.Errorf("constructing instance provider: %w", err)
	}
	cloudProvider := cp.New(
		op.GetClient(),
		instanceProvider,
		instancetype.New(hcloudClient, pricing.New(hcloudClient)),
		imagefamily.New(hcloudClient),
	)

	if err := registerControllers(op.Manager, op.GetClient(), hcloudClient); err != nil {
		return fmt.Errorf("registering controllers: %w", err)
	}
	if err := registerKarpenterCloudProvider(ctx, op, cloudProvider); err != nil {
		return fmt.Errorf("registering cloudprovider with Karpenter: %w", err)
	}
	op.Start(ctx)
	return nil
}

func registerControllers(mgr manager.Manager, kubeClient client.Client, hcloudClient *hcloud.Client) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("hcloudnodeclass").
		For(&apiv1.HCloudNodeClass{}).
		Complete(nodeclass.New(kubeClient, hcloudClient)); err != nil {
		return fmt.Errorf("building HCloudNodeClass controller: %w", err)
	}
	return nil
}

func registerKarpenterCloudProvider(ctx context.Context, op *operator.Operator, cloudProvider *cp.CloudProvider) error {
	decorated := overlay.Decorate(cloudProvider, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), decorated)
	for _, controller := range controllers.NewControllers(
		ctx,
		op.Manager,
		op.Clock,
		op.GetClient(),
		op.EventRecorder,
		decorated,
		cloudProvider,
		clusterState,
		op.InstanceTypeStore,
	) {
		if err := controller.Register(ctx, op.Manager); err != nil {
			return fmt.Errorf("registering %T: %w", controller, err)
		}
	}
	return nil
}
