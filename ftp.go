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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTPStorage implements Storage interface for FTP (optionally explicit FTPS).
//
// FTP uses a single control connection, so all operations are serialised by a
// mutex. The body returned by Get holds that connection until it is closed:
// a Get body MUST be closed before any other call on the same FTPStorage,
// otherwise that call blocks.
type FTPStorage struct {
	host    string
	port    string
	baseDir string

	mu     sync.Mutex
	client *ftp.ServerConn
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
	hostPort := net.JoinHostPort(config.FTPHost, port)

	opts := []ftp.DialOption{ftp.DialWithTimeout(30 * time.Second)}
	if config.FTPTLS {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{
			ServerName: config.FTPHost,
			MinVersion: tls.VersionTLS12,
		}))
	}
	conn, err := ftp.Dial(hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("ftp: failed to connect to %s: %w", hostPort, err)
	}

	if err := conn.Login(config.FTPUsername, config.FTPPassword); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("ftp: login failed: %w", err)
	}

	baseDir := normalizePathSeparators(config.FTPDirectory)
	if baseDir == "" {
		if wd, err := conn.CurrentDir(); err == nil && wd != "" {
			baseDir = wd
		} else {
			baseDir = "/"
		}
	}

	return &FTPStorage{
		host:    config.FTPHost,
		port:    port,
		baseDir: path.Clean(baseDir),
		client:  conn,
	}, nil
}

func (f *FTPStorage) resolve(key string) (string, string, error) {
	k, err := cleanKey(key)
	if err != nil {
		return "", "", fmt.Errorf("ftp: %w", err)
	}
	return k, path.Join(f.baseDir, k), nil
}

func ftpCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

func ftpNotFound(err error) bool {
	return ftpCode(err) == ftp.StatusFileUnavailable
}

// Put streams r to <key>.<rand>.partial and renames it on success.
func (f *FTPStorage) Put(ctx context.Context, key string, r io.Reader, _ *PutOptions) (Item, error) {
	k, full, err := f.resolve(key)
	if err != nil {
		return Item{}, err
	}
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensureDirectory(path.Dir(full))
	tmp := partialName(full)
	err = f.client.Stor(tmp, ctxReader{ctx: ctx, r: r})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = f.client.Delete(tmp)
		return Item{}, fmt.Errorf("ftp: failed to upload %s: %w", k, err)
	}
	if err := f.client.Rename(tmp, full); err != nil {
		// Some servers refuse to rename onto an existing file.
		_ = f.client.Delete(full)
		if err := f.client.Rename(tmp, full); err != nil {
			_ = f.client.Delete(tmp)
			return Item{}, fmt.Errorf("ftp: failed to rename %s: %w", k, err)
		}
	}
	return f.stat(k, full)
}

// PutIf is not supported on FTP: it always returns ErrConditionNotSupported.
// Callers should fall back to a lock-based protocol.
func (f *FTPStorage) PutIf(_ context.Context, key string, _ io.Reader, _ Condition, _ *PutOptions) (Item, error) {
	return Item{}, fmt.Errorf("ftp: conditional put of %s: %w", key, ErrConditionNotSupported)
}

// ftpBody releases the connection lock when closed.
type ftpBody struct {
	ctx  context.Context
	resp *ftp.Response
	once sync.Once
	mu   *sync.Mutex
}

func (b *ftpBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.resp.Read(p)
}

func (b *ftpBody) Close() error {
	var err error
	b.once.Do(func() {
		err = b.resp.Close()
		b.mu.Unlock()
	})
	return err
}

