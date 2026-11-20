// Package main implements the cpa-meoo CLIProxyAPI dynamic plugin: a read-only
// panel for 秒悟 (Meoo) accounts — points, entitlement, the project each key owns
// and the project-level AK that the model gateway wants.
//
// ---------------------------------------------------------------------------
// What this plugin is, and why it exists at all
// ---------------------------------------------------------------------------
//
// Meoo's chat traffic is served by CPA's own openai-compatibility provider
// (api-keys -> openai-compatibility -> name: "秒悟"), whose project-level AK
// reaches https://api.meoo.host/meoo-ai/compatible-mode/v1. That arrangement
// gives the operator no readout of any kind: the host's quota card is driven by
// /v0/management/auth-files, and config-declared openai-compatibility
// credentials are not listable there (internal/api/handlers/management/
// auth_files.go isAuthFileListable returns false when the auth has neither a
// "path" attribute nor runtime_only, and synthesizer/config.go never sets
// either for these). So no host-side plugin can attach a card to those rows; the
// only honest place for the numbers is a plugin-owned panel — the same decision
// loomy recorded in docs/adr/ADR-0001.
//
// This plugin therefore:
//
//  1. keeps its own account store (%USERPROFILE%\.meoo-cpa\accounts.json), one
//     row per operator-pasted USER-level key (meoo_ak_...),
//  2. walks the documented discovery chain per account — identity -> projects ->
//     cloud -> environment (project-level AK) -> points -> entitlement — against
//     https://meoo.com/open/v1,
//  3. writes the discovered project id / project AK back into the account row
//     (so the gateway leg and the operator both have them), and
//  4. caches every reading behind a TTL (default 60 minutes) and serves the
//     panel from that cache, so opening or polling the panel costs nothing
//     upstream; one explicit 刷新数据 action forces a fresh walk.
//
// ---------------------------------------------------------------------------
// Two credential families, never interchangeable
// ---------------------------------------------------------------------------
//
//	用户级 meoo_ak_...            -> https://meoo.com/open/v1        (this plugin)
//	项目级 meoo_project_service_v1. -> https://api.meoo.host/...        (the gateway)
//
// Sending the wrong family is not a degraded read, it is a 401 that looks like a
// dead account; upstream.go refuses the mismatch locally, before any request
// leaves the machine, and says which family it wanted.
//
// ---------------------------------------------------------------------------
// Safety posture
// ---------------------------------------------------------------------------
//
// Both credentials are secrets. The full key is NEVER logged, NEVER echoed by
// the management API and NEVER rendered into the panel: the panel only ever sees
// key_tail (the last four characters) and the row id (a sha256 prefix). The
// upstream calls are read-only except for the two provisioning POSTs, and those
// fire only when the operator has left auto_provision at its default.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static int meoo_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void meoo_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// providerName is the host-facing plugin id. It is ALSO the plugin id used in
// every route and in plugins.configs.<id>, which is why it is a constant and not
// a build-time variable: the routes are string literals in the gate's view.
const providerName = "meoo"

// pluginLogoURL is declared in logo.go together with its provenance.

// version self-reports the plugin build. Date-epoch, matching the fleet
// standard, and locked byte-for-byte to the VERSION file by
// version_guard_test.go.
//
//	2026.11.13.2: first shipping version of the Meoo panel. (The 2026.11.13.1
//	baseline commit is already tagged as the tree's starting point, so the first
//	real release takes the next number — ship.ps1 refuses a tag that is not on
//	HEAD. Nothing about the tree differs from what 2026.11.13.1 would have been.)
//	2026.11.16.1: 积分条改画「剩下的」（与 loomy 面板同向），汇总条同口径。
//	2026.11.20.1: 截断改为码点安全。`truncate` 用 `s[:n]`（字节下标）切，而它切的是
//	operator 面对的中文文本（账号备注 label、身份显示名、上游错误、panic 详情）；
//	3 字节的汉字被切开后经 encoding/json 变成 U+FFFD 进卡片/日志。现在回退到前一个
//	码点边界（truncateUTF8Bytes），字节上限不变，结果永远合法 UTF-8；
//	clampProjectName 同样改为按码点切（64 字节上限不变，不再把半个汉字 POST 给上游）。
var version = "2026.11.20.1"

