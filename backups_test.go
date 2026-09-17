package main

import (
	"encoding/json"
	"mime"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

func TestBackupDownload(t *testing.T) {
	dir := t.TempDir()
	name := "grassblock-2026-09-17T04-00-00.tar.gz"
	content := "test archive contents"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := "grassblock-2026-09-18T04-00-00.tar.gz"
	if err := os.Symlink(outside, filepath.Join(dir, alias)); err != nil {
		t.Fatal(err)
	}
	directory := "grassblock-2026-09-19T04-00-00.tar.gz"
	if err := os.Mkdir(filepath.Join(dir, directory), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BackupPath: dir, BackupPrefix: "grassblock", JWTSecret: "test-secret"}
	mux := http.NewServeMux()
	mux.Handle("GET /api/backups/{name}/download", jwtMiddleware(cfg, backupDownloadHandler(cfg)))
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))})
	signed, err := token.SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, rangeHeader string
		auth                    bool
		status                  int
		body                    string
	}{
		{name: "complete", file: name, auth: true, status: 200, body: content},
		{name: "resumed", file: name, rangeHeader: "bytes=5-11", auth: true, status: 206, body: content[5:12]},
		{name: "anonymous", file: name, status: 401},
		{name: "missing", file: "grassblock-2026-09-20T04-00-00.tar.gz", auth: true, status: 404},
		{name: "wrong prefix", file: "world-2026-09-17T04-00-00.tar.gz", auth: true, status: 404},
		{name: "latest", file: "grassblock.latest.tar.gz", auth: true, status: 404},
		{name: "invalid date", file: "grassblock-2026-02-30T04-00-00.tar.gz", auth: true, status: 404},
		{name: "symlink", file: alias, auth: true, status: 404},
		{name: "directory", file: directory, auth: true, status: 404},
		{name: "traversal", file: "..%2F" + name, auth: true, status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/backups/"+tc.file+"/download", nil)
			if tc.auth {
				r.AddCookie(&http.Cookie{Name: cookieName, Value: signed})
			}
			if tc.rangeHeader != "" {
				r.Header.Set("Range", tc.rangeHeader)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body)
			}
			if tc.body != "" {
				if w.Body.String() != tc.body {
					t.Fatalf("body = %q", w.Body.String())
				}
				if w.Header().Get("Content-Type") != "application/gzip" || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("headers: %v", w.Header())
				}
				disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
				if err != nil || disposition != "attachment" || params["filename"] != name {
					t.Fatalf("disposition = %v %v %v", disposition, params, err)
				}
				if w.Header().Get("Accept-Ranges") != "bytes" {
					t.Fatal("missing range support")
				}
			} else if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), content) {
				t.Fatal("unexpected file disclosure")
			}
		})
	}
}

func TestBackupDownloadInvalidSessions(t *testing.T) {
	cfg := Config{JWTSecret: "test-secret", BackupPath: t.TempDir(), BackupPrefix: "world"}
	name := "world-2026-09-17T04-00-00.tar.gz"
	if err := os.WriteFile(filepath.Join(cfg.BackupPath, name), []byte("private archive"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/backups/{name}/download", jwtMiddleware(cfg, backupDownloadHandler(cfg)))
	for _, tc := range []struct {
		name, secret string
		expiry       time.Time
	}{
		{name: "expired", secret: cfg.JWTSecret, expiry: time.Now().Add(-time.Hour)},
		{name: "forged", secret: "wrong-secret", expiry: time.Now().Add(time.Hour)},
		{name: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := "invalid-token"
			if tc.secret != "" {
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(tc.expiry)})
				var err error
				value, err = token.SignedString([]byte(tc.secret))
				if err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest(http.MethodGet, "/api/backups/"+name+"/download", nil)
			r.AddCookie(&http.Cookie{Name: cookieName, Value: value})
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "private archive") || w.Header().Get("Content-Disposition") != "" {
				t.Fatalf("invalid session response: %d %v %s", w.Code, w.Header(), w.Body)
			}
		})
	}
}
