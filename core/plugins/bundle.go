package plugins

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxBundleBytes = 30 << 20 // 30MB cap for a published bundle

// bundleSkipDirs are folders excluded from a published bundle (re-installed on
// the installer's machine, or noise).
var bundleSkipDirs = map[string]bool{"_deps": true, "node_modules": true, ".git": true, "__pycache__": true, ".venv": true, "venv": true}

// Bundle zips a plugin's folder (minus deps/junk) and returns the zip bytes.
func (m *Manager) Bundle(id string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("invalid plugin id")
	}
	root := filepath.Join(m.dir, id)
	if _, err := os.Stat(filepath.Join(root, "plugin.json")); err != nil {
		return nil, fmt.Errorf("plugin has no plugin.json")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		// Skip excluded dirs (and their contents).
		first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if info.IsDir() {
			if bundleSkipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if bundleSkipDirs[first] {
			return nil
		}
		total += info.Size()
		if total > maxBundleBytes {
			return fmt.Errorf("plugin is too large to publish (>30MB)")
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Extract unzips a downloaded bundle into plugins/<id>/ (rejecting path escapes).
// Fails if the plugin already exists.
func (m *Manager) Extract(id string, zipBytes []byte) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid plugin id")
	}
	dest := filepath.Join(m.dir, id)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("a plugin with id %q already exists", id)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("invalid bundle: %w", err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			return fmt.Errorf("unsafe path in bundle: %s", f.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			_ = os.RemoveAll(dest)
			return err
		}
	}
	return nil
}

// Update overwrites an existing plugin with a newer bundle. Unlike Extract,
// it never deletes the folder first, so local-only files survive.
func (m *Manager) Update(id string, zipBytes []byte) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid plugin id")
	}
	dest := filepath.Join(m.dir, id)
	if _, err := os.Stat(dest); err != nil {
		return fmt.Errorf("no installed plugin with id %q", id)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("invalid bundle: %w", err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			return fmt.Errorf("unsafe path in bundle: %s", f.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, io.LimitReader(rc, maxBundleBytes))
	return err
}
