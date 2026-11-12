package main

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runB is the benchmark equivalent of run in commands_test.go.
func runB(b *testing.B, stdin string, args ...string) (string, string, error) {
	b.Helper()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// BenchmarkPutGetRoundtrip measures end-to-end put+get throughput on a 20 MiB
// random file using the CLI entry points with a real filesystem.
func BenchmarkPutGetRoundtrip(b *testing.B) {
	dir := b.TempDir()
	repo := filepath.Join(dir, "repo")
	if _, _, err := runB(b, "", "init", repo); err != nil {
		b.Fatal(err)
	}

	data := make([]byte, 20<<20) // 20 MiB
	rand.New(rand.NewSource(42)).Read(data)
	src := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		b.Fatal(err)
	}
	dst := filepath.Join(dir, "restored.bin")

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := runB(b, "", "--repo", repo, "put", src); err != nil {
			b.Fatal(err)
		}
		if _, _, err := runB(b, "", "--repo", repo, "get", "source.bin", dst); err != nil {
			b.Fatal(err)
		}
		_ = os.Remove(dst)
	}
}

// BenchmarkPutDeduplicated measures the second put of a nearly-identical file —
// only the chunks near the edit change, so dedup should exceed 99%.
func BenchmarkPutDeduplicated(b *testing.B) {
	dir := b.TempDir()
	repo := filepath.Join(dir, "repo")
	if _, _, err := runB(b, "", "init", repo); err != nil {
		b.Fatal(err)
	}

	data := make([]byte, 20<<20)
	rand.New(rand.NewSource(42)).Read(data)
	src := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		b.Fatal(err)
	}
	if _, _, err := runB(b, "", "--repo", repo, "put", src); err != nil {
		b.Fatal(err)
	}

	// Modified copy: insert 20 bytes at the midpoint.
	modified := make([]byte, 0, len(data)+20)
	modified = append(modified, data[:len(data)/2]...)
	modified = append(modified, make([]byte, 20)...)
	modified = append(modified, data[len(data)/2:]...)
	src2 := filepath.Join(dir, "modified.bin")
	if err := os.WriteFile(src2, modified, 0o644); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(modified)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _, err := runB(b, "", "--repo", repo, "put", src2, "--name", "source.bin")
		if err != nil {
			b.Fatal(err)
		}
		if !strings.Contains(out, "deduplicated:") {
			b.Fatalf("unexpected put output: %s", out)
		}
	}
}
