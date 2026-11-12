package vault

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/chunker"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/codec"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

// eachMode runs fn for an unencrypted and an encrypted repository. fn gets the
// repository directory and the vault opened on it.
func eachMode(t *testing.T, fn func(t *testing.T, v *Vault, dir string)) {
	t.Helper()
	t.Run("plain", func(t *testing.T) {
		v, dir := newVault(t)
		fn(t, v, dir)
	})
	t.Run("encrypted", func(t *testing.T) {
		v, dir := initEncrypted(t)
		fn(t, v, dir)
	})
}

// reopenOpts opens dir like newVault and initEncrypted do; the passphrase
// source is ignored by an unencrypted repository.
func reopenOpts() []Option { return []Option{WithChunkerConfig(testCfg), passFn(testPass)} }

func mustOpen(t *testing.T, dir string) *Vault {
	t.Helper()
	v, err := Open(dir, reopenOpts()...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func mustReindex(t *testing.T, dir string) ReindexStats {
	t.Helper()
	st, err := Reindex(context.Background(), dir, reopenOpts()...)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	return st
}

// snapshot is the contents of an index. Chunks without references are left
// out: an incrementally maintained index keeps them until garbage collection
// forgets them, while a rebuilt one never learns of them.
type snapshot struct {
	Files  []index.File
	Chunks []index.ChunkEntry
}

func snap(t *testing.T, v *Vault) snapshot {
	t.Helper()
	var s snapshot
	if err := v.idx.ForEachFile(func(f index.File) error { s.Files = append(s.Files, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := v.idx.ForEachChunk(func(c index.ChunkEntry) error {
		if c.Refs > 0 {
			s.Chunks = append(s.Chunks, c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// zeroRefChunks counts the indexed chunks that nothing references.
func zeroRefChunks(t *testing.T, v *Vault) int {
	t.Helper()
	st, err := v.idx.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return int(st.ZeroRefChunks)
}

// wantRefs recomputes the reference count of every chunk from the manifests,
// the source of truth, and checks the index against it, including that the
// stored size is the size of the blob on disk.
func wantRefs(t *testing.T, v *Vault) {
	t.Helper()
	ctx := context.Background()
	want := map[store.Key]uint64{}
	entries, err := v.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		m, err := v.manifest(ctx, e.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range m.Chunks {
			k, err := store.ParseKey(c.Hash)
			if err != nil {
				t.Fatal(err)
			}
			want[k]++
		}
		f, err := v.idx.GetFile(e.Name)
		if err != nil {
			t.Fatalf("index lacks file %q: %v", e.Name, err)
		}
		if f.ManifestKey != e.ManifestKey || f.Size != e.Size || f.ChunkCount != e.Chunks || !f.Created.Equal(e.Created) {
			t.Errorf("index entry for %q = %+v, vault says %+v", e.Name, f, e)
		}
	}
	got := map[store.Key]uint64{}
	if err := v.idx.ForEachChunk(func(c index.ChunkEntry) error {
		if c.Refs > 0 {
			got[c.Key] = c.Refs
		}
		size, err := v.blobs.Size(ctx, c.Key)
		if err != nil {
			return err
		}
		if size != c.StoredSize {
			t.Errorf("chunk %s: indexed stored size %d, blob is %d bytes", c.Key, c.StoredSize, size)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("index refcounts = %v, manifests say %v", got, want)
	}
	if st, err := v.idx.Stats(); err != nil || int(st.Files) != len(entries) {
		t.Fatalf("index has %d files (err %v), vault has %d", st.Files, err, len(entries))
	}
}

// saveIndex returns the contents of index.db. The vault must be closed.
func saveIndex(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, indexFile))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func restoreIndex(t *testing.T, dir string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, indexFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// crash runs op on a freshly opened vault and then rolls index.db back to what
// it was before op. The blobs, manifest and ref op wrote stay and its index
// update is lost, which is the state a crash between the ref update and the
// index update leaves, see the notes on Vault. No vault may be open.
func crash(t *testing.T, dir string, op func(v *Vault)) {
	t.Helper()
	before := saveIndex(t, dir)
	v, err := Open(dir, reopenOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	op(v)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	restoreIndex(t, dir, before)
}

func TestInitCreatesIndex(t *testing.T) {
	_, dir := newVault(t)
	if _, err := os.Stat(filepath.Join(dir, indexFile)); err != nil {
		t.Fatalf("Init did not create %s: %v", indexFile, err)
	}
}

func TestPutMaintainsIndex(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		a := randomBytes(60, 60_000)
		mustPut(t, v, "a", a)
		wantRefs(t, v)

		// Same content under another name: every chunk is shared, so every
		// reference count is 2.
		mustPut(t, v, "b", a)
		wantRefs(t, v)
		err := v.idx.ForEachChunk(func(c index.ChunkEntry) error {
			if c.Refs != 2 {
				t.Errorf("chunk %s has %d refs, want 2", c.Key, c.Refs)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// A file with repeated chunks counts each occurrence.
		mustPut(t, v, "rep", bytes.Repeat(randomBytes(61, 700), 40))
		wantRefs(t, v)

		// The empty file has no chunks.
		mustPut(t, v, "empty", nil)
		wantRefs(t, v)
		if f, err := v.idx.GetFile("empty"); err != nil || f.ChunkCount != 0 || f.Size != 0 {
			t.Fatalf("empty file = %+v, %v", f, err)
		}
	})
}

func TestOverwriteReleasesOldChunks(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		v1 := randomBytes(62, 80_000)
		v2 := append(append([]byte(nil), v1[:40_000]...), randomBytes(63, 40_000)...)
		mustPut(t, v, "f", v1)
		wantRefs(t, v)
		mustPut(t, v, "f", v2)
		wantRefs(t, v)
		if got := mustGet(t, v, "f"); !bytes.Equal(got, v2) {
			t.Fatal("Get after overwrite returned the old content")
		}
		// The tail of v1 is no longer referenced but its blobs and index
		// entries stay until garbage collection.
		if zeroRefChunks(t, v) == 0 {
			t.Fatal("overwrite left no unreferenced chunk, the tail of the old version should be one")
		}
		// Overwriting with identical content leaves the reference counts alone.
		before := snap(t, v)
		mustPut(t, v, "f", v2)
		if after := snap(t, v); !reflect.DeepEqual(before.Chunks, after.Chunks) {
			t.Fatal("re-putting identical content changed the reference counts")
		}
	})
}

func TestRemove(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		shared := randomBytes(64, 50_000)
		mustPut(t, v, "a", shared)
		mustPut(t, v, "b", shared)
		mustPut(t, v, "c", randomBytes(65, 30_000))
		blobs := countBlobs(t, dir)

		if err := v.Remove("a"); err != nil {
			t.Fatal(err)
		}
		wantRefs(t, v)
		if err := v.Get(context.Background(), "a", io.Discard); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get after Remove = %v, want ErrNotFound", err)
		}
		if got := mustGet(t, v, "b"); !bytes.Equal(got, shared) {
			t.Fatal("removing a file broke one that shares its chunks")
		}
		if zeroRefChunks(t, v) != 0 {
			t.Fatal("chunks of a are still shared with b and must keep a reference")
		}

		if err := v.Remove("c"); err != nil {
			t.Fatal(err)
		}
		wantRefs(t, v)
		if zeroRefChunks(t, v) == 0 {
			t.Fatal("removing c left its chunks referenced")
		}
		if got := countBlobs(t, dir); got != blobs {
			t.Fatalf("Remove deleted blobs: %d -> %d, garbage collection is not implemented", blobs, got)
		}
		names, err := v.List(context.Background())
		if err != nil || len(names) != 1 || names[0].Name != "b" {
			t.Fatalf("List = %+v, %v", names, err)
		}

		if err := v.Remove("a"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second Remove = %v, want ErrNotFound", err)
		}
		if err := v.Remove(""); !errors.Is(err, store.ErrInvalidName) {
			t.Fatalf("Remove of an invalid name = %v", err)
		}
	})
}

func TestReindexMatchesIncrementalIndex(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		shared := randomBytes(66, 60_000)
		mustPut(t, v, "a", shared)
		mustPut(t, v, "b", shared)
		mustPut(t, v, "dir/c", randomBytes(67, 40_000))
		mustPut(t, v, "rep", bytes.Repeat(randomBytes(68, 700), 30))
		mustPut(t, v, "empty", nil)
		mustPut(t, v, "a", randomBytes(69, 20_000)) // overwrite
		if err := v.Remove("dir/c"); err != nil {
			t.Fatal(err)
		}
		want := snap(t, v)
		if len(want.Files) != 4 || len(want.Chunks) == 0 {
			t.Fatalf("setup produced %d files, %d referenced chunks", len(want.Files), len(want.Chunks))
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}

		st := mustReindex(t, dir)
		v2 := mustOpen(t, dir)
		got := snap(t, v2)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("reindexed index differs from the incremental one\n got: %+v\nwant: %+v", got, want)
		}
		if st.Files != int64(len(want.Files)) || st.Chunks != int64(len(want.Chunks)) {
			t.Fatalf("stats = %+v, want %d files, %d chunks", st, len(want.Files), len(want.Chunks))
		}
		wantRefs(t, v2)
		// The rebuilt index forgot the unreferenced chunks of the overwritten and
		// removed files: it only describes what the refs reach.
		if n := zeroRefChunks(t, v2); n != 0 {
			t.Fatalf("rebuilt index has %d unreferenced chunks", n)
		}
	})
}

func TestReindexRepairsCrashAfterPut(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(70, 30_000))
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		lost := randomBytes(71, 40_000)
		crash(t, dir, func(v *Vault) { mustPut(t, v, "lost", lost) })

		// The blobs, the manifest and the ref survived, the index update did not.
		v2 := mustOpen(t, dir)
		if _, err := v2.idx.GetFile("lost"); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("index knows the file the crash lost: %v", err)
		}
		// The repository is readable.
		if got := mustGet(t, v2, "lost"); !bytes.Equal(got, lost) {
			t.Fatal("file written before the crash is not readable")
		}
		if entries, err := v2.List(context.Background()); err != nil || len(entries) != 2 {
			t.Fatalf("List = %+v, %v", entries, err)
		}
		// A Put works too, and dedups against the blobs the index never heard of.
		st := mustPut(t, v2, "copy", lost)
		if st.NewChunks != 0 {
			t.Fatalf("Put after the crash stored %d chunks again, the blob store has them", st.NewChunks)
		}
		if err := v2.Close(); err != nil {
			t.Fatal(err)
		}

		// Reindex repairs the index.
		rs := mustReindex(t, dir)
		v3 := mustOpen(t, dir)
		wantRefs(t, v3)
		if f, err := v3.idx.GetFile("lost"); err != nil || f.Size != int64(len(lost)) {
			t.Fatalf("GetFile after Reindex = %+v, %v", f, err)
		}
		if rs.Files != 3 {
			t.Fatalf("Reindex stats = %+v, want 3 files", rs)
		}
	})
}

func TestReindexRepairsCrashAfterRemove(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		shared := randomBytes(72, 30_000)
		mustPut(t, v, "a", shared)
		mustPut(t, v, "b", shared)
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		crash(t, dir, func(v *Vault) {
			if err := v.Remove("a"); err != nil {
				t.Fatal(err)
			}
		})

		v2 := mustOpen(t, dir)
		// The index still lists the removed file but the repository does not.
		if _, err := v2.idx.GetFile("a"); err != nil {
			t.Fatalf("index forgot a, the crash should have kept it: %v", err)
		}
		if err := v2.Get(context.Background(), "a", io.Discard); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get of the removed file = %v, want ErrNotFound", err)
		}
		if got := mustGet(t, v2, "b"); !bytes.Equal(got, shared) {
			t.Fatal("b is not readable")
		}
		// Putting the name again after the crash releases the stale references
		// held in the index, so counts stay right.
		other := randomBytes(73, 20_000)
		mustPut(t, v2, "a", other)
		wantRefs(t, v2)
		if err := v2.Remove("a"); err != nil {
			t.Fatal(err)
		}
		if err := v2.Close(); err != nil {
			t.Fatal(err)
		}

		crash(t, dir, func(v *Vault) {
			if err := v.Remove("b"); err != nil {
				t.Fatal(err)
			}
		})
		mustReindex(t, dir)
		v3 := mustOpen(t, dir)
		wantRefs(t, v3)
		if _, err := v3.idx.GetFile("b"); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("Reindex kept the removed file: %v", err)
		}
	})
}

