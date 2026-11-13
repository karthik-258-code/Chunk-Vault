package vault

import (
	"bytes"
	"context"
	"testing"
)

func TestGCDeletesUnreferencedChunks(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	// Put a file, then remove it — chunks become unreferenced.
	data := bytes.Repeat([]byte("hello world garbage collect me"), 10000)
	if _, err := v.Put(context.Background(), "f", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := v.Remove("f"); err != nil {
		t.Fatal(err)
	}

	// Before GC: stats should show reclaimable space.
	before, err := v.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.UnreferencedChunks == 0 {
		t.Fatal("expected unreferenced chunks before GC")
	}

	st, err := v.GC(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected == 0 {
		t.Error("GC collected 0 chunks, expected > 0")
	}
	if st.FreedBytes == 0 {
		t.Error("GC freed 0 bytes, expected > 0")
	}

	// After GC: unreferenced chunks should be gone.
	after, err := v.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.UnreferencedChunks != 0 {
		t.Errorf("after GC: UnreferencedChunks = %d, want 0", after.UnreferencedChunks)
	}
}

func TestGCSkipsReferencedChunks(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	// Two files share most of their chunks (identical content).
	data := bytes.Repeat([]byte("shared chunk content for dedup test"), 5000)
	if _, err := v.Put(context.Background(), "a", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Put(context.Background(), "b", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	// Remove file a — shared chunks still referenced by b.
	if err := v.Remove("a"); err != nil {
		t.Fatal(err)
	}

	if _, err := v.GC(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	// File b must still be readable.
	var out bytes.Buffer
	if err := v.Get(context.Background(), "b", &out); err != nil {
		t.Fatalf("get after GC: %v", err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("file b content changed after GC")
	}
}

func TestGCDryRun(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	data := bytes.Repeat([]byte("dry run test data"), 8000)
	if _, err := v.Put(context.Background(), "f", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := v.Remove("f"); err != nil {
		t.Fatal(err)
	}

	before, err := v.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	st, err := v.GC(context.Background(), true) // dry run
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected == 0 {
		t.Error("dry run: expected Collected > 0")
	}

	// Blobs must still exist — dry run must not delete anything.
	after, err := v.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.UnreferencedChunks != before.UnreferencedChunks {
		t.Errorf("dry run changed unreferenced count: before=%d after=%d",
			before.UnreferencedChunks, after.UnreferencedChunks)
	}
}
