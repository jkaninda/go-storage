package go_storage

import (
	"context"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// AzureStorage implements Storage interface for Azure Blob Storage.
type AzureStorage struct {
	client    *azblob.Client
	container string
}

// NewAzureStorage creates a new Azure Blob Storage instance.
func NewAzureStorage(config Config) (*AzureStorage, error) {
	if err := validateConfig(config, map[string]string{
		"container": config.AzureContainer,
	}); err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}

	// Try to get credentials from environment if not provided
	account := config.AzureAccount
	if account == "" {
		account = os.Getenv("AZURE_STORAGE_ACCOUNT")
	}

	key := config.AzureKey
	if key == "" {
		key = os.Getenv("AZURE_STORAGE_KEY")
	}

	endpoint := config.AzureEndpoint
	if endpoint == "" {
		endpoint = "blob.core.windows.net"
	}

	var client *azblob.Client
	var err error

	if account == "" {
		// Anonymous access
		client, err = azblob.NewClientWithNoCredential(endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to create anonymous client: %w", err)
		}
	} else {
		// Authenticated access
		credential, err := azblob.NewSharedKeyCredential(account, key)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to create credentials: %w", err)
		}

		url := fmt.Sprintf("https://%s.%s", account, endpoint)

		client, err = azblob.NewClientWithSharedKeyCredential(url, credential, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to create client: %w", err)
		}
	}

	return &AzureStorage{
		client:    client,
		container: config.AzureContainer,
	}, nil
}

// Upload uploads a file to Azure Blob Storage.
func (a *AzureStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("azure: failed to open local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("azure: failed to close file: %v\n", err)
		}
	}(file)

	_, err = a.client.UploadFile(ctx, a.container, normalizePathSeparators(remotePath), file, nil)
	if err != nil {
		return fmt.Errorf("azure: failed to upload to container %s: %w", a.container, err)
	}

	return nil
}

// Download downloads a file from Azure Blob Storage.
func (a *AzureStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("azure: failed to create local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("azure: failed to close file: %v\n", err)
		}
	}(file)

	_, err = a.client.DownloadFile(ctx, a.container, normalizePathSeparators(remotePath), file, nil)
	if err != nil {
		return fmt.Errorf("azure: failed to download from container %s: %w", a.container, err)
	}

	return nil
}

// List lists blobs in Azure Blob Storage with the given prefix.
func (a *AzureStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	normalizedPrefix := normalizePathSeparators(prefix)

	pager := a.client.NewListBlobsFlatPager(a.container, &azblob.ListBlobsFlatOptions{
		Prefix: &normalizedPrefix,
	})

	items := make([]Item, 0)

	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to list blobs in container %s: %w", a.container, err)
		}

		for _, blob := range resp.Segment.BlobItems {
			items = append(items, Item{
				Key:          *blob.Name,
				ModifiedTime: *blob.Properties.LastModified,
				Size:         *blob.Properties.ContentLength,
				IsDirectory:  false,
			})
		}
	}

	return items, nil
}

// Remove deletes a blob from Azure Blob Storage.
func (a *AzureStorage) Remove(ctx context.Context, remotePath string) error {
	_, err := a.client.DeleteBlob(ctx, a.container, normalizePathSeparators(remotePath), nil)
	if err != nil {
		return fmt.Errorf("azure: failed to remove blob from container %s: %w", a.container, err)
	}

	return nil
}

// Close releases resources.
func (a *AzureStorage) Close() error {
	return nil
}
