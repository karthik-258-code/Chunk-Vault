package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

const snapshotBlobVersion = 1

// SnapshotEntry records one file's name and manifest key at snapshot time.
type SnapshotEntry struct {
	// Name is the file's stored name.
	Name string `json:"name"`
	// ManifestKey is the hex storage key of the file's manifest at snapshot time.
	ManifestKey string `json:"manifest_key"`
}

// Snapshot describes a point-in-time view of a vault.
type Snapshot struct {
	// Tag is the user-supplied label given to CreateSnapshot.
	Tag string `json:"tag"`
	// Created is when the snapshot was taken.
	Created time.Time `json:"created"`
	// Files lists every stored file at snapshot time.
	Files []SnapshotEntry `json:"files"`
}

// snapshotBlob is the JSON structure stored in the blob store for a snapshot.
type snapshotBlob struct {
	Version int             `json:"version"`
	Tag     string          `json:"tag"`
	Created time.Time       `json:"created"`
	Files   []SnapshotEntry `json:"files"`
}

// CreateSnapshot captures the current set of file refs and stores them under
// tag. The tag must be a valid ref name. A concurrent put or rm may or may not
// be reflected; the snapshot records a consistent point in time.
func (v *Vault) CreateSnapshot(ctx context.Context, tag string) (Snapshot, error) {
	if err := store.ValidateName(tag); err != nil {
		return Snapshot{}, fmt.Errorf("vault: snapshot tag: %w", err)
	}

	names, err := v.refs.List()
	if err != nil {
		return Snapshot{}, err
	}

	entries := make([]SnapshotEntry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		key, err := v.refs.Get(name)
		if err != nil {
			return Snapshot{}, err
		}
		entries = append(entries, SnapshotEntry{Name: name, ManifestKey: key.String()})
	}

	created := v.now().UTC().Truncate(time.Second)
	blob := snapshotBlob{
		Version: snapshotBlobVersion,
		Tag:     tag,
		Created: created,
		Files:   entries,
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return Snapshot{}, fmt.Errorf("vault: encode snapshot: %w", err)
	}

	bkey := v.codec.Key(data)
	if _, _, err := v.putBlob(ctx, bkey, data); err != nil {
		return Snapshot{}, fmt.Errorf("vault: store snapshot: %w", err)
	}
	if err := v.snapshotRefs.Set(tag, bkey); err != nil {
		return Snapshot{}, fmt.Errorf("vault: write snapshot ref: %w", err)
	}

	return Snapshot{Tag: tag, Created: created, Files: entries}, nil
}

// ListSnapshots returns all snapshots in ascending tag order.
func (v *Vault) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	tags, err := v.snapshotRefs.List()
	if err != nil {
		return nil, err
	}

	snaps := make([]Snapshot, 0, len(tags))
	for _, tag := range tags {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snap, err := v.loadSnapshotByTag(ctx, tag)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

// RestoreFromSnapshot restores files to the versions recorded in the named
// snapshot. Files whose current manifest key already matches the snapshot are
// left untouched. Files not recorded in the snapshot are also left untouched
// — this is a restore, not a rollback: files added after the snapshot are not
// deleted. Blobs for the restored versions must still be present in the store.
func (v *Vault) RestoreFromSnapshot(ctx context.Context, tag string) error {
	snap, err := v.loadSnapshotByTag(ctx, tag)
	if err != nil {
		return err
	}

	for _, entry := range snap.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		mkey, kErr := store.ParseKey(entry.ManifestKey)
		if kErr != nil {
			return fmt.Errorf("%w: snapshot entry %q has invalid key: %w", ErrCorrupt, entry.Name, kErr)
		}

		// Skip if the ref already points at the snapshot version.
		currentKey, refErr := v.refs.Get(entry.Name)
		if refErr == nil && currentKey == mkey {
			continue
		}

		// Load the manifest to build the index chunk list.
		m, err := v.loadManifest(ctx, mkey)
		if err != nil {
			return fmt.Errorf("vault: restore %q: %w", entry.Name, err)
		}

		chunks := make([]index.Chunk, 0, len(m.Chunks))
		for i, c := range m.Chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			ckey, _ := store.ParseKey(c.Hash)
			storedSize, serr := v.blobs.Size(ctx, ckey)
			if serr != nil {
				return fmt.Errorf("vault: restore %q chunk %d: %w", entry.Name, i, serr)
			}
			chunks = append(chunks, index.Chunk{Key: ckey, StoredSize: storedSize, RawSize: int64(c.Size)})
		}

		if err := v.refs.Set(entry.Name, mkey); err != nil {
			return fmt.Errorf("vault: restore ref %q: %w", entry.Name, err)
		}
		if err := v.idx.AddFile(entry.Name, mkey, m.Created, chunks); err != nil {
			return fmt.Errorf("vault: restore index %q: %w", entry.Name, err)
		}
	}
	return nil
}

// loadSnapshotByTag reads and decodes the snapshot stored under tag.
func (v *Vault) loadSnapshotByTag(ctx context.Context, tag string) (Snapshot, error) {
	bkey, err := v.snapshotRefs.Get(tag)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Snapshot{}, fmt.Errorf("%w: snapshot %q", store.ErrNotFound, tag)
		}
		return Snapshot{}, err
	}
	data, err := v.readBlob(ctx, bkey, v.maxManifest)
	if err != nil {
		return Snapshot{}, fmt.Errorf("vault: read snapshot %q: %w", tag, err)
	}
	var blob snapshotBlob
	if err := json.Unmarshal(data, &blob); err != nil {
		return Snapshot{}, fmt.Errorf("%w: snapshot %q: %w", ErrCorrupt, tag, err)
	}
	if blob.Version != snapshotBlobVersion {
		return Snapshot{}, fmt.Errorf("%w: snapshot %q: unsupported version %d", ErrCorrupt, tag, blob.Version)
	}
	return Snapshot{Tag: blob.Tag, Created: blob.Created, Files: blob.Files}, nil
}
