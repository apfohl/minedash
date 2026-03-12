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

// dockerService wraps the Docker SDK client and targets a single named container.
type dockerService struct {
	client        *client.Client
	containerName string
}

// newDockerService creates a dockerService, resolving the target container name using
// the following priority order:
//
//  1. MC_CONTAINER_NAME — use directly (explicit override, no label lookup)
//  2. MC_STACK_NAME + MC_SERVICE_NAME — look up container by Compose labels
func newDockerService(cfg Config) (*dockerService, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	name, err := resolveContainerName(context.Background(), c, cfg)
	if err != nil {
		_ = c.Close()
		return nil, err
	}

	return &dockerService{client: c, containerName: name}, nil
}

// resolveContainerName determines the target MC container name/ID.
func resolveContainerName(ctx context.Context, c *client.Client, cfg Config) (string, error) {
	// Priority 1: explicit container name override.
	if cfg.MCContainerName != "" {
		return cfg.MCContainerName, nil
	}

	// Priority 2: look up by Compose labels.
	if cfg.MCStackName == "" {
		return "", fmt.Errorf(
			"MC_STACK_NAME is required; set it to ${COMPOSE_PROJECT_NAME} in your compose.yml, " +
				"or set MC_CONTAINER_NAME for a direct name override")
	}

	id, err := findContainerByLabels(ctx, c, cfg.MCStackName, cfg.MCServiceName)
	if err != nil {
		return "", fmt.Errorf("finding minecraft container (stack=%q service=%q): %w",
			cfg.MCStackName, cfg.MCServiceName, err)
	}
	return id, nil
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
// When the container has a healthcheck (itzg/minecraft-server ships one by default),
// the health state is used to distinguish a fully running server from one still booting.
func (d *dockerService) status(ctx context.Context) (string, error) {
	info, err := d.client.ContainerInspect(ctx, d.containerName)
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
	if err := d.client.ContainerStart(ctx, d.containerName, container.StartOptions{}); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// stop gracefully stops the container (SIGTERM, 10 s timeout).
func (d *dockerService) stop(ctx context.Context) error {
	timeout := 10
	if err := d.client.ContainerStop(ctx, d.containerName, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// restart restarts the container.
func (d *dockerService) restart(ctx context.Context) error {
	timeout := 10
	if err := d.client.ContainerRestart(ctx, d.containerName, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	return nil
}
