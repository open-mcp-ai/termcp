package sshclient

import (
	"strings"
	"testing"
)

func TestParseProxyURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr string
		host    string
		port    int
		user    string
		pass    string
	}{
		{"", "", "", 0, "", ""},
		{"socks5://127.0.0.1:1080", "", "127.0.0.1", 1080, "", ""},
		{"socks5://u:p@10.0.0.1:1080", "", "10.0.0.1", 1080, "u", "p"},
		{"socks5://user@host:1080", "", "host", 1080, "user", ""},
		{"socks5://127.0.0.1", "", "127.0.0.1", 1080, "", ""},
		{"127.0.0.1:1080", "", "127.0.0.1", 1080, "", ""},
		{"127.0.0.1", "", "127.0.0.1", 1080, "", ""},
		{"u:p@127.0.0.1:1080", "", "127.0.0.1", 1080, "u", "p"},
		{"http://127.0.0.1:1080", "unsupported", "", 0, "", ""},
		{"socks5://:1080", "host is required", "", 0, "", ""},
		{"socks5://127.0.0.1:99999", "invalid proxy port", "", 0, "", ""},
	}
	for i, c := range cases {
		p, err := ParseProxyURL(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("case %d: want err %q, got %v", i, c.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("case %d: unexpected err %v", i, err)
		}
		if c.host == "" {
			if p != nil {
				t.Fatalf("case %d: expected nil proxy, got %+v", i, p)
			}
			continue
		}
		if p.Host != c.host || p.Port != c.port || p.Username != c.user || p.Password != c.pass {
			t.Fatalf("case %d: got %+v want host=%s port=%d user=%s pass=%s", i, p, c.host, c.port, c.user, c.pass)
		}
	}
}

func TestParseProxyURLEncodedCreds(t *testing.T) {
	// url.Parse decodes percent-escaping; p@ss with special char ":" would split user/pass,
	// so encode the password.
	p, err := ParseProxyURL("socks5://u:p%40ss@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if p.Password != "p@ss" {
		t.Fatalf("password decode: got %q want %q", p.Password, "p@ss")
	}
	if !p.Enabled() {
		t.Fatal("expected enabled")
	}
}
