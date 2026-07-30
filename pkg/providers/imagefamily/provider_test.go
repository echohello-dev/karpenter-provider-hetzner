package imagefamily

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// fakeImageServer pretends to be the Hetzner /images endpoint. It returns
// the page of images registered for the requested page number and records
// every request so tests can assert on the query parameters the provider
// actually sent. It is the public seam hcloud-go exposes — no internal
// client stubs, no package-private mocks.
type fakeImageServer struct {
	*httptest.Server

	mu      sync.Mutex
	seen    []*http.Request
	pageMap map[string]fakeImagePage

	// Optional handler: when set, replaces the default page response.
	// Useful for simulating API errors.
	override func(w http.ResponseWriter, r *http.Request)
}

type fakeImagePage struct {
	images   []schema.Image
	page     int
	lastPage int
	total    int
}

func newFakeImageServer(t *testing.T) *fakeImageServer {
	t.Helper()
	s := &fakeImageServer{pageMap: map[string]fakeImagePage{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *fakeImageServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	r2 := r.Clone(r.Context())
	s.seen = append(s.seen, r2)
	s.mu.Unlock()

	if s.override != nil {
		s.override(w, r)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/images") {
		http.NotFound(w, r)
		return
	}

	pageStr := r.URL.Query().Get("page")
	if pageStr == "" {
		pageStr = "1"
	}
	page, _ := strconv.Atoi(pageStr)

	p, ok := s.pageMap[fmt.Sprintf("%d", page)]
	if !ok {
		writeImageList(w, []schema.Image{}, page, page, 0)
		return
	}
	writeImageList(w, p.images, p.page, p.lastPage, p.total)
}

func writeImageList(w http.ResponseWriter, images []schema.Image, page, lastPage, total int) {
	w.Header().Set("Content-Type", "application/json")
	nextPage := 0
	if page < lastPage {
		nextPage = page + 1
	}
	_ = json.NewEncoder(w).Encode(struct {
		Images []schema.Image `json:"images"`
		Meta   schema.Meta    `json:"meta"`
	}{
		Images: images,
		Meta: schema.Meta{
			Pagination: &schema.MetaPagination{
				Page:         page,
				LastPage:     lastPage,
				PerPage:      len(images),
				TotalEntries: total,
				NextPage:     nextPage,
			},
		},
	})
}

func (s *fakeImageServer) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*http.Request, len(s.seen))
	copy(out, s.seen)
	return out
}

// helper: build a complete available image fixture.
func newImage(id int64, desc string, arch hcloud.Architecture, created time.Time) schema.Image {
	return schema.Image{
		ID:           id,
		Status:       "available",
		Type:         "snapshot",
		Description:  desc,
		Architecture: string(arch),
		Created:      &created,
		Labels:       map[string]string{},
	}
}

func newClient(t *testing.T, ts *fakeImageServer) *hcloud.Client {
	t.Helper()
	c := hcloud.NewClient(
		hcloud.WithEndpoint(ts.URL),
		hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{
			BackoffFunc: hcloud.ConstantBackoff(0),
			MaxRetries:  1,
		}),
	)
	return c
}

// requestMustHaveArch asserts that every recorded request sent the
// architecture query parameter.
func requestMustHaveArch(t *testing.T, reqs []*http.Request, want hcloud.Architecture) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("no requests were sent to the image API")
	}
	for i, r := range reqs {
		got := r.URL.Query().Get("architecture")
		if got != string(want) {
			t.Errorf("request %d: architecture=%q, want %q", i, got, want)
		}
	}
}

func TestResolve_ReturnsNewestMatchingImage(t *testing.T) {
	ts := newFakeImageServer(t)
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(11, "talos v1.9.4", hcloud.ArchitectureX86, older),
			newImage(12, "talos v1.9.5", hcloud.ArchitectureX86, newer),
		},
		page: 1, lastPage: 1, total: 2,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 12 {
		t.Errorf("resolved image id = %d, want 12 (newest)", res.Image.ID)
	}
	if res.Architecture != hcloud.ArchitectureX86 {
		t.Errorf("resolved architecture = %s, want x86", res.Architecture)
	}
}

func TestResolve_PaginationAcrossPages(t *testing.T) {
	ts := newFakeImageServer(t)
	oldest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.0", hcloud.ArchitectureX86, oldest),
			newImage(2, "talos v1.9.1", hcloud.ArchitectureX86, mid),
		},
		page: 1, lastPage: 2, total: 3,
	}
	ts.pageMap["2"] = fakeImagePage{
		images: []schema.Image{
			newImage(3, "talos v1.9.2", hcloud.ArchitectureX86, newest),
		},
		page: 2, lastPage: 2, total: 3,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 3 {
		t.Errorf("resolved image id = %d, want 3 (newest, from page 2)", res.Image.ID)
	}
	if got := len(ts.requests()); got < 2 {
		t.Errorf("expected at least 2 paginated requests; got %d", got)
	}
}

