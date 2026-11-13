package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/index"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/keys"
)

// run executes the CLI with args and returns stdout, stderr and the error.
func run(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func mustRun(t *testing.T, stdin string, args ...string) (string, string) {
	t.Helper()
	out, errs, err := run(t, stdin, args...)
	if err != nil {
		t.Fatalf("chunkvault %v: %v\nstdout: %s\nstderr: %s", args, err, out, errs)
	}
	return out, errs
}

func writeRandomFile(t *testing.T, path string, seed int64, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(data)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	src := filepath.Join(dir, "input.bin")
	data := writeRandomFile(t, src, 1, 1<<20)

	out, _ := mustRun(t, "", "init", repo)
	if !strings.Contains(out, "Initialized") {
		t.Errorf("init output = %q", out)
	}

	out, _ = mustRun(t, "", "--repo", repo, "put", src)
	for _, want := range []string{`"input.bin"`, "total chunks:", "new chunks:", "deduplicated:  0 chunks"} {
		if !strings.Contains(out, want) {
			t.Errorf("first put output missing %q:\n%s", want, out)
		}
	}

	// Same content under another name: everything deduplicates.
	out, _ = mustRun(t, "", "put", src, "--name", "copy.bin", "--repo", repo)
	if !strings.Contains(out, "new chunks:    0 (0 B)") || !strings.Contains(out, "100.0%") {
		t.Errorf("second put should fully deduplicate:\n%s", out)
	}

	restored := filepath.Join(dir, "restored.bin")
	_, errs := mustRun(t, "", "--repo", repo, "get", "copy.bin", restored)
	if !strings.Contains(errs, "Restored") {
		t.Errorf("get stderr = %q", errs)
	}
	got, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored file differs (err %v)", err)
	}

	out, _ = mustRun(t, "", "--repo", repo, "ls")
	for _, want := range []string{"NAME", "input.bin", "copy.bin", "1.0 MiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q:\n%s", want, out)
		}
	}
}

func TestGCCommand(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustRun(t, "", "init", repo)

	// Put a file, remove it, then gc.
	src := filepath.Join(dir, "f.bin")
	writeRandomFile(t, src, 42, 256*1024)
	mustRun(t, "", "--repo", repo, "put", src)
	mustRun(t, "", "--repo", repo, "rm", "f.bin")

	// Dry run reports reclaimable chunks without deleting.
	out, _ := mustRun(t, "", "--repo", repo, "gc", "--dry-run")
	if !strings.Contains(out, "Would collect") {
		t.Fatalf("dry-run gc output = %q", out)
	}

	// Real gc frees the space.
	out, _ = mustRun(t, "", "--repo", repo, "gc")
	if !strings.Contains(out, "Collected") {
		t.Fatalf("gc output = %q", out)
	}

	// A second gc run should collect nothing.
	out, _ = mustRun(t, "", "--repo", repo, "gc")
	if !strings.Contains(out, "0 chunks") {
		t.Fatalf("second gc output = %q", out)
	}
}

func TestPutAndGetViaStdio(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	mustRun(t, "", "init", repo)
	mustRun(t, "piped content", "--repo", repo, "put", "-", "--name", "piped")
	out, _ := mustRun(t, "", "--repo", repo, "get", "piped", "-")
	if out != "piped content" {
		t.Fatalf("stdout = %q", out)
	}
	if _, _, err := run(t, "x", "--repo", repo, "put", "-"); err == nil {
		t.Fatal("put - without --name should fail")
	}
}

