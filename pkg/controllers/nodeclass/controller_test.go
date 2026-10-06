package nodeclass

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// testEnv wires a fake controller-runtime client and an httptest-backed
// hcloud client. The fake client has the HCloudNodeClass status subresource
// registered so reconcile loop status patches hit the same code path the
// real API server would.
type testEnv struct {
	kubeClient client.Client
	hcloud     *hcloud.Client
	server     *httptest.Server
	mux        *http.ServeMux
}

func newTestEnv(t *testing.T, handler http.Handler, initObjs ...client.Object) *testEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apiv1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	var mux *http.ServeMux
	if handler == nil {
		mux = http.NewServeMux()
		handler = mux
	} else if m, ok := handler.(*http.ServeMux); ok {
		mux = m
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	hcloudClient := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Microsecond), MaxRetries: 1}),
	)

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithStatusSubresource(&apiv1.HCloudNodeClass{}).
		Build()

	return &testEnv{
		kubeClient: kubeClient,
		hcloud:     hcloudClient,
		server:     server,
		mux:        mux,
	}
}

func (e *testEnv) getNodeClass(t *testing.T, name string) *apiv1.HCloudNodeClass {
	t.Helper()
	nc := &apiv1.HCloudNodeClass{}
	if err := e.kubeClient.Get(context.Background(), types.NamespacedName{Name: name}, nc); err != nil {
		t.Fatalf("re-reading HCloudNodeClass: %v", err)
	}
	return nc
}

// findCondition returns the named condition, or nil when it is absent.
func findCondition(t *testing.T, nc *apiv1.HCloudNodeClass, condType string) *status.Condition {
	t.Helper()
	for i := range nc.Status.Conditions {
		if nc.Status.Conditions[i].Type == condType {
			return &nc.Status.Conditions[i]
		}
	}
	return nil
}

func mustCondition(t *testing.T, nc *apiv1.HCloudNodeClass, condType string) status.Condition {
	t.Helper()
	c := findCondition(t, nc, condType)
	if c == nil {
		t.Fatalf("expected condition %s on HCloudNodeClass, got none", condType)
	}
	return *c
}

func validNodeClass(name string) *apiv1.HCloudNodeClass {
	return &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:     []string{"fsn1", "hel1"},
			NetworkID:     12345,
			FirewallIDs:   []int64{9001},
			SSHKeyIDs:     []int64{7001},
			ImageSelector: apiv1.ImageSelector{Family: apiv1.ImageFamily("talos")},
		},
	}
}

// happyPathMux returns an hcloud handler that responds positively to every
// endpoint the reconciler calls during a successful reconcile.
func happyPathMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/images", imageListHandlerFiltered([]schema.Image{
		amd64Image(11, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		amd64Image(12, "talos v1.9.6", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		arm64Image(13, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		amd64Image(99, "ubuntu-22.04", time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("cluster-net"))
	mux.HandleFunc("/firewalls/9001", firewallHandler(9001, "node-firewall"))
	mux.HandleFunc("/ssh_keys/7001", sshKeyHandler())
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{
		{Name: "fsn1", ID: 1, NetworkZone: "eu-central"},
		{Name: "hel1", ID: 2, NetworkZone: "eu-central"},
	}))

	return mux
}

func amd64Image(id int64, description string, created time.Time) schema.Image {
	return schema.Image{
		ID:           id,
		Status:       string(hcloud.ImageStatusAvailable),
		Type:         string(hcloud.ImageTypeSnapshot),
		Description:  description,
		Architecture: string(hcloud.ArchitectureX86),
		Created:      &created,
	}
}

func arm64Image(id int64, created time.Time) schema.Image {
	img := amd64Image(id, "talos v1.9.5", created)
	img.Architecture = string(hcloud.ArchitectureARM)
	return img
}

func imageListHandler(images []schema.Image) http.HandlerFunc {
	return imageListHandlerFiltered(images)
}

// imageListHandlerFiltered mirrors the hcloud API behaviour of filtering
// images by the architecture, type and status query parameters — without
// this, the test mock would return images of every architecture to every
// reconcile pass, defeating the purpose of the arch filter.
func imageListHandlerFiltered(images []schema.Image) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var filtered []schema.Image
		for _, img := range images {
			if arch := q.Get("architecture"); arch != "" && img.Architecture != arch {
				continue
			}
			if t := q.Get("type"); t != "" && img.Type != t {
				continue
			}
			if s := q.Get("status"); s != "" && img.Status != s {
				continue
			}
			filtered = append(filtered, img)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Images []schema.Image `json:"images"`
			Meta   schema.Meta    `json:"meta"`
		}{
			Images: filtered,
			Meta: schema.Meta{
				Pagination: &schema.MetaPagination{
					Page: 1, LastPage: 1, PerPage: len(filtered), TotalEntries: len(filtered),
				},
			},
		})
	}
}

func networkHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.NetworkGetResponse{
			Network: schema.Network{Name: name},
		})
	}
}

func firewallHandler(id int64, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.FirewallGetResponse{
			Firewall: schema.Firewall{ID: id, Name: name},
		})
	}
}

// sshKeyHandler serves the one SSH key the fixtures reference —
// validNodeClass always points at 7001, so there is nothing to parameterise.
func sshKeyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.SSHKeyGetResponse{
			SSHKey: schema.SSHKey{ID: 7001, Name: "key-7001"},
		})
	}
}

func locationsHandler(locations []schema.Location) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Locations []schema.Location `json:"locations"`
			Meta      schema.Meta       `json:"meta"`
		}{
			Locations: locations,
			Meta: schema.Meta{
				Pagination: &schema.MetaPagination{
					Page: 1, LastPage: 1, PerPage: len(locations), TotalEntries: len(locations),
				},
			},
		})
	}
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(schema.ErrorResponse{
		Error: schema.Error{Code: string(hcloud.ErrorCodeNotFound), Message: "not found"},
	})
}

func TestReconcile_NotFoundReturnsClean(t *testing.T) {
	env := newTestEnv(t, nil)
	r := New(env.kubeClient, env.hcloud)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue on not-found, got %s", res.RequeueAfter)
	}
}

func TestReconcile_DeletionMarksReadyFalse(t *testing.T) {
	now := metav1.Now()
	nc := validNodeClass("being-deleted")
	nc.DeletionTimestamp = &now
	nc.Finalizers = []string{"keep"}

	env := newTestEnv(t, nil, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "being-deleted"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "being-deleted")
	ready := mustCondition(t, got, status.ConditionReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != "Deleted" {
		t.Fatalf("expected Ready=False/Deleted, got %+v", ready)
	}
}

func TestReconcile_HappyPath(t *testing.T) {
	env := newTestEnv(t, happyPathMux(), validNodeClass("ok"))
	r := New(env.kubeClient, env.hcloud)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ok"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != requeueInterval {
		t.Fatalf("expected RequeueAfter=%s, got %s", requeueInterval, res.RequeueAfter)
	}

	got := env.getNodeClass(t, "ok")
	for _, cond := range []string{
		apiv1.ConditionTypeImagesReady,
		apiv1.ConditionTypeNetworkReady,
		apiv1.ConditionTypeResourcesReady,
		apiv1.ConditionTypeUserDataReady,
		status.ConditionReady,
	} {
		c := mustCondition(t, got, cond)
		if c.Status != metav1.ConditionTrue {
			t.Errorf("condition %s expected True, got %+v", cond, c)
		}
	}

	if len(got.Status.ResolvedImages) != 2 {
		t.Fatalf("expected 2 resolved images, got %d (%+v)", len(got.Status.ResolvedImages), got.Status.ResolvedImages)
	}
	byArch := map[string]int64{}
	for _, ri := range got.Status.ResolvedImages {
		byArch[ri.Architecture] = ri.ImageID
	}
	// amd64: newest talos snapshot is id=12 (v1.9.6), arm64 is id=13.
	if byArch["amd64"] != 12 {
		t.Errorf("expected amd64 image id=12, got %d", byArch["amd64"])
	}
	if byArch["arm64"] != 13 {
		t.Errorf("expected arm64 image id=13, got %d", byArch["arm64"])
	}
}

func TestReconcile_ImageSelectorLabelIsForwarded(t *testing.T) {
	mux := http.NewServeMux()
	var observedSelector string
	mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		observedSelector = r.URL.Query().Get("label_selector")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Images []schema.Image `json:"images"`
			Meta   schema.Meta    `json:"meta"`
		}{
			Images: []schema.Image{
				amd64Image(21, "talos v1.9.5", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)),
			},
			Meta: schema.Meta{Pagination: &schema.MetaPagination{Page: 1, LastPage: 1, PerPage: 1, TotalEntries: 1}},
		})
	})
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1", ID: 1}}))

	nc := validNodeClass("labeled")
	nc.Spec.Locations = []string{"fsn1"}
	nc.Spec.ImageSelector = apiv1.ImageSelector{
		Family:   apiv1.ImageFamily("talos"),
		Selector: map[string]string{"caph-image-name": "talos-v1.9.5-gvisor"},
	}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "labeled"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// buildLabelSelector sorts keys so the expected value is stable.
	want := "caph-image-name=talos-v1.9.5-gvisor"
	if observedSelector != want {
		t.Errorf("expected label_selector=%q, got %q", want, observedSelector)
	}
}

