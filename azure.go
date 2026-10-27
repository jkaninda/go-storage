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
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// Azure streaming upload tuning: 8 MiB blocks allow blobs up to ~390 GiB
// (50,000 blocks) while keeping memory bounded to blockSize*concurrency.
const (
	azureBlockSize   int64 = 8 << 20
	azureConcurrency       = 2
)

// AzureStorage implements Storage interface for Azure Blob Storage.
type AzureStorage struct {
	client    *azblob.Client
	container string
}

// NewAzureStorage creates a new Azure Blob Storage instance.
//
// Authentication: AzureSASToken if set, otherwise shared key
// (AzureAccount/AzureKey), otherwise anonymous.
func NewAzureStorage(config Config) (*AzureStorage, error) {
	if err := validateConfig(config, map[string]string{
		"container": config.AzureContainer,
	}); err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}

	serviceURL, err := azureServiceURL(config)
	if err != nil {
		return nil, err
	}

	var client *azblob.Client
	switch {
	case config.AzureSASToken != "":
		u := serviceURL + "?" + strings.TrimPrefix(config.AzureSASToken, "?")
		client, err = azblob.NewClientWithNoCredential(u, nil)
	case config.AzureAccount != "" && config.AzureKey != "":
		cred, cerr := azblob.NewSharedKeyCredential(config.AzureAccount, config.AzureKey)
		if cerr != nil {
			return nil, fmt.Errorf("azure: failed to create credentials: %w", cerr)
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	default:
		client, err = azblob.NewClientWithNoCredential(serviceURL, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create client: %w", err)
	}

	return &AzureStorage{client: client, container: config.AzureContainer}, nil
}

func azureServiceURL(config Config) (string, error) {
	endpoint := config.AzureEndpoint
	if endpoint == "" {
		endpoint = "blob.core.windows.net"
	}
	if strings.Contains(endpoint, "://") {
		return strings.TrimSuffix(endpoint, "/") + "/", nil
	}
	if config.AzureAccount == "" {
		return "", fmt.Errorf("azure: missing required configuration: account")
	}
	return fmt.Sprintf("https://%s.%s/", config.AzureAccount, strings.Trim(endpoint, "/")), nil
}

// Client returns the underlying azblob client.
func (a *AzureStorage) Client() *azblob.Client { return a.client }

func (a *AzureStorage) put(ctx context.Context, key string, r io.Reader, opts *PutOptions, ac *blob.AccessConditions) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("azure: %w", err)
	}
	o := &azblob.UploadStreamOptions{
		BlockSize:        azureBlockSize,
		Concurrency:      azureConcurrency,
		AccessConditions: ac,
	}
	if opts != nil {
		if opts.ContentType != "" {
			o.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &opts.ContentType}
		}
		if len(opts.Metadata) > 0 {
			o.Metadata = make(map[string]*string, len(opts.Metadata))
			for mk, mv := range opts.Metadata {
				o.Metadata[mk] = &mv
			}
		}
	}
	cr := &countingReader{r: ctxReader{ctx: ctx, r: r}}
	resp, err := a.client.UploadStream(ctx, a.container, k, cr, o)
	if err != nil {
		if ac != nil && (isAzurePrecondition(err) || bloberror.HasCode(err, bloberror.BlobNotFound)) {
			return Item{}, fmt.Errorf("azure: conditional put of %s: %w", k, ErrPreconditionFailed)
		}
		return Item{}, fmt.Errorf("azure: failed to upload %s to container %s: %w", k, a.container, err)
	}
	it := Item{Key: k, Size: cr.n, ModifiedTime: time.Now().UTC()}
	if resp.ETag != nil {
		it.ETag = string(*resp.ETag)
	}
	if resp.LastModified != nil {
		it.ModifiedTime = *resp.LastModified
	}
	if resp.VersionID != nil {
		it.VersionID = *resp.VersionID
	}
	return it, nil
}

// Put streams r to key with a block upload; the block list is only
// committed when the whole stream was read successfully.
func (a *AzureStorage) Put(ctx context.Context, key string, r io.Reader, opts *PutOptions) (Item, error) {
	return a.put(ctx, key, r, opts, nil)
}

// PutIf writes r to key with If-Match / If-None-Match: * access conditions.
func (a *AzureStorage) PutIf(ctx context.Context, key string, r io.Reader, cond Condition, opts *PutOptions) (Item, error) {
	if err := validateCondition(cond); err != nil {
		return Item{}, fmt.Errorf("azure: %w", err)
	}
	mac := &blob.ModifiedAccessConditions{}
	if cond.IfMatch != "" {
		etag := azcore.ETag(cond.IfMatch)
		mac.IfMatch = &etag
	}
	if cond.IfNoneMatch {
		etag := azcore.ETagAny
		mac.IfNoneMatch = &etag
	}
	return a.put(ctx, key, r, opts, &blob.AccessConditions{ModifiedAccessConditions: mac})
}