func TestDefaultRepoFlag(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	mustRun(t, "", "init", defaultRepo)
	if _, err := os.Stat(filepath.Join(dir, ".chunkvault", "config.json")); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "hello", "put", "-", "--name", "h")
	out, _ := mustRun(t, "", "ls")
	if !strings.Contains(out, "h ") {
		t.Fatalf("ls = %q", out)
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	notRepo := filepath.Join(dir, "none")
	src := filepath.Join(dir, "f")
	writeRandomFile(t, src, 2, 100)
	mustRun(t, "", "init", repo)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"init twice", []string{"init", repo}, "already exists"},
		{"put without repo", []string{"--repo", notRepo, "put", src}, "not initialized"},
		{"get without repo", []string{"--repo", notRepo, "get", "x", filepath.Join(dir, "o")}, "not initialized"},
		{"ls without repo", []string{"--repo", notRepo, "ls"}, "not initialized"},
		{"put missing file", []string{"--repo", repo, "put", filepath.Join(dir, "missing")}, "no such file"},
		{"put directory", []string{"--repo", repo, "put", dir}, "is a directory"},
		{"get unknown name", []string{"--repo", repo, "get", "ghost", filepath.Join(dir, "o")}, "not found"},
		{"init no args", []string{"init"}, "accepts 1 arg"},
		{"get one arg", []string{"--repo", repo, "get", "x"}, "accepts 2 arg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "o")); err == nil {
		t.Fatal("failed get left an output file behind")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, ".chunkvault-get-*")); len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestGetFailureKeepsExistingOutput(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustRun(t, "", "init", repo)
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(out, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "", "--repo", repo, "get", "ghost", out); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := os.ReadFile(out); string(got) != "precious" {
		t.Fatalf("output clobbered: %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB",
		1 << 20: "1.0 MiB", 5 << 30: "5.0 GiB", 3 << 40: "3.0 TiB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	out, _ := mustRun(t, "", "--version")
	if !strings.Contains(out, version) {
		t.Fatalf("--version output = %q", out)
	}
}

func TestGetPreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not supported on Windows")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustRun(t, "", "init", repo)
	mustRun(t, "contents", "--repo", repo, "put", "-", "--name", "f")

	out := filepath.Join(dir, "out")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o600); err != nil { // defeat the umask
		t.Fatal(err)
	}
	mustRun(t, "", "--repo", repo, "get", "f", out)
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 preserved", st.Mode().Perm())
	}
	if got, _ := os.ReadFile(out); string(got) != "contents" {
		t.Fatalf("content = %q", got)
	}
}

func TestGetNewFileModeIs0644(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not supported on Windows")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustRun(t, "", "init", repo)
	mustRun(t, "contents", "--repo", repo, "put", "-", "--name", "f")

	out := filepath.Join(dir, "new")
	mustRun(t, "", "--repo", repo, "get", "f", out)
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", st.Mode().Perm())
	}
}

// useFastKDF keeps Argon2id cheap for the CLI tests.
func useFastKDF(t *testing.T) {
	t.Helper()
	old := kdfParams
	kdfParams = keys.KDFParams{Algorithm: keys.AlgorithmArgon2id, Time: 1, MemoryKiB: 64, Threads: 1}
	t.Cleanup(func() { kdfParams = old })
}

// fakeTerminal makes stdin look like a terminal that types the given lines at
// successive no-echo prompts, and returns how many prompts were shown.
func fakeTerminal(t *testing.T, lines ...string) *int {
	t.Helper()
	oldFD, oldRead := terminalFD, readPassword
	t.Cleanup(func() { terminalFD, readPassword = oldFD, oldRead })
	prompts := 0
	terminalFD = func(io.Reader) (int, bool) { return 0, true }
	readPassword = func(int) ([]byte, error) {
		if prompts >= len(lines) {
			return nil, errors.New("unexpected extra prompt")
		}
		prompts++
		return []byte(lines[prompts-1]), nil
	}
	return &prompts
}

func setPass(t *testing.T, p string) { t.Helper(); t.Setenv(passphraseEnv, p) }

