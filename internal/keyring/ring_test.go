package keyring

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"
)

const (
	oldSecret = "old-secret-0123456789abcdef0123456789abcdef"
	newSecret = "new-secret-0123456789abcdef0123456789abcdef"
)

func TestDerivedKeysAreSeparatedFromEachOtherAndFromTheSecret(t *testing.T) {
	k := derive(oldSecret)
	if bytes.Equal(k.token, k.settings) {
		t.Fatal("token and settings keys must differ")
	}
	legacy := sha256.Sum256([]byte(legacySettingsKey + oldSecret))
	if bytes.Equal(k.settings, legacy[:]) || bytes.Equal(k.token, []byte(oldSecret)) {
		t.Fatal("derived keys must not equal the legacy key material")
	}
	if bytes.Equal(NewRing(oldSecret).Hash("token"), LegacyHash("token", oldSecret)) {
		t.Fatal("a new hash must not equal the legacy hash")
	}
	if NewRing(oldSecret).KeyID() == NewRing(newSecret).KeyID() || len(NewRing(oldSecret).KeyID()) != 2*keyIDLen {
		t.Fatalf("key ids: %q %q", NewRing(oldSecret).KeyID(), NewRing(newSecret).KeyID())
	}
}

func TestHashCandidatesCoverLegacyAndPreviousSecrets(t *testing.T) {
	token := "hfpx_token-value-0123456789"
	rotated := NewRing(newSecret, oldSecret)
	candidates := rotated.HashCandidates(token)
	if len(candidates) != 4 || !bytes.Equal(candidates[0], rotated.Hash(token)) {
		t.Fatalf("candidates = %d, first must be the current hash", len(candidates))
	}
	for name, stored := range map[string][]byte{
		"new derived": NewRing(newSecret).Hash(token),
		"new legacy":  LegacyHash(token, newSecret),
		"old derived": NewRing(oldSecret).Hash(token),
		"old legacy":  LegacyHash(token, oldSecret),
	} {
		if !rotated.MatchesAny(token, stored) {
			t.Errorf("%s hash is not recognised", name)
		}
	}
	if rotated.MatchesAny(token, LegacyHash(token, "some-other-secret-0123456789abcdef012345")) {
		t.Error("a hash from an unknown secret was recognised")
	}
	if rotated.IsCurrent(token, LegacyHash(token, oldSecret)) || !rotated.IsCurrent(token, rotated.Hash(token)) {
		t.Error("IsCurrent must be true only for the current derived hash")
	}
	if len(NewRing(oldSecret, oldSecret, "").HashCandidates(token)) != 2 {
		t.Error("duplicate and empty secrets must be ignored")
	}
}

func TestSealOpenRoundTripAndKeyRotation(t *testing.T) {
	plaintext := []byte(`{"s3_secret_key":"super secret"}`)
	sealed, err := NewRing(oldSecret).Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("super secret")) {
		t.Fatal("sealed value leaks the plaintext")
	}
	got, current, err := NewRing(oldSecret).Open(sealed)
	if err != nil || !current || !bytes.Equal(got, plaintext) {
		t.Fatalf("same-key open = %q current=%v err=%v", got, current, err)
	}
	got, current, err = NewRing(newSecret, oldSecret).Open(sealed)
	if err != nil || current || !bytes.Equal(got, plaintext) {
		t.Fatalf("rotated open = %q current=%v err=%v; want readable but flagged for re-sealing", got, current, err)
	}
	if _, _, err := NewRing(newSecret).Open(sealed); err != ErrCannotOpen {
		t.Fatalf("open without the old secret = %v, want ErrCannotOpen", err)
	}
	resealed, err := NewRing(newSecret, oldSecret).Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, current, err := NewRing(newSecret).Open(resealed); err != nil || !current {
		t.Fatalf("after re-sealing, the new secret alone must open it: current=%v err=%v", current, err)
	}
	tampered := append([]byte{}, resealed...)
	tampered[len(tampered)-1] ^= 1
	if _, _, err := NewRing(newSecret).Open(tampered); err == nil {
		t.Fatal("a tampered value was accepted")
	}
}

// legacySeal reproduces how settings were sealed before keys were derived.
func legacySeal(t *testing.T, secret string, plaintext []byte) []byte {
	t.Helper()
	key := sha256.Sum256([]byte(legacySettingsKey + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nonce, nonce, plaintext, []byte(legacySettingsAAD))
}

func TestLegacySettingsStillOpenAndAreFlaggedForResealing(t *testing.T) {
	plaintext := []byte(`{"hf_token":"hf_x"}`)
	sealed := legacySeal(t, oldSecret, plaintext)
	got, current, err := NewRing(oldSecret).Open(sealed)
	if err != nil || current || !bytes.Equal(got, plaintext) {
		t.Fatalf("legacy open = %q current=%v err=%v", got, current, err)
	}
	if _, _, err := NewRing(newSecret, oldSecret).Open(sealed); err != nil {
		t.Fatalf("legacy value under a previous secret: %v", err)
	}
	if _, _, err := NewRing(newSecret).Open(sealed); err != ErrCannotOpen {
		t.Fatalf("legacy value with the wrong secret = %v, want ErrCannotOpen", err)
	}
	if _, _, err := NewRing(oldSecret).Open([]byte("short")); err == nil || !strings.Contains(err.Error(), "decrypted") {
		t.Fatalf("garbage = %v", err)
	}
}
