// Package vault is the service layer of chunkvault. It ties the chunker, the
// blob codec and the blob store together into a deduplicating file store: files
// are split into content-defined chunks, each chunk is compressed and
// optionally encrypted by package codec and stored once under its storage key
// (SHA-256, or a keyed HMAC-SHA-256 for an encrypted repository), and a
// manifest listing the chunks lets the file be reassembled and verified. The
// manifest is stored through the same codec, so an encrypted repository
// encrypts it too.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/chunker"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/codec"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/keys"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

const (
	// DefaultMaxManifestSize is the default limit on the encoded size of a
	// manifest, before compression and encryption. A manifest is held in memory in full, both when it is built by
	// Put and when it is read back, so it bounds the memory a single file can
	// cost: roughly 90 bytes per chunk.
	DefaultMaxManifestSize = 1 << 30

	// indexFile is the name of the chunk and file index in a repository.
	indexFile = "index.db"
)

var (
	// ErrNotInitialized is returned by Open when the directory is not a vault.
	ErrNotInitialized = errors.New("vault: repository not initialized")
	// ErrCorrupt is returned when stored data fails verification.
	ErrCorrupt = errors.New("vault: corrupt data")
	// ErrManifestTooLarge is returned when a file's encoded manifest exceeds
	// the vault's manifest size limit, either by Put while building it or when
	// reading back a stored manifest.
	ErrManifestTooLarge = errors.New("vault: manifest too large")
	// ErrLegacyRepo is returned by Open for a repository in the M1 format,
	// which has no config.json and cannot be read by this version.
	ErrLegacyRepo = errors.New("vault: repository is in the old pre-config format and cannot be opened; run 'chunkvault init' on a new directory and put your files again")
	// ErrPassphraseRequired is returned by Open for an encrypted repository
	// when no passphrase source was given, see WithPassphrase.
	ErrPassphraseRequired = errors.New("vault: repository is encrypted and needs a passphrase")
	// ErrIndexMissing is returned by Open for a repository that has no
	// index.db, such as one created before the index existed. Open does not
	// rebuild it, because that reads every manifest; Reindex does.
	ErrIndexMissing = errors.New("vault: repository has no index")
)

// Vault is a deduplicating file store. It is safe for concurrent use as long
// as the underlying BlobStore is. Close it when done: it holds the index file
// open and locked.
//
// # Crash consistency
//
// The blobs, manifests and refs are the source of truth. The index (package
// index) is a cache of them that Put and Remove update last, in this order:
//
//  1. Put writes every chunk blob, then the manifest blob, then the ref
//     (store.Refs.Set), and only then calls index.AddFile.
//  2. Remove deletes the ref, and only then calls index.RemoveFile.
//
// A crash between the ref and the index update therefore leaves a repository
// that is fully readable (Get, List and Put never consult the index for
// anything they cannot recompute) but whose index is stale: after a Put it
// lacks the new file, after a Remove it still holds the removed one. Reindex
// repairs it by rebuilding the index from the refs. Anything destructive that
// trusts reference counts, such as garbage collection, must run Reindex first.
type Vault struct {
	blobs store.BlobStore
	refs  *store.Refs
	idx   *index.Index
	codec *codec.Codec
	cfg   chunker.Config
	// pass supplies the passphrase of an encrypted repository to Open.
	pass func() ([]byte, error)
	now  func() time.Time
	// maxManifest is the largest encoded manifest Put will write or
	// loadManifest will read.
	maxManifest int64
}

// Option customises a Vault.
type Option func(*Vault)

// WithChunkerConfig overrides the chunker parameters used by Put.
func WithChunkerConfig(cfg chunker.Config) Option {
	return func(v *Vault) { v.cfg = cfg }
}

// WithPassphrase sets the source of the passphrase used by Open to unlock an
// encrypted repository. It is only called if the repository is encrypted, and
// the returned slice is wiped after use. Open returns ErrPassphraseRequired
// for an encrypted repository if it is not set.
func WithPassphrase(fn func() ([]byte, error)) Option {
	return func(v *Vault) { v.pass = fn }
}

