package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// TestABIMaxBufferLenIsTheFleetCeiling pins the A2 gate's actual value.
//
// The 2026-11-12 fleet audit standardised this at 256 MiB: the old `1<<31-1` is
// only the point where C.int truncates, and a plugin willing to copy 2 GiB across
// the boundary has already lost. The ceiling MUST stay under 2^31 (or GoBytes can
// still be handed a negative length) and MUST be bounded (or an oversized request
// is an OOM instead of an error envelope).
func TestABIMaxBufferLenIsTheFleetCeiling(t *testing.T) {
	if abiMaxBufferLen > 256<<20 {
		t.Fatalf("abiMaxBufferLen = %d, want <= 256<<20 (fleet口径)", abiMaxBufferLen)
	}
	if abiMaxBufferLen >= 1<<31 {
		t.Fatalf("abiMaxBufferLen = %d, must stay below 2^31 so C.int cannot truncate", abiMaxBufferLen)
	}
	if abiMaxBufferLen != 256<<20 {
		t.Fatalf("abiMaxBufferLen = %d, want exactly 256<<20", abiMaxBufferLen)
	}

	if !abiRequestLenOK(0) {
		t.Fatal("a zero-length request is legal")
	}
	if !abiRequestLenOK(abiMaxBufferLen) {
		t.Fatal("the ceiling itself must be accepted")
	}
	if abiRequestLenOK(abiMaxBufferLen + 1) {
		t.Fatal("one byte over the ceiling must be refused (before C.GoBytes)")
	}
	if abiRequestLenOK(1<<31 - 1) {
		t.Fatal("2^31-1 must be refused: the ceiling is the fleet value, not the C.int limit")
	}
	if abiRequestLenOK(1 << 40) {
		t.Fatal("an absurd length must be refused")
	}
	if message := abiRequestTooLargeMessage(1 << 40); !strings.Contains(message, "abiMaxBufferLen") {
		t.Fatalf("the refusal message must name the constant: %s", message)
	}
}

// TestPanicDetailBoundsAndTypes: the ABI guard's log/envelope text must never be
// an unbounded expansion of an arbitrary panic value (which could be a whole
// struct with a credential inside).
func TestPanicDetailBoundsAndTypes(t *testing.T) {
	if got := panicDetail("boom"); got != "boom" {
		t.Fatalf("panicDetail(string) = %q", got)
	}
	if got := panicDetail(errString("nope")); got != "nope" {
		t.Fatalf("panicDetail(error) = %q", got)
	}
	if got := panicDetail(42); got != "int" {
		t.Fatalf("a non-string panic value must report only its type, got %q", got)
	}
	if got := panicDetail(struct{ Secret string }{"hidden"}); !strings.Contains(got, "struct") {
		t.Fatalf("a struct panic value must report its type only, got %q", got)
	}
	long := panicDetail(strings.Repeat("x", 4000))
	if len(long) > 244 {
		t.Fatalf("panicDetail did not bound its input: %d bytes", len(long))
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("a truncated detail must show that it was truncated: %q", long[len(long)-8:])
	}
}

// errString is a minimal error so this file needs no fmt import.
type errString string

func (e errString) Error() string { return string(e) }

// TestEnvelopeShapes pins the two JSON documents the host parses: a success
// envelope carrying `result`, and a failure envelope carrying a coded error.
func TestEnvelopeShapes(t *testing.T) {
	okRaw, errOK := okEnvelope(map[string]any{"quiesced": true})
	if errOK != nil {
		t.Fatalf("okEnvelope: %v", errOK)
	}
	var okEnvelopeDecoded envelope
	if errJSON := json.Unmarshal(okRaw, &okEnvelopeDecoded); errJSON != nil {
		t.Fatalf("decode okEnvelope: %v", errJSON)
	}
	if !okEnvelopeDecoded.OK || okEnvelopeDecoded.Error != nil {
		t.Fatalf("okEnvelope shape: %s", string(okRaw))
	}
	if !strings.Contains(string(okEnvelopeDecoded.Result), `"quiesced":true`) {
		t.Fatalf("okEnvelope result = %s", string(okEnvelopeDecoded.Result))
	}

	var errorEnvelopeDecoded envelope
	if errJSON := json.Unmarshal(errorEnvelope("unknown_method", "nope"), &errorEnvelopeDecoded); errJSON != nil {
		t.Fatalf("decode errorEnvelope: %v", errJSON)
	}
	if errorEnvelopeDecoded.OK || errorEnvelopeDecoded.Error == nil {
		t.Fatal("errorEnvelope must carry ok=false and an error object")
	}
	if errorEnvelopeDecoded.Error.Code != "unknown_method" || errorEnvelopeDecoded.Error.Message != "nope" {
		t.Fatalf("errorEnvelope payload = %+v", errorEnvelopeDecoded.Error)
	}

	if got := errorEnvelopeFor(nil); !strings.Contains(string(got), "unknown error") {
		t.Fatalf("errorEnvelopeFor(nil) = %s", string(got))
	}
	if got := errorEnvelopeFor(errString("disk on fire")); !strings.Contains(string(got), "disk on fire") {
		t.Fatalf("errorEnvelopeFor(err) = %s", string(got))
	}
}

