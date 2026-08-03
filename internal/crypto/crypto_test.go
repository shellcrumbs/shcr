package crypto

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"event_id":"abc","command":"echo hello"}` + "\n")
	sealed, err := Encrypt(k, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("echo hello")) {
		t.Fatal("plaintext is visible in the ciphertext")
	}
	got, err := Decrypt(k, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip changed the data:\n want %q\n got  %q", plain, got)
	}
}

// The whole point of the recovery phrase: seal on one machine, write down the
// words, type them on another, read the data back.
func TestPhraseCarriesKeyBetweenMachines(t *testing.T) {
	machineA, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := machineA.Phrase()
	if err != nil {
		t.Fatal(err)
	}
	if words := len(strings.Fields(phrase)); words != 24 {
		t.Fatalf("phrase has %d words, want 24", words)
	}

	sealed, err := Encrypt(machineA, []byte("secret history"))
	if err != nil {
		t.Fatal(err)
	}

	machineB, err := KeyFromPhrase(phrase)
	if err != nil {
		t.Fatal(err)
	}
	if machineB != machineA {
		t.Fatal("phrase did not reproduce the original key")
	}
	got, err := Decrypt(machineB, sealed)
	if err != nil {
		t.Fatalf("machine B could not read machine A's batch: %v", err)
	}
	if string(got) != "secret history" {
		t.Fatalf("got %q", got)
	}
}

func TestPhraseToleratesTranscriptionWhitespaceAndCase(t *testing.T) {
	k, _ := GenerateKey()
	phrase, _ := k.Phrase()
	messy := "  " + strings.ToUpper(strings.ReplaceAll(phrase, " ", "   ")) + "\n"
	got, err := KeyFromPhrase(messy)
	if err != nil {
		t.Fatalf("a phrase with odd spacing and casing should still work: %v", err)
	}
	if got != k {
		t.Fatal("normalised phrase produced a different key")
	}
}

// A mistyped word must be rejected outright, not silently turned into some
// other key that then fails to decrypt anything with no explanation.
func TestMistypedPhraseIsRejected(t *testing.T) {
	k, _ := GenerateKey()
	phrase, _ := k.Phrase()
	words := strings.Fields(phrase)
	words[3] = "zzzznotaword"
	if _, err := KeyFromPhrase(strings.Join(words, " ")); !errors.Is(err, ErrBadPhrase) {
		t.Fatalf("expected ErrBadPhrase for an invalid word, got %v", err)
	}

	// A real word in the wrong place breaks the checksum.
	words = strings.Fields(phrase)
	if words[0] == "zoo" {
		words[0] = "zone"
	} else {
		words[0] = "zoo"
	}
	if _, err := KeyFromPhrase(strings.Join(words, " ")); err == nil {
		t.Fatal("expected the BIP39 checksum to catch a swapped word")
	}

	if _, err := KeyFromPhrase("too short a phrase"); !errors.Is(err, ErrBadPhrase) {
		t.Fatalf("expected ErrBadPhrase for a short phrase, got %v", err)
	}
}

// Fail closed: corruption must never surface as a partial or garbage plaintext.
func TestCorruptedCiphertextFailsClosed(t *testing.T) {
	k, _ := GenerateKey()
	sealed, err := Encrypt(k, []byte("aws configure --secret hunter2"))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"flipped bit in body":  flip(sealed, len(sealed)-3),
		"flipped bit in nonce": flip(sealed, 2),
		"truncated":            sealed[:len(sealed)-1],
		"empty":                {},
		"shorter than nonce":   sealed[:5],
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := Decrypt(k, bad)
			if err == nil {
				t.Fatalf("expected an error, got plaintext %q", out)
			}
			if !errors.Is(err, ErrNotDecryptable) {
				t.Fatalf("want ErrNotDecryptable, got %v", err)
			}
			if out != nil {
				t.Fatalf("no data may be returned on failure, got %q", out)
			}
		})
	}
}

func TestWrongKeyFailsClosed(t *testing.T) {
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	sealed, _ := Encrypt(a, []byte("private"))
	if _, err := Decrypt(b, sealed); !errors.Is(err, ErrNotDecryptable) {
		t.Fatalf("a different key must not decrypt, got %v", err)
	}
}

// Nonce reuse under XChaCha20-Poly1305 is catastrophic, so identical input must
// still produce different ciphertext every time.
func TestNoncesAreFreshPerBatch(t *testing.T) {
	k, _ := GenerateKey()
	seen := make(map[string]bool)
	const n = 200
	for range n {
		sealed, err := Encrypt(k, []byte("identical batch contents"))
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(sealed[:24])
		if seen[nonce] {
			t.Fatal("a nonce was reused")
		}
		seen[nonce] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct nonces out of %d", len(seen), n)
	}
}

// The OS keychain is per-user and global. t.TempDir() isolates the file
// fallback and nothing else, so a test that lets Save reach the real Secret
// Service overwrites whatever key the person running the suite actually has —
// and a t.Cleanup calling Delete then removes it. That is the one secret this
// tool tells people nobody can recover for them, destroyed by `go test ./...`.
//
// Every test in this package therefore runs against an in-memory keychain. Use
// disableKeyring to test the no-keychain path; nothing here should ever call
// the real one.
func TestMain(m *testing.M) {
	var mu sync.Mutex
	entries := map[string]string{}
	key := func(service, user string) string { return service + "\x00" + user }

	keyringSet = func(service, user, value string) error {
		mu.Lock()
		defer mu.Unlock()
		entries[key(service, user)] = value
		return nil
	}
	keyringGet = func(service, user string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := entries[key(service, user)]
		if !ok {
			return "", errors.New("no entry in the test keychain")
		}
		return v, nil
	}
	keyringDelete = func(service, user string) error {
		mu.Lock()
		defer mu.Unlock()
		delete(entries, key(service, user))
		return nil
	}
	os.Exit(m.Run())
}

func TestKeystoreRoundTrip(t *testing.T) {
	ks := NewKeystore(t.TempDir())
	t.Cleanup(func() { _ = ks.Delete() })

	if _, _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty keystore should report ErrNoKey, got %v", err)
	}

	k, _ := GenerateKey()
	src, err := ks.Save(k)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic now that the keychain is the in-memory one: the keychain is
	// preferred whenever it works, and this used to depend on whether the
	// machine running the tests happened to have a working Secret Service.
	if src != SourceKeyring {
		t.Fatalf("a working keychain should be preferred, got %s", src)
	}

	got, gotSrc, err := ks.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != k {
		t.Fatal("keystore returned a different key")
	}
	if gotSrc != src {
		t.Fatalf("saved to %s but loaded from %s", src, gotSrc)
	}
}

// disableKeyring simulates a machine with no Secret Service — the normal
// situation on a headless server, and the path most likely to go untested.
func disableKeyring(t *testing.T) {
	t.Helper()
	unavailable := errors.New("no keychain on this machine")
	oldSet, oldGet, oldDel := keyringSet, keyringGet, keyringDelete
	keyringSet = func(string, string, string) error { return unavailable }
	keyringGet = func(string, string) (string, error) { return "", unavailable }
	keyringDelete = func(string, string) error { return unavailable }
	t.Cleanup(func() { keyringSet, keyringGet, keyringDelete = oldSet, oldGet, oldDel })
}

// When there is no OS keychain the key lands in a file, and that file must not
// be readable by anyone else.
func TestFileFallbackIsPrivateAndWorks(t *testing.T) {
	disableKeyring(t)
	ks := NewKeystore(t.TempDir())

	k, _ := GenerateKey()
	src, err := ks.Save(k)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFile {
		t.Fatalf("with no keychain the key must fall back to a file, got %s", src)
	}

	info, err := os.Stat(ks.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode is %o, want 600", perm)
	}

	got, gotSrc, err := ks.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != k || gotSrc != SourceFile {
		t.Fatalf("fallback round trip failed: key match=%v source=%s", got == k, gotSrc)
	}

	// The file holds the key, so it must never survive a delete.
	if err := ks.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ks.FilePath); !os.IsNotExist(err) {
		t.Fatal("key file still present after delete")
	}
}

// Once a keychain becomes available, the plaintext copy on disk must not be
// left behind.
func TestSaveClearsStaleFileCopyWhenKeychainWorks(t *testing.T) {
	dir := t.TempDir()

	disableKeyring(t)
	ks := NewKeystore(dir)
	k, _ := GenerateKey()
	if src, err := ks.Save(k); err != nil || src != SourceFile {
		t.Fatalf("setup: src=%s err=%v", src, err)
	}
	if _, err := os.Stat(ks.FilePath); err != nil {
		t.Fatal("setup: expected a key file")
	}

	// Now pretend the keychain came back.
	stored := ""
	keyringSet = func(_, _, v string) error { stored = v; return nil }
	keyringGet = func(_, _ string) (string, error) { return stored, nil }

	ks2 := NewKeystore(dir)
	t.Cleanup(func() { _ = ks2.Delete() })
	if src, err := ks2.Save(k); err != nil || src != SourceKeyring {
		t.Fatalf("expected the keychain to be used, got src=%s err=%v", src, err)
	}
	if _, err := os.Stat(ks2.FilePath); !os.IsNotExist(err) {
		t.Fatal("a plaintext key file was left on disk after the keychain took over")
	}
}

func flip(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0x40
	return out
}