func TestInitEncryptWithEnvPassphrase(t *testing.T) {
	useFastKDF(t)
	setPass(t, "env pass")
	repo := filepath.Join(t.TempDir(), "repo")
	out, _ := mustRun(t, "", "init", "--encrypt", repo)
	if !strings.Contains(out, "encrypted") {
		t.Errorf("init output = %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"encryption": true`, `"compression": true`, `"salt"`, `"wrapped_key"`, `"kdf"`, `"format_version": 1`, `"min_size"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config.json missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "env pass") {
		t.Fatal("config.json contains the passphrase")
	}
}

func TestInitPlainNeedsNoPassphrase(t *testing.T) {
	setPass(t, "")
	repo := filepath.Join(t.TempDir(), "repo")
	out, _ := mustRun(t, "", "init", repo)
	if !strings.Contains(out, "unencrypted") {
		t.Errorf("init output = %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"encryption": false`) || strings.Contains(string(raw), "wrapped_key") {
		t.Fatalf("config.json = %s", raw)
	}
}

func TestInitEncryptPromptsTwice(t *testing.T) {
	useFastKDF(t)
	setPass(t, "")
	prompts := fakeTerminal(t, "hunter2", "hunter2")
	repo := filepath.Join(t.TempDir(), "repo")
	_, errs := mustRun(t, "", "init", "--encrypt", repo)
	if *prompts != 2 {
		t.Fatalf("prompted %d times, want 2 (passphrase and confirmation)", *prompts)
	}
	if !strings.Contains(errs, "Confirm passphrase") || strings.Contains(errs, "hunter2") {
		t.Fatalf("stderr = %q", errs)
	}

	// The prompt is used again, once, to open the repository.
	src := filepath.Join(t.TempDir(), "f")
	writeRandomFile(t, src, 5, 1000)
	fakeTerminal(t, "hunter2")
	mustRun(t, "", "--repo", repo, "put", src)
}

func TestInitEncryptPassphraseErrors(t *testing.T) {
	useFastKDF(t)
	setPass(t, "")
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"mismatch", []string{"one", "two"}, "do not match"},
		{"empty", []string{""}, "must not be empty"},
		{"read failure", nil, "read passphrase"},
		{"confirm read failure", []string{"one"}, "read passphrase"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeTerminal(t, tc.lines...)
			repo := filepath.Join(t.TempDir(), "repo")
			_, _, err := run(t, "", "init", "--encrypt", repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(repo, "config.json")); err == nil {
				t.Fatal("failed init left a config.json behind")
			}
		})
	}
}

func TestEncryptWithoutPassphraseSourceFails(t *testing.T) {
	setPass(t, "")
	repo := filepath.Join(t.TempDir(), "repo")
	// stdin is not a terminal and the variable is unset: never read a
	// passphrase from piped data.
	_, _, err := run(t, "piped-passphrase\npiped-passphrase\n", "init", "--encrypt", repo)
	if err == nil || !strings.Contains(err.Error(), passphraseEnv) {
		t.Fatalf("err = %v, want a message naming %s", err, passphraseEnv)
	}
}

func TestWrongPassphraseIsClear(t *testing.T) {
	useFastKDF(t)
	repo := filepath.Join(t.TempDir(), "repo")
	setPass(t, "right")
	mustRun(t, "", "init", "--encrypt", repo)

	setPass(t, "wrong")
	for _, args := range [][]string{
		{"--repo", repo, "ls"},
		{"--repo", repo, "get", "x", "-"},
		{"--repo", repo, "put", "-", "--name", "x"},
	} {
		_, _, err := run(t, "data", args...)
		if err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
			t.Errorf("%v: err = %v, want a wrong passphrase error", args, err)
		}
	}

	setPass(t, "")
	if _, _, err := run(t, "", "--repo", repo, "ls"); err == nil || !strings.Contains(err.Error(), passphraseEnv) {
		t.Errorf("ls without passphrase: err = %v", err)
	}
}

func TestLegacyRepoFailsToOpen(t *testing.T) {
	repo := t.TempDir()
	// An M1 repository only has the old marker file.
	if err := os.WriteFile(filepath.Join(repo, "chunkvault.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"ls"}, {"get", "x", "-"}, {"put", "-", "--name", "x"}} {
		_, _, err := run(t, "x", append([]string{"--repo", repo}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "old pre-config format") || !strings.Contains(err.Error(), "chunkvault init") {
			t.Errorf("%v: err = %v, want a message telling the user to re-init", args, err)
		}
	}
}

// bothModes runs fn for an unencrypted and an encrypted repository.
func bothModes(t *testing.T, fn func(t *testing.T, repo string)) {
	t.Run("plain", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "repo")
		mustRun(t, "", "init", repo)
		fn(t, repo)
	})
	t.Run("encrypted", func(t *testing.T) {
		useFastKDF(t)
		setPass(t, "e2e pass")
		repo := filepath.Join(t.TempDir(), "repo")
		mustRun(t, "", "init", "--encrypt", repo)
		fn(t, repo)
	})
}

func TestEndToEndBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		src := filepath.Join(dir, "input.bin")
		data := writeRandomFile(t, src, 11, 1<<20)

		out, _ := mustRun(t, "", "--repo", repo, "put", src)
		if !strings.Contains(out, "deduplicated:  0 chunks") {
			t.Errorf("first put output:\n%s", out)
		}
		restored := filepath.Join(dir, "restored.bin")
		mustRun(t, "", "--repo", repo, "get", "input.bin", restored)
		if got, err := os.ReadFile(restored); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("restored file differs (err %v)", err)
		}
		out, _ = mustRun(t, "", "--repo", repo, "get", "input.bin", "-")
		if out != string(data) {
			t.Fatal("stdout restore differs")
		}
		out, _ = mustRun(t, "", "--repo", repo, "ls")
		if !strings.Contains(out, "input.bin") || !strings.Contains(out, "1.0 MiB") {
			t.Errorf("ls output:\n%s", out)
		}
	})
}

func TestDedupBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		src := filepath.Join(t.TempDir(), "input.bin")
		writeRandomFile(t, src, 12, 1<<20)
		mustRun(t, "", "--repo", repo, "put", src)
		out, _ := mustRun(t, "", "--repo", repo, "put", src, "--name", "copy.bin")
		if !strings.Contains(out, "new chunks:    0 (0 B)") || !strings.Contains(out, "100.0%") {
			t.Errorf("second put should fully deduplicate:\n%s", out)
		}
	})
}

func TestTamperedBlobFailsGetBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		src := filepath.Join(dir, "input.bin")
		writeRandomFile(t, src, 13, 300_000)
		mustRun(t, "", "--repo", repo, "put", src)

		// Corrupt every blob in turn that is not the manifest: flip one byte
		// in the middle of the largest blob, a chunk.
		var victim string
		var size int64
		err := filepath.Walk(filepath.Join(repo, "objects"), func(p string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() && info.Size() > size {
				victim, size = p, info.Size()
			}
			return err
		})
		if err != nil || victim == "" {
			t.Fatalf("no blobs found: %v", err)
		}
		raw, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)/2] ^= 0xff
		if err := os.Chmod(victim, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(victim, raw, 0o644); err != nil {
			t.Fatal(err)
		}

		out := filepath.Join(dir, "restored.bin")
		_, _, err = run(t, "", "--repo", repo, "get", "input.bin", out)
		if err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("get of a tampered repository: err = %v, want a corruption error", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("failed get left an output file behind")
		}
	})
}

