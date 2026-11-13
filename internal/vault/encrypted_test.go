package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/karthik-258-code/Chunk-Vault/internal/codec"
	"github.com/karthik-258-code/Chunk-Vault/internal/store"
)

// blobBytes returns the total size of every blob in the repository.
func blobBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(filepath.Join(dir, "objects"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// modes runs fn against an unencrypted and an encrypted vault.
func modes(t *testing.T, fn func(t *testing.T, v *Vault, dir string)) {
	t.Run("plain", func(t *testing.T) {
		v, dir := newVault(t)
		fn(t, v, dir)
	})
	t.Run("encrypted", func(t *testing.T) {
		v, dir := initEncrypted(t)
		fn(t, v, dir)
	})
}

func TestModesRoundTripAndList(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, _ string) {
		for name, size := range map[string]int{"empty": 0, "tiny": 17, "multi": 200_000} {
			data := randomBytes(int64(size)+100, size)
			mustPut(t, v, name, data)
			if got := mustGet(t, v, name); !bytes.Equal(got, data) {
				t.Errorf("%s: round trip mismatch", name)
			}
		}
		entries, err := v.List(context.Background())
		if err != nil || len(entries) != 3 {
			t.Fatalf("List = %v, %v", entries, err)
		}
	})
}

func TestModesDedup(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, dir string) {
		data := randomBytes(40, 300_000)
		first := mustPut(t, v, "a", data)
		before := countBlobs(t, dir)
		second := mustPut(t, v, "b", data)
		if second.NewChunks != 0 || second.DedupBytes != int64(len(data)) || second.TotalChunks != first.TotalChunks {
			t.Fatalf("second put = %+v, first = %+v", second, first)
		}
		if after := countBlobs(t, dir); after != before+1 {
			t.Fatalf("blob count %d -> %d, want only the new manifest", before, after)
		}
	})
}

func TestModesCompression(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, dir string) {
		data := bytes.Repeat([]byte("compressible "), 30_000)
		mustPut(t, v, "f", data)
		if stored := blobBytes(t, dir); stored > int64(len(data))/4 {
			t.Fatalf("stored %d bytes for %d bytes of repetitive input", stored, len(data))
		}
		if got := mustGet(t, v, "f"); !bytes.Equal(got, data) {
			t.Fatal("round trip mismatch")
		}
	})
}

func TestStorageKeys(t *testing.T) {
	data := randomBytes(41, 50_000)

	plain, _ := newVault(t)
	pm := chunkHashes(t, plain, mustPut(t, plain, "f", data))
	if want := sha256.Sum256(data[:testChunkSize(t, plain, "f", 0)]); pm[0] != store.Key(want).String() {
		t.Fatalf("unencrypted chunk key %s is not the SHA-256 of the chunk", pm[0])
	}

	enc, _ := initEncrypted(t)
	em := chunkHashes(t, enc, mustPut(t, enc, "f", data))
	if len(em) != len(pm) {
		t.Fatalf("chunk counts differ: %d vs %d", len(em), len(pm))
	}
	for i := range em {
		if em[i] == pm[i] {
			t.Fatalf("chunk %d: encrypted storage key equals the plain SHA-256", i)
		}
	}
	// Another repository with its own keys files the same chunks elsewhere.
	enc2, _ := initEncrypted(t)
	for i, h := range chunkHashes(t, enc2, mustPut(t, enc2, "f", data)) {
		if h == em[i] {
			t.Fatalf("chunk %d has the same key in two repositories", i)
		}
	}
}

func chunkHashes(t *testing.T, v *Vault, st PutStats) []string {
	t.Helper()
	m, err := v.loadManifest(context.Background(), st.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(m.Chunks))
	for i, c := range m.Chunks {
		out[i] = c.Hash
	}
	return out
}

func testChunkSize(t *testing.T, v *Vault, name string, i int) int {
	t.Helper()
	key, err := v.refs.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := v.loadManifest(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return m.Chunks[i].Size
}

func TestEncryptedBlobsHideContent(t *testing.T) {
	v, dir := initEncrypted(t)
	secret := []byte("TOP-SECRET-MARKER-0123456789")
	data := append(bytes.Repeat([]byte("."), 1000), secret...)
	mustPut(t, v, "secret-file-name", data)
	err := filepath.Walk(filepath.Join(dir, "objects"), func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, secret) || bytes.Contains(raw, []byte("secret-file-name")) {
			t.Errorf("%s contains plaintext", p)
		}
		if !bytes.HasPrefix(raw, []byte(codec.Magic)) || raw[len(codec.Magic)+1]&codec.FlagEncrypted == 0 {
			t.Errorf("%s is not an encrypted blob", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestModesTamperedChunkDetected(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, dir string) {
		data := randomBytes(42, 50_000)
		hashes := chunkHashes(t, v, mustPut(t, v, "f", data))
		corruptBlob(t, dir, hashes[1])
		err := v.Get(context.Background(), "f", io.Discard)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Get = %v, want ErrCorrupt", err)
		}
		if v.Encrypted() && !errors.Is(err, codec.ErrAuth) {
			t.Fatalf("Get = %v, want the GCM tag check to fail", err)
		}
	})
}

func TestEncryptedTamperedHeaderDetected(t *testing.T) {
	// Clearing the compressed flag changes the authenticated header.
	v, dir := initEncrypted(t)
	data := bytes.Repeat([]byte("compressible "), 5_000)
	hashes := chunkHashes(t, v, mustPut(t, v, "f", data))
	h := hashes[0]
	p := filepath.Join(dir, "objects", h[:2], h[2:4], h)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if raw[len(codec.Magic)+1]&codec.FlagCompressed == 0 {
		t.Fatal("test needs a compressed blob")
	}
	raw[len(codec.Magic)+1] &^= codec.FlagCompressed
	rewrite(t, p, raw)
	if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, codec.ErrAuth) {
		t.Fatalf("Get = %v, want ErrAuth", err)
	}
}

