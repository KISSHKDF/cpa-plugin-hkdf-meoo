package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// -----------------------------------------------------------------------------
// A local stand-in for the Meoo open platform
// -----------------------------------------------------------------------------

// fakeProjectRow is one row of a MULTI-project account: the project-list fields
// plus the two legs a walk reads per project (its cloud flag and its
// environment's project-level key). An empty AK models the measured account,
// whose key-less projects answer the environment endpoint without a key.
type fakeProjectRow struct {
	URLID  string `json:"url_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Type   string `json:"type"`
	// Cloud is what GET .../cloud reports for this project.
	Cloud bool `json:"-"`
	// AK is what GET .../cloud/environment hands back. Empty = no key here.
	AK string `json:"-"`
}

// activeRow builds one active project row, in a multi-project list.
func activeRow(urlID, name string, cloud bool, ak string) fakeProjectRow {
	return fakeProjectRow{URLID: urlID, Name: name, Status: "active", Type: "create-desktop", Cloud: cloud, AK: ak}
}

// projectURLIDFromPath pulls the url_id out of .../cli-compat/projects/{id}/...
func projectURLIDFromPath(path string) string {
	const marker = "/cli-compat/projects/"
	index := strings.Index(path, marker)
	if index < 0 {
		return ""
	}
	rest := path[index+len(marker):]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		return rest[:slash]
	}
	return rest
}

// fakeRowByURLID finds one row in a fixture list.
func fakeRowByURLID(rows []fakeProjectRow, urlID string) (fakeProjectRow, bool) {
	for _, row := range rows {
		if row.URLID == urlID {
			return row, true
		}
	}
	return fakeProjectRow{}, false
}

// jsonBool renders a Go bool as JSON, so a fixture body can be assembled by hand.
func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// fakeOpenPlatform answers the six documented open-platform endpoints, records
// every call and lets a test break exactly one leg at a time. Nothing here
// touches the network: the whole reader is aimed at this server.
type fakeOpenPlatform struct {
	mu            sync.Mutex
	calls         []string
	authorization string
	contentType   string

	// authErrStatus, when non-zero, answers EVERY path with that status (the
	// "the key was refused" / "the site is down" shapes).
	authErrStatus int
	// pointsBody overrides GET points wholesale.
	pointsBody string
	// entitlementBody overrides GET entitlement wholesale.
	entitlementBody string
	// projectsEmpty makes the project list answer an empty list.
	projectsEmpty bool
	// projectsStatus breaks GET projects only.
	projectsStatus int
	// cloudEnabled is what GET cloud reports.
	cloudEnabled bool
	// projectAK is what GET environment hands back.
	projectAK string
	// createName records the name sent to POST projects.
	createName string
	// cloudPOSTs counts POST cloud calls.
	cloudPOSTs int
	// projects, when non-nil, replaces the single hardcoded project row with this
	// list, so a test can model an account that owns several projects.
	projects []fakeProjectRow
	// cloudPOSTStatus, when non-zero, makes POST cloud answer that status — the
	// shape a Free plan returns once its cloud-instance quota is used up.
	cloudPOSTStatus int
}

func newFakeOpenPlatform() *fakeOpenPlatform {
	return &fakeOpenPlatform{cloudEnabled: true, projectAK: fakeProjectAK}
}

func (f *fakeOpenPlatform) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.authorization = r.Header.Get("Authorization")
	f.contentType = r.Header.Get("Content-Type")
}

func (f *fakeOpenPlatform) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeOpenPlatform) sawCall(want string) bool {
	for _, call := range f.callList() {
		if call == want {
			return true
		}
	}
	return false
}

func writeRawJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func (f *fakeOpenPlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)

	f.mu.Lock()
	authErr := f.authErrStatus
	pointsBody := f.pointsBody
	entitlementBody := f.entitlementBody
	projectsEmpty := f.projectsEmpty
	projectsStatus := f.projectsStatus
	cloudEnabled := f.cloudEnabled
	projectAK := f.projectAK
	projects := f.projects
	cloudPOSTStatus := f.cloudPOSTStatus
	f.mu.Unlock()

	if authErr != 0 {
		w.WriteHeader(authErr)
		writeRawJSON(w, `{"success":false,"message":"forced failure"}`)
		return
	}

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/account/identity"):
		writeRawJSON(w, `{"success":true,"data":{"openid":null,"user_id":"123","username":"boy","name":"好看的风","email":"meoo-test@example.invalid","avatar":null,"edition":"personal","tenant":{"id":"7","name":"公共租户"}}}`)
	case strings.HasSuffix(path, "/account/points"):
		if pointsBody != "" {
			writeRawJSON(w, pointsBody)
			return
		}
		writeRawJSON(w, `{"success":true,"data":{"account":{"userId":123,"availableBalance":11146,"frozenBalance":0,"totalGranted":12000,"totalConsumed":854,"totalExpired":0,"totalRefunded":0},"activeGrants":[{"sourceType":"SUBSCRIPTION","sourceTypeName":"订阅赠送积分","totalInitialAmount":5000,"totalRemainingAmount":5000}]}}`)
	case strings.HasSuffix(path, "/account/entitlement"):
		if entitlementBody != "" {
			writeRawJSON(w, entitlementBody)
			return
		}
		writeRawJSON(w, `{"success":true,"data":{"userId":"123","planCode":"free","planType":"personal","planName":"Free 免费版","activatedStartTime":"2026-11-01T00:00:00","activatedEndTime":"2026-12-01T00:00:00","benefits":[{"typeCode":"model_credits","typeName":"构建使用的模型消耗","valueType":"QUOTA","quota":12000,"used":854,"unit":null,"groupName":null}]}}`)
	case strings.HasSuffix(path, "/cloud/environment"):
		enabled := true
		ak := projectAK
		if projects != nil {
			row, found := fakeRowByURLID(projects, projectURLIDFromPath(path))
			if !found {
				w.WriteHeader(http.StatusNotFound)
				writeRawJSON(w, `{"success":false,"message":"no such project"}`)
				return
			}
			ak = row.AK
			enabled = row.AK != ""
		}
		writeRawJSON(w, `{"success":true,"data":{"enabled":`+jsonBool(enabled)+`,"envMode":"SINGLE","environment":"single","instance":{"sbInfo":{"url":"https://x.database.meoo.xyz"}},"meooProjectApiKey":"`+ak+`"}}`)
	case strings.HasSuffix(path, "/cloud") && r.Method == http.MethodGet:
		enabled := cloudEnabled
		if projects != nil {
			row, found := fakeRowByURLID(projects, projectURLIDFromPath(path))
			if !found {
				w.WriteHeader(http.StatusNotFound)
				writeRawJSON(w, `{"success":false,"message":"no such project"}`)
				return
			}
			enabled = row.Cloud
		}
		writeRawJSON(w, `{"success":true,"data":{"enabled":`+jsonBool(enabled)+`,"instance":{"id":181932,"appId":"app-1","status":null,"name":"Demo Cloud","description":"Demo Cloud","sbInfo":{"url":"https://x.database.meoo.xyz","publicUrl":"https://x.database.meoo.xyz","anonKey":"anon","vpcHost":""}}}}`)
	case strings.HasSuffix(path, "/cloud") && r.Method == http.MethodPost:
		// The bare shape: no success/data wrapper.
		f.mu.Lock()
		f.cloudPOSTs++
		f.mu.Unlock()
		if cloudPOSTStatus != 0 {
			w.WriteHeader(cloudPOSTStatus)
			writeRawJSON(w, `{"success":false,"message":"云服务实例数已达上限"}`)
			return
		}
		writeRawJSON(w, `{"status":"ready","created":true,"public_url":"https://l1l7coxwlq0f.meoo.pub","warnings":[]}`)
	case strings.HasSuffix(path, "/cli-compat/projects") && r.Method == http.MethodGet:
		if projectsStatus != 0 {
			w.WriteHeader(projectsStatus)
			writeRawJSON(w, `{"success":false,"message":"projects unavailable"}`)
			return
		}
		if projectsEmpty {
			writeRawJSON(w, `{"success":true,"data":{"list":[],"total":0,"page":1,"page_size":20,"hasMore":false,"nextCursor":null}}`)
			return
		}
		if projects != nil {
			list, errMarshal := json.Marshal(projects)
			if errMarshal != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeRawJSON(w, `{"success":true,"data":{"list":`+string(list)+`,"total":`+itoaInt(len(projects))+`,"page":1,"page_size":20,"hasMore":false,"nextCursor":null}}`)
			return
		}
		writeRawJSON(w, `{"success":true,"data":{"list":[{"url_id":"l1l7coxwlq0f","name":"CPA-Model-Gateway","status":"active","type":"create-desktop","runtimeStrategy":"legacy","accessUrl":"https://x.meoo.pub"}],"total":1,"page":1,"page_size":20,"hasMore":false,"nextCursor":null}}`)
	case strings.HasSuffix(path, "/cli-compat/projects") && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.createName = body.Name
		f.mu.Unlock()
		// The bare shape again: the created project, no wrapper.
		writeRawJSON(w, `{"url_id":"newproj1","name":"`+body.Name+`","type":"`+body.Type+`","created_at":1794541686000}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		writeRawJSON(w, `{"success":false,"message":"unexpected path `+path+`"}`)
	}
}