func TestEncryptedRepoHidesPlaintext(t *testing.T) {
	useFastKDF(t)
	setPass(t, "pw")
	repo := filepath.Join(t.TempDir(), "repo")
	mustRun(t, "", "init", "--encrypt", repo)
	marker := strings.Repeat("PLAINTEXT-MARKER ", 200)
	mustRun(t, marker, "--repo", repo, "put", "-", "--name", "doc")
	err := filepath.Walk(repo, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(raw, []byte("PLAINTEXT-MARKER")) {
			t.Errorf("%s contains plaintext", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dumpIndex returns the files and the referenced chunks of the index in repo.
// Chunks nothing references are left out: an index maintained incrementally
// keeps them until garbage collection, a rebuilt one never learns of them. No
// chunkvault command may be running.
func dumpIndex(t *testing.T, repo string) (files []index.File, chunks []index.ChunkEntry) {
	t.Helper()
	x, err := index.Open(filepath.Join(repo, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if err := x.ForEachFile(func(f index.File) error { files = append(files, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := x.ForEachChunk(func(c index.ChunkEntry) error {
		if c.Refs > 0 {
			chunks = append(chunks, c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files, chunks
}

// totalRefs sums the reference counts of all chunks in the index in repo,
// including chunks that end up with none.
func totalRefs(t *testing.T, repo string) (refs uint64, zeroRef int) {
	t.Helper()
	x, err := index.Open(filepath.Join(repo, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if err := x.ForEachChunk(func(c index.ChunkEntry) error {
		refs += c.Refs
		if c.Refs == 0 {
			zeroRef++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return refs, zeroRef
}

func blobCount(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(filepath.Join(repo, "objects"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInitCreatesIndexBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		if _, err := os.Stat(filepath.Join(repo, "index.db")); err != nil {
			t.Fatalf("init did not create index.db: %v", err)
		}
	})
}

func TestRmBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		shared := filepath.Join(dir, "shared.bin")
		data := writeRandomFile(t, shared, 21, 400_000)
		other := filepath.Join(dir, "other.bin")
		writeRandomFile(t, other, 22, 200_000)

		mustRun(t, "", "--repo", repo, "put", shared)
		mustRun(t, "", "--repo", repo, "put", shared, "--name", "copy.bin")
		mustRun(t, "", "--repo", repo, "put", other)
		files, chunks := dumpIndex(t, repo)
		if len(files) != 3 {
			t.Fatalf("index has %d files, want 3", len(files))
		}
		refs, zero := totalRefs(t, repo)
		var want uint64
		for _, f := range files {
			want += uint64(f.ChunkCount)
		}
		if refs != want || zero != 0 {
			t.Fatalf("total refs = %d (%d unreferenced), files hold %d chunk occurrences", refs, zero, want)
		}
		for _, c := range chunks {
			if c.Refs < 1 || c.StoredSize <= 0 || c.RawSize <= 0 {
				t.Fatalf("bad chunk entry %+v", c)
			}
		}
		blobs := blobCount(t, repo)

		// rm one of two names for the same content: the data stays readable.
		out, _ := mustRun(t, "", "--repo", repo, "rm", "copy.bin")
		if !strings.Contains(out, `Removed "copy.bin"`) || !strings.Contains(out, "garbage collection") {
			t.Errorf("rm output = %q", out)
		}
		got, _ := mustRun(t, "", "--repo", repo, "get", "shared.bin", "-")
		if got != string(data) {
			t.Fatal("rm of a copy damaged the original")
		}
		files, _ = dumpIndex(t, repo)
		if len(files) != 2 {
			t.Fatalf("index has %d files after rm, want 2", len(files))
		}
		if _, zero := totalRefs(t, repo); zero != 0 {
			t.Fatalf("%d chunks unreferenced, but shared.bin still uses them", zero)
		}

		// rm the file that owns chunks alone: they drop to zero but stay on disk.
		mustRun(t, "", "--repo", repo, "rm", "other.bin")
		if _, zero := totalRefs(t, repo); zero == 0 {
			t.Fatal("no unreferenced chunks after removing other.bin")
		}
		if got := blobCount(t, repo); got != blobs {
			t.Fatalf("rm deleted blobs (%d -> %d), they stay until garbage collection", blobs, got)
		}

		// get after rm fails cleanly, and leaves nothing behind.
		restored := filepath.Join(dir, "restored.bin")
		_, _, err := run(t, "", "--repo", repo, "get", "other.bin", restored)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("get after rm: err = %v, want not found", err)
		}
		if _, err := os.Stat(restored); err == nil {
			t.Fatal("failed get left an output file behind")
		}
		out, _ = mustRun(t, "", "--repo", repo, "ls")
		if strings.Contains(out, "other.bin") || strings.Contains(out, "copy.bin") || !strings.Contains(out, "shared.bin") {
			t.Errorf("ls after rm:\n%s", out)
		}

		// rm again, and rm of an unknown name, fail.
		for _, name := range []string{"other.bin", "never-existed"} {
			_, _, err = run(t, "", "--repo", repo, "rm", name)
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Errorf("rm %s: err = %v, want not found", name, err)
			}
		}
		if _, _, err := run(t, "", "--repo", repo, "rm"); err == nil || !strings.Contains(err.Error(), "accepts 1 arg") {
			t.Errorf("rm without a name: err = %v", err)
		}
	})
}

func TestOverwriteKeepsIndexCorrectBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		v1 := writeRandomFile(t, filepath.Join(dir, "v1"), 23, 500_000)
		v2 := append(append([]byte(nil), v1[:250_000]...), make([]byte, 100_000)...)
		v2path := filepath.Join(dir, "v2")
		if err := os.WriteFile(v2path, v2, 0o644); err != nil {
			t.Fatal(err)
		}

		mustRun(t, "", "--repo", repo, "put", filepath.Join(dir, "v1"), "--name", "f")
		mustRun(t, "", "--repo", repo, "put", v2path, "--name", "f")

		files, _ := dumpIndex(t, repo)
		if len(files) != 1 || files[0].Name != "f" || files[0].Size != int64(len(v2)) {
			t.Fatalf("index files = %+v, want only f at the new size %d", files, len(v2))
		}
		refs, zero := totalRefs(t, repo)
		if refs != uint64(files[0].ChunkCount) {
			t.Fatalf("total refs = %d, the one file has %d chunk occurrences", refs, files[0].ChunkCount)
		}
		if zero == 0 {
			t.Fatal("the tail of the old version should be unreferenced")
		}
		if got, _ := mustRun(t, "", "--repo", repo, "get", "f", "-"); got != string(v2) {
			t.Fatal("get returned the old version")
		}
	})
}

func TestReindexMatchesIncrementalIndexBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.bin")
		writeRandomFile(t, a, 24, 300_000)
		b := filepath.Join(dir, "b.bin")
		writeRandomFile(t, b, 25, 150_000)
		mustRun(t, "", "--repo", repo, "put", a)
		mustRun(t, "", "--repo", repo, "put", a, "--name", "dir/a2.bin")
		mustRun(t, "", "--repo", repo, "put", b)
		mustRun(t, "", "--repo", repo, "put", b, "--name", "a.bin") // overwrite
		mustRun(t, "", "--repo", repo, "put", "-", "--name", "empty")
		mustRun(t, "", "--repo", repo, "rm", "b.bin")

		wantFiles, wantChunks := dumpIndex(t, repo)
		if len(wantFiles) != 3 {
			t.Fatalf("setup: %d files, want 3", len(wantFiles))
		}

		out, _ := mustRun(t, "", "--repo", repo, "reindex")
		wantOut := fmt.Sprintf("Reindexed %s: %d files, %d chunks\n", repo, len(wantFiles), len(wantChunks))
		if out != wantOut {
			t.Fatalf("reindex output = %q, want %q", out, wantOut)
		}
		gotFiles, gotChunks := dumpIndex(t, repo)
		if !reflect.DeepEqual(gotFiles, wantFiles) || !reflect.DeepEqual(gotChunks, wantChunks) {
			t.Fatalf("reindexed index differs from the incremental one\nfiles got  %+v\nfiles want %+v", gotFiles, wantFiles)
		}
		if _, zero := totalRefs(t, repo); zero != 0 {
			t.Fatalf("rebuilt index has %d unreferenced chunks", zero)
		}
		if tmps, _ := filepath.Glob(filepath.Join(repo, ".index-*")); len(tmps) != 0 {
			t.Fatalf("temporary index left behind: %v", tmps)
		}

		// The repository still works afterwards, and reindex is idempotent.
		if got, _ := mustRun(t, "", "--repo", repo, "get", "dir/a2.bin", "-"); len(got) != 300_000 {
			t.Fatalf("get after reindex returned %d bytes", len(got))
		}
		if out2, _ := mustRun(t, "", "--repo", repo, "reindex"); out2 != wantOut {
			t.Fatalf("second reindex output = %q", out2)
		}
	})
}

func TestReindexRepairsCrashBeforeIndexUpdateBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.bin")
		writeRandomFile(t, a, 26, 100_000)
		mustRun(t, "", "--repo", repo, "put", a)
		before, err := os.ReadFile(filepath.Join(repo, "index.db"))
		if err != nil {
			t.Fatal(err)
		}

		lostPath := filepath.Join(dir, "lost.bin")
		lost := writeRandomFile(t, lostPath, 27, 200_000)
		mustRun(t, "", "--repo", repo, "put", lostPath)
		mustRun(t, "", "--repo", repo, "rm", "a.bin")
		wantFiles, wantChunks := dumpIndex(t, repo)

		// Crash: the blobs, manifest and refs were written, the index update was
		// not. Put back the index from before the put and the rm.
		if err := os.WriteFile(filepath.Join(repo, "index.db"), before, 0o600); err != nil {
			t.Fatal(err)
		}
		files, _ := dumpIndex(t, repo)
		if len(files) != 1 || files[0].Name != "a.bin" {
			t.Fatalf("stale index files = %+v", files)
		}

		// The repository is still readable.
		if got, _ := mustRun(t, "", "--repo", repo, "get", "lost.bin", "-"); got != string(lost) {
			t.Fatal("file written before the crash is not readable")
		}
		out, _ := mustRun(t, "", "--repo", repo, "ls")
		if !strings.Contains(out, "lost.bin") || strings.Contains(out, "a.bin") {
			t.Fatalf("ls after the crash:\n%s", out)
		}

		mustRun(t, "", "--repo", repo, "reindex")
		gotFiles, gotChunks := dumpIndex(t, repo)
		if !reflect.DeepEqual(gotFiles, wantFiles) || !reflect.DeepEqual(gotChunks, wantChunks) {
			t.Fatalf("reindex did not repair the index\nfiles got  %+v\nfiles want %+v", gotFiles, wantFiles)
		}
	})
}

func TestMissingIndexNeedsReindexBothModes(t *testing.T) {
	bothModes(t, func(t *testing.T, repo string) {
		data := []byte("created before the index existed")
		mustRun(t, string(data), "--repo", repo, "put", "-", "--name", "old")
		wantFiles, wantChunks := dumpIndex(t, repo)
		if err := os.Remove(filepath.Join(repo, "index.db")); err != nil {
			t.Fatal(err)
		}

		// Every command that opens the repository says what to do, and none
		// creates an empty index on the way.
		for _, args := range [][]string{{"ls"}, {"get", "old", "-"}, {"put", "-", "--name", "x"}, {"rm", "old"}} {
			_, _, err := run(t, "x", append([]string{"--repo", repo}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "chunkvault reindex") {
				t.Errorf("%v: err = %v, want a message telling the user to run reindex", args, err)
			}
		}
		if _, err := os.Stat(filepath.Join(repo, "index.db")); err == nil {
			t.Fatal("a failed command created index.db")
		}

		out, _ := mustRun(t, "", "--repo", repo, "reindex")
		if !strings.Contains(out, "1 files") {
			t.Fatalf("reindex output = %q", out)
		}
		gotFiles, gotChunks := dumpIndex(t, repo)
		if !reflect.DeepEqual(gotFiles, wantFiles) || !reflect.DeepEqual(gotChunks, wantChunks) {
			t.Fatal("reindex of a missing index differs from the original")
		}
		if got, _ := mustRun(t, "", "--repo", repo, "get", "old", "-"); got != string(data) {
			t.Fatalf("get after reindex = %q", got)
		}
	})
}

func TestReindexErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "", "--repo", filepath.Join(dir, "none"), "reindex"); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("reindex without a repo: err = %v", err)
	}
	if _, _, err := run(t, "", "--repo", filepath.Join(dir, "none"), "rm", "x"); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("rm without a repo: err = %v", err)
	}
	if _, _, err := run(t, "", "reindex", "extra"); err == nil {
		t.Fatal("reindex accepted an argument")
	}

	// A wrong passphrase must not replace the index.
	useFastKDF(t)
	repo := filepath.Join(dir, "enc")
	setPass(t, "right")
	mustRun(t, "", "init", "--encrypt", repo)
	mustRun(t, "x", "--repo", repo, "put", "-", "--name", "f")
	setPass(t, "wrong")
	if _, _, err := run(t, "", "--repo", repo, "reindex"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("reindex with the wrong passphrase: err = %v", err)
	}
	if _, _, err := run(t, "", "--repo", repo, "rm", "f"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("rm with the wrong passphrase: err = %v", err)
	}
}

func TestRmAndReindexHelp(t *testing.T) {
	out, _ := mustRun(t, "", "rm", "--help")
	if !strings.Contains(out, "chunkvault gc") || !strings.Contains(out, "stays on disk") {
		t.Errorf("rm --help does not say blobs stay until garbage collection:\n%s", out)
	}
	out, _ = mustRun(t, "", "reindex", "--help")
	if !strings.Contains(out, "index.db") {
		t.Errorf("reindex --help:\n%s", out)
	}
}
