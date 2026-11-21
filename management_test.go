package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callRoute drives handleManagement the way the host does: an RPC envelope whose
// request carries the FULL request path, and whose Body is raw bytes (base64 on
// the wire because pluginapi.ManagementRequest declares []byte).
func callRoute(t *testing.T, method, fullPath string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	request := map[string]any{"method": method, "path": fullPath}
	if body != nil {
		request["body"] = body
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	out, errHandle := handleManagement(raw)
	if errHandle != nil {
		t.Fatalf("handleManagement(%s %s): %v", method, fullPath, errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("envelope not ok for %s %s: %s", method, fullPath, string(out))
	}
	var resp pluginapi.ManagementResponse
	if errResp := json.Unmarshal(env.Result, &resp); errResp != nil {
		t.Fatalf("decode management response: %v", errResp)
	}
	return resp
}

// payloadMap decodes a route's JSON body into a generic map.
func payloadMap(t *testing.T, resp pluginapi.ManagementResponse) map[string]any {
	t.Helper()
	var payload map[string]any
	if errJSON := json.Unmarshal(resp.Body, &payload); errJSON != nil {
		t.Fatalf("route body is not JSON (%s): %v", truncate(string(resp.Body), 200), errJSON)
	}
	return payload
}

// TestManagementRegistrationMirrorsUseLowercaseTags is the regression for the
// silent 404 incident: pluginapi.ManagementRoute has no json tags, so serialising
// it emits "Method"/"Path"/"Menu" while the host decodes the lower-case names. The
// plugin would register successfully and every declared route would 404.
func TestManagementRegistrationMirrorsUseLowercaseTags(t *testing.T) {
	raw, errMarshal := json.Marshal(managementRegistration())
	if errMarshal != nil {
		t.Fatalf("marshal registration: %v", errMarshal)
	}
	encoded := string(raw)
	for _, needle := range []string{`"method":"GET"`, `"method":"POST"`, `"path":"/plugins/meoo/`, `"menu":"Meoo 秒悟"`} {
		if !strings.Contains(encoded, needle) {
			t.Errorf("registration JSON is missing %s: %s", needle, encoded)
		}
	}
	for _, forbidden := range []string{`"Method"`, `"Path"`, `"Menu"`} {
		if strings.Contains(encoded, forbidden) {
			t.Errorf("registration JSON must not carry the PascalCase key %s: %s", forbidden, encoded)
		}
	}
}

// TestManagementRoutesAreExactlyTheAgreedSurface pins §5: eight routes, one
// resource with a menu label, and NO use of the host-reserved /quota path.
func TestManagementRoutesAreExactlyTheAgreedSurface(t *testing.T) {
	registration := managementRegistration()

	got := make([]string, 0, len(registration.Routes))
	for _, route := range registration.Routes {
		got = append(got, route.Method+" "+route.Path)
	}
	want := []string{
		"GET /plugins/meoo/accounts",
		"POST /plugins/meoo/refresh",
		"POST /plugins/meoo/accounts/add",
		"POST /plugins/meoo/accounts/remove",
		"GET /plugins/meoo/models",
		"POST /plugins/meoo/daily-login",
		"GET /plugins/meoo/daily-login",
		"POST /plugins/meoo/daily-login/session",
		"DELETE /plugins/meoo/daily-login/session",
		"POST /plugins/meoo/daily-login/login-window",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("route surface drifted:\n got %v\nwant %v", got, want)
	}
	for _, route := range registration.Routes {
		if strings.Contains(route.Path, "/quota") {
			t.Fatalf("route %s uses the host-reserved /quota path", route.Path)
		}
		if strings.TrimSpace(route.Description) == "" {
			t.Errorf("route %s carries no description", route.Path)
		}
	}
	if len(registration.Resources) != 1 {
		t.Fatalf("resources = %d, want exactly the panel card", len(registration.Resources))
	}
	if registration.Resources[0].Path != "/panel" || registration.Resources[0].Menu != "Meoo 秒悟" {
		t.Fatalf("panel resource = %+v", registration.Resources[0])
	}
}

// TestEveryDeclaredRouteIsDispatched keeps the registration table and the
// dispatch switch from drifting: a route advertised to the host but not handled
// here 404s in the operator's face.
func TestEveryDeclaredRouteIsDispatched(t *testing.T) {
	isolateMeoo(t)
	routes := managementRegistration().Routes
	if len(routes) == 0 {
		t.Fatal("no routes declared")
	}
	for _, route := range routes {
		resp := callRoute(t, route.Method, "/v0/management"+route.Path, nil)
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("declared route %s %s fell through to the 404 branch", route.Method, route.Path)
		}
	}
}

// TestPanelResourceServesRawHTML pins the incident this fleet already paid for:
// an HTML panel wrapped in a JSON envelope arrives as application/json and the
// browser shows the source instead of the panel.
//
// It also covers the second door: the browser's card request arrives on
// /v0/resource/plugins/meoo/panel, not on the management base.
func TestPanelResourceServesRawHTML(t *testing.T) {
	isolateMeoo(t)

	for _, door := range []string{
		"/v0/resource/plugins/meoo/panel",
		"/v8/resource/plugins/meoo/panel",
		"/v0/management/plugins/meoo/panel",
	} {
		resp := callRoute(t, "GET", door, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", door, resp.StatusCode)
		}
		contentType := ""
		for key, values := range resp.Headers {
			if strings.EqualFold(key, "Content-Type") && len(values) > 0 {
				contentType = values[0]
			}
		}
		if !strings.Contains(contentType, "text/html") {
			t.Fatalf("%s Content-Type = %q, want text/html", door, contentType)
		}
		if !strings.Contains(string(resp.Body), "<!DOCTYPE html>") {
			t.Fatalf("%s did not answer the panel document: %s", door, truncate(string(resp.Body), 120))
		}
		if strings.HasPrefix(strings.TrimSpace(string(resp.Body)), "{") {
			t.Fatalf("%s answered JSON instead of raw HTML", door)
		}
	}

	// An unknown sub-path on the resource door is the panel's own 404 page, with
	// the requested path escaped rather than interpolated raw.
	resp := callRoute(t, "GET", "/v0/resource/plugins/meoo/<b>nope</b>", nil)
	if !strings.Contains(string(resp.Body), "未知面板路径") {
		t.Fatalf("an unknown panel sub-path must say so: %s", truncate(string(resp.Body), 160))
	}
	if !strings.Contains(string(resp.Body), "&lt;b&gt;") {
		t.Fatalf("the unknown-path page must escape the requested path: %s", truncate(string(resp.Body), 200))
	}
	if strings.Contains(string(resp.Body), "<b>nope") {
		t.Fatalf("the requested path was interpolated raw: %s", truncate(string(resp.Body), 200))
	}

	// An unknown MANAGEMENT path still answers JSON, so a typo is diagnosable.
	typo := callRoute(t, "GET", "/v0/management/plugins/meoo/not-a-route", nil)
	if typo.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown management route status = %d, want 404", typo.StatusCode)
	}
}

