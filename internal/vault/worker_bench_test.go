package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
)

func BenchmarkPutParallel(b *testing.B) {
	dir := b.TempDir()
	if err := Init(dir); err != nil {
		b.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer v.Close()

	data := make([]byte, 64<<20) // 64 MiB
	if _, err := rand.Read(data); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.Put(context.Background(), "bench", bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
