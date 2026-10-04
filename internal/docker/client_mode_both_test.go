package docker

import (
	"context"
	"testing"
)

// TestWithMode_Both covers the ModeBoth option setter.
func TestWithMode_Both(t *testing.T) {
	opt := WithMode(ModeBoth)
	c := &Client{}
	opt(c)
	if c.mode != ModeBoth {
		t.Errorf("WithMode(ModeBoth) set mode = %q, want %q", c.mode, ModeBoth)
	}
}

// TestIsSwarm_BothMode pins that IsSwarm returns false in ModeBoth.
// IsSwarm is a question about listing semantics, not local Swarm presence;
// IsSwarmActive covers the latter.
func TestIsSwarm_BothMode(t *testing.T) {
	c := &Client{detectedMode: ModeBoth, swarmActive: true}
	if c.IsSwarm() {
		t.Error("IsSwarm() should return false in ModeBoth")
	}
	if !c.IsSwarmActive() {
		t.Error("IsSwarmActive() should return true when swarmActive is set")
	}
}

// TestIsSwarmActive covers every combination of mode and swarmActive.
func TestIsSwarmActive(t *testing.T) {
	tests := []struct {
		name        string
		swarmActive bool
		mode        Mode
		want        bool
	}{
		{"swarm-active + ModeSwarm", true, ModeSwarm, true},
		{"swarm-active + ModeBoth", true, ModeBoth, true},
		{"swarm-active + ModeStandalone (manager running standalone)", true, ModeStandalone, true},
		{"swarm-inactive + ModeStandalone", false, ModeStandalone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{detectedMode: tt.mode, swarmActive: tt.swarmActive}
			if got := c.IsSwarmActive(); got != tt.want {
				t.Errorf("IsSwarmActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Guard acceptance for ModeBoth (ListServices, ListContainers,
// GetServiceLabels, GetContainerLabels) is covered end-to-end via the
// fakeDaemon-backed tests in client_fakeserver_test.go — those exercise
// the real Docker SDK through the guard and into the HTTP round-trip.

// TestFilterSwarmTaskContainers is the correctness test for the filter that
// prevents service/task double-counting. Cases exercise every combination
// of Swarm labels so future changes can't silently alter the semantic.
func TestFilterSwarmTaskContainers(t *testing.T) {
	tests := []struct {
		name string
		in   []Container
		want []string // container IDs expected in output, in order
	}{
		{
			name: "empty input",
			in:   nil,
			want: nil,
		},
		{
			name: "no swarm labels — all pass through",
			in: []Container{
				{ID: "a", Labels: map[string]string{"app": "web"}},
				{ID: "b", Labels: nil},
			},
			want: []string{"a", "b"},
		},
		{
			name: "only task.id (no service.id) — passes through by design",
			in: []Container{
				{ID: "orphan-task", Labels: map[string]string{
					"com.docker.swarm.task.id": "t-1",
				}},
			},
			want: []string{"orphan-task"},
		},
		{
			name: "only service.id — filtered",
			in: []Container{
				{ID: "task-1", Labels: map[string]string{
					"com.docker.swarm.service.id": "s-1",
				}},
			},
			want: nil,
		},
		{
			name: "both swarm labels — filtered",
			in: []Container{
				{ID: "task-2", Labels: map[string]string{
					"com.docker.swarm.task.id":    "t-2",
					"com.docker.swarm.service.id": "s-2",
				}},
			},
			want: nil,
		},
		{
			name: "mixed — standalone preserved, tasks stripped, order kept",
			in: []Container{
				{ID: "standalone-1", Labels: map[string]string{"app": "db"}},
				{ID: "task-a", Labels: map[string]string{"com.docker.swarm.service.id": "s"}},
				{ID: "standalone-2", Labels: map[string]string{"app": "cache"}},
				{ID: "task-b", Labels: map[string]string{"com.docker.swarm.service.id": "s"}},
			},
			want: []string{"standalone-1", "standalone-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterSwarmTaskContainers(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d containers, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, id := range tt.want {
				if got[i].ID != id {
					t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

// TestComposeWorkloads covers the ListWorkloads composition path:
// service-first ordering, task filtering gated on the filterTasks flag, and
// correct Workload.Type assignment.
func TestComposeWorkloads(t *testing.T) {
	services := []Service{
		{ID: "svc-1", Name: "web", Labels: map[string]string{"app": "web"}},
		{ID: "svc-2", Name: "api"},
	}
	containers := []Container{
		{ID: "ctr-standalone", Name: "tools"},
		{ID: "ctr-task", Name: "web.1.abc", Labels: map[string]string{
			"com.docker.swarm.service.id": "svc-1",
		}},
	}

	t.Run("filterTasks=true strips task-containers and puts services first", func(t *testing.T) {
		got := composeWorkloads(services, containers, true)
		wantIDs := []string{"svc-1", "svc-2", "ctr-standalone"}
		if len(got) != len(wantIDs) {
			t.Fatalf("got %d workloads, want %d: %+v", len(got), len(wantIDs), got)
		}
		for i, id := range wantIDs {
			if got[i].ID != id {
				t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, id)
			}
		}
		if got[0].Type != WorkloadTypeService || got[2].Type != WorkloadTypeContainer {
			t.Errorf("unexpected Type assignment: %+v", got)
		}
	})

	t.Run("filterTasks=false keeps everything (documents standalone-non-manager path)", func(t *testing.T) {
		got := composeWorkloads(services, containers, false)
		if len(got) != 4 {
			t.Fatalf("got %d workloads, want 4", len(got))
		}
	})

	t.Run("services-only yields no containers", func(t *testing.T) {
		got := composeWorkloads(services, nil, true)
		if len(got) != 2 {
			t.Fatalf("got %d workloads, want 2", len(got))
		}
		for _, w := range got {
			if w.Type != WorkloadTypeService {
				t.Errorf("expected all WorkloadTypeService, got %q", w.Type)
			}
		}
	})

	t.Run("containers-only with filterTasks=true strips tasks (ModeStandalone-on-manager)", func(t *testing.T) {
		got := composeWorkloads(nil, containers, true)
		if len(got) != 1 || got[0].ID != "ctr-standalone" {
			t.Fatalf("expected single standalone container, got %+v", got)
		}
	})
}

// TestGetWorkloadLabels_UninitializedModeErrors verifies an uninitialized
// detectedMode (a programmer error) surfaces as an error instead of a
// misleading nil map, without crashing the process from a watcher goroutine.
func TestGetWorkloadLabels_UninitializedModeErrors(t *testing.T) {
	c := &Client{detectedMode: Mode("")}
	labels, err := c.GetWorkloadLabels(context.Background(), "x")
	if err == nil {
		t.Fatalf("expected error on uninitialized detectedMode, got labels %+v", labels)
	}
}
