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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3 defaults.
const (
	DefaultS3PartSize    int64 = 64 << 20
	MinS3PartSize        int64 = 5 << 20
	DefaultS3Concurrency       = 2
	DefaultS3Region            = "us-east-1"
)

// S3Storage implements Storage for Amazon S3 and S3-compatible services
// (MinIO, Ceph, Wasabi, ...).
type S3Storage struct {
	client       *s3.Client
	uploader     *manager.Uploader //nolint:staticcheck // see NewS3Storage
	bucket       string
	region       string
	storageClass types.StorageClass
	sse          types.ServerSideEncryption
	sseKMSKeyID  string
}

// NewS3Storage creates a new S3 storage instance.
//
// Credentials: static keys (S3KeyID/S3Secret/S3SessionToken) when set,
// otherwise the default AWS credential chain (environment, shared profile
// S3Profile, web identity/IRSA, ECS/EC2 roles).
func NewS3Storage(config Config) (*S3Storage, error) {
	if err := validateConfig(config, map[string]string{
		"bucket": config.S3Bucket,
	}); err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if config.S3Region != "" {
		opts = append(opts, awsconfig.WithRegion(config.S3Region))
	}
	if config.S3Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(config.S3Profile))
	}
	if config.S3KeyID != "" && config.S3Secret != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(config.S3KeyID, config.S3Secret, config.S3SessionToken)))
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = DefaultS3Region
	}

	endpoint := config.S3Endpoint
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		if config.S3DisableTLS {
			endpoint = "http://" + endpoint
		} else {
			endpoint = "https://" + endpoint
		}
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = config.S3ForcePath
	})

	partSize := config.S3PartSize
	if partSize <= 0 {
		partSize = DefaultS3PartSize
	}
	if partSize < MinS3PartSize {
		partSize = MinS3PartSize
	}
	concurrency := config.S3Concurrency
	if concurrency <= 0 {
		concurrency = DefaultS3Concurrency
	}

	//nolint:staticcheck // feature/s3/manager is deprecated in favour of the (pre-1.0) transfermanager.
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = partSize
		u.Concurrency = concurrency
		u.LeavePartsOnError = false
		u.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	s := &S3Storage{
		client:       client,
		uploader:     uploader,
		bucket:       config.S3Bucket,
		region:       cfg.Region,
		storageClass: types.StorageClass(config.S3StorageClass),
	}
	switch strings.ToLower(config.S3SSE) {
	case "":
	case "aes256":
		s.sse = types.ServerSideEncryptionAes256
	case "aws:kms":
		s.sse = types.ServerSideEncryptionAwsKms
		s.sseKMSKeyID = config.S3SSEKMSKeyID
	default:
		s.sse = types.ServerSideEncryption(config.S3SSE)
		s.sseKMSKeyID = config.S3SSEKMSKeyID
	}
	return s, nil
}

// Client returns the underlying S3 client.
func (s *S3Storage) Client() *s3.Client { return s.client }

func (s *S3Storage) putInput(key string, body io.Reader, opts *PutOptions) *s3.PutObjectInput {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if opts != nil {
		if opts.ContentType != "" {
			in.ContentType = aws.String(opts.ContentType)
		}
		if len(opts.Metadata) > 0 {
			in.Metadata = opts.Metadata
		}
	}
	if s.storageClass != "" {
		in.StorageClass = s.storageClass
	}
	if s.sse != "" {
		in.ServerSideEncryption = s.sse
		if s.sseKMSKeyID != "" {
			in.SSEKMSKeyId = aws.String(s.sseKMSKeyID)
		}
	}
	return in
}

// Put streams r to key using a multipart upload (aborted on error).
func (s *S3Storage) Put(ctx context.Context, key string, r io.Reader, opts *PutOptions) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("s3: %w", err)
	}
	cr := &countingReader{r: ctxReader{ctx: ctx, r: r}}
	//nolint:staticcheck // see NewS3Storage
	out, err := s.uploader.Upload(ctx, s.putInput(k, cr, opts))
	if err != nil {
		return Item{}, fmt.Errorf("s3: failed to upload %s to bucket %s: %w", k, s.bucket, err)
	}
	return Item{
		Key:          k,
		Size:         cr.n,
		ModifiedTime: time.Now().UTC(),
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionID),
	}, nil
}

