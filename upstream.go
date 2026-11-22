// upstream.go — every call this plugin makes to Meoo.
//
// ---------------------------------------------------------------------------
// Two hosts, two credential families, never interchangeable
// ---------------------------------------------------------------------------
//
//	开放平台 (this file's callOpenPlatform)
//	  base  https://meoo.com/open/v1
//	  auth  Authorization: Bearer <用户级 meoo_ak_...>
//	  use   identity / points / entitlement / projects / cloud / environment
//
//	模型网关 (fetchGatewayModels)
//	  base  https://api.meoo.host/meoo-ai/compatible-mode/v1
//	  auth  Authorization: Bearer <项目级 meoo_project_service_v1....>
//	  use   /models
//
// The two key shapes are refused locally, before anything leaves the machine:
// sending the wrong family produces a 401 that is indistinguishable from an
// expired account, and a panel that reports "account dead" for a paste mistake
// is a panel that lies.
//
// ---------------------------------------------------------------------------
// Discovery chain (one walk per account)
// ---------------------------------------------------------------------------
//
//	identity -> projects (first status=active) -> cloud (enable when off)
//	         -> environment (project-level AK) -> points -> entitlement
//
// The project-level AK only exists in the project that owns a cloud instance, so
// when that first-active walk ends with no key, resolveProject appends ONE
// read-only probe over the remaining active projects (stored url_id first). The
// probe reads cloud state and environment only: it never enables a cloud
// instance and never creates a project.
//
// Only the two provisioning POSTs write anything upstream, and both are gated by
// the auto_provision config key (config.go). Everything else is a read.
//
// ---------------------------------------------------------------------------
// host.http.do, and the shape of its answer
// ---------------------------------------------------------------------------
//
// Production traffic goes through the host bridge, so the host's transport
// policy (proxy, timeouts) and request log apply. The response wire struct is
// PASCALCASE — {"StatusCode","Headers","Body"} — because the host marshals
// pluginapi.HTTPResponse, which carries no json tags. Decoding it as
// `status_code` silently yields status 0 for every answer, which turns a real
// 403 into "the site never answered"; rpcHostHTTPResponseWire below is the
// PascalCase shape and httpDo treats a 0 status as a failure, never as success.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Endpoint bases. Variables, not constants, so a unit test can aim the whole
// reader at a local httptest server: a test must never touch the real site.
var (
	openPlatformBase = "https://meoo.com/open/v1"
	gatewayBase      = "https://api.meoo.host/meoo-ai/compatible-mode/v1"
)

// upstreamTimeout bounds one read. These are probes, not data-plane calls: the
// panel must never hang on a slow upstream.
const upstreamTimeout = 20 * time.Second

// outcome values, shared with the cache and the panel.
const (
	outcomeOK        = "ok"
	outcomeTransient = "transient"
	outcomeDead      = "dead"
)

// upstreamExcerptLen bounds any upstream text copied into a note or a log line.
const upstreamExcerptLen = 160

// -----------------------------------------------------------------------------
// Host HTTP bridge
// -----------------------------------------------------------------------------

// hostHTTPResponse is the plugin-side view of a bridged response.
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// rpcHostHTTPRequestWire mirrors the host's request shape.
type rpcHostHTTPRequestWire struct {
	Request *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPInner struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// rpcHostHTTPResponseWire mirrors pluginapi.HTTPResponse, which has NO json tags
// and therefore marshals as PascalCase. Getting this wrong is silent: a
// `status_code` decode returns 0 for every response, so a 403 looks like a
// transport failure and a 200 looks like nothing at all.
type rpcHostHTTPResponseWire struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

var (
	directClientOnce sync.Once
	directClient     *http.Client
)

// directHTTPClient is the no-host fallback (unit tests only).
//
// CheckRedirect is not decoration: these requests carry a credential in a header,
// and Go's default client FOLLOWS a cross-origin redirect while copying headers
// onto the new request — a hostile or misconfigured 307 would hand the key to a
// third party. Same-origin redirects are still followed (a site may canonicalise
// a trailing slash); cross-origin ones are refused. Bridged requests are subject
// to the HOST's redirect policy instead; this guard protects the direct path.
func directHTTPClient() *http.Client {
	directClientOnce.Do(func() {
		directClient = &http.Client{
			Timeout:       upstreamTimeout,
			CheckRedirect: credentialSafeRedirect(),
		}
	})
	return directClient
}

// credentialSafeRedirect refuses to carry a credential across origins.
func credentialSafeRedirect() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		origin := via[0].URL
		if origin == nil || req.URL == nil {
			return fmt.Errorf("refusing redirect with an unparseable origin")
		}
		if !strings.EqualFold(origin.Scheme, req.URL.Scheme) || !strings.EqualFold(origin.Host, req.URL.Host) {
			return fmt.Errorf("refusing cross-origin redirect %s -> %s (the Meoo key must not leave %s)",
				origin.Host, req.URL.Host, origin.Host)
		}
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 redirects")
		}
		return nil
	}
}

