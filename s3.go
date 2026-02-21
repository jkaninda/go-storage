package go_storage

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// S3Storage implements Storage interface for Amazon S3.
type S3Storage struct {
	session *session.Session
	bucket  string
	region  string
}

// NewS3Storage creates a new S3 storage instance.
func NewS3Storage(config Config) (*S3Storage, error) {
	if err := validateConfig(config, map[string]string{
		"bucket": config.S3Bucket,
	}); err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	awsConfig := aws.NewConfig()

	if config.S3Region != "" {
		awsConfig = awsConfig.WithRegion(config.S3Region)
	}

	if config.S3KeyID != "" && config.S3Secret != "" {
		awsConfig = awsConfig.WithCredentials(
			credentials.NewStaticCredentials(config.S3KeyID, config.S3Secret, ""),
		)
	}

	if config.S3Endpoint != "" {
		awsConfig = awsConfig.WithEndpoint(config.S3Endpoint)
	}

	if config.S3ForcePath {
		awsConfig = awsConfig.WithS3ForcePathStyle(true)
	}

	if config.S3DisableTLS {
		awsConfig = awsConfig.WithDisableSSL(true)
	}

	sessionOpts := session.Options{
		Config:            *awsConfig,
		SharedConfigState: session.SharedConfigEnable,
	}

	if config.S3Profile != "" {
		sessionOpts.Profile = config.S3Profile
	}

	sess, err := session.NewSessionWithOptions(sessionOpts)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to create session: %w", err)
	}

	return &S3Storage{
		session: sess,
		bucket:  config.S3Bucket,
		region:  config.S3Region,
	}, nil
}

// Upload uploads a file to S3.
func (s *S3Storage) Upload(ctx context.Context, localPath string, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("s3: failed to open local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("s3: warning: failed to close file: %v\n", err)
		}
	}(file)

	uploader := s3manager.NewUploader(s.session)

	_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(normalizePathSeparators(remotePath)),
		Body:   file,
	})

	if err != nil {
		return fmt.Errorf("s3: failed to upload to bucket %s: %w", s.bucket, err)
	}

	return nil
}

// Download downloads a file from S3.
func (s *S3Storage) Download(ctx context.Context, remotePath string, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("s3: failed to create local file: %w", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			fmt.Printf("s3: warning: failed to close file: %v\n", err)
		}
	}(file)

	downloader := s3manager.NewDownloader(s.session)

	_, err = downloader.DownloadWithContext(ctx, file, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(normalizePathSeparators(remotePath)),
	})

	if err != nil {
		return fmt.Errorf("s3: failed to download from bucket %s: %w", s.bucket, err)
	}

	return nil
}

// List lists objects in S3 with the given prefix.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]Item, error) {
	svc := s3.New(s.session)
	items := make([]Item, 0)

	var continuationToken *string

	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(normalizePathSeparators(prefix)),
			ContinuationToken: continuationToken,
		}

		resp, err := svc.ListObjectsV2WithContext(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("s3: failed to list objects in bucket %s: %w", s.bucket, err)
		}

		for _, obj := range resp.Contents {
			items = append(items, Item{
				Key:          *obj.Key,
				ModifiedTime: *obj.LastModified,
				Size:         *obj.Size,
				IsDirectory:  false,
			})
		}

		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}

		continuationToken = resp.NextContinuationToken
	}

	return items, nil
}

// Remove deletes an object from S3.
func (s *S3Storage) Remove(ctx context.Context, remotePath string) error {
	svc := s3.New(s.session)

	_, err := svc.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(normalizePathSeparators(remotePath)),
	})

	if err != nil {
		return fmt.Errorf("s3: failed to remove object from bucket %s: %w", s.bucket, err)
	}

	return nil
}

// Close releases resources.
func (s *S3Storage) Close() error {
	return nil
}