func TestOpenWithoutIndexTellsUserToReindex(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		data := randomBytes(74, 30_000)
		mustPut(t, v, "f", data)
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		// A repository created before the index existed has no index.db.
		if err := os.Remove(filepath.Join(dir, indexFile)); err != nil {
			t.Fatal(err)
		}
		_, err := Open(dir, reopenOpts()...)
		if !errors.Is(err, ErrIndexMissing) || !strings.Contains(err.Error(), "chunkvault reindex") {
			t.Fatalf("Open = %v, want ErrIndexMissing telling the user to run reindex", err)
		}
		if _, err := os.Stat(filepath.Join(dir, indexFile)); err == nil {
			t.Fatal("a failed Open created index.db")
		}

		st := mustReindex(t, dir)
		if st.Files != 1 {
			t.Fatalf("Reindex stats = %+v", st)
		}
		v2 := mustOpen(t, dir)
		wantRefs(t, v2)
		if got := mustGet(t, v2, "f"); !bytes.Equal(got, data) {
			t.Fatal("data differs after reindex")
		}
	})
}

func TestOpenWithDamagedIndex(t *testing.T) {
	v, dir := newVault(t)
	mustPut(t, v, "f", randomBytes(75, 10_000))
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexFile), bytes.Repeat([]byte{0xAB}, 16<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, reopenOpts()...)
	if err == nil || !strings.Contains(err.Error(), "chunkvault reindex") {
		t.Fatalf("Open = %v, want an error pointing at reindex", err)
	}
	// Reindex does not read the old index, so it repairs this too.
	mustReindex(t, dir)
	wantRefs(t, mustOpen(t, dir))
}

