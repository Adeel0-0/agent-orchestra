//go:build windows

package openrouter

import (
	"errors"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows Credential Manager, in the layout Python's keyring WinVaultKeyring uses
// (go-keyring uses a different target name and blob encoding, so it cannot read
// a key Python saved):
//
//   - generic credential, TargetName = service, UserName = account,
//     CredentialBlob = UTF-16LE, persisted for the user (CRED_PERSIST_ENTERPRISE);
//   - when the service target belongs to another user name, the fallback
//     target "account@service" is read instead.

const (
	credTypeGeneric       = 1
	credPersistEnterprise = 3
)

type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32      = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW = advapi32.NewProc("CredReadW")
	procCredWrite = advapi32.NewProc("CredWriteW")
	procCredFree  = advapi32.NewProc("CredFree")
)

// storedCredential is what one target holds.
type storedCredential struct {
	user string
	blob []byte
}

// Low-level access, replaceable in tests so they never touch the real store.
var (
	credRead  = winCredRead
	credWrite = winCredWrite
)

func winCredRead(target string) (*storedCredential, error) {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}
	var cred *credentialW
	r, _, callErr := procCredReadW.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r == 0 {
		if errors.Is(callErr, syscall.Errno(windows.ERROR_NOT_FOUND)) {
			return nil, nil
		}
		return nil, callErr
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	blob := make([]byte, cred.CredentialBlobSize)
	if cred.CredentialBlobSize > 0 {
		copy(blob, unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	}
	return &storedCredential{user: windows.UTF16PtrToString(cred.UserName), blob: blob}, nil
}

func winCredWrite(target, user string, blob []byte) error {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	userPtr, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	comment, _ := windows.UTF16PtrFromString("Stored by Agent Orchestra")
	cred := credentialW{Type: credTypeGeneric, TargetName: targetPtr, Comment: comment, UserName: userPtr,
		Persist: credPersistEnterprise, CredentialBlobSize: uint32(len(blob))}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}
	r, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return callErr
	}
	return nil
}

type nativeStore struct{}

func compoundName(account, service string) string { return account + "@" + service }

func (nativeStore) Get(service, account string) (string, bool, error) {
	cred, err := credRead(service)
	if err != nil {
		return "", false, err
	}
	if cred == nil || (account != "" && cred.user != account) {
		cred, err = credRead(compoundName(account, service))
		if err != nil {
			return "", false, err
		}
	}
	if cred == nil {
		return "", false, nil
	}
	return decodeBlob(cred.blob), true, nil
}

// Set writes the service target. A credential there that belongs to another user
// name is first preserved under its compound name, as Python keyring does.
// (Python also copies our own previous key there; that stale copy is not made.)
func (nativeStore) Set(service, account, value string) error {
	existing, err := credRead(service)
	if err != nil {
		return err
	}
	if existing != nil && existing.user != account {
		if err := credWrite(compoundName(existing.user, service), existing.user, existing.blob); err != nil {
			return err
		}
	}
	return credWrite(service, account, encodeBlob(value))
}

// encodeBlob is UTF-16LE without a BOM (win32ctypes/pywin32 CredWrite).
func encodeBlob(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, 0, 2*len(units))
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// decodeBlob is keyring's DecodingCredential.value: UTF-16 (a BOM selects the
// byte order, otherwise little-endian), falling back to UTF-8.
func decodeBlob(blob []byte) string {
	if len(blob)%2 == 0 {
		bigEndian := false
		data := blob
		if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
			data = data[2:]
		} else if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
			data, bigEndian = data[2:], true
		}
		units := make([]uint16, len(data)/2)
		for i := range units {
			if bigEndian {
				units[i] = uint16(data[2*i])<<8 | uint16(data[2*i+1])
			} else {
				units[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
			}
		}
		if validUTF16(units) {
			return string(utf16.Decode(units))
		}
	}
	if utf8.Valid(blob) {
		return string(blob)
	}
	return string([]rune(string(blob)))
}

func validUTF16(units []uint16) bool {
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u <= 0xDBFF:
			if i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
				return false
			}
			i++
		case u >= 0xDC00 && u <= 0xDFFF:
			return false
		}
	}
	return true
}
