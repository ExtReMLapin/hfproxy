package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3 struct {
	client               *s3.Client
	upload               *manager.Uploader
	bucket               string
	prefix               string
	serverSideEncryption bool
}

func New(ctx context.Context, endpoint, region, bucket, prefix, accessKey, secretKey string, pathStyle, serverSideEncryption bool) (*S3, error) {
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if accessKey != "" {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = pathStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &S3{client: client, upload: manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 64 << 20
		u.Concurrency = 3
	}), bucket: bucket, prefix: strings.Trim(prefix, "/"), serverSideEncryption: serverSideEncryption}, nil
}

func (s *S3) Key(sha256 string) string {
	key := "blobs/sha256/" + sha256[:2] + "/" + sha256
	if s.prefix != "" {
		return s.prefix + "/" + key
	}
	return key
}

func (s *S3) PartialKey(id string, part int) string {
	key := fmt.Sprintf("partials/%s/%08d", id, part)
	if s.prefix != "" {
		return s.prefix + "/" + key
	}
	return key
}

func (s *S3) UploadFile(ctx context.Context, file *os.File, key, contentType string, size int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: file,
		ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
	}
	if s.serverSideEncryption {
		input.ServerSideEncryption = "AES256"
	}
	_, err := s.upload.Upload(ctx, input)
	if err != nil {
		return fmt.Errorf("upload object to S3: %w", err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key, byteRange string) (*s3.GetObjectOutput, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if byteRange != "" {
		input.Range = aws.String(byteRange)
	}
	return s.client.GetObject(ctx, input)
}

func (s *S3) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	return s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *S3) Ready(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func (s *S3) Bucket() string { return s.bucket }