// WithIndex sets the index the Vault maintains. New requires it. Open opens
// index.db itself and does not need it. The Vault takes ownership: Close closes
// the index.
func WithIndex(x *index.Index) Option {
	return func(v *Vault) { v.idx = x }
}

// WithClock overrides the clock used to stamp manifests, mainly for tests.
func WithClock(now func() time.Time) Option {
	return func(v *Vault) { v.now = now }
}

// WithMaxManifestSize overrides the limit on the encoded size of a manifest,
// see DefaultMaxManifestSize. It must be positive.
func WithMaxManifestSize(n int64) Option {
	return func(v *Vault) { v.maxManifest = n }
}

// New builds a Vault from existing components, which must include an index,
// see WithIndex. It returns an error wrapping chunker.ErrInvalidConfig if the
// chunker configuration is invalid.
func New(blobs store.BlobStore, refs *store.Refs, c *codec.Codec, opts ...Option) (*Vault, error) {
	v, err := build(blobs, refs, c, opts...)
	if err != nil {
		return nil, err
	}
	if v.idx == nil {
		return nil, errors.New("vault: nil index")
	}
	return v, nil
}

// build is New without the requirement for an index, which Reindex does not
// have while it is rebuilding it.
func build(blobs store.BlobStore, refs *store.Refs, c *codec.Codec, opts ...Option) (*Vault, error) {
	if c == nil {
		return nil, errors.New("vault: nil codec")
	}
	v := &Vault{
		blobs: blobs, refs: refs, codec: c, cfg: chunker.DefaultConfig(), now: time.Now,
		maxManifest: DefaultMaxManifestSize,
	}
	for _, o := range opts {
		o(v)
	}
	if v.maxManifest <= 0 {
		return nil, fmt.Errorf("vault: invalid manifest size limit %d", v.maxManifest)
	}
	if err := v.cfg.Validate(); err != nil {
		return nil, err
	}
	return v, nil
}

// Init creates a new repository in dir, which may already exist but must not
// already be a repository. By default blobs are compressed but not encrypted;
// see InitEncrypted. Only the salt, the KDF parameters and the wrapped master
// key are written to disk, never the passphrase or a raw key.
func Init(dir string, opts ...InitOption) error {
	s := initSettings{chunker: chunker.DefaultConfig(), compress: true, kdf: keys.DefaultKDFParams()}
	for _, o := range opts {
		o(&s)
	}
	for _, name := range []string{configFile, legacyMetaFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("vault: repository already exists at %s", dir)
		}
	}
	cfg := Config{
		FormatVersion: ConfigVersion,
		Chunker:       ChunkerParams{MinSize: s.chunker.MinSize, AvgSize: s.chunker.AvgSize, MaxSize: s.chunker.MaxSize},
		Compression:   s.compress,
		Encryption:    s.encrypt,
	}
	if s.encrypt {
		w, master, err := keys.NewWrapped(s.passphrase, s.kdf)
		if err != nil {
			return fmt.Errorf("vault: set up encryption: %w", err)
		}
		clear(master[:])
		cfg.Key = &w
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	if _, err := store.NewFS(dir); err != nil {
		return err
	}
	if _, err := store.NewRefs(dir); err != nil {
		return err
	}
	x, err := index.Open(filepath.Join(dir, indexFile))
	if err != nil {
		return err
	}
	if err := x.Close(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: encode config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFile), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("vault: write config: %w", err)
	}
	return nil
}

