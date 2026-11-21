// management.go — the operator surface.
//
// Eight verbs, and each maps to one decision:
//
//	GET  /accounts           the panel's data. Cache-first: inside the TTL this
//	                         makes ZERO upstream calls, which is what makes the
//	                         panel's 300-second poll free.
//	POST /refresh            刷新数据: ignore the TTL, re-walk every account now.
//	POST /accounts/add       store one user-level key. Writes only the plugin's own
//	                         account file; never CPA's config.
//	POST /accounts/remove    drop one row by id.
//	GET  /models             the gateway catalogue, with qwen4-max-preview marked
//	                         unusable (403 Token-Plan-only, measured).
//	POST /cache/reset        drop our readings. Never touches the accounts.
//	GET  /health             version, ABI latch, window, paths. No upstream.
//	GET  /panel              the browser UI (a resource route, raw HTML).
//
// The panel resource is served as RAW HTML with the outer content type as
// text/html — wrapping HTML inside a JSON envelope makes the browser render the
// envelope's source instead (measured on this fleet 2026-10-25; loomy/panel.go is
// the working pattern this copies).
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	managementBasePathMu    sync.RWMutex
	managementBasePathCache string
	resourceBasePathMu      sync.RWMutex
	resourceBasePathCache   string
)

func setManagementBasePath(p string) {
	managementBasePathMu.Lock()
	managementBasePathCache = strings.TrimRight(strings.TrimSpace(p), "/")
	managementBasePathMu.Unlock()
}

func setResourceBasePath(p string) {
	resourceBasePathMu.Lock()
	resourceBasePathCache = strings.TrimRight(strings.TrimSpace(p), "/")
	resourceBasePathMu.Unlock()
}

func loadedManagementBasePath() string {
	managementBasePathMu.RLock()
	cached := managementBasePathCache
	managementBasePathMu.RUnlock()
	if cached != "" {
		return cached
	}
	return "/v0/management/plugins/" + providerName
}

// managementRoute / resourceRoute / managementRegistrationResponse are LOCAL
// mirrors of the SDK types, and the reason matters: pluginapi.ManagementRoute has
// NO json tags, so encoding/json emits "Method"/"Path"/"Menu" with capital
// letters while the host decodes the lower-case names in its own rpc schema. The
// failure is silent — the plugin registers, and every declared route 404s because
// the decoded Method/Path were empty. (Measured 2026-10-19 on the websession
// tree; loomy/management.go carries the same note.)
//
// TestManagementRegistrationMirrorsUseLowercaseTags is the regression gate.
type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementRegistration declares the routes.
//
// PATHS ARE RELATIVE TO THE MANAGEMENT ROOT, not to the plugin's own base: the
// host injects BasePath = "/v0/management", so a route is declared as
// "/plugins/meoo/accounts" and the host prefixes it.
//
// `/quota` is a host-reserved path and is deliberately unused here.
func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: "GET", Path: base + "/accounts", Description: "Meoo accounts with points, plan and project membership. Cache-first: inside the refresh window this makes zero upstream calls."},
			{Method: "POST", Path: base + "/refresh", Description: "刷新数据: re-walk every account now, ignoring the cache window, and return the full panel payload."},
			{Method: "POST", Path: base + "/accounts/add", Description: "Store one user-level Meoo key (meoo_ak_…), with an optional label. The key is never echoed back."},
			{Method: "POST", Path: base + "/accounts/remove", Description: "Remove one stored account by id. Never touches CPA's config or the Meoo account itself."},
			{Method: "GET", Path: base + "/models", Description: "The model gateway catalogue for the first account that owns a project-level AK, with unusable models marked and explained."},
			{Method: "POST", Path: base + "/cache/reset", Description: "Drop the cached readings and the cached catalogue so the next read goes upstream. Never touches the accounts."},
			{Method: "POST", Path: base + "/daily-login", Description: "立即领取每日积分: attempt Meoo's daily 2,000-point login reward for every account (or one named by account_id), using the web session already stored in this machine's browser profile."},
			{Method: "GET", Path: base + "/daily-login", Description: "The daily-login reward state per account: today's outcome, the amount granted and when the batch became effective. No upstream calls."},
			{Method: "POST", Path: base + "/daily-login/session", Description: "Store a pasted Meoo web-session cookie for one account (the credential the daily reward needs). It is verified against the account's identity before it is stored, and never echoed back."},
			{Method: "DELETE", Path: base + "/daily-login/session", Description: "Drop the stored web-session cookie of one account (account_id). The account itself is untouched."},
			{Method: "POST", Path: base + "/daily-login/login-window", Description: "Open the account's own Edge login window at meoo.com (account_id). The operator logs in once; the session lands in that profile and the next claim attempt picks it up."},
			{Method: "GET", Path: base + "/health", Description: "Plugin version, host ABI latch, effective refresh window and both file paths. Zero upstream calls."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Meoo 秒悟", Description: "Meoo panel: every account's points, plan and project, the gateway catalogue, hourly auto-refresh and an explicit 刷新数据."},
		},
	}
}

