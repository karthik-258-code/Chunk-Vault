// Package store provides blob storage addressed by 32-byte keys.
//
// A key is opaque to the store: it is chosen by the caller, normally as a
// digest of the logical content that the blob encodes, which makes writes
// idempotent. The store does not look inside blobs, so it cannot verify that a
// blob matches its key; callers verify what they read (see package codec).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize is the length in bytes of a Key.
const KeySize = sha256.Size

// Key identifies a blob. It is a 32-byte digest chosen by the caller.
type Key [KeySize]byte

// ErrNotFound is returned when a requested blob does not exist.
var ErrNotFound = errors.New("store: blob not found")

// ErrInvalidKey is returned when a textual key is not 64 lowercase hex digits.
var ErrInvalidKey = errors.New("store: invalid key")

// KeyOf returns the SHA-256 digest of data as a Key.
func KeyOf(data []byte) Key { return sha256.Sum256(data) }

// String returns the key as 64 lowercase hexadecimal characters.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// ParseKey parses the 64-character lowercase hexadecimal form of a Key.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != hex.EncodedLen(KeySize) || s != strings.ToLower(s) {
		return k, fmt.Errorf("%w: %q", ErrInvalidKey, s)
	}
	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return k, fmt.Errorf("%w: %q", ErrInvalidKey, s)
	}
	return k, nil
}

// BlobStore stores blobs under caller-chosen keys. Implementations must be safe
// for concurrent use.
type BlobStore interface {
	// Put stores the blob read from r under key. Putting a key that already
	// exists is a successful no-op and the existing blob is kept. It reports
	// whether the blob was newly written.
	Put(ctx context.Context, key Key, r io.Reader) (created bool, err error)
	// Get opens the blob stored under key. It returns ErrNotFound if absent.
	// The caller must close the returned reader.
	Get(ctx context.Context, key Key) (io.ReadCloser, error)
	// Has reports whether a blob exists under key.
	Has(ctx context.Context, key Key) (bool, error)
	// Size returns the stored size in bytes of the blob under key. It returns
	// ErrNotFound if absent.
	Size(ctx context.Context, key Key) (int64, error)
	// Delete removes the blob under key. Deleting an absent blob is not an
	// error.
	Delete(ctx context.Context, key Key) error
	// List calls fn for every stored key in ascending order, stopping at the
	// first error fn returns.
	List(ctx context.Context, fn func(Key) error) error
}