var hostAPI *C.cliproxy_host_api

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	// A1: the Go runtime cannot unwind a panic through the host's cgo frame, so a
	// panic here would abort the whole CPA process (abi_guard.go documents why).
	// Refusing to load (rc=1) is the honest failure: the host reports it, and a
	// rebuild fixes it — a dead process takes every channel down with it.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: plugin_init recovered a panic: %s", providerName, panicDetail(r))
			rc = 1
		}
	}()
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	if host != nil {
		checkHostABIVersion(uint32(host.abi_version))
	}
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	// The size_t -> C.int gate must run BEFORE GoBytes: a length >= 2^31
	// truncates negative there and makeslice panics ahead of any recover
	// installed deeper in the call chain. See abi_guard.go.
	if !abiRequestLenOK(uint64(requestLen)) {
		writeResponse(response, errorEnvelope("request_too_large", abiRequestTooLargeMessage(uint64(requestLen))))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	rc, raw := abiPluginCallBody(C.GoString(method), requestBytes)
	writeResponse(response, raw)
	return C.int(rc)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// No-op on purpose: the host dlclose()es this library right after its own
	// teardown begins, and touching Go runtime state here risks a cgo SIGSEGV.
}

// writeResponse copies the payload into the host's buffer.
func writeResponse(response *C.cliproxy_buffer, payload []byte) {
	if response == nil || len(payload) == 0 {
		return
	}
	ptr := C.CBytes(payload)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(payload))
}

// hostCall invokes a host RPC via the function-pointer table.
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.meoo_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	// The response leg runs the same size_t -> C.int gate as the request leg: a
	// length >= 2^31 would truncate negative inside GoBytes and panic (abi_guard.go).
	if resp.ptr != nil && resp.len > 0 && abiRequestLenOK(uint64(resp.len)) {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	} else if resp.ptr != nil && resp.len > 0 {
		out = nil
		log.Printf("%s: host call %s response length %d exceeds the ABI ceiling; response dropped", providerName, method, uint64(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.meoo_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, rc)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Method dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		startBackgroundRefresh()
		return okEnvelope(meooRegistration())
	case pluginabi.MethodManagementRegister:
		applyManagementRegistration(request)
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodPluginQuiesce:
		stopBackgroundRefresh()
		return okEnvelope(map[string]any{"quiesced": true})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// applyManagementRegistration reads the base paths the host offers.
//
// Decoded permissively on purpose: pluginapi.ManagementRegistrationRequest has no
// json tags, and the host has shipped both snake_case and camelCase spellings of
// these two fields. A silent empty value is not harmless — it would make every
// route in the panel 404 while the plugin still reported itself registered.
func applyManagementRegistration(raw []byte) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	if v := firstStringField(fields, "base_path", "basePath", "BasePath", "base"); v != "" {
		setManagementBasePath(v)
	}
	if v := firstStringField(fields, "resource_base_path", "resourceBasePath", "ResourceBasePath"); v != "" {
		setResourceBasePath(v)
	}
}

// firstStringField returns the first non-empty string among the candidate keys.
func firstStringField(fields map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := fields[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability mirrors the host's rpcCapabilities. Everything except
// ManagementAPI is false: this plugin owns no chat traffic, no credential store,
// no executor and no quota card (see the package comment for why a quota card is
// impossible for a config-declared provider).
type registrationCapability struct {
	ModelProvider bool `json:"model_provider"`
	AuthProvider  bool `json:"auth_provider"`
	Executor      bool `json:"executor"`
	ManagementAPI bool `json:"management_api"`
	QuotaProvider bool `json:"quota_provider"`
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
}

// meooConfigFields declares this plugin's configuration surface.
//
// LITERALS ON PURPOSE. Building this slice in a loop reads better, but it makes
// the fields invisible to the release gate's static config-key scan
// (dead-declaration.mjs CHECK 1 looks for `{Name: "..."}` literals inside a
// `[]pluginapi.ConfigField{` block), which would then report NOT_CHECKED — and an
// unchecked surface is exactly what that gate exists to prevent.
//
// `enabled` is deliberately NOT declared here (it is not part of the agreed
// surface) but IS still parsed and honoured: the host config block may carry it,
// the panel echoes it and /health reports it. See config.go.
var meooConfigFields = []pluginapi.ConfigField{
	{Name: "refresh_minutes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Cache lifetime and background refresh period, in minutes. Reads inside the window are served from cache with zero upstream calls. Default 60, clamped to 1..1440."},
	{Name: "auto_provision", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When an account owns no active project (or its cloud instance is not enabled yet), create/enable it automatically on the next walk. Default true. Set false to inspect-only."},
	{Name: "project_name", Type: pluginapi.ConfigFieldTypeString, Description: "Name given to a project this plugin creates for an account that has none. Default CPA-Model-Gateway."},
}

// meooRegistration declares management API + resource routes and nothing else:
// this plugin holds no chat traffic, no auth store and no executor.
func meooRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Meoo 秒悟",
			Version:          version,
			Author:           "xzy",
			GitHubRepository: "https://github.com/xzy/cpa-plugins",
			Logo:             pluginLogoURL,
			ConfigFields:     meooConfigFields,
		},
		Capabilities: registrationCapability{
			ModelProvider: false,
			AuthProvider:  false,
			Executor:      false,
			ManagementAPI: true,
			QuotaProvider: false,
			UsagePlugin:   false,
			Scheduler:     false,
		},
	}
}

// -----------------------------------------------------------------------------
// Background refresh
// -----------------------------------------------------------------------------

var (
	refreshOnce   sync.Once
	refreshStopCh = make(chan struct{})
	refreshStopMu sync.Mutex
	refreshClosed bool
)

// startBackgroundRefresh starts the periodic sweep exactly once.
//
// The sweep exists so the panel is never the thing that pays for a cold read: it
// fills the cache in the background, which leaves every panel render a pure
// cache hit. It is idempotent because the host calls register/reconfigure
// repeatedly, and a second sweep would multiply the upstream traffic.
func startBackgroundRefresh() {
	refreshOnce.Do(func() {
		safeGo("meoo-refresh-loop", func() {
			for {
				ttl := currentRefreshTTL()
				select {
				case <-refreshStopCh:
					return
				case <-timeAfter(ttl):
				}
				if !currentEnabled() {
					continue
				}
				// One sweep per period. force=true because the whole point of the
				// tick is to refresh readings the TTL has already expired.
				_, _ = refreshAll(true)
			}
		})
	})
}

// stopBackgroundRefresh ends the sweep (host quiesce). Idempotent.
func stopBackgroundRefresh() {
	refreshStopMu.Lock()
	defer refreshStopMu.Unlock()
	if refreshClosed {
		return
	}
	refreshClosed = true
	close(refreshStopCh)
}

// -----------------------------------------------------------------------------
// Envelopes
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func errorEnvelopeFor(err error) []byte {
	if err == nil {
		return errorEnvelope("internal_error", "unknown error")
	}
	return errorEnvelope("internal_error", err.Error())
}

// truncate bounds a string for a log line or an envelope. Credentials are never
// passed through here; this is for notes and diagnostics.
//
// 2026-11-20: the cut now lands on a rune boundary (truncateUTF8Bytes) instead of
// a byte index. Callers here hand in operator-facing prose — the account label
// (a human note), the identity display name, upstream error text, panic details —
// and the old "…" bound split CJK/emoji runs in half.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return truncateUTF8Bytes(s, n) + "…"
}

