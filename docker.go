package main

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// dockerService wraps the Docker SDK client and targets a single named container.
type dockerService struct {
	client        *client.Client
	containerName string
}

// newDockerService creates a new dockerService using the default Docker socket.
func newDockerService(containerName string) (*dockerService, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &dockerService{client: c, containerName: containerName}, nil
}

// close releases the underlying Docker client.
func (d *dockerService) close() {
	_ = d.client.Close()
}

// status returns one of: "running", "stopped", "starting", "stopping", "unknown".
func (d *dockerService) status(ctx context.Context) (string, error) {
	info, err := d.client.ContainerInspect(ctx, d.containerName)
	if err != nil {
		if client.IsErrNotFound(err) {
			return "stopped", nil
		}
		return "unknown", fmt.Errorf("inspect: %w", err)
	}

	switch info.State.Status {
	case "running":
		return "running", nil
	case "exited", "dead", "created":
		return "stopped", nil
	case "restarting":
		return "starting", nil
	case "paused":
		return "stopped", nil
	default:
		return info.State.Status, nil
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