func TestResolve_VersionSubstringFilter(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now().Add(-time.Hour)),
			newImage(2, "talos v1.10.0", hcloud.ArchitectureX86, time.Now().Add(-time.Minute)),
		},
		page: 1, lastPage: 1, total: 2,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{
		Family:  FamilyTalos,
		Version: "v1.9",
	}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 1 {
		t.Errorf("resolved image id = %d, want 1 (only v1.9 match)", res.Image.ID)
	}
}

func TestResolve_LabelSelectorSentToAPI(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	selector := map[string]string{
		"caph-image-name": "talos-v1.9.5-gvisor",
		"environment":     "prod",
	}
	if _, err := p.Resolve(context.Background(), apiv1.ImageSelector{
		Family:   FamilyTalos,
		Selector: selector,
	}, hcloud.ArchitectureX86); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	reqs := ts.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests were sent")
	}
	got := reqs[0].URL.Query().Get("label_selector")
	// formatLabelSelector sorts keys alphabetically.
	want := "caph-image-name=talos-v1.9.5-gvisor,environment=prod"
	if got != want {
		t.Errorf("label_selector = %q, want %q", got, want)
	}
}

func TestResolve_ArchitectureFilterSentToAPI(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureARM, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	if _, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86); err == nil {
		t.Fatal("expected no-match error for arch=x86 when only an arm image is returned")
	}
	reqs := ts.requests()
	requestMustHaveArch(t, reqs, hcloud.ArchitectureX86)
}

func TestResolve_DefensiveMismatchSkipped(t *testing.T) {
	ts := newFakeImageServer(t)
	// The fake server ignores the architecture query parameter and
	// returns a mismatched image. The provider must defensively skip it
	// rather than boot a wrong-arch node.
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureARM, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected no-match error for defensively-skipped wrong-arch image")
	}
	if !strings.Contains(err.Error(), "no matching image") {
		t.Errorf("expected descriptive no-match error, got %v", err)
	}
}

func TestResolve_IgnoresDeprecatedAndDeleted(t *testing.T) {
	ts := newFakeImageServer(t)
	now := time.Now()
	nowCopy := now
	deprecated := newImage(1, "talos v1.9.4", hcloud.ArchitectureX86, now)
	deprecated.Deprecated = &nowCopy
	deleted := newImage(2, "talos v1.9.5", hcloud.ArchitectureX86, now)
	deleted.Deleted = &nowCopy
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{deprecated, deleted},
		page:   1, lastPage: 1, total: 2,
	}
	p := New(newClient(t, ts))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected no-match error when every image is deprecated or deleted")
	}
	if !strings.Contains(err.Error(), "no matching image") {
		t.Errorf("expected descriptive no-match error, got %v", err)
	}
}

func TestResolve_IgnoresNonAvailableStatus(t *testing.T) {
	ts := newFakeImageServer(t)
	creating := newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now())
	creating.Status = "creating"
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{creating},
		page:   1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected no-match error when only a 'creating' image is returned")
	}
}

func TestResolve_SendsStatusAvailableFilter(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	if _, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	reqs := ts.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests were sent")
	}
	if got := reqs[0].URL.Query().Get("status"); got != "available" {
		t.Errorf("status filter = %q, want %q", got, "available")
	}
}

func TestResolve_DeterministicNewestByCreatedThenID(t *testing.T) {
	ts := newFakeImageServer(t)
	same := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(10, "talos v1.9.5", hcloud.ArchitectureX86, same),
			newImage(20, "talos v1.9.5", hcloud.ArchitectureX86, same),
			newImage(30, "talos v1.9.5", hcloud.ArchitectureX86, same),
		},
		page: 1, lastPage: 1, total: 3,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 30 {
		t.Errorf("resolved image id = %d, want 30 (highest ID on equal Created)", res.Image.ID)
	}
}

func TestResolve_OrdersByCreatedNewestFirst(t *testing.T) {
	ts := newFakeImageServer(t)
	earliest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	middle := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	// Note: the API is not required to return images in Created order,
	// so feed them in the wrong order and let Resolve sort.
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.0", hcloud.ArchitectureX86, earliest),
			newImage(3, "talos v1.9.2", hcloud.ArchitectureX86, latest),
			newImage(2, "talos v1.9.1", hcloud.ArchitectureX86, middle),
		},
		page: 1, lastPage: 1, total: 3,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 3 {
		t.Errorf("resolved image id = %d, want 3 (latest Created)", res.Image.ID)
	}
}