// truncateUTF8Bytes cuts s to at most limit BYTES, at a rune boundary.
//
// Why not s[:limit]: this is the shape that cut CJK notes in half elsewhere in
// the fleet (workbuddy A9, qoder/zcode note cards). A CJK rune is 3 bytes and an
// emoji is 4, so a byte index regularly lands INSIDE a rune; the tail bytes are
// then an orphan, encoding/json serializes them as U+FFFD, and the replacement
// character travels into the card / credential file and survives every later
// rewrite of the same text. Backing off to the previous rune start is the smaller
// loss: the byte ceiling (the thing that keeps a card to one line) is unchanged,
// and the result is always valid UTF-8.
//
// limit <= 0 yields "". A limit of at least utf8.UTFMax bytes always keeps one
// whole character rather than collapsing to "" — a caller that re-bounds a value
// must not be able to shrink it into nothing.
func truncateUTF8Bytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// The guarantee above, stated as a guard rather than as an observation: with
	// limit >= UTFMax a valid string can always spare one whole character, so a
	// walk-to-zero (only reachable for malformed input) must not return "".
	if cut == 0 && limit >= utf8.UTFMax {
		if _, size := utf8.DecodeRuneInString(s); size > 1 && size <= limit {
			cut = size
		}
	}
	return s[:cut]
}

// nowTimestamp renders the fleet's wire format for a wall-clock stamp:
// RFC3339 with the local offset (e.g. 2026-11-13T12:00:05+08:00).
func nowTimestamp(t time.Time) string {
	return t.Format("2006-01-02T15:04:05-07:00")
}

// nowPtr renders an optional stamp: absent (nil) stays absent, never "".
func nowPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	stamp := nowTimestamp(t)
	return &stamp
}
