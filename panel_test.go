package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Two gate items that once lived OUTSIDE this tree, both RESOLVED (2026-11-13).
// Kept here as the audit trail; drop this block when it outlives its value.
//
//  1. scripts/panel-js-check.mjs WAS the oxion copy (its requiredIds asserted
//     oxion's skeleton, so the build gate was red here no matter what).
//     RESOLVED 2026-11-13: the script now asserts meoo's own 22-id skeleton
//     and passes ("2 个 <script> 块语法通过，22 个 id，0 个 el() 引用全部命中").
//  2. PANEL_CALLS_UNDECLARED_PATH for config/api-keys/openai-compatibility —
//     the HOST config path §7.3 requires for the CPA cross-check. RESOLVED
//     2026-11-13: registered in cpa-plugin-updater/gates/dead-declaration.whitelist.json
//     (precedent: workbuddy's `quota` entry). Still best-effort by contract:
//     TestPanelMarksUnusableModelsAndDegradesCrossCheck pins that a failure
//     there degrades to 未知 and never touches the readings.
// ---------------------------------------------------------------------------

// TestPanelHTMLContract pins the behaviour the operator asked for by name:
// 刷新数据 forces a fresh walk, 模型 is a read-only dialog, 加账号 posts one
// user-level key — and the panel talks to nothing but its own management base.
// (读缓存 was removed 2026.11.21.4 and 诊断 / 清空缓存 in 2026.11.21.5: the
// operator judged them redundant.)
func TestPanelHTMLContract(t *testing.T) {
	required := []string{
		"刷新数据",             // the explicit action
		"模型",               // GET /models
		"加账号",              // POST /accounts/add
		"POLL_MS = 300000", // 5-minute panel poll (cache-first, so zero upstream)
		"/plugins/meoo",    // the only management surface it talks to
		"document.hidden",  // hidden tabs must not poll
		"Bearer",           // the management key travels in the standard header
		// every verb of the agreed route table (§5) must actually be called:
		"/accounts/add",
		"/accounts/remove",
		"/refresh",
		"/models",
		"/models/refresh",
		// the model picker (2026.11.21.6, workbuddy-shaped): refresh/save verbs and
		// the host write-back path with a full-override confirmation.
		"刷新目录",
		"保存选用",
		"/openai-compatibility",
		"确认保存",
		// the panel-side cross-check (§7.3) reads CPA's own config path:
		"/config/api-keys/openai-compatibility",
		// ... and its three verdicts, plus the entry it looks for:
		"已在 CPA",
		"未挂进 CPA",
		"未知",
		"秒悟",
		// unusable models render a 不可用 pill keyed off the usable flag (the
		// Token-Plan wording lives in the backend note, rendered via the title):
		"不可用",
		// the picker renders gateway id + alias per card:
		"注册别名",
	}
	for _, needle := range required {
		if !strings.Contains(string(panelHTML), needle) {
			t.Errorf("panel HTML is missing %q", needle)
		}
	}
	// The same payload is parsed by every account-facing verb: the panel must key
	// off the agreed row shape rather than inventing field names.
	for _, field := range []string{"summary", "accounts", "server_time", "cache_path", "refreshed_count"} {
		if !strings.Contains(string(panelHTML), field) {
			t.Errorf("panel HTML does not touch the agreed payload field %q", field)
		}
	}
}

// TestPanelHTMLIsSelfContained: no external asset may be fetched, because the
// resource route is served from inside the host with no internet assumption.
func TestPanelHTMLIsSelfContained(t *testing.T) {
	html := string(panelHTML)
	for _, forbidden := range []string{"http://", "https://", "<script src", "<link rel=\"stylesheet\""} {
		if strings.Contains(html, forbidden) {
			t.Errorf("panel HTML references an external resource (%q)", forbidden)
		}
	}
	// One inline stylesheet, and it must not reach for a webfont either.
	if strings.Count(html, "<style") != 1 {
		t.Errorf("panel HTML must carry exactly one inline <style> block, found %d", strings.Count(html, "<style"))
	}
	if strings.Contains(html, "@import") {
		t.Errorf("panel HTML must not @import anything")
	}
}