// Open opens the repository in dir created by Init. It validates config.json,
// and for an encrypted repository unlocks the master key with the passphrase
// from WithPassphrase; a wrong passphrase returns an error wrapping
// keys.ErrWrongPassphrase. A repository in the M1 format, which has no
// config.json, returns ErrLegacyRepo. WithChunkerConfig overrides the chunker
// parameters recorded in the config.
//
// Open also opens, and locks, the repository's index.db; call Close to release
// it. A repository without an index.db, which is one created before the index
// existed, returns ErrIndexMissing instead of being rebuilt on the spot: the
// rebuild reads every manifest, which can take long, and should not be a
// hidden side effect of get or ls. Run Reindex to create the index.
func Open(dir string, opts ...Option) (*Vault, error) {
	v, err := openParts(dir, opts)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, indexFile)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s has no %s; run 'chunkvault reindex' to build it", ErrIndexMissing, dir, indexFile)
		}
		return nil, fmt.Errorf("vault: stat index: %w", err)
	}
	x, err := index.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vault: %w (if %s is damaged rather than in use by another chunkvault process, 'chunkvault reindex' rebuilds it)", err, indexFile)
	}
	v.idx = x
	return v, nil
}

// openParts builds the Vault for the repository in dir without its index.
func openParts(dir string, opts []Option) (*Vault, error) {
	cfg, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	// Apply the options to a scratch vault to learn the passphrase source
	// before the codec, which build needs, can be created.
	var probe Vault
	for _, o := range opts {
		o(&probe)
	}
	c, err := cfg.newCodec(dir, probe.pass)
	if err != nil {
		return nil, err
	}
	blobs, err := store.NewFS(dir)
	if err != nil {
		return nil, err
	}
	refs, err := store.NewRefs(dir)
	if err != nil {
		return nil, err
	}
	return build(blobs, refs, c, append([]Option{WithChunkerConfig(cfg.chunkerConfig())}, opts...)...)
}

// Close releases the index file. Closing a Vault twice is a no-op.
func (v *Vault) Close() error { return v.idx.Close() }

// Encrypted reports whether the repository encrypts its blobs.
func (v *Vault) Encrypted() bool { return v.codec.Encrypted() }

// PutStats summarises a Put.
type PutStats struct {
	// ManifestKey is the blob store key of the file's manifest.
	ManifestKey store.Key
	// TotalBytes is the size of the stored file.
	TotalBytes int64
	// TotalChunks is the number of chunks the file was split into.
	TotalChunks int
	// NewChunks is the number of chunks that were not already stored.
	NewChunks int
	// NewBytes is the size of the newly stored chunks.
	NewBytes int64
	// DedupBytes is the size of the chunks that were already stored and so were
	// not written again.
	DedupBytes int64
}