// managementTail strips everything up to and including this plugin's segment, so
// the switch below can match on the tail.
//
// The host hands us the FULL request path, and it arrives through TWO doors:
//
//	/v0/management/plugins/meoo/accounts   (and every other /v<n>/ door)
//	/v0/resource/plugins/meoo/panel        (the browser-facing card)
//
// Both must land on the same tail, or the panel loads and every button 404s.
func managementTail(rawPath string) string {
	path := strings.TrimRight(strings.TrimSpace(rawPath), "/")
	if idx := strings.Index(path, "/plugins/"+providerName); idx >= 0 {
		path = path[idx+len("/plugins/"+providerName):]
	}
	if path == "" {
		return "/"
	}
	return path
}

// handleManagement routes by path.
func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := managementTail(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch {
	case method == "GET" && (path == "/panel" || path == "/panel.html" || path == "/" || isResourceRequest(req.Path)):
		// The panel, through either door. managementTail has already reduced a
		// resource request to its sub-path, so `path` IS the sub-path here; the
		// resource namespace belongs to this plugin, which is why an unknown
		// sub-path there answers the panel's own 404 page rather than JSON that a
		// browser would render as text.
		return okEnvelope(mgmtHTMLResponse(servePanel(path)))
	case method == "GET" && path == "/accounts":
		return okEnvelope(jsonResponse(http.StatusOK, buildView(false)))
	case method == "POST" && path == "/refresh":
		return okEnvelope(jsonResponse(http.StatusOK, buildView(true)))
	case method == "POST" && path == "/accounts/add":
		return okEnvelope(addAccountResponse(req))
	case method == "POST" && path == "/accounts/remove":
		return okEnvelope(removeAccountResponse(req))
	case method == "GET" && path == "/models":
		return okEnvelope(jsonResponse(http.StatusOK, modelsView(false)))
	case method == "POST" && path == "/cache/reset":
		dropped := cacheReset()
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{
			"success": true,
			"dropped": dropped,
			"message": "已清空本插件缓存读数与模型目录；账号文件与 CPA 配置均未改动，下次读取会重新打上游。",
		}))
	case method == "GET" && path == "/health":
		return okEnvelope(jsonResponse(http.StatusOK, healthReport()))
	case method == "POST" && path == "/daily-login":
		return okEnvelope(jsonResponse(http.StatusOK, dailyLoginResponse(req)))
	case method == "GET" && path == "/daily-login":
		return okEnvelope(jsonResponse(http.StatusOK, dailyLoginStatusView()))
	case method == "POST" && path == "/daily-login/session":
		return okEnvelope(jsonResponse(http.StatusOK, saveSessionCookieResponse(req)))
	case method == "DELETE" && path == "/daily-login/session":
		return okEnvelope(jsonResponse(http.StatusOK, clearSessionCookieResponse(req)))
	case method == "POST" && path == "/daily-login/login-window":
		return okEnvelope(jsonResponse(http.StatusOK, openLoginWindowResponse(req)))
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
}