func (f *fakeOpenPlatform) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return server
}

// httptestJSON starts a local server whose answer is decided by respond, and
// returns its base URL. It exists so a route-level test can aim the gateway or the
// open platform at a body it controls without a second fake struct.
func httptestJSON(t *testing.T, respond func(path string) (status int, body string)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := respond(r.URL.Path)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		writeRawJSON(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// -----------------------------------------------------------------------------
// Contract pins
// -----------------------------------------------------------------------------

// TestDefaultEndpointsMatchTheContract pins both hosts and both credential
// prefixes: they are the interface, and a typo here is a plugin that silently
// talks to nothing.
func TestDefaultEndpointsMatchTheContract(t *testing.T) {
	if openPlatformBase != "https://meoo.com/open/v1" {
		t.Fatalf("open-platform base = %q", openPlatformBase)
	}
	if gatewayBase != "https://api.meoo.host/meoo-ai/compatible-mode/v1" {
		t.Fatalf("gateway base = %q", gatewayBase)
	}
	if userKeyPrefix != "meoo_ak_" {
		t.Fatalf("user key prefix = %q", userKeyPrefix)
	}
	if projectKeyPrefix != "meoo_project_service_v1." {
		t.Fatalf("project key prefix = %q", projectKeyPrefix)
	}
	if err := (&apiError{Status: 0}).Error(); err != "" {
		t.Fatalf("a status-less apiError must render empty, got %q", err)
	}
}

// TestHostHTTPResponseWireIsPascalCase is the regression for a silent defect: the
// host marshals pluginapi.HTTPResponse, which carries no json tags, so the wire
// keys are StatusCode/Headers/Body. Decoding them as snake_case yields status 0
// for every answer — a real 403 becomes "the site never answered".
func TestHostHTTPResponseWireIsPascalCase(t *testing.T) {
	body := []byte(`{"ok":true}`)
	wire := `{"StatusCode":403,"Headers":{"Content-Type":["application/json"]},"Body":"` +
		base64.StdEncoding.EncodeToString(body) + `"}`

	var decoded rpcHostHTTPResponseWire
	if errJSON := json.Unmarshal([]byte(wire), &decoded); errJSON != nil {
		t.Fatalf("decode PascalCase host response: %v", errJSON)
	}
	if decoded.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want 403 — the wire struct must read the PascalCase keys", decoded.StatusCode)
	}
	if string(decoded.Body) != string(body) {
		t.Fatalf("Body = %q, want %q (base64 must be decoded by encoding/json)", decoded.Body, body)
	}
	if got := decoded.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("Headers were not decoded: %+v", decoded.Headers)
	}

	// The other spelling must NOT be what carries the status: a struct that only
	// knows status_code would read this payload as no answer at all.
	var snake rpcHostHTTPResponseWire
	if errJSON := json.Unmarshal([]byte(`{"status_code":403,"body":"`+base64.StdEncoding.EncodeToString(body)+`"}`), &snake); errJSON != nil {
		t.Fatalf("decode snake_case payload: %v", errJSON)
	}
	if snake.StatusCode != 0 {
		t.Fatalf("the wire struct must not be driven by status_code, got %d", snake.StatusCode)
	}
}

