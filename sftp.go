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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SFTPStorage implements Storage interface for SFTP.
type SFTPStorage struct {
	host      string
	baseDir   string
	sshClient *ssh.Client
	client    *sftp.Client
}

// NewSFTPStorage creates a new SFTP storage instance.
//
// Host keys are verified against SFTPKnownHostsFile (or ~/.ssh/known_hosts
// and /etc/ssh/ssh_known_hosts) unless SFTPIgnoreKnownHosts is true.
func NewSFTPStorage(config Config) (*SFTPStorage, error) {
	if err := validateConfig(config, map[string]string{
		"host": config.SFTPHost,
	}); err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}

	port := config.SFTPPort
	if port == "" {
		port = "22"
	}
	hostPort := net.JoinHostPort(config.SFTPHost, port)

	username := config.SFTPUsername
	if username == "" {
		currentUser, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("sftp: failed to get current user: %w", err)
		}
		username = currentUser.Username
	}

	authMethods := make([]ssh.AuthMethod, 0, 2)
	if config.SFTPPassword != "" {
		authMethods = append(authMethods, ssh.Password(config.SFTPPassword))
	}
	passphrase := config.SFTPIdentityPassphrase
	if passphrase == "" {
		passphrase = config.SFTPPassword // backwards compatibility
	}
	signers, err := getSSHSigners(config.SFTPIdentityFile, passphrase)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	if len(signers) > 0 {
		authMethods = append(authMethods, ssh.PublicKeys(signers...))
	}

	hostKeyCallback, hostKeyAlgos, err := getHostKeyCallback(config, hostPort)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User:              username,
		Auth:              authMethods,
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           30 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", hostPort, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("sftp: failed to connect to %s: %w", hostPort, err)
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("sftp: failed to open sftp session: %w", err)
	}

	baseDir := normalizePathSeparators(config.SFTPDirectory)
	if baseDir == "" {
		wd, err := sftpClient.Getwd()
		if err != nil {
			_ = sftpClient.Close()
			_ = sshClient.Close()
			return nil, fmt.Errorf("sftp: failed to get working directory: %w", err)
		}
		baseDir = wd
	}

	return &SFTPStorage{
		host:      config.SFTPHost,
		baseDir:   path.Clean(baseDir),
		sshClient: sshClient,
		client:    sftpClient,
	}, nil
}

// Client returns the underlying sftp client.
func (s *SFTPStorage) Client() *sftp.Client { return s.client }

func (s *SFTPStorage) resolve(key string) (string, string, error) {
	k, err := cleanKey(key)
	if err != nil {
		return "", "", fmt.Errorf("sftp: %w", err)
	}
	return k, path.Join(s.baseDir, k), nil
}

func (s *SFTPStorage) item(key string, fi os.FileInfo) Item {
	it := Item{Key: key, ModifiedTime: fi.ModTime(), IsDirectory: fi.IsDir()}
	if !fi.IsDir() {
		it.Size = fi.Size()
	}
	return it
}

func (s *SFTPStorage) mkdirParent(full string) error {
	dir := path.Dir(full)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := s.client.MkdirAll(dir); err != nil {
		return fmt.Errorf("sftp: failed to create directory %s: %w", dir, err)
	}
	return nil
}

// Put streams r to <key>.<rand>.partial and renames it to key on success.
func (s *SFTPStorage) Put(ctx context.Context, key string, r io.Reader, _ *PutOptions) (Item, error) {
	k, full, err := s.resolve(key)
	if err != nil {
		return Item{}, err
	}
	if err := s.mkdirParent(full); err != nil {
		return Item{}, err
	}
	tmp := partialName(full)
	f, err := s.client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return Item{}, fmt.Errorf("sftp: failed to create %s: %w", tmp, err)
	}
	if err := copyAndClose(ctx, f, r); err != nil {
		_ = s.client.Remove(tmp)
		return Item{}, fmt.Errorf("sftp: failed to upload %s: %w", k, err)
	}
	if err := s.rename(tmp, full); err != nil {
		_ = s.client.Remove(tmp)
		return Item{}, err
	}
	return s.Stat(ctx, k)
}

func copyAndClose(ctx context.Context, w io.WriteCloser, r io.Reader) error {
	_, err := io.Copy(w, ctxReader{ctx: ctx, r: r})
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (s *SFTPStorage) rename(from, to string) error {
	if _, ok := s.client.HasExtension("posix-rename@openssh.com"); ok {
		if err := s.client.PosixRename(from, to); err != nil {
			return fmt.Errorf("sftp: failed to rename %s to %s: %w", from, to, err)
		}
		return nil
	}
	if err := s.client.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sftp: failed to replace %s: %w", to, err)
	}
	if err := s.client.Rename(from, to); err != nil {
		return fmt.Errorf("sftp: failed to rename %s to %s: %w", from, to, err)
	}
	return nil
}

