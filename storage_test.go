package go_storage

import (
	"context"
	"testing"
	"time"
)

// Test helpers
func TestStorageInterface(t *testing.T) {
	var _ Storage = (*S3Storage)(nil)
	var _ Storage = (*GCSStorage)(nil)
	var _ Storage = (*AzureStorage)(nil)
	var _ Storage = (*SFTPStorage)(nil)
}

func TestContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	config := Config{
		S3Region: "us-east-1",
		S3Bucket: "test-bucket",
	}

	s3, err := NewStorage(StorageTypeS3, config)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func(s3 Storage) {
		err = s3.Close()
		if err != nil {
			t.Logf("Failed to close storage: %v", err)
		}
	}(s3)

	err = s3.Upload(ctx, "test.txt", "remote-test.txt")
	if err != nil {
		t.Logf("Upload failed (expected in test): %v", err)
	}
}
