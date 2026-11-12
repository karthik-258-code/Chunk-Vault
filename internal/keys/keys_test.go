package keys

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// fastKDF keeps tests quick; the defaults are exercised separately.
func fastKDF() KDFParams {
	return KDFParams{Algorithm: AlgorithmArgon2id, Time: 1, MemoryKiB: 64, Threads: 1}
}

func TestDefaultKDFParams(t *testing.T) {
	p := DefaultKDFParams()
	if p.Algorithm != "argon2id" || p.Time != 3 || p.MemoryKiB != 64*1024 || p.Threads != 4 {
		t.Fatalf("defaults = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWrapUnwrap(t *testing.T) {
	w, m, err := NewWrapped([]byte("correct horse"), fastKDF())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := w.Unwrap([]byte("correct horse"))
	if err != nil || got != m {
		t.Fatalf("Unwrap = %x, %v; want the original master key", got, err)
	}
	if bytes.Contains(w.WrappedKey, m[:]) {
		t.Fatal("wrapped key contains the raw master key")
	}
}

func TestWrongPassphrase(t *testing.T) {
	w, _, err := NewWrapped([]byte("right"), fastKDF())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"wrong", "", "righ", "right "} {
		if _, err := w.Unwrap([]byte(bad)); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("Unwrap(%q) = %v, want ErrWrongPassphrase", bad, err)
		}
	}
}

func TestTamperedWrappedKeyRejected(t *testing.T) {
	w, _, err := NewWrapped([]byte("pw"), fastKDF())
	if err != nil {
		t.Fatal(err)
	}
	for i := range w.WrappedKey {
		bad := w
		bad.WrappedKey = append([]byte(nil), w.WrappedKey...)
		bad.WrappedKey[i] ^= 1
		if _, err := bad.Unwrap([]byte("pw")); !errors.Is(err, ErrWrongPassphrase) {
			t.Fatalf("flip at %d: %v", i, err)
		}
	}
	// Changing the salt or KDF parameters changes the KEK, so it fails too.
	bad := w
	bad.Salt = append([]byte(nil), w.Salt...)
	bad.Salt[0] ^= 1
	if _, err := bad.Unwrap([]byte("pw")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("salt flip: %v", err)
	}
	bad = w
	bad.KDF.Time++
	if _, err := bad.Unwrap([]byte("pw")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("kdf change: %v", err)
	}
}

func TestFreshSaltAndMasterPerRepository(t *testing.T) {
	a, ma, _ := NewWrapped([]byte("pw"), fastKDF())
	b, mb, _ := NewWrapped([]byte("pw"), fastKDF())
	if bytes.Equal(a.Salt, b.Salt) || ma == mb || bytes.Equal(a.WrappedKey, b.WrappedKey) {
		t.Fatal("two repositories with the same passphrase must not share salt or keys")
	}
}

func TestDerive(t *testing.T) {
	_, m, err := NewWrapped([]byte("pw"), fastKDF())
	if err != nil {
		t.Fatal(err)
	}
	s1, err := m.Derive()
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := m.Derive()
	if len(s1.Enc) != KeySize || len(s1.Mac) != KeySize {
		t.Fatalf("subkey sizes %d, %d", len(s1.Enc), len(s1.Mac))
	}
	if !bytes.Equal(s1.Enc, s2.Enc) || !bytes.Equal(s1.Mac, s2.Mac) {
		t.Fatal("derivation must be deterministic")
	}
	if bytes.Equal(s1.Enc, s1.Mac) || bytes.Equal(s1.Enc, m[:]) || bytes.Equal(s1.Mac, m[:]) {
		t.Fatal("subkeys must differ from each other and from the master key")
	}
	var other Master
	other[0] = 1
	if o, _ := other.Derive(); bytes.Equal(o.Enc, s1.Enc) {
		t.Fatal("different master keys must give different subkeys")
	}
}

func TestValidateParams(t *testing.T) {
	good := fastKDF()
	tests := map[string]func(p *KDFParams){
		"algorithm":      func(p *KDFParams) { p.Algorithm = "scrypt" },
		"zero time":      func(p *KDFParams) { p.Time = 0 },
		"huge time":      func(p *KDFParams) { p.Time = 1 << 20 },
		"zero threads":   func(p *KDFParams) { p.Threads = 0 },
		"many threads":   func(p *KDFParams) { p.Threads = 200 },
		"tiny memory":    func(p *KDFParams) { p.MemoryKiB = 1 },
		"huge memory":    func(p *KDFParams) { p.MemoryKiB = 1 << 30 },
		"memory/threads": func(p *KDFParams) { p.Threads = 8; p.MemoryKiB = 32 },
	}
	for name, mutate := range tests {
		p := good
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: Validate = %v, want ErrInvalidParams", name, err)
		}
		if _, _, err := NewWrapped([]byte("pw"), p); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: NewWrapped = %v, want ErrInvalidParams", name, err)
		}
	}
	if _, _, err := NewWrapped(nil, good); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("empty passphrase: %v", err)
	}
}

func TestWrappedValidate(t *testing.T) {
	w, _, _ := NewWrapped([]byte("pw"), fastKDF())
	short := w
	short.Salt = w.Salt[:5]
	if err := short.Validate(); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("short salt: %v", err)
	}
	short = w
	short.WrappedKey = w.WrappedKey[:10]
	if _, err := short.Unwrap([]byte("pw")); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("short wrapped key: %v", err)
	}
	bad := w
	bad.KDF.Algorithm = "x"
	if _, err := bad.Unwrap([]byte("pw")); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("bad kdf: %v", err)
	}
}

func TestJSONNeverContainsSecrets(t *testing.T) {
	pass := []byte("hunter2-passphrase")
	w, m, _ := NewWrapped(pass, fastKDF())
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Derive()
	for name, secret := range map[string][]byte{"passphrase": pass, "master": m[:], "enc": s.Enc, "mac": s.Mac} {
		if bytes.Contains(data, secret) {
			t.Errorf("serialised key material contains the %s", name)
		}
	}
	var back Wrapped
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got, err := back.Unwrap(pass); err != nil || got != m {
		t.Fatalf("JSON round trip broke Unwrap: %v", err)
	}
}

func TestDefaultParamsUnwrap(t *testing.T) {
	// One run with the real default cost, so a regression in how the
	// parameters are passed to Argon2id is not hidden by the cheap test KDF.
	w, m, err := NewWrapped([]byte("pw"), DefaultKDFParams())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.Unwrap([]byte("pw")); err != nil || got != m {
		t.Fatalf("Unwrap: %v", err)
	}
}