// PutIf performs a conditional single-request PutObject (If-Match /
// If-None-Match: *). The body is buffered and limited to 16 MiB.
func (s *S3Storage) PutIf(ctx context.Context, key string, r io.Reader, cond Condition, opts *PutOptions) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("s3: %w", err)
	}
	if err := validateCondition(cond); err != nil {
		return Item{}, fmt.Errorf("s3: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(ctxReader{ctx: ctx, r: r}, maxConditionalPutSize+1))
	if err != nil {
		return Item{}, fmt.Errorf("s3: failed to read data: %w", err)
	}
	if len(data) > maxConditionalPutSize {
		return Item{}, fmt.Errorf("s3: conditional put of %s exceeds %d bytes", k, maxConditionalPutSize)
	}
	in := s.putInput(k, bytes.NewReader(data), opts)
	in.ContentLength = aws.Int64(int64(len(data)))
	if cond.IfMatch != "" {
		in.IfMatch = aws.String(cond.IfMatch)
	}
	if cond.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		// If-Match on a missing key yields 404 NoSuchKey on AWS.
		if isS3Precondition(err) || (cond.IfMatch != "" && s3StatusCode(err) == http.StatusNotFound) {
			return Item{}, fmt.Errorf("s3: conditional put of %s: %w", k, ErrPreconditionFailed)
		}
		return Item{}, fmt.Errorf("s3: failed to put %s: %w", k, err)
	}
	return Item{
		Key:          k,
		Size:         int64(len(data)),
		ModifiedTime: time.Now().UTC(),
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionId),
	}, nil
}

// Get streams the object body.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return nil, Item{}, fmt.Errorf("s3: %w", err)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)})
	if err != nil {
		return nil, Item{}, s.mapErr("get", k, err)
	}
	return out.Body, Item{
		Key:          k,
		Size:         aws.ToInt64(out.ContentLength),
		ModifiedTime: aws.ToTime(out.LastModified),
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionId),
	}, nil
}

// Stat returns object metadata (HeadObject).
func (s *S3Storage) Stat(ctx context.Context, key string) (Item, error) {
	k, err := cleanKey(key)
	if err != nil {
		return Item{}, fmt.Errorf("s3: %w", err)
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)})
	if err != nil {
		return Item{}, s.mapErr("stat", k, err)
	}
	return Item{
		Key:          k,
		Size:         aws.ToInt64(out.ContentLength),
		ModifiedTime: aws.ToTime(out.LastModified),
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionId),
	}, nil
}

// Upload uploads a file to S3.
func (s *S3Storage) Upload(ctx context.Context, localPath string, remotePath string) error {
	if err := uploadFile(ctx, s, localPath, remotePath); err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	return nil
}

// Download downloads a file from S3.
func (s *S3Storage) Download(ctx context.Context, remotePath string, localPath string) error {
	if err := downloadFile(ctx, s, remotePath, localPath); err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	return nil
}

// List lists objects (recursively) with the given prefix.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]Item, error) {
	p, err := cleanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	return s.list(ctx, p, "")
}

// ListDir lists the objects directly under dir.
func (s *S3Storage) ListDir(ctx context.Context, dir string) ([]Item, error) {
	d, err := cleanDir(dir)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	return s.list(ctx, dirPrefix(d), "/")
}

func (s *S3Storage) list(ctx context.Context, prefix, delimiter string) ([]Item, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	items := make([]Item, 0)
	p := s3.NewListObjectsV2Paginator(s.client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: failed to list objects in bucket %s: %w", s.bucket, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if delimiter != "" && strings.HasSuffix(key, "/") {
				continue // directory marker
			}
			items = append(items, Item{
				Key:          key,
				ModifiedTime: aws.ToTime(obj.LastModified),
				Size:         aws.ToInt64(obj.Size),
				ETag:         aws.ToString(obj.ETag),
			})
		}
	}
	return items, nil
}

// Remove deletes an object. A missing key returns ErrNotFound (checked with
// HeadObject because DeleteObject is idempotent on S3).
func (s *S3Storage) Remove(ctx context.Context, remotePath string) error {
	k, err := cleanKey(remotePath)
	if err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	if _, err := s.Stat(ctx, k); err != nil {
		return err
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)}); err != nil {
		return s.mapErr("remove", k, err)
	}
	return nil
}

// Close releases resources (no-op).
func (s *S3Storage) Close() error {
	return nil
}

func (s *S3Storage) mapErr(op, key string, err error) error {
	if s3StatusCode(err) == http.StatusNotFound {
		return fmt.Errorf("s3: %s %s in bucket %s: %w", op, key, s.bucket, ErrNotFound)
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return fmt.Errorf("s3: %s %s in bucket %s: %w", op, key, s.bucket, ErrNotFound)
	}
	return fmt.Errorf("s3: %s %s in bucket %s: %w", op, key, s.bucket, err)
}

func s3StatusCode(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func isS3Precondition(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return true
		}
	}
	switch s3StatusCode(err) {
	case http.StatusPreconditionFailed, http.StatusConflict:
		return true
	}
	return false
}
