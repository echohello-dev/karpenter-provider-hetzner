package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/test/e2e/fakehcloud"
)

// Run the live test's full scenario against a local API, including the
// non-default location and architecture paths, without opting in to live calls.
func TestRealHetznerLifecycleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, typeName, arch string
		fallback             bool
		wantImage            int64
	}{
		{name: "x86 snapshot with newer ARM snapshot", typeName: "cx22", arch: "x86", wantImage: 12},
		{name: "ARM snapshot", typeName: "cax21", arch: "arm", wantImage: 13},
		{name: "x86 fallback", typeName: "cx22", arch: "x86", fallback: true, wantImage: 21},
		{name: "ARM fallback", typeName: "cax21", arch: "arm", fallback: true, wantImage: 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images := []fakehcloud.Image{
				{ID: 13, Architecture: "arm", Type: "snapshot", Created: time.Now()},
				{ID: 12, Architecture: "x86", Type: "snapshot", Created: time.Now().Add(-time.Hour)},
			}
			if tc.fallback {
				// Only the incompatible architecture has a snapshot.
				if tc.arch == "arm" {
					images = images[1:]
				} else {
					images = images[:1]
				}
			}
			images = append(images,
				fakehcloud.Image{ID: 21, Architecture: "x86", Type: "system"},
				fakehcloud.Image{ID: 22, Architecture: "arm", Type: "system"},
			)
			backend := fakehcloud.New(fakehcloud.Config{
				Images: images,
				ServerTypes: []fakehcloud.ServerType{{
					Name: tc.typeName, Architecture: tc.arch, Cores: 2, MemoryGB: 4, DiskGB: 40,
					HourlyNet: "0.01", Locations: map[string]bool{"hel1": true},
				}},
				Locations: []fakehcloud.Location{{ID: 1, Name: "hel1", NetworkZone: "eu-central"}},
			})
			t.Cleanup(backend.Close)
			var mu sync.Mutex
			var created schema.ServerCreateRequest
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// The shared fake has no image-name catalog; serve named system
				// images here with the API's architecture filtering semantics.
				if r.URL.Path == "/images" && r.URL.Query().Get("name") == "ubuntu-24.04" {
					name := "ubuntu-24.04"
					out := []schema.Image{}
					for _, image := range images {
						if image.Type == "system" && image.Architecture == r.URL.Query().Get("architecture") {
							out = append(out, schema.Image{ID: image.ID, Name: &name, Type: "system", Status: "available", Architecture: image.Architecture})
						}
					}
					_ = json.NewEncoder(w).Encode(schema.ImageListResponse{Images: out})
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/servers" {
					recorder := httptest.NewRecorder()
					backend.ServeHTTP(recorder, r)
					var response schema.ServerCreateResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Errorf("decoding fake create response: %v", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					if response.Server.Image == nil {
						t.Errorf("fake creation failed: %s", recorder.Body.String())
						w.WriteHeader(recorder.Code)
						_, _ = w.Write(recorder.Body.Bytes())
						return
					}
					mu.Lock()
					created = schema.ServerCreateRequest{Location: response.Server.Location.Name, Image: schema.IDOrName{ID: response.Server.Image.ID}}
					mu.Unlock()
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
					return
				}
				backend.ServeHTTP(w, r)
			}))
			t.Cleanup(api.Close)
			client := localHCloudClient(api.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			runRealHetznerLifecycle(t, ctx, client, "hel1", tc.typeName, "karpenter-e2e")
			mu.Lock()
			defer mu.Unlock()
			if created.Location != "hel1" || created.Image.ID != tc.wantImage {
				t.Errorf("created location=%q image=%d, want hel1 image=%d", created.Location, created.Image.ID, tc.wantImage)
			}
			if backend.ServerCount() != 0 {
				t.Error("lifecycle left a server behind")
			}
		})
	}
}

func TestWaitRealServerWaitsForUnlockedRunningServer(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		reads++
		server := schema.Server{ID: 4242, Status: "initializing", Locked: true}
		if reads >= 2 {
			server.Status = "running"
		}
		if reads >= 3 {
			server.Locked = false
		}
		_ = json.NewEncoder(w).Encode(schema.ServerGetResponse{Server: server})
	}))
	t.Cleanup(api.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitRealServer(ctx, localHCloudClient(api.URL), "hcloud://4242"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads < 3 {
		t.Fatal("returned before the running server was unlocked")
	}
}

func TestDeleteRealServerRetriesAndConfirmsAbsence(t *testing.T) {
	var mu sync.Mutex
	deletes, readsAfterDelete := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete {
			deletes++
			if deletes == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(schema.ErrorResponse{Error: schema.Error{Code: "locked", Message: "creation in progress"}})
				return
			}
			_ = json.NewEncoder(w).Encode(schema.ServerDeleteResponse{Action: schema.Action{ID: 2, Status: "running"}})
			return
		}
		if deletes >= 2 {
			readsAfterDelete++
			if readsAfterDelete >= 2 {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(schema.ErrorResponse{Error: schema.Error{Code: "not_found", Message: "deleted"}})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(schema.ServerGetResponse{Server: schema.Server{
			ID: 4242, Status: "running", Labels: map[string]string{"karpenter.sh/cluster": "karpenter-e2e"},
		}})
	}))
	t.Cleanup(api.Close)
	inst, err := instance.New(localHCloudClient(api.URL), "karpenter-e2e")
	if err != nil {
		t.Fatal(err)
	}
	cp := cloudprovider.New(nil, inst, nil, nil)
	claim := &karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: "hcloud://4242"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deleteRealServer(ctx, cp, claim); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 2 || readsAfterDelete < 2 {
		t.Errorf("deletes=%d readsAfterDelete=%d, want retry and confirmed absence", deletes, readsAfterDelete)
	}
}

