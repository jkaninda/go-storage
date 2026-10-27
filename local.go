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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LocalStorage implements the Storage interface using the local filesystem.
// It is safe for concurrent use.
type LocalStorage struct {
	basePath string
}

// Lock tuning for PutIf / Put on the local backend.
const (
	localLockTimeout = 10 * time.Second
	localLockRetry   = 10 * time.Millisecond
	// A lock older than this is considered stale (the holder crashed). The
	// lock is only held around a stat + rename, never while streaming data.
	localLockStale = 30 * time.Second
)

// NewLocalStorage creates a new LocalStorage instance with the given base directory.
// The base directory will be created if it doesn't exist.
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("local: failed to resolve absolute path: %w", err)
	}

	if err := os.MkdirAll(absPath, 0o755); err != nil {
		return nil, fmt.Errorf("local: failed to create base directory: %w", err)
	}

	return &LocalStorage{basePath: absPath}, nil
}

// resolvePath safely resolves a key to an absolute local path, ensuring it
// stays within the base directory.
func (s *LocalStorage) resolvePath(remotePath string) (string, error) {
	key, err := cleanKey(remotePath)
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	return s.join(key)
}

func (s *LocalStorage) join(key string) (string, error) {
	fullPath := filepath.Join(s.basePath, filepath.FromSlash(key))
	rel, err := filepath.Rel(s.basePath, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("local: path traversal attempt detected: %s", key)
	}
	return fullPath, nil
}

// localETag derives the version token from mtime and size.
func localETag(fi fs.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 16) + "-" + strconv.FormatInt(fi.Size(), 16)
}

func (s *LocalStorage) item(full string, fi fs.FileInfo) Item {
	rel, err := filepath.Rel(s.basePath, full)
	if err != nil {
		rel = full
	}
	it := Item{
		Key:          filepath.ToSlash(rel),
		ModifiedTime: fi.ModTime(),
		IsDirectory:  fi.IsDir(),
	}
	if !fi.IsDir() {
		it.Size = fi.Size()
		it.ETag = localETag(fi)
	}
	return it
}

// Upload uploads a local file to the storage with the given target name.
func (s *LocalStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	return uploadFile(ctx, s, localPath, remotePath)
}

// Download downloads a file from storage to a local path.
func (s *LocalStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	return downloadFile(ctx, s, remotePath, localPath)
}

// writeTemp streams r into a new temporary file next to full and returns its path.
func writeTemp(ctx context.Context, full string, r io.Reader) (string, error) {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("local: failed to create parent directories: %w", err)
	}
	tmp := partialName(full)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("local: failed to create temporary file: %w", err)
	}
	_, err = io.Copy(f, ctxReader{ctx: ctx, r: r})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("local: failed to write data: %w", err)
	}
	return tmp, nil
}

// Put streams r to key via a temporary file that is renamed on success.
func (s *LocalStorage) Put(ctx context.Context, key string, r io.Reader, _ *PutOptions) (Item, error) {
	return s.put(ctx, key, r, nil)
}

// PutIf writes r to key only if cond holds. It uses an exclusive lock file
// (<key>.lock) so it is atomic with respect to other PutIf/Put calls on the
// same filesystem.
func (s *LocalStorage) PutIf(ctx context.Context, key string, r io.Reader, cond Condition, _ *PutOptions) (Item, error) {
	if err := validateCondition(cond); err != nil {
		return Item{}, fmt.Errorf("local: %w", err)
	}
	return s.put(ctx, key, r, &cond)
}

