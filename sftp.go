package go_storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"

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

	username := config.SFTPUsername
	if username == "" {
		currentUser, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("sftp: failed to get current user: %w", err)
		}
		username = currentUser.Username
	}

	password := config.SFTPPassword
	if password == "" {
		password = os.Getenv("PGBK_SSH_PASS")
	}

	// Build authentication methods
	authMethods := make([]ssh.AuthMethod, 0)

	// Password authentication (if no identity file is specified)
	if config.SFTPIdentityFile == "" && password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}

	// Public key authentication
	signers, err := getSSHSigners(config.SFTPIdentityFile, password)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	if len(signers) > 0 {
		authMethods = append(authMethods, ssh.PublicKeys(signers...))
	}

	// Build SSH client config
	sshConfig := &ssh.ClientConfig{
		User:            username,
		Auth:            authMethods,
		HostKeyCallback: getHostKeyCallback(config.SFTPIgnoreKnownHosts),
	}

	// Connect to SSH server
	hostPort := fmt.Sprintf("%s:%s", config.SFTPHost, port)
	sshClient, err := ssh.Dial("tcp", hostPort, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("sftp: failed to connect to %s: %w", hostPort, err)
	}

	// Open SFTP session
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("sftp: failed to open sftp session: %w", err)
	}

	baseDir := config.SFTPDirectory
	if baseDir == "" {
		wd, err := sftpClient.Getwd()
		if err != nil {
			sftpClient.Close()
			sshClient.Close()
			return nil, fmt.Errorf("sftp: failed to get working directory: %w", err)
		}
		baseDir = wd
	}

	return &SFTPStorage{
		host:      config.SFTPHost,
		baseDir:   baseDir,
		sshClient: sshClient,
		client:    sftpClient,
	}, nil
}

// Upload uploads a file via SFTP.
func (s *SFTPStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("sftp: failed to open local file: %w", err)
	}
	defer src.Close()

	fullRemotePath := filepath.Join(s.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	// Create parent directory if needed
	remoteDir := filepath.Dir(fullRemotePath)
	remoteDir = normalizePathSeparators(remoteDir)

	if remoteDir != "." && remoteDir != "/" {
		if err := s.client.MkdirAll(remoteDir); err != nil {
			return fmt.Errorf("sftp: failed to create directory %s: %w", remoteDir, err)
		}
	}

	dst, err := s.client.Create(fullRemotePath)
	if err != nil {
		return fmt.Errorf("sftp: failed to create remote file %s: %w", fullRemotePath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("sftp: failed to transfer data: %w", err)
	}

	return nil
}

// Download downloads a file via SFTP.
func (s *SFTPStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("sftp: failed to create local file: %w", err)
	}
	defer dst.Close()

	fullRemotePath := filepath.Join(s.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	src, err := s.client.Open(fullRemotePath)
	if err != nil {
		return fmt.Errorf("sftp: failed to open remote file %s: %w", fullRemotePath, err)
	}
	defer src.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("sftp: failed to transfer data: %w", err)
	}

	return nil
}

// List lists files via SFTP with the given prefix.
func (s *SFTPStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	items := make([]Item, 0)

	baseDir := normalizePathSeparators(s.baseDir)

	walker := s.client.Walk(baseDir)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return nil, fmt.Errorf("sftp: walk error: %w", err)
		}

		// Get relative path
		path := walker.Path()
		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			continue
		}

		// Normalize separators for comparison
		relPath = normalizePathSeparators(relPath)

		if !strings.HasPrefix(relPath, prefix) {
			continue
		}

		stat := walker.Stat()
		items = append(items, Item{
			Key:          relPath,
			ModifiedTime: stat.ModTime(),
			Size:         stat.Size(),
			IsDirectory:  stat.IsDir(),
		})
	}

	return items, nil
}

// Remove deletes a file via SFTP.
func (s *SFTPStorage) Remove(ctx context.Context, remotePath string) error {
	fullRemotePath := filepath.Join(s.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	if err := s.client.Remove(fullRemotePath); err != nil {
		return fmt.Errorf("sftp: failed to remove %s: %w", fullRemotePath, err)
	}

	return nil
}

// Close releases resources.
func (s *SFTPStorage) Close() error {
	if err := s.client.Close(); err != nil {
		s.sshClient.Close()
		return err
	}
	return s.sshClient.Close()
}

// getSSHSigners returns SSH signers from identity file and SSH agent.
func getSSHSigners(identityFile string, passphrase string) ([]ssh.Signer, error) {
	signers := make([]ssh.Signer, 0)

	// Load identity file if provided
	if identityFile != "" {
		path, err := expandHomeDir(identityFile)
		if err != nil {
			return nil, fmt.Errorf("failed to expand identity file path: %w", err)
		}

		keyData, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read identity file %s: %w", path, err)
		}

		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if errors.As(err, &passErr) {
				signer, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
				if err != nil {
					return nil, fmt.Errorf("failed to decrypt identity file: %w", err)
				}
			} else {
				return nil, fmt.Errorf("failed to parse identity file: %w", err)
			}
		}

		signers = append(signers, signer)
	}

	// Try to get keys from SSH agent
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket != "" {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			agentClient := agent.NewClient(conn)
			agentSigners, err := agentClient.Signers()
			if err == nil {
				signers = append(signers, agentSigners...)
			}
		}
	}

	return signers, nil
}

// getHostKeyCallback returns appropriate host key callback.
func getHostKeyCallback(ignoreHostKey bool) ssh.HostKeyCallback {
	if ignoreHostKey {
		return ssh.InsecureIgnoreHostKey()
	}

	knownHostsFiles := make([]string, 0)
	for _, p := range []string{"/etc/ssh/ssh_known_hosts", "~/.ssh/known_hosts"} {
		path, err := expandHomeDir(p)
		if err != nil {
			continue
		}

		if _, err := os.Stat(path); err == nil {
			knownHostsFiles = append(knownHostsFiles, path)
		}
	}

	if len(knownHostsFiles) == 0 {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("no known_hosts files found for host key verification")
		}
	}

	callback, err := knownhosts.New(knownHostsFiles...)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("failed to load known_hosts: %w", err)
		}
	}

	return callback
}

// expandHomeDir expands ~ in file paths to home directory.
func expandHomeDir(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return filepath.Clean(path), nil
	}

	parts := strings.SplitN(path, "/", 2)
	username := strings.TrimPrefix(parts[0], "~")

	var homeDir string
	var err error

	if username == "" {
		homeDir, err = os.UserHomeDir()
		if err != nil || homeDir == "" {
			currentUser, err := user.Current()
			if err != nil {
				return "", fmt.Errorf("failed to get home directory: %w", err)
			}
			homeDir = currentUser.HomeDir
		}
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
