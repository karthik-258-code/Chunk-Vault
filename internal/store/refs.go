package store

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const refsDir = "refs"

// ErrInvalidName is returned for ref names that are empty or unsafe to map to
// a file.
var ErrInvalidName = errors.New("store: invalid name")

// Refs maps human-readable names to the Key of a manifest blob. Each ref is a
// small file under <root>/refs holding the key in hex. Updates are atomic.
type Refs struct {
	dir string
}

// NewRefs opens, creating if necessary, the ref directory under root.
func NewRefs(root string) (*Refs, error) {
	dir := filepath.Join(root, refsDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: create refs dir: %w", err)
	}
	return &Refs{dir: dir}, nil
}

// encodeName makes name safe to use as a single file name. Names are escaped
// rather than rejected so any non-empty name, including ones containing path
// separators such as "photos/2024/a.jpg", can be stored. Upper-case letters are
// escaped too so names stay distinct on case-insensitive file systems.
func encodeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidName)
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	// Stay well under the common 255-byte file name limit.
	if b.Len() > 240 {
		return "", fmt.Errorf("%w: too long", ErrInvalidName)
	}
	return b.String(), nil
}

// decodeName reverses encodeName. It reports false for file names that
// encodeName could not have produced.
func decodeName(enc string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(enc); i++ {
		if enc[i] != '%' {
			b.WriteByte(enc[i])
			continue
		}
		if i+3 > len(enc) {
			return "", false
		}
		v, err := hex.DecodeString(enc[i+1 : i+3])
		if err != nil {
			return "", false
		}
		b.WriteByte(v[0])
		i += 2
	}
	return b.String(), true
}

// ValidateName reports whether name can be used as a ref name.
func ValidateName(name string) error {
	_, err := encodeName(name)
	return err
}

// Set points name at key, replacing any previous value atomically.
func (r *Refs) Set(name string, key Key) error {
	enc, err := encodeName(name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: create ref temp: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(key.String() + "\n"); err != nil {
		return fmt.Errorf("store: write ref: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("store: fsync ref: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close ref: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(r.dir, enc)); err != nil {
		return fmt.Errorf("store: commit ref: %w", err)
	}
	ok = true
	syncDir(r.dir)
	return nil
}

// Remove deletes the ref called name. It returns ErrNotFound if there is no
// such ref. The manifest blob the ref pointed at is left in the blob store.
func (r *Refs) Remove(name string) error {
	enc, err := encodeName(name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(r.dir, enc)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: ref %q", ErrNotFound, name)
		}
		return fmt.Errorf("store: remove ref %q: %w", name, err)
	}
	syncDir(r.dir)
	return nil
}

// Get returns the key name points at, or ErrNotFound.
func (r *Refs) Get(name string) (Key, error) {
	enc, err := encodeName(name)
	if err != nil {
		return Key{}, err
	}
	data, err := os.ReadFile(filepath.Join(r.dir, enc))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Key{}, fmt.Errorf("%w: ref %q", ErrNotFound, name)
		}
		return Key{}, fmt.Errorf("store: read ref %q: %w", name, err)
	}
	return ParseKey(strings.TrimSpace(string(data)))
}

// List returns all ref names in ascending order.
func (r *Refs) List() ([]string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("store: read refs: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if name, ok := decodeName(e.Name()); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
