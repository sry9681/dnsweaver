package docker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

// fakeDaemon provides configurable Docker API responses for tests that need
// to exercise the real docker SDK. Each scenario constructs a daemon with
// the desired state, then wraps it in a Client. This lets us test
// ListWorkloads, GetWorkloadLabels, and the Swarm task-container filter
// end-to-end without needing a live Docker instance.
type fakeDaemon struct {
	info       system.Info
	services   []swarm.Service // indexed by ID for inspect
	containers []container.Summary

	// Single-inspect stubs keyed by ID.
	serviceByID   map[string]swarm.Service
	containerByID map[string]container.InspectResponse

	// Failure injection for fail-closed testing.
	failServices   bool
	failContainers bool
}

func (f *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()

	// Ping — docker SDK sanity check.
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Api-Version", "1.44")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Mirror under /v1.44 to match pinned API version requests.
	prefixes := []string{"", "/v1.44"}

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	for _, p := range prefixes {
		mux.HandleFunc(p+"/info", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, f.info)
		})
		mux.HandleFunc(p+"/services", func(w http.ResponseWriter, _ *http.Request) {
			if f.failServices {
				http.Error(w, "injected failure", http.StatusInternalServerError)
				return
			}
			writeJSON(w, f.services)
		})
		mux.HandleFunc(p+"/containers/json", func(w http.ResponseWriter, _ *http.Request) {
			if f.failContainers {
				http.Error(w, "injected failure", http.StatusInternalServerError)
				return
			}
			writeJSON(w, f.containers)
		})
	}

	// /services/{id} and /containers/{id}/json — serve via prefix match.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Strip optional /v1.44 prefix
		path := r.URL.Path
		path = strings.TrimPrefix(path, "/v1.44")

		switch {
		case strings.HasPrefix(path, "/services/"):
			if f.failServices {
				http.Error(w, "simulated services failure", http.StatusInternalServerError)
				return
			}
			id := strings.TrimPrefix(path, "/services/")
			if svc, ok := f.serviceByID[id]; ok {
				writeJSON(w, svc)
				return
			}
			http.Error(w, "service not found", http.StatusNotFound)
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.TrimSuffix(id, "/json")
			if ctr, ok := f.containerByID[id]; ok {
				writeJSON(w, ctr)
				return
			}
			http.Error(w, "container not found", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	})

	return mux
}

// newFakeClient wires a fakeDaemon into a real docker SDK client and returns
// a DNSWeaver *Client with its fields populated. detectedMode and
// swarmActive match what initializeMode would infer from info.
func newFakeClient(t *testing.T, f *fakeDaemon, mode Mode) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	sdk, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+u.Host),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatalf("construct sdk client: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() })

	c := &Client{
		docker:        sdk,
		mode:          mode,
		logger:        slog.Default(),
		cleanupOnStop: true,
	}
	if err := c.initializeMode(context.Background()); err != nil {
		t.Fatalf("initializeMode: %v", err)
	}
	return c, srv
}

// swarmActiveInfo builds a system.Info that looks like an active Swarm
// manager to initializeMode.
func swarmActiveInfo() system.Info {
	return system.Info{
		Swarm: swarm.Info{
			LocalNodeState:   swarm.LocalNodeStateActive,
			ControlAvailable: true,
			NodeID:           "test-node",
		},
	}
}

// standaloneInfo builds a system.Info that looks like a non-Swarm host.
func standaloneInfo() system.Info {
	return system.Info{
		Swarm: swarm.Info{
			LocalNodeState: swarm.LocalNodeStateInactive,
		},
	}
}

