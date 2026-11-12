// Package index keeps a persistent, transactional index of the chunks and
// files in a repository, backed by a single bbolt database file.
//
// The index records, for every chunk, its stored (post-codec) size, its raw
// size and a reference count: the number of times the chunk occurs in the
// chunk lists of all indexed files. It records, for every file, the storage
// key of its manifest, its logical size, its chunk count and its creation
// time. This answers "is this chunk already stored?", "how much space does the
// repository use?" and "which chunks are no longer referenced?" without
// listing the blob store or reading manifests.
//
// The index is a cache, not a source of truth. Everything in it can be rebuilt
// by walking the refs and reading the manifests they point at, and the blobs
// and manifests in the blob store stay authoritative. A missing, corrupt or
// stale index file may therefore be deleted and rebuilt, and nothing is lost
// by doing so. Callers must not rely on the index for anything that cannot be
// recomputed from the blob store.
//
// Every method runs in exactly one bbolt transaction. AddFile, RemoveFile and
// ForgetChunk are atomic, so reference counts never disagree with the file
// records: either both change or neither does. Methods are safe for concurrent
// use; bbolt serialises writers. The index file is locked while open, so a
// second Open of the same file waits and then fails.
//
// A chunk that occurs several times in a file, or in several files, is counted
// once per occurrence. A chunk whose count drops to zero stays in the index
// with a zero count, because its blob is still in the blob store, until
// ForgetChunk removes it after the blob has been deleted.
package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"go.etcd.io/bbolt"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

const (
	filePerm = 0o600

	// chunkRecLen is the encoded length of a chunk record: stored size, raw
	// size and reference count, each a big-endian uint64.
	chunkRecLen = 3 * 8
	// fileHdrLen is the encoded length of the fixed part of a file record:
	// manifest key, logical size, chunk count and creation time (Unix
	// nanoseconds). The chunk keys follow, store.KeySize bytes each. They are
	// kept so that replacing or removing a file can release its chunks without
	// reading the manifest from the blob store.
	fileHdrLen = store.KeySize + 3*8
)

var (
	chunksBucket = []byte("chunks")
	filesBucket  = []byte("files")

	// openTimeout bounds how long Open waits for the file lock. It is a
	// variable so tests can shorten it.
	openTimeout = 5 * time.Second

	// minCreated and maxCreated bound the creation times that fit in the
	// int64 Unix nanoseconds a file record stores.
	minCreated = time.Unix(0, math.MinInt64)
	maxCreated = time.Unix(0, math.MaxInt64)
)

var (
	// ErrNotFound is returned when a requested file or chunk does not exist in
	// the index.
	ErrNotFound = errors.New("index: not found")
	// ErrInvalidName is returned for file names that are empty or too long to
	// be used as an index key.
	ErrInvalidName = errors.New("index: invalid name")
	// ErrInvalidTime is returned by AddFile for a creation time outside the
	// years 1678 to 2262, which cannot be stored.
	ErrInvalidTime = errors.New("index: invalid time")
	// ErrInvalidChunk is returned by AddFile for a chunk with a negative size,
	// or for a chunk list whose sizes overflow.
	ErrInvalidChunk = errors.New("index: invalid chunk")
	// ErrChunkMismatch is returned by AddFile when a chunk key is already
	// indexed with a different raw size. Equal keys must mean equal content, so
	// this indicates a bug or a stale index.
	ErrChunkMismatch = errors.New("index: chunk size mismatch")
	// ErrChunkInUse is returned by ForgetChunk for a chunk that files still
	// reference.
	ErrChunkInUse = errors.New("index: chunk in use")
	// ErrCorrupt is returned when the index contents are malformed or
	// inconsistent. The index is a cache, so the remedy is to delete the file
	// and rebuild it.
	ErrCorrupt = errors.New("index: corrupt")
)

// Chunk describes a chunk as supplied to AddFile.
type Chunk struct {
	// Key is the chunk's storage key in the blob store.
	Key store.Key
	// StoredSize is the size in bytes of the stored blob, after compression
	// and encryption.
	StoredSize int64
	// RawSize is the chunk's length in bytes before compression.
	RawSize int64
}