// TestReadAccountWalksTheWholeDocumentedChain pins the order and the credential
// dialect: Authorization: Bearer <用户级 key> against the open-platform base.
func TestReadAccountWalksTheWholeDocumentedChain(t *testing.T) {
	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	swapOpenPlatform(t, platform.server(t).URL)

	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

	if read.Outcome != outcomeOK || !read.HasNumbers {
		t.Fatalf("a healthy account was classified %q (numbers=%v): %s", read.Outcome, read.HasNumbers, read.Note)
	}
	if read.Available == nil || *read.Available != 11146 {
		t.Fatalf("available = %v, want 11146", read.Available)
	}
	if read.Granted == nil || *read.Granted != 12000 {
		t.Fatalf("granted = %v, want 12000", read.Granted)
	}
	if read.Consumed == nil || *read.Consumed != 854 {
		t.Fatalf("consumed = %v, want 854", read.Consumed)
	}
	if read.PlanCode != "free" || read.PlanName != "Free 免费版" {
		t.Fatalf("plan = %q/%q", read.PlanCode, read.PlanName)
	}
	if read.ProjectURLID != "l1l7coxwlq0f" || read.ProjectName != "CPA-Model-Gateway" {
		t.Fatalf("project = %q/%q", read.ProjectURLID, read.ProjectName)
	}
	if read.ProjectAK != fakeProjectAK {
		t.Fatalf("the project-level AK was not picked up from the environment leg")
	}
	if !read.CloudEnabled {
		t.Fatal("the cloud leg reported enabled=true upstream")
	}
	if read.IdentityName != "好看的风" {
		t.Fatalf("identity name = %q", read.IdentityName)
	}
	if read.Note != "" {
		t.Fatalf("a clean walk must not carry notes: %s", read.Note)
	}

	if got := platform.authorization; got != "Bearer "+fakeUserKey {
		t.Fatalf("Authorization = %q, want the user-level key as a bearer token", got)
	}

	want := []string{
		"GET /cli-compat/account/identity",
		"GET /cli-compat/account/points",
		"GET /cli-compat/account/entitlement",
		"GET /cli-compat/projects",
		"GET /cli-compat/projects/l1l7coxwlq0f/cloud",
		"GET /cli-compat/projects/l1l7coxwlq0f/cloud/environment",
	}
	got := platform.callList()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("walk order/legs drifted:\n got %v\nwant %v", got, want)
	}
}

// TestReadAccountAuthFailureIsDeadAndServerErrorIsNot is the classification rule
// the panel's wording depends on: only an explicit refusal kills the row.
func TestReadAccountAuthFailureIsDeadAndServerErrorIsNot(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolateMeoo(t)
			platform := newFakeOpenPlatform()
			platform.authErrStatus = status
			swapOpenPlatform(t, platform.server(t).URL)

			read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
			if read.Outcome != outcomeDead {
				t.Fatalf("HTTP %d must classify the account dead, got %q", status, read.Outcome)
			}
			if read.HasNumbers {
				t.Fatal("a dead key must not report numbers")
			}
			if !strings.Contains(read.Note, "401/403") {
				t.Fatalf("the note must name the verdict: %s", read.Note)
			}
		})
	}

	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	platform.authErrStatus = http.StatusInternalServerError
	swapOpenPlatform(t, platform.server(t).URL)
	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
	if read.Outcome != outcomeTransient {
		t.Fatalf("HTTP 500 is the site's problem, not a key verdict: got %q", read.Outcome)
	}
	if read.HasNumbers {
		t.Fatal("nothing was readable, so no numbers may be claimed")
	}
}

// TestReadAccountSurvivesABrokenProjectLeg: points and entitlement are
// account-scoped, so a project failure must not blank the numbers.
func TestReadAccountSurvivesABrokenProjectLeg(t *testing.T) {
	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	platform.projectsStatus = http.StatusInternalServerError
	swapOpenPlatform(t, platform.server(t).URL)

	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
	if read.Outcome != outcomeOK || !read.HasNumbers {
		t.Fatalf("a project-leg failure must not fail the account: %q %s", read.Outcome, read.Note)
	}
	if read.Available == nil || *read.Available != 11146 {
		t.Fatalf("numbers were lost with the project leg: %v", read.Available)
	}
	if !strings.Contains(read.Note, "项目") {
		t.Fatalf("the operator must be told the project leg failed: %s", read.Note)
	}
}

// TestReadAccountFallsBackToTheEntitlementBenefit: when points answers with no
// ledger at all, the entitlement's model_credits row states the same allowance —
// use it rather than showing an empty card.
func TestReadAccountFallsBackToTheEntitlementBenefit(t *testing.T) {
	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	platform.pointsBody = `{"success":true,"data":{"account":{},"activeGrants":[]}}`
	swapOpenPlatform(t, platform.server(t).URL)

	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
	if !read.HasNumbers {
		t.Fatalf("the entitlement fallback did not produce numbers: %s", read.Note)
	}
	if read.Granted == nil || *read.Granted != 12000 || read.Consumed == nil || *read.Consumed != 854 {
		t.Fatalf("entitlement numbers mis-parsed: granted=%v consumed=%v", read.Granted, read.Consumed)
	}
	if read.Available == nil || *read.Available != 11146 {
		t.Fatalf("available should be quota-used = 11146, got %v", read.Available)
	}
	if !strings.Contains(read.Note, "entitlement") {
		t.Fatalf("the fallback must be declared in the note: %s", read.Note)
	}
}

