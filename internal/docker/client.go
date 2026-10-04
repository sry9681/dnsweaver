// Package docker provides a client for interacting with Docker in both standalone and Swarm modes.
//
// The client abstracts away the differences between Docker Swarm services and standalone
// containers, providing a unified Workload type that the reconciler can use without
// knowing which mode Docker is running in.
//
// Mode Detection:
//   - ModeAuto: Auto-detect based on Docker daemon state (default)
//   - ModeSwarm: Force Swarm mode (fails if not in Swarm)
//   - ModeStandalone: Force standalone mode
//
// Example usage:
//
//	client, err := docker.NewClient(ctx, docker.WithHost("unix:///var/run/docker.sock"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	workloads, err := client.ListWorkloads(ctx)
//	for _, w := range workloads {
//	    log.Printf("%s: %s (%s)", w.Type, w.Name, w.ID)
//	}
package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// Mode represents the Docker operation mode.
type Mode string

const (
	// ModeAuto auto-detects whether Docker is in Swarm or standalone mode.
	// Auto never selects Both — it's an opt-in for managers that also host
	// standalone containers, and changing auto's behavior would be a silent
	// breaking change for existing Swarm users.
	ModeAuto Mode = "auto"
	// ModeSwarm forces Swarm mode operation.
	ModeSwarm Mode = "swarm"
	// ModeStandalone forces standalone container mode operation.
	ModeStandalone Mode = "standalone"
	// ModeBoth lists Swarm services AND standalone containers on the same
	// node. Requires an active Swarm with the local node as manager.
	// Swarm task-containers (containers belonging to a service) are filtered
	// out to avoid double-counting.
	ModeBoth Mode = "both"
)

// swarmTaskLabel is the Docker-managed label Swarm places on every container
// that is a task of a service. We use its presence to filter task-containers
// out of standalone listings so they don't double-count with their parent
// service. Chosen over com.docker.swarm.task.id because "owned by a service"
// is the semantic we want — an orphaned task without a service.id is not
// something we'd double-count anyway (it has no parent in ListServices).
const swarmTaskLabel = "com.docker.swarm.service.id"

// String returns the string representation of the mode.
func (m Mode) String() string {
	return string(m)
}

var (
	// ErrNotSwarmMode is returned when a Swarm-only operation is attempted in standalone mode.
	ErrNotSwarmMode = errors.New("operation requires Docker Swarm mode")
	// ErrNotStandaloneMode is returned when a standalone-only operation is attempted in Swarm mode.
	ErrNotStandaloneMode = errors.New("operation requires Docker standalone mode")
	// ErrNotManager is returned when the node is in Swarm mode but not a manager.
	ErrNotManager = errors.New("swarm mode detected but this node is not a manager")
	// ErrSwarmNotActive is returned when Swarm mode is forced but Swarm is not active.
	ErrSwarmNotActive = errors.New("swarm mode forced but swarm is not active")
)

// Client wraps the Docker SDK client with DNSWeaver-specific functionality.
//
// The client automatically detects whether Docker is running in Swarm or
// standalone mode and provides appropriate methods for each. Use ListWorkloads()
// for mode-agnostic workload listing.
type Client struct {
	docker         *client.Client
	mode           Mode
	detectedMode   Mode
	logger         *slog.Logger
	host           string
	cleanupOnStop  bool          // If true, only list running containers; if false, include stopped
	connectTimeout time.Duration // Max time to retry the initial connect before failing hard; 0 = fail immediately

	// swarmActive is a snapshot of Swarm participation taken at client init.
	// It is intentionally not refreshed: if the Swarm is torn down at runtime,
	// the task-container filter keeps running (fail-safe — no risk of Swarm
	// task-containers leaking into the workload list until the next restart).
	swarmActive bool
}

