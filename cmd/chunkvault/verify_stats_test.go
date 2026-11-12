package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
)

// putRandom stores size random bytes under name and returns the file's
// manifest key.
func putRandom(t *testing.T, repo, name string, seed int64, size int) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	writeRandomFile(t, src, seed, size)
	mustRun(t, "", "--repo", repo, "put", src, "--name", name)
	raw, err := os.ReadFile(filepath.Join(repo, "refs", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// largestBlob returns the path of the biggest blob in repo, which is a chunk.
func largestBlob(t *testing.T, repo string) string {
	t.Helper()
	var victim string
	var size int64
	err := filepath.Walk(filepath.Join(repo, "objects"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && info.Size() > size {
			victim, size = p, info.Size()
		}
		return err
	})
	if err != nil || victim == "" {
		t.Fatalf("no blobs found: %v", err)
	}
	return victim
}

func blobPath(repo, key string) string {
	return filepath.Join(repo, "objects", key[:2], key[2:4], key)
}

func flipByte(t *testing.T, p string) {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// verify runs verify with args and returns its output. It fails the test
// unless the command succeeded, if wantOK, or reported problems, if not.
func verify(t *testing.T, repo string, wantOK bool, args ...string) string {
	t.Helper()
	out, errs, err := run(t, "", append([]string{"--repo", repo, "verify"}, args...)...)
	switch {
	case wantOK && err != nil:
		t.Fatalf("verify %v: %v\nstdout: %s\nstderr: %s", args, err, out, errs)
	case !wantOK && !errors.Is(err, errVerifyFailed):
		t.Fatalf("verify %v: err = %v, want a verification failure\nstdout: %s", args, err, out)
	}
	return out
}

func problemLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  ") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

func TestVerifyCleanBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		for _, args := range [][]string{nil, {"--full"}} {
			out := verify(t, repo, true, args...)
			if !strings.Contains(out, "Checked 0 files, 0 chunks, 0 B") || !strings.Contains(out, "no problems found") {
				t.Errorf("empty repo verify %v:\n%s", args, out)
			}
		}
		putRandom(t, repo, "a.bin", 31, 300_000)
		mustRun(t, "", "--repo", repo, "put", "-", "--name", "empty")
		mustRun(t, "", "--repo", repo, "rm", "empty")
		src := filepath.Join(t.TempDir(), "src")
		writeRandomFile(t, src, 31, 300_000)
		mustRun(t, "", "--repo", repo, "put", src, "--name", "copy.bin")

		out := verify(t, repo, true)
		if !strings.HasPrefix(out, "Checked 2 files, ") || !strings.Contains(out, "(default): no problems found\n") {
			t.Errorf("verify output:\n%s", out)
		}
		out = verify(t, repo, true, "--full")
		if !strings.Contains(out, "(full): no problems found\n") || len(problemLines(out)) != 0 {
			t.Errorf("verify --full output:\n%s", out)
		}
	})
}

func TestVerifyDetectsCorruptBlobBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		putRandom(t, repo, "input.bin", 32, 300_000)
		flipByte(t, largestBlob(t, repo))

		// A flipped byte keeps the size, so only a full check reads it.
		if out := verify(t, repo, true); !strings.Contains(out, "no problems found") {
			t.Errorf("default verify of a flipped byte:\n%s", out)
		}
		out := verify(t, repo, false, "--full")
		lines := problemLines(out)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], `corrupt_chunk "input.bin"`) {
			t.Fatalf("verify --full output:\n%s", out)
		}
		if !strings.Contains(out, "1 problem found") || strings.Contains(out, "reindex") {
			t.Errorf("data damage must not be blamed on the index:\n%s", out)
		}
	})
}

func TestVerifyTruncatedBlobSuggestsFullNotReindex(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		putRandom(t, repo, "input.bin", 40, 300_000)
		victim := largestBlob(t, repo)
		raw, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(victim, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(victim, raw[:len(raw)-5], 0o644); err != nil {
			t.Fatal(err)
		}

		// The index remembers the size, so the default mode finds it, but it
		// is damage to the blob: reindex would record the truncated size.
		out := verify(t, repo, false)
		lines := problemLines(out)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], `size_mismatch "input.bin"`) {
			t.Fatalf("verify output:\n%s", out)
		}
		if !strings.Contains(out, "chunkvault verify --full") || strings.Contains(out, "reindex") {
			t.Errorf("want a hint to run verify --full and none for reindex:\n%s", out)
		}

		// A full check reads the blob and reports it as corrupt.
		out = verify(t, repo, false, "--full")
		var kinds []string
		for _, l := range problemLines(out) {
			kinds = append(kinds, strings.Fields(l)[0])
		}
		if len(kinds) != 2 || kinds[0] != "size_mismatch" || kinds[1] != "corrupt_chunk" {
			t.Fatalf("verify --full problems = %v\n%s", kinds, out)
		}
		if strings.Contains(out, "reindex") {
			t.Errorf("reindex suggested for a damaged blob:\n%s", out)
		}
	})
}

func TestVerifyDetectsDeletedBlobBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		putRandom(t, repo, "input.bin", 33, 300_000)
		if err := os.Remove(largestBlob(t, repo)); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{nil, {"--full"}} {
			out := verify(t, repo, false, args...)
			lines := problemLines(out)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], `missing_chunk "input.bin"`) || strings.Contains(out, "reindex") {
				t.Fatalf("verify %v output:\n%s", args, out)
			}
		}
	})
}

func TestVerifyDetectsDeletedManifestBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		mkey := putRandom(t, repo, "gone.bin", 34, 100_000)
		putRandom(t, repo, "kept.bin", 35, 100_000)
		if err := os.Chmod(blobPath(repo, mkey), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(blobPath(repo, mkey)); err != nil {
			t.Fatal(err)
		}
		out := verify(t, repo, false)
		lines := problemLines(out)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], `missing_manifest "gone.bin" `+mkey) {
			t.Fatalf("verify output:\n%s", out)
		}
		// The other file is still checked.
		if !strings.Contains(out, "Checked 2 files") || strings.Contains(out, "Checked 2 files, 0 chunks") {
			t.Errorf("verify stopped at the broken file:\n%s", out)
		}
	})
}

func TestVerifyDetectsCorruptManifestBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		mkey := putRandom(t, repo, "bad.bin", 36, 100_000)
		flipByte(t, blobPath(repo, mkey))
		out := verify(t, repo, false)
		if lines := problemLines(out); len(lines) != 1 || !strings.HasPrefix(lines[0], `corrupt_manifest "bad.bin" `+mkey) {
			t.Fatalf("verify output:\n%s", out)
		}
	})
}

// skewRefcount adds one to the reference count of every chunk in the index of
// repo, which the index API cannot do.
func skewRefcount(t *testing.T, repo string) {
	t.Helper()
	var keys [][]byte
	x, err := index.Open(filepath.Join(repo, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.ForEachChunk(func(c index.ChunkEntry) error {
		keys = append(keys, append([]byte(nil), c.Key[:]...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(filepath.Join(repo, "index.db"), 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("chunks"))
		for _, k := range keys {
			rec := append([]byte(nil), b.Get(k)...)
			binary.BigEndian.PutUint64(rec[16:], binary.BigEndian.Uint64(rec[16:])+1)
			if err := b.Put(k, rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsSkewedRefcountBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		putRandom(t, repo, "input.bin", 37, 200_000)
		skewRefcount(t, repo)
		for _, args := range [][]string{nil, {"--full"}} {
			out := verify(t, repo, false, args...)
			lines := problemLines(out)
			if len(lines) == 0 {
				t.Fatalf("verify %v found nothing:\n%s", args, out)
			}
			for _, l := range lines {
				if !strings.HasPrefix(l, "refcount_mismatch ") || !strings.Contains(l, "the index counts 2 references") {
					t.Errorf("unexpected problem line %q", l)
				}
			}
			// Only the index is wrong, so the user is told how to repair it.
			if !strings.Contains(out, "chunkvault reindex") {
				t.Errorf("no reindex suggestion:\n%s", out)
			}
		}

		mustRun(t, "", "--repo", repo, "reindex")
		if out := verify(t, repo, true, "--full"); !strings.Contains(out, "no problems found") {
			t.Errorf("verify after reindex:\n%s", out)
		}
	})
}

func TestVerifyReportsAllProblemsAndMixedHint(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		putRandom(t, repo, "a.bin", 38, 200_000)
		putRandom(t, repo, "b.bin", 39, 200_000)
		skewRefcount(t, repo)
		if err := os.Remove(largestBlob(t, repo)); err != nil {
			t.Fatal(err)
		}
		out := verify(t, repo, false, "--full")
		if !strings.Contains(out, "missing_chunk") || !strings.Contains(out, "refcount_mismatch") {
			t.Fatalf("verify did not report both kinds of problem:\n%s", out)
		}
		// Not only the index is wrong, so reindex is not suggested: it would
		// fail on the missing chunk.
		if strings.Contains(out, "reindex") {
			t.Errorf("reindex suggested although data is damaged:\n%s", out)
		}
	})
}

func TestVerifyErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "", "--repo", filepath.Join(dir, "none"), "verify"); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("verify without a repo: err = %v", err)
	}
	if _, _, err := run(t, "", "--repo", dir, "verify", "extra"); err == nil {
		t.Fatal("verify accepted an argument")
	}

	useFastKDF(t)
	repo := filepath.Join(dir, "enc")
	setPass(t, "right")
	mustRun(t, "", "init", "--encrypt", repo)
	setPass(t, "wrong")
	if _, _, err := run(t, "", "--repo", repo, "verify"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("verify with the wrong passphrase: err = %v", err)
	}
	if _, _, err := run(t, "", "--repo", repo, "stats"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("stats with the wrong passphrase: err = %v", err)
	}
	if _, _, err := run(t, "", "--repo", filepath.Join(dir, "none"), "stats"); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("stats without a repo: err = %v", err)
	}
}

