package vault

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/karthik-258-code/Chunk-Vault/internal/store"
)

// ManifestVersion is the current on-disk manifest schema version.
const ManifestVersion = 1

// ChunkRef identifies one chunk of a stored file.
type ChunkRef struct {
	// Hash is the lowercase hex storage key of the chunk, which is also its key
	// in the blob store: the SHA-256 of the chunk, or for an encrypted
	// repository its HMAC-SHA-256 under the secret MAC key.
	Hash string `json:"hash"`
	// Size is the chunk length in bytes.
	Size int `json:"size"`
}

// encodedLen returns the exact number of bytes c occupies in an encoded
// manifest, not counting the comma that separates it from its neighbour.
func (c ChunkRef) encodedLen() int64 {
	// {"hash":"<64 hex>","size":<digits>}
	const fixed = len(`{"hash":"`) + 2*32 + len(`","size":`) + len(`}`)
	return int64(fixed + len(strconv.Itoa(c.Size)))
}

// Manifest describes a stored file as an ordered list of chunks. It is
// serialised to JSON and stored in the blob store, through the codec, under its
// own storage key.
type Manifest struct {
	// Version is the schema version, see ManifestVersion.
	Version int `json:"version"`
	// Name is the name the file was stored under.
	Name string `json:"name"`
	// Size is the total file size in bytes.
	Size int64 `json:"size"`
	// Created is when the file was stored, in UTC.
	Created time.Time `json:"created"`
	// Chunks lists the file's chunks in order.
	Chunks []ChunkRef `json:"chunks"`
}

// encode serialises m.
func (m *Manifest) encode() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("vault: encode manifest: %w", err)
	}
	return data, nil
}

// decodeManifest parses and validates a serialised manifest.
func decodeManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %w", ErrCorrupt, err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("%w: unsupported manifest version %d", ErrCorrupt, m.Version)
	}
	var total int64
	for i, c := range m.Chunks {
		if _, err := store.ParseKey(c.Hash); err != nil {
			return nil, fmt.Errorf("%w: manifest chunk %d: %w", ErrCorrupt, i, err)
		}
		if c.Size < 0 {
			return nil, fmt.Errorf("%w: manifest chunk %d has negative size", ErrCorrupt, i)
		}
		total += int64(c.Size)
	}
	if total != m.Size {
		return nil, fmt.Errorf("%w: manifest size %d does not match chunk sizes (%d)", ErrCorrupt, m.Size, total)
	}
	return &m, nil
}