// Put reads r to the end, stores it as name, and returns deduplication
// statistics. File data is streamed: at most a couple of maximum-size chunks
// are held in memory, plus the manifest, which grows with the number of chunks
// and is capped by the manifest size limit. Put fails with ErrManifestTooLarge
// as soon as the manifest is known to exceed that limit, before the rest of the
// input is read or stored. An existing file called name is replaced, and the
// index releases the chunks of the old version. If Put fails part way, chunks
// already written remain in the store but name is left unchanged. If only the
// final index update fails, name has been replaced and the error says so:
// Reindex repairs the index, see the crash consistency notes on Vault.
//
// Put always checks the blob store, never the index, for chunks that already
// exist, because the index is only a cache and may lack or lag behind them.
func (v *Vault) Put(ctx context.Context, name string, r io.Reader) (PutStats, error) {
	var stats PutStats
	if err := store.ValidateName(name); err != nil {
		return stats, err
	}
	ck, err := chunker.New(r, v.cfg)
	if err != nil {
		return stats, err
	}
	m := &Manifest{Version: ManifestVersion, Name: name}
	// encoded is a lower bound on the final encoded manifest size: the empty
	// manifest plus every chunk reference and separator. It ignores that the
	// total size field grows wider, so exceeding the limit here means the final
	// manifest will too; the exact size is checked again once it is encoded.
	skeleton, err := (&Manifest{Version: ManifestVersion, Name: name, Chunks: []ChunkRef{}}).encode()
	if err != nil {
		return stats, err
	}
	encoded := int64(len(skeleton))

	// Phase 1: read all chunks sequentially (chunker is not concurrent-safe).
	type rawChunk struct {
		offset int64
		key    store.Key
		data   []byte
		ref    ChunkRef
	}
	var raw []rawChunk
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		ch, err := ck.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("vault: read input: %w", err)
		}
		// Copy ch.Data: the chunker reuses its internal buffer on the next Next() call.
		data := make([]byte, len(ch.Data))
		copy(data, ch.Data)
		key := v.codec.Key(data)
		size := len(data)
		ref := ChunkRef{Hash: key.String(), Size: size}
		encoded += ref.encodedLen()
		if len(raw) > 0 {
			encoded++ // comma between references
		}
		if encoded > v.maxManifest {
			return stats, fmt.Errorf("%w: exceeds the %d byte limit after %d chunks", ErrManifestTooLarge, v.maxManifest, len(raw))
		}
		raw = append(raw, rawChunk{offset: ch.Offset, key: key, data: data, ref: ref})
	}

	// Phase 2: store chunks in parallel, preserving order in results.
	type blobResult struct {
		created bool
		stored  int64
	}
	results, err := workerPool(ctx, raw, defaultWorkers(), func(ctx context.Context, rc rawChunk) (blobResult, error) {
		created, stored, err := v.putBlob(ctx, rc.key, rc.data)
		return blobResult{created: created, stored: stored}, err
	})
	if err != nil {
		return stats, fmt.Errorf("vault: store chunk: %w", err)
	}

	var chunks []index.Chunk
	for i, rc := range raw {
		r := results[i]
		chunks = append(chunks, index.Chunk{Key: rc.key, StoredSize: r.stored, RawSize: int64(len(rc.data))})
		m.Chunks = append(m.Chunks, rc.ref)
		m.Size += int64(len(rc.data))
		stats.TotalChunks++
		if r.created {
			stats.NewChunks++
			stats.NewBytes += int64(len(rc.data))
		} else {
			stats.DedupBytes += int64(len(rc.data))
		}
	}
	if m.Chunks == nil {
		m.Chunks = []ChunkRef{}
	}
	m.Created = v.now().UTC().Truncate(time.Second)
	data, err := m.encode()
	if err != nil {
		return stats, err
	}
	if int64(len(data)) > v.maxManifest {
		return stats, fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrManifestTooLarge, len(data), v.maxManifest)
	}
	mkey := v.codec.Key(data)
	if _, _, err := v.putBlob(ctx, mkey, data); err != nil {
		return stats, fmt.Errorf("vault: store manifest: %w", err)
	}
	if err := v.refs.Set(name, mkey); err != nil {
		return stats, err
	}
	stats.ManifestKey = mkey
	stats.TotalBytes = m.Size
	// The index comes last, see the crash consistency notes on Vault.
	if err := v.idx.AddFile(name, mkey, m.Created, chunks); err != nil {
		return stats, fmt.Errorf("vault: stored %q but could not update the index (run 'chunkvault reindex'): %w", name, err)
	}
	return stats, nil
}

// Remove deletes the file called name: its ref goes first and the index entry,
// which releases one reference per chunk occurrence, second, see the crash
// consistency notes on Vault. A file the index does not know, because a crash
// left it stale, is still removed. Remove returns an error wrapping
// store.ErrNotFound if name does not exist.
//
// The chunk and manifest blobs stay in the blob store, also those that no file
// references any more: nothing deletes blobs until garbage collection exists.
func (v *Vault) Remove(name string) error {
	if err := v.refs.Remove(name); err != nil {
		return err
	}
	if err := v.idx.RemoveFile(name); err != nil && !errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("vault: removed %q but could not update the index (run 'chunkvault reindex'): %w", name, err)
	}
	return nil
}

// ReindexStats summarises a Reindex.
type ReindexStats struct {
	// Files is the number of files indexed.
	Files int64
	// Chunks is the number of distinct chunks the files reference.
	Chunks int64
}

