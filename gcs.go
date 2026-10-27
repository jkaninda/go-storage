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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSStorage implements Storage interface for Google Cloud Storage.
type GCSStorage struct {
	client *storage.Client
	bucket string
}

// NewGCSStorage creates a new Google Cloud Storage instance.
//
// Credentials: GCSCredentialsJSON, else GCSCredentialsFile, else Application
// Default Credentials. The STORAGE_EMULATOR_HOST environment variable is
// honoured by the Google client library itself.
func NewGCSStorage(config Config) (*GCSStorage, error) {
	if err := validateConfig(config, map[string]string{
		"bucket": config.GCSBucket,
	}); err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}

	options := make([]option.ClientOption, 0, 2)
	if config.GCSEndpoint != "" {
		options = append(options, option.WithEndpoint(config.GCSEndpoint))
	}
	switch {
	case config.GCSCredentialsJSON != "":
		data := []byte(config.GCSCredentialsJSON)
		options = append(options, option.WithAuthCredentialsJSON(gcsCredType(data), data))
	case config.GCSCredentialsFile != "":
		data, err := os.ReadFile(config.GCSCredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("gcs: failed to read credentials file: %w", err)
		}
		options = append(options, option.WithAuthCredentialsFile(gcsCredType(data), config.GCSCredentialsFile))
	}
	return newGCSStorage(context.Background(), config.GCSBucket, options...)
}

func newGCSStorage(ctx context.Context, bucket string, options ...option.ClientOption) (*GCSStorage, error) {
	client, err := storage.NewClient(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to create client: %w", err)
	}
	return &GCSStorage{client: client, bucket: bucket}, nil
}

// gcsCredType detects the credential type from the JSON "type" field.
func gcsCredType(data []byte) option.CredentialsType {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &v)
	switch v.Type {
	case "authorized_user":
		return option.AuthorizedUser
	case "impersonated_service_account":
		return option.ImpersonatedServiceAccount
	case "external_account":
		return option.ExternalAccount
	default:
		return option.ServiceAccount
	}
}

// Client returns the underlying GCS client.
func (g *GCSStorage) Client() *storage.Client { return g.client }

func gcsItem(attrs *storage.ObjectAttrs) Item {
	return Item{
		Key:          attrs.Name,
		ModifiedTime: attrs.Updated,
		Size:         attrs.Size,
		ETag:         strconv.FormatInt(attrs.Generation, 10),
	}
}

func (g *GCSStorage) put(ctx context.Context, key string, r io.Reader, opts *PutOptions, cond *storage.Conditions) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("gcs: %w", err)
	}
	obj := g.client.Bucket(g.bucket).Object(k)
	if cond != nil {
		obj = obj.If(*cond)
	}
	// A child context: cancelling it before Close aborts the upload so the
	// object is never finalised.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(wctx)
	if opts != nil {
		w.ContentType = opts.ContentType
		if len(opts.Metadata) > 0 {
			w.Metadata = opts.Metadata
		}
	}
	if _, err := io.Copy(w, ctxReader{ctx: ctx, r: r}); err != nil {
		cancel()
		_ = w.Close()
		if cond != nil && isGCSPrecondition(err) {
			return Item{}, fmt.Errorf("gcs: conditional put of %s: %w", k, ErrPreconditionFailed)
		}
		return Item{}, fmt.Errorf("gcs: failed to upload %s: %w", k, err)
	}
	if err := ctx.Err(); err != nil {
		cancel()
		_ = w.Close()
		return Item{}, fmt.Errorf("gcs: failed to upload %s: %w", k, err)
	}
	if err := w.Close(); err != nil {
		if cond != nil && isGCSPrecondition(err) {
			return Item{}, fmt.Errorf("gcs: conditional put of %s: %w", k, ErrPreconditionFailed)
		}
		return Item{}, fmt.Errorf("gcs: failed to finalize upload of %s: %w", k, err)
	}
	return gcsItem(w.Attrs()), nil
}

// Put streams r to key.
func (g *GCSStorage) Put(ctx context.Context, key string, r io.Reader, opts *PutOptions) (Item, error) {
	return g.put(ctx, key, r, opts, nil)
}

