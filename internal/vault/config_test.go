package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/karthik-258-code/Chunk-Vault/internal/chunker"
	"github.com/karthik-258-code/Chunk-Vault/internal/codec"
	"github.com/karthik-258-code/Chunk-Vault/internal/keys"
)

const testPass = "correct horse battery staple"

// fastKDF keeps Argon2id cheap in tests.
func fastKDF() keys.KDFParams {
	return keys.KDFParams{Algorithm: keys.AlgorithmArgon2id, Time: 1, MemoryKiB: 64, Threads: 1}
}

func passFn(p string) Option {
	return WithPassphrase(func() ([]byte, error) { return []byte(p), nil })
}

// initEncrypted creates an encrypted repository and opens it.
func initEncrypted(t *testing.T, opts ...Option) (*Vault, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := Init(dir, InitEncrypted([]byte(testPass)), InitKDFParams(fastKDF())); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir, append([]Option{WithChunkerConfig(testCfg), passFn(testPass)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, dir
}

func readConfigFile(t *testing.T, dir string) (Config, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, raw
}

func TestInitWritesConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readConfigFile(t, dir)
	if cfg.FormatVersion != ConfigVersion || !cfg.Compression || cfg.Encryption || cfg.Key != nil {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.chunkerConfig() != chunker.DefaultConfig() {
		t.Fatalf("chunker = %+v", cfg.Chunker)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyMetaFile)); err == nil {
		t.Fatal("Init wrote the legacy marker file")
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Encrypted() {
		t.Fatal("unencrypted repository reports Encrypted")
	}
}

func TestInitEncryptedConfigHasNoSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := Init(dir, InitEncrypted([]byte(testPass)), InitKDFParams(fastKDF())); err != nil {
		t.Fatal(err)
	}
	cfg, raw := readConfigFile(t, dir)
	if !cfg.Encryption || cfg.Key == nil {
		t.Fatalf("config = %+v", cfg)
	}
	if err := cfg.Key.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Key.KDF != fastKDF() || len(cfg.Key.Salt) != keys.SaltSize {
		t.Fatalf("key = %+v", cfg.Key)
	}
	if strings.Contains(string(raw), testPass) {
		t.Fatal("config contains the passphrase")
	}
	master, err := cfg.Key.Unwrap([]byte(testPass))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := master.Derive()
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string][]byte{"master": master[:], "enc": sub.Enc, "mac": sub.Mac} {
		for _, enc := range [][]byte{secret, []byte(hexOf(secret))} {
			if bytes.Contains(raw, enc) {
				t.Fatalf("config contains the raw %s key", name)
			}
		}
	}
	v, err := Open(dir, passFn(testPass))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if !v.Encrypted() {
		t.Fatal("encrypted repository reports !Encrypted")
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func TestOpenEncryptedWrongPassphrase(t *testing.T) {
	_, dir := initEncrypted(t)
	_, err := Open(dir, passFn("not it"))
	if !errors.Is(err, keys.ErrWrongPassphrase) {
		t.Fatalf("Open = %v, want ErrWrongPassphrase", err)
	}
	if !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("error %q is not clear about the cause", err)
	}
}

func TestOpenEncryptedNeedsPassphrase(t *testing.T) {
	_, dir := initEncrypted(t)
	if _, err := Open(dir); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("Open without passphrase source = %v", err)
	}
	want := errors.New("no tty")
	_, err := Open(dir, WithPassphrase(func() ([]byte, error) { return nil, want }))
	if !errors.Is(err, want) {
		t.Fatalf("Open = %v, want the passphrase source's error", err)
	}
}