func rewrite(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedSwappedBlobsDetected(t *testing.T) {
	// A blob moved under another chunk's key must not decode: the key is bound
	// into the GCM additional data.
	v, dir := initEncrypted(t)
	hashes := chunkHashes(t, v, mustPut(t, v, "f", randomBytes(43, 50_000)))
	path := func(h string) string { return filepath.Join(dir, "objects", h[:2], h[2:4], h) }
	other, err := os.ReadFile(path(hashes[1]))
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, path(hashes[0]), other)
	if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, codec.ErrAuth) {
		t.Fatalf("Get = %v, want ErrAuth", err)
	}
}

func TestModesTamperedManifestDetected(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, dir string) {
		st := mustPut(t, v, "f", randomBytes(44, 5_000))
		corruptBlob(t, dir, st.ManifestKey.String())
		if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Get = %v, want ErrCorrupt", err)
		}
		if _, err := v.List(context.Background()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("List = %v, want ErrCorrupt", err)
		}
	})
}

func TestModesTruncatedBlobDetected(t *testing.T) {
	modes(t, func(t *testing.T, v *Vault, dir string) {
		h := chunkHashes(t, v, mustPut(t, v, "f", randomBytes(45, 20_000)))[0]
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
	})
}

func TestOversizedBlobRejected(t *testing.T) {
	// A blob much larger than any chunk can be is refused without being
	// decoded.
	modes(t, func(t *testing.T, v *Vault, dir string) {
		h := chunkHashes(t, v, mustPut(t, v, "f", randomBytes(46, 5_000)))[0]
		p := filepath.Join(dir, "objects", h[:2], h[2:4], h)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rewrite(t, p, append(raw, make([]byte, 1000)...))
		if err := v.Get(context.Background(), "f", io.Discard); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Get = %v, want ErrCorrupt", err)
		}
	})
}

func TestEncryptedManifestLimit(t *testing.T) {
	v, dir := initEncrypted(t)
	st := mustPut(t, v, "f", randomBytes(47, 30_000))
	size := manifestSize(t, v, st.ManifestKey)
	v.Close()
	small, err := Open(dir, WithChunkerConfig(testCfg), passFn(testPass), WithMaxManifestSize(size-1))
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	if err := small.Get(context.Background(), "f", io.Discard); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("Get = %v, want ErrManifestTooLarge", err)
	}
}

func TestReopenEncrypted(t *testing.T) {
	v, dir := initEncrypted(t)
	data := randomBytes(48, 80_000)
	mustPut(t, v, "f", data)
	v.Close()
	v2, err := Open(dir, passFn(testPass))
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if got := mustGet(t, v2, "f"); !bytes.Equal(got, data) {
		t.Fatal("data differs after reopening")
	}
}

func TestPropModesRoundTrip(t *testing.T) {
	plain, _ := newVault(t)
	enc, _ := initEncrypted(t)
	rapid.Check(t, func(t *rapid.T) {
		v := plain
		if rapid.Bool().Draw(t, "encrypted") {
			v = enc
		}
		var data []byte
		if rapid.Bool().Draw(t, "repetitive") {
			unit := rapid.SliceOfN(rapid.Byte(), 1, 8).Draw(t, "unit")
			data = bytes.Repeat(unit, rapid.IntRange(0, 3000).Draw(t, "reps"))
		} else {
			data = rapid.SliceOfN(rapid.Byte(), 0, 12_000).Draw(t, "data")
		}
		if _, err := v.Put(context.Background(), "f", bytes.NewReader(data)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		var buf bytes.Buffer
		if err := v.Get(context.Background(), "f", &buf); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("round trip mismatch for %d bytes", len(data))
		}
	})
}

func TestPropEncryptedDedupAcrossNames(t *testing.T) {
	v, _ := initEncrypted(t)
	n := 0
	rapid.Check(t, func(t *rapid.T) {
		data := rapid.SliceOfN(rapid.Byte(), 1, 8_000).Draw(t, "data")
		n++
		a, err := v.Put(context.Background(), "a"+string(rune('a'+n%26))+string(rune('a'+n/26%26)), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b, err := v.Put(context.Background(), "dup", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if b.TotalChunks != a.TotalChunks || b.NewChunks != 0 {
			t.Fatalf("re-putting identical data stored new chunks: first %+v, second %+v", a, b)
		}
	})
}
