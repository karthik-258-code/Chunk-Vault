package vault

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
)

func mustStats(t *testing.T, v *Vault) Stats {
	t.Helper()
	s, err := v.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestStatsEmptyRepo(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, _ string) {
		if s := mustStats(t, v); s != (Stats{}) {
			t.Fatalf("Stats of an empty repository = %+v, want all zero", s)
		}
		// Only empty files: no chunks, so still no ratio to divide by.
		mustPut(t, v, "empty", nil)
		s := mustStats(t, v)
		if s.Files != 1 || s.LogicalBytes != 0 || s.DedupRatio != 0 || s.CompressionRatio != 0 {
			t.Fatalf("Stats with only an empty file = %+v", s)
		}
	})
}

func TestStatsDedupAndCompression(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, _ string) {
		// Compressible data stored under two names.
		data := bytes.Repeat([]byte("chunkvault stats "), 4000)
		mustPut(t, v, "a", data)
		mustPut(t, v, "b", data)
		s := mustStats(t, v)
		if s.Files != 2 || s.LogicalBytes != 2*int64(len(data)) {
			t.Fatalf("Stats = %+v, want 2 files of %d bytes each", s, len(data))
		}
		if s.UniqueRawBytes <= 0 || s.UniqueRawBytes > int64(len(data)) {
			t.Fatalf("unique raw bytes = %d, want at most one copy (%d)", s.UniqueRawBytes, len(data))
		}
		if !near(s.DedupRatio, float64(s.LogicalBytes)/float64(s.UniqueRawBytes)) || s.DedupRatio <= 1 {
			t.Fatalf("dedup ratio = %v, logical %d, unique raw %d", s.DedupRatio, s.LogicalBytes, s.UniqueRawBytes)
		}
		if !near(s.CompressionRatio, float64(s.UniqueRawBytes)/float64(s.StoredBytes)) || s.CompressionRatio <= 1 {
			t.Fatalf("compression ratio = %v, raw %d, stored %d", s.CompressionRatio, s.UniqueRawBytes, s.StoredBytes)
		}
		if s.UnreferencedChunks != 0 || s.ReclaimableBytes != 0 {
			t.Fatalf("nothing was removed yet: %+v", s)
		}

		// Random data does not dedup and does not compress: every raw byte is
		// new, and its blobs are larger than the data.
		v2 := randomBytes(99, 50_000)
		mustPut(t, v, "rand", v2)
		after := mustStats(t, v)
		if after.Files != 3 || after.LogicalBytes != s.LogicalBytes+int64(len(v2)) ||
			after.UniqueRawBytes != s.UniqueRawBytes+int64(len(v2)) {
			t.Fatalf("Stats after adding random data = %+v (before %+v)", after, s)
		}
		if after.StoredBytes <= s.StoredBytes+int64(len(v2)) {
			t.Fatalf("stored %d -> %d: random data cannot shrink, and blobs have a header", s.StoredBytes, after.StoredBytes)
		}
	})
}

func TestStatsReclaimable(t *testing.T) {
	eachMode(t, func(t *testing.T, v *Vault, _ string) {
		keep := randomBytes(100, 30_000)
		drop := randomBytes(101, 30_000)
		mustPut(t, v, "keep", keep)
		before := mustStats(t, v)
		mustPut(t, v, "drop", drop)
		both := mustStats(t, v)
		if err := v.Remove("drop"); err != nil {
			t.Fatal(err)
		}
		s := mustStats(t, v)
		// Back to the figures of keep alone, with the chunks of drop as garbage.
		if s.Files != 1 || s.LogicalBytes != int64(len(keep)) || s.UniqueChunks != before.UniqueChunks ||
			s.UniqueRawBytes != before.UniqueRawBytes || s.StoredBytes != before.StoredBytes {
			t.Fatalf("Stats after rm = %+v, want the figures of keep alone %+v", s, before)
		}
		if s.UnreferencedChunks != both.UniqueChunks-before.UniqueChunks ||
			s.ReclaimableBytes != both.StoredBytes-before.StoredBytes || s.ReclaimableBytes == 0 {
			t.Fatalf("Stats after rm = %+v, want the chunks of drop reclaimable (both %+v)", s, both)
		}
	})
}

func TestStatsContextCancelled(t *testing.T) {
	v, _ := newVault(t)
	mustPut(t, v, "a", randomBytes(102, 10_000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Stats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stats with a cancelled context = %v", err)
	}
}
