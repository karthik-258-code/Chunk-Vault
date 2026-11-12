package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

// ProblemKind classifies a problem found by Verify.
type ProblemKind string

// The problems Verify reports. The first four mean that stored data is
// missing or damaged, and so does ProblemSizeMismatch unless a full
// verification shows the blob to be sound; the rest mean that the index
// disagrees with the refs, manifests and blobs, which are the source of truth,
// see ProblemKind.IndexOnly.
const (
	// ProblemMissingManifest is a ref whose manifest blob does not exist.
	ProblemMissingManifest ProblemKind = "missing_manifest"
	// ProblemCorruptManifest is a ref or a manifest blob that cannot be read,
	// decrypted, decoded or validated.
	ProblemCorruptManifest ProblemKind = "corrupt_manifest"
	// ProblemMissingChunk is a chunk a manifest lists whose blob does not
	// exist.
	ProblemMissingChunk ProblemKind = "missing_chunk"
	// ProblemCorruptChunk is a chunk blob that cannot be read or, in a full
	// verification, fails to decode or to hash to its key.
	ProblemCorruptChunk ProblemKind = "corrupt_chunk"
	// ProblemRefcountMismatch is a chunk whose reference count in the index is
	// not the number of times the manifests list it.
	ProblemRefcountMismatch ProblemKind = "refcount_mismatch"
	// ProblemSizeMismatch is a chunk whose stored size in the index is not the
	// size of its blob. The index records the size when the chunk is written,
	// so this is most likely a blob that was truncated or extended since.
	ProblemSizeMismatch ProblemKind = "size_mismatch"
	// ProblemIndexFileWithoutRef is a file in the index that has no ref.
	ProblemIndexFileWithoutRef ProblemKind = "index_file_without_ref"
	// ProblemRefWithoutIndexEntry is a ref whose file is not in the index.
	ProblemRefWithoutIndexEntry ProblemKind = "ref_without_index_entry"
	// ProblemIndexManifestMismatch is a file whose index entry names another
	// manifest than its ref does.
	ProblemIndexManifestMismatch ProblemKind = "index_manifest_mismatch"
)

// IndexOnly reports whether problems of this kind are confined to the index,
// which Reindex rebuilds, rather than being damage to the stored data.
// ProblemSizeMismatch is not: Reindex takes the stored size from the blob on
// disk without reading it, so it would adopt a damaged size and hide the
// damage from the next default verification.
func (k ProblemKind) IndexOnly() bool {
	switch k {
	case ProblemRefcountMismatch, ProblemIndexFileWithoutRef,
		ProblemRefWithoutIndexEntry, ProblemIndexManifestMismatch:
		return true
	}
	return false
}

// Problem is one inconsistency found by Verify.
type Problem struct {
	Kind ProblemKind
	// Name is the file the problem was found through, or "" if there is none.
	// A chunk that several files share is reported once, under the first file
	// that lists it.
	Name string
	// Key is the hex storage key of the manifest or chunk concerned, or "".
	Key string
	// Detail says what is wrong.
	Detail string
}

// String formats the problem on one line.
func (p Problem) String() string {
	s := string(p.Kind)
	if p.Name != "" {
		s += fmt.Sprintf(" %q", p.Name)
	}
	if p.Key != "" {
		s += " " + p.Key
	}
	return s + ": " + p.Detail
}

// VerifyReport is the result of Verify.
type VerifyReport struct {
	// Full is whether chunks were decoded, see Verify.
	Full bool
	// Files is the number of refs checked.
	Files int
	// Chunks is the number of distinct chunks the readable manifests list.
	Chunks int
	// Bytes is the stored size of the chunk blobs that exist: the data checked
	// for existence and size and, in a full verification, also read, decoded
	// and hashed.
	Bytes int64
	// Problems lists everything that was found, in the order found.
	Problems []Problem
}

// OK reports whether no problem was found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

// IndexOnly reports whether there are problems and all of them are confined
// to the index, so that Reindex repairs them. A size mismatch is not, see
// ProblemKind.IndexOnly; HasSizeMismatch tells it apart.
func (r VerifyReport) IndexOnly() bool {
	if r.OK() {
		return false
	}
	for _, p := range r.Problems {
		if !p.Kind.IndexOnly() {
			return false
		}
	}
	return true
}

// HasSizeMismatch reports whether a chunk blob has another size than the
// index records, which may mean that the blob is damaged.
func (r VerifyReport) HasSizeMismatch() bool {
	for _, p := range r.Problems {
		if p.Kind == ProblemSizeMismatch {
			return true
		}
	}
	return false
}

func (r *VerifyReport) add(kind ProblemKind, name, key, detail string) {
	r.Problems = append(r.Problems, Problem{Kind: kind, Name: name, Key: key, Detail: detail})
}

