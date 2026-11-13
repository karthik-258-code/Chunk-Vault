package vault

import (
	"context"
	"fmt"

	"github.com/karthik-258-code/Chunk-Vault/internal/index"
	"github.com/karthik-258-code/Chunk-Vault/internal/store"
)

// GCStats summarises a GC run.
type GCStats struct {
	// Collected is the number of blobs deleted (or that would be deleted in a dry run).
	Collected int64
	// FreedBytes is the total stored size of deleted blobs.
	FreedBytes int64
}

// GC runs a two-phase mark-and-sweep garbage collector that removes blobs whose
// reference count has dropped to zero (chunks left behind by Remove or by
// overwriting a file with Put).
//
// Phase 1 (scan): collect the keys of all zero-ref chunks from the index.
// Phase 2 (sweep): for each key, re-read the index entry; if Refs > 0 a
// concurrent Put added a reference — skip it. Otherwise delete the blob and
// remove the index entry.
//
// When dryRun is true the sweep reports what would be collected without
// deleting anything.
func (v *Vault) GC(ctx context.Context, dryRun bool) (GCStats, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var stats GCStats

	// Phase 1: snapshot zero-ref keys.
	var candidates []store.Key
	if err := v.idx.ForEachChunk(func(e index.ChunkEntry) error {
		if e.Refs == 0 {
			candidates = append(candidates, e.Key)
		}
		return ctx.Err()
	}); err != nil {
		return stats, fmt.Errorf("vault: gc scan: %w", err)
	}

	if dryRun {
		// In dry-run mode, count without deleting.
		for _, key := range candidates {
			e, err := v.idx.GetChunk(key)
			if err != nil {
				continue // may have been removed by a concurrent operation
			}
			if e.Refs > 0 {
				continue // concurrent Put added a reference
			}
			stats.Collected++
			stats.FreedBytes += e.StoredSize
		}
		return stats, nil
	}

	// Phase 2: re-verify and delete.
	for _, key := range candidates {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		e, err := v.idx.GetChunk(key)
		if err != nil {
			continue // already gone
		}
		if e.Refs > 0 {
			continue // concurrent Put added a reference since phase 1
		}
		freed := e.StoredSize
		if err := v.blobs.Delete(ctx, key); err != nil {
			return stats, fmt.Errorf("vault: gc delete blob %s: %w", key, err)
		}
		if err := v.idx.ForgetChunk(key); err != nil {
			return stats, fmt.Errorf("vault: gc remove index entry %s: %w", key, err)
		}
		stats.Collected++
		stats.FreedBytes += freed
	}
	return stats, nil
}