func hostBridgeAvailable() bool { return hostAPI != nil && hostAPI.call != nil }

// httpDo performs one request through the host bridge, falling back to a direct
// client only when there is no host.
func httpDo(req *http.Request) (*hostHTTPResponse, error) {
	if !hostBridgeAvailable() {
		return httpDoDirect(req)
	}
	inner := &rpcHostHTTPInner{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
	}
	wire, errMarshal := json.Marshal(rpcHostHTTPRequestWire{Request: inner})
	if errMarshal != nil {
		return nil, errMarshal
	}
	raw, errCall := hostCall("host.http.do", wire)
	if errCall != nil {
		return nil, errCall
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("host.http.do: decode envelope: %w", errUnmarshal)
	}
	if !env.OK {
		message := "host returned not-ok"
		if env.Error != nil && env.Error.Message != "" {
			message = env.Error.Message
		}
		return nil, fmt.Errorf("host.http.do: %s", message)
	}
	var resp rpcHostHTTPResponseWire
	if errResp := json.Unmarshal(env.Result, &resp); errResp != nil {
		return nil, errResp
	}
	if resp.StatusCode == 0 {
		// A zero status is not a valid HTTP answer. Treating it as one would make a
		// real failure look like a success, so fall back to a direct call instead.
		return httpDoDirect(req)
	}
	return &hostHTTPResponse{StatusCode: resp.StatusCode, Headers: http.Header(resp.Headers), Body: resp.Body}, nil
}

// httpDoDirect is the no-host fallback (tests only).
func httpDoDirect(req *http.Request) (*hostHTTPResponse, error) {
	client := directHTTPClient()
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()
	resp, errDo := client.Do(req.WithContext(ctx))
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, errRead
	}
	return &hostHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, nil
}

// -----------------------------------------------------------------------------
// Redaction
// -----------------------------------------------------------------------------

// secretShapeRE matches both Meoo credential families. Upstream text is copied
// into notes and log lines; a body that echoes the key back (an error document,
// a mis-scoped proxy) must not carry it into either.
var secretShapeRE = regexp.MustCompile(`(meoo_ak_|meoo_project_service_v1\.)[A-Za-z0-9._\-]{4,}`)

// redactSecrets replaces every credential-shaped run with its prefix and a mask.
func redactSecrets(s string) string {
	return secretShapeRE.ReplaceAllString(s, "${1}***")
}

// note bounds and redacts a string destined for a note or a log line.
func note(s string) string {
	return truncate(redactSecrets(strings.TrimSpace(s)), upstreamExcerptLen)
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// apiError carries the verdict for one call. Auth is the only thing that may
// classify an account dead: "the site said no" and "the site did not answer" call
// for different operator actions.
type apiError struct {
	Status int
	Text   string
	Auth   bool
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	if e.Status > 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Text)
	}
	return e.Text
}

// newAPIError classifies one failed call. Only 401/403 are read as "this key was
// refused"; everything else (5xx, 429, a timeout, an unparseable body) is the
// site's problem and must not clear an account's readings.
func newAPIError(status int, text string) *apiError {
	return &apiError{Status: status, Text: note(text), Auth: status == http.StatusUnauthorized || status == http.StatusForbidden}
}

// isAuthError reports whether err says the credential itself was refused.
func isAuthError(err error) bool {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Auth
	}
	return false
}

// -----------------------------------------------------------------------------
// Open platform
// -----------------------------------------------------------------------------

// apiEnvelope is the open platform's common answer shape. Most endpoints answer
// {"success":true,"data":{...}}; two (POST /projects, POST /projects/{id}/cloud)
// answer the payload bare, so payload() falls back to the whole body.
type apiEnvelope struct {
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
}

// payload returns the meaningful body: `data` when present, else the whole body.
func (e apiEnvelope) payload(raw []byte) json.RawMessage {
	if len(e.Data) > 0 && !bytes.Equal(bytes.TrimSpace(e.Data), []byte("null")) {
		return e.Data
	}
	return raw
}

// described renders the diagnostic an operator can act on.
func (e apiEnvelope) described() string {
	parts := []string{}
	if strings.TrimSpace(e.Message) != "" {
		parts = append(parts, strings.TrimSpace(e.Message))
	}
	if len(e.Error) > 0 && !bytes.Equal(bytes.TrimSpace(e.Error), []byte("null")) {
		parts = append(parts, string(e.Error))
	}
	if len(parts) == 0 {
		return "the answer carried no message"
	}
	return strings.Join(parts, " / ")
}