// PutIf writes r to key with a generation precondition (IfMatch is the
// generation as returned in Item.ETag).
func (g *GCSStorage) PutIf(ctx context.Context, key string, r io.Reader, cond Condition, opts *PutOptions) (Item, error) {
	if err := validateCondition(cond); err != nil {
		return Item{}, fmt.Errorf("gcs: %w", err)
	}
	c := storage.Conditions{}
	switch {
	case cond.IfNoneMatch:
		c.DoesNotExist = true
	default:
		gen, err := strconv.ParseInt(cond.IfMatch, 10, 64)
		if err != nil || gen <= 0 {
			return Item{}, fmt.Errorf("gcs: invalid generation %q: %w", cond.IfMatch, ErrPreconditionFailed)
		}
		c.GenerationMatch = gen
	}
	return g.put(ctx, key, r, opts, &c)
}

// Get streams the object body.
func (g *GCSStorage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return nil, Item{}, fmt.Errorf("gcs: %w", err)
	}
	rd, err := g.client.Bucket(g.bucket).Object(k).NewReader(ctx)
	if err != nil {
		return nil, Item{}, g.mapErr("get", k, err)
	}
	return rd, Item{
		Key:          k,
		Size:         rd.Attrs.Size,
		ModifiedTime: rd.Attrs.LastModified,
		ETag:         strconv.FormatInt(rd.Attrs.Generation, 10),
	}, nil
}

// Stat returns object attributes.
func (g *GCSStorage) Stat(ctx context.Context, key string) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("gcs: %w", err)
	}
	attrs, err := g.client.Bucket(g.bucket).Object(k).Attrs(ctx)
	if err != nil {
		return Item{}, g.mapErr("stat", k, err)
	}
	return gcsItem(attrs), nil
}

// Upload uploads a file to GCS.
func (g *GCSStorage) Upload(ctx context.Context, localPath string, remotePath string) error {
	if err := uploadFile(ctx, g, localPath, remotePath); err != nil {
		return fmt.Errorf("gcs: %w", err)
	}
	return nil
}

// Download downloads a file from GCS.
func (g *GCSStorage) Download(ctx context.Context, remotePath string, localPath string) error {
	if err := downloadFile(ctx, g, remotePath, localPath); err != nil {
		return fmt.Errorf("gcs: %w", err)
	}
	return nil
}

// List lists objects (recursively) with the given prefix.
func (g *GCSStorage) List(ctx context.Context, prefix string) ([]Item, error) {
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}
	return g.list(ctx, &storage.Query{Prefix: p})
}

// ListDir lists the objects directly under dir.
func (g *GCSStorage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}
	return g.list(ctx, &storage.Query{Prefix: dirPrefix(d), Delimiter: "/"})
}

func (g *GCSStorage) list(ctx context.Context, q *storage.Query) ([]Item, error) {
	items := make([]Item, 0)
	it := g.client.Bucket(g.bucket).Objects(ctx, q)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: failed to list objects in bucket %s: %w", g.bucket, err)
		}
		if attrs.Name == "" || (q.Delimiter != "" && strings.HasSuffix(attrs.Name, "/")) {
			continue // synthetic prefix or directory marker
		}
		items = append(items, gcsItem(attrs))
	}
	return items, nil
}

// Remove deletes an object.
func (g *GCSStorage) Remove(ctx context.Context, remotePath string) error {
	k, err := cleanKey(remotePath)
	if err != nil {
		return fmt.Errorf("gcs: %w", err)
	}
	if err := g.client.Bucket(g.bucket).Object(k).Delete(ctx); err != nil {
		return g.mapErr("remove", k, err)
	}
	return nil
}

// Close releases resources.
func (g *GCSStorage) Close() error {
	return g.client.Close()
}

func (g *GCSStorage) mapErr(op, key string, err error) error {
	var ge *googleapi.Error
	if errors.Is(err, storage.ErrObjectNotExist) || (errors.As(err, &ge) && ge.Code == http.StatusNotFound) {
		return fmt.Errorf("gcs: %s %s in bucket %s: %w", op, key, g.bucket, ErrNotFound)
	}
	return fmt.Errorf("gcs: %s %s in bucket %s: %w", op, key, g.bucket, err)
}

func isGCSPrecondition(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && (ge.Code == http.StatusPreconditionFailed || ge.Code == http.StatusConflict)
}
