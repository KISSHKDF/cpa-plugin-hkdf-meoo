// chromium_cookie.go — Chromium cookie-value decryption, shared by every source
// that reads a Chromium-family store (here: the Edge user-data profiles the
// operator keeps a logged-in 秒悟 session in).
//
// The scheme is Chromium's classic "v10": `Local State → os_crypt.encrypted_key`
// is a DPAPI-wrapped 32-byte AES key, and every cookie value is
//
//	"v10" || nonce(12) || AES-256-GCM ciphertext || tag(16)
//
// where the plaintext starts with a 32-byte binding prefix sha256(host_key) that
// must be stripped. Measured on this box 2026-11-11 against the 灵犀 client
// (`Local State` has no `app_bound_encrypted_key`, i.e. NOT the v20 app-bound
// scheme), and independently re-measured 2026-11-21 against an Edge profile
// holding a live 秒悟 session: the same key layout decrypts meoo.com's
// `oneday_sid` / `login_oneday_ticket` and the sha256(host_key) prefix matches.
//
// Ported verbatim from the 灵犀 tree (work/cpa-plugins/lingxi), which proved the
// scheme on this machine first. Nothing about the format is 灵犀-specific; only
// the caller is.
//
// Security posture: this file only ever decrypts data the same Windows user can
// already read with two shell commands. Nothing is logged: callers carry lengths
// and hashes, never the value (see session_cookie.go).
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// cookieHostBinding is the plaintext prefix length Chromium binds to the cookie's
// host key (sha256 of it, so a stolen blob cannot be replayed on another host).
const cookieHostBinding = sha256.Size

var (
	errCookieNotV10       = errors.New("cookie value is not a v10/v11 blob")
	errCookieHostMismatch = errors.New("cookie value is bound to a different host_key")
)

// chromiumLocalState holds only the one field we need from `Local State`.
type chromiumLocalState struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
		AppBoundKey  string `json:"app_bound_encrypted_key"`
	} `json:"os_crypt"`
}

// loadChromiumAESKey resolves the 32-byte AES key from a Chromium `Local State`.
//
// `app_bound_encrypted_key` present means the profile uses Chrome's newer
// app-bound encryption, which cannot be unwrapped without the app's own signing
// identity. That is reported as an explicit error (never a silent empty key) so
// the operator sees "this profile cannot be read" instead of "no accounts".
func loadChromiumAESKey(localStatePath string) ([]byte, error) {
	raw, errRead := os.ReadFile(localStatePath)
	if errRead != nil {
		return nil, errRead
	}
	var state chromiumLocalState
	if errUnmarshal := json.Unmarshal(raw, &state); errUnmarshal != nil {
		return nil, fmt.Errorf("local state parse: %w", errUnmarshal)
	}
	if state.OSCrypt.EncryptedKey == "" {
		if state.OSCrypt.AppBoundKey != "" {
			return nil, errors.New("chromium profile uses app-bound encryption (v20); it cannot be decrypted")
		}
		return nil, errors.New("chromium local state has no os_crypt.encrypted_key")
	}
	blob, errDecode := base64Decode(state.OSCrypt.EncryptedKey)
	if errDecode != nil {
		return nil, fmt.Errorf("encrypted_key decode: %w", errDecode)
	}
	// Chromium prefixes the stored key with the ASCII marker "DPAPI".
	if len(blob) <= 5 || string(blob[:5]) != "DPAPI" {
		return nil, errors.New("encrypted_key is not DPAPI-wrapped")
	}
	key, errUnprotect := dpapiUnprotect(blob[5:])
	if errUnprotect != nil {
		return nil, fmt.Errorf("dpapi: %w", errUnprotect)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("unexpected aes key length %d", len(key))
	}
	return key, nil
}

// decryptChromiumCookieValue unwraps one encrypted_value for a known host_key.
// The host binding is verified, not assumed: a blob lifted from another host is
// rejected with errCookieHostMismatch instead of being handed to the upstream.
func decryptChromiumCookieValue(aesKey, blob []byte, hostKey string) (string, error) {
	if len(blob) < 3+12+16 {
		return "", errCookieNotV10
	}
	prefix := string(blob[:3])
	if prefix != "v10" && prefix != "v11" {
		return "", fmt.Errorf("%w (prefix %q)", errCookieNotV10, prefix)
	}
	nonce := blob[3:15]
	sealed := blob[15:] // ciphertext || tag, the shape cipher.AEAD.Open wants
	block, errBlock := aes.NewCipher(aesKey)
	if errBlock != nil {
		return "", errBlock
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", errGCM
	}
	plain, errOpen := aead.Open(nil, nonce, sealed, nil)
	if errOpen != nil {
		return "", fmt.Errorf("aes-gcm open: %w", errOpen)
	}
	if len(plain) < cookieHostBinding {
		return "", errCookieNotV10
	}
	want := sha256.Sum256([]byte(hostKey))
	if !equalBytes(plain[:cookieHostBinding], want[:]) {
		return "", errCookieHostMismatch
	}
	return string(plain[cookieHostBinding:]), nil
}

// equalBytes is a constant-time-ish comparison; the values here are public
// hashes, so the only goal is avoiding a length panic.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// base64Decode is a thin wrapper so this file has no direct encoding/base64
// import churn across the two call sites.
func base64Decode(s string) ([]byte, error) { return b64Std.DecodeString(s) }
