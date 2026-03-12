package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// resolveWorldPath returns the world directory path.
// If cfg.WorldPath is set it is used directly (must contain level.dat).
// Otherwise /mc-data is scanned for the first directory containing level.dat.
func resolveWorldPath(cfg Config) (string, error) {
	if cfg.WorldPath != "" {
		if _, err := os.Stat(filepath.Join(cfg.WorldPath, "level.dat")); err != nil {
			return "", fmt.Errorf("WORLD_PATH=%q does not contain level.dat", cfg.WorldPath)
		}
		return cfg.WorldPath, nil
	}
	return detectWorldPath("/mc-data")
}

// detectWorldPath walks root and returns the first directory that contains level.dat.
func detectWorldPath(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if found != "" {
			return filepath.SkipAll
		}
		if !d.IsDir() && d.Name() == "level.dat" {
			found = filepath.Dir(path)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return "", fmt.Errorf("walk %s: %w", root, err)
	}
	if found == "" {
		return "", fmt.Errorf("no level.dat found under %s", root)
	}
	return found, nil
}

// streamWorldZip walks worldPath and writes all files into a ZIP streamed to w.
func streamWorldZip(worldPath string, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	base := filepath.Dir(worldPath)
	return filepath.WalkDir(worldPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		fw, err := zw.Create(rel)
		if err != nil {
			return err
		}

		_, err = io.Copy(fw, f)
		return err
	})
}

// receiveWorldUpload reads a multipart ZIP upload, validates it contains level.dat,
// atomically replaces worldPath with the ZIP contents, then chowns the new tree to uid:gid
// so the Minecraft container (default 1000:1000) can read and write it.
func receiveWorldUpload(r *http.Request, worldPath string, uid, gid int) error {
	// 2 GB max upload size
	if err := r.ParseMultipartForm(2 << 30); err != nil {
		return fmt.Errorf("parse form: %w", err)
	}

	file, _, err := r.FormFile("world")
	if err != nil {
		return fmt.Errorf("form file 'world': %w", err)
	}
	defer file.Close()

	// Write the upload to a temp file so we can use zip.OpenReader which needs io.ReaderAt + size.
	tmp, err := os.CreateTemp("", "minedash-upload-*.zip")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, file)
	if err != nil {
		return fmt.Errorf("write temp: %w", err)
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	// Validate that the ZIP contains a level.dat at the root level of the world.
	if err := validateWorldZip(zr); err != nil {
		return err
	}

	// Unpack to a temporary directory next to the world so rename is atomic (same filesystem).
	parent := filepath.Dir(worldPath)
	tmpDir, err := os.MkdirTemp(parent, "minedash-world-tmp-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // cleaned up on error; on success this is already renamed away

	if err := unpackZip(zr, tmpDir); err != nil {
		return fmt.Errorf("unpack zip: %w", err)
	}

	// Rename current world to world.old (overwrites previous backup).
	oldPath := filepath.Join(parent, "world.old")
	if _, err := os.Stat(worldPath); err == nil {
		if err := os.RemoveAll(oldPath); err != nil {
			return fmt.Errorf("remove old backup: %w", err)
		}
		if err := os.Rename(worldPath, oldPath); err != nil {
			return fmt.Errorf("backup current world: %w", err)
		}
	}

	// Atomic swap: move tmpDir into place.
	if err := os.Rename(tmpDir, worldPath); err != nil {
		return fmt.Errorf("swap world: %w", err)
	}

	// Chown the new world tree so the Minecraft container (uid:gid) can access it.
	if err := chownTree(worldPath, uid, gid); err != nil {
		return fmt.Errorf("chown world: %w", err)
	}

	return nil
}

// chownTree recursively sets ownership of root and all contents to uid:gid.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// validateWorldZip checks that the ZIP contains a level.dat somewhere at depth ≤ 2.
func validateWorldZip(zr *zip.Reader) error {
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "level.dat" {
			return nil
		}
	}
	return errors.New("ZIP does not contain level.dat — is this a valid Minecraft world?")
}

// unpackZip extracts all files from zr into destDir, stripping the top-level directory prefix.
func unpackZip(zr *zip.Reader, destDir string) error {
	// Determine common prefix (the top-level folder inside the ZIP, if any).
	prefix := zipTopPrefix(zr)

	for _, f := range zr.File {
		rel := f.Name
		if prefix != "" {
			rel, _ = filepath.Rel(prefix, f.Name)
		}
		// Guard against zip-slip.
		dest := filepath.Join(destDir, filepath.Clean("/"+rel))

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}

		out, err := os.Create(dest)
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// zipTopPrefix returns the common top-level directory prefix all ZIP entries share, or "".
// ZIP entry paths always use forward slashes per spec, so we split on "/" directly.
func zipTopPrefix(zr *zip.Reader) string {
	if len(zr.File) == 0 {
		return ""
	}
	prefix := ""
	for _, f := range zr.File {
		parts := strings.SplitN(f.Name, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			// Entry sits at root level (no slash) or is the top-level dir entry itself.
			return ""
		}
		top := parts[0]
		if prefix == "" {
			prefix = top
		} else if prefix != top {
			return "" // inconsistent top-level directory — no common prefix
		}
	}
	return prefix
}
