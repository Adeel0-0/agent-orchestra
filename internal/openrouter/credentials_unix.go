//go:build !windows

package openrouter

import (
	"errors"

	keyring "github.com/zalando/go-keyring"
)

// nativeStore uses go-keyring: Secret Service on Linux (same "service" and
// "username" attributes Python keyring searches), Keychain on macOS (same
// service/account). go-keyring has no file fallback.
type nativeStore struct{}

func (nativeStore) Get(service, account string) (string, bool, error) {
	value, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", false, nil
	}
	if errors.Is(err, keyring.ErrUnsupportedPlatform) {
		return "", false, &CredentialError{Message: "The OS keyring is unavailable."}
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (nativeStore) Set(service, account, value string) error {
	err := setNative(service, account, value)
	if errors.Is(err, keyring.ErrUnsupportedPlatform) {
		return &CredentialError{Message: "The OS keyring is unavailable."}
	}
	return err
}