// TestReadAccountReportsNothingWhenBothSourcesAreEmpty: absent is not zero.
func TestReadAccountReportsNothingWhenBothSourcesAreEmpty(t *testing.T) {
	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	platform.pointsBody = `{"success":true,"data":{"account":{}}}`
	platform.entitlementBody = `{"success":true,"data":{"planCode":"free","planName":"Free 免费版","benefits":[]}}`
	swapOpenPlatform(t, platform.server(t).URL)

	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
	if read.HasNumbers {
		t.Fatal("no source produced a number, so HasNumbers must stay false")
	}
	if read.Available != nil || read.Granted != nil || read.Consumed != nil {
		t.Fatalf("absent numbers must stay nil, got %v/%v/%v", read.Available, read.Granted, read.Consumed)
	}
	if read.Outcome != outcomeTransient {
		t.Fatalf("outcome = %q, want transient", read.Outcome)
	}
}

// TestAutoProvisionCreatesProjectAndEnablesCloud covers BOTH sides of the
// auto_provision switch, and the project_name the create call must carry.
func TestAutoProvisionCreatesProjectAndEnablesCloud(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) {
			cfg.AutoProvision = true
			cfg.ProjectName = "CPA-Model-Gateway-Test"
		})
		platform := newFakeOpenPlatform()
		platform.projectsEmpty = true
		platform.cloudEnabled = false
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
		if read.ProjectURLID != "newproj1" {
			t.Fatalf("the created project was not adopted: %q (%s)", read.ProjectURLID, read.Note)
		}
		if platform.createName != "CPA-Model-Gateway-Test" {
			t.Fatalf("the create call carried name %q, want the configured project_name", platform.createName)
		}
		if platform.cloudPOSTs != 1 {
			t.Fatalf("cloud POSTs = %d, want 1", platform.cloudPOSTs)
		}
		if read.ProjectAK != fakeProjectAK {
			t.Fatalf("the project AK leg did not run after provisioning")
		}
		if !strings.Contains(read.Note, "自动创建项目") || !strings.Contains(read.Note, "开通云服务") {
			t.Fatalf("the notes must declare what was changed upstream: %s", read.Note)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = false })
		platform := newFakeOpenPlatform()
		platform.projectsEmpty = true
		platform.cloudEnabled = false
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})
		if platform.sawCall("POST /cli-compat/projects") {
			t.Fatal("auto_provision=false must never create a project")
		}
		if platform.cloudPOSTs != 0 {
			t.Fatal("auto_provision=false must never enable a cloud instance")
		}
		if read.ProjectURLID != "" || read.ProjectAK != "" {
			t.Fatalf("nothing should have been provisioned: %+v", read)
		}
		if !strings.Contains(read.Note, "auto_provision=false") {
			t.Fatalf("the note must explain why nothing was provisioned: %s", read.Note)
		}
		if !read.HasNumbers {
			t.Fatal("points are account-scoped: no project must not mean no numbers")
		}
	})
}

// TestStoredProjectIsReusedWhenTheListHasNoActiveRow: the walk must not create a
// second project for an account that already has one.
func TestStoredProjectIsReusedWhenTheListHasNoActiveRow(t *testing.T) {
	isolateMeoo(t)
	setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = true })
	platform := newFakeOpenPlatform()
	platform.projectsEmpty = true
	swapOpenPlatform(t, platform.server(t).URL)

	read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey, ProjectURLID: "stored-1", ProjectAK: fakeProjectAK2})
	if platform.sawCall("POST /cli-compat/projects") {
		t.Fatal("an account that already has a stored project must not be re-provisioned")
	}
	if read.ProjectURLID != "stored-1" {
		t.Fatalf("the stored project must survive an empty active list, got %q", read.ProjectURLID)
	}
}

// TestCallOpenPlatformRefusesTheProjectFamilyLocally: the request must never be
// built, let alone sent.
func TestCallOpenPlatformRefusesTheProjectFamilyLocally(t *testing.T) {
	fired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fired = true
		writeRawJSON(w, `{"success":true,"data":{}}`)
	}))
	defer server.Close()
	swapOpenPlatform(t, server.URL)

	if _, _, errCall := callOpenPlatform(fakeProjectAK, http.MethodGet, "/cli-compat/account/identity", nil); errCall == nil {
		t.Fatal("a project-level key must be refused on the open platform")
	} else if !strings.Contains(errCall.Error(), "项目级") {
		t.Fatalf("the refusal must name the family mismatch: %v", errCall)
	}
	if _, _, errCall := callOpenPlatform("sk-something-else", http.MethodGet, "/cli-compat/account/identity", nil); errCall == nil {
		t.Fatal("a key with the wrong prefix must be refused")
	}
	if _, _, errCall := callOpenPlatform("  ", http.MethodGet, "/cli-compat/account/identity", nil); errCall == nil {
		t.Fatal("an empty key must be refused")
	}
	if _, _, errCall := callOpenPlatform(fakeUserKey, http.MethodGet, "cli-compat/account/identity", nil); errCall == nil {
		t.Fatal("a relative path must be refused")
	}
	if fired {
		t.Fatal("not one of those refusals may reach the network")
	}
}

// TestGatewayRefusesTheUserFamilyLocally is the mirror of the test above.
func TestGatewayRefusesTheUserFamilyLocally(t *testing.T) {
	fired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fired = true
		writeRawJSON(w, `{"models":[]}`)
	}))
	defer server.Close()
	swapGateway(t, server.URL)

	if _, errFetch := fetchGatewayModels(fakeUserKey); errFetch == nil {
		t.Fatal("a user-level key must be refused by the gateway")
	} else if !strings.Contains(errFetch.Error(), "项目级") {
		t.Fatalf("the refusal must name the family mismatch: %v", errFetch)
	}
	if _, errFetch := fetchGatewayModels(""); errFetch == nil {
		t.Fatal("an empty project key must be refused")
	}
	if _, errFetch := fetchGatewayModels("garbage"); errFetch == nil {
		t.Fatal("a key with the wrong prefix must be refused")
	}
	if fired {
		t.Fatal("not one of those refusals may reach the network")
	}
}

