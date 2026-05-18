package sshclient

import (
	"strings"
	"testing"
	"time"
)

func TestBuildClientConfig_MissingUser(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:             "",
		Password:         "x",
		TrustUnknownHost: true,
		DialTimeout:      time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_user") {
		t.Fatalf("expected ssh_user error, got %v", err)
	}
}

func TestBuildClientConfig_NoAuth(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:             "u",
		TrustUnknownHost: true,
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_password") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestBuildClientConfig_StrictHostWithoutKnownHosts(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:              "u",
		Password:          "p",
		TrustUnknownHost:  false,
		KnownHostsContent: "",
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_known_hosts") {
		t.Fatalf("expected known_hosts error, got %v", err)
	}
}