// callOpenPlatform performs one authenticated open-platform request and returns
// the unwrapped payload plus the HTTP status.
//
// The credential is checked BEFORE the request is built: this endpoint family
// only ever accepts a user-level key, and a project-level key sent here answers
// 401 — a verdict about the account that would be wrong.
func callOpenPlatform(userKey, method, path string, body any) (json.RawMessage, int, error) {
	key := strings.TrimSpace(userKey)
	if key == "" {
		return nil, 0, fmt.Errorf("no user-level key for this account")
	}
	if strings.HasPrefix(key, projectKeyPrefix) {
		return nil, 0, fmt.Errorf("拒绝把项目级 key（%s…）发给开放平台：这里需要以 %q 开头的用户级 key", projectKeyPrefix, userKeyPrefix)
	}
	if !strings.HasPrefix(key, userKeyPrefix) {
		return nil, 0, fmt.Errorf("存储的 key 不以 %q 开头，不是用户级开放平台 key", userKeyPrefix)
	}
	if !strings.HasPrefix(path, "/") {
		return nil, 0, fmt.Errorf("open-platform path %q must be absolute", path)
	}

	var reader io.Reader
	if body != nil {
		buf, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return nil, 0, errMarshal
		}
		reader = bytes.NewReader(buf)
	}
	req, errReq := http.NewRequest(method, strings.TrimRight(openPlatformBase, "/")+path, reader)
	if errReq != nil {
		return nil, 0, errReq
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cpa-meoo-panel/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doAndUnwrap(req, "开放平台 "+path)
}

// doAndUnwrap issues the request, classifies a failure and unwraps the envelope.
func doAndUnwrap(req *http.Request, label string) (json.RawMessage, int, error) {
	resp, errDo := httpDo(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("%s: %s", label, note(errDo.Error()))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, newAPIError(resp.StatusCode, fmt.Sprintf("%s answered %d %s: %s",
			label, resp.StatusCode, http.StatusText(resp.StatusCode), note(string(resp.Body))))
	}
	var env apiEnvelope
	if errJSON := json.Unmarshal(resp.Body, &env); errJSON != nil {
		return nil, resp.StatusCode, newAPIError(resp.StatusCode, fmt.Sprintf("%s: answer is not JSON (site change?): %s", label, note(errJSON.Error())))
	}
	if env.Success != nil && !*env.Success {
		return nil, resp.StatusCode, newAPIError(resp.StatusCode, fmt.Sprintf("%s refused: %s", label, env.described()))
	}
	return env.payload(resp.Body), resp.StatusCode, nil
}

// identityReading is the auth probe's answer. Only its verdict is used: the
// account name is carried for the panel (an operator with two unlabelled keys
// cannot tell them apart), and nothing else from it is kept.
type identityReading struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Edition  string `json:"edition"`
}

// displayName picks the most human of the identity's names.
func (i *identityReading) displayName() string {
	for _, candidate := range []string{i.Name, i.Username, i.UserID} {
		if strings.TrimSpace(candidate) != "" {
			return truncate(strings.TrimSpace(candidate), 60)
		}
	}
	return ""
}

// fetchIdentity is the chain's first step: it decides whether the key is alive.
func fetchIdentity(userKey string) (*identityReading, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/account/identity", nil)
	if errCall != nil {
		return nil, errCall
	}
	var identity identityReading
	if errJSON := json.Unmarshal(payload, &identity); errJSON != nil {
		return nil, fmt.Errorf("identity: %s", note(errJSON.Error()))
	}
	return &identity, nil
}

// pointsReading is the account's credit ledger.
type pointsReading struct {
	Available *float64 `json:"availableBalance"`
	Frozen    *float64 `json:"frozenBalance"`
	Granted   *float64 `json:"totalGranted"`
	Consumed  *float64 `json:"totalConsumed"`
	Expired   *float64 `json:"totalExpired"`
	Refunded  *float64 `json:"totalRefunded"`
	// LoginGrantToday is the face value of TODAY's「登录赠送积分」batches in
	// activeGrants (sum). It is upstream FACT — the same role workbuddy's
	// checkin-ledger face value plays in its blood bar — and it is the only
	// buff evidence that survives a claim that landed BEFORE today's first
	// reading (2026-11-22 operator ruling: the day boundary is midnight, not
	// the panel's first read).
	LoginGrantToday float64 `json:"-"`
}