func (s *LocalStorage) put(ctx context.Context, key string, r io.Reader, cond *Condition) (Item, error) {
	full, err := s.resolvePath(key)
	if err != nil {
		return Item{}, err
	}
	tmp, err := writeTemp(ctx, full, r)
	if err != nil {
		return Item{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp)
		}
	}()

	unlock, err := acquireLock(ctx, full+".lock")
	if err != nil {
		return Item{}, err
	}
	defer unlock()

	cur, statErr := os.Stat(full)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return Item{}, fmt.Errorf("local: failed to stat %s: %w", key, statErr)
	}
	if cond != nil {
		switch {
		case cond.IfNoneMatch && exists:
			return Item{}, fmt.Errorf("local: %s already exists: %w", key, ErrPreconditionFailed)
		case cond.IfMatch != "" && (!exists || localETag(cur) != cond.IfMatch):
			return Item{}, fmt.Errorf("local: %s etag mismatch: %w", key, ErrPreconditionFailed)
		}
	}
	if exists {
		if err := bumpMtime(tmp, cur.ModTime()); err != nil {
			return Item{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	if err := os.Rename(tmp, full); err != nil {
		return Item{}, fmt.Errorf("local: failed to rename temporary file: %w", err)
	}
	committed = true
	fi, err := os.Stat(full)
	if err != nil {
		return Item{}, fmt.Errorf("local: failed to stat %s: %w", key, err)
	}
	return s.item(full, fi), nil
}

// bumpMtime makes sure the mtime of tmp is strictly after prev so that the
// ETag changes even on filesystems with coarse timestamps.
func bumpMtime(tmp string, prev time.Time) error {
	for _, step := range []time.Duration{time.Microsecond, time.Millisecond, time.Second, 2 * time.Second} {
		fi, err := os.Stat(tmp)
		if err != nil {
			return fmt.Errorf("local: failed to stat temporary file: %w", err)
		}
		if fi.ModTime().After(prev) {
			return nil
		}
		t := prev.Add(step)
		if err := os.Chtimes(tmp, t, t); err != nil {
			return fmt.Errorf("local: failed to set mtime: %w", err)
		}
	}
	return nil
}

// acquireLock creates lockPath exclusively, retrying until localLockTimeout.
func acquireLock(ctx context.Context, lockPath string) (func(), error) {
	deadline := time.Now().Add(localLockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("local: failed to create lock %s: %w", lockPath, err)
		}
		if fi, serr := os.Stat(lockPath); serr == nil && time.Since(fi.ModTime()) > localLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("local: timed out waiting for lock %s", lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(localLockRetry):
		}
	}
}

// Get opens key for reading.
func (s *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, Item{}, err
	}
	full, err := s.resolvePath(key)
	if err != nil {
		return nil, Item{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, Item{}, mapLocalErr(key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Item{}, mapLocalErr(key, err)
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, Item{}, fmt.Errorf("local: %s is a directory", key)
	}
	return f, s.item(full, fi), nil
}

// Stat returns metadata for key.
func (s *LocalStorage) Stat(ctx context.Context, key string) (Item, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	full, err := s.resolvePath(key)
	if err != nil {
		return Item{}, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return Item{}, mapLocalErr(key, err)
	}
	return s.item(full, fi), nil
}

func mapLocalErr(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("local: %s: %w", key, ErrNotFound)
	}
	return fmt.Errorf("local: %s: %w", key, err)
}

// ListDir lists the files directly inside dir.
func (s *LocalStorage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("local: %w", err)
	}
	full, err := s.join(d)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if errors.Is(err, fs.ErrNotExist) {
		return []Item{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("local: failed to read directory %s: %w", dir, err)
	}
	items := make([]Item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		items = append(items, s.item(filepath.Join(full, e.Name()), fi))
	}
	return items, nil
}

// List returns all items (files and directories, recursively) whose key
// starts with prefix.
func (s *LocalStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("local: %w", err)
	}

	// Walk from the deepest directory fully contained in the prefix.
	root := s.basePath
	if i := strings.LastIndex(p, "/"); i >= 0 {
		root = filepath.Join(s.basePath, filepath.FromSlash(p[:i]))
	}
	items := make([]Item, 0)
	err = filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if full == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return nil // skip unreadable entries
		}
		if full == s.basePath {
			return nil
		}
		rel, rerr := filepath.Rel(s.basePath, full)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, p) {
			// Prune directories that cannot contain matches.
			if d.IsDir() && !strings.HasPrefix(p, rel+"/") {
				return fs.SkipDir
			}
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		items = append(items, s.item(full, fi))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("local: failed to walk directory: %w", err)
	}
	return items, nil
}

// Remove deletes a file (or directory tree) from storage.
func (s *LocalStorage) Remove(ctx context.Context, remotePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	targetPath, err := s.resolvePath(remotePath)
	if err != nil {
		return err
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		return mapLocalErr(remotePath, err)
	}
	if info.IsDir() {
		err = os.RemoveAll(targetPath)
	} else {
		err = os.Remove(targetPath)
	}
	if err != nil {
		return mapLocalErr(remotePath, err)
	}
	return nil
}

// Close releases any resources held by the storage implementation (no-op).
func (s *LocalStorage) Close() error {
	return nil
}

// BasePath returns the base directory path of the storage.
func (s *LocalStorage) BasePath() string {
	return s.basePath
}
