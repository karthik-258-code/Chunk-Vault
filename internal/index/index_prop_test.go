package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pgregory.net/rapid"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

// model is an in-memory reference implementation of the index. It keeps only
// the files and the set of known chunks; reference counts, sizes and stats are
// recomputed from those on demand, never maintained incrementally.
type model struct {
	files map[string]modelFile
	// known holds every chunk that has been indexed and not forgotten.
	known map[store.Key]Chunk
}

type modelFile struct {
	manifest store.Key
	created  time.Time
	chunks   []Chunk
}

func (m *model) refs() map[store.Key]uint64 {
	r := make(map[store.Key]uint64)
	for _, f := range m.files {
		for _, c := range f.chunks {
			r[c.Key]++
		}
	}
	return r
}

func (m *model) stats() Stats {
	var s Stats
	refs := m.refs()
	for _, f := range m.files {
		s.Files++
		for _, c := range f.chunks {
			s.LogicalBytes += c.RawSize
		}
	}
	for k, c := range m.known {
		s.UniqueChunks++
		s.StoredBytes += c.StoredSize
		if refs[k] == 0 {
			s.ZeroRefChunks++
			s.ZeroRefBytes += c.StoredSize
		}
	}
	return s
}

// check compares the whole index against the model.
func (m *model) check(t *rapid.T, x *Index) {
	refs := m.refs()

	got := make(map[store.Key]ChunkEntry)
	if err := x.ForEachChunk(func(e ChunkEntry) error { got[e.Key] = e; return nil }); err != nil {
		t.Fatalf("ForEachChunk: %v", err)
	}
	if len(got) != len(m.known) {
		t.Fatalf("index has %d chunks, model %d", len(got), len(m.known))
	}
	for k, want := range m.known {
		e, ok := got[k]
		if !ok {
			t.Fatalf("chunk %s missing from index", k)
		}
		if e.Chunk != want || e.Refs != refs[k] {
			t.Fatalf("chunk %s = %+v refs %d, model %+v refs %d", k, e.Chunk, e.Refs, want, refs[k])
		}
		if ok, err := x.HasChunk(k); err != nil || !ok {
			t.Fatalf("HasChunk(%s) = %v, %v", k, ok, err)
		}
	}
	// Every referenced chunk must be known.
	for k := range refs {
		if _, ok := m.known[k]; !ok {
			t.Fatalf("model references unknown chunk %s", k)
		}
	}

	files := make(map[string]File)
	if err := x.ForEachFile(func(f File) error { files[f.Name] = f; return nil }); err != nil {
		t.Fatalf("ForEachFile: %v", err)
	}
	if len(files) != len(m.files) {
		t.Fatalf("index has %d files, model %d", len(files), len(m.files))
	}
	for name, mf := range m.files {
		var size int64
		for _, c := range mf.chunks {
			size += c.RawSize
		}
		want := File{Name: name, ManifestKey: mf.manifest, Size: size, ChunkCount: len(mf.chunks), Created: mf.created}
		if files[name] != want {
			t.Fatalf("file %q = %+v, model %+v", name, files[name], want)
		}
	}

	s, err := x.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if want := m.stats(); s != want {
		t.Fatalf("Stats = %+v, model %+v", s, want)
	}
}