// fetchPoints reads the ledger. Absent numbers stay nil — never 0.
func fetchPoints(userKey string) (*pointsReading, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/account/points", nil)
	if errCall != nil {
		return nil, errCall
	}
	var envelope struct {
		Account      pointsReading `json:"account"`
		ActiveGrants []struct {
			SourceType     string `json:"sourceType"`
			SourceTypeName string `json:"sourceTypeName"`
			GrantDetails   []struct {
				RemainingAmount float64 `json:"remainingAmount"`
				EffectiveDate   string  `json:"effectiveDate"`
				ExpiresDate     string  `json:"expiresDate"`
			} `json:"grantDetails"`
		} `json:"activeGrants"`
	}
	if errJSON := json.Unmarshal(payload, &envelope); errJSON != nil {
		return nil, fmt.Errorf("points: %s", note(errJSON.Error()))
	}
	// Sum the face value of today's login-grant batches. effectiveDate is a
	// Beijing-time instant; the day compare uses the same local YYYY-MM-DD the
	// day baselines use (the operator's ruling: the day is the calendar day).
	envelope.Account.LoginGrantToday = 0
	today := time.Now().Format("2006-01-02")
	for _, grant := range envelope.ActiveGrants {
		if !strings.Contains(strings.TrimSpace(grant.SourceTypeName), "登录") &&
			!strings.Contains(strings.TrimSpace(grant.SourceType), "LOGIN") {
			continue
		}
		for _, detail := range grant.GrantDetails {
			if strings.TrimSpace(detail.EffectiveDate) == today ||
				strings.HasPrefix(strings.TrimSpace(detail.EffectiveDate), today) {
				envelope.Account.LoginGrantToday += detail.RemainingAmount
			}
		}
	}
	return &envelope.Account, nil
}

// benefitReading is one entitlement benefit row.
type benefitReading struct {
	TypeCode  string   `json:"typeCode"`
	TypeName  string   `json:"typeName"`
	ValueType string   `json:"valueType"`
	Quota     *float64 `json:"quota"`
	Used      *float64 `json:"used"`
}

// entitlementReading is the plan and its benefits.
type entitlementReading struct {
	PlanCode string           `json:"planCode"`
	PlanName string           `json:"planName"`
	PlanType string           `json:"planType"`
	Benefits []benefitReading `json:"benefits"`
}

// modelCredits returns the model_credits benefit, which is the row the panel's
// progress bar describes. It is a fallback source of numbers when the points
// endpoint is unavailable, and the only place a plan's own quota is stated.
func (e *entitlementReading) modelCredits() *benefitReading {
	if e == nil {
		return nil
	}
	for i := range e.Benefits {
		if strings.EqualFold(strings.TrimSpace(e.Benefits[i].TypeCode), "model_credits") {
			return &e.Benefits[i]
		}
	}
	return nil
}

// fetchEntitlement reads the plan. A failure here never fails the account: the
// numbers come from points, and the plan is decoration.
func fetchEntitlement(userKey string) (*entitlementReading, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/account/entitlement", nil)
	if errCall != nil {
		return nil, errCall
	}
	var entitlement entitlementReading
	if errJSON := json.Unmarshal(payload, &entitlement); errJSON != nil {
		return nil, fmt.Errorf("entitlement: %s", note(errJSON.Error()))
	}
	return &entitlement, nil
}

// meooProject is one project row.
type meooProject struct {
	URLID     string `json:"url_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Type      string `json:"type"`
	AccessURL string `json:"accessUrl"`
}

// fetchProjects lists the account's projects.
func fetchProjects(userKey string) ([]meooProject, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/projects", nil)
	if errCall != nil {
		return nil, errCall
	}
	var envelope struct {
		List []meooProject `json:"list"`
	}
	if errJSON := json.Unmarshal(payload, &envelope); errJSON != nil {
		return nil, fmt.Errorf("projects: %s", note(errJSON.Error()))
	}
	return envelope.List, nil
}

// createProject provisions one project and returns its url_id.
//
// This is one of the two calls in this plugin that WRITE anything upstream, and
// it only runs when auto_provision is on (config.go) and the account owns no
// active project.
func createProject(userKey, name string) (meooProject, error) {
	body := map[string]string{"name": clampProjectName(name), "type": "create-desktop"}
	payload, _, errCall := callOpenPlatform(userKey, http.MethodPost, "/cli-compat/projects", body)
	if errCall != nil {
		return meooProject{}, errCall
	}
	var created meooProject
	if errJSON := json.Unmarshal(payload, &created); errJSON != nil {
		return meooProject{}, fmt.Errorf("create project: %s", note(errJSON.Error()))
	}
	if strings.TrimSpace(created.URLID) == "" {
		return meooProject{}, fmt.Errorf("create project: the answer carried no url_id")
	}
	if strings.TrimSpace(created.Name) == "" {
		created.Name = clampProjectName(name)
	}
	return created, nil
}

// cloudState is the project's cloud-instance state.
type cloudState struct {
	Enabled  bool `json:"enabled"`
	Instance *struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"instance"`
}