// Verify checks the repository and reports every problem it finds instead of
// stopping at the first. A returned error means the check itself could not
// finish, such as a cancelled ctx or an unreadable index; the report then
// holds what was found so far.
//
// Every mode reads and validates each manifest, because the reference counts
// are recomputed from them, and compares the index with the refs, the
// manifests and the sizes of the blobs on disk. By default it only checks that
// each chunk blob exists; it does not read them. A full verification also
// reads, decrypts, decompresses and re-hashes every chunk through the codec,
// the same path Get takes, which finds a blob that was damaged without
// changing its size.
//
// Each distinct chunk is checked once however many files use it. If some
// manifests cannot be read the recomputed counts are incomplete, so then only
// an index count below the recomputed one is reported.
func (v *Vault) Verify(ctx context.Context, full bool) (VerifyReport, error) {
	rep := VerifyReport{Full: full}
	names, err := v.refs.List()
	if err != nil {
		return rep, err
	}
	rep.Files = len(names)

	refNames := make(map[string]struct{}, len(names))
	want := make(map[store.Key]uint64)
	seen := make(map[store.Key]struct{})
	complete := true
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		refNames[name] = struct{}{}
		mkey, refErr := v.refs.Get(name)
		if err := v.verifyIndexFile(ctx, &rep, name, mkey, refErr); err != nil {
			return rep, err
		}
		if refErr != nil {
			complete = false
			rep.add(ProblemCorruptManifest, name, "", fmt.Sprintf("ref is unreadable: %v", refErr))
			continue
		}
		m, err := v.loadManifest(ctx, mkey)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return rep, cerr
			}
			complete = false
			if errors.Is(err, store.ErrNotFound) {
				rep.add(ProblemMissingManifest, name, mkey.String(), "manifest blob is missing")
			} else {
				rep.add(ProblemCorruptManifest, name, mkey.String(), err.Error())
			}
			continue
		}
		for _, c := range m.Chunks {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			// decodeManifest checked that the hash parses.
			key, _ := store.ParseKey(c.Hash)
			want[key]++
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			rep.Chunks++
			if err := v.verifyChunk(ctx, &rep, name, key, c, full); err != nil {
				return rep, err
			}
		}
	}

	if err := v.verifyIndexOrphans(ctx, &rep, refNames); err != nil {
		return rep, err
	}
	return rep, v.verifyRefcounts(ctx, &rep, want, complete)
}

// indexError wraps a failure to read the index, unless it is a cancelled ctx.
func indexError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return fmt.Errorf("vault: verify: read index (if it is damaged, 'chunkvault reindex' rebuilds it): %w", err)
}

// verifyIndexFile compares the index entry of the file called name with its
// ref, whose lookup returned mkey and refErr.
func (v *Vault) verifyIndexFile(ctx context.Context, rep *VerifyReport, name string, mkey store.Key, refErr error) error {
	f, err := v.idx.GetFile(name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		rep.add(ProblemRefWithoutIndexEntry, name, "", "the file is not in the index")
	case err != nil:
		return indexError(ctx, err)
	case refErr == nil && f.ManifestKey != mkey:
		rep.add(ProblemIndexManifestMismatch, name, mkey.String(),
			fmt.Sprintf("the index has manifest %s for this file", f.ManifestKey))
	}
	return nil
}

// verifyChunk checks the blob of chunk key, which the manifest of the file
// called name lists as c: that it exists, that its size is the one the index
// holds, and when full that it decodes to the chunk.
func (v *Vault) verifyChunk(ctx context.Context, rep *VerifyReport, name string, key store.Key, c ChunkRef, full bool) error {
	size, err := v.blobs.Size(ctx, key)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if errors.Is(err, store.ErrNotFound) {
			rep.add(ProblemMissingChunk, name, key.String(), "chunk blob is missing")
		} else {
			rep.add(ProblemCorruptChunk, name, key.String(), err.Error())
		}
		return nil
	}
	rep.Bytes += size
	switch e, err := v.idx.GetChunk(key); {
	case errors.Is(err, index.ErrNotFound):
		// verifyRefcounts reports it.
	case err != nil:
		return indexError(ctx, err)
	case e.StoredSize != size:
		rep.add(ProblemSizeMismatch, name, key.String(),
			fmt.Sprintf("the index has stored size %d, the blob is %d bytes", e.StoredSize, size))
	}
	if !full {
		return nil
	}
	if _, err := v.readChunk(ctx, c); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rep.add(ProblemCorruptChunk, name, key.String(), err.Error())
	}
	return nil
}

// verifyIndexOrphans reports the files in the index that are not in refNames.
func (v *Vault) verifyIndexOrphans(ctx context.Context, rep *VerifyReport, refNames map[string]struct{}) error {
	err := v.idx.ForEachFile(func(f index.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := refNames[f.Name]; !ok {
			rep.add(ProblemIndexFileWithoutRef, f.Name, "", "the index lists a file that has no ref")
		}
		return nil
	})
	if err != nil {
		return indexError(ctx, err)
	}
	return nil
}

// verifyRefcounts compares the reference count of every indexed chunk with
// want, the number of occurrences found in the manifests, which it consumes.
// If complete is false some manifests were not counted, so only counts that
// are too low can be told to be wrong.
func (v *Vault) verifyRefcounts(ctx context.Context, rep *VerifyReport, want map[store.Key]uint64, complete bool) error {
	err := v.idx.ForEachChunk(func(e index.ChunkEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := want[e.Key]
		delete(want, e.Key)
		if e.Refs == w || (!complete && e.Refs > w) {
			return nil
		}
		rep.add(ProblemRefcountMismatch, "", e.Key.String(),
			fmt.Sprintf("the index counts %d references, the manifests list the chunk %d times", e.Refs, w))
		return nil
	})
	if err != nil {
		return indexError(ctx, err)
	}
	// What is left is listed by a manifest but unknown to the index.
	missing := make([]store.Key, 0, len(want))
	for k := range want {
		missing = append(missing, k)
	}
	sort.Slice(missing, func(i, j int) bool { return bytes.Compare(missing[i][:], missing[j][:]) < 0 })
	for _, k := range missing {
		rep.add(ProblemRefcountMismatch, "", k.String(),
			fmt.Sprintf("the index has no entry, the manifests list the chunk %d times", want[k]))
	}
	return nil
}