// Get streams the blob body.
func (a *AzureStorage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return nil, Item{}, fmt.Errorf("azure: %w", err)
	}
	resp, err := a.client.DownloadStream(ctx, a.container, k, nil)
	if err != nil {
		return nil, Item{}, a.mapErr("get", k, err)
	}
	it := Item{Key: k}
	if resp.ContentLength != nil {
		it.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		it.ModifiedTime = *resp.LastModified
	}
	if resp.ETag != nil {
		it.ETag = string(*resp.ETag)
	}
	if resp.VersionID != nil {
		it.VersionID = *resp.VersionID
	}
	return resp.Body, it, nil
}

// Stat returns blob properties.
func (a *AzureStorage) Stat(ctx context.Context, key string) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("azure: %w", err)
	}
	bc := a.client.ServiceClient().NewContainerClient(a.container).NewBlobClient(k)
	resp, err := bc.GetProperties(ctx, nil)
	if err != nil {
		return Item{}, a.mapErr("stat", k, err)
	}
	it := Item{Key: k}
	if resp.ContentLength != nil {
		it.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		it.ModifiedTime = *resp.LastModified
	}
	if resp.ETag != nil {
		it.ETag = string(*resp.ETag)
	}
	if resp.VersionID != nil {
		it.VersionID = *resp.VersionID
	}
	return it, nil
}

// Upload uploads a file to Azure Blob Storage.
func (a *AzureStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	if err := uploadFile(ctx, a, localPath, remotePath); err != nil {
		return fmt.Errorf("azure: %w", err)
	}
	return nil
}

// Download downloads a file from Azure Blob Storage.
func (a *AzureStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	if err := downloadFile(ctx, a, remotePath, localPath); err != nil {
		return fmt.Errorf("azure: %w", err)
	}
	return nil
}

// List lists blobs (recursively) with the given prefix.
func (a *AzureStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	pager := a.client.NewListBlobsFlatPager(a.container, &azblob.ListBlobsFlatOptions{Prefix: &p})
	items := make([]Item, 0)
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to list blobs in container %s: %w", a.container, err)
		}
		for _, b := range resp.Segment.BlobItems {
			items = append(items, azureItem(b))
		}
	}
	return items, nil
}

// ListDir lists the blobs directly under dir.
func (a *AzureStorage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	p := dirPrefix(d)
	cc := a.client.ServiceClient().NewContainerClient(a.container)
	pager := cc.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: &p})
	items := make([]Item, 0)
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to list blobs in container %s: %w", a.container, err)
		}
		for _, b := range resp.Segment.BlobItems {
			if b.Name == nil || strings.HasSuffix(*b.Name, "/") {
				continue
			}
			items = append(items, azureItem(b))
		}
	}
	return items, nil
}

func azureItem(b *container.BlobItem) Item {
	it := Item{}
	if b.Name != nil {
		it.Key = *b.Name
	}
	if b.VersionID != nil {
		it.VersionID = *b.VersionID
	}
	if p := b.Properties; p != nil {
		if p.LastModified != nil {
			it.ModifiedTime = *p.LastModified
		}
		if p.ContentLength != nil {
			it.Size = *p.ContentLength
		}
		if p.ETag != nil {
			it.ETag = string(*p.ETag)
		}
	}
	return it
}

// Remove deletes a blob.
func (a *AzureStorage) Remove(ctx context.Context, remotePath string) error {
	k, err := cleanKey(remotePath)
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}
	if _, err := a.client.DeleteBlob(ctx, a.container, k, nil); err != nil {
		return a.mapErr("remove", k, err)
	}
	return nil
}

// Close releases resources (no-op).
func (a *AzureStorage) Close() error {
	return nil
}

func (a *AzureStorage) mapErr(op, key string, err error) error {
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
		return fmt.Errorf("azure: %s %s in container %s: %w", op, key, a.container, ErrNotFound)
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) && re.StatusCode == http.StatusNotFound && re.ErrorCode != string(bloberror.ContainerNotFound) {
		return fmt.Errorf("azure: %s %s in container %s: %w", op, key, a.container, ErrNotFound)
	}
	return fmt.Errorf("azure: %s %s in container %s: %w", op, key, a.container, err)
}

func isAzurePrecondition(err error) bool {
	if bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists, bloberror.TargetConditionNotMet) {
		return true
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode == http.StatusPreconditionFailed ||
			(re.StatusCode == http.StatusConflict && re.ErrorCode == string(bloberror.BlobAlreadyExists))
	}
	return false
}