// TestPanelHTMLEscapesDynamicValues keeps account metadata from becoming markup.
func TestPanelHTMLEscapesDynamicValues(t *testing.T) {
	if !strings.Contains(string(panelHTML), "function esc(") {
		t.Fatal("panel HTML has no esc() helper — dynamic values would be injected raw")
	}
}

// TestPanelReadsHostManagementKey is the regression lock for the operator's
// complaint ("每次都让我输一遍 CPA 密码"). The panel is an iframe on the host's own
// origin, and the host's control panel already keeps the key in
// localStorage["cli-proxy-auth"] as "enc::v1::" + base64(XOR(plain, salt|host|UA)).
//
// The five assertions below are all load-bearing; each one is a way this can break:
//
//  1. the store name, prefix and salt must be byte-identical to the host's
//     (internal webui: vl="enc::v1::", yl="cli-proxy-api-webui::secure-storage",
//     Vc="cli-proxy-auth"), because a wrong salt decodes to garbage;
//  2. reading it must be gated on real embedding — a standalone tab cannot see the
//     parent's storage and must fall through to the manual box instead;
//  3. a host key the host itself rejected must not be retried, or the panel keeps
//     knocking with a stale key and bans this machine's IP (5 failures, 30 min);
//  4. the key must never be persisted to localStorage or left in the address bar;
//  5. the panel's own session slot must be meoo's, not another plugin's.
func TestPanelReadsHostManagementKey(t *testing.T) {
	html := string(panelHTML)

	// 1) the host's exact scheme.
	hostScheme := []string{
		`"cli-proxy-auth"`,                      // Vc
		`"enc::v1::"`,                           // vl
		`"cli-proxy-api-webui::secure-storage"`, // yl
		"localStorage.getItem(PANEL_STORE)",     // read the host's store
		`st.managementKey`,                      // {state:{managementKey:...}}
		`"cpa-meoo-mgmt-key"`,                   // this plugin's own session slot
	}
	for _, needle := range hostScheme {
		if !strings.Contains(html, needle) {
			t.Errorf("panel cannot read the host key store: missing %q", needle)
		}
	}
	if !strings.Contains(html, "function hostKeyDiag(") {
		t.Error("panel has no hostKeyDiag(): a missing key must be explained, not silently turned into a password prompt")
	}
	// 1b) host panel 19.3.1 moved the slot to enc::v2:: and dropped
	// navigator.userAgent from the XOR key. A v1-only decoder returns nothing, the
	// panel then asks for a password that never changed -- that was the incident.
	// Both generations must be recognised, and the derivation must match the host's.
	if !strings.Contains(html, `"enc::v2::"`) {
		t.Error("panel does not recognise the current enc::v2:: host slot -- every load would demand a password")
	}
	if !strings.Contains(html, `SECRET_SALT+"|"+window.location.host+(noUA?"":"|"+navigator.userAgent)`) {
		t.Error("XOR key must be salt|location.host for v2 and salt|location.host|userAgent for v1, exactly as the host derives them")
	}

	// 2) only read the host store when genuinely embedded.
	if !strings.Contains(html, "function isEmbedded(") || !strings.Contains(html, "window.self !== window.top") {
		t.Fatal("readHostKey must be gated on window.self !== window.top")
	}
	if !strings.Contains(html, `if(!isEmbedded()) return "";`) {
		t.Fatal("readHostKey has no embedding gate — a standalone tab would read the wrong storage")
	}

	// 3) a rejected host key must degrade to the manual box, not loop.
	if !strings.Contains(html, "hostKeyRejected = true") || !strings.Contains(html, "if(!hostKeyRejected)") {
		t.Fatal("a 401/403 must retire the host key instead of retrying it forever")
	}

	// 4) no key => zero requests (an empty key counts as a failed auth upstream).
	if !strings.Contains(html, "if(!k){ showAuth(); return Promise.reject") {
		t.Fatal("api() must refuse to fire without a key")
	}
	if !strings.Contains(html, "if(!getKey()){ showAuth(); return Promise.resolve(); }") {
		t.Fatal("load() must not fire a request without a key")
	}

	// 5) never persist the key where it outlives the session, never leave it in the URL.
	if strings.Contains(html, "localStorage.setItem") {
		t.Fatal("panel must not write the management key to localStorage")
	}
	if !strings.Contains(html, "history.replaceState") {
		t.Fatal("?key= must be stripped from the address bar after reading")
	}
	if !strings.Contains(html, "sessionStorage.setItem") {
		t.Fatal("manual key entry must live in sessionStorage")
	}
	if !strings.Contains(html, `id="keyInput" type="password"`) {
		t.Fatal("the key input must be a password field")
	}

	// 6) the door must be derived from the URL the browser actually arrived on, with
	// a /v0 fallback, or every button 404s on the other door.
	if !strings.Contains(html, `location.pathname`) || !strings.Contains(html, `resource\/plugins\/`) {
		t.Fatal("the management door must be reverse-engineered from location.pathname (host keeps /v0 and /v8 alive at once)")
	}
	if !strings.Contains(html, `"/v0"`) {
		t.Fatal("the door derivation needs its /v0 fallback")
	}
}

