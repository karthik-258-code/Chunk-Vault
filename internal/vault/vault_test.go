package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/chunker"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

var testCfg = chunker.Config{MinSize: 256, AvgSize: 1024, MaxSize: 4096}

func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func newVault(t *testing.T, opts ...Option) (*Vault, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir, append([]Option{WithChunkerConfig(testCfg)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, dir
}

func mustPut(t *testing.T, v *Vault, name string, data []byte) PutStats {
	t.Helper()
	st, err := v.Put(context.Background(), name, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(%q): %v", name, err)
	}
	return st
}

func mustGet(t *testing.T, v *Vault, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := v.Get(context.Background(), name, &buf); err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	v, _ := newVault(t)
	for name, size := range map[string]int{"empty": 0, "tiny": 17, "multi": 200_000} {
		data := randomBytes(int64(size), size)
		st := mustPut(t, v, name, data)
		if st.TotalBytes != int64(size) {
			t.Errorf("%s: TotalBytes = %d, want %d", name, st.TotalBytes, size)
		}
		if got := mustGet(t, v, name); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip mismatch", name)
		}
	}
}

func TestPutStatsAccounting(t *testing.T) {
	v, _ := newVault(t)
	data := randomBytes(1, 100_000)
	st := mustPut(t, v, "f", data)
	if st.TotalChunks < 10 || st.NewChunks != st.TotalChunks || st.DedupBytes != 0 {
		t.Fatalf("first put stats = %+v", st)
	}
	if st.NewBytes != int64(len(data)) {
		t.Fatalf("NewBytes = %d, want %d", st.NewBytes, len(data))
	}
}

func TestDedupSameFileTwice(t *testing.T) {
	v, dir := newVault(t)
	data := randomBytes(2, 300_000)
	first := mustPut(t, v, "a", data)
	before := countBlobs(t, dir)

	second := mustPut(t, v, "b", data)
	if second.NewChunks != 0 || second.NewBytes != 0 {
		t.Fatalf("second put stored new chunks: %+v", second)
	}
	if second.TotalChunks != first.TotalChunks || second.DedupBytes != int64(len(data)) {
		t.Fatalf("second put stats = %+v, first = %+v", second, first)
	}
	// "b" differs from "a" only by name, so it adds just its own manifest.
	if after := countBlobs(t, dir); after != before+1 {
		t.Fatalf("blob count %d -> %d, want exactly one new (manifest) blob", before, after)
	}
}

func TestDedupSameNameSameContentIsIdempotent(t *testing.T) {
	v, dir := newVault(t, WithClock(func() time.Time { return time.Unix(1700000000, 0) }))
	data := randomBytes(3, 50_000)
	mustPut(t, v, "a", data)
	before := countBlobs(t, dir)
	mustPut(t, v, "a", data)
	if after := countBlobs(t, dir); after != before {
		t.Fatalf("re-putting identical file changed blob count %d -> %d", before, after)
	}
}

func TestDedupModifiedFileReusesMostChunks(t *testing.T) {
	v, _ := newVault(t)
	data := randomBytes(4, 500_000)
	first := mustPut(t, v, "v1", data)

	modified := append([]byte(nil), data[:250_000]...)
	modified = append(modified, []byte("a small insertion in the middle")...)
	modified = append(modified, data[250_000:]...)
	second := mustPut(t, v, "v2", modified)

	if second.NewChunks > 6 {
		t.Fatalf("modified file stored %d new chunks of %d", second.NewChunks, second.TotalChunks)
	}
	if second.DedupBytes < int64(len(data))*9/10 {
		t.Fatalf("only %d of %d bytes deduplicated", second.DedupBytes, len(data))
	}
	if first.TotalChunks == 0 || second.NewBytes+second.DedupBytes != int64(len(modified)) {
		t.Fatalf("inconsistent accounting: %+v", second)
	}
	if got := mustGet(t, v, "v1"); !bytes.Equal(got, data) {
		t.Fatal("v1 corrupted")
	}
	if got := mustGet(t, v, "v2"); !bytes.Equal(got, modified) {
		t.Fatal("v2 corrupted")
	}
}

func TestOverwriteName(t *testing.T) {
	v, _ := newVault(t)
	mustPut(t, v, "f", []byte("old"))
	mustPut(t, v, "f", []byte("new content"))
	if got := mustGet(t, v, "f"); string(got) != "new content" {
		t.Fatalf("got %q", got)
	}
}

func TestManifestContents(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 678, time.FixedZone("x", 3600))
	v, _ := newVault(t, WithClock(func() time.Time { return when }))
	data := randomBytes(5, 20_000)
	st := mustPut(t, v, "doc.bin", data)

	m, err := v.loadManifest(context.Background(), st.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "doc.bin" || m.Size != int64(len(data)) || m.Version != ManifestVersion {
		t.Fatalf("manifest = %+v", m)
	}
	if !m.Created.Equal(when.UTC().Truncate(time.Second)) {
		t.Fatalf("Created = %v", m.Created)
	}
	if len(m.Chunks) != st.TotalChunks {
		t.Fatalf("manifest has %d chunks, stats say %d", len(m.Chunks), st.TotalChunks)
	}
	var off int
	for _, c := range m.Chunks {
		if want := store.KeyOf(data[off : off+c.Size]).String(); c.Hash != want {
			t.Fatalf("chunk hash %s, want %s", c.Hash, want)
		}
		off += c.Size
	}
}

func TestList(t *testing.T) {
	v, _ := newVault(t)
	if got, err := v.List(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("empty List = %v, %v", got, err)
	}
	mustPut(t, v, "b", []byte("bbb"))
	mustPut(t, v, "a/x", randomBytes(6, 10_000))
	got, err := v.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a/x" || got[1].Name != "b" {
		t.Fatalf("List = %+v", got)
	}
	if got[0].Size != 10_000 || got[1].Size != 3 || got[1].Chunks != 1 || got[0].Created.IsZero() {
		t.Fatalf("List = %+v", got)
	}
}