// ChunkEntry is a chunk as recorded in the index.
type ChunkEntry struct {
	Chunk
	// Refs is the number of occurrences of the chunk in indexed files.
	Refs uint64
}

// File is a file as recorded in the index.
type File struct {
	// Name is the name the file was stored under.
	Name string
	// ManifestKey is the storage key of the file's manifest blob.
	ManifestKey store.Key
	// Size is the logical size of the file in bytes: the sum of the raw sizes
	// of its chunks.
	Size int64
	// ChunkCount is the number of chunks in the file, counting repeats.
	ChunkCount int
	// Created is when the file was stored, in UTC.
	Created time.Time
}

// Stats summarises the index. Computing it scans every record.
type Stats struct {
	// Files is the number of indexed files.
	Files int64
	// LogicalBytes is the sum of the logical sizes of all files, before
	// deduplication.
	LogicalBytes int64
	// UniqueChunks is the number of distinct chunks, including those with no
	// references.
	UniqueChunks int64
	// StoredBytes is the total stored size of all distinct chunks, including
	// those with no references.
	StoredBytes int64
	// ZeroRefChunks is the number of chunks that no file references.
	ZeroRefChunks int64
	// ZeroRefBytes is the total stored size of the chunks that no file
	// references, which is the space garbage collection could reclaim.
	ZeroRefBytes int64
}

// Index is a bbolt-backed chunk and file index. See the package comment.
type Index struct {
	db *bbolt.DB
}

