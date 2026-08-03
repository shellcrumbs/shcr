package crypto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "shellcrumbs"
	keyringUser    = "sync-key"
)

// Source records where a key came from, so the CLI can warn when the OS
// keychain was unavailable and the key ended up on disk instead.
type Source string

const (
	SourceKeyring Source = "os-keychain"
	SourceFile    Source = "file"
	SourceNone    Source = "none"
)

var ErrNoKey = errors.New("no encryption key configured (run: shcr key init)")

// Indirected so tests can exercise the file fallback on a machine that does
// have a working keychain — headless servers are the common case in practice
// and the untested path is the one that matters.
var (
	keyringSet    = keyring.Set
	keyringGet    = keyring.Get
	keyringDelete = keyring.Delete
)

// Keystore keeps the key in the OS keychain when there is one, and in a 0600
// file when there is not. The file fallback is a real downgrade — it is always
// reported so it can never happen silently.
type Keystore struct {
	// FilePath is where the fallback key lives.
	FilePath string
}

func NewKeystore(dataDir string) *Keystore {
	return &Keystore{FilePath: filepath.Join(dataDir, "key")}
}

// keychainDisabled lets a user opt out when the platform's Secret Service is
// broken or absent, and lets two installs on one machine keep separate keys.
func keychainDisabled() bool {
	return os.Getenv("SHCR_NO_KEYCHAIN") != ""
}

func (ks *Keystore) Save(k Key) (Source, error) {
	encoded := base64.StdEncoding.EncodeToString(k[:])
	if err := trySet(encoded); err == nil {
		// Do not leave a stale plaintext copy behind once the keychain works.
		_ = os.Remove(ks.FilePath)
		return SourceKeyring, nil
	}
	if err := os.MkdirAll(filepath.Dir(ks.FilePath), 0o700); err != nil {
		return SourceNone, err
	}
	if err := os.WriteFile(ks.FilePath, []byte(encoded+"\n"), 0o600); err != nil {
		return SourceNone, fmt.Errorf("write key file: %w", err)
	}
	return SourceFile, nil
}

func (ks *Keystore) Load() (Key, Source, error) {
	if encoded, err := tryGet(); err == nil {
		k, err := decodeKey(encoded)
		return k, SourceKeyring, err
	}
	b, err := os.ReadFile(ks.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return Key{}, SourceNone, ErrNoKey
		}
		return Key{}, SourceNone, err
	}
	k, err := decodeKey(string(b))
	return k, SourceFile, err
}

func trySet(encoded string) error {
	if keychainDisabled() {
		return errors.New("keychain disabled by SHCR_NO_KEYCHAIN")
	}
	return keyringSet(keyringService, keyringUser, encoded)
}

func tryGet() (string, error) {
	if keychainDisabled() {
		return "", errors.New("keychain disabled by SHCR_NO_KEYCHAIN")
	}
	return keyringGet(keyringService, keyringUser)
}

func (ks *Keystore) Delete() error {
	if !keychainDisabled() {
		_ = keyringDelete(keyringService, keyringUser)
	}
	err := os.Remove(ks.FilePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func decodeKey(encoded string) (Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return Key{}, fmt.Errorf("stored key is not valid base64: %w", err)
	}
	if len(raw) != KeySize {
		return Key{}, fmt.Errorf("stored key is %d bytes, want %d", len(raw), KeySize)
	}
	var k Key
	copy(k[:], raw)
	return k, nil
}