// openLoginWindowResponse launches the account's own login window.
//
// The operator's "one click" for (re)creating a web session: the window opens
// at meoo.com in the account's own Edge profile, the operator logs in, and the
// next claim attempt picks the session up (needs_login does not block the day).
func openLoginWindowResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accountID := strings.TrimSpace(req.Query.Get("account_id"))
	if accountID == "" && len(req.Body) > 0 {
		var body struct {
			AccountID string `json:"account_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		accountID = strings.TrimSpace(body.AccountID)
	}
	if accountID == "" {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": "需要 account_id 参数",
		}))
	}
	account, errAccount := findAccount(accountID)
	if errAccount != nil {
		return jsonResponse(http.StatusNotFound, unifiedPayload(false, map[string]any{
			"error": "找不到这个账号：" + accountID,
		}))
	}
	message, errLaunch := openLoginWindow(account)
	if errLaunch != nil {
		return jsonResponse(http.StatusInternalServerError, unifiedPayload(false, map[string]any{
			"error": note(errLaunch.Error()),
		}))
	}
	return jsonResponse(http.StatusOK, unifiedPayload(false, map[string]any{
		"login_window": map[string]any{
			"opened":     true,
			"account_id": accountID,
			"hint":       loginWindowHintFor(account, sessionExpiryForAccount(account)),
		},
		"message": message,
	}))
}

// saveSessionCookieResponse stores a pasted web-session cookie for one account.
//
// The cookie is the credential the daily 2,000-point reward needs (the
// open-platform key cannot trigger it). It is stored in the account file with
// the same discipline as the key itself: never echoed back.
//
// The cookie is VERIFIED before it is stored: it must be a live session AND it
// must belong to that account (its open_id must match the account's). A cookie
// that does not verify is refused, so the store never holds a credential that
// silently does the wrong thing.
func saveSessionCookieResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AccountID string `json:"account_id"`
		Cookie    string `json:"cookie"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	if strings.TrimSpace(body.AccountID) == "" {
		body.AccountID = req.Query.Get("account_id")
	}
	if strings.TrimSpace(body.Cookie) == "" {
		body.Cookie = req.Query.Get("cookie")
	}
	accountID := strings.TrimSpace(body.AccountID)
	cookie := strings.TrimSpace(body.Cookie)
	if accountID == "" || cookie == "" {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": "需要 account_id 与 cookie 两个参数",
		}))
	}
	account, errAccount := findAccount(accountID)
	if errAccount != nil {
		return jsonResponse(http.StatusNotFound, unifiedPayload(false, map[string]any{
			"error": "找不到这个账号：" + accountID,
		}))
	}
	want := accountOpenID(account)
	identity, errIdentity := fetchSessionIdentity(cookie)
	if errIdentity != nil {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": "这个会话不被秒悟接受：" + note(errIdentity.Error()),
		}))
	}
	if want != "" && identity.OpenID != want {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": "这个会话属于另一个账号（open_id 不匹配），已拒绝保存",
		}))
	}
	if errSave := setAccountSessionCookie(accountID, cookie); errSave != nil {
		return jsonResponse(http.StatusInternalServerError, unifiedPayload(false, map[string]any{
			"error": note(errSave.Error()),
		}))
	}
	return jsonResponse(http.StatusOK, unifiedPayload(false, map[string]any{
		"daily_login": map[string]any{
			"session_saved": true,
			"account_id":    accountID,
			"identity":      identity.Username,
		},
		"message": "会话已保存；点「领每日积分」即可用它领取今日的赠送积分。",
	}))
}

// clearSessionCookieResponse drops a stored session cookie for one account.
func clearSessionCookieResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accountID := strings.TrimSpace(req.Query.Get("account_id"))
	if accountID == "" && len(req.Body) > 0 {
		var body struct {
			AccountID string `json:"account_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		accountID = strings.TrimSpace(body.AccountID)
	}
	if accountID == "" {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": "需要 account_id 参数",
		}))
	}
	if errClear := setAccountSessionCookie(accountID, ""); errClear != nil {
		return jsonResponse(http.StatusInternalServerError, unifiedPayload(false, map[string]any{
			"error": note(errClear.Error()),
		}))
	}
	return jsonResponse(http.StatusOK, unifiedPayload(false, map[string]any{
		"message": "已清除该账号保存的会话。",
	}))
}

// dailyLoginResponse runs the daily-login reward sweep on demand.
//
// The operator's trigger: 「立即领取每日积分」. It runs for every account, or for
// one when `account_id` is given.
//
// The answer keeps the unified payload so the panel can re-render from one shape,
// and it adds a `daily_login` block describing what the attempt did.
func dailyLoginResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	only := strings.TrimSpace(req.Query.Get("account_id"))
	if only == "" && len(req.Body) > 0 {
		var body struct {
			AccountID string `json:"account_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		only = strings.TrimSpace(body.AccountID)
	}
	summary := runDailyLoginSweep(time.Now(), only)
	return jsonResponse(http.StatusOK, unifiedPayload(false, map[string]any{
		"daily_login": map[string]any{
			"day":         summary.Day,
			"attempted":   summary.Attempted,
			"granted":     summary.Granted,
			"already":     summary.Already,
			"failed":      summary.Failed,
			"needs_login": summary.NeedsLog,
			"at":          summary.At,
		},
	}))
}