// TestPanelNeverReadsFullCredentials is the PANEL half of the leak rule.
//
// The payload half already exists with the backend, under the name its own source
// comments cite: accounts_test.go's TestAccountViewNeverEchoesFullKey (the meoo
// twin of loomy's TestAccountViewNeverEchoesSessionToken) proves the marshalled row
// carries only `key_tail` / `project_ak_tail` and never a whole credential. This
// test proves the other direction: the page consumes exactly those tails, and never
// reaches for a full `user_key` / `project_ak` / session token out of a row — so a
// future "just show the key so I can copy it" edit fails here rather than in a
// screenshot with the operator's credentials on it.
func TestPanelNeverReadsFullCredentials(t *testing.T) {
	html := string(panelHTML)

	// The tails the API publishes, and the label the operator reads them by.
	for _, needle := range []string{"a.key_tail", "a.project_ak_tail", "key 尾号"} {
		if !strings.Contains(html, needle) {
			t.Errorf("panel must show the published tail (%q is missing)", needle)
		}
	}

	// Whole credentials must never be read off a row.
	for _, forbidden := range []string{"a.user_key", "row.user_key", "row.project_ak", "a.session", "a.token"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("the panel reads %q — rows carry tails only, never a whole credential", forbidden)
		}
	}

	// `a.project_ak` is only allowed with a non-secret suffix: _tail (published) or
	// _state (ok / missing).
	for idx := 0; ; {
		rel := strings.Index(html[idx:], "a.project_ak")
		if rel < 0 {
			break
		}
		at := idx + rel
		rest := html[at+len("a.project_ak"):]
		if !strings.HasPrefix(rest, "_tail") && !strings.HasPrefix(rest, "_state") {
			t.Errorf("the panel reads a whole project-level AK: %.70q", html[at:])
		}
		idx = at + len("a.project_ak")
	}
}

// TestPanelMarksUnusableModelsAndDegradesCrossCheck locks the two meoo-specific
// behaviours of §7.3 that no other gate can see:
//
//   - a model the gateway refuses must be marked 不可用 (the exact Token-Plan
//     wording lives in the backend payload note; the picker renders 不可用 and
//     keys off the usable flag, never a hardcoded model id);
//   - the CPA cross-check is an ADD-ON: when that host path cannot be read the
//     verdict must fall back to 未知 and the panel must keep rendering the readings
//     it already has (the call sits behind its own catch).
func TestPanelMarksUnusableModelsAndDegradesCrossCheck(t *testing.T) {
	html := string(panelHTML)

	if !strings.Contains(html, "m.usable") {
		t.Fatal("the model marking must key off the payload's usable flag, not a hardcoded model id")
	}

	if !strings.Contains(html, "function loadCpaKeys(") {
		t.Fatal("the CPA cross-check has no loader")
	}
	// The degradation path: unknown state, and the check remembered as attempted.
	if !strings.Contains(html, "CPA.state = 'unknown'") || !strings.Contains(html, "CPA.checked = true") {
		t.Fatal("a failed cross-check must degrade to 未知 rather than retry/throw")
	}
	// The cross-check must be called AFTER the payload is rendered, so a host-path
	// failure cannot blank the account cards.
	loadIdx := strings.Index(html, "render(d);")
	cpaIdx := strings.Index(html, "return loadCpaKeys()")
	if loadIdx < 0 || cpaIdx < 0 || cpaIdx < loadIdx {
		t.Fatalf("loadCpaKeys() must run after render(d) (render=%d crossCheck=%d)", loadIdx, cpaIdx)
	}
}
