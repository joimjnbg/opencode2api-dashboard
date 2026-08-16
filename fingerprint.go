package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
)

// deviceFingerprint is a stable fake client identity bound to one account key.
// Reusing the same identity across requests makes the upstream believe each
// account comes from its own physical device, sidestepping single-device
// multi-account rate limiting.
type deviceFingerprint struct {
	MachineID       string `json:"x-machine-id"`
	VSCodeMachineID string `json:"vscode-machine-id"`
}

// fingerprintStore persists fingerprints on disk so that restarts keep the
// same identity per key. The store is keyed by a SHA-256 digest of the API key
// so the raw secrets never touch the disk file.
type fingerprintStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]deviceFingerprint
}

func newFingerprintStore(path string) *fingerprintStore {
	store := &fingerprintStore{path: path, entries: map[string]deviceFingerprint{}}
	store.load()
	return store
}

func fingerprintKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *fingerprintStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var stored map[string]deviceFingerprint
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	s.entries = stored
}

func (s *fingerprintStore) save() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, data, 0o600)
}

// ForKey returns the fingerprint bound to a key, generating and persisting one
// on first use.
func (s *fingerprintStore) ForKey(secret string) deviceFingerprint {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fingerprintKey(secret)
	if fp, ok := s.entries[key]; ok && fp.MachineID != "" {
		return fp
	}
	fp := deviceFingerprint{
		MachineID:       newUUID(),
		VSCodeMachineID: newUUID(),
	}
	s.entries[key] = fp
	s.save()
	return fp
}

// newUUID returns a standards-compliant random UUID v4 string.
func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(raw)
}

func formatUUID(raw [16]byte) string {
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