func TestGetMissing(t *testing.T) {
	v, _ := newVault(t)
	if err := v.Get(context.Background(), "nope", io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestInvalidName(t *testing.T) {
	v, _ := newVault(t)
	if _, err := v.Put(context.Background(), "", strings.NewReader("x")); !errors.Is(err, store.ErrInvalidName) {
		t.Fatalf("Put = %v", err)
	}
}

// corruptBlob flips a byte in the stored blob for key.
func corruptBlob(t *testing.T, dir string, key string) {
	t.Helper()
	p := filepath.Join(dir, "objects", key[:2], key[2:4], key)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGetDetectsCorruptChunk(t *testing.T) {
	v, dir := newVault(t)
	data := randomBytes(7, 50_000)
	st := mustPut(t, v, "f", data)
	m, err := v.loadManifest(context.Background(), st.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	corruptBlob(t, dir, m.Chunks[2].Hash)

	var buf bytes.Buffer
	err = v.Get(context.Background(), "f", &buf)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
	// Only the verified prefix may have been emitted.
	want := m.Chunks[0].Size + m.Chunks[1].Size
	if buf.Len() != want || !bytes.Equal(buf.Bytes(), data[:want]) {
		t.Fatalf("emitted %d bytes, want verified prefix of %d", buf.Len(), want)
	}
}

func TestGetDetectsTruncatedChunk(t *testing.T) {
	v, dir := newVault(t)
	st := mustPut(t, v, "f", randomBytes(8, 20_000))
	m, _ := v.loadManifest(context.Background(), st.ManifestKey)
	h := m.Chunks[0].Hash
	p := filepath.Join(dir, "objects", h[:2], h[2:4], h)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 10); err != nil {
		t.Fatal(err)
	}
	if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
}

func TestGetDetectsMissingChunk(t *testing.T) {
	v, dir := newVault(t)
	st := mustPut(t, v, "f", randomBytes(9, 20_000))
	m, _ := v.loadManifest(context.Background(), st.ManifestKey)
	h := m.Chunks[1].Hash
	if err := os.Remove(filepath.Join(dir, "objects", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestCorruptManifestDetected(t *testing.T) {
	v, dir := newVault(t)
	st := mustPut(t, v, "f", randomBytes(10, 5_000))
	corruptBlob(t, dir, st.ManifestKey.String())
	if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
	if _, err := v.List(context.Background()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("List = %v, want ErrCorrupt", err)
	}
}

func TestDecodeManifestValidation(t *testing.T) {
	good := strings.Repeat("a", 64)
	tests := map[string]string{
		"not json":      `{`,
		"bad version":   `{"version":9,"size":0,"chunks":[]}`,
		"bad hash":      `{"version":1,"size":1,"chunks":[{"hash":"zz","size":1}]}`,
		"negative":      `{"version":1,"size":-1,"chunks":[{"hash":"` + good + `","size":-1}]}`,
		"size mismatch": `{"version":1,"size":5,"chunks":[{"hash":"` + good + `","size":1}]}`,
	}
	for name, in := range tests {
		if _, err := decodeManifest([]byte(in)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	if _, err := decodeManifest([]byte(`{"version":1,"size":1,"chunks":[{"hash":"` + good + `","size":1}]}`)); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func TestGetWriteError(t *testing.T) {
	v, _ := newVault(t)
	mustPut(t, v, "f", randomBytes(11, 5_000))
	if err := v.Get(context.Background(), "f", errWriter{}); err == nil {
		t.Fatal("expected write error")
	}
}

type errReader struct{ after int }

func (e *errReader) Read(p []byte) (int, error) {
	if e.after <= 0 {
		return 0, errors.New("read failed")
	}
	n := min(e.after, len(p))
	e.after -= n
	return n, nil
}

func TestPutInputErrorLeavesRefUntouched(t *testing.T) {
	v, _ := newVault(t)
	mustPut(t, v, "f", []byte("original"))
	if _, err := v.Put(context.Background(), "f", &errReader{after: 30_000}); err == nil {
		t.Fatal("expected error")
	}
	if got := mustGet(t, v, "f"); string(got) != "original" {
		t.Fatalf("ref changed after failed Put: %q", got)
	}
}

func TestContextCancelled(t *testing.T) {
	v, _ := newVault(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Put(ctx, "f", strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v", err)
	}
	mustPut(t, v, "f", []byte("data"))
	if err := v.Get(ctx, "f", io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := v.List(ctx); err == nil {
		t.Fatal("List ignored cancelled context")
	}
}

func TestInitAndOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := Open(dir); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Open uninitialised = %v", err)
	}
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir); err == nil {
		t.Fatal("second Init succeeded")
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(`{"format_version":42}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with bad metadata = %v", err)
	}
}

func TestNewRejectsBadChunkerConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, WithChunkerConfig(chunker.Config{MinSize: 1, AvgSize: 2, MaxSize: 3}))
	if !errors.Is(err, chunker.ErrInvalidConfig) {
		t.Fatalf("Open = %v", err)
	}
}

func TestDefaultChunkerConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.cfg != chunker.DefaultConfig() {
		t.Fatalf("cfg = %+v", v.cfg)
	}
}

func countBlobs(t *testing.T, dir string) int {
	t.Helper()
	s, err := store.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := s.List(context.Background(), func(store.Key) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPutManifestTooLarge(t *testing.T) {
	// A limit that fits the empty manifest but not many chunk references.
	v, dir := newVault(t, WithMaxManifestSize(600))
	before := countBlobs(t, dir)
	_, err := v.Put(context.Background(), "big", bytes.NewReader(randomBytes(20, 100_000)))
	if !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("Put = %v, want ErrManifestTooLarge", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("ErrManifestTooLarge must be distinct from ErrCorrupt: %v", err)
	}
	// It fails early: only the few chunks that fit were stored, and no
	// manifest or name ref was written.
	if got := countBlobs(t, dir) - before; got >= 10 {
		t.Fatalf("Put stored %d chunks before failing", got)
	}
	if err := v.Get(context.Background(), "big", io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after failed Put = %v, want ErrNotFound", err)
	}
}

func TestPutManifestLimitBoundary(t *testing.T) {
	data := randomBytes(21, 30_000)
	fixed := fixedClock()
	big, _ := newVault(t, fixed)
	st := mustPut(t, big, "f", data)
	size := manifestSize(t, big, st.ManifestKey)

	// Exactly at the limit is accepted, and can be read back.
	exact, _ := newVault(t, fixed, WithMaxManifestSize(size))
	st = mustPut(t, exact, "f", data)
	if got := mustGet(t, exact, "f"); !bytes.Equal(got, data) {
		t.Fatal("round trip at the limit failed")
	}
	if _, err := exact.List(context.Background()); err != nil {
		t.Fatalf("List at the limit: %v", err)
	}
	_ = st

	// One byte under is rejected.
	under, _ := newVault(t, fixed, WithMaxManifestSize(size-1))
	if _, err := under.Put(context.Background(), "f", bytes.NewReader(data)); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("Put = %v, want ErrManifestTooLarge", err)
	}
}

func fixedClock() Option {
	return WithClock(func() time.Time { return time.Unix(1700000000, 0) })
}

// manifestSize returns the encoded (plaintext) size of the manifest at key.
func manifestSize(t *testing.T, v *Vault, key store.Key) int64 {
	t.Helper()
	data, err := v.readBlob(context.Background(), key, DefaultMaxManifestSize)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

func TestLoadManifestTooLarge(t *testing.T) {
	big, dir := newVault(t)
	st := mustPut(t, big, "f", randomBytes(22, 30_000))
	size := manifestSize(t, big, st.ManifestKey)

	// A vault with a smaller limit sees the stored manifest as too large,
	// not as corrupt.
	big.Close()
	small, err := Open(dir, WithChunkerConfig(testCfg), WithMaxManifestSize(size-1))
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	for name, err := range map[string]error{
		"Get":  small.Get(context.Background(), "f", io.Discard),
		"List": func() error { _, err := small.List(context.Background()); return err }(),
	} {
		if !errors.Is(err, ErrManifestTooLarge) || errors.Is(err, ErrCorrupt) {
			t.Errorf("%s = %v, want ErrManifestTooLarge and not ErrCorrupt", name, err)
		}
	}
}

func TestInvalidManifestLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, WithMaxManifestSize(0)); err == nil {
		t.Fatal("Open accepted a zero manifest limit")
	}
}

func TestChunkRefEncodedLen(t *testing.T) {
	for _, size := range []int{0, 9, 10, 99, 100, 262144, 1 << 30} {
		c := ChunkRef{Hash: strings.Repeat("a", 64), Size: size}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.encodedLen(); got != int64(len(data)) {
			t.Errorf("size %d: encodedLen = %d, json is %d bytes", size, got, len(data))
		}
	}
}