// fetchCloud reads the project's cloud state.
func fetchCloud(userKey, projectURLID string) (*cloudState, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/projects/"+projectURLID+"/cloud", nil)
	if errCall != nil {
		return nil, errCall
	}
	var state cloudState
	if errJSON := json.Unmarshal(payload, &state); errJSON != nil {
		return nil, fmt.Errorf("cloud: %s", note(errJSON.Error()))
	}
	return &state, nil
}

// enableCloud opens the project's cloud instance. The second (and last) call
// that writes anything upstream; gated by auto_provision.
func enableCloud(userKey, projectURLID string) error {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodPost, "/cli-compat/projects/"+projectURLID+"/cloud", map[string]any{})
	if errCall != nil {
		return errCall
	}
	var opened struct {
		Status  string `json:"status"`
		Created *bool  `json:"created"`
	}
	if errJSON := json.Unmarshal(payload, &opened); errJSON != nil {
		return fmt.Errorf("enable cloud: %s", note(errJSON.Error()))
	}
	return nil
}

// projectEnvironment carries the project-level AK the model gateway wants.
type projectEnvironment struct {
	Enabled           bool   `json:"enabled"`
	EnvMode           string `json:"envMode"`
	MeooProjectAPIKey string `json:"meooProjectApiKey"`
}

// fetchProjectEnvironment reads the project-level AK. The value is a secret: it
// is stored and used, never echoed.
func fetchProjectEnvironment(userKey, projectURLID string) (*projectEnvironment, error) {
	payload, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/projects/"+projectURLID+"/cloud/environment", nil)
	if errCall != nil {
		return nil, errCall
	}
	var environment projectEnvironment
	if errJSON := json.Unmarshal(payload, &environment); errJSON != nil {
		return nil, fmt.Errorf("project environment: %s", note(errJSON.Error()))
	}
	if !strings.HasPrefix(strings.TrimSpace(environment.MeooProjectAPIKey), projectKeyPrefix) {
		return nil, fmt.Errorf("project environment carried no %s… key", projectKeyPrefix)
	}
	return &environment, nil
}

// -----------------------------------------------------------------------------
// Model gateway
// -----------------------------------------------------------------------------

// gatewayUnusableModels is measured, not guessed. qwen4-max-preview answers
// 403 {"error":{"message":"Access denied","type":"access_denied"}} on this
// gateway for every account — 阿里云 limits it to a Token Plan, so no amount of
// credential handling changes it. The panel must therefore offer it as
// unavailable rather than count it as a usable channel.
var gatewayUnusableModels = map[string]string{
	"qwen4-max-preview": "网关固定 403 Access denied（阿里云限定 Token Plan，与账号无关）",
}

// gatewayModels is the gateway catalogue.
type gatewayModels struct {
	Models       []string `json:"models"`
	DefaultModel string   `json:"defaultModel"`
}

// fetchGatewayModels reads the catalogue with a PROJECT-level key. As with the
// open platform, the wrong family is refused locally: a user-level key here
// answers 401 and would look like a dead project.
func fetchGatewayModels(projectAK string) (*gatewayModels, error) {
	key := strings.TrimSpace(projectAK)
	if key == "" {
		return nil, fmt.Errorf("no project-level key is stored yet (run a refresh so the discovery chain can find one)")
	}
	if strings.HasPrefix(key, userKeyPrefix) {
		return nil, fmt.Errorf("拒绝把用户级 key（%s…）发给模型网关：这里需要以 %q 开头的项目级 key", userKeyPrefix, projectKeyPrefix)
	}
	if !strings.HasPrefix(key, projectKeyPrefix) {
		return nil, fmt.Errorf("存储的项目 key 不以 %q 开头", projectKeyPrefix)
	}
	req, errReq := http.NewRequest(http.MethodGet, strings.TrimRight(gatewayBase, "/")+"/models", nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cpa-meoo-panel/"+version)
	payload, _, errCall := doAndUnwrap(req, "模型网关 /models")
	if errCall != nil {
		return nil, errCall
	}
	var catalogue gatewayModels
	if errJSON := json.Unmarshal(payload, &catalogue); errJSON != nil {
		return nil, fmt.Errorf("gateway models: %s", note(errJSON.Error()))
	}
	return &catalogue, nil
}

// usable reports whether a catalogue entry may be counted as a channel, plus the
// reason when it may not.
func gatewayModelUsable(name string) (bool, string) {
	if reason, known := gatewayUnusableModels[strings.ToLower(strings.TrimSpace(name))]; known {
		return false, reason
	}
	return true, ""
}

// displayModelName renders "qwen3.8-flash" as "Qwen3.8 Flash" for the panel. The
// gateway publishes machine ids only; the panel needs something an operator reads.
//
// 2026-11-20：首字母大写改为按 **rune** 切，不是 part[:1]。
// 理由与 truncateUTF8Bytes 同源（见 main.go）：part[:1] 只切 1 字节，一旦上游把
// 中文显示名（3 字节/字）或带 emoji 的模型名发下来，就会切出半个字符 ——
// 面板显示成 U+FFFD，且这个坏字节会跟着卡片/凭据一起落盘。
// 纯 ASCII 输入结果逐字节不变（rune 边界 == 字节边界），所以这是严格的收窄。
func displayModelName(name string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(name), func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		first, size := utf8.DecodeRuneInString(part)
		if first == utf8.RuneError && size == 1 {
			// 非法 UTF-8：原样保留，不放大损坏（DecodeRuneInString 的 RuneError/size==1
			// 是"这一字节解不出字符"的专用信号）。
			continue
		}
		parts[i] = strings.ToUpper(string(first)) + part[size:]
	}
	if len(parts) == 0 {
		return name
	}
	return strings.Join(parts, " ")
}