// NewClient creates a new Docker client with the given options.
//
// By default, the client:
//   - Uses DOCKER_HOST environment variable or the default socket
//   - Auto-detects Swarm vs standalone mode
//   - Uses slog.Default() for logging
//
// Options can be provided to customize behavior:
//
//	client, err := docker.NewClient(ctx,
//	    docker.WithHost("tcp://docker.example.com:2375"),
//	    docker.WithMode(docker.ModeSwarm),
//	    docker.WithLogger(myLogger),
//	)
func NewClient(ctx context.Context, opts ...Option) (*Client, error) {
	c := &Client{
		mode:          ModeAuto,
		logger:        slog.Default(),
		cleanupOnStop: true, // Default: only list running containers
	}

	// Apply options
	for _, opt := range opts {
		opt(c)
	}

	// Build Docker client options
	var dockerOpts []client.Opt
	dockerOpts = append(dockerOpts, client.FromEnv)
	dockerOpts = append(dockerOpts, client.WithAPIVersionNegotiation())

	if c.host != "" {
		dockerOpts = append(dockerOpts, client.WithHost(c.host))
	}

	// Create Docker client
	dockerClient, err := client.NewClientWithOpts(dockerOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	c.docker = dockerClient

	// Detect or verify mode, retrying the initial connection for up to
	// connectTimeout. This tolerates a dependency that isn't ready the instant
	// dnsweaver starts (e.g. a label-driven socket proxy that authorizes this
	// container a few seconds after it comes up, #125) while still failing hard
	// if the endpoint never becomes usable.
	if err := c.connectWithRetry(ctx); err != nil {
		dockerClient.Close()
		return nil, err
	}

	c.logger.Info("docker client initialized",
		slog.String("mode", c.detectedMode.String()),
		slog.String("configured_mode", c.mode.String()),
	)

	return c, nil
}

// connectWithRetry runs initializeMode, retrying with capped exponential
// backoff until it succeeds or connectTimeout elapses. A connectTimeout of 0
// means fail immediately on the first error (strict fail-fast). Configuration
// errors that can't be fixed by waiting (e.g. Swarm mode forced but this node
// is not a manager) fail immediately regardless of the timeout.
func (c *Client) connectWithRetry(ctx context.Context) error {
	err := c.initializeMode(ctx)
	if err == nil {
		return nil
	}
	// Never retry deterministic misconfiguration — waiting won't fix it.
	if c.connectTimeout <= 0 || isTerminalModeErr(err) {
		return err
	}

	deadline := time.Now().Add(c.connectTimeout)
	backoff := 500 * time.Millisecond
	const maxBackoff = 5 * time.Second

	for {
		wait := backoff
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return fmt.Errorf("connecting to docker after %s: %w", c.connectTimeout, err)
		}

		c.logger.Info("waiting for docker endpoint to become available",
			slog.String("host", c.host),
			slog.String("error", err.Error()),
			slog.Duration("retry_in", wait),
		)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("connecting to docker: %w", ctx.Err())
		case <-timer.C:
		}

		err = c.initializeMode(ctx)
		if err == nil {
			return nil
		}
		if isTerminalModeErr(err) {
			return err
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// isTerminalModeErr reports whether an initializeMode error is a deterministic
// misconfiguration that retrying cannot resolve, so we should fail immediately
// instead of waiting out the connect timeout.
func isTerminalModeErr(err error) bool {
	return errors.Is(err, ErrNotManager) || errors.Is(err, ErrSwarmNotActive)
}

// initializeMode detects or verifies the Docker mode based on configuration.
func (c *Client) initializeMode(ctx context.Context) error {
	info, err := c.docker.Info(ctx)
	if err != nil {
		return fmt.Errorf("getting docker info: %w", err)
	}

	isSwarmActive := info.Swarm.LocalNodeState == swarm.LocalNodeStateActive
	isManager := info.Swarm.ControlAvailable

	c.logger.Debug("docker info retrieved",
		slog.String("swarm_state", string(info.Swarm.LocalNodeState)),
		slog.Bool("control_available", isManager),
		slog.String("node_id", info.Swarm.NodeID),
	)

	c.swarmActive = isSwarmActive

	switch c.mode {
	case ModeAuto:
		if isSwarmActive {
			if !isManager {
				return ErrNotManager
			}
			c.detectedMode = ModeSwarm
		} else {
			c.detectedMode = ModeStandalone
		}

	case ModeSwarm:
		if !isSwarmActive {
			return ErrSwarmNotActive
		}
		if !isManager {
			return ErrNotManager
		}
		c.detectedMode = ModeSwarm

	case ModeStandalone:
		c.detectedMode = ModeStandalone

	case ModeBoth:
		if !isSwarmActive {
			return ErrSwarmNotActive
		}
		if !isManager {
			return ErrNotManager
		}
		c.detectedMode = ModeBoth
	}

	return nil
}

// Mode returns the detected Docker mode.
// This reflects the actual operating mode after initialization.
func (c *Client) Mode() Mode {
	return c.detectedMode
}

// IsSwarm returns true if the client is operating in Swarm mode.
// Note: this returns false for ModeBoth even when Swarm is active locally.
// Use IsSwarmActive() when you need to know about local Swarm participation
// independent of listing behavior.
func (c *Client) IsSwarm() bool {
	return c.detectedMode == ModeSwarm
}

// IsSwarmActive reports whether the local daemon participates in an active
// Swarm (as a manager or a worker), independent of configured mode. Use this
// when the decision depends on local Swarm presence rather than on Swarm-only listing
// semantics — for example, deciding whether to filter out Swarm task-containers
// from a standalone container listing.
func (c *Client) IsSwarmActive() bool {
	return c.swarmActive
}

// Close closes the underlying Docker client connection.
func (c *Client) Close() error {
	if c.docker != nil {
		return c.docker.Close()
	}
	return nil
}

// Ping verifies connectivity to the Docker daemon.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.docker.Ping(ctx)
	if err != nil {
		return fmt.Errorf("pinging docker: %w", err)
	}
	return nil
}

// RawClient returns the underlying Docker SDK client for advanced operations.
// Use with caution — prefer using the wrapped methods when possible.
func (c *Client) RawClient() *client.Client {
	return c.docker
}

// Service represents a Docker Swarm service with relevant fields for DNS management.
type Service struct {
	ID     string
	Name   string
	Labels map[string]string
}

// Container represents a Docker container with relevant fields for DNS management.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
}

