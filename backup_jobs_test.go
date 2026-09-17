package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

func TestBackupProcess(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"/usr/bin/backup", true}, {"/bin/sh /usr/bin/backup", true}, {"/bin/sh -c backup", true}, {"/bin/sh /usr/bin/backup.sh", false}, {"crond -f", false}, {"sleep 5", false},
		{"/usr/bin/backup -foreground", false},
		{"/usr/bin/backup --foreground", false},
		{"/usr/bin/backup -foreground=true -profile 0", false},
		{"/usr/bin/backup print-config", false},
		{"echo backup", false},
		{"grep backup /proc/locks", false},
	} {
		if backupProcess(tc.command) != tc.want {
			t.Fatalf("%q", tc.command)
		}
	}
}

func TestManualBackupLifecycleAndGuards(t *testing.T) {
	running := false
	exitCode := 0
	scheduled := false
	failTop := false
	created := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		switch {
		case path == "/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), "backup") {
				t.Error("missing backup service filter")
			}
			w.Write([]byte(`[{"Id":"backup-container"}]`))
		case path == "/containers/backup-container/json":
			w.Write([]byte(`{"Id":"backup-container","State":{"Running":true}}`))
		case path == "/containers/backup-container/top":
			// Docker rejects custom formats without PID. Require the default listing.
			if args := r.URL.Query().Get("ps_args"); args != "" {
				w.WriteHeader(500)
				w.Write([]byte(`{"message":"Couldn't find PID field in ps output"}`))
				return
			}
			if failTop {
				w.WriteHeader(500)
				w.Write([]byte(`{"message":"unavailable"}`))
				return
			}
			process := "/usr/bin/backup -foreground"
			json.NewEncoder(w).Encode(map[string]any{"Titles": []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}, "Processes": [][]string{{"root", "42", "1", "0", "10:00", "?", "00:00:00", process}}})
		case path == "/containers/backup-container/exec":
			var body struct{ Cmd []string }
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Cmd) == 5 && body.Cmd[0] == "sh" && body.Cmd[2] == backupLockProbe {
				w.WriteHeader(201)
				w.Write([]byte(`{"Id":"probe"}`))
				return
			}
			if len(body.Cmd) != 1 || body.Cmd[0] != "backup" {
				t.Error("wrong command")
			}
			created++
			w.WriteHeader(201)
			w.Write([]byte(`{"Id":"job"}`))
		case path == "/exec/probe/start":
			w.WriteHeader(200)
		case path == "/exec/probe/json":
			code := 0
			if scheduled {
				code = 10
			}
			json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": code})
		case path == "/exec/job/start":
			running = true
			w.WriteHeader(200)
		case path == "/exec/job/json":
			json.NewEncoder(w).Encode(map[string]any{"Running": running, "ExitCode": exitCode})
		default:
			t.Errorf("unexpected request %s", path)
			w.WriteHeader(404)
		}
	})
	dockerClient, e := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.47"), client.WithHTTPClient(&http.Client{Transport: backupTestTransport{handler}}))
	if e != nil {
		t.Fatal(e)
	}
	defer dockerClient.Close()
	jobs := &backupJobs{docker: &dockerService{client: dockerClient, stackName: "stack"}, service: "backup"}
	original := manualBackups
	manualBackups = jobs
	defer func() { manualBackups = original }()
	if s := jobs.snapshot(context.Background()); s.State != "idle" || !s.Available {
		t.Fatalf("%+v", s)
	}
	w := httptest.NewRecorder()
	createBackupHandler(jobs).ServeHTTP(w, httptest.NewRequest("POST", "/api/backups", nil))
	if w.Code != 202 || created != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	createBackupHandler(jobs).ServeHTTP(w, httptest.NewRequest("POST", "/api/backups", nil))
	if w.Code != 409 || created != 1 {
		t.Fatal("duplicate backup allowed")
	}
	called := false
	guard := guardedVolumeAction(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	w = httptest.NewRecorder()
	guard.ServeHTTP(w, httptest.NewRequest("POST", "/api/start", nil))
	if w.Code != 409 || called {
		t.Fatal("startup allowed during backup")
	}
	running = false
	if s := jobs.snapshot(context.Background()); s.State != "succeeded" {
		t.Fatalf("%+v", s)
	}
	scheduled = true
	if !backupBlocksControls() {
		t.Fatal("scheduled backup ignored")
	}
	scheduled = false
	failTop = true
	var errorLog bytes.Buffer
	previousLogOutput := log.Writer()
	log.SetOutput(&errorLog)
	defer log.SetOutput(previousLogOutput)
	if !backupBlocksControls() {
		t.Fatal("unknown backup state allowed startup")
	}
	if output := errorLog.String(); !strings.Contains(output, "list backup processes") || !strings.Contains(output, "unavailable") || !strings.Contains(output, `service="backup"`) {
		t.Fatalf("missing diagnostic context: %s", output)
	}
	if !backupBlocksControls() {
		t.Fatal("unknown status no longer blocks controls")
	}
	if count := strings.Count(errorLog.String(), "backup status:"); count != 1 {
		t.Fatalf("repeated polling logged %d errors", count)
	}
	failTop = false
	w = httptest.NewRecorder()
	createBackupHandler(jobs).ServeHTTP(w, httptest.NewRequest("POST", "/api/backups", nil))
	running = false
	exitCode = 1
	if s := jobs.snapshot(context.Background()); s.State != "failed" {
		t.Fatalf("%+v", s)
	}
}

type backupTestTransport struct{ handler http.Handler }

func (t backupTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestScheduledBackupLockProbe(t *testing.T) {
	for _, tc := range []struct {
		name, fdinfo string
		code         int
	}{
		{"idle scheduler", "pos: 0\nflags: 0100002\n", 0},
		{"scheduled backup", "pos: 0\nlock: 1: FLOCK ADVISORY WRITE 1 00:22:42 0 EOF\n", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"1/fd", "1/fdinfo"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/var/lock/dockervolumebackup.lock", filepath.Join(root, "1/fd/7")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "1/fdinfo/7"), []byte(tc.fdinfo), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", backupLockProbe, "minedash-backup-status", root)
			err := cmd.Run()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("exit code %d, want %d", code, tc.code)
			}
		})
	}
}