// TestGatewayCatalogueAndTheKnownUnusableModel: qwen4-max-preview answers 403 on
// this gateway for everyone (Token Plan only), so it must be offered as
// unavailable rather than counted as a channel.
func TestGatewayCatalogueAndTheKnownUnusableModel(t *testing.T) {
	isolateMeoo(t)
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRawJSON(w, `{"models":["qwen3.8-flash","qwen4-max-preview"],"defaultModel":"qwen3.8-flash"}`)
	}))
	defer server.Close()
	swapGateway(t, server.URL)

	catalogue, errFetch := fetchGatewayModels(fakeProjectAK)
	if errFetch != nil {
		t.Fatalf("fetchGatewayModels: %v", errFetch)
	}
	if len(catalogue.Models) != 2 || catalogue.DefaultModel != "qwen3.8-flash" {
		t.Fatalf("catalogue mis-parsed: %+v", catalogue)
	}
	if sawAuth != "Bearer "+fakeProjectAK {
		t.Fatalf("gateway Authorization = %q", sawAuth)
	}

	if usable, reason := gatewayModelUsable("qwen3.8-flash"); !usable || reason != "" {
		t.Fatalf("a normal model must be usable with no note, got %v/%q", usable, reason)
	}
	usable, reason := gatewayModelUsable("qwen4-max-preview")
	if usable {
		t.Fatal("qwen4-max-preview is 403 on this gateway and must be marked unusable")
	}
	if !strings.Contains(reason, "403") || !strings.Contains(reason, "Token Plan") {
		t.Fatalf("the note must explain the fixed 403: %q", reason)
	}
	if usable, _ := gatewayModelUsable("QWEN4-MAX-PREVIEW"); usable {
		t.Fatal("the unusable table must be case-insensitive")
	}

	if got := displayModelName("qwen3.8-flash"); got != "Qwen3.8 Flash" {
		t.Fatalf("displayModelName = %q", got)
	}
	if got := displayModelName(""); got != "" {
		t.Fatalf("displayModelName(\"\") = %q", got)
	}
}

// TestBarePostAnswersAreAccepted: POST /projects and POST /projects/{id}/cloud
// answer without the success/data wrapper, so the unwrapper must fall back to the
// whole body.
func TestBarePostAnswersAreAccepted(t *testing.T) {
	isolateMeoo(t)
	platform := newFakeOpenPlatform()
	platform.cloudEnabled = false
	swapOpenPlatform(t, platform.server(t).URL)

	created, errCreate := createProject(fakeUserKey, "CPA-Model-Gateway")
	if errCreate != nil {
		t.Fatalf("createProject: %v", errCreate)
	}
	if created.URLID != "newproj1" || created.Name != "CPA-Model-Gateway" {
		t.Fatalf("the bare create answer was not parsed: %+v", created)
	}
	if created.Type != "create-desktop" {
		t.Fatalf("the create call must ask for type=create-desktop, echoed %q", created.Type)
	}
	if errEnable := enableCloud(fakeUserKey, "newproj1"); errEnable != nil {
		t.Fatalf("enableCloud: %v", errEnable)
	}
	if platform.cloudPOSTs != 1 {
		t.Fatalf("cloud POSTs = %d", platform.cloudPOSTs)
	}

	// A create answer without a url_id is a failure, not a project called "".
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, `{"name":"x","type":"create-desktop"}`)
	}))
	defer server.Close()
	swapOpenPlatform(t, server.URL)
	if _, errCreate := createProject(fakeUserKey, "x"); errCreate == nil {
		t.Fatal("a create answer without url_id must be refused")
	}
}

// TestRefusalsAndErrorsAreRedacted: upstream text reaches notes and log lines, so
// a body that echoes a credential back must not carry it there.
func TestRefusalsAndErrorsAreRedacted(t *testing.T) {
	isolateMeoo(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeRawJSON(w, `{"success":false,"message":"bad key `+fakeUserKey+` / `+fakeProjectAK+`"}`)
	}))
	defer server.Close()
	swapOpenPlatform(t, server.URL)

	_, _, errCall := callOpenPlatform(fakeUserKey, http.MethodGet, "/cli-compat/account/points", nil)
	if errCall == nil {
		t.Fatal("a 401 must be an error")
	}
	if !isAuthError(errCall) {
		t.Fatalf("a 401 must be classified as an auth failure: %v", errCall)
	}
	text := errCall.Error()
	for _, secret := range []string{fakeUserKey, fakeProjectAK} {
		if strings.Contains(text, secret) {
			t.Fatalf("the error text echoed a credential: %s", text)
		}
	}
	if !strings.Contains(text, userKeyPrefix+"***") {
		t.Fatalf("the error text should mask the credential shape: %s", text)
	}

	if got := redactSecrets("token=" + fakeProjectAK); strings.Contains(got, fakeProjectAK) {
		t.Fatalf("redactSecrets left the key in place: %s", got)
	}
	if got := redactSecrets("nothing to hide here"); got != "nothing to hide here" {
		t.Fatalf("redactSecrets damaged clean text: %s", got)
	}
	long := note(strings.Repeat("x", 500))
	if len(long) > upstreamExcerptLen+4 {
		t.Fatalf("note() did not bound its input: %d bytes", len(long))
	}
}

// TestAPIErrorClassification pins the three shapes.
func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		auth   bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}
	for _, testCase := range cases {
		errCall := newAPIError(testCase.status, "upstream said no")
		if errCall.Auth != testCase.auth {
			t.Fatalf("HTTP %d: Auth = %v, want %v", testCase.status, errCall.Auth, testCase.auth)
		}
		if !strings.Contains(errCall.Error(), "HTTP "+itoaInt(testCase.status)) {
			t.Fatalf("the error must name the status: %s", errCall.Error())
		}
		if isAuthError(nil) {
			t.Fatal("nil is not an auth failure")
		}
		if isAuthError(io.EOF) {
			t.Fatal("a transport error is not an auth failure")
		}
	}
}

