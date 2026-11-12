package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

func mustVerify(t *testing.T, v *Vault, full bool) VerifyReport {
	t.Helper()
	rep, err := v.Verify(context.Background(), full)
	if err != nil {
		t.Fatalf("Verify(full=%v): %v", full, err)
	}
	return rep
}

// kinds returns the kinds of the problems in rep, in order.
func kinds(rep VerifyReport) []ProblemKind {
	var ks []ProblemKind
	for _, p := range rep.Problems {
		ks = append(ks, p.Kind)
	}
	return ks
}

func wantKinds(t *testing.T, rep VerifyReport, want ...ProblemKind) {
	t.Helper()
	got := kinds(rep)
	if len(got) != len(want) {
		t.Fatalf("problems = %v, want kinds %v", rep.Problems, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("problems = %v, want kinds %v", rep.Problems, want)
		}
	}
}

// chunkKeys returns the keys of the chunks of the file called name.
func chunkKeys(t *testing.T, v *Vault, name string) []store.Key {
	t.Helper()
	m, err := v.manifest(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Key
	for _, c := range m.Chunks {
		k, err := store.ParseKey(c.Hash)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func removeBlob(t *testing.T, dir string, key store.Key) {
	t.Helper()
	k := key.String()
	if err := os.Remove(filepath.Join(dir, "objects", k[:2], k[2:4], k)); err != nil {
		t.Fatal(err)
	}
}

// editChunkRecord changes the record of a chunk in index.db directly, which
// the index API does not allow. fn gets the stored size, raw size and refs.
// The vault must be closed.
func editChunkRecord(t *testing.T, dir string, key store.Key, fn func(rec []byte)) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, indexFile), 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("chunks"))
		old := b.Get(key[:])
		if old == nil {
			t.Fatalf("chunk %s is not indexed", key)
		}
		rec := append([]byte(nil), old...)
		fn(rec)
		return b.Put(key[:], rec)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCleanRepo(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, _ string) {
		// An empty repository is fine.
		rep := mustVerify(t, v, true)
		if !rep.OK() || rep.Files != 0 || rep.Chunks != 0 || rep.Bytes != 0 {
			t.Fatalf("empty repo report = %+v", rep)
		}

		shared := randomBytes(80, 40_000)
		mustPut(t, v, "a", shared)
		mustPut(t, v, "b", shared)
		mustPut(t, v, "rep", append(append([]byte(nil), shared[:3000]...), shared[:3000]...))
		mustPut(t, v, "empty", nil)
		mustPut(t, v, "a", randomBytes(81, 20_000)) // overwrite leaves unreferenced chunks
		if err := v.Remove("b"); err != nil {
			t.Fatal(err)
		}

		for _, full := range []bool{false, true} {
			rep := mustVerify(t, v, full)
			if !rep.OK() || rep.IndexOnly() {
				t.Fatalf("full=%v: problems on a clean repo: %v", full, rep.Problems)
			}
			if rep.Full != full || rep.Files != 3 || rep.Chunks == 0 || rep.Bytes == 0 {
				t.Fatalf("full=%v: report = %+v", full, rep)
			}
		}
	})
}

func TestVerifyCountsChunksOnce(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, _ string) {
		data := randomBytes(82, 30_000)
		mustPut(t, v, "a", data)
		one := mustVerify(t, v, false)
		mustPut(t, v, "b", data)
		two := mustVerify(t, v, false)
		if two.Files != 2 || two.Chunks != one.Chunks || two.Bytes != one.Bytes {
			t.Fatalf("one copy %+v, two copies %+v: shared chunks must be counted once", one, two)
		}
	})
}

func TestVerifyDeletedChunk(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(83, 30_000))
		keys := chunkKeys(t, v, "a")
		removeBlob(t, dir, keys[1])
		for _, full := range []bool{false, true} {
			rep := mustVerify(t, v, full)
			wantKinds(t, rep, ProblemMissingChunk)
			p := rep.Problems[0]
			if p.Name != "a" || p.Key != keys[1].String() || rep.IndexOnly() {
				t.Fatalf("full=%v: problem = %+v", full, p)
			}
		}
	})
}

func TestVerifyCorruptChunk(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(84, 30_000))
		keys := chunkKeys(t, v, "a")
		corruptBlob(t, dir, keys[2].String()) // flips a byte, the size stays

		// The default mode does not read chunks, so it cannot see this.
		if rep := mustVerify(t, v, false); !rep.OK() {
			t.Fatalf("default mode reported %v", rep.Problems)
		}
		rep := mustVerify(t, v, true)
		wantKinds(t, rep, ProblemCorruptChunk)
		if p := rep.Problems[0]; p.Name != "a" || p.Key != keys[2].String() || !strings.Contains(p.Detail, "corrupt") {
			t.Fatalf("problem = %+v", p)
		}
	})
}

