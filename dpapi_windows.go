//go:build windows

// dpapi_windows.go — the Windows DPAPI half of the cookie decryption.
//
// Only two calls are needed, so this file uses lazy DLL resolution from the
// standard library (`syscall.NewLazyDLL`) instead of pulling in
// golang.org/x/sys/windows: the plugin's go.mod deliberately carries just the
// host SDK and YAML, and a release rebuild must not need new modules.
//
// DPAPI scope is CurrentUser (the same user the CPA host runs as). That is the
// only scope that can unwrap a Chromium key, and it is also the reason this
// whole feature is local-only: the ciphertext is already readable by this user.
//
// Ported verbatim from the 灵犀 tree (work/cpa-plugins/lingxi).
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// b64Std is shared by chromium_cookie.go and the session-cookie sources.
var b64Std = base64.StdEncoding

// dataBlob mirrors the Win32 DATA_BLOB used by CryptProtectData/CryptUnprotectData.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

// dpapiUnprotect unwraps one DPAPI blob for the current user.
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty dpapi blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	ret, _, lastErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description (out)
		0, // optional entropy
		0, // reserved
		0, // prompt struct
		0, // flags: current-user, no UI
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", lastErr)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.pbData == nil || out.cbData == 0 {
		return nil, errors.New("CryptUnprotectData returned no data")
	}
	plain := make([]byte, out.cbData)
	copy(plain, unsafe.Slice(out.pbData, out.cbData))
	return plain, nil
}
