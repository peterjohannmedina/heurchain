package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Envelope encryption for cryptographic silos.
//
// Each silo gets a 256-bit DEK (AES-256-GCM). Content (text + metadata) is
// encrypted under the DEK before it touches Redis. The DEK is wrapped under
// the firm KEK (HEURCHAIN_KEK env, base64 32 bytes) and only the wrapped form
// is persisted (silo_keys.wrapped_dek). Deleting the silo_keys row is the
// crypto-shred: all ciphertext for that silo becomes unrecoverable.
//
// The DEK fingerprint is HMAC-SHA256 over the *plaintext* DEK keyed by
// HEURCHAIN_FP_KEY, computed once at DEK creation and stored alongside the
// wrapped DEK. Fingerprinting the plaintext (not the wrapped form) keeps the
// fingerprint stable across KEK rotation re-wraps, which is what makes the
// Destruction Certificate equality check (destroyed_fingerprint ==
// certificate dek_fingerprint) hold for the lifetime of the silo.
//
// Honest operational limit (documented for the trust protocol): crypto-shred
// deletes the wrapped DEK from the live database. Encrypted off-site backups
// of SQLite retain prior wrapped DEKs until the backup retention window
// (currently 7 days) expires. Destruction certificates state this window.
// ---------------------------------------------------------------------------

const (
	envKEK        = "HEURCHAIN_KEK"
	envFPKey      = "HEURCHAIN_FP_KEY"
	envAttestSeed = "HEURCHAIN_ATTEST_SEED"

	dekCacheTTL = 5 * time.Minute
	dekCacheMax = 256
)

var errCryptoNotConfigured = errors.New("silo crypto not configured (HEURCHAIN_KEK / HEURCHAIN_FP_KEY unset)")

// siloCrypto holds the firm KEK and fingerprint key plus a bounded TTL cache
// of unwrapped DEKs so the hot store/query path does not unwrap on every call.
type siloCrypto struct {
	kek   []byte // 32 bytes
	kekID string // identifies the KEK generation (for re-wrap on rotation)
	fpKey []byte // 32 bytes, HMAC key for DEK fingerprints

	mu       sync.Mutex
	cache    map[string]cachedDEK // silo_id -> plaintext DEK
	shredded map[string]struct{}  // silo_ids whose DEK was destroyed this process lifetime
}

type cachedDEK struct {
	dek     []byte
	expires time.Time
}

// loadSiloCrypto reads the crypto secrets from the environment. Returns nil
// (feature disabled, not an error) when the KEK is unset so the broker can run
// with silos dark until secrets are provisioned.
func loadSiloCrypto() (*siloCrypto, error) {
	kekB64 := os.Getenv(envKEK)
	if kekB64 == "" {
		return nil, nil
	}
	kek, err := base64.StdEncoding.DecodeString(kekB64)
	if err != nil || len(kek) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes", envKEK)
	}
	fpB64 := os.Getenv(envFPKey)
	fpKey, err := base64.StdEncoding.DecodeString(fpB64)
	if err != nil || len(fpKey) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes", envFPKey)
	}
	kekID := os.Getenv("HEURCHAIN_KEK_ID")
	if kekID == "" {
		kekID = "kek-1"
	}
	return &siloCrypto{kek: kek, kekID: kekID, fpKey: fpKey, cache: make(map[string]cachedDEK), shredded: make(map[string]struct{})}, nil
}

// gcmSeal encrypts plaintext with key under AES-256-GCM using a random nonce.
// Output layout: nonce || ciphertext.
func gcmSeal(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, aad)...), nil
}

// gcmOpen decrypts a nonce||ciphertext blob produced by gcmSeal.
func gcmOpen(key, blob, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], aad)
}

