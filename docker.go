package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

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
//  1. MC_CONTAINER_NAME — use directly (backwards compat / explicit override)
//  2. MC_STACK_NAME + MC_SERVICE_NAME — look up container by Compose labels
//  3. MC_SERVICE_NAME only — derive the stack name from the minedash container's own
//     com.docker.compose.project label (self-inspection via /proc/self/cgroup)
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

	// Priority 2 & 3: look up by Compose labels.
	stackName := cfg.MCStackName
	if stackName == "" {
		// Derive stack name from our own container's labels.
		var err error
		stackName, err = selfStackName(ctx, c)
		if err != nil {
			return "", fmt.Errorf(
				"MC_STACK_NAME is not set and could not be derived automatically (%w); "+
					"set MC_STACK_NAME or MC_CONTAINER_NAME explicitly", err)
		}
	}

	id, err := findContainerByLabels(ctx, c, stackName, cfg.MCServiceName)
	if err != nil {
		return "", fmt.Errorf("finding minecraft container (stack=%q service=%q): %w",
			stackName, cfg.MCServiceName, err)
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

// selfStackName inspects the minedash container itself to read its own
// com.docker.compose.project label, which is identical to the MC container's stack name
// since both services are part of the same Compose stack.
//
// The container ID is obtained by parsing /proc/self/cgroup, which Docker populates
// with the full 64-character container ID on both cgroup v1 and v2 hosts.
func selfStackName(ctx context.Context, c *client.Client) (string, error) {
	id, err := selfContainerID()
	if err != nil {
		return "", fmt.Errorf("read own container ID: %w", err)
	}

	info, err := c.ContainerInspect(ctx, id)
	if err != nil {
		return "", fmt.Errorf("inspect own container: %w", err)
	}

	stack, ok := info.Config.Labels[labelProject]
	if !ok || stack == "" {
		return "", fmt.Errorf("own container has no %s label — is minedash running inside Docker Compose?", labelProject)
	}
	return stack, nil
}

// selfContainerID extracts the running container's full ID from /proc/self/cgroup.
// Docker writes the 64-char hex ID into the cgroup path for every container.
func selfContainerID() (string, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("open /proc/self/cgroup: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// cgroup v1: "12:devices:/docker/<id>"
		// cgroup v2: "0::/system.slice/docker-<id>.scope"
		parts := strings.Split(line, "/")
		for _, part := range parts {
			part = strings.TrimSuffix(part, ".scope")
			part = strings.TrimPrefix(part, "docker-")
			if len(part) == 64 && isHex(part) {
				return part, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan /proc/self/cgroup: %w", err)
	}
	return "", fmt.Errorf("container ID not found in /proc/self/cgroup")
}

// isHex reports whether s consists entirely of hexadecimal characters.
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// close releases the underlying Docker client.
func (d *dockerService) close() {
	_ = d.client.Close()
}

// status returns one of: "running", "starting", "stopping", "stopped", "unhealthy", "unknown".
// When the container has a healthcheck (itzg/docker-minecraft-server ships one by default),
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