// dailyLoginStatusView is the read-only view of the daily-login state.
//
// Each row also carries the SESSION EXPIRY (the C of "A+C"): the reward
// credential dies every 30 days, and the operator must see that coming — a
// session that died silently is a day's points silently lost.
func dailyLoginStatusView() map[string]any {
	now := time.Now()
	accounts, errAccounts := listAccounts()
	rows := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		row := dailyLoginStatusFor(account.ID, now)
		row["account_id"] = account.ID
		row["label"] = account.Label
		expiry := sessionExpiryForAccount(account)
		// The field is named session_state (not session) on purpose: the panel
		// leak gate forbids reading a whole credential off a row, and the expiry
		// object carries only a date, a day count and a directory path — no
		// secret of any kind.
		row["web_session"] = map[string]any{
			"found":      expiry.Found,
			"expires_at": expiry.ExpiresAt,
			"days_left":  expiry.DaysLeft,
			"hint":       loginWindowHintFor(account, expiry),
		}
		rows = append(rows, row)
	}
	payload := map[string]any{
		"day":     businessDay(now),
		"enabled": currentDailyLogin(),
		"rows":    rows,
	}
	if errAccounts != nil {
		payload["error"] = note(errAccounts.Error())
	}
	return payload
}

// isResourceRequest reports whether the FULL path arrived through the browser's
// resource door rather than the management one.
func isResourceRequest(fullPath string) bool {
	_, ok := resourceSubPath(fullPath)
	return ok
}

// unifiedPayload is the ONE shape every account-facing verb answers with.
//
// extra is merged on top, so a failure can carry an "error" key WITHOUT changing
// the shape the panel parses: a panel that special-cases an error document is a
// panel that shows an empty page exactly when the operator needs the reason.
func unifiedPayload(force bool, extra map[string]any) map[string]any {
	payload := buildView(force)
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

// addAccountResponse stores one key and answers with the unified payload.
func addAccountResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		UserKey string `json:"user_key"`
		Label   string `json:"label"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	// The panel posts JSON; a curl-style caller may use query parameters instead.
	if strings.TrimSpace(body.UserKey) == "" {
		body.UserKey = req.Query.Get("user_key")
	}
	if strings.TrimSpace(body.Label) == "" {
		body.Label = req.Query.Get("label")
	}
	if strings.TrimSpace(body.UserKey) == "" {
		body.UserKey = req.Headers.Get("X-Meoo-User-Key")
	}

	record, errAdd := addAccount(body.UserKey, body.Label)
	if errAdd != nil {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"error": truncate(errAdd.Error(), 240),
		}))
	}
	return jsonResponse(http.StatusOK, unifiedPayload(false, map[string]any{
		"added": map[string]any{
			"id":       record.ID,
			"label":    record.Label,
			"key_tail": keyTail(record.UserKey),
		},
	}))
}

// removeAccountResponse drops one row and answers with the unified payload.
func removeAccountResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		ID string `json:"id"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	if strings.TrimSpace(body.ID) == "" {
		body.ID = req.Query.Get("id")
	}
	removed, errRemove := removeAccount(body.ID)
	if errRemove != nil {
		return jsonResponse(http.StatusBadRequest, unifiedPayload(false, map[string]any{
			"removed": false,
			"error":   truncate(errRemove.Error(), 240),
		}))
	}
	extra := map[string]any{"removed": removed}
	if !removed {
		extra["error"] = "没有这个 id 的账号（可能已被移除）"
	}
	return jsonResponse(http.StatusOK, unifiedPayload(false, extra))
}

// modelsView is the gateway catalogue, with each entry carrying whether it may be
// counted as a usable channel.
//
// The catalogue is cached behind the same TTL as the readings: it changes rarely,
// and a panel poll must not become a gateway poll.
func modelsView(force bool) map[string]any {
	reading := loadModels(force)
	models := make([]map[string]any, 0, len(reading.Models))
	for _, name := range reading.Models {
		usable, reason := gatewayModelUsable(name)
		models = append(models, map[string]any{
			"name":         name,
			"usable":       usable,
			"note":         reason,
			"display_name": displayModelName(name),
		})
	}
	out := map[string]any{
		"models":        models,
		"default_model": reading.DefaultModel,
		"gateway":       gatewayBase,
	}
	if reading.Note != "" {
		out["note"] = reading.Note
	}
	return out
}

// healthReport is the diagnostics read.
func healthReport() map[string]any {
	return map[string]any{
		"plugin":          providerName,
		"version":         version,
		"management_base": loadedManagementBasePath(),
		"resource_base":   loadedResourceBasePath(),
		"refresh_minutes": int(currentRefreshTTL().Minutes()),
		"enabled":         currentEnabled(),
		"cache_path":      cachePath(),
		"accounts_file":   accountsPath(),
		"gateway":         gatewayBase,
		"open_platform":   openPlatformBase,
		"abi":             abiGuardHealth(),
	}
}

// jsonResponse builds a ManagementResponse carrying JSON.
func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	headers := map[string][]string{"Content-Type": {"application/json; charset=utf-8"}}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}
