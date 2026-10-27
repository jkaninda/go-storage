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

// Package go_storage provides a uniform, streaming storage abstraction over
// S3 (and S3-compatible services), Azure Blob Storage, Google Cloud Storage,
// SFTP, FTP and the local filesystem.
//
// All backends implement [Storage]. Objects are addressed by keys that are
// relative to the backend base (bucket, container, remote directory or local
// base path) and always use forward slashes.
package go_storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Sentinel errors. Backends wrap them with %w, so test with errors.Is.
var (
	// ErrNotFound is returned (wrapped) by Stat, Get and Remove when the key does not exist.
	ErrNotFound = errors.New("storage: object not found")
	// ErrPreconditionFailed is returned (wrapped) by PutIf when the condition does not hold.
	ErrPreconditionFailed = errors.New("storage: precondition failed")
	// ErrConditionNotSupported is returned (wrapped) by PutIf when the backend cannot
	// evaluate the requested condition atomically. Callers should fall back to locking.
	ErrConditionNotSupported = errors.New("storage: conditional write not supported by backend")
)

// Storage defines the interface for remote file storage operations.
// All implementations are safe for concurrent use (see FTPStorage for the
// one restriction regarding open Get bodies).
type Storage interface {
	// Upload uploads a local file to the remote storage with the given target name.
	Upload(ctx context.Context, localPath string, remotePath string) error

	// Download downloads a file from remote storage to a local path.
	Download(ctx context.Context, remotePath string, localPath string) error

	// List returns all items (recursively) in the storage that match the given prefix.
	List(ctx context.Context, prefix string) ([]Item, error)

	// Remove deletes a file from remote storage. A missing key returns an
	// error wrapping ErrNotFound.
	Remove(ctx context.Context, remotePath string) error

	// Close releases any resources held by the storage implementation.
	Close() error

	// Put streams r to key. r may be of unknown length; memory use is bounded.
	// If reading r fails or ctx is cancelled, Put returns an error and no
	// (partial) object is left at key.
	Put(ctx context.Context, key string, r io.Reader, opts *PutOptions) (Item, error)

	// Get opens key for streaming reads. The caller must Close the body.
	// A missing key returns an error wrapping ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, Item, error)

	// Stat returns metadata for key. A missing key returns an error wrapping ErrNotFound.
	Stat(ctx context.Context, key string) (Item, error)

	// PutIf writes r to key only if cond holds, atomically with respect to
	// other PutIf calls. It returns an error wrapping ErrPreconditionFailed when
	// the condition does not hold, or ErrConditionNotSupported when the backend
	// cannot evaluate it. Intended for small objects (index files).
	PutIf(ctx context.Context, key string, r io.Reader, cond Condition, opts *PutOptions) (Item, error)

	// ListDir lists only the files directly inside dir (dir is treated as a
	// directory: "a/b" never matches "a/bc/x"; "" = root). Non-recursive, no
	// directories are returned. A missing directory yields an empty list.
	ListDir(ctx context.Context, dir string) ([]Item, error)
}

// Item represents a file or directory in remote storage.
type Item struct {
	Key          string    // The path/key of the item, relative to the backend base
	ModifiedTime time.Time // Last modification time
	Size         int64     // Size in bytes (0 for directories)
	IsDirectory  bool      // Whether this item is a directory
	// ETag is an opaque version token usable with Condition.IfMatch
	// (S3/Azure ETag; GCS generation as decimal string; local:
	// "<mtime-unixnano hex>-<size hex>"; SFTP/FTP: "").
	ETag string
	// VersionID is the S3 version id when bucket versioning is enabled.
	VersionID string
}

// PutOptions are optional parameters for Put and PutIf.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	Size        int64 // optional size hint; -1 or 0 = unknown
}

// Condition is a precondition for PutIf. Exactly one field must be set; a
// zero Condition is rejected (it would otherwise silently become an
// unconditional write, e.g. when an empty ETag is passed as IfMatch).
type Condition struct {
	IfMatch     string // only write if the current ETag equals this
	IfNoneMatch bool   // only write if the object does not exist
}

// Config holds configuration options for creating storage instances.
type Config struct {
	// Local configuration
	LocalPath string // base directory for the local backend (default "/backup")

	// S3 configuration
	S3Region       string
	S3Bucket       string
	S3Profile      string
	S3KeyID        string
	S3Secret       string
	S3SessionToken string
	S3Endpoint     string
	S3ForcePath    bool
	S3DisableTLS   bool
	S3StorageClass string // e.g. STANDARD, STANDARD_IA, GLACIER_IR
	S3SSE          string // "AES256" or "aws:kms"
	S3SSEKMSKeyID  string // KMS key id when S3SSE is "aws:kms"
	S3PartSize     int64  // multipart part size in bytes (default 64 MiB, min 5 MiB)
	S3Concurrency  int    // multipart upload concurrency (default 2)

	// B2 configuration (reserved, no backend implementation yet)
	B2KeyID                 string
	B2AppKey                string
	B2Bucket                string
	B2ConcurrentConnections int
	B2ForcePath             bool

	// Google Cloud Storage configuration
	GCSBucket          string
	GCSEndpoint        string
	GCSCredentialsFile string
	GCSCredentialsJSON string

	// Azure configuration
	AzureAccount   string
	AzureKey       string
	AzureContainer string
	// AzureEndpoint is either a domain suffix (default "blob.core.windows.net",
	// the service URL becomes https://<account>.<endpoint>) or a full service
	// URL containing a scheme (e.g. "http://127.0.0.1:10000/devstoreaccount1"
	// for Azurite), used as is.
	AzureEndpoint string
	// AzureSASToken, when set, is used instead of the shared key.
	AzureSASToken string

	// SFTP configuration
	SFTPHost               string
	SFTPPort               string
	SFTPUsername           string
	SFTPPassword           string
	SFTPDirectory          string
	SFTPIdentityFile       string
	SFTPIdentityPassphrase string
	SFTPKnownHostsFile     string
	SFTPIgnoreKnownHosts   bool

	// FTP configuration
	FTPHost      string
	FTPPort      string
	FTPUsername  string
	FTPPassword  string
	FTPDirectory string
	FTPTLS       bool // explicit FTPS (AUTH TLS)
}