func TestReconcile_ImageNotFoundMarksFalse(t *testing.T) {
	mux := http.NewServeMux()
	// Return no images so the description filter has nothing to match.
	mux.HandleFunc("/images", imageListHandler(nil))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("missing-img")
	nc.Spec.Locations = []string{"fsn1"}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing-img"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "missing-img")
	c := mustCondition(t, got, apiv1.ConditionTypeImagesReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "ImageNotFound" {
		t.Fatalf("expected ImagesReady=False/ImageNotFound, got %+v", c)
	}
	if len(got.Status.ResolvedImages) != 0 {
		t.Fatalf("expected ResolvedImages cleared, got %+v", got.Status.ResolvedImages)
	}
	ready := mustCondition(t, got, status.ConditionReady)
	if ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False when images unresolved, got %+v", ready)
	}
}

func TestReconcile_NetworkMissingMarksFalse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(31, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(32, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/", notFoundHandler)
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("no-net")
	nc.Spec.Locations = []string{"fsn1"}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "no-net"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "no-net")
	c := mustCondition(t, got, apiv1.ConditionTypeNetworkReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "NetworkNotFound" {
		t.Fatalf("expected NetworkReady=False/NetworkNotFound, got %+v", c)
	}
}

func TestReconcile_MissingFirewallMarksResourcesFalse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(41, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(42, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/firewalls/", notFoundHandler)
	mux.HandleFunc("/ssh_keys/7001", sshKeyHandler())
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("no-fw")
	nc.Spec.Locations = []string{"fsn1"}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "no-fw"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "no-fw")
	c := mustCondition(t, got, apiv1.ConditionTypeResourcesReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "FirewallsNotFound" {
		t.Fatalf("expected ResourcesReady=False/FirewallsNotFound, got %+v", c)
	}
}

func TestReconcile_UnknownLocationMarksResourcesFalse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(51, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(52, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/firewalls/9001", firewallHandler(9001, "fw"))
	mux.HandleFunc("/ssh_keys/7001", sshKeyHandler())
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{
		{Name: "fsn1", ID: 1},
	}))

	nc := validNodeClass("bad-loc")
	nc.Spec.Locations = []string{"fsn1", "atlantis"}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "bad-loc"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "bad-loc")
	c := mustCondition(t, got, apiv1.ConditionTypeResourcesReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "LocationsNotFound" {
		t.Fatalf("expected ResourcesReady=False/LocationsNotFound, got %+v", c)
	}
}

func TestReconcile_UserDataSecretReady(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(61, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(62, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-config", Namespace: "kube-system"},
		Data:       map[string][]byte{"userData": []byte("#!talos")},
	}
	nc := validNodeClass("secret-ok")
	nc.Spec.Locations = []string{"fsn1"}
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretRef{
		Namespace: "kube-system",
		Name:      "talos-config",
		Key:       "userData",
	}
	env := newTestEnv(t, mux, nc, secret)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "secret-ok"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "secret-ok")
	c := mustCondition(t, got, apiv1.ConditionTypeUserDataReady)
	if c.Status != metav1.ConditionTrue || c.Reason != "UserDataFromSecret" {
		t.Fatalf("expected UserDataReady=True/UserDataFromSecret, got %+v", c)
	}

	// Confirm the secret payload was NOT copied into the NodeClass status
	// (the status struct has no userData field, but assert defensively).
	bs, err := json.Marshal(got.Status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if contains(bs, "#!talos") {
		t.Fatalf("secret payload leaked into status: %s", string(bs))
	}
}

// contains is a tiny helper to avoid importing bytes for one substring check.
func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestReconcile_UserDataSecretMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(71, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(72, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("secret-missing")
	nc.Spec.Locations = []string{"fsn1"}
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretRef{
		Namespace: "kube-system",
		Name:      "does-not-exist",
		Key:       "userData",
	}
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "secret-missing"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "secret-missing")
	c := mustCondition(t, got, apiv1.ConditionTypeUserDataReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "UserDataSecretNotFound" {
		t.Fatalf("expected UserDataReady=False/UserDataSecretNotFound, got %+v", c)
	}
}

func TestReconcile_InlineUserDataEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(81, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(82, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/networks/12345", networkHandler("n"))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("inline-empty")
	nc.Spec.Locations = []string{"fsn1"}
	nc.Spec.UserData = "" // explicitly empty; API allows this
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "inline-empty"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "inline-empty")
	c := mustCondition(t, got, apiv1.ConditionTypeUserDataReady)
	if c.Status != metav1.ConditionTrue || c.Reason != "UserDataInline" {
		t.Fatalf("expected UserDataReady=True/UserDataInline, got %+v", c)
	}
}

func TestReconcile_NetworkIDZeroMarksFalse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", imageListHandler([]schema.Image{
		amd64Image(91, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		arm64Image(92, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}))
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{{Name: "fsn1"}}))

	nc := validNodeClass("no-netid")
	nc.Spec.Locations = []string{"fsn1"}
	nc.Spec.NetworkID = 0
	env := newTestEnv(t, mux, nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "no-netid"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "no-netid")
	c := mustCondition(t, got, apiv1.ConditionTypeNetworkReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "NetworkIDMissing" {
		t.Fatalf("expected NetworkReady=False/NetworkIDMissing, got %+v", c)
	}
}