// itoaInt is a tiny helper so this file needs no strconv import for one call.
func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestCredentialSafeRedirectRefusesCrossOrigin: the direct client carries a
// credential in a header, and Go's default client copies headers onto a redirect.
func TestCredentialSafeRedirectRefusesCrossOrigin(t *testing.T) {
	check := credentialSafeRedirect()
	origin, errOrigin := http.NewRequest(http.MethodGet, "https://meoo.com/open/v1/cli-compat/projects", nil)
	if errOrigin != nil {
		t.Fatalf("build origin: %v", errOrigin)
	}
	sameOrigin, errSame := http.NewRequest(http.MethodGet, "https://meoo.com/open/v1/other", nil)
	if errSame != nil {
		t.Fatalf("build same-origin: %v", errSame)
	}
	crossOrigin, errCross := http.NewRequest(http.MethodGet, "https://attacker.invalid/collect", nil)
	if errCross != nil {
		t.Fatalf("build cross-origin: %v", errCross)
	}
	if errRedirect := check(sameOrigin, []*http.Request{origin}); errRedirect != nil {
		t.Fatalf("a same-origin redirect must be allowed: %v", errRedirect)
	}
	if errRedirect := check(crossOrigin, []*http.Request{origin}); errRedirect == nil {
		t.Fatal("a cross-origin redirect must be refused")
	}
	if errRedirect := check(sameOrigin, nil); errRedirect != nil {
		t.Fatalf("the first hop has nothing to compare and must pass: %v", errRedirect)
	}
}

// TestJoinNotesAndProjectAKState details.
func TestJoinNotes(t *testing.T) {
	if got := joinNotes("", ""); got != "" {
		t.Fatalf("joinNotes(empty, empty) = %q", got)
	}
	if got := joinNotes("", "b"); got != "b" {
		t.Fatalf("joinNotes(empty, b) = %q", got)
	}
	if got := joinNotes("a", ""); got != "a" {
		t.Fatalf("joinNotes(a, empty) = %q", got)
	}
	if got := joinNotes("a", "b"); got != "a；b" {
		t.Fatalf("joinNotes(a, b) = %q", got)
	}
}

// -----------------------------------------------------------------------------
// The multi-project fallback probe
// -----------------------------------------------------------------------------

// walkHead is the account-scoped part every readAccount walk starts with, in
// order. The project legs are appended per test.
func walkHead() []string {
	return []string{
		"GET /cli-compat/account/identity",
		"GET /cli-compat/account/points",
		"GET /cli-compat/account/entitlement",
		"GET /cli-compat/projects",
	}
}

// requireCallSequence pins the COMPLETE request order. A probe is an addition to
// a walk whose shape is itself a contract, so "no extra request" is only provable
// by comparing every leg, not by looking for one absent path.
func requireCallSequence(t *testing.T, platform *fakeOpenPlatform, want []string) {
	t.Helper()
	if got := platform.callList(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("walk drifted:\n got %v\nwant %v", got, want)
	}
}

// requireNoWrites fails on any write. The probe exists to LOOK: enableCloud and
// createProject are exactly what it must never do.
func requireNoWrites(t *testing.T, platform *fakeOpenPlatform) {
	t.Helper()
	if platform.cloudPOSTs != 0 {
		t.Fatalf("cloud POSTs = %d, want 0", platform.cloudPOSTs)
	}
	for _, call := range platform.callList() {
		if strings.HasPrefix(call, "POST ") {
			t.Fatalf("a read-only probe must not write anything upstream, saw %q", call)
		}
	}
	if platform.sawCall("POST /cli-compat/projects") {
		t.Fatal("the discovery chain must not create a project here")
	}
}

// measuredRows is the account from the incident report: three active projects in
// this order, and only the third one owns a cloud instance — hence the only
// project-level key.
func measuredRows() []fakeProjectRow {
	return []fakeProjectRow{
		activeRow("o1gyf6uzfh9f", "本地客户端咨询", false, ""),
		activeRow("a8dl5ebxoj66", "云服务开通查询", false, ""),
		activeRow("l1l7coxwlq0f", "CPA-Model-Gateway", true, fakeProjectAK),
	}
}