func TestVerifyTruncatedChunkChangesStoredSize(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(85, 30_000))
		k := chunkKeys(t, v, "a")[0].String()
		p := filepath.Join(dir, "objects", k[:2], k[2:4], k)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, raw[:len(raw)-5], 0o644); err != nil {
			t.Fatal(err)
		}
		// Found without reading the blob: the index remembers its size. It is
		// damage to the data, not to the index: Reindex would adopt the size.
		rep := mustVerify(t, v, false)
		wantKinds(t, rep, ProblemSizeMismatch)
		if rep.IndexOnly() || !rep.HasSizeMismatch() {
			t.Fatalf("default: IndexOnly = %v, HasSizeMismatch = %v, want false and true", rep.IndexOnly(), rep.HasSizeMismatch())
		}
		// A full check also decodes it, which fails.
		rep = mustVerify(t, v, true)
		wantKinds(t, rep, ProblemSizeMismatch, ProblemCorruptChunk)
		if rep.IndexOnly() {
			t.Fatal("full: a truncated blob is not an index problem")
		}
	})
}

func TestVerifyManifestProblems(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		a := mustPut(t, v, "a", randomBytes(86, 20_000))
		b := mustPut(t, v, "b", randomBytes(87, 20_000))
		mustPut(t, v, "c", randomBytes(88, 20_000))

		removeBlob(t, dir, a.ManifestKey)
		corruptBlob(t, dir, b.ManifestKey.String())
		// Both are found in the default mode, and neither stops the check: c
		// is still verified. The index counts of the chunks of a and b cannot
		// be compared, so they are not reported.
		for _, full := range []bool{false, true} {
			rep := mustVerify(t, v, full)
			wantKinds(t, rep, ProblemMissingManifest, ProblemCorruptManifest)
			if rep.Problems[0].Name != "a" || rep.Problems[1].Name != "b" || rep.Files != 3 || rep.Chunks == 0 {
				t.Fatalf("full=%v: report = %+v", full, rep)
			}
			if rep.IndexOnly() {
				t.Fatal("manifest damage is not an index problem")
			}
		}
	})
}

func TestVerifyIndexProblems(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(89, 20_000))
		mustPut(t, v, "b", randomBytes(90, 20_000))
		keys := chunkKeys(t, v, "a")
		created := time.Unix(1_700_000_000, 0).UTC()

		// A file in the index with no ref.
		if err := v.idx.AddFile("ghost", store.Key{1}, created, nil); err != nil {
			t.Fatal(err)
		}
		wantKinds(t, mustVerify(t, v, true), ProblemIndexFileWithoutRef)
		if err := v.idx.RemoveFile("ghost"); err != nil {
			t.Fatal(err)
		}

		// A ref with no index entry: releases the references of a's chunks too.
		if err := v.idx.RemoveFile("a"); err != nil {
			t.Fatal(err)
		}
		rep := mustVerify(t, v, true)
		if rep.Problems[0].Kind != ProblemRefWithoutIndexEntry || rep.Problems[0].Name != "a" {
			t.Fatalf("problems = %v", rep.Problems)
		}
		for _, p := range rep.Problems[1:] {
			if p.Kind != ProblemRefcountMismatch {
				t.Fatalf("unexpected problem %v", p)
			}
		}
		if len(rep.Problems) < 2 || !rep.IndexOnly() {
			t.Fatalf("problems = %v, want index-only problems including refcount mismatches", rep.Problems)
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}

		// Reindex repairs all of it.
		mustReindex(t, dir)
		v2 := mustOpen(t, dir)
		if rep := mustVerify(t, v2, true); !rep.OK() {
			t.Fatalf("problems after Reindex: %v", rep.Problems)
		}

		// A skewed refcount and a skewed stored size, both through the file.
		if err := v2.Close(); err != nil {
			t.Fatal(err)
		}
		editChunkRecord(t, dir, keys[0], func(rec []byte) {
			binary.BigEndian.PutUint64(rec[16:], binary.BigEndian.Uint64(rec[16:])+1)
		})
		editChunkRecord(t, dir, keys[1], func(rec []byte) {
			binary.BigEndian.PutUint64(rec[0:], binary.BigEndian.Uint64(rec[0:])+7)
		})
		v3 := mustOpen(t, dir)
		rep = mustVerify(t, v3, false)
		wantKinds(t, rep, ProblemSizeMismatch, ProblemRefcountMismatch)
		if rep.IndexOnly() || !rep.HasSizeMismatch() {
			t.Fatalf("problems = %v, want a size mismatch to rule out index-only", rep.Problems)
		}
		for _, p := range rep.Problems {
			if p.Key == "" || p.Detail == "" {
				t.Fatalf("problem lacks detail: %+v", p)
			}
		}
	})
}

