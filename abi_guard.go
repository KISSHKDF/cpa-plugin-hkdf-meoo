// abi_guard.go — panic containment at the C ABI boundary.
//
// WHY THIS FILE EXISTS (audit 2026-10-19, finding H1):
//
//	The Go runtime cannot unwind a panic through a cgo call frame. A panic that
//	escapes cliproxyPluginCall does not kill the plugin -- it aborts the WHOLE CPA
//	process, taking every account's forwarding down with it. The host cannot save
//	us either: internal/pluginhost/loader_windows.go calls the plugin through
//	syscall.SyscallN with no recover on the other side (verified against upstream
//	v8.0.33, commit 2422c6b0).
//	Before this file this tree contained ZERO recover() calls anywhere, so any
//	slice index, nil dereference or failed type assertion in any handler was a
//	latent process killer.
//
// A2 (length gate) lives here because it must run BEFORE C.GoBytes: C.int is 32-bit
// signed while size_t is 64-bit unsigned, so a length >= 2^31 truncates to a
// negative int and GoBytes panics on makeslice -- in the entry point, ahead of any
// recover installed deeper. The ceiling itself is the fleet's 256 MiB (see
// abiMaxBufferLen), asserted by abi_guard_test.go.
//
// WHAT THIS DOES NOT FIX: panics on background goroutines. This tree starts them
// only through goroutine_guard.go's safeGo/safeCall, which carry their own recover
// — the static count test pins that.

package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// abiMaxBufferLen is the largest request/response this plugin will copy across the
// ABI boundary.
//
// 256 MiB is the fleet口径 (T1 #2283), not an accident: 2^31-1 is only the point
// where C.int truncates, and a plugin that happily allocates 2 GiB of host
// memory to answer one call has already lost. Nothing legitimate gets near
// 256 MiB, so the only behaviour this changes is "would have panicked / would
// have OOM'd the host" -> "returns an error envelope".
const abiMaxBufferLen = 256 << 20

// abiRequestLenOK reports whether a length may cross into C.int losslessly AND
// stays inside the fleet's 256 MiB ceiling. size_t itself is unsigned, so the
// hard failure it prevents is >= 2^31 truncating negative.
func abiRequestLenOK(n uint64) bool { return n <= abiMaxBufferLen }

// abiRequestTooLargeMessage is the envelope body for an over-length request. It
// carries the numeric length only -- at that point not one byte of the request has
// been pulled into the Go heap, and it must stay that way.
func abiRequestTooLargeMessage(n uint64) string {
	return fmt.Sprintf("request length %d exceeds the %d-byte ABI ceiling (abiMaxBufferLen)", n, uint64(abiMaxBufferLen))
}

// abiPanicHook is a test seam: when non-nil it runs before dispatch. Production
// leaves it nil (see abi_guard_test.go, which drives the real C entry through it).
var abiPanicHook func(method string, request []byte)

// panicDetail flattens and bounds a panic value. This tree has no shared redactor,
// so the bound is the protection: an envelope or log line must never carry an
// unbounded expansion of an arbitrary value (which could be a whole session struct
// with the cookie inside it). Non-string/error panic values therefore report only
// their type — their %v expansion is exactly the leak channel this guard exists
// to close.
func panicDetail(r any) string {
	switch r.(type) {
	case string, error:
		return truncate(fmt.Sprint(r), 240)
	default:
		return truncate(fmt.Sprintf("%T", r), 240)
	}
}

// abiPluginCallBody is the guarded core behind cliproxyPluginCall: one recover per
// ABI call, panic becomes a plugin_panic error envelope with rc=1. The host treats
// "rc != 0 with a valid envelope" as a plugin error -- so the process stays alive
// and this one call turns into a diagnosable failure instead of a crash.
//
// The envelope body carries the entry function name, the RPC method and the
// sanitized panic summary. It never carries the request body.
func abiPluginCallBody(method string, requestBytes []byte) (rc int, out []byte) {
	defer func() {
		if r := recover(); r != nil {
			detail := panicDetail(r)
			log.Printf("%s: plugin_panic recovered at ABI entry cliproxyPluginCall (method=%s): %s", providerName, method, detail)
			rc = 1
			out = errorEnvelope("plugin_panic", fmt.Sprintf("cliproxyPluginCall recovered a panic while handling %s: %s", method, detail))
		}
	}()
	if hook := abiPanicHook; hook != nil {
		hook(method, requestBytes)
	}
	raw, errHandle := handleMethod(method, requestBytes)
	if errHandle != nil {
		return 1, errorEnvelopeFor(errHandle)
	}
	return 0, raw
}

// --- A3: host ABI version check (warn-only) -----------------------------------

var (
	abiGuardMu      sync.Mutex
	abiMismatchSeen bool
)

// checkHostABIVersion compares the host's reported abi_version with the SDK constant
// this build compiled against. A mismatch is latched (so health output can report it)
// and logged once. It does NOT refuse to load: refusing would remove the plugin's
// entire capability set over a signal that usually still works. Fail-open is the
// right default; the latch makes it visible.
func checkHostABIVersion(hostVersion uint32) bool {
	if hostVersion == pluginabi.ABIVersion {
		return true
	}
	abiGuardMu.Lock()
	abiMismatchSeen = true
	abiGuardMu.Unlock()
	log.Printf("%s: abi_version_mismatch host=%d plugin(SDK)=%d — RPC contract may differ; loading anyway (warn-only)", providerName, hostVersion, pluginabi.ABIVersion)
	return false
}

// abiGuardHealth is the read-back of the A3 state.
func abiGuardHealth() map[string]any {
	abiGuardMu.Lock()
	defer abiGuardMu.Unlock()
	return map[string]any{
		"abi_version":          pluginabi.ABIVersion,
		"abi_version_mismatch": abiMismatchSeen,
	}
}
