// Package chunker implements FastCDC content-defined chunking.
//
// FastCDC (Xia et al., USENIX ATC 2016) splits a byte stream into
// variable-sized chunks whose boundaries depend on the local content rather
// than on absolute offsets. Inserting or deleting bytes therefore only
// disturbs the chunks near the edit, which is what makes deduplication of
// slightly-modified files effective.
//
// This implementation uses a Gear rolling hash, cut-point skipping below the
// minimum chunk size, and normalized chunking: a stricter mask is applied
// before the target average size and a looser one after it, which tightens
// the chunk size distribution around the average.
package chunker

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// Default chunk size parameters.
const (
	// DefaultMinSize is the default minimum chunk size (16 KiB).
	DefaultMinSize = 16 * 1024
	// DefaultAvgSize is the default target average chunk size (64 KiB).
	DefaultAvgSize = 64 * 1024
	// DefaultMaxSize is the default maximum chunk size (256 KiB).
	DefaultMaxSize = 256 * 1024
)

const (
	// absoluteMinSize is the smallest minimum chunk size accepted. It keeps the
	// Gear hash window (64 bytes) meaningful.
	absoluteMinSize = 64
	// absoluteMaxSize bounds the buffer a Chunker allocates.
	absoluteMaxSize = 1 << 30
	// normalization is the normalization level: the number of mask bits added
	// below, and removed above, the average size.
	normalization = 2
	// maxEmptyReads is how many consecutive (0, nil) reads are tolerated
	// before the reader is deemed broken.
	maxEmptyReads = 100
)

// ErrInvalidConfig is returned when a Config violates its constraints.
var ErrInvalidConfig = errors.New("chunker: invalid config")

// Config holds the chunk size parameters, in bytes.
type Config struct {
	// MinSize is the smallest chunk produced, except possibly the last one.
	MinSize int
	// AvgSize is the target average chunk size. It is rounded to the nearest
	// power of two when deriving the cut masks.
	AvgSize int
	// MaxSize is the largest chunk produced.
	MaxSize int
}

// DefaultConfig returns the default 16 KiB / 64 KiB / 256 KiB configuration.
func DefaultConfig() Config {
	return Config{MinSize: DefaultMinSize, AvgSize: DefaultAvgSize, MaxSize: DefaultMaxSize}
}

// Validate reports whether c is usable. It requires
// 64 <= MinSize < AvgSize < MaxSize <= 1 GiB.
func (c Config) Validate() error {
	switch {
	case c.MinSize < absoluteMinSize:
		return fmt.Errorf("%w: min size %d is below %d", ErrInvalidConfig, c.MinSize, absoluteMinSize)
	case c.AvgSize <= c.MinSize:
		return fmt.Errorf("%w: avg size %d must exceed min size %d", ErrInvalidConfig, c.AvgSize, c.MinSize)
	case c.MaxSize <= c.AvgSize:
		return fmt.Errorf("%w: max size %d must exceed avg size %d", ErrInvalidConfig, c.MaxSize, c.AvgSize)
	case c.MaxSize > absoluteMaxSize:
		return fmt.Errorf("%w: max size %d exceeds %d", ErrInvalidConfig, c.MaxSize, absoluteMaxSize)
	}
	return nil
}

// gear is the table of pseudo-random 64-bit values used by the rolling hash.
// It is derived deterministically with splitmix64 so that chunk boundaries are
// stable across builds, platforms and releases. Changing it changes every
// boundary and therefore defeats deduplication against existing data.
var gear = func() (t [256]uint64) {
	const seed = 0x6368756e6b766c74 // "chunkvlt"
	state := uint64(seed)
	for i := range t {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return t
}()

// Chunk is a content-defined slice of the input stream.
type Chunk struct {
	// Offset is the position of the first byte of Data in the stream.
	Offset int64
	// Data holds the chunk bytes. It aliases the Chunker's internal buffer and
	// is only valid until the next call to Next.
	Data []byte
}

// Chunker splits an io.Reader into content-defined chunks. It buffers at most
// 2*MaxSize bytes regardless of the input length. A Chunker is not safe for
// concurrent use.
type Chunker struct {
	r      io.Reader
	cfg    Config
	maskS  uint64 // strict mask, used below the average size
	maskL  uint64 // loose mask, used above the average size
	buf    []byte
	start  int   // first unconsumed byte in buf
	end    int   // end of valid data in buf
	offset int64 // stream offset of buf[start]
	err    error // sticky read error (io.EOF once input is exhausted)
}

// New returns a Chunker reading from r. It returns an error wrapping
// ErrInvalidConfig if cfg is invalid.
func New(r io.Reader, cfg Config) (*Chunker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Round log2(avg) to the nearest integer.
	b := bits.Len(uint(cfg.AvgSize)) - 1
	if cfg.AvgSize-(1<<b) > (1<<(b+1))-cfg.AvgSize {
		b++
	}
	return &Chunker{
		r:     r,
		cfg:   cfg,
		maskS: topBitsMask(b + normalization),
		maskL: topBitsMask(b - normalization),
		buf:   make([]byte, 2*cfg.MaxSize),
	}, nil
}

// topBitsMask returns a mask with the n most significant bits set. The Gear
// hash shifts left each step, so its high bits mix the most history.
func topBitsMask(n int) uint64 {
	if n <= 0 {
		return 0
	}
	if n >= 64 {
		return ^uint64(0)
	}
	return ^uint64(0) << (64 - n)
}

// Next returns the next chunk. It returns io.EOF once the input is exhausted
// and any other error encountered while reading. The returned Chunk.Data is
// only valid until the following call to Next.
func (c *Chunker) Next() (Chunk, error) {
	c.fill()
	avail := c.end - c.start
	if avail == 0 {
		if c.err != nil {
			return Chunk{}, c.err
		}
		return Chunk{}, io.EOF
	}
	// A read error other than EOF is surfaced as soon as buffered data that
	// might be affected by it is needed; fully-buffered chunks are still
	// returned first so no valid data is lost.
	if c.err != nil && !errors.Is(c.err, io.EOF) && avail < c.cfg.MaxSize {
		return Chunk{}, c.err
	}
	n := c.cut(c.buf[c.start:c.end])
	ch := Chunk{Offset: c.offset, Data: c.buf[c.start : c.start+n : c.start+n]}
	c.start += n
	c.offset += int64(n)
	return ch, nil
}

// fill reads from the underlying reader until at least MaxSize bytes are
// buffered or the reader fails or ends. Always having MaxSize bytes available
// (unless at EOF) makes boundaries independent of how the reader fragments
// its data.
func (c *Chunker) fill() {
	if c.end-c.start >= c.cfg.MaxSize || c.err != nil {
		return
	}
	if c.start > 0 {
		c.end = copy(c.buf, c.buf[c.start:c.end])
		c.start = 0
	}
	empty := 0
	for c.end-c.start < c.cfg.MaxSize {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if err != nil {
			c.err = err
			return
		}
		if n > 0 {
			empty = 0
			continue
		}
		if empty++; empty >= maxEmptyReads {
			c.err = io.ErrNoProgress
			return
		}
	}
}

// cut returns the length of the first chunk in data, which holds at most
// 2*MaxSize bytes.
func (c *Chunker) cut(data []byte) int {
	n := len(data)
	if n <= c.cfg.MinSize {
		return n
	}
	if n > c.cfg.MaxSize {
		n = c.cfg.MaxSize
	}
	normal := c.cfg.AvgSize
	if n < normal {
		normal = n
	}
	var fp uint64
	i := c.cfg.MinSize
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