// -----------------------------------------------------------------------------
// One account's walk
// -----------------------------------------------------------------------------

// projectLeg is the outcome of the projects -> cloud -> environment leg.
type projectLeg struct {
	URLID        string
	Name         string
	AK           string
	CloudEnabled bool
	Note         string
}

// resolveProject walks the project leg for one account.
//
// The project is decoration for the NUMBERS (points and entitlement are
// account-scoped) and load-bearing for the GATEWAY (the project-level AK only
// exists there). So every failure in this leg is a note, never an account
// verdict: an account whose project leg failed still shows its points.
//
// 2026-11-22: an account that ALREADY holds a project-level AK never triggers
// the cloud-enable POST, regardless of auto_provision. The walk's chosen
// project can be a key-less one while the account's usable instance lives
// elsewhere (the 2026-11-22 measured account: first-active "本地客户端咨询"
// without cloud, usable key on another project); auto-enabling the chosen
// project then hits the plan's account-level instance quota every sweep and
// pins the same refusal note to every reading. There is nothing to recover —
// the gateway key is in hand — so provisioning would be a pure write with no
// possible upside.
func resolveProject(userKey string, stored accountRecord) projectLeg {
	leg := projectLeg{URLID: stored.ProjectURLID, AK: stored.ProjectAK}

	projects, errProjects := fetchProjects(userKey)
	if errProjects != nil {
		if len(leg.URLID) == 0 {
			leg.Note = "项目列表读取失败：" + note(errProjects.Error())
			return leg
		}
		leg.Note = "项目列表读取失败，沿用已存项目"
		return leg
	}

	chosen := meooProject{}
	for _, project := range projects {
		if strings.EqualFold(strings.TrimSpace(project.Status), "active") && strings.TrimSpace(project.URLID) != "" {
			chosen = project
			break
		}
	}
	if chosen.URLID == "" && strings.TrimSpace(stored.ProjectURLID) != "" {
		// No active project. Prefer one we already know about: the operator may have
		// paused it, the list may lag, or the list may be empty. Re-provisioning an
		// account that already has a project is exactly the mistake this prevents.
		chosen = meooProject{URLID: strings.TrimSpace(stored.ProjectURLID), Name: projectNameByURLID(projects, stored.ProjectURLID)}
		leg.Note = "账号下没有 active 项目，沿用已存项目"
	}
	if chosen.URLID == "" {
		if !currentAutoProvision() {
			leg.Note = "账号下没有 active 项目，且 auto_provision=false，未自动创建"
			return leg
		}
		created, errCreate := createProject(userKey, currentProjectName())
		if errCreate != nil {
			leg.Note = "自动创建项目失败：" + note(errCreate.Error())
			return leg
		}
		chosen = created
		leg.Note = "已按 auto_provision 自动创建项目 " + truncate(created.Name, 40)
	}
	leg.URLID = chosen.URLID
	leg.Name = chosen.Name

	cloud, errCloud := fetchCloud(userKey, leg.URLID)
	switch {
	case errCloud != nil:
		leg.Note = joinNotes(leg.Note, "云服务状态读取失败："+note(errCloud.Error()))
	case cloud.Enabled:
		leg.CloudEnabled = true
	case leg.AK != "":
		// The account already holds a project-level AK: the gateway leg has
		// nothing to recover, so enabling this project's cloud would be a write
		// with no possible upside. On a plan whose instance quota is exhausted it
		// also pins the same refusal note to every sweep (2026-11-22).
		leg.Note = joinNotes(leg.Note, "云服务未开通，但账号已持有项目级 AK，不自动开通")
	case !currentAutoProvision():
		leg.Note = joinNotes(leg.Note, "云服务未开通，且 auto_provision=false，未自动开通")
	default:
		if errEnable := enableCloud(userKey, leg.URLID); errEnable != nil {
			leg.Note = joinNotes(leg.Note, "自动开通云服务失败："+note(errEnable.Error()))
		} else {
			leg.CloudEnabled = true
			leg.Note = joinNotes(leg.Note, "已按 auto_provision 开通云服务")
		}
	}

	environment, errEnvironment := fetchProjectEnvironment(userKey, leg.URLID)
	if errEnvironment != nil {
		if leg.AK != "" {
			// A key is already stored for this account: nothing to recover, and a
			// note here would only be noise.
			return leg
		}
		leg.Note = joinNotes(leg.Note, "项目级 AK 读取失败："+note(errEnvironment.Error()))

		// The chosen project answered nothing, and "chosen" only means FIRST
		// ACTIVE — not "carries a project-level key". On an account with several
		// projects the key lives in exactly one of them (and only where a cloud
		// instance exists), so a first-active pick can end with no AK at all while
		// a usable one sits right next to it. Walk the rest, READ-ONLY, before
		// declaring the account key-less.
		adopted, key, checked := probeProjectAK(userKey, projects, leg.URLID, stored.ProjectURLID)
		switch {
		case adopted.URLID != "":
			leg.URLID = adopted.URLID
			leg.Name = adopted.Name
			leg.AK = key
			leg.CloudEnabled = true
			leg.Note = joinNotes(leg.Note, "已改选带云服务的项目 "+adopted.URLID)
		case checked > 0:
			leg.Note = joinNotes(leg.Note, fmt.Sprintf("已检查其余 %d 个项目，均无可用项目级 AK", checked))
		}
		return leg
	}
	leg.AK = strings.TrimSpace(environment.MeooProjectAPIKey)
	leg.CloudEnabled = leg.CloudEnabled || environment.Enabled
	return leg
}

