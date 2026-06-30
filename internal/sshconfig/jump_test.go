package sshconfig

import (
	"strings"
	"testing"
)

func TestParseAndValidateJumpChain(t *testing.T) {
	toml := `kind = "remote"
host = "target.example"
user = "pi"
password = "pw"

[jump]
host = "bastion1"
port = 2222
user = "hop"
password = "hpw"
trust_unknown_host = true

[jump.jump]
host = "bastion2"
user = "hop2"
private_key = """-----BEGIN OPENSSH PRIVATE KEY-----
x
-----END OPENSSH PRIVATE KEY-----"""
`
	e, err := ParseAndValidate([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Jump == nil || e.Jump.Host != "bastion1" {
		t.Fatalf("jump1: %+v", e.Jump)
	}
	if e.Jump.Port != 2222 {
		t.Fatalf("jump1 port: %d", e.Jump.Port)
	}
	if e.Jump.Jump == nil || e.Jump.Jump.Host != "bastion2" {
		t.Fatalf("jump2: %+v", e.Jump.Jump)
	}
	if !strings.Contains(e.Jump.Jump.PrivateKey, "BEGIN OPENSSH") {
		t.Fatalf("jump2 key lost: %q", e.Jump.Jump.PrivateKey)
	}
	if e.Jump.Jump.Jump != nil {
		t.Fatalf("expected chain end, got %+v", e.Jump.Jump.Jump)
	}

	r, err := RemoteFromEntry(e, "")
	if err != nil {
		t.Fatalf("remote: %v", err)
	}
	if r.Host != "target.example" || r.Jump == nil || r.Jump.Host != "bastion1" {
		t.Fatalf("remote chain: %+v", r)
	}
	if r.Jump.Jump == nil || r.Jump.Jump.Host != "bastion2" || r.Jump.Jump.Port != 22 {
		t.Fatalf("remote jump2: %+v", r.Jump.Jump)
	}
}

func TestParseAndValidateJumpMissingAuth(t *testing.T) {
	toml := `kind = "remote"
host = "t"
user = "u"
password = "p"

[jump]
host = "bastion"
user = "hop"
`
	_, err := ParseAndValidate([]byte(toml))
	if err == nil || !strings.Contains(err.Error(), "password or private_key") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestParseAndValidateJumpBadPort(t *testing.T) {
	toml := `kind = "remote"
host = "t"
user = "u"
password = "p"

[jump]
host = "bastion"
user = "hop"
password = "x"
port = 99999
`
	_, err := ParseAndValidate([]byte(toml))
	if err == nil || !strings.Contains(err.Error(), "invalid port") {
		t.Fatalf("expected port error, got %v", err)
	}
}
