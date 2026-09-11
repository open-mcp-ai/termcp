package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestStoreRemoteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "internal" {
		t.Fatalf("list: %#v", names)
	}
	in, err := s.Load("internal")
	if err != nil || in.Kind != KindInternal {
		t.Fatalf("internal: %+v err %v", in, err)
	}
	// Virtual internal must not create a disk directory.
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "internal")); !os.IsNotExist(err) {
		t.Fatalf("expected no on-disk internal dir, stat err=%v", err)
	}
	if err := InitRemoteSkeleton(dir, "prod"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ssh_configs", "prod", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["host"] = "h.example"
	m["user"] = "u"
	m["password"] = "p"
	out, _ := toml.Marshal(m)
	if err := s.Save("prod", out); err != nil {
		t.Fatal(err)
	}
	e, err := s.Load("prod")
	if err != nil || e.Host != "h.example" {
		t.Fatalf("load prod: %+v %v", e, err)
	}
	// Leftover disk internal dir is ignored; still only one "internal" in list.
	if err := os.MkdirAll(filepath.Join(dir, "ssh_configs", "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_configs", "internal", "config.toml"), []byte("kind = \"remote\"\nhost=\"x\"\nuser=\"u\"\npassword=\"p\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	names, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	var internals int
	for _, n := range names {
		if n == "internal" {
			internals++
		}
	}
	if internals != 1 {
		t.Fatalf("list should have exactly one internal, got %#v", names)
	}
	in, err = s.Load("internal")
	if err != nil || in.Kind != KindInternal {
		t.Fatalf("disk leftover must not override virtual internal: %+v %v", in, err)
	}
	if err := s.Save("internal", InternalTemplate()); err == nil {
		t.Fatal("expected Save(internal) to fail")
	}
}

func TestLoad_UnknownReturnsErrNotFound(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("no-such-config"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
