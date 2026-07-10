package plugins

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUpdate(t *testing.T) {
	t.Run("fails when the plugin does not exist", func(t *testing.T) {
		dir := t.TempDir()
		m := New(dir, 1430)
		zipBytes := buildZip(t, map[string]string{"plugin.json": `{"id":"demo"}`})

		if err := m.Update("demo", zipBytes); err == nil {
			t.Fatal("expected an error, got nil")
		}
		if _, err := os.Stat(filepath.Join(dir, "demo")); !os.IsNotExist(err) {
			t.Fatalf("expected no folder to be created, stat err = %v", err)
		}
	})

	t.Run("overwrites a file present in the new bundle", func(t *testing.T) {
		dir := t.TempDir()
		m := New(dir, 1430)
		pluginDir := filepath.Join(dir, "demo")
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"id":"demo","version":"1.0.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}

		zipBytes := buildZip(t, map[string]string{"plugin.json": `{"id":"demo","version":"2.0.0"}`})
		if err := m.Update("demo", zipBytes); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"id":"demo","version":"2.0.0"}` {
			t.Fatalf("plugin.json = %q, want the new bundle's content", got)
		}
	})

	t.Run("leaves a local-only file alone", func(t *testing.T) {
		dir := t.TempDir()
		m := New(dir, 1430)
		pluginDir := filepath.Join(dir, "demo")
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"id":"demo"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "local-state.txt"), []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}

		zipBytes := buildZip(t, map[string]string{"plugin.json": `{"id":"demo","version":"2.0.0"}`})
		if err := m.Update("demo", zipBytes); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := os.ReadFile(filepath.Join(pluginDir, "local-state.txt"))
		if err != nil {
			t.Fatalf("local-state.txt should still exist: %v", err)
		}
		if string(got) != "keep me" {
			t.Fatalf("local-state.txt = %q, want unchanged", got)
		}
	})

	t.Run("rejects a path-escape entry", func(t *testing.T) {
		dir := t.TempDir()
		m := New(dir, 1430)
		pluginDir := filepath.Join(dir, "demo")
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"id":"demo"}`), 0o644); err != nil {
			t.Fatal(err)
		}

		zipBytes := buildZip(t, map[string]string{"../escape.txt": "pwned"})
		if err := m.Update("demo", zipBytes); err == nil {
			t.Fatal("expected an error for a path-escape entry, got nil")
		}
	})
}
