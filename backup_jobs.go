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
	mu          sync.Mutex
	docker      *dockerService
	service     string
	execID      string
	last        backupJobStatus
	statusError string
}

var manualBackups *backupJobs

func backupScheduler(command string) bool {
	words := strings.Fields(command)
	for _, word := range words {
		option := strings.Trim(word, "'\";")
		if option == "-foreground" || option == "--foreground" || option == "-foreground=true" || option == "--foreground=true" {
			return true
		}
	}
	return false
}
func backupProcess(command string) bool {
	if backupScheduler(command) {
		return false
	}
	words := strings.Fields(command)
	if len(words) == 0 {
		return false
	}
	// Match the executable, rather than arbitrary arguments containing 'backup'.
	executable := filepath.Base(strings.Trim(words[0], "'\";"))
	if executable == "backup" {
		for _, word := range words[1:] {
			if strings.Trim(word, "'\";") == "print-config" {
				return false
			}
		}
		return true
	}
	if executable == "sh" || executable == "bash" {
		if len(words) > 1 && filepath.Base(strings.Trim(words[1], "'\";")) == "backup" {
			return true
		}
		if len(words) > 2 && words[1] == "-c" && filepath.Base(strings.Trim(words[2], "'\";")) == "backup" {
			return true
		}
	}
	return false
}

// The foreground scheduler runs scheduled backups in-process. Read the lock
// recorded on its file descriptor; checking the lock file's existence is insufficient.
const backupLockProbe = `
proc_root=$1
[ -d "$proc_root/1/fd" ] && [ -r "$proc_root/1/fd" ] || exit 20
for fd in "$proc_root"/[0-9]*/fd/*; do
 [ "$(readlink "$fd" 2>/dev/null)" = /var/lock/dockervolumebackup.lock ] || continue
 info="${fd%/fd/*}/fdinfo/${fd##*/}"
 [ -r "$info" ] || exit 20
 grep -q '^lock:' "$info"
 result=$?
 [ "$result" = 0 ] && exit 10
 [ "$result" = 1 ] || exit 20
done
exit 0
`

func (b *backupJobs) scheduledBackupRunning(ctx context.Context, id string) (bool, error) {
	job, err := b.docker.client.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: []string{"sh", "-c", backupLockProbe, "minedash-backup-status", "/proc"}})
	if err != nil {
		return false, err
	}
	if err = b.docker.client.ContainerExecStart(ctx, job.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return false, err
	}
	var exitCode int
	for {
		result, err := b.docker.client.ContainerExecInspect(ctx, job.ID)
		if err != nil {
			return false, err
		}
		if !result.Running {
			exitCode = result.ExitCode
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	switch exitCode {
	case 0:
		return false, nil
	case 10:
		return true, nil
	default:
		return false, fmt.Errorf("backup lock probe failed with exit code %d", exitCode)
	}
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
	unknown := func(operation string, err error) backupJobStatus {
		detail := fmt.Sprintf("%s: %v", operation, err)
		if detail != b.statusError {
			log.Printf("backup status: service=%q stack=%q: %s", b.service, b.docker.stackName, detail)
			b.statusError = detail
		}
		return backupJobStatus{Available: true, State: "unknown", Message: "Backup status unavailable. Server controls are blocked until it can be checked."}
	}
	id, e := b.containerID(ctx)
	if e != nil {
		return unknown("resolve backup container", e)
	}
	if id == "" {
		if b.execID != "" {
			return unknown("backup container missing", fmt.Errorf("tracked backup exec %s has no service container", b.execID))
		}
		return backupJobStatus{State: "unavailable", Message: "No backup service found in this Compose stack."}
	}
	info, e := b.docker.client.ContainerInspect(ctx, id)
	if e != nil {
		return unknown("inspect backup container", e)
	}
	if b.execID != "" {
		job, e := b.docker.client.ContainerExecInspect(ctx, b.execID)
		if e != nil {
			return unknown("inspect backup exec", e)
		}
		if job.Running {
			return backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."}
		}
		log.Printf("backup completed: service=%q exec=%q exit_code=%d", b.service, b.execID, job.ExitCode)
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
	// Docker must see a PID column to filter processes to this container.
	// Its default listing includes PID and puts the full command last.
	top, e := b.docker.client.ContainerTop(ctx, id, nil)
	if e != nil {
		return unknown("list backup processes", e)
	}
	scheduler := false
	for _, process := range top.Processes {
		if len(process) > 0 && backupScheduler(process[len(process)-1]) {
			scheduler = true
		}
		if len(process) > 0 && backupProcess(process[len(process)-1]) {
			return backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."}
		}
	}
	if scheduler {
		running, err := b.scheduledBackupRunning(ctx, id)
		if err != nil {
			return unknown("inspect scheduled backup lock", err)
		}
		if running {
			return backupJobStatus{Available: true, State: "running", Message: "Scheduled backup in progress. The backup service manages stopping and restarting the server."}
		}
	}
	b.statusError = ""
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
			log.Printf("backup create: service=%q container=%q: %v", b.service, id, e)
			errorJSON(w, 503, "Could not create backup process.")
			return
		}
		b.execID = job.ID
		if e = b.docker.client.ContainerExecStart(ctx, job.ID, container.ExecStartOptions{Detach: true}); e != nil {
			// Retain the ID: the daemon may have accepted the start despite a transport failure.
			log.Printf("backup start: service=%q container=%q exec=%q: %v", b.service, id, job.ID, e)
			errorJSON(w, 503, "Could not confirm backup startup. Checking process status.")
			return
		}
		logRequest(r, "manual backup")
		writeJSON(w, http.StatusAccepted, backupJobStatus{Available: true, State: "running", Message: "Backup in progress. The backup service manages stopping and restarting the server."})
	})
}