// PutIf supports only Condition.IfNoneMatch (via O_EXCL on the final path).
// IfMatch returns ErrConditionNotSupported.
func (s *SFTPStorage) PutIf(ctx context.Context, key string, r io.Reader, cond Condition, _ *PutOptions) (Item, error) {
	if cond.IfMatch != "" || !cond.IfNoneMatch {
		return Item{}, fmt.Errorf("sftp: IfMatch: %w", ErrConditionNotSupported)
	}
	k, full, err := s.resolve(key)
	if err != nil {
		return Item{}, err
	}
	if err := s.mkdirParent(full); err != nil {
		return Item{}, err
	}
	f, err := s.client.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		if _, serr := s.client.Lstat(full); serr == nil {
			return Item{}, fmt.Errorf("sftp: %s already exists: %w", k, ErrPreconditionFailed)
		}
		return Item{}, fmt.Errorf("sftp: failed to create %s: %w", k, err)
	}
	if err := copyAndClose(ctx, f, r); err != nil {
		_ = s.client.Remove(full)
		return Item{}, fmt.Errorf("sftp: failed to upload %s: %w", k, err)
	}
	return s.Stat(ctx, k)
}

// Get opens key for streaming reads.
func (s *SFTPStorage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, Item{}, err
	}
	k, full, err := s.resolve(key)
	if err != nil {
		return nil, Item{}, err
	}
	f, err := s.client.Open(full)
	if err != nil {
		return nil, Item{}, mapSFTPErr("get", k, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Item{}, mapSFTPErr("get", k, err)
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, Item{}, fmt.Errorf("sftp: %s is a directory", k)
	}
	return f, s.item(k, fi), nil
}

// Stat returns metadata for key.
func (s *SFTPStorage) Stat(ctx context.Context, key string) (Item, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	k, full, err := s.resolve(key)
	if err != nil {
		return Item{}, err
	}
	fi, err := s.client.Stat(full)
	if err != nil {
		return Item{}, mapSFTPErr("stat", k, err)
	}
	return s.item(k, fi), nil
}

func mapSFTPErr(op, key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, sftp.ErrSSHFxNoSuchFile) {
		return fmt.Errorf("sftp: %s %s: %w", op, key, ErrNotFound)
	}
	var se *sftp.StatusError
	if errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxNoSuchFile {
		return fmt.Errorf("sftp: %s %s: %w", op, key, ErrNotFound)
	}
	return fmt.Errorf("sftp: %s %s: %w", op, key, err)
}

// Upload uploads a file via SFTP.
func (s *SFTPStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	return uploadFile(ctx, s, localPath, remotePath)
}

// Download downloads a file via SFTP.
func (s *SFTPStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	return downloadFile(ctx, s, remotePath, localPath)
}

// List lists files and directories (recursively) whose key starts with prefix.
func (s *SFTPStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	items := make([]Item, 0)
	root := s.baseDir
	if i := strings.LastIndex(p, "/"); i >= 0 {
		root = path.Join(s.baseDir, p[:i])
	}
	walker := s.client.Walk(root)
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := walker.Err(); err != nil {
			if walker.Path() == root && errors.Is(err, fs.ErrNotExist) {
				return items, nil
			}
			continue
		}
		full := walker.Path()
		if full == s.baseDir {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(full, s.baseDir), "/")
		stat := walker.Stat()
		if !strings.HasPrefix(rel, p) {
			if stat.IsDir() && !strings.HasPrefix(p, rel+"/") {
				walker.SkipDir()
			}
			continue
		}
		items = append(items, s.item(rel, stat))
	}
	return items, nil
}