func TestOpenUnencryptedIgnoresPassphraseSource(t *testing.T) {
	v, dir := newVault(t)
	mustPut(t, v, "f", []byte("data"))
	v.Close()
	called := false
	v2, err := Open(dir, WithPassphrase(func() ([]byte, error) { called = true; return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if called {
		t.Fatal("asked for a passphrase for an unencrypted repository")
	}
	if got := mustGet(t, v2, "f"); string(got) != "data" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenLegacyRepository(t *testing.T) {
	dir := t.TempDir()
	// An M1 repository: marker file, no config.json.
	if err := os.WriteFile(filepath.Join(dir, legacyMetaFile), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrLegacyRepo) {
		t.Fatalf("Open = %v, want ErrLegacyRepo", err)
	}
	if errors.Is(err, ErrNotInitialized) || !strings.Contains(err.Error(), "init") {
		t.Fatalf("error %q should tell the user to re-init", err)
	}
	if err := Init(dir); err == nil {
		t.Fatal("Init overwrote a legacy repository")
	}
}

func TestOpenInvalidConfig(t *testing.T) {
	_, dir := initEncrypted(t)
	good, _ := readConfigFile(t, dir)
	mutate := func(f func(c *Config)) Config {
		c := good
		k := *good.Key
		c.Key = &k
		f(&c)
		return c
	}
	tests := map[string]Config{
		"bad version":    mutate(func(c *Config) { c.FormatVersion = 9 }),
		"bad chunker":    mutate(func(c *Config) { c.Chunker.MinSize = 1 }),
		"no key":         mutate(func(c *Config) { c.Key = nil }),
		"stray key":      mutate(func(c *Config) { c.Encryption = false }),
		"bad kdf":        mutate(func(c *Config) { c.Key.KDF.Time = 0 }),
		"short wrapped":  mutate(func(c *Config) { c.Key.WrappedKey = c.Key.WrappedKey[:10] }),
		"short salt":     mutate(func(c *Config) { c.Key.Salt = c.Key.Salt[:3] }),
		"unknown kdf":    mutate(func(c *Config) { c.Key.KDF.Algorithm = "md5" }),
		"huge kdf mem":   mutate(func(c *Config) { c.Key.KDF.MemoryKiB = 1 << 31 }),
		"zero threads":   mutate(func(c *Config) { c.Key.KDF.Threads = 0 }),
		"zero chunker":   mutate(func(c *Config) { c.Chunker = ChunkerParams{} }),
		"huge max chunk": mutate(func(c *Config) { c.Chunker.MaxSize = 1 << 40 }),
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, configFile), data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, passFn(testPass)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open = %v, want ErrCorrupt", err)
			}
		})
	}
	for name, raw := range map[string]string{"not json": "{", "empty": ""} {
		if err := os.WriteFile(filepath.Join(dir, configFile), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, passFn(testPass)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Open = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestInitRejectsBadOptions(t *testing.T) {
	base := t.TempDir()
	tests := map[string][]InitOption{
		"empty passphrase": {InitEncrypted(nil), InitKDFParams(fastKDF())},
		"bad kdf":          {InitEncrypted([]byte("x")), InitKDFParams(keys.KDFParams{})},
		"bad chunker":      {InitChunkerConfig(chunker.Config{MinSize: 1, AvgSize: 2, MaxSize: 3})},
	}
	for name, opts := range tests {
		dir := filepath.Join(base, strings.ReplaceAll(name, " ", "-"))
		if err := Init(dir, opts...); err == nil {
			t.Errorf("%s: Init succeeded", name)
		}
		if _, err := os.Stat(filepath.Join(dir, configFile)); err == nil {
			t.Errorf("%s: Init left a config.json behind", name)
		}
	}
}

func TestInitOptionsRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	cc := chunker.Config{MinSize: 128, AvgSize: 512, MaxSize: 2048}
	if err := Init(dir, InitChunkerConfig(cc), InitCompression(false)); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.cfg != cc {
		t.Fatalf("chunker config = %+v, want %+v", v.cfg, cc)
	}
	// Without compression no blob carries the compressed flag, even though
	// the input is highly compressible.
	data := bytes.Repeat([]byte("abcdefgh"), 4000)
	for _, h := range chunkHashes(t, v, mustPut(t, v, "f", data)) {
		raw, err := os.ReadFile(filepath.Join(dir, "objects", h[:2], h[2:4], h))
		if err != nil {
			t.Fatal(err)
		}
		if raw[len(codec.Magic)+1]&codec.FlagCompressed != 0 {
			t.Fatalf("chunk %s is compressed although compression is off", h)
		}
	}
	if got := mustGet(t, v, "f"); !bytes.Equal(got, data) {
		t.Fatal("round trip failed")
	}
}

func TestNewRejectsNilCodec(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("New accepted a nil codec")
	}
}

func TestEncryptedRepoCannotBeOpenedAsPlain(t *testing.T) {
	// Replacing the config with an unencrypted one must not make the blobs
	// readable: the codec rejects the encryption flag mismatch.
	v, dir := initEncrypted(t)
	mustPut(t, v, "f", randomBytes(30, 10_000))
	plain := t.TempDir()
	if err := Init(plain); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readConfigFile(t, plain)
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, configFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	v.Close()
	v2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if err := v2.Get(context.Background(), "f", &bytes.Buffer{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
}
