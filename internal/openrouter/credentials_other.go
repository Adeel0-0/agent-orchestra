//go:build !windows && !darwin

package openrouter

import keyring "github.com/zalando/go-keyring"

func setNative(service, account, value string) error {
	return keyring.Set(service, account, value)
}