// statsJSON runs stats --json and parses it, failing on keys it does not
// expect or lacks.
func statsJSON(t *testing.T, repo string) map[string]float64 {
	t.Helper()
	out, _ := mustRun(t, "", "--repo", repo, "stats", "--json")
	var got map[string]float64
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stats --json is not a JSON object of numbers: %v\n%s", err, out)
	}
	want := []string{
		"files", "logical_bytes", "unique_chunks", "unique_raw_bytes", "stored_bytes",
		"dedup_ratio", "compression_ratio", "unreferenced_chunks", "reclaimable_bytes",
	}
	if len(got) != len(want) {
		t.Fatalf("stats --json has keys %v, want exactly %v", got, want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("stats --json lacks %q:\n%s", k, out)
		}
	}
	return got
}

func TestStatsEmptyRepoBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		got := statsJSON(t, repo)
		for k, v := range got {
			if v != 0 {
				t.Errorf("%s = %v on an empty repository, want 0", k, v)
			}
		}
		out, _ := mustRun(t, "", "--repo", repo, "stats")
		for _, want := range []string{"Files:", "0", "0 B", "n/a"} {
			if !strings.Contains(out, want) {
				t.Errorf("stats output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
			t.Errorf("stats of an empty repository divided by zero:\n%s", out)
		}
	})
}

func TestStatsDedupBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		const size = 512 << 10
		putRandom(t, repo, "a.bin", 41, size)
		putRandom(t, repo, "copy.bin", 41, size) // the same bytes under another name

		got := statsJSON(t, repo)
		if got["files"] != 2 || got["logical_bytes"] != 2*size {
			t.Errorf("files = %v, logical_bytes = %v, want 2 and %d", got["files"], got["logical_bytes"], 2*size)
		}
		if got["unique_raw_bytes"] != size || got["dedup_ratio"] != 2 {
			t.Errorf("unique_raw_bytes = %v, dedup_ratio = %v, want %d and 2", got["unique_raw_bytes"], got["dedup_ratio"], size)
		}
		if got["unique_chunks"] < 2 {
			t.Errorf("unique_chunks = %v", got["unique_chunks"])
		}
		// Random data does not compress, and each blob adds a header.
		if got["stored_bytes"] <= size || got["compression_ratio"] >= 1 || got["compression_ratio"] < 0.9 {
			t.Errorf("stored_bytes = %v, compression_ratio = %v", got["stored_bytes"], got["compression_ratio"])
		}
		if got["unreferenced_chunks"] != 0 || got["reclaimable_bytes"] != 0 {
			t.Errorf("nothing is unreferenced yet: %v", got)
		}

		out, _ := mustRun(t, "", "--repo", repo, "stats")
		for _, want := range []string{"Files:", "512.0 KiB", "1.0 MiB", "2.00x", "Compression ratio:", "Reclaimable by GC:"} {
			if !strings.Contains(out, want) {
				t.Errorf("stats output lacks %q:\n%s", want, out)
			}
		}
		// Aligned: every value starts in the same column.
		col := -1
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			i := strings.Index(l, ":")
			j := i + 1
			for j < len(l) && l[j] == ' ' {
				j++
			}
			if col >= 0 && j != col {
				t.Errorf("values are not aligned in %q (column %d, others %d):\n%s", l, j, col, out)
			}
			col = j
		}

		// Removing one name leaves the shared chunks referenced.
		mustRun(t, "", "--repo", repo, "rm", "copy.bin")
		after := statsJSON(t, repo)
		if after["files"] != 1 || after["logical_bytes"] != size || after["dedup_ratio"] != 1 ||
			after["unreferenced_chunks"] != 0 || after["stored_bytes"] != got["stored_bytes"] {
			t.Errorf("stats after rm of the copy = %v", after)
		}

		// Removing the last name that uses chunks makes them reclaimable.
		putRandom(t, repo, "other.bin", 42, 100_000)
		mustRun(t, "", "--repo", repo, "rm", "other.bin")
		gc := statsJSON(t, repo)
		if gc["unreferenced_chunks"] == 0 || gc["reclaimable_bytes"] <= 100_000 {
			t.Errorf("stats after rm of other.bin = %v, want its chunks reclaimable", gc)
		}
		if gc["stored_bytes"] != got["stored_bytes"] || gc["unique_raw_bytes"] != size {
			t.Errorf("unreferenced chunks leaked into the live figures: %v", gc)
		}
		out, _ = mustRun(t, "", "--repo", repo, "stats")
		if !strings.Contains(out, "Unreferenced chunks:") || !strings.Contains(out, "97.") {
			t.Errorf("human stats do not show the reclaimable size:\n%s", out)
		}
	})
}

func TestVerifyAndStatsHelp(t *testing.T) {
	out, _ := mustRun(t, "", "verify", "--help")
	if !strings.Contains(out, "--full") || !strings.Contains(out, "non-zero") ||
		!strings.Contains(out, "reindex is not suggested") {
		t.Errorf("verify --help:\n%s", out)
	}
	out, _ = mustRun(t, "", "stats", "--help")
	if !strings.Contains(out, "--json") || !strings.Contains(out, "snake_case") {
		t.Errorf("stats --help:\n%s", out)
	}
}
