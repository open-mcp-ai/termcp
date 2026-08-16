package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestMigrateDataDir_MovesLegacyIntoMissingTarget(t *testing.T) {
	legacy := t.TempDir()
	target := filepath.Join(t.TempDir(), "data")
	writeTree(t, legacy, map[string]string{
		"sessions.json":                "{}",
		"messages/s1/index.json":       "1",
		"ssh_configs/pi/config.toml":   "host=pi",
		"messages/s1/messages/m1.json": "{}",
	})

	moved, err := migrateDataDir(legacy, target)
	if err != nil {
		t.Fatalf("migrateDataDir() error: %v", err)
	}
	if !moved {
		t.Fatal("expected migration to happen")
	}
	if got := mustReadFile(t, filepath.Join(target, "sessions.json")); got != "{}" {
		t.Fatalf("sessions.json = %q", got)
	}
	if got := mustReadFile(t, filepath.Join(target, "ssh_configs/pi/config.toml")); got != "host=pi" {
		t.Fatalf("ssh config = %q", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir still exists (stat err = %v)", err)
	}
}

func TestMigrateDataDir_SkipsWhenTargetInUse(t *testing.T) {
	legacy := t.TempDir()
	target := t.TempDir()
	writeTree(t, legacy, map[string]string{"sessions.json": "{}"})
	writeTree(t, target, map[string]string{"keep.txt": "x"})

	moved, err := migrateDataDir(legacy, target)
	if err != nil {
		t.Fatalf("migrateDataDir() error: %v", err)
	}
	if moved {
		t.Fatal("expected no migration when target already holds data")
	}
	if _, err := os.Stat(filepath.Join(legacy, "sessions.json")); err != nil {
		t.Fatalf("legacy data was disturbed: %v", err)
	}
}

func TestMigrateDataDir_SkipsWhenLegacyMissingOrEmpty(t *testing.T) {
	target := t.TempDir()

	empty := t.TempDir()
	if moved, err := migrateDataDir(empty, target); err != nil || moved {
		t.Fatalf("empty legacy: moved=%v err=%v", moved, err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if moved, err := migrateDataDir(missing, target); err != nil || moved {
		t.Fatalf("missing legacy: moved=%v err=%v", moved, err)
	}
}

func TestMigrateDataDir_SamePathIsNoop(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"sessions.json": "{}"})

	moved, err := migrateDataDir(dir, dir)
	if err != nil {
		t.Fatalf("migrateDataDir() error: %v", err)
	}
	if moved {
		t.Fatal("expected no migration for identical paths")
	}
}

func TestCopyTree_CopiesNestedTree(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTree(t, src, map[string]string{
		"sessions.json":                "{}",
		"messages/s1/index.json":       "1",
		"messages/s1/messages/m1.json": "{}",
	})

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree() error: %v", err)
	}
	for name, want := range map[string]string{
		"sessions.json":                "{}",
		"messages/s1/index.json":       "1",
		"messages/s1/messages/m1.json": "{}",
	} {
		if got := mustReadFile(t, filepath.Join(dst, name)); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}