// TestModeBoth_RequiresManager verifies initializeMode rejects ModeBoth on a
// non-Swarm host.
func TestModeBoth_RequiresSwarmActive(t *testing.T) {
	f := &fakeDaemon{info: standaloneInfo()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	sdk, _ := client.NewClientWithOpts(client.WithHost("tcp://"+u.Host), client.WithVersion("1.44"))
	defer sdk.Close()

	c := &Client{docker: sdk, mode: ModeBoth, logger: slog.Default()}
	err := c.initializeMode(context.Background())
	if !errors.Is(err, ErrSwarmNotActive) {
		t.Errorf("ModeBoth on non-Swarm host: got %v, want ErrSwarmNotActive", err)
	}
}

// TestModeBoth_RequiresManagerRole verifies initializeMode rejects ModeBoth
// when Swarm is active but local node is a worker.
func TestModeBoth_RequiresManagerRole(t *testing.T) {
	info := swarmActiveInfo()
	info.Swarm.ControlAvailable = false // worker
	f := &fakeDaemon{info: info}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	sdk, _ := client.NewClientWithOpts(client.WithHost("tcp://"+u.Host), client.WithVersion("1.44"))
	defer sdk.Close()

	c := &Client{docker: sdk, mode: ModeBoth, logger: slog.Default()}
	err := c.initializeMode(context.Background())
	if !errors.Is(err, ErrNotManager) {
		t.Errorf("ModeBoth on Swarm worker: got %v, want ErrNotManager", err)
	}
}

// TestListWorkloads_ModeBoth_MergesAndFilters exercises the critical path:
// on a manager, services and standalone containers both appear, but Swarm
// task-containers are filtered out so they don't double-count.
func TestListWorkloads_ModeBoth_MergesAndFilters(t *testing.T) {
	f := &fakeDaemon{
		info: swarmActiveInfo(),
		services: []swarm.Service{
			{ID: "svc-web", Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name:   "web",
					Labels: map[string]string{"dnsweaver.hostname": "web.example.com"},
				},
			}},
		},
		containers: []container.Summary{
			{
				ID:     "standalone-tools",
				Names:  []string{"/tools"},
				Labels: map[string]string{"dnsweaver.hostname": "tools.example.com"},
			},
			{
				ID:    "task-web-1",
				Names: []string{"/web.1.xyz"},
				Labels: map[string]string{
					"com.docker.swarm.service.id": "svc-web",
					"dnsweaver.hostname":          "web.example.com", // inherited
				},
			},
		},
	}
	c, _ := newFakeClient(t, f, ModeBoth)

	workloads, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}

	if len(workloads) != 2 {
		t.Fatalf("expected 2 workloads (1 service + 1 standalone), got %d: %+v", len(workloads), workloads)
	}
	if workloads[0].Type != WorkloadTypeService || workloads[0].ID != "svc-web" {
		t.Errorf("workloads[0] = %+v, want service svc-web", workloads[0])
	}
	if workloads[1].Type != WorkloadTypeContainer || workloads[1].ID != "standalone-tools" {
		t.Errorf("workloads[1] = %+v, want container standalone-tools", workloads[1])
	}
	// Explicit negative assertion: the task-container must NOT appear.
	for _, w := range workloads {
		if w.ID == "task-web-1" {
			t.Fatalf("Swarm task-container leaked into workload list: %+v", w)
		}
	}
}

// TestListWorkloads_StandaloneOnManager_FiltersTaskContainers pins the
// pre-existing bug fix: running ModeStandalone on a Swarm manager must also
// filter out task-containers so they don't get DNS records of their own.
func TestListWorkloads_StandaloneOnManager_FiltersTaskContainers(t *testing.T) {
	f := &fakeDaemon{
		info: swarmActiveInfo(),
		containers: []container.Summary{
			{ID: "standalone-db", Names: []string{"/db"}},
			{ID: "task-api-1", Names: []string{"/api.1.abc"}, Labels: map[string]string{
				"com.docker.swarm.service.id": "svc-api",
			}},
		},
	}
	c, _ := newFakeClient(t, f, ModeStandalone)
	if !c.IsSwarmActive() {
		t.Fatal("expected IsSwarmActive() to be true for a manager running ModeStandalone")
	}

	workloads, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(workloads) != 1 || workloads[0].ID != "standalone-db" {
		t.Fatalf("expected only standalone-db, got %+v", workloads)
	}
}