func TestReconcile_ReconcileAgainIsIdempotent(t *testing.T) {
	env := newTestEnv(t, happyPathMux(), validNodeClass("idem"))
	r := New(env.kubeClient, env.hcloud)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "idem"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	got := env.getNodeClass(t, "idem")
	ready := mustCondition(t, got, status.ConditionReady)
	if ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True after repeated reconciles, got %+v", ready)
	}
	if len(got.Status.ResolvedImages) != 2 {
		t.Fatalf("expected 2 resolved images after repeated reconciles, got %d", len(got.Status.ResolvedImages))
	}
}

func TestArchLabel(t *testing.T) {
	if got := archLabel(hcloud.ArchitectureX86); got != "amd64" {
		t.Errorf("archLabel(x86) = %q, want amd64", got)
	}
	if got := archLabel(hcloud.ArchitectureARM); got != "arm64" {
		t.Errorf("archLabel(arm) = %q, want arm64", got)
	}
}

// Ensure the fake client returns a typed NotFound error so the controller's
// IgnoreNotFound check exercises the same code path the real client would.
func TestReconcile_NotFoundErrorIsTyped(t *testing.T) {
	env := newTestEnv(t, nil)
	r := New(env.kubeClient, env.hcloud)

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "x"}})
	if err != nil {
		t.Fatalf("expected nil error on not-found, got %v", err)
	}
	if err := env.kubeClient.Get(context.Background(), types.NamespacedName{Name: "absent"}, &apiv1.HCloudNodeClass{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected typed NotFound from fake client, got %v", err)
	}
}

// TestReconcile_UnsupportedImageFamilyMarksFalse verifies the controller
// rejects an out-of-contract family before doing any image API work. The
// CRD's XValidation catches this on a real cluster, but the controller must
// not trust admission alone — and it must report it distinctly from "the
// selector matched nothing".
func TestReconcile_UnsupportedImageFamilyMarksFalse(t *testing.T) {
	nc := validNodeClass("bad-family")
	nc.Spec.ImageSelector = apiv1.ImageSelector{Family: apiv1.ImageFamily("debian")}
	env := newTestEnv(t, happyPathMux(), nc)
	r := New(env.kubeClient, env.hcloud)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "bad-family"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := env.getNodeClass(t, "bad-family")
	c := mustCondition(t, got, apiv1.ConditionTypeImagesReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "ImageSelectorInvalid" {
		t.Fatalf("expected ImagesReady=False/ImageSelectorInvalid, got %+v", c)
	}
	if len(got.Status.ResolvedImages) != 0 {
		t.Fatalf("expected ResolvedImages cleared, got %+v", got.Status.ResolvedImages)
	}
}

// mutableImageMux returns the endpoints a successful reconcile needs, with
// an /images handler that reads *images at request time (so tests can publish
// a snapshot mid-test) and can be switched to fail like a flaky API.
func mutableImageMux(images *[]schema.Image, failing *bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		if failing != nil && *failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		imageListHandlerFiltered(*images)(w, r)
	})
	mux.HandleFunc("/networks/12345", networkHandler("cluster-net"))
	mux.HandleFunc("/firewalls/9001", firewallHandler(9001, "node-firewall"))
	mux.HandleFunc("/ssh_keys/7001", sshKeyHandler())
	mux.HandleFunc("/locations", locationsHandler([]schema.Location{
		{Name: "fsn1", ID: 1, NetworkZone: "eu-central"},
		{Name: "hel1", ID: 2, NetworkZone: "eu-central"},
	}))
	return mux
}

