package index

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

var epoch = time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

func newIndex(t *testing.T) (*Index, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	x, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x, path
}

func ck(n byte, stored, raw int64) Chunk {
	return Chunk{Key: store.KeyOf([]byte{n}), StoredSize: stored, RawSize: raw}
}

func mk(s string) store.Key { return store.KeyOf([]byte(s)) }

func mustAdd(t *testing.T, x *Index, name string, chunks ...Chunk) {
	t.Helper()
	if err := x.AddFile(name, mk("manifest-"+name), epoch, chunks); err != nil {
		t.Fatalf("AddFile(%q): %v", name, err)
	}
}

func refs(t *testing.T, x *Index, c Chunk) uint64 {
	t.Helper()
	e, err := x.GetChunk(c.Key)
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	return e.Refs
}

func TestOpenEmpty(t *testing.T) {
	x, _ := newIndex(t)
	s, err := x.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s != (Stats{}) {
		t.Fatalf("empty index stats = %+v", s)
	}
	ok, err := x.HasChunk(mk("x"))
	if err != nil || ok {
		t.Fatalf("HasChunk = %v, %v", ok, err)
	}
}

func TestOpenErrors(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing-dir", "index.db"))
	if err == nil || !strings.HasPrefix(err.Error(), "index: open") {
		t.Fatalf("Open in missing dir: %v", err)
	}
}

