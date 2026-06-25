//go:build integration

package blobs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bbell/reading-lite/internal/blobs"
)

// TestR2_MinIORoundTrip proves the R2 adapter against a real S3 implementation
// (MinIO), the production-faithful arm of the httptest round-trip in r2_test.go.
// It skips when Docker is unavailable.
func TestR2_MinIORoundTrip(t *testing.T) {
	ctx := context.Background()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	const (
		user   = "minioadmin"
		secret = "minioadmin"
		bucket = "content"
	)
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:latest",
			ExposedPorts: []string{"9000/tcp"},
			Env:          map[string]string{"MINIO_ROOT_USER": user, "MINIO_ROOT_PASSWORD": secret},
			Cmd:          []string{"server", "/data"},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	cfg := blobs.R2Config{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     user,
		SecretAccessKey: secret,
		Bucket:          bucket,
	}
	createBucket(t, ctx, cfg)

	r := blobs.NewR2(cfg)
	const key = "readings/r1/content.md"
	if err := r.Put(ctx, key, []byte("# minio hello"), "text/markdown"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	data, ct, err := r.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "# minio hello" || ct != "text/markdown" {
		t.Fatalf("Get = %q/%q, want # minio hello / text/markdown", data, ct)
	}

	if err := r.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := r.Get(ctx, key); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func createBucket(t *testing.T, ctx context.Context, cfg blobs.R2Config) {
	t.Helper()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: true,
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
}
