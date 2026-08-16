package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintStorePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprints.json")

	store := newFingerprintStore(path)
	key := "sk-test-key-123"
	fp1 := store.ForKey(key)

	if fp1.MachineID == "" || fp1.VSCodeMachineID == "" {
		t.Fatalf("fingerprint fields empty: %+v", fp1)
	}
	if fp1.MachineID == fp1.VSCodeMachineID {
		t.Errorf("machine id and vscode id should differ")
	}

	// Second call returns the same identity.
	if fp2 := store.ForKey(key); fp2 != fp1 {
		t.Errorf("same key returned different fingerprint: %+v vs %+v", fp1, fp2)
	}

	// Reload from disk and confirm stability across "restarts".
	reloaded := newFingerprintStore(path)
	if fp3 := reloaded.ForKey(key); fp3 != fp1 {
		t.Errorf("fingerprint not persisted: reloaded %+v != %+v", fp3, fp1)
	}

	// A different key gets a different identity.
	if fp4 := store.ForKey("sk-other-key"); fp4 == fp1 {
		t.Errorf("different keys should get distinct fingerprints")
	}
}

func TestFingerprintKeyHashesSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprints.json")
	store := newFingerprintStore(path)
	secret := "sk-super-secret"
	store.ForKey(secret)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	got := string(data)
	if contains(got, secret) {
		t.Errorf("raw secret leaked into fingerprint file")
	}
	// The key field is a SHA-256 digest (64 hex chars).
	if !contains(got, fingerprintKey(secret)) {
		t.Errorf("expected hashed key in file, got: %s", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && (len(haystack) >= len(needle)) && (indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestNewUUIDFormat(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("uuid length = %d", len(id))
	}
	// Version nibble at position 14 must be '4'; variant nibble at 19 in (8..b).
	if id[14] != '4' {
		t.Errorf("uuid version = %c, want 4", id[14])
	}
	variant := id[19]
	if variant < '8' || variant > 'b' {
		t.Errorf("uuid variant = %c, want 8-b", variant)
	}
}
