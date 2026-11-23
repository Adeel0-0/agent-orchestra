package openrouter

import (
	"errors"
	"strings"
	"unicode"
)

// The OpenRouter key lives only in the native OS keyring, never in project
// config. These names match orchestra/players/credentials.py, so a key saved
// by either implementation is read by the other.
const (
	KeyringService = "agent-orchestra"
	KeyringAccount = "openrouter"
)

// CredentialError is a keyring failure. Messages never contain the key.
type CredentialError struct{ Message string }

func (e *CredentialError) Error() string { return e.Message }

// secretStore is the native keyring (replaced by a memory store in tests).
type secretStore interface {
	// Get returns the secret, or found=false when none is saved.
	Get(service, account string) (value string, found bool, err error)
	Set(service, account, value string) error
}

var vault secretStore = nativeStore{}

// GetKey returns the saved key, or "" when none is saved.
func GetKey() (string, error) {
	value, found, err := vault.Get(KeyringService, KeyringAccount)
	if err != nil {
		var ce *CredentialError
		if errors.As(err, &ce) {
			return "", ce
		}
		return "", &CredentialError{Message: "Unable to read the OpenRouter key from the OS keyring."}
	}
	if !found {
		return "", nil
	}
	return value, nil
}

// SetKey validates and saves the key, replacing any saved one.
func SetKey(value string) error {
	if strings.TrimSpace(value) == "" || strings.IndexFunc(value, isPySpace) >= 0 {
		return &CredentialError{Message: "The API key must be non-empty and contain no whitespace."}
	}
	if err := vault.Set(KeyringService, KeyringAccount, value); err != nil {
		var ce *CredentialError
		if errors.As(err, &ce) {
			return ce
		}
		return &CredentialError{Message: "Unable to save the OpenRouter key in the OS keyring."}
	}
	return nil
}

// KeyPresent reports whether a key is saved (doctor and settings). It checks
// presence only, not credit or model access.
func KeyPresent() (bool, error) {
	key, err := GetKey()
	return key != "", err
}

// isPySpace is Python's str.isspace for one character.
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}
