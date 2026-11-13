package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

// TestS3RoundTrip exercises Put, Get, Has, Size, Delete and List against a
// real S3-compatible endpoint. It is skipped unless CHUNKVAULT_TEST_S3_BUCKET
// is set.
func TestS3RoundTrip(t *testing.T) {
	bucket := os.Getenv("CHUNKVAULT_TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("CHUNKVAULT_TEST_S3_BUCKET not set")
	}
	endpoint := os.Getenv("CHUNKVAULT_TEST_S3_ENDPOINT")
	region := os.Getenv("CHUNKVAULT_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}

	ctx := context.Background()
	s, err := NewS3(ctx, bucket, region, endpoint)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("s3 round trip test blob")
	key := KeyOf(data)

	// Put
	created, err := s.Put(ctx, key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !created {
		t.Error("Put: expected created=true for a new blob")
	}

	// Idempotent put
	created, err = s.Put(ctx, key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if created {
		t.Error("second Put: expected created=false for existing blob")
	}

	// Has
	has, err := s.Has(ctx, key)
	if err != nil || !has {
		t.Fatalf("Has: err=%v has=%v", err, has)
	}

	// Size
	sz, err := s.Size(ctx, key)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if sz != int64(len(data)) {
		t.Errorf("Size = %d, want %d", sz, len(data))
	}

	// Get
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get content mismatch")
	}

	// List
	found := false
	if err := s.List(ctx, func(k Key) error {
		if k == key {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !found {
		t.Error("key not found in List")
	}

	// Delete
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	has, err = s.Has(ctx, key)
	if err != nil || has {
		t.Fatalf("after Delete: Has err=%v has=%v", err, has)
	}
}
