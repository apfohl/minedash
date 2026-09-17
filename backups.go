package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const backupTimestampLayout = "2006-01-02T15-04-05"

type backup struct {
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
	Size      int64  `json:"size"`
}

// listBackups reads metadata only, without opening or decompressing archives.
func listBackups(cfg Config) ([]backup, error) {
	backups := make([]backup, 0)
	entries, err := os.ReadDir(cfg.BackupPath)
	if errors.Is(err, os.ErrNotExist) {
		return backups, nil // The optional mount is absent.
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, cfg.BackupPrefix+"-") || !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, cfg.BackupPrefix+"-"), ".tar.gz")
		parsed, err := time.Parse(backupTimestampLayout, stamp)
		if err != nil || parsed.Format(backupTimestampLayout) != stamp {
			continue
		}
		info, err := entry.Info() // Lstat semantics: never follow aliases outside the mount.
		if errors.Is(err, os.ErrNotExist) {
			continue // Retention may remove an archive during the scan.
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		backups = append(backups, backup{Name: name, Timestamp: stamp, Size: info.Size()})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].Timestamp > backups[j].Timestamp })
	return backups, nil
}

func backupsHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		backups, err := listBackups(cfg)
		if err != nil {
			log.Printf("backups: %v", err)
			errorJSON(w, http.StatusServiceUnavailable, "Backup list is temporarily unavailable.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
	})
}
