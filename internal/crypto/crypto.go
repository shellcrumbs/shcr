// Package crypto owns the end-to-end encryption key and the sealing of sync
// batches.
//
// The bucket only ever holds ciphertext produced here. The key is generated on
// one machine, carried to the others as a written phrase, and never transmitted
// anywhere — not to the bucket, not to us, not to anything.
package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is 256 bits, which BIP39 encodes as a 24-word phrase.
const KeySize = 32

type Key [KeySize]byte

var (
	// ErrNotDecryptable is returned for anything that does not authenticate:
	// wrong key, truncation, or a flipped bit. There is deliberately no way to
	// tell those apart, and no partial result is ever returned.
	ErrNotDecryptable = errors.New("ciphertext failed authentication (wrong key or corrupted data)")
	ErrBadPhrase      = errors.New("recovery phrase is not valid")
)

func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// Phrase renders the key as a 24-word BIP39 mnemonic. The words carry a
// checksum, so a transcription slip is caught rather than silently producing a
// different key.
func (k Key) Phrase() (string, error) {
	m, err := bip39.NewMnemonic(k[:])
	if err != nil {
		return "", fmt.Errorf("encode recovery phrase: %w", err)
	}
	return m, nil
}

// KeyFromPhrase is the exact inverse of Phrase. It reverses the BIP39 encoding
// rather than running the phrase through a seed derivation, so the key that
// comes back is byte-identical to the one that was written down.
func KeyFromPhrase(phrase string) (Key, error) {
	phrase = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(phrase))), " ")
	entropy, err := bip39.EntropyFromMnemonic(phrase)
	if err != nil {
		return Key{}, fmt.Errorf("%w: %v", ErrBadPhrase, err)
	}
	if len(entropy) != KeySize {
		return Key{}, fmt.Errorf("%w: expected a 24-word phrase, got %d words", ErrBadPhrase, len(strings.Fields(phrase)))
	}
	var k Key
	copy(k[:], entropy)
	return k, nil
}

// Encrypt seals a batch with XChaCha20-Poly1305. The nonce is 192 bits of fresh
// randomness prepended to the output — wide enough that generating them
// randomly will not collide, so no counter has to be kept in sync across
// machines that never talk to each other directly.
func Encrypt(k Key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(k[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt fails closed: any tampering, truncation or wrong key yields an error
// and never a partial or garbage plaintext.
func Decrypt(k Key, blob []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(k[:])
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("%w: too short to be a sealed batch", ErrNotDecryptable)
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	out, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrNotDecryptable
	}
	return out, nil
}