// TestABIPluginCallBodyRecoversPanic is the process-liveness gate: a panic that
// escapes cliproxyPluginCall aborts the WHOLE CPA process (the Go runtime cannot
// unwind through the host's cgo frame, and the host installs no recover of its
// own). The guarded body must instead answer rc=1 with a coded envelope.
func TestABIPluginCallBodyRecoversPanic(t *testing.T) {
	previous := abiPanicHook
	t.Cleanup(func() { abiPanicHook = previous })

	abiPanicHook = func(string, []byte) { panic("kaboom") }
	rc, out := abiPluginCallBody("plugin.register", nil)
	if rc != 1 {
		t.Fatalf("a panic must answer rc=1, got %d (%s)", rc, string(out))
	}
	if !strings.Contains(string(out), `"code":"plugin_panic"`) || !strings.Contains(string(out), "kaboom") {
		t.Fatalf("the panic envelope must be diagnosable: %s", string(out))
	}
	if !strings.Contains(string(out), `"ok":false`) {
		t.Fatalf("the panic envelope must be a failure envelope: %s", string(out))
	}

	// A non-string panic value must not be expanded into the envelope (it could be
	// a whole struct with a credential inside).
	abiPanicHook = func(string, []byte) { panic(struct{ Secret string }{"hidden"}) }
	_, out = abiPluginCallBody("plugin.register", nil)
	if strings.Contains(string(out), "hidden") {
		t.Fatalf("a struct panic value leaked its contents: %s", string(out))
	}

	// The happy path and the handler-error path, both through the same body.
	abiPanicHook = nil
	rc, out = abiPluginCallBody("plugin.does_not_exist", nil)
	if rc != 0 || !strings.Contains(string(out), `"code":"unknown_method"`) {
		t.Fatalf("the normal path = rc %d / %s", rc, string(out))
	}
	rc, out = abiPluginCallBody("management.handle", []byte("this is not JSON"))
	if rc != 1 || !strings.Contains(string(out), `"code":"internal_error"`) {
		t.Fatalf("a handler error must become an envelope, got rc=%d %s", rc, string(out))
	}
}

// TestABIGuardHealthReportsTheLatch.
func TestABIGuardHealthReportsTheLatch(t *testing.T) {
	health := abiGuardHealth()
	version, okVersion := health["abi_version"].(uint32)
	if !okVersion || version != pluginabi.ABIVersion {
		t.Fatalf("abi_version = %v, want %d", health["abi_version"], pluginabi.ABIVersion)
	}
	if pluginabi.ABIVersion != 1 {
		t.Fatalf("the host resolves this plugin at ABI version 1, got %d", pluginabi.ABIVersion)
	}
	// 2026-11-15 机队抬钉：SDK 快照 v8.0.16 → v8.0.72，上报档位随 SDK 常量走到 10。
	// 断言改为"跟随 SDK 常量且不低于 10"，防止意外回退到旧快照。
	if pluginabi.SchemaVersion < 10 {
		t.Fatalf("schema_version = %d, want >= 10 (SDK snapshot v8.0.72 fleet baseline)", pluginabi.SchemaVersion)
	}
	if _, present := health["abi_version_mismatch"]; !present {
		t.Fatalf("the ABI mismatch latch must be reported: %v", health)
	}
}

// TestTruncateIsBounded.
func TestTruncateIsBounded(t *testing.T) {
	if got := truncate("  spaced  ", 40); got != "spaced" {
		t.Fatalf("truncate trimmed = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Fatalf("truncate must not touch a value that fits: %q", got)
	}
}

// TestNowTimestampWireFormat pins the stamp format the panel parses.
func TestNowTimestampWireFormat(t *testing.T) {
	stamp := nowTimestamp(time.Now())
	if len(stamp) != 25 {
		t.Fatalf("timestamp %q must be RFC3339 with an offset (25 chars)", stamp)
	}
	if stamp[4] != '-' || stamp[7] != '-' || stamp[10] != 'T' || stamp[13] != ':' {
		t.Fatalf("timestamp %q is not RFC3339", stamp)
	}
	if pointer := nowPtr(time.Now()); pointer == nil || len(*pointer) != 25 {
		t.Fatalf("nowPtr = %v, want an RFC3339 stamp", pointer)
	}
	if zero := nowPtr(time.Time{}); zero != nil {
		t.Fatalf("an unset stamp must serialise as null, got %q", *zero)
	}
}
