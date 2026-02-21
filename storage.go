package go_storage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Storage defines the interface for remote file storage operations.
// All implementations must be safe for concurrent use.
type Storage interface {
	// Upload uploads a local file to the remote storage with the given target name.
	Upload(ctx context.Context, localPath string, remotePath string) error

	// Download downloads a file from remote storage to a local path.
	Download(ctx context.Context, remotePath string, localPath string) error

	// List returns all items in the storage that match the given prefix.
	List(ctx context.Context, prefix string) ([]Item, error)

	// Remove deletes a file from remote storage.
	Remove(ctx context.Context, remotePath string) error

	// Close releases any resources held by the storage implementation.
	Close() error
}

// Item represents a file or directory in remote storage.
type Item struct {
	Key          string    // The path/key of the item
	ModifiedTime time.Time // Last modification time
	Size         int64     // Size in bytes (0 for directories)
	IsDirectory  bool      // Whether this item is a directory
}

// Config holds configuration options for creating storage instances.
type Config struct {
	// S3 configuration
	S3Region     string
	S3Bucket     string
	S3Profile    string
	S3KeyID      string
	S3Secret     string
	S3Endpoint   string
	S3ForcePath  bool
	S3DisableTLS bool

	// B2 configuration
	B2KeyID                 string
	B2AppKey                string
	B2Bucket                string
	B2ConcurrentConnections int
	B2ForcePath             bool

	// Google Cloud Storage configuration
	GCSBucket          string
	GCSEndpoint        string
	GCSCredentialsFile string

	// Azure configuration
	AzureAccount   string
	AzureKey       string
	AzureContainer string
	AzureEndpoint  string

	// SFTP configuration
	SFTPHost             string
	SFTPPort             string
	SFTPUsername         string
	SFTPPassword         string
	SFTPDirectory        string
	SFTPIdentityFile     string
	SFTPIgnoreKnownHosts bool

	// FTP configuration
	FTPHost      string
	FTPPort      string
	FTPUsername  string
	FTPPassword  string
	FTPDirectory string
}

// StorageType represents the type of storage backend.
type StorageType string

const (
	StorageTypeS3    StorageType = "s3"
	StorageTypeGCS   StorageType = "gcs"
	StorageTypeAzure StorageType = "azure"
	StorageTypeSFTP  StorageType = "sftp"
	StorageTypeFTP   StorageType = "ftp"
	StorageTypeLocal StorageType = "local"
)

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
		return NewLocalStorage("/backups")
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", storageType)
	}
}

// normalizePathSeparators converts OS-specific path separators to forward slashes.
// This is necessary for cloud storage services which expect forward slashes.
func normalizePathSeparators(path string) string {
	if os.PathSeparator == '/' {
		return path
	}
	return strings.ReplaceAll(path, string(os.PathSeparator), "/")
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