func TestOpenLocked(t *testing.T) {
	old := openTimeout
	openTimeout = 50 * time.Millisecond
	t.Cleanup(func() { openTimeout = old })
	x, path := newIndex(t)
	if _, err := Open(path); err == nil {
		t.Fatal("second Open of a locked index succeeded")
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	y, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = y.Close()
}

func TestCloseTwiceAndUseAfterClose(t *testing.T) {
	x, _ := newIndex(t)
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := x.HasChunk(mk("x")); err == nil || !strings.HasPrefix(err.Error(), "index:") {
		t.Fatalf("HasChunk after Close: %v", err)
	}
	if err := x.AddFile("a", mk("m"), epoch, nil); err == nil || !strings.HasPrefix(err.Error(), "index:") {
		t.Fatalf("AddFile after Close: %v", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	x, path := newIndex(t)
	a, b := ck(1, 10, 20), ck(2, 30, 40)
	mustAdd(t, x, "f", a, b, a)
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	y, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer y.Close()
	f, err := y.GetFile("f")
	if err != nil {
		t.Fatal(err)
	}
	want := File{Name: "f", ManifestKey: mk("manifest-f"), Size: 80, ChunkCount: 3, Created: epoch}
	if f != want {
		t.Fatalf("GetFile = %+v, want %+v", f, want)
	}
	if got := refs(t, y, a); got != 2 {
		t.Fatalf("refs(a) = %d, want 2", got)
	}
}

func TestAddFileCountsOccurrences(t *testing.T) {
	x, _ := newIndex(t)
	a, b := ck(1, 10, 20), ck(2, 30, 40)
	mustAdd(t, x, "f", a, a, b)
	mustAdd(t, x, "g", a)
	if got := refs(t, x, a); got != 3 {
		t.Fatalf("refs(a) = %d, want 3", got)
	}
	if got := refs(t, x, b); got != 1 {
		t.Fatalf("refs(b) = %d, want 1", got)
	}
	e, err := x.GetChunk(a.Key)
	if err != nil {
		t.Fatal(err)
	}
	if e.StoredSize != 10 || e.RawSize != 20 {
		t.Fatalf("entry = %+v", e)
	}
	s, err := x.Stats()
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Files: 2, LogicalBytes: 80 + 20, UniqueChunks: 2, StoredBytes: 40}
	if s != want {
		t.Fatalf("Stats = %+v, want %+v", s, want)
	}
}

func TestAddFileOverwriteReleasesOld(t *testing.T) {
	x, _ := newIndex(t)
	a, b, c := ck(1, 10, 20), ck(2, 30, 40), ck(3, 50, 60)
	mustAdd(t, x, "f", a, b)
	mustAdd(t, x, "f", b, c) // a is released, b keeps one ref, c is new
	if got := refs(t, x, a); got != 0 {
		t.Fatalf("refs(a) = %d, want 0", got)
	}
	if got := refs(t, x, b); got != 1 {
		t.Fatalf("refs(b) = %d, want 1", got)
	}
	if got := refs(t, x, c); got != 1 {
		t.Fatalf("refs(c) = %d, want 1", got)
	}
	s, _ := x.Stats()
	want := Stats{Files: 1, LogicalBytes: 100, UniqueChunks: 3, StoredBytes: 90, ZeroRefChunks: 1, ZeroRefBytes: 10}
	if s != want {
		t.Fatalf("Stats = %+v, want %+v", s, want)
	}
	// Overwriting with an identical chunk list leaves counts unchanged.
	mustAdd(t, x, "f", b, c)
	if refs(t, x, b) != 1 || refs(t, x, c) != 1 {
		t.Fatal("identical overwrite changed refcounts")
	}
}

func TestAddFileEmpty(t *testing.T) {
	x, _ := newIndex(t)
	mustAdd(t, x, "empty")
	f, err := x.GetFile("empty")
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != 0 || f.ChunkCount != 0 {
		t.Fatalf("file = %+v", f)
	}
	if err := x.RemoveFile("empty"); err != nil {
		t.Fatal(err)
	}
}

func TestAddFileValidation(t *testing.T) {
	x, _ := newIndex(t)
	good := ck(1, 1, 1)
	tests := []struct {
		name   string
		file   string
		when   time.Time
		chunks []Chunk
		want   error
	}{
		{"empty name", "", epoch, nil, ErrInvalidName},
		{"long name", strings.Repeat("x", bbolt.MaxKeySize+1), epoch, nil, ErrInvalidName},
		{"negative stored", "f", epoch, []Chunk{good, ck(2, -1, 1)}, ErrInvalidChunk},
		{"negative raw", "f", epoch, []Chunk{ck(2, 1, -1)}, ErrInvalidChunk},
		{"size overflow", "f", epoch, []Chunk{ck(2, 1, math.MaxInt64), ck(3, 1, 1)}, ErrInvalidChunk},
		{"zero time", "f", time.Time{}, nil, ErrInvalidTime},
		{"far future", "f", time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), nil, ErrInvalidTime},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := x.AddFile(tc.file, mk("m"), tc.when, tc.chunks)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if s, _ := x.Stats(); s != (Stats{}) {
		t.Fatalf("failed AddFile changed the index: %+v", s)
	}
	// The maximum-length name is accepted.
	if err := x.AddFile(strings.Repeat("x", bbolt.MaxKeySize), mk("m"), epoch, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAddFileMismatchRollsBack(t *testing.T) {
	x, _ := newIndex(t)
	a, b := ck(1, 10, 20), ck(2, 30, 40)
	mustAdd(t, x, "f", a)
	before, _ := x.Stats()

	bad := Chunk{Key: a.Key, StoredSize: 10, RawSize: 21}
	err := x.AddFile("f", mk("m2"), epoch, []Chunk{b, bad})
	if !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("err = %v, want ErrChunkMismatch", err)
	}
	after, _ := x.Stats()
	if before != after {
		t.Fatalf("stats changed: %+v -> %+v", before, after)
	}
	if ok, _ := x.HasChunk(b.Key); ok {
		t.Fatal("chunk from a failed AddFile was indexed")
	}
	if refs(t, x, a) != 1 {
		t.Fatal("refcount changed by a failed AddFile")
	}
	f, _ := x.GetFile("f")
	if f.ManifestKey != mk("manifest-f") {
		t.Fatal("file record changed by a failed AddFile")
	}
}

func TestExistingChunkKeepsStoredSize(t *testing.T) {
	x, _ := newIndex(t)
	a := ck(1, 10, 20)
	mustAdd(t, x, "f", a)
	mustAdd(t, x, "g", Chunk{Key: a.Key, StoredSize: 99, RawSize: 20})
	e, _ := x.GetChunk(a.Key)
	if e.StoredSize != 10 || e.Refs != 2 {
		t.Fatalf("entry = %+v", e)
	}
}

func TestRemoveFile(t *testing.T) {
	x, _ := newIndex(t)
	a, b := ck(1, 10, 20), ck(2, 30, 40)
	mustAdd(t, x, "f", a, a, b)
	mustAdd(t, x, "g", a)
	if err := x.RemoveFile("f"); err != nil {
		t.Fatal(err)
	}
	if refs(t, x, a) != 1 || refs(t, x, b) != 0 {
		t.Fatal("wrong refcounts after RemoveFile")
	}
	if _, err := x.GetFile("f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFile after remove: %v", err)
	}
	s, _ := x.Stats()
	want := Stats{Files: 1, LogicalBytes: 20, UniqueChunks: 2, StoredBytes: 40, ZeroRefChunks: 1, ZeroRefBytes: 30}
	if s != want {
		t.Fatalf("Stats = %+v, want %+v", s, want)
	}
	err := x.RemoveFile("f")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("second RemoveFile: %v", err)
	}
}

func TestNotFound(t *testing.T) {
	x, _ := newIndex(t)
	if _, err := x.GetFile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFile: %v", err)
	}
	if _, err := x.GetChunk(mk("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetChunk: %v", err)
	}
}