// StorageType represents the type of storage backend.
type StorageType string

// Supported storage types.
const (
	StorageTypeS3    StorageType = "s3"
	StorageTypeGCS   StorageType = "gcs"
	StorageTypeAzure StorageType = "azure"
	StorageTypeSFTP  StorageType = "sftp"
	StorageTypeFTP   StorageType = "ftp"
	StorageTypeLocal StorageType = "local"
)

// DefaultLocalPath is the base directory of the local backend when Config.LocalPath is empty.
const DefaultLocalPath = "/backup"

// NewStorage creates a new Storage instance based on the specified type and configuration.
func NewStorage(storageType StorageType, config Config) (Storage, error) {
	switch storageType {
	case StorageTypeS3:
		return NewS3Storage(config)
	case StorageTypeGCS:
		return NewGCSStorage(config)
	case StorageTypeAzure:
		return NewAzureStorage(config)
	case StorageTypeSFTP:
		return NewSFTPStorage(config)
	case StorageTypeFTP:
		return NewFTPStorage(config)
	case StorageTypeLocal:
		p := config.LocalPath
		if p == "" {
			p = DefaultLocalPath
		}
		return NewLocalStorage(p)
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", storageType)
	}
}

// normalizePathSeparators converts OS-specific path separators to forward slashes.
func normalizePathSeparators(p string) string {
	if os.PathSeparator != '/' {
		p = strings.ReplaceAll(p, string(os.PathSeparator), "/")
	}
	return strings.ReplaceAll(p, "\\", "/")
}

// cleanKey normalises an object key: separators become "/", the path is
// cleaned and leading slashes are removed. Keys escaping the base ("..")
// and empty keys are rejected.
func cleanKey(key string) (string, error) {
	k, err := cleanDir(key)
	if err != nil {
		return "", err
	}
	if k == "" {
		return "", fmt.Errorf("storage: empty key")
	}
	return k, nil
}

// cleanDir is like cleanKey but allows the empty string (root).
func cleanDir(dir string) (string, error) {
	d := path.Clean("/" + normalizePathSeparators(dir))
	d = strings.TrimPrefix(d, "/")
	// path.Clean of an absolute path never yields "..", but check the raw
	// input so that escaping attempts are reported rather than silently clamped.
	for _, seg := range strings.Split(path.Clean(normalizePathSeparators(dir)), "/") {
		if seg == ".." {
			return "", fmt.Errorf("storage: key %q escapes the base directory", dir)
		}
	}
	return d, nil
}

// cleanPrefix normalises a List prefix. Unlike keys, a prefix is not
// path-cleaned (a trailing "/" is significant) but must not contain "..".
func cleanPrefix(prefix string) (string, error) {
	p := normalizePathSeparators(prefix)
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("storage: prefix %q escapes the base directory", prefix)
		}
	}
	return strings.TrimLeft(p, "/"), nil
}

// dirPrefix returns the object prefix for a directory ("" for root, "a/b/" otherwise).
func dirPrefix(dir string) string {
	if dir == "" {
		return ""
	}
	return dir + "/"
}

// partialName returns a unique temporary name next to p.
func partialName(p string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return p + "." + hex.EncodeToString(b[:]) + ".partial"
}

// ctxReader aborts reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// countingReader counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// errEmptyCondition is returned by PutIf when neither IfMatch nor IfNoneMatch is set.
var errEmptyCondition = errors.New("storage: PutIf requires Condition.IfMatch or Condition.IfNoneMatch")

func validateCondition(c Condition) error {
	if c.IfMatch == "" && !c.IfNoneMatch {
		return errEmptyCondition
	}
	if c.IfMatch != "" && c.IfNoneMatch {
		return errors.New("storage: Condition.IfMatch and Condition.IfNoneMatch are mutually exclusive")
	}
	return nil
}

// maxConditionalPutSize bounds the buffer used by backends whose conditional
// write needs a single request (S3).
const maxConditionalPutSize = 16 << 20

// uploadFile implements Storage.Upload on top of Put.
func uploadFile(ctx context.Context, s Storage, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer func() { _ = f.Close() }()
	opts := &PutOptions{Size: -1}
	if fi, err := f.Stat(); err == nil {
		opts.Size = fi.Size()
	}
	_, err = s.Put(ctx, remotePath, f, opts)
	return err
}

// downloadFile implements Storage.Download on top of Get.
func downloadFile(ctx context.Context, s Storage, remotePath, localPath string) (err error) {
	body, _, err := s.Get(ctx, remotePath)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if dir := filepath.Dir(localPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create local directory: %w", err)
		}
	}
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("failed to close local file: %w", cerr)
		}
		if err != nil {
			_ = os.Remove(localPath)
		}
	}()
	if _, err = io.Copy(f, ctxReader{ctx: ctx, r: body}); err != nil {
		return fmt.Errorf("failed to download %s: %w", remotePath, err)
	}
	return nil
}

// validateConfig checks if the required configuration fields are present.
func validateConfig(_ Config, requiredFields map[string]string) error {
	var missing []string
	for field, value := range requiredFields {
		if value == "" {
			missing = append(missing, field)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}

	return nil
}