// probeProjectAK searches the OTHER active projects for a usable project-level
// key, and returns the adopted project, that key and how many candidates were
// inspected (0 = there was nothing else to look at).
//
// It is read-only by construction: the only two calls it makes are fetchCloud and
// fetchProjectEnvironment. A probe must never run enableCloud or createProject —
// provisioning while looking would burn a plan's cloud-instance quota (the exact
// failure that motivated this fallback) and would leave resources behind in an
// account that never asked for them.
//
// A candidate is accepted only when its cloud instance is ENABLED and its
// environment hands back a non-empty project-level key: an unopened project has
// no environment to read, and a read failure is not proof of anything, so both
// are skipped rather than fatal.
func probeProjectAK(userKey string, projects []meooProject, chosenURLID, storedURLID string) (meooProject, string, int) {
	candidates := probeCandidates(projects, chosenURLID, storedURLID)
	for _, candidate := range candidates {
		cloud, errCloud := fetchCloud(userKey, candidate.URLID)
		if errCloud != nil || !cloud.Enabled {
			continue
		}
		environment, errEnvironment := fetchProjectEnvironment(userKey, candidate.URLID)
		if errEnvironment != nil {
			continue
		}
		key := strings.TrimSpace(environment.MeooProjectAPIKey)
		if key == "" {
			continue
		}
		return candidate, key, len(candidates)
	}
	return meooProject{}, "", len(candidates)
}

// probeCandidates orders the projects a probe should visit: the other ACTIVE
// rows, with the stored project first (this plugin already believes in it, so a
// re-read beats a walk) and the project list's own order after that. The stored
// url_id is only promoted when the list still carries it as an active row; a
// stored id that the list does not mention may be paused or gone, and a probe
// must not spend requests guessing.
func probeCandidates(projects []meooProject, chosenURLID, storedURLID string) []meooProject {
	chosenURLID = strings.TrimSpace(chosenURLID)
	storedURLID = strings.TrimSpace(storedURLID)

	rest := make([]meooProject, 0, len(projects))
	seen := make(map[string]bool, len(projects))
	for _, project := range projects {
		urlID := strings.TrimSpace(project.URLID)
		if urlID == "" || urlID == chosenURLID || seen[urlID] {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(project.Status), "active") {
			continue
		}
		seen[urlID] = true
		project.URLID = urlID
		rest = append(rest, project)
	}
	if storedURLID == "" || storedURLID == chosenURLID {
		return rest
	}
	for index, project := range rest {
		if project.URLID != storedURLID {
			continue
		}
		ordered := make([]meooProject, 0, len(rest))
		ordered = append(ordered, project)
		ordered = append(ordered, rest[:index]...)
		ordered = append(ordered, rest[index+1:]...)
		return ordered
	}
	return rest
}