// ListDir lists the files directly inside dir.
func (s *SFTPStorage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	entries, err := s.client.ReadDir(path.Join(s.baseDir, d))
	if errors.Is(err, fs.ErrNotExist) {
		return []Item{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sftp: failed to read directory %s: %w", dir, err)
	}
	items := make([]Item, 0, len(entries))
	for _, fi := range entries {
		if !fi.Mode().IsRegular() {
			continue
		}
		items = append(items, s.item(path.Join(d, fi.Name()), fi))
	}
	return items, nil
}

// Remove deletes a file via SFTP.
func (s *SFTPStorage) Remove(ctx context.Context, remotePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, full, err := s.resolve(remotePath)
	if err != nil {
		return err
	}
	if err := s.client.Remove(full); err != nil {
		return mapSFTPErr("remove", k, err)
	}
	return nil
}

// Close releases resources.
func (s *SFTPStorage) Close() error {
	err := s.client.Close()
	if cerr := s.sshClient.Close(); err == nil {
		err = cerr
	}
	return err
}

// getSSHSigners returns SSH signers from identity file and SSH agent.
func getSSHSigners(identityFile string, passphrase string) ([]ssh.Signer, error) {
	signers := make([]ssh.Signer, 0)

	if identityFile != "" {
		p, err := expandHomeDir(identityFile)
		if err != nil {
			return nil, fmt.Errorf("failed to expand identity file path: %w", err)
		}

		keyData, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("failed to read identity file %s: %w", p, err)
		}

		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if !errors.As(err, &passErr) {
				return nil, fmt.Errorf("failed to parse identity file: %w", err)
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt identity file: %w", err)
			}
		}
		signers = append(signers, signer)
	}

	// Keys from a running SSH agent, if any.
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		if conn, err := net.Dial("unix", socket); err == nil {
			if agentSigners, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, agentSigners...)
			}
		}
	}

	return signers, nil
}

// getHostKeyCallback builds the host key verification callback and the list
// of host key algorithms known for hostPort (so the server negotiates a key
// type present in known_hosts).
func getHostKeyCallback(config Config, hostPort string) (ssh.HostKeyCallback, []string, error) {
	if config.SFTPIgnoreKnownHosts {
		return ssh.InsecureIgnoreHostKey(), nil, nil //nolint:gosec // explicitly requested
	}

	files := make([]string, 0, 2)
	if config.SFTPKnownHostsFile != "" {
		p, err := expandHomeDir(config.SFTPKnownHostsFile)
		if err != nil {
			return nil, nil, err
		}
		if _, err := os.Stat(p); err != nil {
			return nil, nil, fmt.Errorf("known_hosts file %s: %w", p, err)
		}
		files = append(files, p)
	} else {
		for _, candidate := range []string{"~/.ssh/known_hosts", "/etc/ssh/ssh_known_hosts"} {
			p, err := expandHomeDir(candidate)
			if err != nil {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no known_hosts file available for host key verification " +
			"(set SSH_KNOWN_HOSTS or SSH_INSECURE_IGNORE_HOST_KEY=true)")
	}

	callback, err := knownhosts.New(files...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load known_hosts: %w", err)
	}
	return callback, knownHostAlgorithms(callback, hostPort), nil
}

// knownHostAlgorithms returns the host key algorithms recorded for hostPort,
// using the KeyError returned for a deliberately unknown key.
func knownHostAlgorithms(cb ssh.HostKeyCallback, hostPort string) []string {
	addr, err := net.ResolveTCPAddr("tcp", hostPort)
	if err != nil {
		addr = &net.TCPAddr{}
	}
	var keyErr *knownhosts.KeyError
	if err := cb(hostPort, addr, probeKey{}); !errors.As(err, &keyErr) {
		return nil
	}
	algos := make([]string, 0, len(keyErr.Want))
	seen := map[string]bool{}
	for _, k := range keyErr.Want {
		for _, a := range algorithmsForKeyType(k.Key.Type()) {
			if !seen[a] {
				seen[a] = true
				algos = append(algos, a)
			}
		}
	}
	if len(algos) == 0 {
		return nil
	}
	return algos
}

func algorithmsForKeyType(t string) []string {
	if t == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{t}
}

// probeKey is a public key that never matches anything in known_hosts.
type probeKey struct{}

func (probeKey) Type() string                        { return "probe-key" }
func (probeKey) Marshal() []byte                     { return []byte("probe-key") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key") }

// expandHomeDir expands ~ in file paths to home directory.
func expandHomeDir(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return filepath.Clean(p), nil
	}

	parts := strings.SplitN(p, "/", 2)
	username := strings.TrimPrefix(parts[0], "~")

	var homeDir string
	if username == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			currentUser, err := user.Current()
			if err != nil {
				return "", fmt.Errorf("failed to get home directory: %w", err)
			}
			h = currentUser.HomeDir
		}
		homeDir = h
	} else {
		userInfo, err := user.Lookup(username)
		if err != nil {
			return "", fmt.Errorf("failed to lookup user %s: %w", username, err)
		}
		homeDir = userInfo.HomeDir
	}

	if homeDir == "" {
		return "", fmt.Errorf("empty home directory")
	}
	if len(parts) == 1 {
		return homeDir, nil
	}
	return filepath.Join(homeDir, parts[1]), nil
}
