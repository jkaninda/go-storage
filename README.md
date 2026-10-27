# Go Storage

A streaming storage abstraction for Go backup tools.

Supported backends:

- Local filesystem
- S3 and S3-compatible services (MinIO, Ceph, Wasabi, ...) via aws-sdk-go-v2
- Azure Blob Storage (and Azurite)
- Google Cloud Storage
- SFTP
- FTP / explicit FTPS

```shell
go get github.com/jkaninda/go-storage
```

## Streaming API

Every backend implements the same `Storage` interface:

```go
type Storage interface {
	Upload(ctx context.Context, localPath, remotePath string) error
	Download(ctx context.Context, remotePath, localPath string) error
	List(ctx context.Context, prefix string) ([]Item, error) // recursive
	Remove(ctx context.Context, remotePath string) error     // missing -> ErrNotFound
	Close() error

	Put(ctx context.Context, key string, r io.Reader, opts *PutOptions) (Item, error)
	Get(ctx context.Context, key string) (io.ReadCloser, Item, error)
	Stat(ctx context.Context, key string) (Item, error)
	PutIf(ctx context.Context, key string, r io.Reader, cond Condition, opts *PutOptions) (Item, error)
	ListDir(ctx context.Context, dir string) ([]Item, error) // non-recursive, files only
}
```

- **`Put`** streams a reader of unknown length with bounded memory (no temp
  files, no whole-file buffering). If the reader fails or the context is
  cancelled, `Put` returns an error and **no object (and no partial object)
  is left at the key**: S3 aborts the multipart upload, Azure never commits
  the block list, GCS cancels the resumable upload, and SFTP/FTP/local write
  to `<key>.<random>.partial` and rename on success.
- **`Get`** returns a streaming body; always `Close` it. On FTP the body holds
  the single control connection, so close it before making another call.
- **`PutIf`** is a conditional write for small files such as a backup index
  (optimistic concurrency): `Condition{IfNoneMatch: true}` creates only if the
  key does not exist, `Condition{IfMatch: item.ETag}` replaces only if the
  object is unchanged. It returns `ErrPreconditionFailed` when the condition
  does not hold and `ErrConditionNotSupported` when the backend cannot
  evaluate it (fall back to locking).
- Errors wrap the sentinels `ErrNotFound`, `ErrPreconditionFailed` and
  `ErrConditionNotSupported`; test them with `errors.Is`.
- Keys are relative to the backend base (bucket, container, remote directory
  or local base path), use `/` separators and may not escape the base (`..`).

| Backend | `Item.ETag`                    | `PutIf` IfNoneMatch | `PutIf` IfMatch |
|---------|--------------------------------|---------------------|-----------------|
| S3      | S3 ETag                        | yes                 | yes             |
| Azure   | blob ETag                      | yes                 | yes             |
| GCS     | object generation              | yes                 | yes             |
| Local   | `<mtime-ns hex>-<size hex>`    | yes (lock file)     | yes (lock file) |
| SFTP    | empty                          | yes (`O_EXCL`)      | not supported   |
| FTP     | empty                          | not supported       | not supported   |

S3 `PutIf` uses a single `PutObject` request and is limited to 16 MiB.

### Example: dump, compress and upload without temp files

```go
package main

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"strings"

	storage "github.com/jkaninda/go-storage"
)

func main() {
	ctx := context.Background()
	s, err := storage.NewStorage(storage.StorageTypeS3, storage.Config{
		S3Bucket:    "backups",
		S3Region:    "us-east-1",
		S3KeyID:     "...",
		S3Secret:    "...",
		S3Endpoint:  "https://minio.example.com", // optional, for S3-compatible services
		S3ForcePath: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		cmd := exec.CommandContext(ctx, "pg_dump", "mydb")
		cmd.Stdout = gz
		err := cmd.Run()
		if err == nil {
			err = gz.Close()
		}
		pw.CloseWithError(err) // a non-nil error aborts the upload
	}()

	item, err := s.Put(ctx, "db/mydb-2026-01-01.sql.gz", pr, &storage.PutOptions{ContentType: "application/gzip"})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("uploaded %s (%d bytes, etag %s)", item.Key, item.Size, item.ETag)

	// Restore: stream it back.
	body, _, err := s.Get(ctx, item.Key)
	if errors.Is(err, storage.ErrNotFound) {
		log.Fatal("backup not found")
	} else if err != nil {
		log.Fatal(err)
	}
	defer body.Close()
	zr, _ := gzip.NewReader(body)
	_, _ = io.Copy(io.Discard, zr)

	// Update an index file with optimistic concurrency.
	idx, err := s.Stat(ctx, "db/index.json")
	cond := storage.Condition{IfMatch: idx.ETag}
	if errors.Is(err, storage.ErrNotFound) {
		cond = storage.Condition{IfNoneMatch: true}
	}
	_, err = s.PutIf(ctx, "db/index.json", strings.NewReader(`{"backups":[]}`), cond, nil)
	if errors.Is(err, storage.ErrPreconditionFailed) {
		// someone else updated the index: re-read, merge and retry
	}
}
```