// TestPropRefcountsMatchModel applies random sequences of add, overwrite,
// remove, forget and failing adds, and checks after every step that the index
// equals a model recomputed from scratch, including after reopening it.
func TestPropRefcountsMatchModel(t *testing.T) {
	// A small pool of chunks makes overlap between files, repeats within a
	// file and zero-ref chunks common. Sizes are a function of the key, as they
	// are for real content-addressed chunks.
	pool := make([]Chunk, 10)
	for i := range pool {
		pool[i] = Chunk{Key: store.KeyOf([]byte{byte(i)}), StoredSize: int64(1 + 3*i), RawSize: int64(10 + 7*i)}
	}
	names := []string{"a", "b", "c", "dir/d", "é"}

	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "index-prop-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "index.db")
		x, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = x.Close() }()
		x.db.NoSync = true // durability is not under test; keep iterations fast

		m := &model{files: map[string]modelFile{}, known: map[store.Key]Chunk{}}
		clock := epoch

		drawName := rapid.SampledFrom(names)
		drawChunks := rapid.SliceOfN(rapid.SampledFrom(pool), 0, 8)

		t.Repeat(map[string]func(*rapid.T){
			"add": func(t *rapid.T) {
				name := drawName.Draw(t, "name")
				chunks := drawChunks.Draw(t, "chunks")
				manifest := store.KeyOf([]byte(name + clock.String()))
				clock = clock.Add(time.Second)
				if err := x.AddFile(name, manifest, clock, chunks); err != nil {
					t.Fatalf("AddFile: %v", err)
				}
				m.files[name] = modelFile{manifest: manifest, created: clock, chunks: chunks}
				for _, c := range chunks {
					m.known[c.Key] = c
				}
			},
			"remove": func(t *rapid.T) {
				name := drawName.Draw(t, "name")
				err := x.RemoveFile(name)
				if _, ok := m.files[name]; ok {
					if err != nil {
						t.Fatalf("RemoveFile: %v", err)
					}
					delete(m.files, name)
				} else if !errors.Is(err, ErrNotFound) {
					t.Fatalf("RemoveFile of absent file: %v", err)
				}
			},
			"forget": func(t *rapid.T) {
				c := rapid.SampledFrom(pool).Draw(t, "chunk")
				err := x.ForgetChunk(c.Key)
				if m.refs()[c.Key] > 0 {
					if !errors.Is(err, ErrChunkInUse) {
						t.Fatalf("ForgetChunk of a referenced chunk: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("ForgetChunk: %v", err)
				}
				delete(m.known, c.Key)
			},
			"add-mismatch": func(t *rapid.T) {
				// An AddFile that fails half way must change nothing, even
				// though earlier chunks in the list were valid.
				name := drawName.Draw(t, "name")
				chunks := append([]Chunk(nil), drawChunks.Draw(t, "chunks")...)
				victim := rapid.SampledFrom(pool).Draw(t, "victim")
				if _, ok := m.known[victim.Key]; !ok {
					return
				}
				bad := victim
				bad.RawSize++
				chunks = append(chunks, bad)
				err := x.AddFile(name, store.KeyOf([]byte("bad")), clock, chunks)
				if !errors.Is(err, ErrChunkMismatch) {
					t.Fatalf("AddFile with a mismatched chunk: %v", err)
				}
			},
			"reopen": func(t *rapid.T) {
				if err := x.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				y, err := Open(path)
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				y.db.NoSync = true
				*x = *y
			},
			"": func(t *rapid.T) { m.check(t, x) },
		})
	})
}

// TestPropAddThenRemoveAllLeavesZeroRefs checks that removing every file
// leaves exactly the chunks that were ever added, all with no references.
func TestPropAddThenRemoveAllLeavesZeroRefs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "index-prop-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		x, err := Open(filepath.Join(dir, "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = x.Close() }()
		x.db.NoSync = true

		n := rapid.IntRange(0, 6).Draw(t, "files")
		seen := map[store.Key]int64{}
		for i := 0; i < n; i++ {
			cs := rapid.SliceOfN(rapid.IntRange(0, 5), 0, 6).Draw(t, "chunks")
			var chunks []Chunk
			for _, v := range cs {
				c := Chunk{Key: store.KeyOf([]byte{byte(v)}), StoredSize: int64(v + 1), RawSize: int64(2*v + 1)}
				chunks = append(chunks, c)
				seen[c.Key] = c.StoredSize
			}
			if err := x.AddFile(string(rune('a'+i)), store.Key{}, epoch, chunks); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < n; i++ {
			if err := x.RemoveFile(string(rune('a' + i))); err != nil {
				t.Fatal(err)
			}
		}
		var stored int64
		for _, s := range seen {
			stored += s
		}
		want := Stats{UniqueChunks: int64(len(seen)), StoredBytes: stored, ZeroRefChunks: int64(len(seen)), ZeroRefBytes: stored}
		got, err := x.Stats()
		if err != nil || got != want {
			t.Fatalf("Stats = %+v, %v; want %+v", got, err, want)
		}
	})
}
