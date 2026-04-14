// Package watcher implements Docker event watching for real-time DNS updates.
//
// The watcher monitors Docker events (container/service start, stop, die, etc.)
// and triggers reconciliation when workloads change. It supports both Docker
// Swarm services and standalone containers.
//
// Key features:
//   - Event filtering (only watches relevant events)
//   - Debouncing for rapid events
//   - Graceful shutdown with context cancellation
//   - Automatic reconnection on Docker socket errors
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"github.com/maxfield-allison/dnsweaver/internal/docker"
	"github.com/maxfield-allison/dnsweaver/internal/metrics"
)

// ReconcileFunc is called when changes are detected that require reconciliation.
type ReconcileFunc func()

// Config holds watcher configuration.
type Config struct {
	// DebounceInterval is the time to wait for additional events before triggering
	// reconciliation. This prevents rapid-fire reconciliations during deployments.
	// Default: 2 seconds
	DebounceInterval time.Duration

	// ReconnectInterval is the time to wait before reconnecting after an error.
	// Default: 5 seconds
	ReconnectInterval time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DebounceInterval:  2 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}
}

// Watcher monitors Docker events and triggers reconciliation on container changes.
type Watcher struct {
	dockerClient *docker.Client
	onReconcile  ReconcileFunc
	config       Config
	logger       *slog.Logger

	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	debounce *time.Timer
}

// Option is a functional option for configuring the Watcher.
type Option func(*Watcher)

// WithConfig sets the watcher configuration.
func WithConfig(cfg Config) Option {
	return func(w *Watcher) {
		w.config = cfg
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Watcher) {
		if logger != nil {
			w.logger = logger
		}
	}
}

// New creates a new Docker event watcher.
func New(dockerClient *docker.Client, onReconcile ReconcileFunc, opts ...Option) *Watcher {
	w := &Watcher{
		dockerClient: dockerClient,
		onReconcile:  onReconcile,
		config:       DefaultConfig(),
		logger:       slog.Default(),
	}

	for _, opt := range opts {
		opt(w)
	}

	return w
}

// Start begins watching Docker events.
// This method is non-blocking — it starts a goroutine and returns immediately.
// Call Stop() to halt watching.
func (w *Watcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}

	ctx, w.cancel = context.WithCancel(ctx)
	w.running = true
	w.mu.Unlock()

	go w.watchLoop(ctx)

	w.logger.Info("docker event watcher started",
		slog.Duration("debounce", w.config.DebounceInterval),
	)

	return nil
}

// Stop halts the event watcher.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}

	if w.debounce != nil {
		w.debounce.Stop()
		w.debounce = nil
	}

	w.running = false
	w.logger.Info("docker event watcher stopped")
}

// IsRunning returns whether the watcher is currently running.
func (w *Watcher) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *Watcher) watchLoop(ctx context.Context) {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := w.watch(ctx); err != nil {
				if ctx.Err() != nil {
					// Context canceled, exit cleanly
					return
				}
				w.logger.Warn("event stream error, reconnecting",
					slog.String("error", err.Error()),
					slog.Duration("retry_in", w.config.ReconnectInterval),
				)
				metrics.DockerWatcherReconnects.Inc()
				// Use select for context-aware sleep so shutdown isn't blocked
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.config.ReconnectInterval):
				}
			}
		}
	}
}

// watch dispatches to the appropriate stream topology based on mode.
// ModeBoth uses two independent subscriptions (watchBoth) to sidestep
// Docker's inconsistent cross-type action-filter semantics across daemon
// and socket-proxy versions.
func (w *Watcher) watch(ctx context.Context) error {
	mode := w.dockerClient.Mode()
	if mode == docker.ModeBoth {
		return w.watchBoth(ctx)
	}
	return w.watchSingle(ctx, mode)
}

// watchSingle subscribes to a single event stream filtered for one mode.
func (w *Watcher) watchSingle(ctx context.Context, mode docker.Mode) error {
	rawClient := w.dockerClient.RawClient()
	filterArgs := w.buildEventFilters(mode)

	w.logger.Debug("subscribing to docker events",
		slog.String("mode", mode.String()),
		slog.Any("filters", filterArgs),
	)

	eventsChan, errChan := rawClient.Events(ctx, events.ListOptions{
		Filters: filterArgs,
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errChan:
			return err

		case event := <-eventsChan:
			w.handleEvent(event)
		}
	}
}

