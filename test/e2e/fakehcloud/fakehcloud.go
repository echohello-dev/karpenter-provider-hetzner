// Package fakehcloud implements a stateful, in-memory Hetzner Cloud API for
// end-to-end scenario tests. Unlike the per-test httptest handlers used by the
// unit tests, the backend keeps server state: create, get, list and delete
// round-trip through the same in-memory catalog, so multi-step scenarios
// (create → get → drift → delete) observe consistent state between calls.
//
// It is deliberately API-shaped: requests and responses are encoded with the
// hcloud schema types, and list filtering honours the query parameters the
// real API supports (architecture, type, status, label_selector) so provider
// code paths under test exercise the same filtering behaviour they would
// against api.hetzner.cloud.
package fakehcloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
)

// Image is a catalog image (snapshot or system image) the backend serves from
// GET /images.
type Image struct {
	ID           int64
	Description  string
	Architecture string // "x86" or "arm"
	Type         string // "snapshot" or "system"
	Labels       map[string]string
	Created      time.Time
}

// ServerType is a catalog entry for GET /server_types and the pricing
// document. HourlyNet is the net hourly price per location; Locations maps a
// location name to whether the type is currently available there.
type ServerType struct {
	Name         string
	Architecture string // "x86" or "arm"
	Cores        int
	MemoryGB     float32
	DiskGB       int
	HourlyNet    string
	Locations    map[string]bool
}

// Location is an hcloud location as served by GET /locations.
type Location struct {
	ID          int64
	Name        string
	NetworkZone string
}

// Config seeds the backend catalog. Servers start empty and accumulate as
// POST /servers calls arrive.
type Config struct {
	Images        []Image
	ServerTypes   []ServerType
	Locations     []Location
	Networks      map[int64]string // id → name
	Firewalls     map[int64]string // id → name
	SSHKeys       map[int64]string // id → name
	FirstServerID int64            // first allocated server ID (default 4242)
}

// ServerView is a point-in-time snapshot of one fake server for assertions.
type ServerView struct {
	ID          int64
	Name        string
	ServerType  string
	Location    string
	ImageID     int64
	Labels      map[string]string
	NetworkIDs  []int64
	FirewallIDs []int64
	UserData    string
}

type storedServer struct {
	schema   schema.Server
	userData string
}

// Backend is the stateful fake Hetzner Cloud API. Create one per test with
// New, then point an hcloud.Client at URL() (or use Client()).
type Backend struct {
	mu          sync.Mutex
	server      *httptest.Server
	cfg         Config
	servers     map[int64]*storedServer
	pgroups     map[string]*schema.PlacementGroup
	nextID      int64
	nextGroupID int64
}

// New builds a Backend from cfg, starts its httptest server, and returns it.
// Call Close when finished (typically via t.Cleanup).
func New(cfg Config) *Backend {
	if cfg.FirstServerID == 0 {
		cfg.FirstServerID = 4242
	}
	b := &Backend{
		cfg:         cfg,
		servers:     map[int64]*storedServer{},
		pgroups:     map[string]*schema.PlacementGroup{},
		nextID:      cfg.FirstServerID,
		nextGroupID: 700,
	}
	b.server = httptest.NewServer(b)
	return b
}

// URL returns the base URL of the fake API.
func (b *Backend) URL() string { return b.server.URL }

// Close shuts the fake API down.
func (b *Backend) Close() { b.server.Close() }

// Client returns an hcloud.Client wired to this backend with fast retries so
// scenario tests do not sleep on transient-error backoff.
func (b *Backend) Client() *hcloud.Client {
	return hcloud.NewClient(
		hcloud.WithEndpoint(b.URL()),
		hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Microsecond), MaxRetries: 1}),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Microsecond)}),
	)
}

// Servers returns snapshots of all live servers, ordered by ID.
func (b *Backend) Servers() []ServerView {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ServerView, 0, len(b.servers))
	for id := range b.servers {
		out = append(out, b.viewLocked(id))
	}
	// Maps iterate randomly; sort by ID for deterministic assertions.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ServerCount returns the number of live servers.
func (b *Backend) ServerCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.servers)
}

// SetServerImage rewrites a live server's image ID. Scenario tests use it to
// inject drift after a create round-trip.
func (b *Backend) SetServerImage(id, imageID int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	srv, ok := b.servers[id]
	if !ok {
		return fmt.Errorf("fakehcloud: server %d not found", id)
	}
	if srv.schema.Image == nil {
		srv.schema.Image = &schema.Image{}
	}
	srv.schema.Image.ID = imageID
	return nil
}

