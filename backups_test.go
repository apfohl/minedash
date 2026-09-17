package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListBackups(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BackupPath: dir, BackupPrefix: "grassblock"}
	for _, name := range []string{
		"grassblock-2026-09-06T04-00-00.tar.gz",
		"grassblock-2026-09-17T04-00-00.tar.gz",
		"grassblock.latest.tar.gz", // Ignore even when it is a regular file.
		"world-2026-09-17T04-00-00.tar.gz",
		"grassblock-2026-02-30T04-00-00.tar.gz",
		"grassblock-2026-09-17T04-00-00.tar.gz.partial",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("archive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "grassblock-2026-09-18T04-00-00.tar.gz"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "grassblock-2026-09-17T04-00-00.tar.gz"), filepath.Join(dir, "grassblock-2026-09-19T04-00-00.tar.gz")); err != nil {
		t.Fatal(err)
	}
	files, err := listBackups(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Timestamp != "2026-09-17T04-00-00" || files[1].Timestamp != "2026-09-06T04-00-00" || files[0].Size != 7 {
		t.Fatalf("unexpected backups: %+v", files)
	}
	cfg.BackupPrefix = "world"
	files, err = listBackups(cfg)
	if err != nil || len(files) != 1 || files[0].Name != "world-2026-09-17T04-00-00.tar.gz" {
		t.Fatalf("prefix filtering: %+v, %v", files, err)
	}
}

func TestBackupsAvailability(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		w := httptest.NewRecorder()
		backupsHandler(Config{BackupPath: dir, BackupPrefix: "world"}).ServeHTTP(w, httptest.NewRequest("GET", "/api/backups", nil))
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response: %+v", w.Result())
		}
		var data struct {
			Backups []backup `json:"backups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if data.Backups == nil || len(data.Backups) != 0 {
			t.Fatalf("expected empty array: %s", w.Body)
		}
	}
	// A path that is a file is a scan failure, not a silently empty list.
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	backupsHandler(Config{BackupPath: file}).ServeHTTP(w, httptest.NewRequest("GET", "/api/backups", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	jwtMiddleware(Config{JWTSecret: "test"}, backupsHandler(Config{BackupPath: dir})).ServeHTTP(w, httptest.NewRequest("GET", "/api/backups", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", w.Code)
	}
}

func TestBackupConfigOverrides(t *testing.T) {
	t.Setenv("BACKUP_PATH", "/custom-backups")
	t.Setenv("BACKUP_PREFIX", "grassblock")
	cfg := loadConfig()
	if cfg.BackupPath != "/custom-backups" || cfg.BackupPrefix != "grassblock" {
		t.Fatalf("config: %+v", cfg)
	}
}