// Get opens key for streaming reads. The returned body holds the FTP control
// connection: close it before issuing any other call on this storage.
func (f *FTPStorage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	k, full, err := f.resolve(key)
	if err != nil {
		return nil, Item{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Item{}, err
	}
	f.mu.Lock()
	it, err := f.stat(k, full)
	if err != nil {
		f.mu.Unlock()
		return nil, Item{}, err
	}
	if it.IsDirectory {
		f.mu.Unlock()
		return nil, Item{}, fmt.Errorf("ftp: %s is a directory", k)
	}
	resp, err := f.client.Retr(full)
	if err != nil {
		f.mu.Unlock()
		if ftpNotFound(err) {
			return nil, Item{}, fmt.Errorf("ftp: get %s: %w", k, ErrNotFound)
		}
		return nil, Item{}, fmt.Errorf("ftp: failed to retrieve %s: %w", k, err)
	}
	return &ftpBody{ctx: ctx, resp: resp, mu: &f.mu}, it, nil
}

// Stat returns metadata for key (from a listing of its parent directory).
func (f *FTPStorage) Stat(ctx context.Context, key string) (Item, error) {
	k, full, err := f.resolve(key)
	if err != nil {
		return Item{}, err
	}
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stat(k, full)
}

func (f *FTPStorage) stat(key, full string) (Item, error) {
	entries, err := f.client.List(path.Dir(full))
	if err != nil {
		if ftpNotFound(err) {
			return Item{}, fmt.Errorf("ftp: stat %s: %w", key, ErrNotFound)
		}
		return Item{}, fmt.Errorf("ftp: stat %s: %w", key, err)
	}
	name := path.Base(full)
	for _, e := range entries {
		if e.Name == name {
			return ftpItem(key, e), nil
		}
	}
	return Item{}, fmt.Errorf("ftp: stat %s: %w", key, ErrNotFound)
}

func ftpItem(key string, e *ftp.Entry) Item {
	it := Item{Key: key, ModifiedTime: e.Time, IsDirectory: e.Type == ftp.EntryTypeFolder}
	if !it.IsDirectory {
		it.Size = int64(e.Size) //nolint:gosec // sizes fit in int64
	}
	return it
}

// Upload uploads a file via FTP.
func (f *FTPStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	return uploadFile(ctx, f, localPath, remotePath)
}

// Download downloads a file via FTP.
func (f *FTPStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	return downloadFile(ctx, f, remotePath, localPath)
}

// List lists files and directories (recursively) whose key starts with prefix.
func (f *FTPStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	items := make([]Item, 0)
	startRel := ""
	if i := strings.LastIndex(p, "/"); i >= 0 {
		startRel = p[:i]
	}
	if err := f.walk(ctx, startRel, p, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (f *FTPStorage) walk(ctx context.Context, rel, prefix string, items *[]Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := f.client.List(path.Join(f.baseDir, rel))
	if err != nil {
		if ftpNotFound(err) {
			return nil
		}
		return fmt.Errorf("ftp: failed to list directory %s: %w", rel, err)
	}
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." || e.Type == ftp.EntryTypeLink {
			continue
		}
		key := path.Join(rel, e.Name)
		isDir := e.Type == ftp.EntryTypeFolder
		if strings.HasPrefix(key, prefix) {
			*items = append(*items, ftpItem(key, e))
		} else if !isDir || !strings.HasPrefix(prefix, key+"/") {
			continue
		}
		if isDir {
			if err := f.walk(ctx, key, prefix, items); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListDir lists the files directly inside dir.
func (f *FTPStorage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.client.List(path.Join(f.baseDir, d))
	if err != nil {
		if ftpNotFound(err) {
			return []Item{}, nil
		}
		return nil, fmt.Errorf("ftp: failed to list directory %s: %w", dir, err)
	}
	items := make([]Item, 0, len(entries))
	for _, e := range entries {
		if e.Type != ftp.EntryTypeFile {
			continue
		}
		items = append(items, ftpItem(path.Join(d, e.Name), e))
	}
	return items, nil
}

// Remove deletes a file via FTP.
func (f *FTPStorage) Remove(ctx context.Context, remotePath string) error {
	k, full, err := f.resolve(remotePath)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.client.Delete(full); err != nil {
		if ftpNotFound(err) {
			return fmt.Errorf("ftp: remove %s: %w", k, ErrNotFound)
		}
		return fmt.Errorf("ftp: failed to remove %s: %w", k, err)
	}
	return nil
}

// Close releases resources.
func (f *FTPStorage) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.client.Quit(); err != nil {
		return fmt.Errorf("ftp: failed to close connection: %w", err)
	}
	return nil
}

// ensureDirectory creates a directory path if it doesn't exist (errors are
// ignored: the directory usually exists already, and a real failure surfaces
// on the following STOR).
func (f *FTPStorage) ensureDirectory(dir string) {
	if dir == "." || dir == "/" || dir == "" {
		return
	}
	current := ""
	if strings.HasPrefix(dir, "/") {
		current = "/"
	}
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		_ = f.client.MakeDir(current)
	}
}