// TestProjectAKProbeAdoptsTheProjectThatCarriesTheKey is the regression for the
// measured incident (2026-11-13): one account, three active projects in this
// order, and only the THIRD one owns a cloud instance — hence the only
// project-level key. Picking the first active project left project_ak_tail empty
// and the panel reported "没有项目级 AK" while a usable key was right there.
func TestProjectAKProbeAdoptsTheProjectThatCarriesTheKey(t *testing.T) {
	rows := measuredRows()

	t.Run("auto_provision 关闭：只读探测", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = false })
		platform := newFakeOpenPlatform()
		platform.projects = rows
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if read.ProjectAK != fakeProjectAK {
			t.Fatalf("the account's usable project-level AK was not found: %q (%s)", read.ProjectAK, read.Note)
		}
		if read.ProjectURLID != "l1l7coxwlq0f" || read.ProjectName != "CPA-Model-Gateway" {
			t.Fatalf("the leg must move to the project that carries the key, got %q/%q", read.ProjectURLID, read.ProjectName)
		}
		if !read.CloudEnabled {
			t.Fatal("the adopted project owns a cloud instance, so cloud_enabled must be true")
		}
		if !strings.Contains(read.Note, "已改选带云服务的项目 l1l7coxwlq0f") {
			t.Fatalf("the note must declare the switch: %s", read.Note)
		}
		if !strings.Contains(read.Note, "项目级 AK 读取失败") {
			t.Fatalf("the original note wording must survive the addition: %s", read.Note)
		}
		if read.Outcome != outcomeOK || !read.HasNumbers {
			t.Fatalf("points are account-scoped: %q numbers=%v", read.Outcome, read.HasNumbers)
		}

		// Only the project whose cloud is ENABLED is asked for its environment:
		// a8dl5ebxoj66 has no cloud instance, so it is skipped after its cloud read.
		requireCallSequence(t, platform, append(walkHead(),
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud",
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud/environment",
			"GET /cli-compat/projects/a8dl5ebxoj66/cloud",
			"GET /cli-compat/projects/l1l7coxwlq0f/cloud",
			"GET /cli-compat/projects/l1l7coxwlq0f/cloud/environment",
		))
		requireNoWrites(t, platform)
	})

	t.Run("开通被套餐上限拒绝：仍然探测", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = true })
		platform := newFakeOpenPlatform()
		platform.projects = rows
		// Free 套餐: the cloud-instance quota is used up, which is how the real
		// account answered when the first project was auto-provisioned.
		platform.cloudPOSTStatus = http.StatusBadRequest
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if read.ProjectAK != fakeProjectAK || read.ProjectURLID != "l1l7coxwlq0f" {
			t.Fatalf("the probe must also run when provisioning was refused: %q/%q (%s)", read.ProjectURLID, read.ProjectAK, read.Note)
		}
		if !strings.Contains(read.Note, "自动开通云服务失败") {
			t.Fatalf("the refusal must be declared: %s", read.Note)
		}
		if !strings.Contains(read.Note, "已改选带云服务的项目 l1l7coxwlq0f") {
			t.Fatalf("the note must declare the switch: %s", read.Note)
		}
		if platform.cloudPOSTs != 1 {
			t.Fatalf("only the walk's own attempt may be a write: cloud POSTs = %d", platform.cloudPOSTs)
		}
		requireCallSequence(t, platform, append(walkHead(),
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud",
			"POST /cli-compat/projects/o1gyf6uzfh9f/cloud",
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud/environment",
			"GET /cli-compat/projects/a8dl5ebxoj66/cloud",
			"GET /cli-compat/projects/l1l7coxwlq0f/cloud",
			"GET /cli-compat/projects/l1l7coxwlq0f/cloud/environment",
		))
		if platform.sawCall("POST /cli-compat/projects") {
			t.Fatal("a probe must never create a project")
		}
	})

	t.Run("已存项目优先于列表顺序", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = false })
		platform := newFakeOpenPlatform()
		platform.projects = []fakeProjectRow{
			activeRow("first-active", "第一个 active", false, ""),
			activeRow("earlier-in-list", "列表里更靠前", true, fakeProjectAK2),
			activeRow("stored-project", "本插件已知的项目", true, fakeProjectAK),
		}
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey, ProjectURLID: "stored-project"})

		if read.ProjectAK != fakeProjectAK || read.ProjectURLID != "stored-project" || read.ProjectName != "本插件已知的项目" {
			t.Fatalf("the stored project must be probed first: %q/%q/%q (%s)", read.ProjectURLID, read.ProjectName, read.ProjectAK, read.Note)
		}
		// The candidate ORDER is observable: the probe stops at the stored project,
		// so the earlier list row is never even asked for its cloud state.
		if platform.sawCall("GET /cli-compat/projects/earlier-in-list/cloud") {
			t.Fatal("a stored project that answers must end the probe; list order is only the fallback")
		}
		requireCallSequence(t, platform, append(walkHead(),
			"GET /cli-compat/projects/first-active/cloud",
			"GET /cli-compat/projects/first-active/cloud/environment",
			"GET /cli-compat/projects/stored-project/cloud",
			"GET /cli-compat/projects/stored-project/cloud/environment",
		))
		requireNoWrites(t, platform)
	})
}

// TestProjectAKProbeNeverProvisionsWhenNoProjectHasACloudInstance is the negative
// control: with three active projects and a cloud instance on none of them, the
// probe must not open one. On a Free plan that POST is the very failure this
// fallback exists for (云服务实例数已达上限), and it leaves resources behind.
func TestProjectAKProbeNeverProvisionsWhenNoProjectHasACloudInstance(t *testing.T) {
	rows := []fakeProjectRow{
		activeRow("o1gyf6uzfh9f", "本地客户端咨询", false, ""),
		activeRow("a8dl5ebxoj66", "云服务开通查询", false, ""),
		activeRow("l1l7coxwlq0f", "CPA-Model-Gateway", false, ""),
	}

	t.Run("auto_provision 关闭", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = false })
		platform := newFakeOpenPlatform()
		platform.projects = rows
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if read.ProjectAK != "" {
			t.Fatalf("nothing upstream has a key, so the leg must stay empty: %q", read.ProjectAK)
		}
		// The exit state is the pre-change one: the first active project stays
		// chosen, nothing is opened, and the account keeps its numbers.
		if read.ProjectURLID != "o1gyf6uzfh9f" || read.CloudEnabled {
			t.Fatalf("the chosen project must be left where the old walk left it: %q cloud=%v", read.ProjectURLID, read.CloudEnabled)
		}
		if read.Outcome != outcomeOK || !read.HasNumbers {
			t.Fatalf("a key-less project leg is a note, never an account verdict: %q numbers=%v", read.Outcome, read.HasNumbers)
		}
		if !strings.Contains(read.Note, "项目级 AK 读取失败") {
			t.Fatalf("the original wording must survive: %s", read.Note)
		}
		if !strings.Contains(read.Note, "已检查其余 2 个项目，均无可用项目级 AK") {
			t.Fatalf("an exhausted probe must say how much it checked: %s", read.Note)
		}
		requireCallSequence(t, platform, append(walkHead(),
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud",
			"GET /cli-compat/projects/o1gyf6uzfh9f/cloud/environment",
			"GET /cli-compat/projects/a8dl5ebxoj66/cloud",
			"GET /cli-compat/projects/l1l7coxwlq0f/cloud",
		))
		requireNoWrites(t, platform)
	})

	t.Run("auto_provision 开启：只开通被选中的那一个", func(t *testing.T) {
		isolateMeoo(t)
		setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = true })
		platform := newFakeOpenPlatform()
		platform.projects = rows
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if platform.cloudPOSTs != 1 || !platform.sawCall("POST /cli-compat/projects/o1gyf6uzfh9f/cloud") {
			t.Fatalf("the probe opened cloud instances it was only meant to look at: %v", platform.callList())
		}
		if platform.sawCall("POST /cli-compat/projects") {
			t.Fatal("the probe must never create a project")
		}
		if read.ProjectURLID != "o1gyf6uzfh9f" || read.ProjectAK != "" {
			t.Fatalf("the walk's own choice must not move on an exhausted probe: %q/%q", read.ProjectURLID, read.ProjectAK)
		}
	})
}

