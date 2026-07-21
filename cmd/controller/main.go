// Command karpenter-provider-hetzner is the controller binary that runs the
// Hetzner Cloud implementation of karpcp.CloudProvider and reconciles
// HCloudNodeClass CRDs.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	cp "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/controllers/nodeclass"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

const (
	metricsPort      = 8080
	healthPort       = 8081
	leaderElectionID = "karpenter-provider-hetzner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	logger := zap.New(zap.UseDevMode(true))
	log.SetLogger(logger)

	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return fmt.Errorf("HCLOUD_TOKEN environment variable is required")
	}
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		return fmt.Errorf("CLUSTER_NAME environment variable is required (would otherwise create servers unscoped across clusters)")
	}

	hcloudClient := hcloud.NewClient(hcloud.WithToken(token))

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("building in-cluster REST config: %w", err)
	}

	kubeClient, err := client.New(config, client.Options{Scheme: scheme()})
	if err != nil {
		return fmt.Errorf("building controller-runtime client: %w", err)
	}

	// Sub-providers. Each is the smallest unit the cloudprovider needs to
	// do its work; keeping them separate makes them independently testable.
	instanceProvider, err := instance.New(hcloudClient, clusterName)
	if err != nil {
		return fmt.Errorf("constructing instance provider: %w", err)
	}
	typeProvider := instancetype.New(hcloudClient, pricing.New(hcloudClient))
	imageProvider := imagefamily.New(hcloudClient)
	cloudProv := cp.New(kubeClient, instanceProvider, typeProvider, imageProvider)

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme(),
		HealthProbeBindAddress: fmt.Sprintf(":%d", healthPort),
		Metrics:                metricsserver.Options{BindAddress: fmt.Sprintf(":%d", metricsPort)},
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 9443}),
		LeaderElection:         true,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		return fmt.Errorf("constructing manager: %w", err)
	}

	if err := apiv1.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("adding HCloudNodeClass API to scheme: %w", err)
	}

	if err := registerControllers(mgr, kubeClient, hcloudClient, cloudProv); err != nil {
		return fmt.Errorf("registering controllers: %w", err)
	}

	if err := registerKarpenterCloudProvider(mgr, cloudProv); err != nil {
		return fmt.Errorf("registering cloudprovider with Karpenter: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	logger.Info("starting manager")
	return mgr.Start(ctx)
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("adding corev1 to scheme: %v", err))
	}
	if err := apiv1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("adding apiv1 to scheme: %v", err))
	}
	return s
}

// registerControllers wires HCloudNodeClass reconciliation into mgr.
//
// TODO: also wire the Karpenter KarpenterNodePool / NodeClaim controllers
// when they ship as standalone objects — in upstream karpenter v1 the
// controllers live in `sigs.k8s.io/karpenter/pkg/controllers` and the
// cloudprovider is registered via `cloudprovider.NewCloudProvider`.
func registerControllers(mgr manager.Manager, kubeClient client.Client, hcloudClient *hcloud.Client, _ *cp.CloudProvider) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("hcloudnodeclass").
		For(&apiv1.HCloudNodeClass{}).
		Complete(nodeclass.New(kubeClient, hcloudClient)); err != nil {
		return fmt.Errorf("building HCloudNodeClass controller: %w", err)
	}
	return nil
}

// registerKarpenterCloudProvider finalizes the integration with the
// upstream sigs.k8s.io/karpenter runtime. In karpenter v1 this is wired via
// `sigs.k8s.io/controller-runtime/pkg/manager.Runnable` plus a
// `sigs.k8s.io/karpenter/pkg/controllers` registration hook — the exact
// composition is the TODO once that wiring is stable.
func registerKarpenterCloudProvider(mgr manager.Manager, _ *cp.CloudProvider) error {
	runnable := manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	return mgr.Add(runnable)
}