// Reindex rebuilds the index of the repository in dir from its refs and the
// manifests they point at. The new index is built in a temporary file in dir
// and renamed over index.db only once complete, so a failed Reindex leaves the
// old index untouched. The old index is not read, so Reindex repairs a missing
// or damaged index.db, and it does not Open the Vault, which would fail on one.
//
// It reads every manifest and the size of every chunk blob, and fails at the
// first ref whose manifest or chunks are missing or corrupt. Chunk blobs that
// no ref uses are not entered into the index. It must not run while another
// chunkvault command is using the repository: that process would keep
// updating the replaced file, and a Put that completes mid-way may be missed.
// Options are as for Open.
func Reindex(ctx context.Context, dir string, opts ...Option) (ReindexStats, error) {
	v, err := openParts(dir, opts)
	if err != nil {
		return ReindexStats{}, err
	}
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return ReindexStats{}, fmt.Errorf("vault: create temporary index: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Close(); err != nil {
		return ReindexStats{}, fmt.Errorf("vault: create temporary index: %w", err)
	}
	stats, err := v.rebuildIndex(ctx, tmpPath)
	if err != nil {
		return ReindexStats{}, err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, indexFile)); err != nil {
		return ReindexStats{}, fmt.Errorf("vault: replace index: %w", err)
	}
	renamed = true
	return stats, nil
}

// rebuildIndex fills the new index file at path from the refs. It does not use
// v.idx.
func (v *Vault) rebuildIndex(ctx context.Context, path string) (stats ReindexStats, err error) {
	x, err := index.Open(path)
	if err != nil {
		return stats, err
	}
	defer func() {
		if cerr := x.Close(); err == nil && cerr != nil {
			stats, err = ReindexStats{}, cerr
		}
	}()
	names, err := v.refs.List()
	if err != nil {
		return stats, err
	}
	stored := make(map[store.Key]int64)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		mkey, err := v.refs.Get(name)
		if err != nil {
			return stats, fmt.Errorf("vault: reindex %q: %w", name, err)
		}
		m, err := v.loadManifest(ctx, mkey)
		if err != nil {
			return stats, fmt.Errorf("vault: reindex %q: %w", name, err)
		}
		chunks := make([]index.Chunk, 0, len(m.Chunks))
		for i, c := range m.Chunks {
			key, err := store.ParseKey(c.Hash)
			if err != nil {
				return stats, fmt.Errorf("vault: reindex %q: %w: %w", name, ErrCorrupt, err)
			}
			// The stored size is the size of the blob on disk, which is not
			// read, so a truncated blob is indexed with its damaged size.
			// That is why Verify does not call a size mismatch index-only.
			size, ok := stored[key]
			if !ok {
				if size, err = v.blobs.Size(ctx, key); err != nil {
					return stats, fmt.Errorf("vault: reindex %q: chunk %d: %w", name, i, err)
				}
				stored[key] = size
			}
			chunks = append(chunks, index.Chunk{Key: key, StoredSize: size, RawSize: int64(c.Size)})
		}
		if err := x.AddFile(name, mkey, m.Created, chunks); err != nil {
			return stats, fmt.Errorf("vault: reindex %q: %w", name, err)
		}
	}
	st, err := x.Stats()
	if err != nil {
		return stats, err
	}
	return ReindexStats{Files: st.Files, Chunks: st.UniqueChunks}, nil
}

// Get writes the file stored as name to w. Every chunk is re-hashed and
// checked against the manifest before it is written, so corrupted chunks are
// never emitted; on a mismatch Get returns an error wrapping ErrCorrupt, with
// the preceding verified chunks already written to w. It returns an error
// wrapping store.ErrNotFound if name does not exist.
func (v *Vault) Get(ctx context.Context, name string, w io.Writer) error {
	m, err := v.manifest(ctx, name)
	if err != nil {
		return err
	}
	for i, c := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := v.readChunk(ctx, c)
		if err != nil {
			return fmt.Errorf("chunk %d of %q: %w", i, name, err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("vault: write output: %w", err)
		}
	}
	return nil
}