// TestProjectLegStaysSingleShotWhenAKeyIsAlreadyFound: the probe is a FALLBACK,
// not an extra round of requests. As long as the existing walk yields a
// project-level key, the sequence must stay byte-for-byte the documented one.
func TestProjectLegStaysSingleShotWhenAKeyIsAlreadyFound(t *testing.T) {
	want := []string{
		"GET /cli-compat/account/identity",
		"GET /cli-compat/account/points",
		"GET /cli-compat/account/entitlement",
		"GET /cli-compat/projects",
		"GET /cli-compat/projects/l1l7coxwlq0f/cloud",
		"GET /cli-compat/projects/l1l7coxwlq0f/cloud/environment",
	}

	t.Run("单项目", func(t *testing.T) {
		isolateMeoo(t)
		platform := newFakeOpenPlatform()
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if read.ProjectAK != fakeProjectAK || read.ProjectURLID != "l1l7coxwlq0f" {
			t.Fatalf("the single-project walk changed shape: %q/%q", read.ProjectURLID, read.ProjectAK)
		}
		if read.Note != "" {
			t.Fatalf("a clean single-project walk must carry no note: %s", read.Note)
		}
		requireCallSequence(t, platform, want)
	})

	t.Run("多项目但首个 active 就有项目级 AK", func(t *testing.T) {
		isolateMeoo(t)
		platform := newFakeOpenPlatform()
		platform.projects = []fakeProjectRow{
			activeRow("l1l7coxwlq0f", "CPA-Model-Gateway", true, fakeProjectAK),
			activeRow("o1gyf6uzfh9f", "本地客户端咨询", true, fakeProjectAK2),
			activeRow("a8dl5ebxoj66", "云服务开通查询", true, fakeProjectAK2),
		}
		swapOpenPlatform(t, platform.server(t).URL)

		read := readAccount(accountRecord{ID: "abc", UserKey: fakeUserKey})

		if read.ProjectAK != fakeProjectAK || read.ProjectURLID != "l1l7coxwlq0f" {
			t.Fatalf("a key on the first active project must end the walk: %q/%q", read.ProjectURLID, read.ProjectAK)
		}
		if read.Note != "" {
			t.Fatalf("nothing was missing, so there is nothing to report: %s", read.Note)
		}
		requireCallSequence(t, platform, want)
	})
}

// TestMeasuredAccountReachesThePanelWithAProjectKey closes the loop on the
// user-visible half of the defect — the report was "面板显示没有项目级 AK". It
// drives the real sweep and reads the row the panel is served.
func TestMeasuredAccountReachesThePanelWithAProjectKey(t *testing.T) {
	isolateMeoo(t)
	setConfigForTest(t, func(cfg *pluginConfig) { cfg.AutoProvision = false })
	platform := newFakeOpenPlatform()
	platform.projects = measuredRows()
	swapOpenPlatform(t, platform.server(t).URL)

	if _, errAdd := addAccount(fakeUserKey, "实测账号"); errAdd != nil {
		t.Fatalf("addAccount: %v", errAdd)
	}
	refreshed, errRefresh := refreshAll(true)
	if errRefresh != nil || refreshed != 1 {
		t.Fatalf("refreshAll = %d, %v", refreshed, errRefresh)
	}

	payload := assembleView(refreshed, true)
	rows, okRows := payload["accounts"].([]accountView)
	if !okRows || len(rows) != 1 {
		t.Fatalf("panel rows = %#v", payload["accounts"])
	}
	row := rows[0]
	if row.ProjectAKState != "ok" {
		t.Fatalf("the panel still reports no project-level AK: %+v", row)
	}
	if row.ProjectURLID != "l1l7coxwlq0f" || row.ProjectName != "CPA-Model-Gateway" || !row.CloudEnabled {
		t.Fatalf("the panel must point at the project that carries the key: %+v", row)
	}
	if row.ProjectAKTail != keyTail(fakeProjectAK) {
		t.Fatalf("project_ak_tail = %q, want the tail of the adopted key", row.ProjectAKTail)
	}
	if !strings.Contains(row.Note, "已改选带云服务的项目 l1l7coxwlq0f") {
		t.Fatalf("the row must declare the switch: %s", row.Note)
	}
	summary, okSummary := payload["summary"].(meooSummary)
	if !okSummary {
		t.Fatalf("summary shape: %#v", payload["summary"])
	}
	for _, summaryNote := range summary.Notes {
		if strings.Contains(summaryNote, "还没有项目级 AK") {
			t.Fatalf("the account still counts as key-less: %s", summaryNote)
		}
	}

	// The adopted project and its key belong in the account store: the sweep is
	// the only writer of either.
	accounts, errList := listAccounts()
	if errList != nil || len(accounts) != 1 {
		t.Fatalf("listAccounts: %v (%d accounts)", errList, len(accounts))
	}
	if accounts[0].ProjectURLID != "l1l7coxwlq0f" || accounts[0].ProjectAK != fakeProjectAK {
		t.Fatalf("the switch was not persisted: %q/%q", accounts[0].ProjectURLID, accounts[0].ProjectAK)
	}
}
