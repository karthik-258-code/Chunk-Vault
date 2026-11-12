package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newFS(t *testing.T) *FS {
	t.Helper()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s BlobStore, data []byte) Key {
	t.Helper()
	k := KeyOf(data)
	if _, err := s.Put(context.Background(), k, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return k
}

func TestKeyRoundTrip(t *testing.T) {
	k := KeyOf([]byte("hello"))
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if k.String() != want {
		t.Fatalf("String() = %s, want %s", k, want)
	}
	got, err := ParseKey(want)
	if err != nil || got != k {
		t.Fatalf("ParseKey = %v, %v", got, err)
	}
}

func TestParseKeyRejectsBadInput(t *testing.T) {
	for _, s := range []string{
		"", "abc", strings.Repeat("g", 64), strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65),
	} {
		if _, err := ParseKey(s); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseKey(%q) = %v, want ErrInvalidKey", s, err)
		}
	}
}

func TestPutGetHasDelete(t *testing.T) {
	ctx := context.Background()
	s := newFS(t)
	data := []byte("some chunk content")
	k := KeyOf(data)

	if ok, err := s.Has(ctx, k); err != nil || ok {
		t.Fatalf("Has before Put = %v, %v", ok, err)
	}
	created, err := s.Put(ctx, k, bytes.NewReader(data))
	if err != nil || !created {
		t.Fatalf("Put = %v, %v", created, err)
	}
	if ok, err := s.Has(ctx, k); err != nil || !ok {
		t.Fatalf("Has after Put = %v, %v", ok, err)
	}
	rc, err := s.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(ctx, k); ok {
		t.Fatal("blob still present after Delete")
	}
	if err := s.Delete(ctx, k); err != nil {
		t.Fatalf("deleting an absent blob: %v", err)
	}
}

func TestSize(t *testing.T) {
	ctx := context.Background()
	s := newFS(t)
	data := []byte("some chunk content")
	k := put(t, s, data)
	if n, err := s.Size(ctx, k); err != nil || n != int64(len(data)) {
		t.Fatalf("Size = %d, %v, want %d", n, err, len(data))
	}
	if _, err := s.Size(ctx, KeyOf([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Size of absent blob = %v, want ErrNotFound", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Size(cancelled, k); !errors.Is(err, context.Canceled) {
		t.Fatalf("Size with cancelled context = %v", err)
	}
}

func TestLayoutIsTwoLevelFanOut(t *testing.T) {
	s := newFS(t)
	k := put(t, s, []byte("layout"))
	h := k.String()
	want := filepath.Join(s.Root(), "objects", h[:2], h[2:4], h)
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("blob not at %s: %v", want, err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("blob is writable: %v", info.Mode())
	}
}

func TestPutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newFS(t)
	data := []byte("twice")
	k := put(t, s, data)
	created, err := s.Put(ctx, k, bytes.NewReader(data))
	if err != nil || created {
		t.Fatalf("second Put = %v, %v; want false, nil", created, err)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("disk on fire")
	}
	n := min(f.n, len(p))
	f.n -= n
	return n, nil
}

func TestPutCleansUpOnReadError(t *testing.T) {
	s := newFS(t)
	if _, err := s.Put(context.Background(), KeyOf([]byte("x")), &failingReader{n: 10}); err == nil {
		t.Fatal("expected error")
	}
	assertNoTempFiles(t, s)
}

func assertNoTempFiles(t *testing.T, s *FS) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d leftover temp files", len(entries))
	}
}