func TestReindexLeavesNoTempFileAndKeepsIndexOnFailure(t *testing.T) {
	v, dir := newVault(t)
	st := mustPut(t, v, "f", randomBytes(76, 30_000))
	mustPut(t, v, "g", randomBytes(77, 30_000))
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	before := saveIndex(t, dir)

	// Make the manifest of f unreadable.
	k := st.ManifestKey.String()
	if err := os.Remove(filepath.Join(dir, "objects", k[:2], k[2:4], k)); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(context.Background(), dir, reopenOpts()...); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Reindex = %v, want ErrNotFound", err)
	}
	if !bytes.Equal(saveIndex(t, dir), before) {
		t.Fatal("a failed Reindex changed index.db")
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, ".index-*")); len(tmps) != 0 {
		t.Fatalf("temporary index left behind: %v", tmps)
	}
}

func TestReindexErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Reindex(ctx, filepath.Join(t.TempDir(), "none")); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Reindex of a non-repository = %v", err)
	}

	v, dir := newVault(t)
	st := mustPut(t, v, "f", randomBytes(78, 30_000))
	m, err := v.loadManifest(ctx, st.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Reindex(cancelled, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reindex with a cancelled context = %v", err)
	}
	// A missing chunk is reported, not skipped.
	h := m.Chunks[1].Hash
	if err := os.Remove(filepath.Join(dir, "objects", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(ctx, dir); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Reindex with a missing chunk = %v, want ErrNotFound", err)
	}
	// So is a corrupt manifest.
	corruptBlob(t, dir, st.ManifestKey.String())
	if _, err := Reindex(ctx, dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Reindex with a corrupt manifest = %v, want ErrCorrupt", err)
	}
}

func TestReindexEncryptedNeedsPassphrase(t *testing.T) {
	_, dir := initEncrypted(t)
	if _, err := Reindex(context.Background(), dir); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("Reindex without a passphrase = %v", err)
	}
	if _, err := Reindex(context.Background(), dir, passFn("wrong")); err == nil {
		t.Fatal("Reindex with the wrong passphrase succeeded")
	}
}

