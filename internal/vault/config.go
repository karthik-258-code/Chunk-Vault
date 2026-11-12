package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/chunker"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/codec"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/keys"
)

const (
	// configFile is the repository configuration written by Init.
	configFile = "config.json"
	// legacyMetaFile is the marker file of the M1 repository format, which
	// stored raw, unversioned blobs and has no configuration file.
	legacyMetaFile = "chunkvault.json"
	// ConfigVersion is the repository format version written by Init.
	ConfigVersion = 1
)

// ChunkerParams are the chunker sizes recorded in a repository's config.
type ChunkerParams struct {
	// MinSize is the smallest chunk produced, except possibly the last one.
	MinSize int `json:"min_size"`
	// AvgSize is the target average chunk size.
	AvgSize int `json:"avg_size"`
	// MaxSize is the largest chunk produced.
	MaxSize int `json:"max_size"`
}

// Config is the persisted repository configuration, stored as config.json.
//
// It never contains the passphrase or any raw key: for an encrypted repository
// it holds only the Argon2id salt and parameters and the wrapped master key.
type Config struct {
	// FormatVersion is the repository format version, see ConfigVersion.
	FormatVersion int `json:"format_version"`
	// Chunker holds the chunker parameters new files are split with.
	Chunker ChunkerParams `json:"chunker"`
	// Compression reports whether blobs are zstd compressed.
	Compression bool `json:"compression"`
	// Encryption reports whether blobs are encrypted with AES-256-GCM.
	Encryption bool `json:"encryption"`
	// Key is the salt, KDF parameters and wrapped master key. It is present if
	// and only if Encryption is set.
	Key *keys.Wrapped `json:"key,omitempty"`
}

// chunkerConfig returns the chunker configuration recorded in c.
func (c Config) chunkerConfig() chunker.Config {
	return chunker.Config{MinSize: c.Chunker.MinSize, AvgSize: c.Chunker.AvgSize, MaxSize: c.Chunker.MaxSize}
}

// validate checks the structure of c without needing the passphrase.
func (c Config) validate() error {
	if c.FormatVersion != ConfigVersion {
		return fmt.Errorf("unsupported format version %d (this build reads version %d)", c.FormatVersion, ConfigVersion)
	}
	if err := c.chunkerConfig().Validate(); err != nil {
		return err
	}
	switch {
	case c.Encryption && c.Key == nil:
		return errors.New("encryption is enabled but the config has no key material")
	case !c.Encryption && c.Key != nil:
		return errors.New("config has key material but encryption is disabled")
	case c.Key != nil:
		return c.Key.Validate()
	}
	return nil
}

// readConfig loads and validates the config of the repository in dir. It
// returns ErrLegacyRepo for an M1 repository and ErrNotInitialized if dir is
// not a repository at all.
func readConfig(dir string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("vault: read config: %w", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, legacyMetaFile)); serr == nil {
			return cfg, fmt.Errorf("%w: %s", ErrLegacyRepo, dir)
		}
		return cfg, fmt.Errorf("%w: %s (run 'chunkvault init')", ErrNotInitialized, dir)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: %s: invalid config: %w", ErrCorrupt, dir, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %s: invalid config: %w", ErrCorrupt, dir, err)
	}
	return cfg, nil
}

// newCodec builds the blob codec described by c. For an encrypted repository
// it asks pass for the passphrase and unlocks the master key; the secrets it
// handles are wiped before returning.
func (c Config) newCodec(dir string, pass func() ([]byte, error)) (*codec.Codec, error) {
	if !c.Encryption {
		return codec.New(codec.Options{Compress: c.Compression})
	}
	if pass == nil {
		return nil, fmt.Errorf("%w: %s", ErrPassphraseRequired, dir)
	}
	p, err := pass()
	if err != nil {
		return nil, fmt.Errorf("vault: read passphrase: %w", err)
	}
	defer clear(p)
	master, err := c.Key.Unwrap(p)
	if err != nil {
		return nil, fmt.Errorf("vault: unlock repository %s: %w", dir, err)
	}
	defer clear(master[:])
	sub, err := master.Derive()
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	defer clear(sub.Enc)
	defer clear(sub.Mac)
	return codec.New(codec.Options{Compress: c.Compression, Keys: &codec.Keys{Enc: sub.Enc, Mac: sub.Mac}})
}

// initSettings collects the InitOption values.
type initSettings struct {
	chunker    chunker.Config
	compress   bool
	encrypt    bool
	passphrase []byte
	kdf        keys.KDFParams
}

// InitOption customises Init.
type InitOption func(*initSettings)

// InitEncrypted makes Init create an encrypted repository whose master key is
// wrapped under passphrase. The passphrase is not retained.
func InitEncrypted(passphrase []byte) InitOption {
	return func(s *initSettings) { s.encrypt, s.passphrase = true, passphrase }
}

// InitKDFParams overrides the Argon2id parameters used for an encrypted
// repository, see keys.DefaultKDFParams.
func InitKDFParams(p keys.KDFParams) InitOption {
	return func(s *initSettings) { s.kdf = p }
}

// InitChunkerConfig overrides the chunker parameters recorded in the config.
func InitChunkerConfig(cfg chunker.Config) InitOption {
	return func(s *initSettings) { s.chunker = cfg }
}

// InitCompression enables or disables zstd compression, which is on by
// default.
func InitCompression(on bool) InitOption {
	return func(s *initSettings) { s.compress = on }
}