// TestListWorkloads_ModeBoth_FailClosed_Services verifies that a services
// listing failure aborts the whole call. Returning a partial result would
// let the reconciler see "no services" and mass-delete their DNS records.
func TestListWorkloads_ModeBoth_FailClosed_Services(t *testing.T) {
	f := &fakeDaemon{
		info:         swarmActiveInfo(),
		failServices: true,
		containers:   []container.Summary{{ID: "c1", Names: []string{"/c1"}}},
	}
	c, _ := newFakeClient(t, f, ModeBoth)
	if _, err := c.ListWorkloads(context.Background()); err == nil {
		t.Fatal("expected error when service listing fails in ModeBoth; got nil")
	}
}

// TestListWorkloads_ModeBoth_FailClosed_Containers verifies symmetric
// failure behavior for the containers-side listing.
func TestListWorkloads_ModeBoth_FailClosed_Containers(t *testing.T) {
	f := &fakeDaemon{
		info: swarmActiveInfo(),
		services: []swarm.Service{
			{ID: "svc-1", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "a"}}},
		},
		failContainers: true,
	}
	c, _ := newFakeClient(t, f, ModeBoth)
	if _, err := c.ListWorkloads(context.Background()); err == nil {
		t.Fatal("expected error when container listing fails in ModeBoth; got nil")
	}
}

// TestGetWorkloadLabels_ModeBoth_FallsBackToContainer verifies the fallback:
// if the ID isn't a service (404), try the container side. This pins the
// reviewer's caught bug — the container guard must accept ModeBoth.
func TestGetWorkloadLabels_ModeBoth_FallsBackToContainer(t *testing.T) {
	f := &fakeDaemon{
		info:        swarmActiveInfo(),
		serviceByID: map[string]swarm.Service{
			// intentionally empty — all service lookups 404
		},
		containerByID: map[string]container.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &container.ContainerJSONBase{ID: "ctr-1"},
				Config: &container.Config{Labels: map[string]string{
					"dnsweaver.hostname": "tools.example.com",
				}},
			},
		},
	}
	c, _ := newFakeClient(t, f, ModeBoth)

	labels, err := c.GetWorkloadLabels(context.Background(), "ctr-1")
	if err != nil {
		t.Fatalf("GetWorkloadLabels: %v", err)
	}
	if labels["dnsweaver.hostname"] != "tools.example.com" {
		t.Errorf("labels = %+v, want dnsweaver.hostname=tools.example.com", labels)
	}
}

// TestGetWorkloadLabels_ModeBoth_ServiceErrorNotMasked verifies the
// fallback only fires on not-found: a failing service lookup must surface its
// own error rather than a misleading container lookup result.
func TestGetWorkloadLabels_ModeBoth_ServiceErrorNotMasked(t *testing.T) {
	f := &fakeDaemon{
		info:         swarmActiveInfo(),
		failServices: true,
		containerByID: map[string]container.InspectResponse{
			"svc-1": {
				ContainerJSONBase: &container.ContainerJSONBase{ID: "svc-1"},
				Config:            &container.Config{Labels: map[string]string{"dnsweaver.hostname": "wrong.example.com"}},
			},
		},
	}
	c, _ := newFakeClient(t, f, ModeBoth)

	labels, err := c.GetWorkloadLabels(context.Background(), "svc-1")
	if err == nil {
		t.Fatalf("expected service lookup error, got labels %+v", labels)
	}
	if !strings.Contains(err.Error(), "inspecting service") {
		t.Errorf("error = %v, want the service inspect error", err)
	}
}

// TestGetWorkloadLabels_ModeBoth_PrefersService verifies the fast path: if
// the ID matches a service, we don't waste a container inspect call.
func TestGetWorkloadLabels_ModeBoth_PrefersService(t *testing.T) {
	f := &fakeDaemon{
		info: swarmActiveInfo(),
		serviceByID: map[string]swarm.Service{
			"svc-1": {
				ID: "svc-1",
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web",
						Labels: map[string]string{"dnsweaver.hostname": "web.example.com"},
					},
				},
			},
		},
	}
	c, _ := newFakeClient(t, f, ModeBoth)

	labels, err := c.GetWorkloadLabels(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("GetWorkloadLabels: %v", err)
	}
	if labels["dnsweaver.hostname"] != "web.example.com" {
		t.Errorf("labels = %+v, want dnsweaver.hostname=web.example.com", labels)
	}
}
