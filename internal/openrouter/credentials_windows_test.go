//go:build windows

package openrouter

import (
	"testing"
)

// fakeCredStore replaces the Win32 calls so tests never touch the real store.
func fakeCredStore(t *testing.T) map[string]*storedCredential {
	t.Helper()
	store := map[string]*storedCredential{}
	oldRead, oldWrite := credRead, credWrite
	credRead = func(target string) (*storedCredential, error) { return store[target], nil }
	credWrite = func(target, user string, blob []byte) error {
		store[target] = &storedCredential{user: user, blob: append([]byte(nil), blob...)}
		return nil
	}
	t.Cleanup(func() { credRead, credWrite = oldRead, oldWrite })
	return store
}

func TestWindowsVaultUsesPythonKeyringLayout(t *testing.T) {
	store := fakeCredStore(t)
	if err := (nativeStore{}).Set(KeyringService, KeyringAccount, "sk-é"); err != nil {
		t.Fatal(err)
	}
	got := store["agent-orchestra"]
	want := []byte{'s', 0, 'k', 0, '-', 0, 0xE9, 0}
	if got == nil || got.user != "openrouter" || string(got.blob) != string(want) {
		t.Fatalf("stored %+v", got)
	}
	if value, found, err := (nativeStore{}).Get(KeyringService, KeyringAccount); err != nil || !found || value != "sk-é" {
		t.Fatalf("get %q %v %v", value, found, err)
	}
}

func TestWindowsVaultReadsPythonCompoundTargetAndKeepsOtherUser(t *testing.T) {
	store := fakeCredStore(t)
	store["agent-orchestra"] = &storedCredential{user: "someone-else", blob: encodeBlob("theirs")}
	store["openrouter@agent-orchestra"] = &storedCredential{user: "openrouter", blob: encodeBlob("ours")}
	if value, found, _ := (nativeStore{}).Get(KeyringService, KeyringAccount); !found || value != "ours" {
		t.Fatalf("get %q %v", value, found)
	}
	if err := (nativeStore{}).Set(KeyringService, KeyringAccount, "new"); err != nil {
		t.Fatal(err)
	}
	if moved := store["someone-else@agent-orchestra"]; moved == nil || decodeBlob(moved.blob) != "theirs" {
		t.Fatalf("other credential not preserved: %+v", moved)
	}
	if value, _, _ := (nativeStore{}).Get(KeyringService, KeyringAccount); value != "new" {
		t.Fatalf("get %q", value)
	}
}

func TestDecodeBlobFallsBackToUTF8(t *testing.T) {
	if got := decodeBlob([]byte("abc")); got != "abc" {
		t.Fatalf("%q", got)
	}
	if got := decodeBlob([]byte{0xFF, 0xFE, 'k', 0}); got != "k" {
		t.Fatalf("BOM %q", got)
	}
	if value, found, err := (nativeStore{}).Get("agent-orchestra-test-missing-"+t.Name(), "nobody"); err != nil || found || value != "" {
		// Reads a target that does not exist: proves ERROR_NOT_FOUND maps to "no key".
		t.Fatalf("missing target: %q %v %v", value, found, err)
	}
}
