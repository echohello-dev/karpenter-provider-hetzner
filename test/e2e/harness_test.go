package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/controllers/nodeclass"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/test/e2e/fakehcloud"
)

const (
	// clusterName scopes the fake fleet the way CLUSTER_NAME does in
	// production (every managed server carries karpenter.sh/cluster=<name>).
	clusterName = "e2e-cluster"

	// Shared catalog fixtures the fake hcloud backend and the NodeClass
	// manifests agree on.
	networkID  = 12345
	firewallID = 9001
	sshKeyID   = 7001

	// reconcileTimeout bounds how long the scenario waits for the
	// nodeclass controller to converge status through the real manager.
	reconcileTimeout = 60 * time.Second
)

// harnessSeq disambiguates controller names across harnesses in one test
// process (controller-runtime requires process-unique controller names).
var harnessSeq atomic.Int64

// harness wires one scenario: a stateful fake hcloud backend, a real
// kube-apiserver (envtest) with the shipped CRDs, the real HCloudNodeClass
// reconciler running under a controller-runtime manager, and the real
// CloudProvider. Only the Hetzner Cloud API side is faked.
type harness struct {
	kube   client.Client // uncached client for test reads/writes
	hcloud *hcloud.Client
	fake   *fakehcloud.Backend
	cp     *cloudprovider.CloudProvider
}

// startHarness builds the full stack. It skips (never fails) when the
// envtest binaries are not installed, so the suite degrades cleanly on
// machines that only run `mise run test`.
func startHarness(t *testing.T) *harness {
	t.Helper()
	requireEnvtest(t)

	fake := fakehcloud.New(defaultCatalog())
	t.Cleanup(fake.Close)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDirectory(t)},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest control plane: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest control plane: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apiv1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building direct kube client: %v", err)
	}

	hcloudClient := fake.Client()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// No metrics port: tests run many managers per machine and a
		// fixed bind address would collide.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("building manager: %v", err)
	}
	// Same registration shape as cmd/controller/main.go: the e2e path
	// exercises the production controller wiring, not a test double. The
	// name gets a per-harness suffix because controller-runtime requires
	// controller names to be unique within a process and each test starts
	// its own manager.
	ctrlName := fmt.Sprintf("hcloudnodeclass-%d", harnessSeq.Add(1))
	if err := ctrl.NewControllerManagedBy(mgr).
		Named(ctrlName).
		For(&apiv1.HCloudNodeClass{}).
		Complete(nodeclass.New(mgr.GetClient(), hcloudClient)); err != nil {
		t.Fatalf("registering nodeclass controller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		// Errors surface as reconcile-timeout failures in the tests;
		// the goroutine must not call t.Fatal after the test ends.
		_ = mgr.Start(ctx)
	}()

	inst, err := instance.New(hcloudClient, clusterName)
	if err != nil {
		t.Fatalf("building instance provider: %v", err)
	}
	cp := cloudprovider.New(
		kube,
		inst,
		instancetype.New(hcloudClient, pricing.New(hcloudClient)),
		imagefamily.New(hcloudClient),
	)

	return &harness{kube: kube, hcloud: hcloudClient, fake: fake, cp: cp}
}

// requireEnvtest skips the test unless envtest binaries are available via
// KUBEBUILDER_ASSETS. `mise run e2e` downloads them through setup-envtest and
// exports the variable; without it the suite must skip, not fail.
func requireEnvtest(t *testing.T) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `mise run e2e` (downloads envtest binaries via setup-envtest) or export KUBEBUILDER_ASSETS yourself")
	}
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil { //nolint:gosec // G703: KUBEBUILDER_ASSETS is developer/mise-controlled, not user input
		t.Skipf("KUBEBUILDER_ASSETS=%q has no kube-apiserver binary: %v", assets, err)
	}
}

// crdDirectory returns the chart CRD directory the envtest control plane
// installs. Resolved relative to the test working directory (go test runs
// with cwd set to the package directory).
func crdDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "charts", "karpenter-provider-hetzner", "crds")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("resolving CRD directory %s: %v", dir, err)
	}
	return dir
}

