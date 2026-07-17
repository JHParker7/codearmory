package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Store keeps blobs in an S3-compatible object store. This is what lets the
// artifacts service scale past one replica: object storage is shared across nodes, so
// the RWO-PVC single-node pin of fsStore disappears.
//
// "S3-compatible" is deliberate — the target is a self-hosted store (MinIO, or Ceph
// RGW on Proxmox), not AWS specifically. Both are addressed by an endpoint URL + path
// style, which is exactly what forcePathStyle configures below.
type s3Store struct {
	client *s3.Client
	bucket string
}

// newS3Store builds an S3 blob store from environment:
//
//	ARTIFACTS_S3_BUCKET     (required — presence is what selects this backend)
//	ARTIFACTS_S3_ENDPOINT   e.g. http://minio.storage.svc:9000 (empty = real AWS)
//	ARTIFACTS_S3_REGION     default "us-east-1" (MinIO/Ceph ignore it but the SDK wants one)
//	ARTIFACTS_S3_ACCESS_KEY / ARTIFACTS_S3_SECRET_KEY
//	                         static creds; empty falls back to the default AWS chain
//	                         (IRSA/instance profile) for a real-AWS deployment
//	ARTIFACTS_S3_FORCE_PATH_STYLE  default true — MinIO and Ceph RGW need path-style
//	                               (bucket in the path, not a vhost subdomain)
func newS3Store(ctx context.Context) (*s3Store, error) {
	bucket := envOrDefault("ARTIFACTS_S3_BUCKET", "")
	if bucket == "" {
		return nil, errors.New("ARTIFACTS_S3_BUCKET is required for the s3 backend")
	}
	region := envOrDefault("ARTIFACTS_S3_REGION", "us-east-1")

	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(region))
	if ak, sk := os.Getenv("ARTIFACTS_S3_ACCESS_KEY"), os.Getenv("ARTIFACTS_S3_SECRET_KEY"); ak != "" && sk != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ak, sk, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	endpoint := envOrDefault("ARTIFACTS_S3_ENDPOINT", "")
	pathStyle := envOrDefault("ARTIFACTS_S3_FORCE_PATH_STYLE", "true") != "false"
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})
	return &s3Store{client: client, bucket: bucket}, nil
}

func (s *s3Store) Write(ctx context.Context, userID, name string, r io.Reader, limit int64) (int64, string, error) {
	// Stage to a temp file first: this enforces the quota during the copy and yields a
	// known ContentLength + a seekable body, which keeps the PutObject a single simple
	// request rather than multipart with an unknown size. The temp file is per-pod
	// scratch, so it does not reintroduce shared storage.
	tmp, size, digest, err := stageBlob(r, limit)
	if err != nil {
		return size, "", err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(objectKey(userID, name)),
		Body:          f,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return 0, "", fmt.Errorf("s3 put: %w", err)
	}
	return size, digest, nil
}

func (s *s3Store) Open(ctx context.Context, userID, name string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey(userID, name)),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			// Match the filesystem backend's contract: a missing blob is an
			// os.ErrNotExist so the handler reports "content unavailable" the same way.
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *s3Store) Remove(ctx context.Context, userID, name string) error {
	// S3 DeleteObject is idempotent — deleting a missing key succeeds — so this already
	// satisfies "a missing blob is not an error".
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey(userID, name)),
	})
	return err
}