// Open opens the index at path, creating the file and its buckets if they do
// not exist. The directory containing path must exist.
func Open(path string) (*Index, error) {
	db, err := bbolt.Open(path, filePerm, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{chunksBucket, filesBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("index: create bucket %s: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

// Close releases the index file. Closing an already closed index is a no-op.
func (x *Index) Close() error {
	if err := x.db.Close(); err != nil {
		return fmt.Errorf("index: close: %w", err)
	}
	return nil
}

// HasChunk reports whether the chunk is indexed, whether or not any file
// references it.
func (x *Index) HasChunk(key store.Key) (bool, error) {
	var ok bool
	err := x.view(func(cb, _ *bbolt.Bucket) error {
		ok = cb.Get(key[:]) != nil
		return nil
	})
	return ok, err
}

// GetChunk returns the index entry for a chunk, or an error wrapping
// ErrNotFound.
func (x *Index) GetChunk(key store.Key) (ChunkEntry, error) {
	var e ChunkEntry
	err := x.view(func(cb, _ *bbolt.Bucket) error {
		v := cb.Get(key[:])
		if v == nil {
			return fmt.Errorf("%w: chunk %s", ErrNotFound, key)
		}
		var err error
		e, err = decodeChunk(key, v)
		return err
	})
	return e, err
}

// GetFile returns the index entry for a file, or an error wrapping
// ErrNotFound.
func (x *Index) GetFile(name string) (File, error) {
	var f File
	err := x.view(func(_, fb *bbolt.Bucket) error {
		v := fb.Get([]byte(name))
		if v == nil {
			return fmt.Errorf("%w: file %q", ErrNotFound, name)
		}
		var err error
		f, _, err = decodeFile(name, v)
		return err
	})
	return f, err
}

// AddFile records a file and takes a reference on each of its chunks, one per
// occurrence, indexing chunks it has not seen before. If a file with the same
// name already exists it is replaced, and the references held by the old
// version are released, all in the same transaction. The file's logical size
// is the sum of the chunks' raw sizes. A file may have no chunks.
//
// On error nothing is changed. A chunk that is already indexed keeps its
// recorded stored size; its raw size must match, see ErrChunkMismatch.
func (x *Index) AddFile(name string, manifestKey store.Key, created time.Time, chunks []Chunk) error {
	if err := validateName(name); err != nil {
		return err
	}
	// The creation time is stored as Unix nanoseconds, which only covers the
	// years 1678 to 2262; UnixNano is undefined outside that range.
	if created.Before(minCreated) || created.After(maxCreated) {
		return fmt.Errorf("%w: creation time %v is out of range", ErrInvalidTime, created)
	}
	var size int64
	for i, c := range chunks {
		if c.StoredSize < 0 || c.RawSize < 0 {
			return fmt.Errorf("%w: chunk %d has a negative size", ErrInvalidChunk, i)
		}
		if size > math.MaxInt64-c.RawSize {
			return fmt.Errorf("%w: file size overflows", ErrInvalidChunk)
		}
		size += c.RawSize
	}
	rec := encodeFile(manifestKey, size, created, chunks)
	return x.update(func(cb, fb *bbolt.Bucket) error {
		for i, c := range chunks {
			if err := addRef(cb, c); err != nil {
				return fmt.Errorf("chunk %d: %w", i, err)
			}
		}
		// Copy the old record: it points into memory bbolt may remap while the
		// transaction writes.
		if old := fb.Get([]byte(name)); old != nil {
			if err := releaseFile(cb, name, append([]byte(nil), old...)); err != nil {
				return err
			}
		}
		if err := fb.Put([]byte(name), rec); err != nil {
			return fmt.Errorf("index: put file %q: %w", name, err)
		}
		return nil
	})
}

// RemoveFile deletes a file's record and releases one reference per
// occurrence of each of its chunks. Chunks that reach zero references stay in
// the index until ForgetChunk. It returns an error wrapping ErrNotFound if the
// file is absent.
func (x *Index) RemoveFile(name string) error {
	return x.update(func(cb, fb *bbolt.Bucket) error {
		v := fb.Get([]byte(name))
		if v == nil {
			return fmt.Errorf("%w: file %q", ErrNotFound, name)
		}
		if err := releaseFile(cb, name, append([]byte(nil), v...)); err != nil {
			return err
		}
		if err := fb.Delete([]byte(name)); err != nil {
			return fmt.Errorf("index: delete file %q: %w", name, err)
		}
		return nil
	})
}

// ForgetChunk drops the index entry of a chunk that no file references,
// normally after its blob has been deleted from the blob store. It returns an
// error wrapping ErrChunkInUse if the chunk still has references. Forgetting
// an unindexed chunk is not an error.
func (x *Index) ForgetChunk(key store.Key) error {
	return x.update(func(cb, _ *bbolt.Bucket) error {
		v := cb.Get(key[:])
		if v == nil {
			return nil
		}
		e, err := decodeChunk(key, v)
		if err != nil {
			return err
		}
		if e.Refs > 0 {
			return fmt.Errorf("%w: chunk %s has %d references", ErrChunkInUse, key, e.Refs)
		}
		if err := cb.Delete(key[:]); err != nil {
			return fmt.Errorf("index: delete chunk %s: %w", key, err)
		}
		return nil
	})
}

// Stats scans the index and returns a summary of it.
func (x *Index) Stats() (Stats, error) {
	var s Stats
	err := x.view(func(cb, fb *bbolt.Bucket) error {
		err := cb.ForEach(func(k, v []byte) error {
			e, err := decodeChunkBytes(k, v)
			if err != nil {
				return err
			}
			s.UniqueChunks++
			s.StoredBytes += e.StoredSize
			if e.Refs == 0 {
				s.ZeroRefChunks++
				s.ZeroRefBytes += e.StoredSize
			}
			return nil
		})
		if err != nil {
			return err
		}
		return fb.ForEach(func(k, v []byte) error {
			f, _, err := decodeFile(string(k), v)
			if err != nil {
				return err
			}
			s.Files++
			s.LogicalBytes += f.Size
			return nil
		})
	})
	return s, err
}

// ForEachChunk calls fn for every indexed chunk in ascending key order,
// stopping at the first error fn returns and returning it unchanged. fn runs
// inside a read transaction, so it must not call methods that write to the
// index.
func (x *Index) ForEachChunk(fn func(ChunkEntry) error) error {
	return x.view(func(cb, _ *bbolt.Bucket) error {
		return cb.ForEach(func(k, v []byte) error {
			e, err := decodeChunkBytes(k, v)
			if err != nil {
				return err
			}
			return fn(e)
		})
	})
}

// ForEachFile calls fn for every indexed file in ascending name order,
// stopping at the first error fn returns and returning it unchanged. fn runs
// inside a read transaction, so it must not call methods that write to the
// index.
func (x *Index) ForEachFile(fn func(File) error) error {
	return x.view(func(_, fb *bbolt.Bucket) error {
		return fb.ForEach(func(k, v []byte) error {
			f, _, err := decodeFile(string(k), v)
			if err != nil {
				return err
			}
			return fn(f)
		})
	})
}

// view runs fn in a read-only transaction. Errors from fn are returned as they
// are, since they already carry the package prefix; only errors from bbolt
// itself, such as a closed database, are wrapped here.
func (x *Index) view(fn func(cb, fb *bbolt.Bucket) error) error {
	var fnErr error
	err := x.db.View(func(tx *bbolt.Tx) error {
		cb, fb, err := buckets(tx)
		if err != nil {
			fnErr = err
			return err
		}
		fnErr = fn(cb, fb)
		return fnErr
	})
	if fnErr != nil {
		return fnErr
	}
	if err != nil {
		return fmt.Errorf("index: read transaction: %w", err)
	}
	return nil
}

// update runs fn in a read-write transaction that commits only if fn returns
// nil. Error handling is as for view.
func (x *Index) update(fn func(cb, fb *bbolt.Bucket) error) error {
	var fnErr error
	err := x.db.Update(func(tx *bbolt.Tx) error {
		cb, fb, err := buckets(tx)
		if err != nil {
			fnErr = err
			return err
		}
		fnErr = fn(cb, fb)
		return fnErr
	})
	if fnErr != nil {
		return fnErr
	}
	if err != nil {
		return fmt.Errorf("index: write transaction: %w", err)
	}
	return nil
}

// buckets returns the chunk and file buckets of tx.
func buckets(tx *bbolt.Tx) (cb, fb *bbolt.Bucket, err error) {
	cb, fb = tx.Bucket(chunksBucket), tx.Bucket(filesBucket)
	if cb == nil || fb == nil {
		return nil, nil, fmt.Errorf("%w: missing bucket", ErrCorrupt)
	}
	return cb, fb, nil
}

// validateName checks that name can be used as a bbolt key.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if len(name) > bbolt.MaxKeySize {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidName, bbolt.MaxKeySize)
	}
	return nil
}

// addRef takes one reference on c, indexing it first if it is new. bbolt
// keeps the value passed to Put until the transaction commits, so every Put
// gets a freshly allocated record.
func addRef(cb *bbolt.Bucket, c Chunk) error {
	e := ChunkEntry{Chunk: c}
	if v := cb.Get(c.Key[:]); v != nil {
		old, err := decodeChunk(c.Key, v)
		if err != nil {
			return err
		}
		if old.RawSize != c.RawSize {
			return fmt.Errorf("%w: chunk %s is indexed with raw size %d, not %d",
				ErrChunkMismatch, c.Key, old.RawSize, c.RawSize)
		}
		e = old
	}
	e.Refs++
	if err := cb.Put(c.Key[:], encodeChunk(e)); err != nil {
		return fmt.Errorf("index: put chunk %s: %w", c.Key, err)
	}
	return nil
}

// releaseFile drops one reference for each chunk in rec, the stored record of
// the file called name. rec must not alias bbolt memory.
func releaseFile(cb *bbolt.Bucket, name string, rec []byte) error {
	_, keys, err := decodeFile(name, rec)
	if err != nil {
		return err
	}
	for i := 0; i < len(keys); i += store.KeySize {
		var key store.Key
		copy(key[:], keys[i:i+store.KeySize])
		v := cb.Get(key[:])
		if v == nil {
			return fmt.Errorf("%w: file %q references unindexed chunk %s", ErrCorrupt, name, key)
		}
		e, err := decodeChunk(key, v)
		if err != nil {
			return err
		}
		if e.Refs == 0 {
			return fmt.Errorf("%w: chunk %s has no reference left to release for file %q", ErrCorrupt, key, name)
		}
		e.Refs--
		if err := cb.Put(key[:], encodeChunk(e)); err != nil {
			return fmt.Errorf("index: put chunk %s: %w", key, err)
		}
	}
	return nil
}

// encodeChunk serialises a chunk record. The key is not part of it.
func encodeChunk(e ChunkEntry) []byte {
	b := make([]byte, chunkRecLen)
	binary.BigEndian.PutUint64(b[0:], uint64(e.StoredSize))
	binary.BigEndian.PutUint64(b[8:], uint64(e.RawSize))
	binary.BigEndian.PutUint64(b[16:], e.Refs)
	return b
}

// decodeChunkBytes decodes a chunk record whose key is given as raw bytes.
func decodeChunkBytes(k, v []byte) (ChunkEntry, error) {
	if len(k) != store.KeySize {
		return ChunkEntry{}, fmt.Errorf("%w: chunk key of %d bytes", ErrCorrupt, len(k))
	}
	var key store.Key
	copy(key[:], k)
	return decodeChunk(key, v)
}

// decodeChunk decodes the chunk record v stored under key.
func decodeChunk(key store.Key, v []byte) (ChunkEntry, error) {
	if len(v) != chunkRecLen {
		return ChunkEntry{}, fmt.Errorf("%w: chunk %s record of %d bytes", ErrCorrupt, key, len(v))
	}
	stored := binary.BigEndian.Uint64(v[0:])
	raw := binary.BigEndian.Uint64(v[8:])
	if stored > math.MaxInt64 || raw > math.MaxInt64 {
		return ChunkEntry{}, fmt.Errorf("%w: chunk %s has an out-of-range size", ErrCorrupt, key)
	}
	return ChunkEntry{
		Chunk: Chunk{Key: key, StoredSize: int64(stored), RawSize: int64(raw)},
		Refs:  binary.BigEndian.Uint64(v[16:]),
	}, nil
}

// encodeFile serialises a file record: the fixed header followed by the chunk
// keys in order.
func encodeFile(manifestKey store.Key, size int64, created time.Time, chunks []Chunk) []byte {
	b := make([]byte, fileHdrLen, fileHdrLen+len(chunks)*store.KeySize)
	copy(b, manifestKey[:])
	binary.BigEndian.PutUint64(b[store.KeySize:], uint64(size))
	binary.BigEndian.PutUint64(b[store.KeySize+8:], uint64(len(chunks)))
	binary.BigEndian.PutUint64(b[store.KeySize+16:], uint64(created.UnixNano()))
	for _, c := range chunks {
		b = append(b, c.Key[:]...)
	}
	return b
}

// decodeFile decodes the file record v stored under name. It also returns the
// concatenated chunk keys, which alias v.
func decodeFile(name string, v []byte) (File, []byte, error) {
	if len(v) < fileHdrLen {
		return File{}, nil, fmt.Errorf("%w: file %q record of %d bytes", ErrCorrupt, name, len(v))
	}
	size := binary.BigEndian.Uint64(v[store.KeySize:])
	count := binary.BigEndian.Uint64(v[store.KeySize+8:])
	rest := v[fileHdrLen:]
	if size > math.MaxInt64 || len(rest)%store.KeySize != 0 || count != uint64(len(rest)/store.KeySize) {
		return File{}, nil, fmt.Errorf("%w: file %q record is inconsistent", ErrCorrupt, name)
	}
	f := File{
		Name:       name,
		Size:       int64(size),
		ChunkCount: int(count),
		Created:    time.Unix(0, int64(binary.BigEndian.Uint64(v[store.KeySize+16:]))).UTC(),
	}
	copy(f.ManifestKey[:], v[:store.KeySize])
	return f, rest, nil
}