// defaultCatalog is the shared fake Hetzner project: the same fixtures the
// unit tests use (so reconcile expectations are comparable) plus the pricing
// and server-type catalog the CloudProvider's create path needs.
func defaultCatalog() fakehcloud.Config {
	return fakehcloud.Config{
		Images: []fakehcloud.Image{
			{ID: 11, Description: "talos v1.9.5", Architecture: "x86", Type: "snapshot", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: 12, Description: "talos v1.9.6", Architecture: "x86", Type: "snapshot", Created: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
			{ID: 13, Description: "talos v1.9.5", Architecture: "arm", Type: "snapshot", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: 99, Description: "ubuntu-22.04", Architecture: "x86", Type: "snapshot", Created: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},
		},
		ServerTypes: []fakehcloud.ServerType{
			{Name: "cx22", Architecture: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, HourlyNet: "0.01", Locations: map[string]bool{"fsn1": true, "hel1": true}},
			{Name: "cpx21", Architecture: "x86", Cores: 4, MemoryGB: 8, DiskGB: 80, HourlyNet: "0.02", Locations: map[string]bool{"fsn1": true}},
		},
		Locations: []fakehcloud.Location{
			{ID: 1, Name: "fsn1", NetworkZone: "eu-central"},
			{ID: 2, Name: "hel1", NetworkZone: "eu-central"},
		},
		Networks:  map[int64]string{networkID: "cluster-net"},
		Firewalls: map[int64]string{firewallID: "node-firewall"},
		SSHKeys:   map[int64]string{sshKeyID: "e2e-key"},
	}
}

// testNodeClass is a valid HCloudNodeClass matching the defaultCatalog
// fixtures (network 12345 exists, talos snapshots resolve, fsn1/hel1 are
// known locations).
func testNodeClass(name string) *apiv1.HCloudNodeClass {
	return &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:     []string{"fsn1", "hel1"},
			NetworkID:     networkID,
			FirewallIDs:   []int64{firewallID},
			SSHKeyIDs:     []int64{sshKeyID},
			ImageSelector: apiv1.ImageSelector{Family: apiv1.ImageFamily("talos")},
			Labels:        map[string]string{"team": "platform"},
			UserData:      "#!e2e-scenario",
		},
	}
}

// newNodeClaim builds a minimal NodeClaim referencing the named NodeClass,
// pinned to fsn1 and asking for one CPU.
func newNodeClaim(name, nodeClassName string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{karpv1.NodePoolLabelKey: "pool-a"},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: apiv1.GroupVersion.Group,
				Kind:  "HCloudNodeClass",
				Name:  nodeClassName,
			},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"fsn1"},
			}},
			Resources: karpv1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		},
	}
}

// createNodeClass creates a NodeClass through the real API server and waits
// for the nodeclass controller to report the wanted Ready condition.
func (h *harness) createNodeClass(t *testing.T, nc *apiv1.HCloudNodeClass, wantReady metav1.ConditionStatus) *apiv1.HCloudNodeClass {
	t.Helper()
	ctx := context.Background()
	if err := h.kube.Create(ctx, nc); err != nil {
		t.Fatalf("creating HCloudNodeClass %q: %v", nc.Name, err)
	}
	t.Cleanup(func() {
		if err := h.kube.Delete(context.Background(), nc); client.IgnoreNotFound(err) != nil {
			t.Logf("deleting HCloudNodeClass %q: %v", nc.Name, err)
		}
	})
	return h.waitNodeClass(t, nc.Name, wantReady)
}

// waitNodeClass polls the real API server until the NodeClass's aggregated
// Ready condition reaches wantReady, then returns the observed object.
func (h *harness) waitNodeClass(t *testing.T, name string, wantReady metav1.ConditionStatus) *apiv1.HCloudNodeClass {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(reconcileTimeout)
	for {
		nc := &apiv1.HCloudNodeClass{}
		if err := h.kube.Get(ctx, types.NamespacedName{Name: name}, nc); err != nil {
			t.Fatalf("reading HCloudNodeClass %q: %v", name, err)
		}
		if cond := nc.StatusConditions().Get(status.ConditionReady); cond != nil && cond.Status == wantReady {
			return nc
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for HCloudNodeClass %q Ready=%s; last status: %+v", name, wantReady, nc.Status.Conditions)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mustCondition returns the named condition or fails the test.
func mustCondition(t *testing.T, nc *apiv1.HCloudNodeClass, condType string) status.Condition {
	t.Helper()
	for _, c := range nc.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	t.Fatalf("condition %s not found on HCloudNodeClass %q (got %+v)", condType, nc.Name, nc.Status.Conditions)
	return status.Condition{}
}

// mustQuantity asserts that got[name] equals want.
func mustQuantity(t *testing.T, got corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	q, ok := got[name]
	if !ok {
		t.Fatalf("resource %s missing from %v", name, got)
	}
	if q.Cmp(resource.MustParse(want)) != 0 {
		t.Fatalf("resource %s = %s, want %s", name, q.String(), want)
	}
}

// fmtLabel renders a label map for failure messages.
func fmtLabel(labels map[string]string, key string) string {
	return fmt.Sprintf("%q=%q (all labels: %v)", key, labels[key], labels)
}