func reconcileNodeClass(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func assertResolvedImages(t *testing.T, nc *apiv1.HCloudNodeClass, want map[string]int64) {
	t.Helper()
	got := make(map[string]int64, len(nc.Status.ResolvedImages))
	for _, ri := range nc.Status.ResolvedImages {
		got[ri.Architecture] = ri.ImageID
	}
	if len(got) != len(want) {
		t.Fatalf("resolvedImages = %v, want %v", got, want)
	}
	for arch, id := range want {
		if got[arch] != id {
			t.Errorf("resolvedImages[%s] = %d, want %d", arch, got[arch], id)
		}
	}
}

// TestReconcile_ResolutionStickyUntilSelectorChanges pins the churn rule the
// image-selector drift check depends on: publishing a snapshot that matches
// the current selector must NOT move status.resolvedImages, while editing
// the selector so the recorded image stops matching must.
func TestReconcile_ResolutionStickyUntilSelectorChanges(t *testing.T) {
	arm := amd64Image(13, "talos v1.9.7", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	arm.Architecture = string(hcloud.ArchitectureARM)
	// Capacity covers the snapshot the test publishes mid-flight below.
	images := make([]schema.Image, 0, 4)
	images = append(images,
		amd64Image(11, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		amd64Image(12, "talos v1.9.6", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		arm,
	)

	nc := validNodeClass("sticky")
	env := newTestEnv(t, mutableImageMux(&images, nil), nc)
	r := New(env.kubeClient, env.hcloud)

	// First pass: newest per architecture.
	reconcileNodeClass(t, r, "sticky")
	assertResolvedImages(t, env.getNodeClass(t, "sticky"), map[string]int64{"amd64": 12, "arm64": 13})

	// A newer amd64 snapshot lands in the project. The selector did not
	// change, so the resolution must not move — otherwise every node would
	// be replaced for someone else's snapshot push.
	images = append(images, amd64Image(14, "talos v1.9.7", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)))
	reconcileNodeClass(t, r, "sticky")
	assertResolvedImages(t, env.getNodeClass(t, "sticky"), map[string]int64{"amd64": 12, "arm64": 13})

	// The operator pins the selector to the new version: the recorded amd64
	// image no longer matches, so resolution moves (and the drift check has
	// something real to act on). arm64 still matches, so it stays put.
	stored := env.getNodeClass(t, "sticky")
	stored.Spec.ImageSelector.Version = "v1.9.7"
	if err := env.kubeClient.Update(context.Background(), stored); err != nil {
		t.Fatalf("updating imageSelector: %v", err)
	}
	reconcileNodeClass(t, r, "sticky")
	assertResolvedImages(t, env.getNodeClass(t, "sticky"), map[string]int64{"amd64": 14, "arm64": 13})
}

// TestReconcile_APIErrorKeepsResolvedImages proves a transient hcloud failure
// does not erase the recorded images: they are still valid, and clearing them
// would make the next healthy pass re-pick the newest match — churning nodes
// over a blip.
func TestReconcile_APIErrorKeepsResolvedImages(t *testing.T) {
	images := []schema.Image{
		amd64Image(11, "talos v1.9.5", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		amd64Image(12, "talos v1.9.6", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		arm64Image(13, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	failing := false

	nc := validNodeClass("blip")
	env := newTestEnv(t, mutableImageMux(&images, &failing), nc)
	r := New(env.kubeClient, env.hcloud)

	reconcileNodeClass(t, r, "blip")
	assertResolvedImages(t, env.getNodeClass(t, "blip"), map[string]int64{"amd64": 12, "arm64": 13})

	failing = true
	reconcileNodeClass(t, r, "blip")

	got := env.getNodeClass(t, "blip")
	c := mustCondition(t, got, apiv1.ConditionTypeImagesReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "ImageResolutionFailed" {
		t.Fatalf("expected ImagesReady=False/ImageResolutionFailed, got %+v", c)
	}
	assertResolvedImages(t, got, map[string]int64{"amd64": 12, "arm64": 13})
}
