package go_storage

import (
	"context"
	"fmt"
	"io"
	"os"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSStorage implements Storage interface for Google Cloud Storage.
type GCSStorage struct {
	client *storage.Client
	bucket string
}

// NewGCSStorage creates a new Google Cloud Storage instance.
func NewGCSStorage(config Config) (*GCSStorage, error) {
	if err := validateConfig(config, map[string]string{
		"bucket": config.GCSBucket,
	}); err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}

	options := make([]option.ClientOption, 0)

	if config.GCSEndpoint != "" {
		options = append(options, option.WithEndpoint(config.GCSEndpoint))
	}

	if config.GCSCredentialsFile != "" {
		options = append(options, option.WithCredentialsFile(config.GCSCredentialsFile))
	}

	client, err := storage.NewClient(context.Background(), options...)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to create client: %w", err)
	}

	return &GCSStorage{
		client: client,
		bucket: config.GCSBucket,
	}, nil
}

// Upload uploads a file to GCS.
func (g *GCSStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("gcs: failed to open local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("gcs: failed to close file: %v\n", err)
		}
	}(file)

	obj := g.client.Bucket(g.bucket).Object(normalizePathSeparators(remotePath)).NewWriter(ctx)
	defer func(obj *storage.Writer) {
		err = obj.Close()
		if err != nil {
			fmt.Printf("gcs: failed to close object writer: %v\n", err)
		}
	}(obj)

	if _, err := io.Copy(obj, file); err != nil {
		return fmt.Errorf("gcs: failed to write data: %w", err)
	}

	if err = obj.Close(); err != nil {
		return fmt.Errorf("gcs: failed to finalize upload: %w", err)
	}

	return nil
}

// Download downloads a file from GCS.
func (g *GCSStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("gcs: failed to create local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("gcs: failed to close file: %v\n", err)
		}
	}(file)

	obj, err := g.client.Bucket(g.bucket).Object(normalizePathSeparators(remotePath)).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("gcs: failed to create reader: %w", err)
	}
	defer func(obj *storage.Reader) {
		err = obj.Close()
		if err != nil {
			fmt.Printf("gcs: failed to close object reader: %v\n", err)
		}
	}(obj)

	if _, err = io.Copy(file, obj); err != nil {
		return fmt.Errorf("gcs: failed to read data: %w", err)
	}

	return nil
}

// List lists objects in GCS with the given prefix.
func (g *GCSStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	items := make([]Item, 0)

	query := &storage.Query{
		Prefix: normalizePathSeparators(prefix),
	}

	it := g.client.Bucket(g.bucket).Objects(ctx, query)

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: failed to iterate objects: %w", err)
		}

		items = append(items, Item{
			Key:          attrs.Name,
			ModifiedTime: attrs.Updated,
			Size:         attrs.Size,
			IsDirectory:  false,
		})
	}

	return items, nil
}

// Remove deletes an object from GCS.
func (g *GCSStorage) Remove(ctx context.Context, remotePath string) error {
	obj := g.client.Bucket(g.bucket).Object(normalizePathSeparators(remotePath))

	if err := obj.Delete(ctx); err != nil {
		return fmt.Errorf("gcs: failed to remove object from bucket %s: %w", g.bucket, err)
	}

	return nil
}

// Close releases resources.
func (g *GCSStorage) Close() error {
	return g.client.Close()
}