func (b *Backend) viewLocked(id int64) ServerView {
	srv := b.servers[id]
	view := ServerView{
		ID:       srv.schema.ID,
		Name:     srv.schema.Name,
		UserData: srv.userData,
		Labels:   map[string]string{},
	}
	for k, v := range srv.schema.Labels {
		view.Labels[k] = v
	}
	view.ServerType = srv.schema.ServerType.Name
	view.Location = srv.schema.Location.Name
	if srv.schema.Image != nil {
		view.ImageID = srv.schema.Image.ID
	}
	for _, pn := range srv.schema.PrivateNet {
		view.NetworkIDs = append(view.NetworkIDs, pn.Network)
	}
	for _, fw := range srv.schema.PublicNet.Firewalls {
		view.FirewallIDs = append(view.FirewallIDs, fw.ID)
	}
	return view
}

// ServeHTTP routes hcloud API requests to the in-memory catalog.
func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/images":
		b.handleImages(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/locations":
		b.handleLocations(w)
	case r.Method == http.MethodGet && r.URL.Path == "/server_types":
		b.handleServerTypes(w)
	case r.Method == http.MethodGet && r.URL.Path == "/pricing":
		b.handlePricing(w)
	case r.Method == http.MethodGet && r.URL.Path == "/networks/"+pathID(r):
		b.handleNamed(w, r, "network", b.cfg.Networks)
	case r.Method == http.MethodGet && r.URL.Path == "/firewalls/"+pathID(r):
		b.handleNamed(w, r, "firewall", b.cfg.Firewalls)
	case r.Method == http.MethodGet && r.URL.Path == "/ssh_keys/"+pathID(r):
		b.handleNamed(w, r, "ssh_key", b.cfg.SSHKeys)
	case r.Method == http.MethodGet && r.URL.Path == "/placement_groups":
		b.handlePlacementGroupsList(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/placement_groups":
		b.handlePlacementGroupCreate(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/servers":
		b.handleServerList(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		b.handleServerCreate(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/servers/"):
		b.handleServerGet(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/servers/"):
		b.handleServerDelete(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unsupported route %s %s", r.Method, r.URL.Path))
	}
}

// pathID extracts the trailing numeric path segment ("/networks/12345" → "12345").
func pathID(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(schema.ErrorResponse{
		Error: schema.Error{Code: code, Message: message},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// handleImages serves GET /images with the architecture/type/status/
// label_selector filters the real API applies.
func (b *Backend) handleImages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	labelSelector := q.Get("label_selector")
	out := make([]schema.Image, 0, len(b.cfg.Images))
	for _, img := range b.cfg.Images {
		if arch := q.Get("architecture"); arch != "" && img.Architecture != arch {
			continue
		}
		if typ := q.Get("type"); typ != "" && img.Type != typ {
			continue
		}
		if status := q.Get("status"); status != "" && status != "available" {
			continue
		}
		if labelSelector != "" && !matchLabelSelector(labelSelector, img.Labels) {
			continue
		}
		created := img.Created
		desc := img.Description
		out = append(out, schema.Image{
			ID:           img.ID,
			Description:  desc,
			Type:         img.Type,
			Status:       "available",
			Architecture: img.Architecture,
			Created:      &created,
			Labels:       img.Labels,
		})
	}
	writeJSON(w, struct {
		Images []schema.Image `json:"images"`
		Meta   schema.Meta    `json:"meta"`
	}{Images: out, Meta: paginationMeta(len(out))})
}

func (b *Backend) handleLocations(w http.ResponseWriter) {
	out := make([]schema.Location, 0, len(b.cfg.Locations))
	for _, loc := range b.cfg.Locations {
		out = append(out, schema.Location{ID: loc.ID, Name: loc.Name, NetworkZone: loc.NetworkZone})
	}
	writeJSON(w, struct {
		Locations []schema.Location `json:"locations"`
		Meta      schema.Meta       `json:"meta"`
	}{Locations: out, Meta: paginationMeta(len(out))})
}

func (b *Backend) handleServerTypes(w http.ResponseWriter) {
	out := make([]schema.ServerType, 0, len(b.cfg.ServerTypes))
	for _, st := range b.cfg.ServerTypes {
		locs := make([]schema.ServerTypeLocation, 0, len(st.Locations))
		prices := make([]schema.PricingServerTypePrice, 0, len(st.Locations))
		for loc, avail := range st.Locations {
			locs = append(locs, schema.ServerTypeLocation{Name: loc, Available: avail})
			prices = append(prices, schema.PricingServerTypePrice{
				Location:    loc,
				PriceHourly: schema.Price{Net: st.HourlyNet, Gross: st.HourlyNet},
			})
		}
		out = append(out, schema.ServerType{
			Name:         st.Name,
			Architecture: st.Architecture,
			Cores:        st.Cores,
			Memory:       st.MemoryGB,
			Disk:         st.DiskGB,
			StorageType:  "local",
			CPUType:      "shared",
			Locations:    locs,
			Prices:       prices,
		})
	}
	writeJSON(w, schema.ServerTypeListResponse{ServerTypes: out})
}

// handlePricing serves GET /pricing in the exact shape the pricing provider
// consumes: net hourly per (server type, location) plus the ipv4 primary-IP
// surcharge (zeroed so prices stay a single comparable figure).
func (b *Backend) handlePricing(w http.ResponseWriter) {
	type price struct {
		Net   string `json:"net"`
		Gross string `json:"gross"`
	}
	type locPrice struct {
		Location    string `json:"location"`
		PriceHourly price  `json:"price_hourly"`
	}
	type primaryIP struct {
		Type   string     `json:"type"`
		Prices []locPrice `json:"prices"`
	}
	type serverTypePrice struct {
		Name   string     `json:"name"`
		Prices []locPrice `json:"prices"`
	}
	body := struct {
		Pricing struct {
			Currency    string            `json:"currency"`
			PrimaryIPs  []primaryIP       `json:"primary_ips"`
			FloatingIPs []map[string]any  `json:"floating_ips"`
			ServerTypes []serverTypePrice `json:"server_types"`
		} `json:"pricing"`
	}{}
	body.Pricing.Currency = "EUR"
	body.Pricing.PrimaryIPs = []primaryIP{{Type: "ipv4", Prices: []locPrice{{Location: "fsn1", PriceHourly: price{Net: "0", Gross: "0"}}}}}
	body.Pricing.FloatingIPs = []map[string]any{{"type": "ipv4"}}
	for _, st := range b.cfg.ServerTypes {
		entry := serverTypePrice{Name: st.Name}
		for loc := range st.Locations {
			entry.Prices = append(entry.Prices, locPrice{Location: loc, PriceHourly: price{Net: st.HourlyNet, Gross: st.HourlyNet}})
		}
		body.Pricing.ServerTypes = append(body.Pricing.ServerTypes, entry)
	}
	writeJSON(w, body)
}

// handleNamed serves the single-resource GETs (networks, firewalls, ssh_keys)
// keyed by ID. The response wrapper differs per resource ("network",
// "firewall", "ssh_key"), so the key is passed in by the route.
func (b *Backend) handleNamed(w http.ResponseWriter, r *http.Request, key string, names map[int64]string) {
	id, err := strconv.ParseInt(pathID(r), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "malformed resource id")
		return
	}
	name, ok := names[id]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("resource %d not found", id))
		return
	}
	writeJSON(w, map[string]any{key: map[string]any{"id": id, "name": name}})
}

func (b *Backend) handlePlacementGroupsList(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	out := []schema.PlacementGroup{}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, pg := range b.pgroups {
		if name != "" && pg.Name != name {
			continue
		}
		out = append(out, *pg)
	}
	writeJSON(w, schema.PlacementGroupListResponse{PlacementGroups: out})
}

func (b *Backend) handlePlacementGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req schema.PlacementGroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pgroups[req.Name]; exists {
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf("placement group %q already exists", req.Name))
		return
	}
	pg := &schema.PlacementGroup{
		ID:      b.nextGroupID,
		Name:    req.Name,
		Labels:  map[string]string{},
		Created: time.Now().UTC(),
		Servers: []int64{},
		Type:    req.Type,
	}
	if req.Labels != nil {
		pg.Labels = *req.Labels
	}
	b.nextGroupID++
	b.pgroups[req.Name] = pg
	writeJSON(w, schema.PlacementGroupCreateResponse{PlacementGroup: *pg})
}

func (b *Backend) handleServerList(w http.ResponseWriter, r *http.Request) {
	selector := r.URL.Query().Get("label_selector")
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []schema.Server{}
	for id := range b.servers {
		srv := b.servers[id]
		if selector != "" && !matchLabelSelector(selector, srv.schema.Labels) {
			continue
		}
		out = append(out, srv.schema)
	}
	writeJSON(w, schema.ServerListResponse{Servers: out})
}

func (b *Backend) handleServerCreate(w http.ResponseWriter, r *http.Request) {
	var req schema.ServerCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	typeName := req.ServerType.Name
	if req.ServerType.ID != 0 {
		typeName = fmt.Sprintf("%d", req.ServerType.ID)
	}
	var typeFound *ServerType
	for i := range b.cfg.ServerTypes {
		if b.cfg.ServerTypes[i].Name == typeName {
			typeFound = &b.cfg.ServerTypes[i]
			break
		}
	}
	if typeFound == nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", fmt.Sprintf("server type %q not found", typeName))
		return
	}

	imageID := req.Image.ID
	if imageID == 0 {
		for _, img := range b.cfg.Images {
			if img.Description == req.Image.Name {
				imageID = img.ID
				break
			}
		}
	}
	if imageID == 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", fmt.Sprintf("image %q not found", req.Image.Name))
		return
	}
	imageArch := "x86"
	for _, img := range b.cfg.Images {
		if img.ID == imageID {
			imageArch = img.Architecture
			break
		}
	}

	id := b.nextID
	b.nextID++

	labels := map[string]string{}
	if req.Labels != nil {
		labels = *req.Labels
	}
	srv := &storedServer{
		userData: req.UserData,
		schema: schema.Server{
			ID:         id,
			Name:       req.Name,
			Status:     "running",
			Created:    time.Now().UTC(),
			ServerType: schema.ServerType{Name: typeFound.Name, Architecture: typeFound.Architecture, Cores: typeFound.Cores, Memory: typeFound.MemoryGB, Disk: typeFound.DiskGB},
			Location:   schema.Location{Name: req.Location},
			Image:      &schema.Image{ID: imageID, Architecture: imageArch},
			Labels:     labels,
		},
	}
	for _, netID := range req.Networks {
		srv.schema.PrivateNet = append(srv.schema.PrivateNet, schema.ServerPrivateNet{Network: netID, IP: "10.0.0.2"})
	}
	for _, fw := range req.Firewalls {
		srv.schema.PublicNet.Firewalls = append(srv.schema.PublicNet.Firewalls, schema.ServerFirewall{ID: fw.Firewall, Status: "applied"})
	}
	if req.PublicNet != nil && req.PublicNet.EnableIPv4 {
		srv.schema.PublicNet.IPv4 = schema.ServerPublicNetIPv4{IP: "1.2.3.4"}
	}
	if req.PublicNet != nil && req.PublicNet.EnableIPv6 {
		srv.schema.PublicNet.IPv6 = schema.ServerPublicNetIPv6{IP: "::1"}
	}
	b.servers[id] = srv

	writeJSON(w, schema.ServerCreateResponse{
		Server: srv.schema,
		Action: schema.Action{ID: 1, Command: "create_server", Status: "running", Started: time.Now().UTC()},
	})
}

func (b *Backend) handleServerGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(pathID(r), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "malformed server id")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	srv, ok := b.servers[id]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("server %d not found", id))
		return
	}
	writeJSON(w, schema.ServerGetResponse{Server: srv.schema})
}

func (b *Backend) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(pathID(r), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "malformed server id")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.servers[id]; !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("server %d not found", id))
		return
	}
	delete(b.servers, id)
	writeJSON(w, schema.ServerDeleteResponse{
		Action: schema.Action{ID: 2, Command: "delete_server", Status: "success", Started: time.Now().UTC()},
	})
}

func paginationMeta(n int) schema.Meta {
	return schema.Meta{Pagination: &schema.MetaPagination{Page: 1, LastPage: 1, PerPage: n, TotalEntries: n}}
}

// matchLabelSelector evaluates a comma-separated key=value selector with AND
// semantics — the only form the provider issues (cluster scoping and image
// label pinning).
func matchLabelSelector(selector string, labels map[string]string) bool {
	for _, pair := range strings.Split(selector, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return false
		}
		if labels[key] != value {
			return false
		}
	}
	return true
}
