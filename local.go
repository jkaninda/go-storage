package go_storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalStorage implements the Storage interface using the local filesystem.
// It is safe for concurrent use as it relies on filesystem operations which
// are handled by the OS.
type LocalStorage struct {
	basePath string
}

// NewLocalStorage creates a new LocalStorage instance with the given base directory.
// The base directory will be created if it doesn't exist.
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	if err := os.MkdirAll(absPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &LocalStorage{
		basePath: absPath,
	}, nil
}

// resolvePath safely resolves a remote path to an absolute local path,
// ensuring it stays within the base directory.
func (s *LocalStorage) resolvePath(remotePath string) (string, error) {
	// Clean the path to remove any .. or . components
	cleanPath := filepath.Clean(remotePath)

	// Remove leading slashes to make it relative
	cleanPath = strings.TrimPrefix(cleanPath, string(filepath.Separator))

	// Join with base path
	fullPath := filepath.Join(s.basePath, cleanPath)

	// Verify the resolved path is still within basePath
	if !strings.HasPrefix(fullPath, s.basePath) {
		return "", fmt.Errorf("path traversal attempt detected: %s", remotePath)
	}

	return fullPath, nil
}

// Upload uploads a local file to the storage with the given target name.
func (s *LocalStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	targetPath, err := s.resolvePath(remotePath)
	if err != nil {
		return err
	}

	// Open source file
	srcFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	// Create parent directories if needed
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}

	// Create destination file
	dstFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dstFile.Close()

	// Copy with context cancellation checks
	buf := make([]byte, 32*1024) // 32KB buffer
	for {
		select {
		case <-ctx.Done():
			// Clean up partial file on cancellation
			os.Remove(targetPath)
			return ctx.Err()
		default:
		}

		n, readErr := srcFile.Read(buf)
		if n > 0 {
			if _, writeErr := dstFile.Write(buf[:n]); writeErr != nil {
				os.Remove(targetPath)
				return fmt.Errorf("failed to write to destination: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			os.Remove(targetPath)
			return fmt.Errorf("failed to read source file: %w", readErr)
		}
	}

	return nil
}

// Download downloads a file from storage to a local path.
func (s *LocalStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	sourcePath, err := s.resolvePath(remotePath)
	if err != nil {
		return err
	}

	// Open source file
	srcFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file: %w", err)
	}
	defer srcFile.Close()

	// Create parent directories for local path if needed
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create local parent directories: %w", err)
	}

	// Create destination file
	dstFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer dstFile.Close()

	// Copy with context cancellation checks
	buf := make([]byte, 32*1024) // 32KB buffer
	for {
		select {
		case <-ctx.Done():
			// Clean up partial file on cancellation
			os.Remove(localPath)
			return ctx.Err()
		default:
		}

		n, readErr := srcFile.Read(buf)
		if n > 0 {
			if _, writeErr := dstFile.Write(buf[:n]); writeErr != nil {
				os.Remove(localPath)
				return fmt.Errorf("failed to write to local file: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			os.Remove(localPath)
			return fmt.Errorf("failed to read remote file: %w", readErr)
		}
	}

	return nil
}

// List returns all items in the storage that match the given prefix.
func (s *LocalStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	var items []Item

	// Clean and resolve the prefix path
	searchPath := s.basePath
	cleanPrefix := strings.TrimPrefix(filepath.Clean(prefix), string(filepath.Separator))

	if cleanPrefix != "" && cleanPrefix != "." {
		searchPath = filepath.Join(s.basePath, cleanPrefix)
	}

	// Check if the search path exists
	info, err := os.Stat(searchPath)
	if os.IsNotExist(err) {
		// If the exact path doesn't exist, try to find files with this prefix
		// by walking the parent directory
		parentDir := filepath.Dir(searchPath)
		baseName := filepath.Base(searchPath)

		if _, err := os.Stat(parentDir); os.IsNotExist(err) {
			return items, nil // Return empty list if parent doesn't exist
		}

		entries, err := os.ReadDir(parentDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read directory: %w", err)
		}

		for _, entry := range entries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}

			if strings.HasPrefix(entry.Name(), baseName) {
				fullPath := filepath.Join(parentDir, entry.Name())
				item, err := s.fileInfoToItem(fullPath)
				if err != nil {
					continue // Skip files we can't stat
				}
				items = append(items, item)
			}
		}
		return items, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to stat path: %w", err)
	}

	// If it's a file, return just that file
	if !info.IsDir() {
		item, err := s.fileInfoToItem(searchPath)
		if err != nil {
			return nil, err
		}
		return []Item{item}, nil
	}

	// Walk the directory
	err = filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			return nil // Skip entries with errors
		}

		item, err := s.fileInfoToItem(path)
		if err != nil {
			return nil // Skip files we can't stat
		}
		items = append(items, item)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return items, nil
}

// fileInfoToItem converts a file path to an Item.
func (s *LocalStorage) fileInfoToItem(path string) (Item, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Item{}, err
	}

	// Get relative path from base
	relPath, err := filepath.Rel(s.basePath, path)
	if err != nil {
		relPath = path
	}

	// Normalize to forward slashes for consistency
	relPath = filepath.ToSlash(relPath)

	size := info.Size()
	if info.IsDir() {
		size = 0
	}

	return Item{
		Key:          relPath,
		ModifiedTime: info.ModTime(),
		Size:         size,
		IsDirectory:  info.IsDir(),
	}, nil
}

// Remove deletes a file from storage.
func (s *LocalStorage) Remove(ctx context.Context, remotePath string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	targetPath, err := s.resolvePath(remotePath)
	if err != nil {
		return err
	}

	// Check if file exists
	info, err := os.Stat(targetPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("file not found: %s", remotePath)
	}
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Remove file or directory
	if info.IsDir() {
		if err := os.RemoveAll(targetPath); err != nil {
			return fmt.Errorf("failed to remove directory: %w", err)
		}
	} else {
		if err := os.Remove(targetPath); err != nil {
			return fmt.Errorf("failed to remove file: %w", err)
		}
	}

	return nil
}

// Close releases any resources held by the storage implementation.
// For LocalStorage, this is a no-op as there are no persistent resources.
func (s *LocalStorage) Close() error {
	return nil
}

// BasePath returns the base directory path of the storage.
func (s *LocalStorage) BasePath() string {
	return s.basePath
}
