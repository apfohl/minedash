package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func restoreArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, v := range files {
		if e := tw.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(v))}); e != nil {
			t.Fatal(e)
		}
		tw.Write([]byte(v))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func TestRestoreReplacement(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "obsolete"), []byte("old"), 0600)
	os.WriteFile(filepath.Join(root, ".hidden"), []byte("old"), 0600)
	e := replaceVolume(root, "backup", bytes.NewReader(restoreArchive(t, map[string]string{"world/level.dat": "new", "server.properties": "settings"})), os.Getuid(), os.Getgid(), func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if restorePending(root) {
		t.Fatal("workspace remains")
	}
	if _, e = os.Stat(filepath.Join(root, "obsolete")); !os.IsNotExist(e) {
		t.Fatal("obsolete retained")
	}
	if _, e = os.Stat(filepath.Join(root, ".hidden")); !os.IsNotExist(e) {
		t.Fatal("hidden retained")
	}
	b, _ := os.ReadFile(filepath.Join(root, "world/level.dat"))
	if string(b) != "new" {
		t.Fatal(string(b))
	}
}
func TestRestoreRejectsUnsafeAndCorrupt(t *testing.T) {
	for _, data := range [][]byte{restoreArchive(t, map[string]string{"../escape": "bad"}), restoreArchive(t, map[string]string{restoreWorkspace + "/journal.json": "bad"}), []byte("invalid")} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "original"), []byte("old"), 0600)
		if e := replaceVolume(root, "backup", bytes.NewReader(data), os.Getuid(), os.Getgid(), func() error { return nil }); e == nil {
			t.Fatal("accepted")
		}
		if restorePending(root) {
			t.Fatal("workspace remains")
		}
		if b, _ := os.ReadFile(filepath.Join(root, "original")); string(b) != "old" {
			t.Fatal("original changed")
		}
	}
}
func TestRestoreStoppedRecheck(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "original"), []byte("old"), 0600)
	if e := replaceVolume(root, "backup", bytes.NewReader(restoreArchive(t, map[string]string{"new": "new"})), os.Getuid(), os.Getgid(), func() error { return errors.New("running") }); e == nil {
		t.Fatal("accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "original")); string(b) != "old" {
		t.Fatal("changed")
	}
}
func TestRestoreRecovery(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, restoreWorkspace)
	os.MkdirAll(filepath.Join(work, "previous"), 0700)
	os.MkdirAll(filepath.Join(work, "staged"), 0700)
	os.WriteFile(filepath.Join(work, "previous", "same"), []byte("old"), 0600)
	os.WriteFile(filepath.Join(root, "same"), []byte("new"), 0600)
	os.WriteFile(filepath.Join(work, "staged", "other"), []byte("new"), 0600)
	if e := saveRestoreJournal(root, restoreJournal{Phase: "installing", Original: []string{"same"}, Incoming: []string{"same", "other"}}); e != nil {
		t.Fatal(e)
	}
	if e := recoverRestore(root); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "same")); string(b) != "old" {
		t.Fatal("not rolled back")
	}
	if restorePending(root) {
		t.Fatal("workspace remains")
	}
}

func TestRestoreSavingRecovery(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, restoreWorkspace)
	os.MkdirAll(filepath.Join(work, "previous"), 0700)
	os.MkdirAll(filepath.Join(work, "staged"), 0700)
	os.WriteFile(filepath.Join(work, "previous", "moved"), []byte("old"), 0600)
	os.WriteFile(filepath.Join(root, "untouched"), []byte("old"), 0600)
	saveRestoreJournal(root, restoreJournal{Phase: "saving", Original: []string{"moved", "untouched"}, Incoming: []string{"new"}})
	if e := recoverRestore(root); e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"moved", "untouched"} {
		if b, _ := os.ReadFile(filepath.Join(root, n)); string(b) != "old" {
			t.Fatal(n)
		}
	}
}

func TestRestoreConcurrentGuard(t *testing.T) {
	volumeOperation.Lock()
	defer volumeOperation.Unlock()
	called := false
	handler := guardedVolumeAction(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/start", nil))
	if w.Code != 409 || called {
		t.Fatal("concurrent action was allowed")
	}
}