// Entry describes a stored file.
type Entry struct {
	// Name is the name the file was stored under.
	Name string
	// Size is the file size in bytes.
	Size int64
	// Chunks is the number of chunks in the file.
	Chunks int
	// Created is when the file was stored.
	Created time.Time
	// ManifestKey is the blob store key of the file's manifest.
	ManifestKey store.Key
}

// List returns all stored files ordered by name.
func (v *Vault) List(ctx context.Context) ([]Entry, error) {
	names, err := v.refs.List()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := v.refs.Get(name)
		if err != nil {
			return nil, err
		}
		m, err := v.loadManifest(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", name, err)
		}
		entries = append(entries, Entry{
			Name: name, Size: m.Size, Chunks: len(m.Chunks), Created: m.Created, ManifestKey: key,
		})
	}
	return entries, nil
}

// manifest resolves name to its verified manifest.
func (v *Vault) manifest(ctx context.Context, name string) (*Manifest, error) {
	key, err := v.refs.Get(name)
	if err != nil {
		return nil, err
	}
	return v.loadManifest(ctx, key)
}

// putBlob encodes plaintext and stores it under key, skipping the encoding
// work if a blob already exists there. It reports whether a blob was written
// and the stored size of the blob under key either way.
func (v *Vault) putBlob(ctx context.Context, key store.Key, plaintext []byte) (created bool, stored int64, err error) {
	n, err := v.blobs.Size(ctx, key)
	if err == nil {
		return false, n, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return false, 0, err
	}
	blob, err := v.codec.Encode(key, plaintext)
	if err != nil {
		return false, 0, err
	}
	created, err = v.blobs.Put(ctx, key, bytes.NewReader(blob))
	if err != nil {
		return false, 0, err
	}
	if created {
		return true, int64(len(blob)), nil
	}
	// Another writer stored it first.
	n, err = v.blobs.Size(ctx, key)
	if err != nil {
		return false, 0, err
	}
	return false, n, nil
}

// readBlob loads the blob under key, then verifies and decodes it, returning
// at most maxPlain bytes of plaintext. Failures of the codec (a bad GCM tag, a
// plaintext that does not hash to key, a malformed blob) wrap ErrCorrupt;
// plaintext over maxPlain wraps codec.ErrTooLarge as well.
func (v *Vault) readBlob(ctx context.Context, key store.Key, maxPlain int64) ([]byte, error) {
	rc, err := v.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	limit := v.codec.MaxEncodedSize(maxPlain)
	blob, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("vault: read blob %s: %w", key, err)
	}
	if int64(len(blob)) > limit {
		return nil, fmt.Errorf("%w: blob %s: %w", ErrCorrupt, key, codec.ErrTooLarge)
	}
	data, err := v.codec.Decode(key, blob, maxPlain)
	if err != nil {
		return nil, fmt.Errorf("%w: blob %s: %w", ErrCorrupt, key, err)
	}
	return data, nil
}

// loadManifest reads the manifest blob under key, verifies it and decodes it.
func (v *Vault) loadManifest(ctx context.Context, key store.Key) (*Manifest, error) {
	data, err := v.readBlob(ctx, key, v.maxManifest)
	if errors.Is(err, codec.ErrTooLarge) {
		return nil, fmt.Errorf("%w: manifest %s exceeds the %d byte limit", ErrManifestTooLarge, key, v.maxManifest)
	}
	if err != nil {
		return nil, fmt.Errorf("vault: manifest %s: %w", key, err)
	}
	return decodeManifest(data)
}

// readChunk loads one chunk and verifies its size and hash.
func (v *Vault) readChunk(ctx context.Context, c ChunkRef) ([]byte, error) {
	key, err := store.ParseKey(c.Hash)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	data, err := v.readBlob(ctx, key, int64(c.Size))
	if err != nil {
		return nil, err
	}
	if len(data) != c.Size {
		return nil, fmt.Errorf("%w: chunk %s fails verification", ErrCorrupt, key)
	}
	return data, nil
}
