/*
MIT License

Copyright (c) 2024 Jonas Kaninda

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package go_storage

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	testSFTPUser = "backup"
	testSFTPPass = "s3cret"
)

// startSFTPServer runs an in-process SSH/SFTP server on 127.0.0.1:0 and
// returns its address and a known_hosts file containing its host key.
func startSFTPServer(t *testing.T) (host, port, knownHostsFile string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == testSFTPUser && string(pass) == testSFTPPass {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSHConn(conn, cfg)
		}
	}()

	addr := ln.Addr().String()
	knownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, signer.PublicKey())
	if err := os.WriteFile(knownHostsFile, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ = net.SplitHostPort(addr)
	return host, port, knownHostsFile
}

func serveSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if !ok {
					continue
				}
				srv, err := sftp.NewServer(ch)
				if err != nil {
					_ = ch.Close()
					return
				}
				if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
					_ = srv.Close()
				}
				_ = ch.Close()
				return
			}
		}()
	}
}

func newTestSFTP(t *testing.T) (*SFTPStorage, string) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	host, port, kh := startSFTPServer(t)
	base := t.TempDir()
	s, err := NewSFTPStorage(Config{
		SFTPHost:           host,
		SFTPPort:           port,
		SFTPUsername:       testSFTPUser,
		SFTPPassword:       testSFTPPass,
		SFTPDirectory:      base,
		SFTPKnownHostsFile: kh,
	})
	if err != nil {
		t.Fatalf("NewSFTPStorage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, base
}

func TestSFTPConformance(t *testing.T) {
	s, base := newTestSFTP(t)
	runConformance(t, s)

	// No temporary files may survive anywhere under the base directory.
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".partial") {
			t.Errorf("leftover partial file %s", p)
		}
		return nil
	})
}

func TestSFTPHostKeyVerification(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	host, port, _ := startSFTPServer(t)
	base := Config{SFTPHost: host, SFTPPort: port, SFTPUsername: testSFTPUser, SFTPPassword: testSFTPPass, SFTPDirectory: t.TempDir()}

	// A known_hosts file with a different key for this host must be rejected.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherPriv)
	wrong := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, port))}, otherSigner.PublicKey())
	if err := os.WriteFile(wrong, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := base
	cfg.SFTPKnownHostsFile = wrong
	if s, err := NewSFTPStorage(cfg); err == nil {
		_ = s.Close()
		t.Fatal("connected despite host key mismatch")
	}

	// Missing known_hosts file is an error with a helpful message.
	cfg = base
	cfg.SFTPKnownHostsFile = filepath.Join(t.TempDir(), "nope")
	if _, err := NewSFTPStorage(cfg); err == nil {
		t.Fatal("connected without known_hosts")
	}
	t.Setenv("HOME", t.TempDir())
	cfg.SFTPKnownHostsFile = ""
	if _, err := os.Stat("/etc/ssh/ssh_known_hosts"); os.IsNotExist(err) {
		if _, err := NewSFTPStorage(cfg); err == nil || !strings.Contains(err.Error(), "SSH_INSECURE_IGNORE_HOST_KEY") {
			t.Fatalf("expected helpful known_hosts error, got %v", err)
		}
	}

	// Explicitly ignoring host keys works.
	cfg.SFTPIgnoreKnownHosts = true
	s, err := NewSFTPStorage(cfg)
	if err != nil {
		t.Fatalf("SFTPIgnoreKnownHosts: %v", err)
	}
	_ = s.Close()
}
