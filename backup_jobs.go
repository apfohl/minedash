package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

type backupJobStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message"`
}
type backupJobs struct {
	mu      sync.Mutex
	docker  *dockerService
	service string
	execID  string
	last    backupJobStatus
}

var manualBackups *backupJobs

func backupProcess(command string) bool {
	for _, word := range strings.Fields(command) {
		if filepath.Base(strings.Trim(word, "'\";")) == "backup" {
			return true
		}
	}
	return false
}
func (b *backupJobs) snapshot(ctx context.Context) backupJobStatus {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked(ctx)
}
func (b *backupJobs) containerID(ctx context.Context) (string, error) {
	stack := b.docker.stackName
	if stack == "" {
		id, e := b.docker.resolve(ctx)
		if e != nil {
			return "", e
		}
		info, e := b.docker.client.ContainerInspect(ctx, id)
		if e != nil {
			return "", e
		}
		stack = info.Config.Labels[labelProject]
		if stack == "" {
			return "", nil
		}
	}
	list, e := b.docker.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", labelProject+"="+stack), filters.Arg("label", labelService+"="+b.service))})
	if e != nil {
		return "", e
	}
	if len(list) == 0 {
		return "", nil
	}
	if len(list) != 1 {
		return "", fmt.Errorf("multiple backup containers")
	}
	return list[0].ID, nil
}
func (b *backupJobs) snapshotLocked(ctx context.Context) backupJobStatus {
	unknown := backupJobStatus{Available: true, State: "unknown", Message: "Backup status unavailable. Server controls are blocked until it can be checked."}
	id, e := b.containerID(ctx)
	if e != nil {
		return unknown
	}
	if id == "" {
		if b.execID != "" {
			return unknown
		}
		return backupJobStatus{State: "unavailable", Message: "No backup service found in this Compose stack."}
	}
	info, e := b.docker.client.ContainerInspect(ctx, id)
	if e != nil {
		return unknown
	}
	if b.execID != "" {
		job, e := b.docker.client.ContainerExecInspect(ctx, b.execID)
		if e != nil {
			return unknown
		}
		if job.Running {
			return backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."}
		}
		b.execID = ""
		if job.ExitCode == 0 {
			b.last = backupJobStatus{Available: true, State: "succeeded", Message: "Backup completed successfully."}
		} else {
			b.last = backupJobStatus{Available: true, State: "failed", Message: fmt.Sprintf("Backup failed (exit code %d). Check the backup service configuration.", job.ExitCode)}
		}
	}
	if info.State == nil || !info.State.Running {
		return backupJobStatus{State: "unavailable", Message: "The backup service is stopped."}
	}
	// Also recognize scheduled backups and jobs that outlive a MineDash restart.
	top, e := b.docker.client.ContainerTop(ctx, id, []string{"-eo", "args"})
	if e != nil {
		return unknown
	}
	for _, process := range top.Processes {
		if len(process) > 0 && backupProcess(process[len(process)-1]) {
			return backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."}
		}
	}
	if b.last.State != "" {
		return b.last
	}
	return backupJobStatus{Available: true, State: "idle", Message: "Ready to create a backup."}
}
func backupBlocksControls() bool {
	if manualBackups == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := manualBackups.snapshot(ctx).State
	return state == "running" || state == "unknown"
}
func backupJobStatusHandler(b *backupJobs) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, b.snapshot(r.Context()))
	})
}
func createBackupHandler(b *backupJobs) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !volumeOperation.TryLock() {
			errorJSON(w, 409, "A server data operation is in progress.")
			return
		}
		defer volumeOperation.Unlock()
		if restorePending(volumePath) {
			errorJSON(w, 409, "An unfinished restore blocks backup creation.")
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		state := b.snapshotLocked(ctx)
		if !state.Available || state.State == "running" || state.State == "unknown" {
			errorJSON(w, 409, state.Message)
			return
		}
		id, e := b.containerID(ctx)
		if e != nil || id == "" {
			errorJSON(w, 503, "Backup service unavailable.")
			return
		}
		job, e := b.docker.client.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: []string{"backup"}})
		if e != nil {
			log.Printf("backup create: %v", e)
			errorJSON(w, 503, "Could not create backup process.")
			return
		}
		b.execID = job.ID
		if e = b.docker.client.ContainerExecStart(ctx, job.ID, container.ExecStartOptions{Detach: true}); e != nil {
			// Retain the ID: the daemon may have accepted the start despite a transport failure.
			log.Printf("backup start: %v", e)
			errorJSON(w, 503, "Could not confirm backup startup. Checking process status.")
			return
		}
		logRequest(r, "manual backup")
		writeJSON(w, http.StatusAccepted, backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."})
	})
}