func TestForgetChunk(t *testing.T) {
	x, _ := newIndex(t)
	a := ck(1, 10, 20)
	mustAdd(t, x, "f", a)
	if err := x.ForgetChunk(a.Key); !errors.Is(err, ErrChunkInUse) {
		t.Fatalf("ForgetChunk in use: %v", err)
	}
	if err := x.RemoveFile("f"); err != nil {
		t.Fatal(err)
	}
	if err := x.ForgetChunk(a.Key); err != nil {
		t.Fatal(err)
	}
	if ok, _ := x.HasChunk(a.Key); ok {
		t.Fatal("chunk still indexed after ForgetChunk")
	}
	if err := x.ForgetChunk(a.Key); err != nil {
		t.Fatalf("ForgetChunk of unindexed chunk: %v", err)
	}
	if s, _ := x.Stats(); s != (Stats{}) {
		t.Fatalf("Stats = %+v", s)
	}
}

func TestIteration(t *testing.T) {
	x, _ := newIndex(t)
	mustAdd(t, x, "b", ck(1, 1, 2))
	mustAdd(t, x, "a", ck(2, 3, 4), ck(1, 1, 2))
	mustAdd(t, x, "c")

	var names []string
	err := x.ForEachFile(func(f File) error {
		names = append(names, f.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v", names)
	}

	var n int
	var prev store.Key
	err = x.ForEachChunk(func(e ChunkEntry) error {
		if n > 0 && string(prev[:]) >= string(e.Key[:]) {
			t.Fatal("chunks not in ascending key order")
		}
		prev = e.Key
		n++
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("ForEachChunk: n=%d err=%v", n, err)
	}

	stop := errors.New("stop")
	var calls int
	if err := x.ForEachFile(func(File) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("ForEachFile stop: err=%v calls=%d", err, calls)
	}
	calls = 0
	if err := x.ForEachChunk(func(ChunkEntry) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("ForEachChunk stop: err=%v calls=%d", err, calls)
	}
}

// corrupt overwrites raw records behind the index's back.
func corrupt(t *testing.T, x *Index, fn func(cb, fb *bbolt.Bucket)) {
	t.Helper()
	err := x.db.Update(func(tx *bbolt.Tx) error {
		fn(tx.Bucket(chunksBucket), tx.Bucket(filesBucket))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCorruptRecords(t *testing.T) {
	a := ck(1, 10, 20)
	fileRec := func() []byte {
		return encodeFile(mk("m"), 20, epoch, []Chunk{a})
	}
	huge := func(off int) []byte {
		b := encodeChunk(ChunkEntry{Chunk: a, Refs: 1})
		for i := 0; i < 8; i++ {
			b[off+i] = 0xff
		}
		return b
	}
	tests := []struct {
		name  string
		setup func(cb, fb *bbolt.Bucket)
	}{
		{"short chunk record", func(cb, _ *bbolt.Bucket) { _ = cb.Put(a.Key[:], []byte{1, 2, 3}) }},
		{"huge stored size", func(cb, _ *bbolt.Bucket) { _ = cb.Put(a.Key[:], huge(0)) }},
		{"huge raw size", func(cb, _ *bbolt.Bucket) { _ = cb.Put(a.Key[:], huge(8)) }},
		{"short chunk key", func(cb, _ *bbolt.Bucket) { _ = cb.Put([]byte("short"), encodeChunk(ChunkEntry{})) }},
		{"short file record", func(_, fb *bbolt.Bucket) { _ = fb.Put([]byte("f"), []byte{1}) }},
		{"file count mismatch", func(_, fb *bbolt.Bucket) {
			r := fileRec()
			r[store.KeySize+15]++
			_ = fb.Put([]byte("f"), r)
		}},
		{"file ragged keys", func(_, fb *bbolt.Bucket) { _ = fb.Put([]byte("f"), append(fileRec(), 0)) }},
		{"file huge size", func(_, fb *bbolt.Bucket) {
			r := fileRec()
			for i := 0; i < 8; i++ {
				r[store.KeySize+i] = 0xff
			}
			_ = fb.Put([]byte("f"), r)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, _ := newIndex(t)
			corrupt(t, x, tc.setup)
			if _, err := x.Stats(); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Stats: %v", err)
			}
			if err := x.ForEachChunk(func(ChunkEntry) error { return nil }); err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ForEachChunk: %v", err)
			}
			if err := x.ForEachFile(func(File) error { return nil }); err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ForEachFile: %v", err)
			}
		})
	}

	t.Run("get corrupt", func(t *testing.T) {
		x, _ := newIndex(t)
		corrupt(t, x, func(cb, fb *bbolt.Bucket) {
			_ = cb.Put(a.Key[:], []byte{1})
			_ = fb.Put([]byte("f"), []byte{1})
		})
		if _, err := x.GetChunk(a.Key); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("GetChunk: %v", err)
		}
		if _, err := x.GetFile("f"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("GetFile: %v", err)
		}
		if err := x.ForgetChunk(a.Key); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("ForgetChunk: %v", err)
		}
		if err := x.RemoveFile("f"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("RemoveFile: %v", err)
		}
		if err := x.AddFile("g", mk("m"), epoch, []Chunk{a}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("AddFile onto corrupt chunk: %v", err)
		}
		if err := x.AddFile("f", mk("m"), epoch, nil); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("AddFile over corrupt file: %v", err)
		}
	})

	t.Run("file references unindexed chunk", func(t *testing.T) {
		x, _ := newIndex(t)
		corrupt(t, x, func(_, fb *bbolt.Bucket) { _ = fb.Put([]byte("f"), fileRec()) })
		if err := x.RemoveFile("f"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("RemoveFile: %v", err)
		}
		// The failed removal rolled back, so the file is still there.
		if _, err := x.GetFile("f"); err != nil {
			t.Fatalf("GetFile: %v", err)
		}
	})

	t.Run("refcount underflow", func(t *testing.T) {
		x, _ := newIndex(t)
		corrupt(t, x, func(cb, fb *bbolt.Bucket) {
			_ = cb.Put(a.Key[:], encodeChunk(ChunkEntry{Chunk: a}))
			_ = fb.Put([]byte("f"), fileRec())
		})
		if err := x.RemoveFile("f"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("RemoveFile: %v", err)
		}
	})

	t.Run("missing buckets", func(t *testing.T) {
		x, _ := newIndex(t)
		err := x.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(filesBucket) })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x.HasChunk(a.Key); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("HasChunk: %v", err)
		}
		if err := x.AddFile("f", mk("m"), epoch, nil); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("AddFile: %v", err)
		}
	})
}

func TestOpenGarbageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a bolt database", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.HasPrefix(err.Error(), "index: open") {
		t.Fatalf("Open of garbage file: %v", err)
	}
}

func TestCreatedRoundTrip(t *testing.T) {
	x, _ := newIndex(t)
	// A non-UTC time with a monotonic reading comes back as the same instant in UTC.
	loc := time.FixedZone("x", 3600)
	now := time.Now().In(loc)
	if err := x.AddFile("f", mk("m"), now, nil); err != nil {
		t.Fatal(err)
	}
	f, _ := x.GetFile("f")
	if !f.Created.Equal(now) || f.Created.Location() != time.UTC {
		t.Fatalf("Created = %v, want %v in UTC", f.Created, now)
	}
}