func TestVerifyRefcountTooLowAndForgottenChunk(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		mustPut(t, v, "a", randomBytes(91, 20_000))
		mustPut(t, v, "b", randomBytes(92, 20_000))
		keys := chunkKeys(t, v, "a")
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		// One count that is too low, and one chunk the index has forgotten.
		editChunkRecord(t, dir, keys[0], func(rec []byte) {
			binary.BigEndian.PutUint64(rec[16:], 0)
		})
		editChunkRecord(t, dir, keys[1], func(rec []byte) {
			binary.BigEndian.PutUint64(rec[16:], 0)
		})
		v2 := mustOpen(t, dir)
		if err := v2.idx.ForgetChunk(keys[1]); err != nil {
			t.Fatal(err)
		}
		rep := mustVerify(t, v2, false)
		wantKinds(t, rep, ProblemRefcountMismatch, ProblemRefcountMismatch)
		for _, p := range rep.Problems {
			wantDetail := "the index counts 0 references"
			if p.Key == keys[1].String() {
				wantDetail = "the index has no entry"
			}
			if p.Key != keys[0].String() && p.Key != keys[1].String() || !strings.Contains(p.Detail, wantDetail) {
				t.Fatalf("unexpected problem %+v", p)
			}
		}
		if !rep.IndexOnly() {
			t.Fatalf("problems = %v, want index-only", rep.Problems)
		}
	})
}

func TestVerifyIndexManifestMismatch(t *testing.T) {
	v, _ := newVault(t)
	st := mustPut(t, v, "a", randomBytes(93, 20_000))
	other := mustPut(t, v, "b", randomBytes(94, 20_000))
	f, err := v.idx.GetFile("a")
	if err != nil {
		t.Fatal(err)
	}
	var chunks []index.Chunk
	for _, k := range chunkKeys(t, v, "a") {
		e, err := v.idx.GetChunk(k)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, e.Chunk)
	}
	// Same chunks, so the counts stay right, but another manifest key.
	if err := v.idx.AddFile("a", other.ManifestKey, f.Created, chunks); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, v, true)
	wantKinds(t, rep, ProblemIndexManifestMismatch)
	if !rep.IndexOnly() || rep.Problems[0].Key != st.ManifestKey.String() {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyReportsEverything(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, dir string) {
		a := mustPut(t, v, "a", randomBytes(95, 30_000))
		mustPut(t, v, "b", randomBytes(96, 30_000))
		mustPut(t, v, "c", randomBytes(97, 30_000))
		bk := chunkKeys(t, v, "b")
		ck := chunkKeys(t, v, "c")
		removeBlob(t, dir, a.ManifestKey)
		removeBlob(t, dir, bk[0])
		corruptBlob(t, dir, ck[0].String())

		rep := mustVerify(t, v, true)
		wantKinds(t, rep, ProblemMissingManifest, ProblemMissingChunk, ProblemCorruptChunk)
		if rep.IndexOnly() {
			t.Fatal("data damage is not an index problem")
		}
		for _, p := range rep.Problems {
			if got := p.String(); !strings.Contains(got, string(p.Kind)) || !strings.Contains(got, p.Key) {
				t.Errorf("String() = %q", got)
			}
		}
	})
}

func TestVerifyContextCancelled(t *testing.T) {
	v, _ := newVault(t)
	mustPut(t, v, "a", randomBytes(98, 20_000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, full := range []bool{false, true} {
		if _, err := v.Verify(ctx, full); !errors.Is(err, context.Canceled) {
			t.Fatalf("Verify(full=%v) with a cancelled context = %v", full, err)
		}
	}
}

func TestProblemKindIndexOnly(t *testing.T) {
	data := []ProblemKind{ProblemMissingManifest, ProblemCorruptManifest, ProblemMissingChunk, ProblemCorruptChunk}
	for _, k := range data {
		if k.IndexOnly() {
			t.Errorf("%s is data damage, not an index problem", k)
		}
	}
	idx := []ProblemKind{ProblemRefcountMismatch, ProblemIndexFileWithoutRef,
		ProblemRefWithoutIndexEntry, ProblemIndexManifestMismatch}
	for _, k := range idx {
		if !k.IndexOnly() {
			t.Errorf("%s is an index problem", k)
		}
	}
	// A size mismatch is probably a damaged blob, which Reindex would hide.
	if ProblemSizeMismatch.IndexOnly() {
		t.Error("size_mismatch is probably data damage, not an index problem")
	}
	if (VerifyReport{}).IndexOnly() || (VerifyReport{}).HasSizeMismatch() {
		t.Error("a clean report is not index-only and has no size mismatch")
	}
	if got := (Problem{Kind: ProblemMissingChunk, Detail: "x"}).String(); got != "missing_chunk: x" {
		t.Errorf("String() = %q", got)
	}
}