// projectNameByURLID returns the listed name of one project, or "" when the list
// does not mention it (a paused project, or a list that lags).
func projectNameByURLID(projects []meooProject, urlID string) string {
	urlID = strings.TrimSpace(urlID)
	for _, project := range projects {
		if strings.TrimSpace(project.URLID) == urlID {
			return strings.TrimSpace(project.Name)
		}
	}
	return ""
}

// joinNotes appends a note, keeping the earlier ones.
func joinNotes(existing, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return addition
	}
	return existing + "；" + addition
}

// accountRead is one account's fresh reading.
type accountRead struct {
	Outcome    string
	HasNumbers bool
	Note       string
	HTTPStatus int

	IdentityName string
	Edition      string

	PlanCode string
	PlanName string

	Available *float64
	Granted   *float64
	Consumed  *float64
	Frozen    *float64
	Expired   *float64

	// LoginGrantToday is the face value of today's「登录赠送积分」batches the
	// ledger reports (upstream fact; the blood bar's ledger-evidence buff).
	LoginGrantToday float64

	ProjectURLID string
	ProjectName  string
	ProjectAK    string
	ProjectNote  string
	CloudEnabled bool
}

// readAccount runs the whole discovery chain for one account.
//
// The verdict rule is deliberately conservative: only an explicit 401/403 turns
// an account DEAD (which drops its numbers and freezes it for a TTL). Everything
// else — a 5xx, a timeout, an unparseable body, a broken project leg — is
// transient, because killing a live account's readings costs far more than
// re-reading it on the next sweep.
func readAccount(record accountRecord) accountRead {
	read := accountRead{Outcome: outcomeTransient}

	identity, errIdentity := fetchIdentity(record.UserKey)
	if errIdentity != nil {
		if isAuthError(errIdentity) {
			read.Outcome = outcomeDead
			read.Note = "上游拒绝该用户级 key（HTTP 401/403）——重新签发后请重新添加"
			return read
		}
		read.Note = joinNotes(read.Note, "身份读取失败："+note(errIdentity.Error()))
	} else {
		read.IdentityName = identity.displayName()
		read.Edition = strings.TrimSpace(identity.Edition)
	}

	points, errPoints := fetchPoints(record.UserKey)
	if errPoints != nil {
		if isAuthError(errPoints) {
			read.Outcome = outcomeDead
			read.Note = "上游拒绝该用户级 key（HTTP 401/403）——重新签发后请重新添加"
			return read
		}
		read.Note = joinNotes(read.Note, "积分读取失败："+note(errPoints.Error()))
	} else if points.Available != nil || points.Granted != nil || points.Consumed != nil {
		read.HasNumbers = true
		read.Available = points.Available
		read.Granted = points.Granted
		read.Consumed = points.Consumed
		read.Frozen = points.Frozen
		read.Expired = points.Expired
		read.LoginGrantToday = points.LoginGrantToday
	}

	entitlement, errEntitlement := fetchEntitlement(record.UserKey)
	if errEntitlement != nil {
		read.Note = joinNotes(read.Note, "套餐读取失败："+note(errEntitlement.Error()))
	} else {
		read.PlanCode = strings.TrimSpace(entitlement.PlanCode)
		read.PlanName = strings.TrimSpace(entitlement.PlanName)
		if !read.HasNumbers {
			// The points endpoint is the primary source; the entitlement's benefit
			// row states the same allowance, so use it rather than showing nothing.
			if benefit := entitlement.modelCredits(); benefit != nil && (benefit.Quota != nil || benefit.Used != nil) {
				read.HasNumbers = true
				read.Granted = benefit.Quota
				read.Consumed = benefit.Used
				if benefit.Quota != nil && benefit.Used != nil {
					remaining := *benefit.Quota - *benefit.Used
					if remaining < 0 {
						remaining = 0
					}
					read.Available = &remaining
				}
				read.Note = joinNotes(read.Note, "积分接口不可用，数字取自套餐 entitlement")
			}
		}
	}

	leg := resolveProject(record.UserKey, record)
	read.ProjectURLID = leg.URLID
	read.ProjectName = leg.Name
	read.ProjectAK = leg.AK
	read.ProjectNote = leg.Note
	read.CloudEnabled = leg.CloudEnabled
	read.Note = joinNotes(read.Note, leg.Note)

	if read.HasNumbers {
		read.Outcome = outcomeOK
	}
	return read
}
