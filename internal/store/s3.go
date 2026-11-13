package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Store is a BlobStore backed by an S3-compatible object store.
// Objects are stored under keys of the form "ab/cd/<full-hex-key>",
// matching the two-level fan-out of the filesystem store.
type S3Store struct {
	client *s3.Client
	bucket string
}

var _ BlobStore = (*S3Store)(nil)

// NewS3 creates an S3Store. endpoint may be empty to use the default AWS
// endpoint; set it to a MinIO or other S3-compatible URL for local testing.
func NewS3(ctx context.Context, bucket, region, endpoint string) (*S3Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("store: s3 config: %w", err)
	}
	var s3opts []func(*s3.Options)
	if endpoint != "" {
		s3opts = append(s3opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	return &S3Store{client: s3.NewFromConfig(cfg, s3opts...), bucket: bucket}, nil
}

// objectKey converts a blob Key to its S3 object key string.
func (s *S3Store) objectKey(k Key) string {
	h := k.String()
	return h[0:2] + "/" + h[2:4] + "/" + h
}

// Put implements BlobStore.
func (s *S3Store) Put(ctx context.Context, key Key, r io.Reader) (bool, error) {
	// Check existence first to satisfy idempotency.
	has, err := s.Has(ctx, key)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	// S3 PutObject requires a seekable reader or known content length.
	// Buffer the blob so we know its size.
	data, err := io.ReadAll(r)
	if err != nil {
		return false, fmt.Errorf("store: s3 read: %w", err)
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.objectKey(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return false, fmt.Errorf("store: s3 put %s: %w", key, err)
	}
	return true, nil
}

// Get implements BlobStore.
func (s *S3Store) Get(ctx context.Context, key Key) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("store: s3 get %s: %w", key, err)
	}
	return out.Body, nil
}

// Has implements BlobStore.
func (s *S3Store) Has(ctx context.Context, key Key) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: s3 head %s: %w", key, err)
	}
	return true, nil
}

// Size implements BlobStore.
func (s *S3Store) Size(ctx context.Context, key Key) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return 0, fmt.Errorf("store: s3 head %s: %w", key, err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}

// Delete implements BlobStore.
func (s *S3Store) Delete(ctx context.Context, key Key) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("store: s3 delete %s: %w", key, err)
	}
	return nil
}

// List implements BlobStore.
func (s *S3Store) List(ctx context.Context, fn func(Key) error) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("store: s3 list: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			// Object key is "ab/cd/<hex>"; the hex part is the last 64 chars.
			name := *obj.Key
			if len(name) < KeySize*2 {
				continue
			}
			hex := name[len(name)-KeySize*2:]
			k, err := ParseKey(hex)
			if err != nil {
				continue // skip unexpected objects
			}
			if err := fn(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// isNotFound returns true for S3 NoSuchKey and 404-style errors.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	// HeadObject returns a generic HTTPResponseError with 404.
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode() == 404
	}
	return false
}
