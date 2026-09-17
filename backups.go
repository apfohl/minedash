package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

const backupTimestampLayout = "2006-01-02T15-04-05"

type backup struct {
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
	Size      int64  `json:"size"`
}

// backupTimestamp validates the same filename rules for listing and downloads.
func backupTimestamp(name, prefix string) (string, bool) {
	if strings.ContainsAny(name, "/\\") || !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ".tar.gz") {
		return "", false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix+"-"), ".tar.gz")
	parsed, err := time.Parse(backupTimestampLayout, stamp)
	return stamp, err == nil && parsed.Format(backupTimestampLayout) == stamp
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
		stamp, valid := backupTimestamp(name, cfg.BackupPrefix)
		if !valid {
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

func backupDownloadHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		name := r.PathValue("name")
		if _, valid := backupTimestamp(name, cfg.BackupPrefix); !valid {
			errorJSON(w, http.StatusNotFound, "Backup not found.")
			return
		}
		root, err := os.OpenRoot(cfg.BackupPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				errorJSON(w, http.StatusNotFound, "Backup not found.")
			} else {
				log.Printf("backup download: %v", err)
				errorJSON(w, http.StatusServiceUnavailable, "Backup is temporarily unavailable.")
			}
			return
		}
		defer root.Close()
		entry, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !entry.Mode().IsRegular()) {
			errorJSON(w, http.StatusNotFound, "Backup not found.")
			return
		}
		if err != nil {
			log.Printf("backup download: lstat: %v", err)
			errorJSON(w, http.StatusServiceUnavailable, "Backup is temporarily unavailable.")
			return
		}
		// Refuse symlinks atomically and avoid blocking if a file becomes a FIFO.
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
				errorJSON(w, http.StatusNotFound, "Backup not found.")
			} else {
				log.Printf("backup download: %v", err)
				errorJSON(w, http.StatusServiceUnavailable, "Backup is temporarily unavailable.")
			}
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			log.Printf("backup download: stat: %v", err)
			errorJSON(w, http.StatusServiceUnavailable, "Backup is temporarily unavailable.")
			return
		}
		if !info.Mode().IsRegular() {
			errorJSON(w, http.StatusNotFound, "Backup not found.")
			return
		}
		logRequest(r, "backup download: "+name)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// ServeContent streams from disk and supports range requests for large archives.
		http.ServeContent(w, r, name, info.ModTime(), file)
	})
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
