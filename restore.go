package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const restoreWorkspace = ".minedash-restore"
const volumePath = "/mc-data"

var volumeOperation sync.Mutex

type restoreJournal struct {
	Phase    string   `json:"phase"`
	Backup   string   `json:"backup"`
	Original []string `json:"original"`
	Incoming []string `json:"incoming"`
}

func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func saveRestoreJournal(root string, j restoreJournal) error {
	work := filepath.Join(root, restoreWorkspace)
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(work, "journal.tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(filepath.Join(work, "journal.tmp"), filepath.Join(work, "journal.json")); e != nil {
		return e
	}
	return syncDirectory(work)
}
func moveRestoreEntry(from, to string) error {
	if e := os.Rename(from, to); e != nil {
		return e
	}
	if e := syncDirectory(filepath.Dir(from)); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(to))
}
func entryNames(root string) ([]string, error) {
	entries, e := os.ReadDir(root)
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, v := range entries {
		if v.Name() != restoreWorkspace {
			names = append(names, v.Name())
		}
	}
	return names, nil
}
func restorePending(root string) bool {
	_, e := os.Lstat(filepath.Join(root, restoreWorkspace))
	return !errors.Is(e, os.ErrNotExist)
}

// Recovery uses the complete move lists recorded before either replacement phase.
func recoverRestore(root string) error {
	work := filepath.Join(root, restoreWorkspace)
	b, e := os.ReadFile(filepath.Join(work, "journal.json"))
	if errors.Is(e, os.ErrNotExist) {
		entries, scanErr := os.ReadDir(work)
		// Cleanup can crash after deleting the journal and before removing its empty directory.
		if scanErr == nil && len(entries) == 0 {
			if e = os.Remove(work); e != nil {
				return e
			}
			return syncDirectory(root)
		}
	}
	if e != nil {
		return e
	}
	var j restoreJournal
	if e = json.Unmarshal(b, &j); e != nil {
		return e
	}
	for _, name := range append(append([]string{}, j.Original...), j.Incoming...) {
		if name == "" || name == restoreWorkspace || filepath.Base(name) != name || name == "." || name == ".." {
			return fmt.Errorf("invalid recovery entry")
		}
	}
	switch j.Phase {
	case "extracting", "complete":
	case "saving", "installing", "rolling-back":
		if j.Phase != "saving" {
			j.Phase = "rolling-back"
			if e = saveRestoreJournal(root, j); e != nil {
				return e
			}
			for _, name := range j.Incoming {
				// An absent staged entry means it was installed. Move it back before restoring originals.
				stage := filepath.Join(work, "staged", name)
				if _, e = os.Lstat(stage); errors.Is(e, os.ErrNotExist) {
					live := filepath.Join(root, name)
					if _, e = os.Lstat(live); e == nil {
						if e = moveRestoreEntry(live, stage); e != nil {
							return e
						}
					} else if !errors.Is(e, os.ErrNotExist) {
						return e
					}
				} else if e != nil {
					return e
				}
			}
		}
		for _, name := range j.Original {
			old := filepath.Join(work, "previous", name)
			if _, e = os.Lstat(old); e == nil {
				if e = moveRestoreEntry(old, filepath.Join(root, name)); e != nil {
					return e
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
	default:
		return fmt.Errorf("unknown restore phase")
	}
	// Rollback is finished; later recovery must only clean disposable data.
	if j.Phase != "complete" && j.Phase != "extracting" {
		j.Phase = "extracting"
		if e = saveRestoreJournal(root, j); e != nil {
			return e
		}
	}
	// Keep the journal until disposable data is gone, so cleanup can resume.
	for _, dir := range []string{"staged", "previous"} {
		if e = os.RemoveAll(filepath.Join(work, dir)); e != nil {
			return e
		}
	}
	if e = syncDirectory(work); e != nil {
		return e
	}
	if e = os.RemoveAll(work); e != nil {
		return e
	}
	return syncDirectory(root)
}

func extractRestore(r io.Reader, dest string, uid, gid int) error {
	gz, e := gzip.NewReader(r)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = filepath.Clean(name)
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") || strings.Split(name, string(filepath.Separator))[0] == restoreWorkspace {
			return fmt.Errorf("unsafe archive path: %q", h.Name)
		}
		target := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			e = os.MkdirAll(target, 0755)
		case tar.TypeReg, tar.TypeRegA:
			var space syscall.Statfs_t
			if e = syscall.Statfs(dest, &space); e != nil {
				return e
			}
			if h.Size < 0 || uint64(h.Size) > uint64(space.Bavail)*uint64(space.Bsize) {
				return fmt.Errorf("insufficient space to extract backup")
			}
			if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
				return e
			}
			var f *os.File
			f, e = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
			if e != nil {
				return e
			}
			_, e = io.CopyN(f, tr, h.Size)
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e == nil {
				e = ce
			}
			count++
		default:
			return fmt.Errorf("unsupported archive entry: %q", h.Name)
		}
		if e != nil {
			return e
		}
	}
	// Read through gzip's trailer to verify its checksum.
	if _, e = io.Copy(io.Discard, gz); e != nil {
		return e
	}
	if count == 0 {
		return fmt.Errorf("backup is empty")
	}
	if e = chownTree(dest, uid, gid); e != nil {
		return e
	}
	return filepath.WalkDir(dest, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return syncDirectory(p)
		}
		return nil
	})
}
func replaceVolume(root, name string, r io.Reader, uid, gid int, check func() error) (err error) {
	work := filepath.Join(root, restoreWorkspace)
	if err = os.Mkdir(work, 0700); err != nil {
		return err
	}
	j := restoreJournal{Phase: "extracting", Backup: name}
	if err = saveRestoreJournal(root, j); err != nil {
		return err
	}
	if err = syncDirectory(root); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if recovery := recoverRestore(root); recovery != nil {
				err = fmt.Errorf("%w; recovery failed: %v", err, recovery)
			}
		}
	}()
	for _, dir := range []string{"staged", "previous"} {
		if err = os.Mkdir(filepath.Join(work, dir), 0700); err != nil {
			return err
		}
	}
	if err = extractRestore(r, filepath.Join(work, "staged"), uid, gid); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if j.Original, err = entryNames(root); err != nil {
		return err
	}
	if j.Incoming, err = entryNames(filepath.Join(work, "staged")); err != nil {
		return err
	}
	j.Phase = "saving"
	if err = saveRestoreJournal(root, j); err != nil {
		return err
	}
	for _, n := range j.Original {
		if err = moveRestoreEntry(filepath.Join(root, n), filepath.Join(work, "previous", n)); err != nil {
			return err
		}
	}
	j.Phase = "installing"
	if err = saveRestoreJournal(root, j); err != nil {
		return err
	}
	for _, n := range j.Incoming {
		if err = moveRestoreEntry(filepath.Join(work, "staged", n), filepath.Join(root, n)); err != nil {
			return err
		}
	}
	j.Phase = "complete"
	if err = saveRestoreJournal(root, j); err != nil {
		return err
	}
	return recoverRestore(root)
}
func (d *dockerService) requireStopped(ctx context.Context) error {
	id, e := d.resolve(ctx)
	if e != nil {
		return e
	}
	info, e := d.client.ContainerInspect(ctx, id)
	if e != nil {
		return e
	}
	if info.State == nil || info.State.Running || info.State.Paused || info.State.Restarting || (info.State.Status != "exited" && info.State.Status != "created" && info.State.Status != "dead") {
		return fmt.Errorf("server must be stopped before restoring")
	}
	return nil
}
func guardedVolumeAction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !volumeOperation.TryLock() {
			errorJSON(w, 409, "A server data operation is in progress.")
			return
		}
		defer volumeOperation.Unlock()
		if restorePending(volumePath) {
			errorJSON(w, 409, "An unfinished backup restore blocks this action.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func backupRestoreHandler(cfg Config, d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := backupTimestamp(name, cfg.BackupPrefix); !ok {
			errorJSON(w, 404, "Backup not found.")
			return
		}
		if e := d.requireStopped(r.Context()); e != nil {
			errorJSON(w, 409, "Server must be confirmed stopped before restoring.")
			return
		}
		root, e := os.OpenRoot(cfg.BackupPath)
		if e != nil {
			errorJSON(w, 404, "Backup not found.")
			return
		}
		defer root.Close()
		f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			errorJSON(w, 404, "Backup not found.")
			return
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			errorJSON(w, 404, "Backup not found.")
			return
		}
		logRequest(r, "backup restore: "+name)
		// Once accepted, browser disconnects must not interrupt replacement or rollback.
		e = replaceVolume(volumePath, name, f, cfg.MCUID, cfg.MCGID, func() error { return d.requireStopped(context.Background()) })
		if e != nil {
			log.Printf("restore: %v", e)
			errorJSON(w, 500, "Restore failed. If recovery is unfinished, server startup remains blocked.")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "restored"})
	})
}