// watchBoth subscribes to service and container event streams independently
// and merges them into a single dispatch loop.
//
// Why two streams instead of one combined filter: Docker's events endpoint
// applies the action filter globally across all requested types. With
// {type=service,container, event=create,update,remove,start,stop,die,destroy}
// some daemon/proxy combinations cross-product the filter and drop events
// whose (type, action) pair isn't valid — e.g. service "start" or container
// "update" — but others silently deliver nothing. Running two streams with
// narrow, type-specific filters is unambiguous and works identically across
// daemon versions.
//
// Reconnect contract: if either stream errors, we tear both down (via the
// derived ctx's cancel) and return the error. watchLoop then handles the
// reconnect delay and re-enters watch(), which re-establishes both streams.
//
// Task-container churn: the container stream will receive start/stop/die/
// destroy events for Swarm task-containers during rolling updates. These
// pass through handleEvent and trigger a debounced reconcile, but
// ListWorkloads filters task-containers out, so the reconcile is a no-op.
// The debouncer (default 2s) absorbs bursts; on very large clusters with
// frequent deployments, operators can raise DebounceInterval.
func (w *Watcher) watchBoth(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rawClient := w.dockerClient.RawClient()

	w.logger.Debug("subscribing to docker events (both streams)",
		slog.String("mode", docker.ModeBoth.String()),
	)

	svcEv, svcErr := rawClient.Events(ctx, events.ListOptions{
		Filters: w.buildEventFilters(docker.ModeSwarm),
	})
	ctrEv, ctrErr := rawClient.Events(ctx, events.ListOptions{
		Filters: w.buildEventFilters(docker.ModeStandalone),
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-svcErr:
			return fmt.Errorf("service event stream: %w", err)

		case err := <-ctrErr:
			return fmt.Errorf("container event stream: %w", err)

		case ev := <-svcEv:
			w.handleEvent(ev)

		case ev := <-ctrEv:
			w.handleEvent(ev)
		}
	}
}

// buildEventFilters returns the event filter set for a single-stream mode.
// ModeBoth is intentionally not handled here — it uses two single-stream
// subscriptions composed in watchBoth.
func (w *Watcher) buildEventFilters(mode docker.Mode) filters.Args {
	filterArgs := filters.NewArgs()

	switch mode {
	case docker.ModeSwarm:
		filterArgs.Add("type", string(events.ServiceEventType))
		filterArgs.Add("event", "create")
		filterArgs.Add("event", "update")
		filterArgs.Add("event", "remove")
	case docker.ModeStandalone:
		filterArgs.Add("type", string(events.ContainerEventType))
		filterArgs.Add("event", "start")
		filterArgs.Add("event", "stop")
		filterArgs.Add("event", "die")
		filterArgs.Add("event", "destroy")
	}
	return filterArgs
}

func (w *Watcher) handleEvent(event events.Message) {
	w.logger.Debug("received docker event",
		slog.String("type", string(event.Type)),
		slog.String("action", string(event.Action)),
		slog.String("actor_id", event.Actor.ID),
		slog.Any("attributes", event.Actor.Attributes),
	)
	metrics.DockerEventsProcessed.WithLabelValues(string(event.Type) + "_" + string(event.Action)).Inc()

	// Debounce: reset timer on each event
	w.mu.Lock()
	if w.debounce != nil {
		w.debounce.Stop()
	}
	w.debounce = time.AfterFunc(w.config.DebounceInterval, func() {
		w.triggerReconcile()
	})
	w.mu.Unlock()
}

func (w *Watcher) triggerReconcile() {
	w.logger.Info("triggering reconciliation due to docker event")
	if w.onReconcile != nil {
		w.onReconcile()
	}
}

// TriggerNow immediately triggers reconciliation, bypassing debounce.
// Useful for initial reconciliation at startup.
func (w *Watcher) TriggerNow() {
	w.mu.Lock()
	if w.debounce != nil {
		w.debounce.Stop()
		w.debounce = nil
	}
	w.mu.Unlock()

	w.triggerReconcile()
}

// EventStats holds statistics about events processed.
type EventStats struct {
	EventsReceived   int64
	ReconcilesCalled int64
	LastEventTime    time.Time
	LastReconcile    time.Time
}

// MockWatcher is a test double for the Watcher.
// It records reconciliation triggers for verification.
type MockWatcher struct {
	mu             sync.Mutex
	triggers       int
	onTrigger      func()
	running        bool
	SimulatedError error
}

// NewMockWatcher creates a new mock watcher for testing.
func NewMockWatcher() *MockWatcher {
	return &MockWatcher{}
}

// Start implements the Start method for testing.
func (m *MockWatcher) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.SimulatedError != nil {
		return m.SimulatedError
	}

	m.running = true
	return nil
}

// Stop implements the Stop method for testing.
func (m *MockWatcher) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
}

// IsRunning returns whether the mock watcher is running.
func (m *MockWatcher) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// SimulateEvent simulates a Docker event for testing.
func (m *MockWatcher) SimulateEvent() {
	m.mu.Lock()
	m.triggers++
	if m.onTrigger != nil {
		m.onTrigger()
	}
	m.mu.Unlock()
}

// TriggerCount returns the number of reconciliation triggers.
func (m *MockWatcher) TriggerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.triggers
}

// OnTrigger sets a callback for when reconciliation is triggered.
func (m *MockWatcher) OnTrigger(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTrigger = fn
}