func TestPutFallsBackToBlobStoreWhenIndexLacksChunks(t *testing.T) {
	v, dir := newVault(t)
	data := randomBytes(79, 40_000)
	mustPut(t, v, "a", data)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// An empty index knows no chunk, yet the blobs are all there.
	if err := os.Remove(filepath.Join(dir, indexFile)); err != nil {
		t.Fatal(err)
	}
	x, err := index.Open(filepath.Join(dir, indexFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	v2 := mustOpen(t, dir)
	before := countBlobs(t, dir)
	st := mustPut(t, v2, "b", data)
	if st.NewChunks != 0 || st.DedupBytes != int64(len(data)) {
		t.Fatalf("Put with an empty index stats = %+v, want everything deduplicated", st)
	}
	if got := countBlobs(t, dir); got != before+1 {
		t.Fatalf("blobs %d -> %d, want only the manifest added", before, got)
	}
	// The index learned the chunks, with the right stored sizes.
	if f, err := v2.idx.GetFile("b"); err != nil || f.Size != int64(len(data)) {
		t.Fatalf("GetFile = %+v, %v", f, err)
	}
	// The index learned the chunks of b, one reference each (a is not indexed),
	// with the stored sizes of the blobs on disk.
	n := 0
	err = v2.idx.ForEachChunk(func(c index.ChunkEntry) error {
		n++
		if c.Refs != 1 {
			t.Errorf("chunk %s has %d refs, want 1", c.Key, c.Refs)
		}
		if size, err := v2.blobs.Size(context.Background(), c.Key); err != nil || size != c.StoredSize {
			t.Errorf("chunk %s indexed with stored size %d, blob has %d (%v)", c.Key, c.StoredSize, size, err)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("indexed %d chunks (err %v)", n, err)
	}
}

func TestNewRequiresIndex(t *testing.T) {
	dir := t.TempDir()
	blobs, err := store.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.NewRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := codec.New(codec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(blobs, refs, c); err == nil {
		t.Fatal("New accepted a vault without an index")
	}
	x, err := index.Open(filepath.Join(dir, indexFile))
	if err != nil {
		t.Fatal(err)
	}
	v, err := New(blobs, refs, c, WithIndex(x), WithChunkerConfig(chunker.Config{MinSize: 256, AvgSize: 1024, MaxSize: 4096}))
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, v, "f", []byte("data"))
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}