// newDEK generates a fresh 256-bit DEK, wraps it under the KEK, and computes
// its fingerprint. The silo id is bound into the wrap AAD so a wrapped DEK
// copied between silo_keys rows fails to unwrap.
func (sc *siloCrypto) newDEK(siloID string) (wrapped []byte, fingerprint string, err error) {
	dek := make([]byte, 32)
	if _, err = rand.Read(dek); err != nil {
		return nil, "", err
	}
	wrapped, err = gcmSeal(sc.kek, dek, []byte("silo:"+siloID))
	if err != nil {
		return nil, "", err
	}
	return wrapped, sc.fingerprint(dek), nil
}

// unwrapDEK recovers the plaintext DEK for a silo, consulting the TTL cache.
// A silo evicted by crypto-shred is permanently refused for the rest of the
// process lifetime, so an in-flight request that read the wrapped DEK just
// before the shred committed cannot re-cache (or use) a destroyed key.
func (sc *siloCrypto) unwrapDEK(siloID string, wrapped []byte) ([]byte, error) {
	sc.mu.Lock()
	if _, gone := sc.shredded[siloID]; gone {
		sc.mu.Unlock()
		return nil, fmt.Errorf("silo %s DEK has been crypto-shredded", siloID)
	}
	if c, ok := sc.cache[siloID]; ok && time.Now().Before(c.expires) {
		dek := c.dek
		sc.mu.Unlock()
		return dek, nil
	}
	sc.mu.Unlock()

	dek, err := gcmOpen(sc.kek, wrapped, []byte("silo:"+siloID))
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK for silo %s: %w", siloID, err)
	}

	sc.mu.Lock()
	if _, gone := sc.shredded[siloID]; gone {
		sc.mu.Unlock()
		return nil, fmt.Errorf("silo %s DEK has been crypto-shredded", siloID)
	}
	if len(sc.cache) >= dekCacheMax {
		// Evict expired entries; if none expired, drop one arbitrary entry to
		// bound memory (cache misses only cost one GCM open).
		now := time.Now()
		evicted := false
		for k, v := range sc.cache {
			if now.After(v.expires) {
				delete(sc.cache, k)
				evicted = true
			}
		}
		if !evicted {
			for k := range sc.cache {
				delete(sc.cache, k)
				break
			}
		}
	}
	sc.cache[siloID] = cachedDEK{dek: dek, expires: time.Now().Add(dekCacheTTL)}
	sc.mu.Unlock()
	return dek, nil
}

// evictDEK removes a silo's DEK from the cache and marks the silo shredded so
// no racing request can re-cache the destroyed key. MUST be called on
// crypto-shred.
func (sc *siloCrypto) evictDEK(siloID string) {
	sc.mu.Lock()
	delete(sc.cache, siloID)
	sc.shredded[siloID] = struct{}{}
	sc.mu.Unlock()
}

// fingerprint computes the stable DEK fingerprint: HMAC-SHA256(fpKey, dek).
func (sc *siloCrypto) fingerprint(dek []byte) string {
	m := hmac.New(sha256.New, sc.fpKey)
	m.Write(dek)
	return hex.EncodeToString(m.Sum(nil))
}

// sealContent encrypts a memory's text+metadata payload under the silo DEK.
// The doc id is bound as AAD so ciphertext cannot be replayed onto another id.
func sealContent(dek []byte, docID string, payload []byte) (string, error) {
	blob, err := gcmSeal(dek, payload, []byte("doc:"+docID))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// openContent decrypts a sealContent blob.
func openContent(dek []byte, docID, blobB64 string) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil {
		return nil, err
	}
	return gcmOpen(dek, blob, []byte("doc:"+docID))
}

// chainChecksum computes the tamper-evident chain link used by
// engagement_audit (and future per-silo ledgers):
// SHA256(prev_checksum || "\n" || row_payload). An empty prev marks genesis.
func chainChecksum(prev, rowPayload string) string {
	h := sha256.Sum256([]byte(prev + "\n" + rowPayload))
	return hex.EncodeToString(h[:])
}
