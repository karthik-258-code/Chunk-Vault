package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	objectsDir = "objects"
	tmpDir     = "tmp"
	dirPerm    = 0o755
	filePerm   = 0o444
)

// FS is a BlobStore backed by a local directory. Blobs live at
// <root>/objects/ab/cd/<full-hex-key>, where ab and cd are the first two
// bytes of the key in hex, which bounds directory sizes.
type FS struct {
	root string
}

var _ BlobStore = (*FS)(nil)

// NewFS opens, creating if necessary, a filesystem blob store rooted at root.
func NewFS(root string) (*FS, error) {
	for _, d := range []string{objectsDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(root, d), dirPerm); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", d, err)
		}
	}
	return &FS{root: root}, nil
}

// Root returns the directory the store lives in.
func (s *FS) Root() string { return s.root }

// path returns the final location of the blob for key.
func (s *FS) path(k Key) string {
	h := k.String()
	return filepath.Join(s.root, objectsDir, h[0:2], h[2:4], h)
}

// Put implements BlobStore. The blob is streamed into a temporary file,
// fsynced, and atomically renamed into place, so readers never observe a
// partially written blob.
func (s *FS) Put(ctx context.Context, key Key, r io.Reader) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dst := s.path(key)
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("store: stat %s: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "put-*")
	if err != nil {
		return false, fmt.Errorf("store: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return false, fmt.Errorf("store: write %s: %w", key, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		return false, fmt.Errorf("store: chmod %s: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("store: fsync %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("store: close %s: %w", key, err)
	}

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return false, fmt.Errorf("store: create fan-out dir: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, fmt.Errorf("store: commit %s: %w", key, err)
	}
	committed = true
	// Persist the rename itself. Directory fsync is unsupported on some
	// platforms, where the rename is already as durable as it can be made.
	syncDir(dir)
	return true, nil
}

// Get implements BlobStore.
func (s *FS) Get(ctx context.Context, key Key) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("store: open %s: %w", key, err)
	}
	return f, nil
}

// Has implements BlobStore.
func (s *FS) Has(ctx context.Context, key Key) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.path(key))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("store: stat %s: %w", key, err)
	}
}

// Size implements BlobStore.
func (s *FS) Size(ctx context.Context, key Key) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	st, err := os.Stat(s.path(key))
	switch {
	case err == nil:
		return st.Size(), nil
	case errors.Is(err, fs.ErrNotExist):
		return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
	default:
		return 0, fmt.Errorf("store: stat %s: %w", key, err)
	}
}

// Delete implements BlobStore.
func (s *FS) Delete(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: delete %s: %w", key, err)
	}
	return nil
}

// List implements BlobStore. Entries that are not valid blob files, such as
// stray files, are skipped.
func (s *FS) List(ctx context.Context, fn func(Key) error) error {
	base := filepath.Join(s.root, objectsDir)
	l1, err := sortedNames(base)
	if err != nil {
		return err
	}
	for _, a := range l1 {
		l2, err := sortedNames(filepath.Join(base, a))
		if err != nil {
			return err
		}
		for _, b := range l2 {
			names, err := sortedNames(filepath.Join(base, a, b))
			if err != nil {
				return err
			}
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return err
				}
				k, err := ParseKey(name)
				if err != nil || name[0:2] != a || name[2:4] != b {
					continue
				}
				if err := fn(k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// sortedNames returns the sorted entry names of dir, or none if it is absent.
func sortedNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// syncDir fsyncs a directory, ignoring failures on platforms that do not
// support it.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