// ListServices returns all Swarm services with their labels.
// Returns ErrNotSwarmMode if the client is not in a mode that lists services
// (ModeSwarm or ModeBoth).
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	if c.detectedMode != ModeSwarm && c.detectedMode != ModeBoth {
		return nil, ErrNotSwarmMode
	}

	services, err := c.docker.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}

	result := make([]Service, 0, len(services))
	for _, svc := range services {
		result = append(result, Service{
			ID:     svc.ID,
			Name:   svc.Spec.Name,
			Labels: svc.Spec.Labels,
		})
	}

	c.logger.Debug("listed swarm services",
		slog.Int("count", len(result)),
	)

	return result, nil
}

// ListContainers returns containers with their labels.
// If cleanupOnStop is true (default), only running containers are returned.
// If cleanupOnStop is false, both running and stopped containers are returned,
// allowing DNS records to persist through stop/restart cycles.
// Returns ErrNotStandaloneMode if the client is not in a mode that lists
// containers (ModeStandalone or ModeBoth).
//
// This method is semantically pure: it returns every container matching the
// status filter, including Swarm task-containers. Callers that want a
// "standalone only" view (e.g. the ListWorkloads path in ModeBoth or in
// ModeStandalone on a Swarm manager) must post-filter with
// filterSwarmTaskContainers.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	if c.detectedMode != ModeStandalone && c.detectedMode != ModeBoth {
		return nil, ErrNotStandaloneMode
	}

	listOpts := container.ListOptions{}
	if c.cleanupOnStop {
		// Only list running containers (stopped containers = orphans)
		listOpts.Filters = filters.NewArgs(
			filters.Arg("status", "running"),
		)
	} else {
		// Include both running and stopped containers
		// Only truly removed containers will be orphans
		listOpts.All = true
		listOpts.Filters = filters.NewArgs(
			filters.Arg("status", "running"),
			filters.Arg("status", "paused"),
			filters.Arg("status", "exited"),
			filters.Arg("status", "created"),
		)
	}

	containers, err := c.docker.ContainerList(ctx, listOpts)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	result := make([]Container, 0, len(containers))
	for _, ctr := range containers {
		name := normalizeContainerName(ctr.Names)

		result = append(result, Container{
			ID:     ctr.ID,
			Name:   name,
			Labels: ctr.Labels,
		})
	}

	c.logger.Debug("listed containers",
		slog.Int("count", len(result)),
		slog.Bool("include_stopped", !c.cleanupOnStop),
	)

	return result, nil
}

// normalizeContainerName extracts a clean container name from Docker's name list.
// Container names from Docker start with "/" which we strip.
func normalizeContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	name := names[0]
	if len(name) > 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}

// GetServiceLabels returns the labels for a specific Swarm service by ID.
// Returns ErrNotSwarmMode if the client is not in ModeSwarm or ModeBoth.
func (c *Client) GetServiceLabels(ctx context.Context, serviceID string) (map[string]string, error) {
	if c.detectedMode != ModeSwarm && c.detectedMode != ModeBoth {
		return nil, ErrNotSwarmMode
	}

	svc, _, err := c.docker.ServiceInspectWithRaw(ctx, serviceID, swarm.ServiceInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspecting service %s: %w", serviceID, err)
	}

	return svc.Spec.Labels, nil
}

