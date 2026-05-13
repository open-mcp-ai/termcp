package sshconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRemoteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureInternal(dir); err != nil {
		t.Fatal(err)
	}
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
	if err := InitRemoteSkeleton(dir, "prod"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ssh_configs", "prod", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["host"] = "h.example"
	m["user"] = "u"
	m["password"] = "p"
	out, _ := json.Marshal(m)
	if err := s.Save("prod", out); err != nil {
		t.Fatal(err)
	}
	e, err := s.Load("prod")
	if err != nil || e.Host != "h.example" {
		t.Fatalf("load prod: %+v %v", e, err)
	}
}
