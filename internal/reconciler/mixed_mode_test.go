package reconciler

import (
	"context"
	"testing"

	"gitlab.bluewillows.net/root/dnsweaver/pkg/provider"
	"gitlab.bluewillows.net/root/dnsweaver/pkg/source"
	"gitlab.bluewillows.net/root/dnsweaver/pkg/workload"
	"gitlab.bluewillows.net/root/dnsweaver/sources/traefik"
)

// These tests pin reconciler behavior when a single Docker lister returns
// both Swarm services and standalone containers — i.e. docker_mode=both.
// The reconciler is hostname-keyed (not workload-keyed), so most correctness
// properties fall out of existing behavior; these tests guard those
// properties against regression as the docker layer evolves.

// mixedSetup returns a ready-to-reconcile triplet seeded with one mock
// provider matching *.example.com with target 10.0.0.1.
func mixedSetup(t *testing.T) (*testMockWorkloadLister, *Reconciler, *testMockProvider) {
	t.Helper()
	dockerMock := newTestMockWorkloadLister(workload.PlatformDocker)

	logger := quietLogger()
	sources := source.NewRegistry(logger)
	sources.Register(traefik.New(traefik.WithLogger(logger)))

	mockProvider := newTestMockProvider("test-dns")
	providers := provider.NewRegistry(logger)
	providers.RegisterFactory("mock", func(_ provider.FactoryConfig) (provider.Provider, error) {
		return mockProvider, nil
	})
	if err := providers.CreateInstance(provider.ProviderInstanceConfig{
		Name:       "test-dns",
		TypeName:   "mock",
		RecordType: provider.RecordTypeA,
		Target:     "10.0.0.1",
		TTL:        300,
		Domains:    []string{"*.example.com"},
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	r := New([]workload.Lister{dockerMock}, sources, providers,
		WithConfig(DefaultConfig()),
		WithLogger(logger),
	)
	return dockerMock, r, mockProvider
}

// TestReconcile_MixedMode_ServiceAndContainerSameHostname covers the
// duplicate-hostname path in mixed mode: one service and one standalone
// container both declare the same hostname. Exactly one record is created,
// the duplicate is counted, and the reconciler emits no error.
func TestReconcile_MixedMode_ServiceAndContainerSameHostname(t *testing.T) {
	dockerMock, r, mockProvider := mixedSetup(t)

	dockerMock.AddWorkloadAs("web-svc", workload.KindService, map[string]string{
		"traefik.http.routers.web.rule": "Host(`shared.example.com`)",
	})
	dockerMock.AddWorkloadAs("web-dev", workload.KindContainer, map[string]string{
		"traefik.http.routers.web.rule": "Host(`shared.example.com`)",
	})

	result, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.HostnamesDiscovered != 1 {
		t.Errorf("HostnamesDiscovered = %d, want 1", result.HostnamesDiscovered)
	}
	if result.HostnamesDuplicate != 1 {
		t.Errorf("HostnamesDuplicate = %d, want 1 (service + container declare same hostname)", result.HostnamesDuplicate)
	}

	records := mockProvider.GetCreated()
	primary := countRecords(records, "shared.example.com", provider.RecordTypeA)
	if primary != 1 {
		t.Errorf("expected one A record for shared.example.com, got %d (all creates: %+v)", primary, records)
	}
}

// countRecords counts creates matching hostname and type. Ignores ancillary
// records like ownership TXT markers so tests assert on user-facing records.
func countRecords(records []provider.Record, hostname string, rtype provider.RecordType) int {
	n := 0
	for _, r := range records {
		if r.Hostname == hostname && r.Type == rtype {
			n++
		}
	}
	return n
}

// TestReconcile_MixedMode_SharedHostnameSurvivesPartialRemoval pins the
// key semantic for mixed mode: removing one of two workloads that share a
// hostname must NOT orphan the record. Only removing both should.
func TestReconcile_MixedMode_SharedHostnameSurvivesPartialRemoval(t *testing.T) {
	dockerMock, r, mockProvider := mixedSetup(t)

	dockerMock.AddWorkloadAs("web-svc", workload.KindService, map[string]string{
		"traefik.http.routers.web.rule": "Host(`shared.example.com`)",
	})
	dockerMock.AddWorkloadAs("web-dev", workload.KindContainer, map[string]string{
		"traefik.http.routers.web.rule": "Host(`shared.example.com`)",
	})

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if n := len(mockProvider.GetDeleted()); n != 0 {
		t.Fatalf("unexpected deletes after initial reconcile: %d", n)
	}

	// Remove the standalone container. Service still holds the hostname.
	dockerMock.RemoveWorkloadByName("web-dev")
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if n := len(mockProvider.GetDeleted()); n != 0 {
		t.Errorf("record was orphan-deleted when one source still holds hostname; deletes=%d", n)
	}

	// Remove the service too. Now the hostname should be orphaned.
	dockerMock.RemoveWorkloadByName("web-svc")
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	// The reconciler may emit skip/delete depending on ownership mode; the
	// behavioral assertion we care about is that the hostname disappeared
	// from the known set and a deletion attempt was recorded.
	if n := len(mockProvider.GetDeleted()); n == 0 {
		t.Errorf("expected at least one delete attempt after both sources removed; deletes=%d", n)
	}
}

// TestReconcile_MixedMode_ServiceRemovalOrphansHostname_AnyTaskState pins
// the service-is-source-of-truth property: once a service is removed from
// the workload list, the hostname is orphaned regardless of task-container
// lifecycle state. The docker layer's filterSwarmTaskContainers guarantees
// task-containers never reach the reconciler, so this property depends on
// the filter — the test exercises the post-filter contract by injecting
// only filtered workload sets.
func TestReconcile_MixedMode_ServiceRemovalOrphansHostname_AnyTaskState(t *testing.T) {
	cases := []struct {
		name            string
		taskAfterRemove []string // task-container names "leaked" post-filter (should be 0)
	}{
		{"no task-containers present", nil},
		{"task-container still running (pre-drain) — filter stripped it", nil},
		{"task-container exited — filter stripped it", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dockerMock, r, mockProvider := mixedSetup(t)
			dockerMock.AddWorkloadAs("web-svc", workload.KindService, map[string]string{
				"traefik.http.routers.web.rule": "Host(`web.example.com`)",
			})
			if _, err := r.Reconcile(context.Background()); err != nil {
				t.Fatalf("initial reconcile: %v", err)
			}
			if got := countRecords(mockProvider.GetCreated(), "web.example.com", provider.RecordTypeA); got != 1 {
				t.Fatalf("expected 1 A record created for web.example.com, got %d (all: %+v)", got, mockProvider.GetCreated())
			}

			// Service removed. filterSwarmTaskContainers must have stripped
			// any lingering task-containers; the reconciler sees an empty
			// workload list and must orphan the hostname.
			dockerMock.RemoveWorkloadByName("web-svc")
			for _, leaked := range tc.taskAfterRemove {
				dockerMock.AddWorkloadAs(leaked, workload.KindContainer, nil)
			}

			result, err := r.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("second reconcile: %v", err)
			}
			// Circuit breaker may skip deletion when the orphan ratio
			// exceeds 50% and orphan count > 1. With a single hostname
			// the breaker does NOT engage (orphan_count=1), so we should
			// see a delete attempt.
			if n := len(mockProvider.GetDeleted()); n == 0 {
				t.Errorf("expected delete attempt after service removal; deletes=%d result=%+v", n, result)
			}
		})
	}
}