func TestGetMissing(t *testing.T) {
	s := newFS(t)
	if _, err := s.Get(context.Background(), KeyOf([]byte("nope"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestListIsSortedAndSkipsStrays(t *testing.T) {
	ctx := context.Background()
	s := newFS(t)
	var want []string
	for i := 0; i < 50; i++ {
		want = append(want, put(t, s, []byte(fmt.Sprintf("blob-%d", i))).String())
	}
	// Strays: wrong name, and a valid-looking name in the wrong fan-out dir.
	dir := filepath.Join(s.Root(), "objects", "ab", "cd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"junk", strings.Repeat("0", 64)} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	if err := s.List(ctx, func(k Key) error { got = append(got, k.String()); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d keys, want %d", len(got), len(want))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("keys not strictly ascending at %d", i)
		}
	}
}

func TestListStopsOnError(t *testing.T) {
	s := newFS(t)
	put(t, s, []byte("a"))
	put(t, s, []byte("b"))
	stop := errors.New("stop")
	calls := 0
	err := s.List(context.Background(), func(Key) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("List = %v after %d calls", err, calls)
	}
}

func TestListEmptyStore(t *testing.T) {
	s := newFS(t)
	if err := os.RemoveAll(filepath.Join(s.Root(), "objects")); err != nil {
		t.Fatal(err)
	}
	if err := s.List(context.Background(), func(Key) error { t.Fatal("unexpected key"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledContext(t *testing.T) {
	s := newFS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k := KeyOf([]byte("c"))
	if _, err := s.Put(ctx, k, strings.NewReader("c")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v", err)
	}
	if _, err := s.Get(ctx, k); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v", err)
	}
	if _, err := s.Has(ctx, k); !errors.Is(err, context.Canceled) {
		t.Errorf("Has = %v", err)
	}
	if err := s.Delete(ctx, k); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v", err)
	}
	put(t, s, []byte("listed"))
	if err := s.List(ctx, func(Key) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v", err)
	}
}

func TestConcurrentPutsOfSameBlob(t *testing.T) {
	ctx := context.Background()
	s := newFS(t)
	data := bytes.Repeat([]byte("concurrent"), 10_000)
	k := KeyOf(data)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(ctx, k, bytes.NewReader(data)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rc, err := s.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("content corrupted by concurrent puts")
	}
	assertNoTempFiles(t, s)
}

func TestNewFSFailsOnFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFS(f); err == nil {
		t.Fatal("expected error")
	}
	if _, err := NewRefs(f); err == nil {
		t.Fatal("expected error")
	}
}

func TestRefs(t *testing.T) {
	r, err := NewRefs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	k1, k2 := KeyOf([]byte("1")), KeyOf([]byte("2"))
	names := []string{"b.txt", "a", "dir/sub/File.TXT", ".hidden", "100%", "ünï.bin"}
	for _, n := range names {
		if err := r.Set(n, k1); err != nil {
			t.Fatalf("Set(%q): %v", n, err)
		}
	}
	if err := r.Set("a", k2); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get("a"); err != nil || got != k2 {
		t.Fatalf("Get(a) = %v, %v", got, err)
	}
	for _, n := range names {
		if _, err := r.Get(n); err != nil {
			t.Errorf("Get(%q): %v", n, err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("List = %q", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("List not sorted: %q", got)
		}
	}
	for _, n := range names {
		found := false
		for _, g := range got {
			found = found || g == n
		}
		if !found {
			t.Errorf("name %q lost in List round trip: %q", n, got)
		}
	}
}

func TestRefsRemove(t *testing.T) {
	r, err := NewRefs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k := KeyOf([]byte("1"))
	for _, n := range []string{"a", "dir/b"} {
		if err := r.Set(n, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Remove("dir/b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := r.Get("dir/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove = %v, want ErrNotFound", err)
	}
	if got, _ := r.List(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("List after Remove = %q", got)
	}
	if err := r.Remove("dir/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Remove = %v, want ErrNotFound", err)
	}
	if err := r.Remove(""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Remove of empty name = %v, want ErrInvalidName", err)
	}
}

func TestRefsInvalidNames(t *testing.T) {
	r, err := NewRefs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "a\x00b", strings.Repeat("x", 300)} {
		if err := r.Set(n, Key{}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Set(%q) = %v", n, err)
		}
		if _, err := r.Get(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Get(%q) = %v", n, err)
		}
	}
}

func TestRefsTraversalNamesStayInside(t *testing.T) {
	root := t.TempDir()
	r, err := NewRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Set("../../escape", Key{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
		t.Fatal("ref escaped the refs directory")
	}
	if got, err := r.Get("../../escape"); err != nil || got != (Key{1}) {
		t.Fatalf("Get = %v, %v", got, err)
	}
}

func TestRefsCorruptAndStrayFiles(t *testing.T) {
	root := t.TempDir()
	r, err := NewRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "refs")
	if err := os.WriteFile(filepath.Join(dir, "bad"), []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad%zz"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trunc%2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("bad"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get(bad) = %v", err)
	}
	names, err := r.List()
	if err != nil || len(names) != 1 || names[0] != "bad" {
		t.Fatalf("List = %q, %v", names, err)
	}
}
