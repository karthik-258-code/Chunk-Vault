package vault

import (
	"context"
	"fmt"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
)

// Stats summarises the space a repository uses and how well it deduplicates
// and compresses. It is computed from the index alone, without reading any
// blob, so it is only as accurate as the index: run Verify, and Reindex if it
// reports index problems, when in doubt. The JSON keys are stable.
type Stats struct {
	// Files is the number of stored files.
	Files int64 `json:"files"`
	// LogicalBytes is the sum of the file sizes, before deduplication.
	LogicalBytes int64 `json:"logical_bytes"`
	// UniqueChunks is the number of distinct chunks that at least one file
	// references.
	UniqueChunks int64 `json:"unique_chunks"`
	// UniqueRawBytes is the size of those chunks before compression and
	// encryption.
	UniqueRawBytes int64 `json:"unique_raw_bytes"`
	// StoredBytes is the size of those chunks on disk, after compression and
	// encryption. It does not include manifests, which the index does not
	// track, or chunks nothing references.
	StoredBytes int64 `json:"stored_bytes"`
	// DedupRatio is LogicalBytes divided by UniqueRawBytes: how many times
	// larger the files are than the data kept for them. It is 0 if there is no
	// data.
	DedupRatio float64 `json:"dedup_ratio"`
	// CompressionRatio is UniqueRawBytes divided by StoredBytes. It is below 1
	// for data that does not compress, because of the blob header and, in an
	// encrypted repository, the nonce and tag. It is 0 if there is no data.
	CompressionRatio float64 `json:"compression_ratio"`
	// UnreferencedChunks is the number of chunks in the index that no file
	// references any more, left by rm or by overwriting a file.
	UnreferencedChunks int64 `json:"unreferenced_chunks"`
	// ReclaimableBytes is the stored size of the unreferenced chunks, which
	// garbage collection will be able to free.
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
}

// Stats returns the repository statistics, see the type.
func (v *Vault) Stats(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	st, err := v.idx.Stats()
	if err != nil {
		return Stats{}, fmt.Errorf("vault: stats: %w", err)
	}
	s := Stats{
		Files:              st.Files,
		LogicalBytes:       st.LogicalBytes,
		UniqueChunks:       st.UniqueChunks - st.ZeroRefChunks,
		StoredBytes:        st.StoredBytes - st.ZeroRefBytes,
		UnreferencedChunks: st.ZeroRefChunks,
		ReclaimableBytes:   st.ZeroRefBytes,
	}
	// The index summary has no raw size, so sum it over the referenced chunks.
	err = v.idx.ForEachChunk(func(e index.ChunkEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Refs > 0 {
			s.UniqueRawBytes += e.RawSize
		}
		return nil
	})
	if err != nil {
		return Stats{}, fmt.Errorf("vault: stats: %w", err)
	}
	if s.UniqueRawBytes > 0 {
		s.DedupRatio = float64(s.LogicalBytes) / float64(s.UniqueRawBytes)
	}
	if s.StoredBytes > 0 {
		s.CompressionRatio = float64(s.UniqueRawBytes) / float64(s.StoredBytes)
	}
	return s, nil
}