func TestResolve_NoMatchReturnsDescriptiveError(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "ubuntu 22.04", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{
		Family:  FamilyTalos,
		Version: "v1.9.5",
	}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected no-match error")
	}
	for _, want := range []string{"family=talos", "arch=x86", `version="v1.9.5"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
}

func TestResolve_APIErrorIsWrapped(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.override = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(schema.ErrorResponse{
			Error: schema.Error{
				Code:    "service_error",
				Message: "boom",
			},
		})
	}
	p := New(newClient(t, ts))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected error on 500 from API")
	}
	if !strings.Contains(err.Error(), "imagefamily.Resolve") {
		t.Errorf("error should be wrapped with imagefamily.Resolve context, got %q", err)
	}
	if !strings.Contains(err.Error(), "listing hcloud images") {
		t.Errorf("error should mention the failing operation, got %q", err)
	}
}

func TestResolve_EmptyFamilyIsRejected(t *testing.T) {
	p := New(newClient(t, newFakeImageServer(t)))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected error when Family is empty")
	}
	if !strings.Contains(err.Error(), "Family is required") {
		t.Errorf("expected 'Family is required' in error, got %q", err)
	}
}

func TestResolve_UnsupportedFamilyIsRejected(t *testing.T) {
	p := New(newClient(t, newFakeImageServer(t)))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{
		Family: apiv1.ImageFamily("debian"),
	}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected error for unsupported family")
	}
	if !strings.Contains(err.Error(), "unsupported family") {
		t.Errorf("expected 'unsupported family' in error, got %q", err)
	}
}

func TestResolve_EmptyArchIsRejected(t *testing.T) {
	p := New(newClient(t, newFakeImageServer(t)))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, "")
	if err == nil {
		t.Fatal("expected error when architecture is empty")
	}
	if !strings.Contains(err.Error(), "architecture is required") {
		t.Errorf("expected 'architecture is required' in error, got %q", err)
	}
}

func TestResolve_UnsupportedArchIsRejected(t *testing.T) {
	p := New(newClient(t, newFakeImageServer(t)))

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.Architecture("arm64"))
	if err == nil {
		t.Fatal("expected error for unsupported architecture")
	}
	if !strings.Contains(err.Error(), "unsupported architecture") {
		t.Errorf("expected 'unsupported architecture' in error, got %q", err)
	}
}

func TestResolve_UbuntuFamilyMatchesByDescription(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now()),
			newImage(2, "Ubuntu 22.04", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 2,
	}
	p := New(newClient(t, ts))

	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyUbuntu}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 2 {
		t.Errorf("resolved image id = %d, want 2 (Ubuntu)", res.Image.ID)
	}
}

func TestResolve_FamilyAndVersionAreAndCombined(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5 amd64", hcloud.ArchitectureX86, time.Now()),
			newImage(2, "ubuntu 22.04 v1.0", hcloud.ArchitectureX86, time.Now()),
			newImage(3, "talos v1.10.0 amd64", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 3,
	}
	p := New(newClient(t, ts))

	// family=talos AND version=v1.9 → only image 1.
	res, err := p.Resolve(context.Background(), apiv1.ImageSelector{
		Family:  FamilyTalos,
		Version: "v1.9",
	}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Image.ID != 1 {
		t.Errorf("resolved image id = %d, want 1 (talos v1.9)", res.Image.ID)
	}
}

func TestResolve_EmptySelectorDoesNotSendLabelSelector(t *testing.T) {
	ts := newFakeImageServer(t)
	ts.pageMap["1"] = fakeImagePage{
		images: []schema.Image{
			newImage(1, "talos v1.9.5", hcloud.ArchitectureX86, time.Now()),
		},
		page: 1, lastPage: 1, total: 1,
	}
	p := New(newClient(t, ts))

	if _, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: FamilyTalos}, hcloud.ArchitectureX86); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	reqs := ts.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests were sent")
	}
	if got := reqs[0].URL.Query().Get("label_selector"); got != "" {
		t.Errorf("label_selector should be empty when no selector is supplied; got %q", got)
	}
}

func TestFormatLabelSelector(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"nil", nil, ""},
		{"empty", map[string]string{}, ""},
		{"single", map[string]string{"a": "b"}, "a=b"},
		{
			"sorted alphabetically",
			map[string]string{"b": "2", "a": "1"},
			"a=1,b=2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatLabelSelector(tc.in); got != tc.want {
				t.Errorf("formatLabelSelector(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
