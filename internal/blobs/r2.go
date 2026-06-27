package blobs

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// R2 is the production [Blobs] backend: an S3-compatible object store (Cloudflare
// R2, or any S3 API) reached with path-style addressing and a custom endpoint.
// Only the bulky content blobs (raw HTML, extracted markdown) live here; metadata
// stays in the Store.
type R2 struct {
	client *s3.Client
	bucket string
}

// R2Config configures an [R2] backend.
type R2Config struct {
	// Endpoint is the S3-compatible API endpoint (e.g. the R2 account URL).
	Endpoint string
	// Region is the signing region; R2 accepts "auto".
	Region string
	// AccessKeyID and SecretAccessKey authenticate requests.
	AccessKeyID     string
	SecretAccessKey string
	// Bucket is the target bucket; keys are stored within it.
	Bucket string
	// HTTPClient, when set, overrides the underlying HTTP client (e.g. for tests).
	HTTPClient *http.Client
}

// NewR2 returns an R2 blob backend from cfg.
func NewR2(cfg R2Config) *R2 {
	opts := s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cmp.Or(cfg.Region, "auto"),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		// Path-style ("/{bucket}/{key}") is required for custom endpoints that do
		// not support virtual-host buckets.
		UsePathStyle: true,
		// Send the body without the SDK's default aws-chunked checksum trailer:
		// some S3-compatible stores (and R2) reject or mishandle it. Checksums are
		// still emitted when an operation explicitly requires one.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}
	if cfg.HTTPClient != nil {
		opts.HTTPClient = cfg.HTTPClient
	}
	return &R2{client: s3.New(opts), bucket: cfg.Bucket}
}

// Put stores data under key with contentType, overwriting any prior value.
func (r *R2) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("blobs: put %q: %w", key, err)
	}
	return nil
}

// Get returns the stored bytes and content type, or [ErrNotFound].
func (r *R2) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("blobs: get %q: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("blobs: read %q: %w", key, err)
	}
	return data, aws.ToString(out.ContentType), nil
}

// Health probes bucket reachability with a deliberately missing key. A typed
// NoSuchKey means the bucket and credentials are usable; other 404s such as
// NoSuchBucket or bare NotFound are configuration failures.
func (r *R2) Health(ctx context.Context) error {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String("__reading-lite-healthcheck-missing__"),
	})
	if err == nil {
		if out.Body != nil {
			_ = out.Body.Close()
		}
		return nil
	}
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey" {
		return nil
	}
	return fmt.Errorf("blobs: r2 health: %w", err)
}

// Delete removes key. Deleting an absent key is a no-op (S3 DeleteObject succeeds
// whether or not the key existed).
func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blobs: delete %q: %w", key, err)
	}
	return nil
}

// isNotFound reports whether err is an S3 "missing object" error. It matches the
// typed NoSuchKey, the NotFound thrown by HeadObject-style 404s, and any bare HTTP
// 404 from an S3-compatible store that does not return a typed code.
func isNotFound(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
