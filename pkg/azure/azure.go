/*
MIT License

Copyright (c) 2023 Jonas Kaninda

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

package azure

import (
	"context"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/jkaninda/go-storage/pkg"

	"io"
	"log"
	"os"
	"path/filepath"
)

type azureStorage struct {
	*pkg.Backend
	client        *azblob.Client
	containerName string
}

type Config struct {
	AccountName   string
	AccountKey    string
	ContainerName string
	LocalPath     string
	RemotePath    string
}

// createClient creates FTP Client
func createClient(conf Config) (*azblob.Client, error) {
	// Create the service URL
	credential, err := azblob.NewSharedKeyCredential(conf.AccountName, conf.AccountKey)
	if err != nil {
		log.Fatalf("Failed to create credential: %v", err)
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", conf.AccountName)
	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, credential, nil)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	return client, nil
}

// NewStorage creates new Storage
func NewStorage(conf Config) (pkg.Storage, error) {
	client, err := createClient(conf)
	if err != nil {
		return nil, err
	}
	return &azureStorage{
		client:        client,
		containerName: conf.ContainerName,
		Backend: &pkg.Backend{
			RemotePath: conf.RemotePath,
			LocalPath:  conf.LocalPath,
		},
	}, nil
}

// Copy copies file to Azure Blob Storage
func (s azureStorage) Copy(fileName string) error {
	filePath := filepath.Join(s.LocalPath, fileName)
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", fileName, err)
	}

	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			return
		}
	}(file)

	blobName := filepath.Join(s.RemotePath, fileName)

	// Get the container client
	containerClient := s.client.ServiceClient().NewContainerClient(s.containerName)

	// Get the blob client
	blobClient := containerClient.NewBlockBlobClient(blobName)

	// Upload the file
	_, err = blobClient.UploadFile(context.Background(), file, nil)
	if err != nil {
		log.Fatalf("Failed to upload file: %v", err)
	}
	return nil
}

// CopyFrom copies a file from Azure Blob Storage to local storage
func (s azureStorage) CopyFrom(blobName string) error {

	// Get the container client
	containerClient := s.client.ServiceClient().NewContainerClient(s.containerName)

	// Get the blob client
	blobClient := containerClient.NewBlockBlobClient(filepath.Join(s.RemotePath, blobName))

	// Download the blob
	downloadResponse, err := blobClient.DownloadStream(context.Background(), nil)
	if err != nil {
		return err
	}

	// Create the file to save the blob data
	downloadFile, err := os.Create(filepath.Join(s.LocalPath, blobName))
	if err != nil {
		return err
	}
	defer func(downloadFile *os.File) {
		err := downloadFile.Close()
		if err != nil {
			log.Fatalf("Failed to close file: %v", err)
		}
	}(downloadFile)

	// Write the blob data to the file
	_, err = io.Copy(downloadFile, downloadResponse.Body)
	if err != nil {
		log.Fatalf("Failed to write blob to file: %v", err)
	}

	return nil
}

// Prune deletes old backup created more than specified days
func (s azureStorage) Prune(retentionDays int) error {
	fmt.Println("Deleting old backup from a remote server is not implemented yet")
	return nil

}

// Name returns the storage name
func (s azureStorage) Name() string {
	return "azure"
}
