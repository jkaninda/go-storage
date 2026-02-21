package go_storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTPStorage implements Storage interface for FTP.
type FTPStorage struct {
	host    string
	port    string
	baseDir string
	client  *ftp.ServerConn
}

// NewFTPStorage creates a new FTP storage instance.
func NewFTPStorage(config Config) (*FTPStorage, error) {
	if err := validateConfig(config, map[string]string{
		"host": config.FTPHost,
	}); err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}

	port := config.FTPPort
	if port == "" {
		port = "21"
	}

	// Connect to FTP server
	hostPort := fmt.Sprintf("%s:%s", config.FTPHost, port)
	conn, err := ftp.Dial(hostPort, ftp.DialWithTimeout(30*time.Second))
	if err != nil {
		return nil, fmt.Errorf("ftp: failed to connect to %s: %w", hostPort, err)
	}

	// Login
	if err := conn.Login(config.FTPUsername, config.FTPPassword); err != nil {
		conn.Quit()
		return nil, fmt.Errorf("ftp: login failed: %w", err)
	}

	return &FTPStorage{
		host:    config.FTPHost,
		port:    port,
		baseDir: config.FTPDirectory,
		client:  conn,
	}, nil
}

// Upload uploads a file via FTP.
func (f *FTPStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("ftp: failed to open local file: %w", err)
	}
	defer src.Close()

	fullRemotePath := filepath.Join(f.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	// Create parent directory if needed
	remoteDir := filepath.Dir(fullRemotePath)
	remoteDir = normalizePathSeparators(remoteDir)

	if remoteDir != "." && remoteDir != "/" {
		f.ensureDirectory(remoteDir)

	}

	// Upload file
	if err := f.client.Stor(fullRemotePath, src); err != nil {
		return fmt.Errorf("ftp: failed to upload file %s: %w", fullRemotePath, err)
	}

	return nil
}

// Download downloads a file via FTP.
func (f *FTPStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	fullRemotePath := filepath.Join(f.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	// Get the file from FTP server
	resp, err := f.client.Retr(fullRemotePath)
	if err != nil {
		return fmt.Errorf("ftp: failed to retrieve file %s: %w", fullRemotePath, err)
	}
	defer resp.Close()

	// Create local file
	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("ftp: failed to create local file: %w", err)
	}
	defer dst.Close()

	// Copy data
	if _, err := io.Copy(dst, resp); err != nil {
		return fmt.Errorf("ftp: failed to transfer data: %w", err)
	}

	return nil
}

// List lists files via FTP with the given prefix.
func (f *FTPStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	items := make([]Item, 0)

	// Normalize the base directory
	baseDir := normalizePathSeparators(f.baseDir)
	if baseDir == "" {
		baseDir = "/"
	}

	// Walk the directory
	if err := f.walkDirectory(baseDir, prefix, &items); err != nil {
		return nil, err
	}

	return items, nil
}

// walkDirectory recursively walks FTP directory structure.
func (f *FTPStorage) walkDirectory(dir string, prefix string, items *[]Item) error {
	entries, err := f.client.List(dir)
	if err != nil {
		return fmt.Errorf("ftp: failed to list directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		// Get relative path
		var relPath string
		if f.baseDir == "" || f.baseDir == "/" {
			relPath = filepath.Join(strings.TrimPrefix(dir, "/"), entry.Name)
		} else {
			fullPath := filepath.Join(dir, entry.Name)
			relPath = strings.TrimPrefix(fullPath, f.baseDir)
			relPath = strings.TrimPrefix(relPath, "/")
		}

		// Normalize separators for comparison
		relPath = normalizePathSeparators(relPath)

		// Skip if doesn't match prefix
		if prefix != "" && !strings.HasPrefix(relPath, prefix) {
			// For directories, we might need to walk them to find matching files
			if entry.Type == ftp.EntryTypeFolder {
				fullPath := filepath.Join(dir, entry.Name)
				fullPath = normalizePathSeparators(fullPath)
				if err := f.walkDirectory(fullPath, prefix, items); err != nil {
					// Log but don't fail on subdirectory errors
					continue
				}
			}
			continue
		}

		item := Item{
			Key:          relPath,
			ModifiedTime: entry.Time,
			Size:         int64(entry.Size),
			IsDirectory:  entry.Type == ftp.EntryTypeFolder,
		}
		*items = append(*items, item)

		// Recursively walk directories
		if entry.Type == ftp.EntryTypeFolder {
			fullPath := filepath.Join(dir, entry.Name)
			fullPath = normalizePathSeparators(fullPath)
			if err := f.walkDirectory(fullPath, prefix, items); err != nil {
				// Log but don't fail on subdirectory errors
				continue
			}
		}
	}

	return nil
}

// Remove deletes a file via FTP.
func (f *FTPStorage) Remove(ctx context.Context, remotePath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	fullRemotePath := filepath.Join(f.baseDir, remotePath)
	fullRemotePath = normalizePathSeparators(fullRemotePath)

	if err := f.client.Delete(fullRemotePath); err != nil {
		return fmt.Errorf("ftp: failed to remove %s: %w", fullRemotePath, err)
	}

	return nil
}

// Close releases resources.
func (f *FTPStorage) Close() error {
	if err := f.client.Quit(); err != nil {
		return fmt.Errorf("ftp: failed to close connection: %w", err)
	}
	return nil
}

// ensureDirectory creates a directory path if it doesn't exist.
func (f *FTPStorage) ensureDirectory(path string) {
	parts := strings.Split(path, "/")
	current := ""

	for _, part := range parts {
		if part == "" {
			continue
		}

		if current == "" {
			current = part
		} else {
			current = current + "/" + part
		}

		// Try to create directory (ignore errors if it already exists)
		_ = f.client.MakeDir(current)
	}

}