// GetContainerLabels returns the labels for a specific container by ID.
// Returns ErrNotStandaloneMode if the client is not in ModeStandalone or ModeBoth.
func (c *Client) GetContainerLabels(ctx context.Context, containerID string) (map[string]string, error) {
	if c.detectedMode != ModeStandalone && c.detectedMode != ModeBoth {
		return nil, ErrNotStandaloneMode
	}

	ctr, err := c.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", containerID, err)
	}

	return ctr.Config.Labels, nil
}

// filterSwarmTaskContainers drops any container carrying the Swarm
// service-id label. Task-containers would otherwise double-count with their
// parent service (and churn on every rescheduling event). The function also
// patches a pre-existing issue where ModeStandalone on a Swarm manager node
// would pick up task-containers as if they were user-run standalone
// containers.
//
// Semantic choice: we filter on com.docker.swarm.service.id (not task.id).
// A task without a service.id is an orphaned task with no matching entry in
// ListServices, so there is no double-count to prevent and we let it through.
func filterSwarmTaskContainers(in []Container) []Container {
	out := make([]Container, 0, len(in))
	for _, ctr := range in {
		if _, isTask := ctr.Labels[swarmTaskLabel]; isTask {
			continue
		}
		out = append(out, ctr)
	}
	return out
}

// composeWorkloads builds the unified workload slice from service and
// container listings. Pure function: all IO has already happened. If
// filterTasks is true, Swarm task-containers (those with the service-id
// label) are removed from the container list before composition.
//
// Services are emitted first, then containers, for stable ordering.
func composeWorkloads(services []Service, containers []Container, filterTasks bool) []Workload {
	if filterTasks {
		containers = filterSwarmTaskContainers(containers)
	}
	out := make([]Workload, 0, len(services)+len(containers))
	for _, svc := range services {
		out = append(out, Workload{
			ID:     svc.ID,
			Name:   svc.Name,
			Labels: svc.Labels,
			Type:   WorkloadTypeService,
		})
	}
	for _, ctr := range containers {
		out = append(out, Workload{
			ID:     ctr.ID,
			Name:   ctr.Name,
			Labels: ctr.Labels,
			Type:   WorkloadTypeContainer,
		})
	}
	return out
}

// ListWorkloads returns all workloads as a unified slice:
//   - ModeSwarm: Swarm services only
//   - ModeStandalone: standalone containers only
//   - ModeBoth: services + standalone containers (task-containers filtered)
//
// Services are returned first, then containers, for stable ordering across
// reconciles (useful in tests and in log comparisons).
//
// Failure policy: fail closed. If either listing call returns an error in
// ModeBoth, the whole call errors out. Returning a partial list would let
// the reconciler treat the missing side as "all workloads gone" and mass-
// delete their DNS records. The orphan-cleanup circuit breaker
// (internal/reconciler/orphan.go) is defense-in-depth; fail-closed here is
// primary protection.
func (c *Client) ListWorkloads(ctx context.Context) ([]Workload, error) {
	var services []Service
	var containers []Container

	if c.detectedMode == ModeSwarm || c.detectedMode == ModeBoth {
		s, err := c.ListServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing services: %w", err)
		}
		services = s
	}

	if c.detectedMode == ModeStandalone || c.detectedMode == ModeBoth {
		ctrs, err := c.ListContainers(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing containers: %w", err)
		}
		containers = ctrs
	}

	// When the local daemon participates in an active Swarm, containers may
	// include task-containers owned by services. Filter them out regardless
	// of configured mode — correctness-preserving in both ModeBoth and
	// ModeStandalone-on-manager.
	return composeWorkloads(services, containers, c.swarmActive), nil
}

// GetWorkloadLabels returns the labels for a specific workload by ID.
// Automatically uses the correct method based on current mode. In ModeBoth
// it tries services first and falls back to containers only when the service
// lookup reports not-found: the ID namespaces are separate and we don't know
// which one the caller has, but any other service error (daemon or proxy
// failure) is returned as-is rather than masked by a container lookup.
func (c *Client) GetWorkloadLabels(ctx context.Context, workloadID string) (map[string]string, error) {
	switch c.detectedMode {
	case ModeSwarm:
		return c.GetServiceLabels(ctx, workloadID)
	case ModeStandalone:
		return c.GetContainerLabels(ctx, workloadID)
	case ModeBoth:
		labels, err := c.GetServiceLabels(ctx, workloadID)
		if err == nil || !cerrdefs.IsNotFound(err) {
			return labels, err
		}
		return c.GetContainerLabels(ctx, workloadID)
	default:
		return nil, fmt.Errorf("docker client mode %q is not initialized", c.detectedMode)
	}
}