// TestUnknownRouteAnswers404NotSilence keeps a typo diagnosable.
func TestUnknownRouteAnswers404NotSilence(t *testing.T) {
	isolateMeoo(t)
	resp := callRoute(t, "GET", "/v0/management/plugins/meoo/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "not found") {
		t.Fatalf("the 404 body should name the path: %s", string(resp.Body))
	}
}

// TestUnifiedPayloadShapeFromRefresh pins §5's payload: the same shape from every
// account-facing verb, and absent totals stay null rather than 0.
func TestUnifiedPayloadShapeFromRefresh(t *testing.T) {
	isolateMeoo(t)

	resp := callRoute(t, "POST", "/v0/management/plugins/meoo/refresh", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	body := string(resp.Body)
	for _, needle := range []string{
		`"summary"`, `"accounts":[]`, `"config"`, `"server_time"`, `"cache_path"`,
		`"readable":0`, `"dead":0`, `"stale":0`, `"refreshed_count":0`,
		`"refresh_minutes":60`, `"served_from_cache":true`, `"last_attempt_at":null`,
		`"total_available":null`, `"total_granted":null`, `"total_consumed":null`,
		`"auto_provision":true`, `"project_name":"CPA-Model-Gateway"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("the refresh payload is missing %s: %s", needle, body)
		}
	}
	if strings.Contains(body, `"total_available":0`) {
		t.Fatalf("absent totals must serialise as null, never as 0: %s", body)
	}

	// /accounts answers the same shape as /refresh (cache-first).
	accounts := callRoute(t, "GET", "/v0/management/plugins/meoo/accounts", nil)
	if !strings.Contains(string(accounts.Body), `"summary"`) {
		t.Fatalf("/accounts does not answer the unified shape: %s", string(accounts.Body))
	}
}

// TestAddAccountThroughManagementNeverEchoesTheKey: the panel posts a key and
// reads the payload back; neither the answer nor a later read may carry it whole.
func TestAddAccountThroughManagementNeverEchoesTheKey(t *testing.T) {
	isolateMeoo(t)

	body, errMarshal := json.Marshal(map[string]string{"user_key": fakeUserKey, "label": "好看的风"})
	if errMarshal != nil {
		t.Fatalf("marshal body: %v", errMarshal)
	}
	resp := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/add", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add status = %d: %s", resp.StatusCode, string(resp.Body))
	}
	answer := string(resp.Body)
	if strings.Contains(answer, fakeUserKey) {
		t.Fatal("the add answer echoed the full key")
	}
	if !strings.Contains(answer, keyTail(fakeUserKey)) {
		t.Fatalf("the add answer must identify the new row by its tail: %s", answer)
	}
	if !strings.Contains(answer, `"added"`) || !strings.Contains(answer, `"accounts":1`) {
		t.Fatalf("the add answer must stay the unified payload: %s", answer)
	}

	// And the ordinary read is clean too — with the key in place this time.
	accounts := callRoute(t, "GET", "/v0/management/plugins/meoo/accounts", nil)
	if strings.Contains(string(accounts.Body), fakeUserKey) {
		t.Fatal("the accounts payload echoed the full key")
	}
	payload := payloadMap(t, accounts)
	rows, okRows := payload["accounts"].([]any)
	if !okRows || len(rows) != 1 {
		t.Fatalf("expected exactly one account row, got %v", payload["accounts"])
	}
	row, okRow := rows[0].(map[string]any)
	if !okRow {
		t.Fatalf("row shape: %v", rows[0])
	}
	// Every field of the agreed row shape must be present unconditionally: a panel
	// that has to guard `row.note === undefined` renders "undefined" one day.
	for _, key := range []string{
		"id", "label", "key_tail", "plan_code", "plan_name", "available", "granted",
		"consumed", "project_url_id", "project_name", "project_ak_tail",
		"project_ak_state", "readable", "outcome", "note", "stale", "age_seconds",
		"last_refresh_at",
	} {
		if _, present := row[key]; !present {
			t.Errorf("the row is missing %s: %v", key, row)
		}
	}
	if row["readable"] != false {
		t.Errorf("an account that could not be read must not report readable=true: %v", row)
	}
	if row["available"] != nil {
		t.Errorf("an unreadable account must report available=null, never a number: %v", row)
	}
}

// TestAddAccountRejectionStillAnswersTheUnifiedShape: a panel that special-cases
// an error document shows an empty page exactly when the operator needs the
// reason.
func TestAddAccountRejectionStillAnswersTheUnifiedShape(t *testing.T) {
	isolateMeoo(t)

	body, _ := json.Marshal(map[string]string{"user_key": fakeProjectAK})
	resp := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/add", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	answer := string(resp.Body)
	if !strings.Contains(answer, `"summary"`) || !strings.Contains(answer, `"accounts":[]`) {
		t.Fatalf("a refused add must still answer the unified payload: %s", answer)
	}
	if !strings.Contains(answer, "项目级") {
		t.Fatalf("the refusal reason must be in the payload: %s", answer)
	}

	// A missing body is a 400 too, not a panic.
	empty := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/add", nil)
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty add = %d, want 400", empty.StatusCode)
	}
}

// TestRemoveAccountThroughManagement.
func TestRemoveAccountThroughManagement(t *testing.T) {
	isolateMeoo(t)
	record, errAdd := addAccount(fakeUserKey, "甲")
	if errAdd != nil {
		t.Fatalf("seed account: %v", errAdd)
	}

	body, _ := json.Marshal(map[string]string{"id": record.ID})
	resp := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/remove", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove status = %d", resp.StatusCode)
	}
	answer := string(resp.Body)
	if !strings.Contains(answer, `"removed":true`) || !strings.Contains(answer, `"accounts":[]`) {
		t.Fatalf("remove answer = %s", answer)
	}

	// Removing it again reports removed=false with a reason, still unified.
	again := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/remove", body)
	if !strings.Contains(string(again.Body), `"removed":false`) {
		t.Fatalf("second remove = %s", string(again.Body))
	}
	if !strings.Contains(string(again.Body), `"summary"`) {
		t.Fatalf("second remove lost the unified shape: %s", string(again.Body))
	}

	// No id at all is a 400.
	noID := callRoute(t, "POST", "/v0/management/plugins/meoo/accounts/remove", []byte(`{}`))
	if noID.StatusCode != http.StatusBadRequest {
		t.Fatalf("remove without an id = %d, want 400", noID.StatusCode)
	}
}

// TestModelsRouteMarksTheTokenPlanOnlyModelUnusable drives the real route against
// a local gateway, with a project-level AK already stored on the account.
func TestModelsRouteMarksTheTokenPlanOnlyModelUnusable(t *testing.T) {
	isolateMeoo(t)
	record, errAdd := addAccount(fakeUserKey, "甲")
	if errAdd != nil {
		t.Fatalf("seed account: %v", errAdd)
	}
	recordProject(record.ID, "l1l7coxwlq0f", fakeProjectAK)

	gatewayHits := 0
	server := httptestJSON(t, func(path string) (int, string) {
		if path != "/models" {
			return http.StatusNotFound, `{"error":"unexpected"}`
		}
		gatewayHits++
		return http.StatusOK, `{"models":["qwen3.8-flash","qwen4-max-preview"],"defaultModel":"qwen3.8-flash"}`
	})
	swapGateway(t, server)

	resp := callRoute(t, "GET", "/v0/management/plugins/meoo/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d: %s", resp.StatusCode, string(resp.Body))
	}
	payload := payloadMap(t, resp)
	if payload["gateway"] != server {
		t.Fatalf("payload gateway = %v, want %q", payload["gateway"], server)
	}
	if payload["default_model"] != "qwen3.8-flash" {
		t.Fatalf("default_model = %v", payload["default_model"])
	}
	models, okModels := payload["models"].([]any)
	if !okModels || len(models) != 2 {
		t.Fatalf("models = %v", payload["models"])
	}
	byName := map[string]map[string]any{}
	for _, entry := range models {
		asMap, okMap := entry.(map[string]any)
		if !okMap {
			t.Fatalf("model entry shape: %v", entry)
		}
		byName[asMap["name"].(string)] = asMap
	}
	if byName["qwen3.8-flash"]["usable"] != true || byName["qwen3.8-flash"]["display_name"] != "Qwen3.8 Flash" {
		t.Fatalf("usable model rendered as %v", byName["qwen3.8-flash"])
	}
	preview := byName["qwen4-max-preview"]
	if preview["usable"] != false {
		t.Fatalf("qwen4-max-preview must be usable=false: %v", preview)
	}
	if note, _ := preview["note"].(string); !strings.Contains(note, "Token Plan") {
		t.Fatalf("the unusable model must carry its reason: %v", preview)
	}

	// The catalogue is cached behind the same TTL: a second call must not re-ask.
	callRoute(t, "GET", "/v0/management/plugins/meoo/models", nil)
	if gatewayHits != 1 {
		t.Fatalf("gateway hits = %d, want 1 (the catalogue must be cached)", gatewayHits)
	}
}

// TestModelsRouteWithoutAnyProjectKeyExplainsItself: an empty catalogue must come
// with the reason, not with silence.
func TestModelsRouteWithoutAnyProjectKeyExplainsItself(t *testing.T) {
	isolateMeoo(t)
	resp := callRoute(t, "GET", "/v0/management/plugins/meoo/models", nil)
	payload := payloadMap(t, resp)
	models, okModels := payload["models"].([]any)
	if !okModels || len(models) != 0 {
		t.Fatalf("models = %v, want an empty list", payload["models"])
	}
	reason, _ := payload["note"].(string)
	if !strings.Contains(reason, "项目级 AK") {
		t.Fatalf("the empty catalogue must explain itself: %v", payload["note"])
	}
	if payload["gateway"] != gatewayBase {
		t.Fatalf("gateway = %v", payload["gateway"])
	}
}

// TestManagementBasePathsHaveSafeDefaults: the host may hand us a base path, but
// the plugin must still answer correctly if it never does.
func TestManagementBasePathsHaveSafeDefaults(t *testing.T) {
	isolateMeoo(t)
	setManagementBasePath("")
	setResourceBasePath("")
	if got := loadedManagementBasePath(); got != "/v0/management/plugins/meoo" {
		t.Fatalf("default management base = %q", got)
	}
	if got := loadedResourceBasePath(); got != "/v0/resource/plugins/meoo" {
		t.Fatalf("default resource base = %q", got)
	}
	applyManagementRegistration([]byte(`{"base_path":"/v0/management","resource_base_path":"/v0/resource/plugins/meoo"}`))
	if got := loadedManagementBasePath(); got != "/v0/management" {
		t.Fatalf("base path not applied: %q", got)
	}
	if got := loadedResourceBasePath(); got != "/v0/resource/plugins/meoo" {
		t.Fatalf("resource base path not applied: %q", got)
	}
	// The camelCase spelling the host has also shipped.
	applyManagementRegistration([]byte(`{"basePath":"/v0/management/","resourceBasePath":"/v0/resource/plugins/meoo/"}`))
	if got := loadedManagementBasePath(); got != "/v0/management" {
		t.Fatalf("camelCase base path not applied: %q", got)
	}
	// Garbage must not wipe the good value.
	applyManagementRegistration([]byte(`not json at all`))
	if got := loadedManagementBasePath(); got != "/v0/management" {
		t.Fatalf("garbage input changed the base path: %q", got)
	}
	setManagementBasePath("")
	setResourceBasePath("")
}

// TestManagementTailHandlesBothDoors pins the prefix stripping directly.
func TestManagementTailHandlesBothDoors(t *testing.T) {
	cases := map[string]string{
		"/v0/management/plugins/meoo/accounts":    "/accounts",
		"/v0/management/plugins/meoo/accounts/":   "/accounts",
		"/v8/management/plugins/meoo/daily-login": "/daily-login",
		"/v0/resource/plugins/meoo/panel":         "/panel",
		"/v0/management/plugins/meoo":             "/",
		"/v0/management/plugins/meoo/":            "/",
		"/weird/prefix/plugins/meoo/models":       "/models",
	}
	for input, want := range cases {
		if got := managementTail(input); got != want {
			t.Errorf("managementTail(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestHandleMethodDispatch covers the four RPC verbs and the unknown-method
// branch: the host must never see an empty answer.
func TestHandleMethodDispatch(t *testing.T) {
	isolateMeoo(t)

	registerBody, errMarshal := json.Marshal(map[string][]byte{
		"config_yaml": []byte("refresh_minutes: 15\nauto_provision: false\nproject_name: My-Proj\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal register body: %v", errMarshal)
	}

	raw, errCall := handleMethod("plugin.register", registerBody)
	if errCall != nil {
		t.Fatalf("plugin.register: %v", errCall)
	}
	var env envelope
	if errJSON := json.Unmarshal(raw, &env); errJSON != nil || !env.OK {
		t.Fatalf("register envelope: %s (%v)", string(raw), errJSON)
	}
	var registration registration
	if errJSON := json.Unmarshal(env.Result, &registration); errJSON != nil {
		t.Fatalf("decode registration: %v", errJSON)
	}
	if registration.Metadata.Name != "Meoo 秒悟" || registration.Metadata.Version != version {
		t.Fatalf("metadata = %+v", registration.Metadata)
	}
	if registration.Metadata.Author == "" || registration.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata must be complete or the host refuses to load it: %+v", registration.Metadata)
	}
	// 2026-11-15 机队抬钉：注册上报档位随 SDK 快照（v8.0.72）走到 10；只防回退，不锁具体值。
	if registration.SchemaVersion < 10 {
		t.Fatalf("schema_version = %d, want >= 10 (SDK snapshot v8.0.72 fleet baseline)", registration.SchemaVersion)
	}
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("management_api must be declared")
	}
	if registration.Capabilities.ModelProvider || registration.Capabilities.AuthProvider ||
		registration.Capabilities.Executor || registration.Capabilities.QuotaProvider ||
		registration.Capabilities.UsagePlugin || registration.Capabilities.Scheduler {
		t.Fatalf("this is a panel-only plugin; every other capability must stay false: %+v", registration.Capabilities)
	}
	if len(registration.Metadata.ConfigFields) != len(meooConfigFields) {
		t.Fatalf("config fields = %d, want %d", len(registration.Metadata.ConfigFields), len(meooConfigFields))
	}

	// reconfigure must be accepted on the same path.
	if _, errReconfigure := handleMethod("plugin.reconfigure", registerBody); errReconfigure != nil {
		t.Fatalf("plugin.reconfigure: %v", errReconfigure)
	}

	registrationRaw, errRegister := handleMethod("management.register", []byte(`{"base_path":"/v0/management","resource_base_path":"/v0/resource/plugins/meoo"}`))
	if errRegister != nil {
		t.Fatalf("management.register: %v", errRegister)
	}
	if !strings.Contains(string(registrationRaw), `"routes"`) {
		t.Fatalf("management.register answer = %s", string(registrationRaw))
	}

	handleRaw, errHandle := handleMethod("management.handle", []byte(`{"Method":"GET","Path":"/v0/management/plugins/meoo/models"}`))
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	if !strings.Contains(string(handleRaw), `"ok":true`) {
		t.Fatalf("management.handle answer = %s", string(handleRaw))
	}

	unknown, errUnknown := handleMethod("plugin.does_not_exist", nil)
	if errUnknown != nil {
		t.Fatalf("unknown method must answer an envelope, not an error: %v", errUnknown)
	}
	if !strings.Contains(string(unknown), `"code":"unknown_method"`) {
		t.Fatalf("unknown method envelope = %s", string(unknown))
	}

	quiesce, errQuiesce := handleMethod("plugin.quiesce", nil)
	if errQuiesce != nil || !strings.Contains(string(quiesce), `"quiesced":true`) {
		t.Fatalf("quiesce = %s (%v)", string(quiesce), errQuiesce)
	}
	// Idempotent: a second quiesce must not panic on a closed channel.
	if _, errAgain := handleMethod("plugin.quiesce", nil); errAgain != nil {
		t.Fatalf("second quiesce: %v", errAgain)
	}
}

// TestConfigFieldsAreExactlyTheAgreedSurface AND each one is consumed: the
// declared keys are the four from §6, and every declared key is echoed from the
// same struct the sweep reads.
//
// The fourth key, daily_login, was added with the daily 2,000-point login
// reward (daily_login.go): it is the switch that lets the operator stop the
// plugin from reading a browser profile on this machine.
func TestConfigFieldsAreExactlyTheAgreedSurface(t *testing.T) {
	isolateMeoo(t)

	declared := make([]string, 0, len(meooConfigFields))
	for _, field := range meooConfigFields {
		declared = append(declared, field.Name)
		if field.Type == "" || strings.TrimSpace(field.Description) == "" {
			t.Errorf("config field %q needs a type and a description", field.Name)
		}
	}
	if strings.Join(declared, ",") != "refresh_minutes,auto_provision,project_name,daily_login" {
		t.Fatalf("declared config surface = %v", declared)
	}

	echo := configEcho()
	for _, field := range meooConfigFields {
		if _, present := echo[field.Name]; !present {
			t.Errorf("declared key %q is not consumed anywhere (it is not even echoed)", field.Name)
		}
	}
}

// TestConfigureAppliesEveryKeyAndClamps drives the real configure() path with the
// host's base64 shape, then with the readable-YAML shape, then with the nested
// block shape that silently swallowed every key once.
func TestConfigureAppliesEveryKeyAndClamps(t *testing.T) {
	isolateMeoo(t)

	body := "enabled: false\nrefresh_minutes: 15\nauto_provision: false\nproject_name: My-Proj\n"
	wrapped, errMarshal := json.Marshal(map[string][]byte{"config_yaml": []byte(body)})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	configure(wrapped)

	cfgMu.RLock()
	got := currentCfg
	cfgMu.RUnlock()
	if got.Enabled || got.AutoProvision {
		t.Fatalf("booleans were not applied: %+v", got)
	}
	if got.RefreshMinutes != 15 || got.ProjectName != "My-Proj" {
		t.Fatalf("values were not applied: %+v", got)
	}
	if ttl := currentRefreshTTL(); ttl.Minutes() != 15 {
		t.Fatalf("the TTL does not follow refresh_minutes: %v", ttl)
	}
	if currentAutoProvision() || currentProjectName() != "My-Proj" {
		t.Fatal("the provisioning accessors do not read the applied config")
	}

	// The readable-YAML shape.
	configure([]byte(`{"config_yaml":"refresh_minutes: 30\nproject_name: Other"}`))
	if ttl := currentRefreshTTL(); ttl.Minutes() != 30 || currentProjectName() != "Other" {
		t.Fatalf("the string shape was not applied: %v / %q", ttl, currentProjectName())
	}

	// The nested block shape.
	configure([]byte(`{"config_yaml":"config_yaml: |\n  refresh_minutes: 45\n"}`))
	if ttl := currentRefreshTTL(); ttl.Minutes() != 45 {
		t.Fatalf("the nested block shape was not applied: %v", ttl)
	}

	// Missing keys fall back to the defaults, not to zero values.
	resetConfigForTest()
	configure([]byte(`{"config_yaml":""}`))
	cfgMu.RLock()
	got = currentCfg
	cfgMu.RUnlock()
	if got != defaultConfig() {
		t.Fatalf("an empty config must keep the defaults: %+v", got)
	}

	// Clamping.
	configure(mustConfigYAML(t, "refresh_minutes: 0\nproject_name: '   '\nauto_provision: true\n"))
	if ttl := currentRefreshTTL(); ttl.Minutes() != refreshMinutesMin {
		t.Fatalf("clamp low: %v", ttl)
	}
	if currentProjectName() != projectNameDefault {
		t.Fatalf("an empty project name must fall back to the default, got %q", currentProjectName())
	}
	configure(mustConfigYAML(t, "refresh_minutes: 99999"))
	if ttl := currentRefreshTTL(); ttl.Minutes() != refreshMinutesMax {
		t.Fatalf("clamp high: %v", ttl)
	}
	configure(mustConfigYAML(t, "project_name: '"+strings.Repeat("n", 200)+"'"))
	if len(currentProjectName()) != projectNameMaxLen {
		t.Fatalf("project name was not bounded: %d chars", len(currentProjectName()))
	}
	// A malformed YAML body resets to the defaults (loomy's behaviour): the parse
	// fails, so nothing in the block may be trusted — not even keeping the old
	// values, which would silently pin a stale window.
	configure(mustConfigYAML(t, "project_name: [unterminated"))
	if currentProjectName() != projectNameDefault || currentRefreshTTL().Minutes() != refreshMinutesDefault {
		t.Fatalf("a malformed body must fall back to the defaults, got %q/%v",
			currentProjectName(), currentRefreshTTL())
	}
}

// mustConfigYAML wraps a YAML body the way the host does (base64 via []byte).
func mustConfigYAML(t *testing.T, body string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string][]byte{"config_yaml": []byte(body)})
	if errMarshal != nil {
		t.Fatalf("marshal config: %v", errMarshal)
	}
	return raw
}

// TestConfigureAcceptsTheRawBase64StringShape covers the second spelling the host
// has shipped: config_yaml as a plain base64 string rather than a decoded []byte.
func TestConfigureAcceptsTheRawBase64StringShape(t *testing.T) {
	isolateMeoo(t)
	encoded := base64.StdEncoding.EncodeToString([]byte("refresh_minutes: 90"))
	configure([]byte(`{"config_yaml":"` + encoded + `"}`))
	if ttl := currentRefreshTTL(); ttl.Minutes() != 90 {
		t.Fatalf("the base64 string shape was not applied: %v", ttl)
	}
}