## Configuration

`storage.NewStorage(storageType, storage.Config{...})` with `storageType` one
of `s3`, `gcs`, `azure`, `sftp`, `ftp`, `local`.

| Field | Backend | Description |
|-------|---------|-------------|
| `LocalPath` | local | Base directory (default `/backup`) |
| `S3Bucket` | s3 | Bucket (required) |
| `S3Region` | s3 | Region (default from AWS config, else `us-east-1`) |
| `S3KeyID`, `S3Secret`, `S3SessionToken` | s3 | Static credentials; when empty the default AWS chain is used (env, shared profile, IRSA/web identity, ECS/EC2 roles) |
| `S3Profile` | s3 | Shared config profile |
| `S3Endpoint` | s3 | Custom endpoint (MinIO etc.); without a scheme `https://` is assumed (`http://` with `S3DisableTLS`) |
| `S3ForcePath` | s3 | Path-style addressing |
| `S3DisableTLS` | s3 | Use `http://` for scheme-less endpoints |
| `S3StorageClass` | s3 | e.g. `STANDARD_IA`, `GLACIER_IR` |
| `S3SSE`, `S3SSEKMSKeyID` | s3 | Server-side encryption: `AES256` or `aws:kms` (+ key id) |
| `S3PartSize` | s3 | Multipart part size (default 64 MiB, min 5 MiB) |
| `S3Concurrency` | s3 | Parallel part uploads (default 2); memory ≈ part size × concurrency |
| `AzureAccount`, `AzureKey` | azure | Shared key credentials |
| `AzureSASToken` | azure | SAS token (used instead of the shared key when set) |
| `AzureContainer` | azure | Container (required) |
| `AzureEndpoint` | azure | Domain suffix (default `blob.core.windows.net`) or a full service URL such as `http://127.0.0.1:10000/devstoreaccount1` (Azurite) |
| `GCSBucket` | gcs | Bucket (required) |
| `GCSCredentialsFile`, `GCSCredentialsJSON` | gcs | Credentials; default is Application Default Credentials |
| `GCSEndpoint` | gcs | Custom endpoint |
| `SFTPHost`, `SFTPPort` | sftp | Server (port default 22) |
| `SFTPUsername`, `SFTPPassword` | sftp | Password authentication |
| `SFTPIdentityFile`, `SFTPIdentityPassphrase` | sftp | Private key authentication (an SSH agent is used too when available) |
| `SFTPKnownHostsFile` | sftp | known_hosts file (default `~/.ssh/known_hosts`); host keys are always verified unless... |
| `SFTPIgnoreKnownHosts` | sftp | ...this is `true` (insecure) |
| `SFTPDirectory` | sftp | Remote base directory (default: login directory) |
| `FTPHost`, `FTPPort`, `FTPUsername`, `FTPPassword` | ftp | Server and credentials (port default 21) |
| `FTPDirectory` | ftp | Remote base directory (default: login directory) |
| `FTPTLS` | ftp | Explicit FTPS (`AUTH TLS`) |

The library never reads environment variables itself (except what the cloud
SDKs do for their default credential chains, and `SSH_AUTH_SOCK` for the SSH agent).

## Testing

```shell
go test ./...                       # unit tests: local + in-process SFTP server
go test -tags integration ./...     # + MinIO, Azurite, fake-gcs-server, FTP via docker
```

Integration tests skip when docker is unavailable. Images can be overridden
with `GO_STORAGE_MINIO_IMAGE`, `GO_STORAGE_AZURITE_IMAGE`,
`GO_STORAGE_FAKEGCS_IMAGE` and `GO_STORAGE_FTP_IMAGE`.

## Deprecated: `pkg/` (v0.1 API)

The packages under `pkg/` (`pkg/local`, `pkg/s3`, `pkg/ssh`, `pkg/ftp`,
`pkg/azure`) are the legacy v0.1 API. They still compile but are deprecated
and will be removed in a future release; use the root package instead.

## License

MIT License, Copyright (c) 2024 Jonas Kaninda. See [LICENSE](LICENSE).
