// Package keyring derives the keys hfproxy needs from the operator's session
// secret and supports rotating that secret without a flag day.
//
// One secret feeds three purposes that must not share a key: hashing access
// tokens and sessions, encrypting saved settings, and naming the key. Each gets
// its own HKDF-SHA256 key. Anything produced before keys were derived (HMAC
// with the raw secret, and settings sealed with a bare SHA-256 of it) is still
// recognised so existing installations keep working after an upgrade.
package keyring

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const (
	infoTokens   = "hfproxy/v2/tokens"
	infoSettings = "hfproxy/v2/settings"
	infoKeyID    = "hfproxy/v2/key-id"

	sealMagic         = "HFS2"
	keyIDLen          = 4
	sealAAD           = "hfproxy-runtime-settings-v2"
	legacySettingsAAD = "hfproxy-runtime-settings-v1"
	legacySettingsKey = "hfproxy settings encryption v1:"
)

// ErrCannotOpen is returned when no secret in the ring decrypts a value.
var ErrCannotOpen = errors.New("saved value cannot be decrypted with any configured session secret")

type derivedKey struct {
	raw      string
	token    []byte
	settings []byte
	id       []byte
}

func derive(secret string) derivedKey {
	must := func(info string, length int) []byte {
		key, err := hkdf.Key(sha256.New, []byte(secret), nil, info, length)
		if err != nil {
			// Only possible for an absurd output length; the lengths are constants.
			panic("hkdf: " + err.Error())
		}
		return key
	}
	return derivedKey{raw: secret, token: must(infoTokens, 32), settings: must(infoSettings, 32), id: must(infoKeyID, keyIDLen)}
}

// Ring holds the current secret and any previous ones still accepted.
type Ring struct {
	keys []derivedKey // keys[0] is the current key
}

// NewRing returns a ring that signs and seals with current and still accepts
// each previous secret. Empty and duplicate secrets are ignored.
func NewRing(current string, previous ...string) *Ring {
	ring := &Ring{}
	seen := map[string]bool{}
	for _, secret := range append([]string{current}, previous...) {
		if secret == "" || seen[secret] {
			continue
		}
		seen[secret] = true
		ring.keys = append(ring.keys, derive(secret))
	}
	if len(ring.keys) == 0 {
		ring.keys = append(ring.keys, derive(""))
	}
	return ring
}

// KeyID names the current key. It is safe to store and log.
func (r *Ring) KeyID() string { return hex.EncodeToString(r.keys[0].id) }

// LegacyHash is the pre-derivation scheme: HMAC-SHA256 keyed with the raw secret.
func LegacyHash(token, secret string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(token))
	return h.Sum(nil)
}

func tokenHash(key []byte, token string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(token))
	return h.Sum(nil)
}

// Hash is what gets stored for a new token or session.
func (r *Ring) Hash(token string) []byte { return tokenHash(r.keys[0].token, token) }

// HashCandidates lists every hash the token may have been stored under: the
// current derived hash first, then the legacy hash of the current secret, then
// the same pair for each previous secret.
func (r *Ring) HashCandidates(token string) [][]byte {
	candidates := make([][]byte, 0, 2*len(r.keys))
	for _, key := range r.keys {
		candidates = append(candidates, tokenHash(key.token, token), LegacyHash(token, key.raw))
	}
	return candidates
}

// IsCurrent reports whether stored is the hash a new token would get now.
func (r *Ring) IsCurrent(token string, stored []byte) bool {
	return hmac.Equal(r.Hash(token), stored)
}

// MatchesAny reports whether stored equals the token's hash under any key.
func (r *Ring) MatchesAny(token string, stored []byte) bool {
	for _, candidate := range r.HashCandidates(token) {
		if hmac.Equal(candidate, stored) {
			return true
		}
	}
	return false
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext under the current key. The result names the key, so
// opening it later does not need to try every secret.
func (r *Ring) Seal(plaintext []byte) ([]byte, error) {
	gcm, err := gcmFor(r.keys[0].settings)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	header := append([]byte(sealMagic), r.keys[0].id...)
	out := append(header, nonce...)
	return gcm.Seal(out, nonce, plaintext, []byte(sealAAD)), nil
}

// Open decrypts a sealed value. current is true only when it was sealed in the
// present format under the current key; false means it should be sealed again.
func (r *Ring) Open(sealed []byte) (plaintext []byte, current bool, err error) {
	if bytes.HasPrefix(sealed, []byte(sealMagic)) && len(sealed) > len(sealMagic)+keyIDLen {
		id := sealed[len(sealMagic) : len(sealMagic)+keyIDLen]
		body := sealed[len(sealMagic)+keyIDLen:]
		for index, key := range r.keys {
			if !bytes.Equal(key.id, id) {
				continue
			}
			if plaintext, ok := openWith(key.settings, body, sealAAD); ok {
				return plaintext, index == 0, nil
			}
		}
		for _, key := range r.keys { // an id collision, or a ring built from other secrets
			if plaintext, ok := openWith(key.settings, body, sealAAD); ok {
				return plaintext, false, nil
			}
		}
		return nil, false, ErrCannotOpen
	}
	for _, key := range r.keys {
		legacy := sha256.Sum256([]byte(legacySettingsKey + key.raw))
		if plaintext, ok := openWith(legacy[:], sealed, legacySettingsAAD); ok {
			return plaintext, false, nil
		}
	}
	return nil, false, ErrCannotOpen
}

func openWith(key, body []byte, aad string) ([]byte, bool) {
	gcm, err := gcmFor(key)
	if err != nil || len(body) < gcm.NonceSize() {
		return nil, false
	}
	plaintext, err := gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], []byte(aad))
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// Fingerprint describes a secret without revealing it, for diagnostics.
func Fingerprint(secret string) string {
	return fmt.Sprintf("%x", derive(secret).id)
}
