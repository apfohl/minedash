package main

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
)

// dockerService wraps the Docker SDK client and targets a single Minecraft container.
// The container is resolved by label on every operation so that container recreations
// (which assign a new ID) are handled transparently.
type dockerService struct {
	client        *client.Client
	containerName string // direct name/ID override — used as-is when set
	stackName     string // com.docker.compose.project label value
	serviceName   string // com.docker.compose.service label value
}

// newDockerService creates a dockerService. Container resolution is deferred to each
// operation so that a recreated MC container is always found correctly.
func newDockerService(cfg Config) (*dockerService, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	if cfg.MCContainerName == "" && cfg.MCStackName == "" {
		_ = c.Close()
		return nil, fmt.Errorf(
			"MC_STACK_NAME is required; set it to ${COMPOSE_PROJECT_NAME} in your compose.yml, " +
				"or set MC_CONTAINER_NAME for a direct name override")
	}

	return &dockerService{
		client:        c,
		containerName: cfg.MCContainerName,
		stackName:     cfg.MCStackName,
		serviceName:   cfg.MCServiceName,
	}, nil
}

// resolve returns the current container ID/name for the MC container.
// When containerName is set it is returned directly.
// Otherwise the container is looked up fresh by Compose labels on every call,
// so that a recreated container (new ID) is always found.
func (d *dockerService) resolve(ctx context.Context) (string, error) {
	if d.containerName != "" {
		return d.containerName, nil
	}
	return findContainerByLabels(ctx, d.client, d.stackName, d.serviceName)
}

// findContainerByLabels returns the ID of the first container matching the given
// com.docker.compose.project and com.docker.compose.service labels.
func findContainerByLabels(ctx context.Context, c *client.Client, stackName, serviceName string) (string, error) {
	f := filters.NewArgs(
		filters.Arg("label", labelProject+"="+stackName),
		filters.Arg("label", labelService+"="+serviceName),
	)
	list, err := c.ContainerList(ctx, container.ListOptions{
		All:     true, // include stopped containers
		Filters: f,
	})
	if err != nil {
		return "", fmt.Errorf("container list: %w", err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no container found with labels %s=%s, %s=%s",
			labelProject, stackName, labelService, serviceName)
	}
	return list[0].ID, nil
}

// close releases the underlying Docker client.
func (d *dockerService) close() {
	_ = d.client.Close()
}

// status returns one of: "running", "starting", "stopping", "stopped", "unhealthy", "unknown".
// When the container has a healthcheck (itzg/docker-minecraft-server ships one by default),
// the health state is used to distinguish a fully running server from one still booting.
// If the container does not exist at all, "stopped" is returned.
func (d *dockerService) status(ctx context.Context) (string, error) {
	id, err := d.resolve(ctx)
	if err != nil {
		// Container not found via labels = it doesn't exist yet, treat as stopped.
		return "stopped", nil
	}

	info, err := d.client.ContainerInspect(ctx, id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return "stopped", nil
		}
		return "unknown", fmt.Errorf("inspect: %w", err)
	}

	switch info.State.Status {
	case container.StateExited, container.StateDead, container.StateCreated, container.StatePaused:
		return "stopped", nil
	case container.StateRestarting:
		return "starting", nil
	case container.StateRemoving:
		return "stopping", nil
	case container.StateRunning:
		// No healthcheck configured — trust Docker's running state directly.
		if info.State.Health == nil {
			return "running", nil
		}
		switch info.State.Health.Status {
		case container.Starting:
			return "starting", nil
		case container.Healthy:
			return "running", nil
		case container.Unhealthy:
			return "unhealthy", nil
		default: // "none" or any future value
			return "running", nil
		}
	default:
		return "unknown", nil
	}
}

// start starts the container.
func (d *dockerService) start(ctx context.Context) error {
	id, err := d.resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	if err := d.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// stop gracefully stops the container (SIGTERM, 10 s timeout).
func (d *dockerService) stop(ctx context.Context) error {
	id, err := d.resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	timeout := 10
	if err := d.client.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// restart restarts the container.
func (d *dockerService) restart(ctx context.Context) error {
	id, err := d.resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	timeout := 10
	if err := d.client.ContainerRestart(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	return nil
}