func TestDeleteRealServerReportsTimeoutAndLastAPIError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(schema.ErrorResponse{Error: schema.Error{Code: "locked", Message: "still locked"}})
			return
		}
		_ = json.NewEncoder(w).Encode(schema.ServerGetResponse{Server: schema.Server{
			ID: 4242, Status: "initializing", Labels: map[string]string{"karpenter.sh/cluster": "karpenter-e2e"},
		}})
	}))
	t.Cleanup(api.Close)
	inst, err := instance.New(localHCloudClient(api.URL), "karpenter-e2e")
	if err != nil {
		t.Fatal(err)
	}
	cp := cloudprovider.New(nil, inst, nil, nil)
	claim := &karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: "hcloud://4242"}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = deleteRealServer(ctx, cp, claim)
	if !errors.Is(err, context.DeadlineExceeded) || !hcloud.IsError(err, hcloud.ErrorCodeLocked) {
		t.Fatalf("got %v, want deadline and last locked API error", err)
	}
}

func localHCloudClient(endpoint string) *hcloud.Client {
	return hcloud.NewClient(hcloud.WithEndpoint(endpoint), hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 1, BackoffFunc: hcloud.ConstantBackoff(time.Microsecond)}))
}

// TestPickLiveServerTypePrefersCheapestAvailable pins the auto-selection
// rule the live smoke test uses when E2E_SERVER_TYPE is unset: cheapest
// AVAILABLE offering at the location, skipping types Hetzner flags out of
// stock even when they are cheaper.
func TestPickLiveServerTypePrefersCheapestAvailable(t *testing.T) {
	backend := fakehcloud.New(fakehcloud.Config{
		ServerTypes: []fakehcloud.ServerType{
			{Name: "cheap-unavailable", Architecture: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, HourlyNet: "0.001", Locations: map[string]bool{"hel1": false}},
			{Name: "cheap-available", Architecture: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, HourlyNet: "0.01", Locations: map[string]bool{"hel1": true}},
			{Name: "expensive-available", Architecture: "x86", Cores: 4, MemoryGB: 8, DiskGB: 80, HourlyNet: "0.05", Locations: map[string]bool{"hel1": true}},
		},
		Locations: []fakehcloud.Location{{ID: 1, Name: "hel1", NetworkZone: "eu-central"}},
	})
	t.Cleanup(backend.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := pickLiveServerType(t, ctx, backend.Client(), "hel1"); got != "cheap-available" {
		t.Fatalf("pickLiveServerType = %q, want cheap-available (cheapest AVAILABLE at hel1)", got)
	}
}

// TestPickRealImageSkipsOversizedSnapshots pins the disk-fit rule: the live
// API refuses to boot a snapshot whose captured disk exceeds the server
// type's disk, so pickRealImage must walk past oversized (newer) snapshots.
func TestPickRealImageSkipsOversizedSnapshots(t *testing.T) {
	backend := fakehcloud.New(fakehcloud.Config{
		Images: []fakehcloud.Image{
			{ID: 20, Description: "talos huge", Architecture: "x86", Type: "snapshot", DiskSizeGB: 80, Created: time.Now()},
			{ID: 21, Description: "talos fits", Architecture: "x86", Type: "snapshot", DiskSizeGB: 40, Created: time.Now().Add(-time.Hour)},
		},
		ServerTypes: []fakehcloud.ServerType{
			{Name: "cx22", Architecture: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, HourlyNet: "0.01", Locations: map[string]bool{"fsn1": true}},
		},
		Locations: []fakehcloud.Location{{ID: 1, Name: "fsn1", NetworkZone: "eu-central"}},
	})
	t.Cleanup(backend.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := pickRealImage(t, ctx, backend.Client(), "cx22")
	if got.ID != 21 {
		t.Fatalf("pickRealImage = %d, want 21 (newest snapshot fitting a 40 GB disk)", got.ID)
	}
}
