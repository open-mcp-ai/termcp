package sshclient

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DialAuth configures SSH client authentication and host verification for a
// non-internal (remote) server.
type DialAuth struct {
	User              string
	Password          string
	PrivateKeyPEM     string
	KeyPassphrase     string
	TrustUnknownHost  bool
	KnownHostsContent string
	DialTimeout       time.Duration
}

// BuildClientConfig builds an ssh.ClientConfig for dialing a user-supplied host.
// At least one of Password or PrivateKeyPEM must be non-empty.
// If TrustUnknownHost is true, host keys are not verified (insecure).
// If false, KnownHostsContent must contain OpenSSH known_hosts lines.
func BuildClientConfig(auth DialAuth) (*ssh.ClientConfig, error) {
	user := strings.TrimSpace(auth.User)
	if user == "" {
		return nil, fmt.Errorf("ssh_user is required for remote SSH")
	}
	pw := auth.Password
	keyPEM := strings.TrimSpace(auth.PrivateKeyPEM)
	if pw == "" && keyPEM == "" {
		return nil, fmt.Errorf("provide ssh_password and/or ssh_private_key_pem for remote SSH")
	}

	var methods []ssh.AuthMethod
	if pw != "" {
		methods = append(methods, ssh.Password(pw))
	}
	if keyPEM != "" {
		signer, err := parsePrivateKey([]byte(keyPEM), auth.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("ssh private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	var hostKeyCallback ssh.HostKeyCallback
	if auth.TrustUnknownHost {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		kh := strings.TrimSpace(auth.KnownHostsContent)
		if kh == "" {
			return nil, fmt.Errorf("ssh_known_hosts is required when ssh_trust_unknown_host is false")
		}
		f, err := os.CreateTemp("", "termcp-known_hosts-*")
		if err != nil {
			return nil, err
		}
		path := f.Name()
		if _, err := f.WriteString(kh); err != nil {
			f.Close()
			_ = os.Remove(path)
			return nil, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		cb, err := knownhosts.New(path)
		_ = os.Remove(path)
		if err != nil {
			return nil, fmt.Errorf("ssh_known_hosts: %w", err)
		}
		hostKeyCallback = cb
	}

	timeout := auth.DialTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}, nil
}

func parsePrivateKey(pemBytes []byte, passphrase string) (ssh.Signer, error) {
	if strings.TrimSpace(passphrase) != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pemBytes, []byte(passphrase))
		if err != nil {
			return nil, err
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return signer, nil
}
