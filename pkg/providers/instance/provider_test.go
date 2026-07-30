package instance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestNew(t *testing.T) {
	client := hcloud.NewClient()

	if _, err := New(nil, "my-cluster"); err == nil || !strings.Contains(err.Error(), "hcloud client is nil") {
		t.Fatalf("New(nil, cluster) error = %v", err)
	}
	if _, err := New(client, ""); err == nil || !strings.Contains(err.Error(), "cluster name is empty") {
		t.Fatalf("New(client, empty) error = %v", err)
	}
	provider, err := New(client, "my-cluster")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	key, value := provider.ClusterLabel()
	if key != clusterLabelKey || value != "my-cluster" {
		t.Fatalf("ClusterLabel() = (%q, %q)", key, value)
	}
}

func TestProviderCreate(t *testing.T) {
	var sawPlacementLookup, sawPlacementCreate, sawServerCreate atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/placement_groups":
			sawPlacementLookup.Store(true)
			if got := r.URL.Query().Get("name"); got != placementGroupName("cluster-a") {
				t.Errorf("placement group name = %q", got)
			}
			writeJSON(t, w, map[string]any{"placement_groups": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/placement_groups":
			sawPlacementCreate.Store(true)
			var body struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
				Type   string            `json:"type"`
			}
			decodeJSON(t, r, &body)
			if body.Name != placementGroupName("cluster-a") || body.Type != "spread" || !reflect.DeepEqual(body.Labels, map[string]string{clusterLabelKey: "cluster-a"}) {
				t.Errorf("placement group request = %+v", body)
			}
			writeJSON(t, w, map[string]any{"placement_group": map[string]any{"id": 700, "name": body.Name, "type": "spread", "labels": body.Labels, "servers": []any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			sawServerCreate.Store(true)
			var body struct {
				Name       string            `json:"name"`
				ServerType json.RawMessage   `json:"server_type"`
				Image      json.RawMessage   `json:"image"`
				SSHKeys    []int64           `json:"ssh_keys"`
				Location   string            `json:"location"`
				UserData   string            `json:"user_data"`
				Labels     map[string]string `json:"labels"`
				Networks   []int64           `json:"networks"`
				Firewalls  []struct {
					Firewall int64 `json:"firewall"`
				} `json:"firewalls"`
				PlacementGroup int64 `json:"placement_group"`
				PublicNet      struct {
					EnableIPv4 bool `json:"enable_ipv4"`
					EnableIPv6 bool `json:"enable_ipv6"`
				} `json:"public_net"`
			}
			decodeJSON(t, r, &body)
			wantLabels := map[string]string{
				"caller":          "value",
				clusterLabelKey:   "cluster-a",
				nodeClaimLabelKey: "claim-a",
				nodePoolLabelKey:  "pool-a",
			}
			if body.Name != "worker-a" || string(body.ServerType) != `"cx22"` || string(body.Image) != `99` || body.Location != "fsn1" || body.UserData != "user-data" {
				t.Errorf("server identity request = %+v", body)
			}
			if !reflect.DeepEqual(body.SSHKeys, []int64{31, 32}) || !reflect.DeepEqual(body.Networks, []int64{41}) || len(body.Firewalls) != 2 || body.Firewalls[0].Firewall != 51 || body.Firewalls[1].Firewall != 52 {
				t.Errorf("server attachments request = %+v", body)
			}
			if !reflect.DeepEqual(body.Labels, wantLabels) {
				t.Errorf("server labels = %#v, want %#v", body.Labels, wantLabels)
			}
			if body.PlacementGroup != 700 || !body.PublicNet.EnableIPv4 || body.PublicNet.EnableIPv6 {
				t.Errorf("server placement/public network request = %+v", body)
			}
			writeJSON(t, w, map[string]any{"server": map[string]any{"id": 123, "name": "worker-a", "labels": wantLabels}, "action": map[string]any{"id": 1}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	created, err := provider.Create(context.Background(), CreateOpts{
		Name:                   "worker-a",
		ServerType:             "cx22",
		Location:               "fsn1",
		Image:                  &hcloud.Image{ID: 99},
		NetworkID:              41,
		FirewallIDs:            []int64{51, 52},
		SSHKeyIDs:              []int64{31, 32},
		Labels:                 map[string]string{"caller": "value", clusterLabelKey: "wrong", nodeClaimLabelKey: "wrong", nodePoolLabelKey: "wrong"},
		UserData:               "user-data",
		NodeClaim:              "claim-a",
		NodePool:               "pool-a",
		PlacementGroupStrategy: "spread",
		EnablePublicIPv4:       true,
		EnablePublicIPv6:       false,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created == nil || created.ID != 123 {
		t.Fatalf("Create() = %#v", created)
	}
	if !sawPlacementLookup.Load() || !sawPlacementCreate.Load() || !sawServerCreate.Load() {
		t.Fatalf("requests: lookup=%t placement-create=%t server-create=%t", sawPlacementLookup.Load(), sawPlacementCreate.Load(), sawServerCreate.Load())
	}
}

func TestProviderCreateWithoutPlacementGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]json.RawMessage
		decodeJSON(t, r, &body)
		if _, ok := body["placement_group"]; ok {
			t.Fatal("placement_group must be absent for strategy none")
		}
		writeJSON(t, w, map[string]any{"server": map[string]any{"id": 124}, "action": map[string]any{"id": 1}})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	created, err := provider.Create(context.Background(), CreateOpts{
		Name:                   "worker-b",
		ServerType:             "cx22",
		Location:               "fsn1",
		Image:                  &hcloud.Image{ID: 99},
		PlacementGroupStrategy: "none",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created == nil || created.ID != 124 {
		t.Fatalf("Create() = %#v", created)
	}
}

func TestProviderCreateReusesExistingPlacementGroup(t *testing.T) {
	var sawLookup, sawCreate atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/placement_groups":
			sawLookup.Store(true)
			writeJSON(t, w, map[string]any{"placement_groups": []any{
				map[string]any{"id": 999, "name": placementGroupName("cluster-a"), "type": "spread", "labels": map[string]string{clusterLabelKey: "cluster-a"}, "servers": []any{}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/placement_groups":
			sawCreate.Store(true)
			http.Error(w, "should not create", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			var body struct {
				PlacementGroup int64 `json:"placement_group"`
			}
			decodeJSON(t, r, &body)
			if body.PlacementGroup != 999 {
				t.Errorf("placement_group = %d", body.PlacementGroup)
			}
			writeJSON(t, w, map[string]any{"server": map[string]any{"id": 500}, "action": map[string]any{"id": 1}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	if _, err := provider.Create(context.Background(), CreateOpts{
		Name: "w", ServerType: "cx22", Location: "fsn1", Image: &hcloud.Image{ID: 1}, PlacementGroupStrategy: "spread",
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !sawLookup.Load() {
		t.Fatal("placement group lookup was not performed")
	}
	if sawCreate.Load() {
		t.Fatal("placement group was created when it already existed")
	}
}

func TestProviderGetMissingServerReturnsNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": map[string]any{"code": "not_found", "message": "not found"}})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	serverObj, err := provider.Get(context.Background(), FormatProviderID(42))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if serverObj != nil {
		t.Fatalf("Get() = %#v, want nil", serverObj)
	}
}

func TestProviderGetInvalidID(t *testing.T) {
	provider := testProvider(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	if _, err := provider.Get(context.Background(), "not-hcloud://1"); err == nil {
		t.Fatal("expected error for invalid provider ID")
	}
}

func TestProviderGetParsesProviderID(t *testing.T) {
	var lastPath atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{"server": map[string]any{"id": 12, "name": "found"}})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	serverObj, err := provider.Get(context.Background(), FormatProviderID(12))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if serverObj == nil || serverObj.ID != 12 {
		t.Fatalf("Get() = %#v", serverObj)
	}
	if lastPath.Load() != "/servers/12" {
		t.Fatalf("Get path = %v", lastPath.Load())
	}
}

func TestProviderListPaginatesWithLabelSelector(t *testing.T) {
	wantSelector := clusterLabelKey + "=cluster-a"
	var sawSelector atomic.Value
	var pageHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/servers" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		sawSelector.Store(r.URL.Query().Get("label_selector"))
		pageHits.Add(1)
		pageParam := r.URL.Query().Get("page")
		if pageParam == "" || pageParam == "1" {
			writeJSON(t, w, map[string]any{
				"servers": []any{
					map[string]any{"id": 1, "labels": map[string]string{clusterLabelKey: "cluster-a"}},
				},
				"meta": map[string]any{"pagination": map[string]any{"page": 1, "next_page": 2, "last_page": 2, "per_page": 50, "total_entries": 2}},
			})
		} else {
			writeJSON(t, w, map[string]any{
				"servers": []any{
					map[string]any{"id": 2, "labels": map[string]string{clusterLabelKey: "cluster-a"}},
				},
				"meta": map[string]any{"pagination": map[string]any{"page": 2, "last_page": 2, "per_page": 50, "total_entries": 2}},
			})
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	servers, err := provider.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := sawSelector.Load(); got != wantSelector {
		t.Fatalf("label_selector = %v, want %q", got, wantSelector)
	}
	if pageHits.Load() != 2 {
		t.Fatalf("page hits = %d, want 2", pageHits.Load())
	}
	if len(servers) != 2 || servers[0].ID != 1 || servers[1].ID != 2 {
		t.Fatalf("List() = %#v", servers)
	}
}

func TestProviderDeleteMissingServerIsIdempotent(t *testing.T) {
	var sawDelete atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/servers/77" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": map[string]any{"code": "not_found"}})
			return
		}
		sawDelete.Store(true)
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()

	provider := testProvider(t, server)
	if err := provider.Delete(context.Background(), FormatProviderID(77)); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if sawDelete.Load() {
		t.Fatal("unexpected delete request was issued")
	}
}

func TestProviderDeleteRefusesForeignServer(t *testing.T) {
	var sawDelete atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/servers/88":
			writeJSON(t, w, map[string]any{"server": map[string]any{"id": 88, "labels": map[string]string{clusterLabelKey: "cluster-other"}}})
		default:
			sawDelete.Store(true)
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	if err := provider.Delete(context.Background(), FormatProviderID(88)); err == nil || !strings.Contains(err.Error(), "not owned by cluster") {
		t.Fatalf("Delete() error = %v, want ownership error", err)
	}
	if sawDelete.Load() {
		t.Fatal("delete must not be issued for foreign server")
	}
}

func TestProviderDeleteOwnedServer(t *testing.T) {
	var sawDelete atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/servers/99":
			if r.Method == http.MethodGet {
				writeJSON(t, w, map[string]any{"server": map[string]any{"id": 99, "labels": map[string]string{clusterLabelKey: "cluster-a"}}})
				return
			}
			if r.Method == http.MethodDelete {
				sawDelete.Store(true)
				writeJSON(t, w, map[string]any{"action": map[string]any{"id": 1, "command": "delete_server"}})
				return
			}
		}
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()

	provider := testProvider(t, server)
	if err := provider.Delete(context.Background(), FormatProviderID(99)); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !sawDelete.Load() {
		t.Fatal("delete request was not issued")
	}
}

func TestProviderDeleteInvalidID(t *testing.T) {
	provider := testProvider(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	if err := provider.Delete(context.Background(), "not-hcloud://1"); err == nil {
		t.Fatal("expected error for invalid provider ID")
	}
}

func testProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	client := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("test-token"),
	)
	provider, err := New(client, "cluster-a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return provider
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func decodeJSON(t *testing.T, r *http.Request, target any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}
