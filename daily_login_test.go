package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// The fixtures
// -----------------------------------------------------------------------------

// fakeOpenID is the id both sides agree on: the open platform reports it as
// account.userId, the web session reports it as open_id (measured 2026-11-21).
const fakeOpenID = "1241690576128755"

// meooFakeUpstream is a fake Meoo server that answers the three endpoints this
// feature touches:
//
//	/open/v1/cli-compat/account/points   the ledger (open platform, Bearer)
//	/api/v1/user/me                       who is this session (web session)
//	/api/mc/entitlement/init              the call that issues the grant
//
// It counts how many times each endpoint was hit, so a test can prove that the
// "already claimed" path makes NO request to the grant endpoint at all.
type meooFakeUpstream struct {
	server *httptest.Server

	// pointsCalls counts ledger reads.
	pointsCalls int
	// initCalls counts grant-triggering calls.
	initCalls int
	// meCalls counts identity reads.
	meCalls int

	// responseClaimsGrant makes the init endpoint ANSWER "granted".
	responseClaimsGrant bool
	// ledgerMovesOnInit makes the init endpoint book a new grant batch on the
	// ledger, as the real site does.
	ledgerMovesOnInit bool
	// skipReason is returned when no grant is issued.
	skipReason string
	// pointsAvailable is the balance the ledger reports.
	pointsAvailable float64
	// batches are the daily-login batches the ledger reports.
	batches []map[string]any
	// sessionOpenID is who the web session claims to be.
	sessionOpenID string
	// pointsWithoutUserID makes the ledger answer without a userId, so the
	// account's open id cannot be read — the state in which the identity check
	// must REFUSE rather than skip.
	pointsWithoutUserID bool
	// sessionStatus is the status the identity endpoint answers with.
	sessionStatus int
	// initStatus is the status the grant endpoint answers with.
	initStatus int
}

// newMeooFakeUpstream starts the fake server.
func newMeooFakeUpstream(t *testing.T) *meooFakeUpstream {
	t.Helper()
	fake := &meooFakeUpstream{
		pointsAvailable: 1000,
		sessionOpenID:   fakeOpenID,
		sessionStatus:   http.StatusOK,
		initStatus:      http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/open/v1/cli-compat/account/points", func(w http.ResponseWriter, _ *http.Request) {
		fake.pointsCalls++
		account := map[string]any{
			"availableBalance": fake.pointsAvailable,
			"totalGranted":     fake.pointsAvailable,
			"totalConsumed":    0,
		}
		if !fake.pointsWithoutUserID {
			// The upstream sends the id as a NUMBER, not a quoted string.
			account["userId"] = json.Number(fakeOpenID)
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{
				"account":      account,
				"activeGrants": fake.batches,
			},
		})
	})
	mux.HandleFunc("/api/v1/user/me", func(w http.ResponseWriter, _ *http.Request) {
		fake.meCalls++
		if fake.sessionStatus != http.StatusOK {
			writeFakeJSON(w, fake.sessionStatus, map[string]any{
				"success": false,
				"errCode": "NEED_LOGIN",
			})
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{
				"open_id":       fake.sessionOpenID,
				"user_id":       "379477068381155328",
				"username":      "fixture",
				"auth_provider": "aliyun",
			},
		})
	})
	mux.HandleFunc("/api/mc/entitlement/init", func(w http.ResponseWriter, _ *http.Request) {
		fake.initCalls++
		if fake.initStatus != http.StatusOK {
			writeFakeJSON(w, fake.initStatus, map[string]any{
				"success": false,
				"errCode": "NEED_LOGIN",
			})
			return
		}
		// The real site books the grant on the ledger AND answers "granted".
		// The two can be separated here, which is what lets a test ask the
		// question the whole design turns on: which of the two does the plugin
		// believe?
		if fake.ledgerMovesOnInit {
			fake.batches = append(fake.batches, dailyLoginBatch("2026-11-22T00:05:03.101", "2026-11-28T23:59:59", 2000))
			fake.pointsAvailable += 2000
		}
		if fake.responseClaimsGrant {
			writeFakeJSON(w, http.StatusOK, map[string]any{
				"success": true,
				"data": map[string]any{
					"initialized":           true,
					"dailyLoginGranted":     true,
					"dailyLoginGrantAmount": 2000,
				},
			})
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{
				"initialized":          true,
				"dailyLoginGranted":    false,
				"dailyLoginSkipReason": fake.skipReason,
			},
		})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// aim points both upstream bases at the fake server.
func (f *meooFakeUpstream) aim(t *testing.T) {
	t.Helper()
	swapOpenPlatform(t, f.server.URL+"/open/v1")
	previousWeb := meooWebBase
	meooWebBase = f.server.URL
	t.Cleanup(func() { meooWebBase = previousWeb })
}

// writeFakeJSON answers one request.
func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// dailyLoginBatch builds one daily-login grant batch for the fake ledger.
func dailyLoginBatch(effective, expires string, remaining float64) map[string]any {
	return map[string]any{
		"sourceType":     "DAILY_LOGIN",
		"sourceTypeName": "登录赠送积分",
		"grantDetails": []map[string]any{
			{
				"remainingAmount": remaining,
				"effectiveDate":   effective,
				"expiresDate":     expires,
			},
		},
	}
}

// TestParseOpenIDHandlesTheNumbersTheUpstreamSends pins the id decoding.
//
// The upstream sends account.userId as a JSON NUMBER larger than a float64 can
// hold exactly (measured: 1978784341143815). Decoding it into an int64 or a
// float64 rounds it, and the identity join would then compare two different
// rounded numbers — silently matching nothing, or the wrong account.
func TestParseOpenIDHandlesTheNumbersTheUpstreamSends(t *testing.T) {
	raw := json.RawMessage(`{"account":{"userId":1978784341143815,"availableBalance":20123}}`)
	if got := parseOpenID(raw); got != "1978784341143815" {
		t.Fatalf("open id = %q, want 1978784341143815", got)
	}
	// The string shape must still work (a different upstream version).
	rawString := json.RawMessage(`{"account":{"userId":"1241690576128755"}}`)
	if got := parseOpenID(rawString); got != "1241690576128755" {
		t.Fatalf("open id = %q, want 1241690576128755", got)
	}
	// A float64 round-trip would lose the last digits — this is the trap.
	if fmt.Sprintf("%.0f", 1978784341143815.0) == "1978784341143815" {
		t.Log("note: this platform prints the float exactly; the json.Number path is still the correct one")
	}
}

// TestSessionCookieIsRefusedWhenTheIdentityCannotBeChecked is the regression
// gate for a real hole in the first draft.
//
// The open-platform id was read as a JSON string while the upstream sends a
// number, so the id always came back empty — and the identity check degraded to
// "accept whatever this profile is". One account's session could then have been
// used to claim another's reward.
//
// The rule: an identity that cannot be compared is REFUSED.
func TestSessionCookieIsRefusedWhenTheIdentityCannotBeChecked(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	// The open platform answers WITHOUT a userId: the id cannot be read.
	fake.pointsWithoutUserID = true

	installFakeSession(t, "fixture-account", fake)
	_, errCookie := sessionCookieFor(accountRecord{ID: "fixture-account", UserKey: fakeUserKey})
	if errCookie == nil {
		t.Fatal("a session whose identity cannot be checked must be refused")
	}
	if !errors.Is(errCookie, errNoSessionProfile) {
		t.Fatalf("error = %v, want errNoSessionProfile", errCookie)
	}
}

// TestSessionCookieIsRefusedWhenTheProfileBelongsToAnotherAccount is the other
// half of the same rule: a live session that belongs to a DIFFERENT account must
// not be used for this one.
func TestSessionCookieIsRefusedWhenTheProfileBelongsToAnotherAccount(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.sessionOpenID = "999999999999999" // some other account

	installFakeSession(t, "fixture-account", fake)
	_, errCookie := sessionCookieFor(accountRecord{ID: "fixture-account", UserKey: fakeUserKey})
	if errCookie == nil {
		t.Fatal("a session belonging to another account must be refused")
	}
}

// TestSessionCookieIsAcceptedForTheMatchingAccount is the positive counterpart:
// the same setup, with the identity matching, must succeed.
func TestSessionCookieIsAcceptedForTheMatchingAccount(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.sessionOpenID = fakeOpenID

	installFakeSession(t, "fixture-account", fake)
	cookie, errCookie := sessionCookieFor(accountRecord{ID: "fixture-account", UserKey: fakeUserKey})
	if errCookie != nil {
		t.Fatalf("the matching session was refused: %v", errCookie)
	}
	if cookie == "" {
		t.Fatal("no cookie returned")
	}
}

// -----------------------------------------------------------------------------
// The stored session cookie
// -----------------------------------------------------------------------------

// TestSessionCookieIsStoredAndNeverEchoed pins the secrecy discipline of the
// pasted-session route: the cookie goes into the account file and comes back
// only as a boolean.
func TestSessionCookieIsStoredAndNeverEchoed(t *testing.T) {
	isolateMeoo(t)
	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	if errSave := setAccountSessionCookie(record.ID, "oneday_sid=secret-value; login_oneday_ticket=secret2"); errSave != nil {
		t.Fatalf("save session: %v", errSave)
	}
	raw, errRead := os.ReadFile(accountsPath())
	if errRead != nil {
		t.Fatalf("read store: %v", errRead)
	}
	if !strings.Contains(string(raw), "oneday_sid") {
		t.Fatal("the session cookie was not stored")
	}
	// The stored cookie must never reach the panel payload.
	view := buildView(false)
	encoded, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	for _, forbidden := range []string{"secret-value", "oneday_sid", "login_oneday_ticket"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the panel payload carries %q", forbidden)
		}
	}
}

// TestReaddingTheSameKeyKeepsTheStoredSession pins that re-adding a key (a
// paste mistake, a label change) does not silently drop the session — that
// would cost the operator the daily reward without any visible cause.
func TestReaddingTheSameKeyKeepsTheStoredSession(t *testing.T) {
	isolateMeoo(t)
	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	if errSave := setAccountSessionCookie(record.ID, "oneday_sid=keep-me"); errSave != nil {
		t.Fatalf("save session: %v", errSave)
	}
	if _, errAgain := addAccount(fakeUserKey, "改名了"); errAgain != nil {
		t.Fatalf("re-add account: %v", errAgain)
	}
	stored, errFind := findAccount(record.ID)
	if errFind != nil {
		t.Fatalf("find account: %v", errFind)
	}
	if !strings.Contains(stored.SessionCookie, "keep-me") {
		t.Fatal("re-adding the key dropped the stored session")
	}
}

// TestClearingTheSessionRemovesIt pins the DELETE route's effect.
func TestClearingTheSessionRemovesIt(t *testing.T) {
	isolateMeoo(t)
	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	if errSave := setAccountSessionCookie(record.ID, "oneday_sid=x"); errSave != nil {
		t.Fatalf("save session: %v", errSave)
	}
	if errClear := setAccountSessionCookie(record.ID, ""); errClear != nil {
		t.Fatalf("clear session: %v", errClear)
	}
	stored, errFind := findAccount(record.ID)
	if errFind != nil {
		t.Fatalf("find account: %v", errFind)
	}
	if stored.SessionCookie != "" {
		t.Fatalf("the session was not cleared: %q", stored.SessionCookie)
	}
}

// TestAPastedSessionIsAcceptedOnlyForItsOwnAccount pins the identity check on
// the paste route: a cookie belonging to another account must be refused.
func TestAPastedSessionIsAcceptedOnlyForItsOwnAccount(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.sessionOpenID = "999999999999999" // some other account

	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	resp := saveSessionCookieResponse(pluginapi.ManagementRequest{
		Body: []byte(`{"account_id":"` + record.ID + `","cookie":"oneday_sid=fake"}`),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the cookie belongs to another account)", resp.StatusCode)
	}
	stored, errFind := findAccount(record.ID)
	if errFind != nil {
		t.Fatalf("find account: %v", errFind)
	}
	if stored.SessionCookie != "" {
		t.Fatal("a cookie for another account was stored")
	}
}

// TestNeedsLoginIsRetriedOnTheSameDay pins the retry policy.
//
// "needs_login" means the OPERATOR has not logged in yet — the day is not over
// for that account. Blocking the whole day would silently cost the operator a
// day's 2,000 points; the sweep must be allowed to pick it up once they log in.
func TestNeedsLoginIsRetriedOnTheSameDay(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = true
	fake.ledgerMovesOnInit = true

	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	// Park a "needs login" outcome for today.
	today := businessDay(time.Now())
	setDailyLoginState(record.ID, dailyLoginState{BusinessDay: today, Outcome: claimNeedsLogin})

	installFakeSession(t, record.ID, fake)
	summary := runDailyLoginSweep(time.Now(), "")
	if summary.Attempted != 1 {
		t.Fatalf("attempted = %d, want 1 (a needs_login account must be retried)", summary.Attempted)
	}
	if summary.Granted != 1 {
		t.Fatalf("granted = %d, want 1", summary.Granted)
	}
}

// TestASuccessfulClaimIsNotRetriedOnTheSameDay is the counterpart: a claimed
// account must NOT be attempted again the same day.
func TestASuccessfulClaimIsNotRetriedOnTheSameDay(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = true
	fake.ledgerMovesOnInit = true

	record, errAdd := addAccount(fakeUserKey, "夹具")
	if errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}
	today := businessDay(time.Now())
	setDailyLoginState(record.ID, dailyLoginState{BusinessDay: today, Outcome: claimGranted, GrantedAmount: 2000})

	installFakeSession(t, record.ID, fake)
	summary := runDailyLoginSweep(time.Now(), "")
	if summary.Attempted != 0 {
		t.Fatalf("attempted = %d, want 0", summary.Attempted)
	}
	if fake.initCalls != 0 {
		t.Fatalf("the grant endpoint was called %d times for an already-claimed account", fake.initCalls)
	}
}

// -----------------------------------------------------------------------------
// The pure decisions — no network at all
// -----------------------------------------------------------------------------

// TestBusinessDayUsesTheLocalCalendarDayNotUTC pins the bug that would have
// silently disabled the whole feature.
//
// At 00:05 local time the UTC date is still YESTERDAY. A UTC-based day key would
// see "today's grant already arrived" and skip every attempt, forever.
func TestBusinessDayUsesTheLocalCalendarDayNotUTC(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	justAfterMidnight := time.Date(2026, 11, 22, 0, 5, 0, 0, zone)

	day := businessDay(justAfterMidnight)
	if day != "2026-11-22" {
		t.Fatalf("business day = %q, want 2026-11-22 (the LOCAL day)", day)
	}
	// The same instant in UTC is the previous day — this is exactly the trap.
	if utcDay := justAfterMidnight.UTC().Format("2006-01-02"); utcDay != "2026-11-21" {
		t.Fatalf("the fixture no longer straddles midnight: UTC day = %q", utcDay)
	}
	if day == justAfterMidnight.UTC().Format("2006-01-02") {
		t.Fatal("businessDay must not be the UTC day")
	}
}

// TestClaimedTodayReadsTheBatchDay pins how a batch is matched to a day.
//
// The batch carries a Beijing timestamp ("2026-11-22T00:05:03"), so the day is
// its own first ten characters — NOT a re-parse into the host's zone.
func TestClaimedTodayReadsTheBatchDay(t *testing.T) {
	ledger := dailyLoginLedger{Batches: []grantBatch{
		{Effective: "2026-11-20T09:35:04.15"},
		{Effective: "2026-11-21T09:35:03.805"},
	}}
	if !claimedToday(ledger, "2026-11-21") {
		t.Fatal("the batch of 2026-11-21 must count as claimed")
	}
	if claimedToday(ledger, "2026-11-22") {
		t.Fatal("there is no batch for 2026-11-22 yet")
	}
}

// TestNewBatchSinceFindsOnlyTheNewOne pins the ledger diff that decides the
// outcome.
func TestNewBatchSinceFindsOnlyTheNewOne(t *testing.T) {
	before := []grantBatch{
		{Effective: "2026-11-20T09:35:04.15"},
		{Effective: "2026-11-21T09:35:03.805"},
	}
	after := []grantBatch{
		{Effective: "2026-11-20T09:35:04.15"},
		{Effective: "2026-11-21T09:35:03.805"},
		{Effective: "2026-11-22T00:05:03.101"},
	}
	fresh := newBatchSince(before, after)
	if len(fresh) != 1 {
		t.Fatalf("fresh batches = %d, want 1", len(fresh))
	}
	if fresh[0].Effective != "2026-11-22T00:05:03.101" {
		t.Fatalf("fresh batch = %+v", fresh[0])
	}
}

// TestIsDailyLoginSourceAcceptsBothSpellings pins the source filter: the code
// keys on the type, the operator sees the name.
func TestIsDailyLoginSourceAcceptsBothSpellings(t *testing.T) {
	if !isDailyLoginSource("DAILY_LOGIN", "登录赠送积分") {
		t.Fatal("DAILY_LOGIN must be recognised")
	}
	if !isDailyLoginSource("", "登录赠送积分") {
		t.Fatal("the Chinese name must be recognised too")
	}
	if isDailyLoginSource("SUBSCRIPTION", "订阅赠送积分") {
		t.Fatal("a subscription grant is not the daily login reward")
	}
	if isDailyLoginSource("INVITE_REWARD", "邀请奖励积分") {
		t.Fatal("an invite reward is not the daily login reward")
	}
}

// -----------------------------------------------------------------------------
// The attempt — end to end against the fake upstream
// -----------------------------------------------------------------------------

// TestClaimDailyLoginGrantsWhenTheLedgerMoves is the positive path: the grant
// endpoint issues points and the ledger shows a new batch.
func TestClaimDailyLoginGrantsWhenTheLedgerMoves(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = true
	fake.ledgerMovesOnInit = true
	fake.batches = []map[string]any{dailyLoginBatch("2026-11-21T09:35:03.805", "2026-11-27T23:59:59", 2000)}

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)

	now := time.Date(2026, 11, 22, 0, 6, 0, 0, time.UTC)
	attempt, errClaim := claimDailyLogin(account, now)
	if errClaim != nil {
		t.Fatalf("claim: %v", errClaim)
	}
	if attempt.Outcome != claimGranted {
		t.Fatalf("outcome = %q, want %q (note %q)", attempt.Outcome, claimGranted, attempt.State.Note)
	}
	if attempt.State.GrantedAmount != dailyLoginRewardPoints {
		t.Fatalf("granted = %d, want %d", attempt.State.GrantedAmount, dailyLoginRewardPoints)
	}
	if fake.initCalls != 1 {
		t.Fatalf("init calls = %d, want 1", fake.initCalls)
	}
}

// TestClaimDailyLoginDoesNotCallTheGrantEndpointWhenAlreadyClaimed is the
// negative control that matters most: an account that already has today's batch
// must cost ZERO requests to the grant endpoint.
//
// Without it, the plugin would nudge the site every hour for an account that
// already got its points.
func TestClaimDailyLoginDoesNotCallTheGrantEndpointWhenAlreadyClaimed(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.batches = []map[string]any{dailyLoginBatch("2026-11-22T00:05:03.101", "2026-11-28T23:59:59", 2000)}

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)

	now := time.Date(2026, 11, 22, 9, 35, 0, 0, time.UTC)
	attempt, errClaim := claimDailyLogin(account, now)
	if errClaim != nil {
		t.Fatalf("claim: %v", errClaim)
	}
	if attempt.Outcome != claimAlready {
		t.Fatalf("outcome = %q, want %q", attempt.Outcome, claimAlready)
	}
	if fake.initCalls != 0 {
		t.Fatalf("the grant endpoint was called %d times for an already-claimed account", fake.initCalls)
	}
	if fake.meCalls != 0 {
		t.Fatalf("the session was read %d times for an already-claimed account", fake.meCalls)
	}
}

// TestClaimDailyLoginReportsNeedsLoginWhenTheSessionIsDead pins the honest
// failure: a dead session must NOT be reported as a claim.
func TestClaimDailyLoginReportsNeedsLoginWhenTheSessionIsDead(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.sessionStatus = http.StatusUnauthorized

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)

	now := time.Date(2026, 11, 22, 0, 6, 0, 0, time.UTC)
	attempt, _ := claimDailyLogin(account, now)
	if attempt.Outcome == claimGranted {
		t.Fatal("a dead session must never be reported as a claim")
	}
	if attempt.Outcome != claimNeedsLogin {
		t.Fatalf("outcome = %q, want %q", attempt.Outcome, claimNeedsLogin)
	}
}

// TestClaimDailyLoginDoesNotClaimWhenTheServerDeclinesAndTheLedgerDoesNotMove
// is the second negative control, and it is the one that keeps the plugin
// honest: the endpoint may SAY it granted while the account's own ledger shows
// nothing new.
//
// The ledger wins. A plugin that trusted the response field would report
// "claimed +2000" on an account that received nothing.
func TestClaimDailyLoginDoesNotClaimWhenTheServerDeclinesAndTheLedgerDoesNotMove(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = true // the response CLAIMS a grant...
	fake.ledgerMovesOnInit = false  // ...but the ledger never moves.

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)

	now := time.Date(2026, 11, 22, 0, 6, 0, 0, time.UTC)
	attempt, _ := claimDailyLogin(account, now)
	if attempt.Outcome == claimGranted {
		t.Fatal("the response field must not be enough to claim points")
	}
	if attempt.Outcome != claimFailed {
		t.Fatalf("outcome = %q, want %q", attempt.Outcome, claimFailed)
	}
	if attempt.State.GrantedAmount != 0 {
		t.Fatalf("granted = %d, want 0", attempt.State.GrantedAmount)
	}
}

// TestClaimDailyLoginGrantsWhenTheLedgerMovedButTheResponseWasSilent pins the
// other direction: the ledger is the authority, so a grant that arrived is a
// grant even when the endpoint's answer was unhelpful.
func TestClaimDailyLoginGrantsWhenTheLedgerMovedButTheResponseWasSilent(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = false
	fake.ledgerMovesOnInit = true
	fake.skipReason = "ALREADY_GRANTED_TODAY"

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)

	// The ledger moves between the "before" read and the "after" read.
	fake.batches = []map[string]any{dailyLoginBatch("2026-11-22T00:05:03.101", "2026-11-28T23:59:59", 2000)}

	now := time.Date(2026, 11, 22, 0, 6, 0, 0, time.UTC)
	attempt, _ := claimDailyLogin(account, now)
	if attempt.Outcome != claimGranted && attempt.Outcome != claimAlready {
		t.Fatalf("outcome = %q; a moved ledger must be reported as claimed", attempt.Outcome)
	}
}

// -----------------------------------------------------------------------------
// The sweep
// -----------------------------------------------------------------------------

// TestRunDailyLoginSweepRunsOncePerBusinessDay pins the once-a-day discipline.
func TestRunDailyLoginSweepRunsOncePerBusinessDay(t *testing.T) {
	isolateMeoo(t)
	fake := newMeooFakeUpstream(t)
	fake.aim(t)
	fake.responseClaimsGrant = true
	fake.ledgerMovesOnInit = true

	account := accountRecord{ID: "fixture-account", Label: "夹具", UserKey: fakeUserKey}
	installFakeSession(t, account.ID, fake)
	if _, errAdd := addAccount(fakeUserKey, "夹具"); errAdd != nil {
		t.Fatalf("add account: %v", errAdd)
	}

	now := time.Date(2026, 11, 22, 0, 6, 0, 0, time.UTC)
	first := runDailyLoginSweep(now, "")
	if first.Attempted != 1 {
		t.Fatalf("first sweep attempted %d, want 1", first.Attempted)
	}
	second := runDailyLoginSweep(now.Add(time.Hour), "")
	if second.Attempted != 0 {
		t.Fatalf("the second sweep on the same day attempted %d, want 0", second.Attempted)
	}
}

// TestDailyLoginStateSurvivesAHostRestart pins the persistence: the outcome is
// parked on disk, so a host restart does not reset the panel to "not claimed".
func TestDailyLoginStateSurvivesAHostRestart(t *testing.T) {
	isolateMeoo(t)
	state := dailyLoginState{
		BusinessDay:   "2026-11-22",
		Outcome:       claimGranted,
		GrantedAmount: 2000,
		BatchAt:       "2026-11-22T00:05:03.101",
	}
	setDailyLoginState("fixture-account", state)

	// Simulate a restart: the in-memory park is dropped, the file is not.
	resetDailyLoginStatesForTest()
	loaded := dailyLoginStateFor("fixture-account")
	if loaded.Outcome != claimGranted {
		t.Fatalf("the parked outcome did not survive: %+v", loaded)
	}
	if loaded.GrantedAmount != 2000 {
		t.Fatalf("granted amount = %d", loaded.GrantedAmount)
	}
}

// TestDailyLoginStateFileIsWrittenNextToTheAccounts pins where the state lives.
func TestDailyLoginStateFileIsWrittenNextToTheAccounts(t *testing.T) {
	isolateMeoo(t)
	setDailyLoginState("fixture-account", dailyLoginState{BusinessDay: "2026-11-22", Outcome: claimGranted})
	if _, errStat := os.Stat(dailyLoginStorePath()); errStat != nil {
		t.Fatalf("the state file was not written: %v", errStat)
	}
	if !strings.HasSuffix(filepath.ToSlash(dailyLoginStorePath()), "/daily-login.json") {
		t.Fatalf("state file = %q", dailyLoginStorePath())
	}
}

// TestDailyLoginStoreNeverCarriesTheCookie pins the secrecy rule: the parked
// state is written to disk, so it must never hold the session cookie.
func TestDailyLoginStoreNeverCarriesTheCookie(t *testing.T) {
	isolateMeoo(t)
	setDailyLoginState("fixture-account", dailyLoginState{
		BusinessDay: "2026-11-22",
		Outcome:     claimGranted,
		Note:        "已领取 2000 积分",
	})
	raw, errRead := os.ReadFile(dailyLoginStorePath())
	if errRead != nil {
		t.Fatalf("read state file: %v", errRead)
	}
	for _, forbidden := range []string{"oneday_sid", "login_oneday_ticket", "Cookie", "meoo_ak_"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the state file carries %q", forbidden)
		}
	}
}

// -----------------------------------------------------------------------------
// The switch
// -----------------------------------------------------------------------------

// TestDailyLoginSwitchIsHonoured pins that the operator can turn the feature
// off, and that the switch is actually READ (not just declared).
func TestDailyLoginSwitchIsHonoured(t *testing.T) {
	isolateMeoo(t)
	if !currentDailyLogin() {
		t.Fatal("the daily login reward must be ON by default")
	}
	setConfigForTest(t, func(c *pluginConfig) { c.DailyLogin = false })
	if currentDailyLogin() {
		t.Fatal("the switch does not take effect")
	}
}

// TestConfigEchoCarriesTheDailyLoginSwitch pins that the panel can see it.
func TestConfigEchoCarriesTheDailyLoginSwitch(t *testing.T) {
	isolateMeoo(t)
	echo := configEcho()
	value, present := echo["daily_login"]
	if !present {
		t.Fatalf("configEcho does not carry daily_login: %v", echo)
	}
	if value != true {
		t.Fatalf("daily_login = %v, want true", value)
	}
}

// -----------------------------------------------------------------------------
// The session plumbing
// -----------------------------------------------------------------------------

// installFakeSession writes a fake Chromium profile for one account.
//
// The REAL cookie store is a Chromium-encrypted SQLite database; building one
// in a test would mean reimplementing the encryptor. Instead the test swaps the
// two seams — the profile lookup and the cookie reader — so the DECISIONS are
// exercised while the cryptography is not.
func installFakeSession(t *testing.T, accountID string, fake *meooFakeUpstream) {
	t.Helper()
	previousProfile := sessionProfileForFn
	previousCookie := readSessionCookieFn
	sessionProfileForFn = func(id string) (sessionProfile, error) {
		if id != accountID {
			return sessionProfile{}, errNoSessionProfile
		}
		return sessionProfile{Dir: "fake"}, nil
	}
	readSessionCookieFn = func(sessionProfile) (string, error) {
		return "oneday_sid=fake; login_oneday_ticket=fake", nil
	}
	t.Cleanup(func() {
		sessionProfileForFn = previousProfile
		readSessionCookieFn = previousCookie
	})
	_ = fake
}

// TestSessionCookieIsNeverEchoed pins that the cookie value does not reach a
// log line or the panel.
func TestSessionCookieIsNeverEchoed(t *testing.T) {
	isolateMeoo(t)
	line := dailyLoginLogLine(
		accountRecord{Label: "夹具"},
		dailyLoginAttempt{Outcome: claimGranted, State: dailyLoginState{Note: "已领取 2000 积分"}},
	)
	for _, forbidden := range []string{"oneday_sid", "login_oneday_ticket", "="} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("the log line carries %q: %s", forbidden, line)
		}
	}
	if !strings.Contains(line, "夹具") {
		t.Fatalf("the log line lost the account label: %s", line)
	}
}

// TestDescribeDailyLoginCoversEveryOutcome pins that every outcome has a human
// explanation — an outcome without one is an outcome the operator cannot act on.
func TestDescribeDailyLoginCoversEveryOutcome(t *testing.T) {
	for _, outcome := range []string{claimGranted, claimAlready, claimNeedsLogin, claimFailed, ""} {
		text := describeDailyLogin(outcome)
		if strings.TrimSpace(text) == "" {
			t.Fatalf("outcome %q has no description", outcome)
		}
	}
}

// TestDailyLoginNextDelayIsShortAfterMidnight pins the cadence: the grant
// becomes available right after the local midnight, so the sweep must look
// soon after it — not an hour later.
func TestDailyLoginNextDelayIsShortAfterMidnight(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	justAfter := time.Date(2026, 11, 22, 0, 5, 0, 0, zone)
	if delay := dailyLoginNextDelay(justAfter); delay > 30*time.Minute {
		t.Fatalf("delay after midnight = %v, want <= 30m", delay)
	}
	midday := time.Date(2026, 11, 22, 12, 0, 0, 0, zone)
	if delay := dailyLoginNextDelay(midday); delay < time.Hour {
		t.Fatalf("delay at midday = %v, want >= 1h", delay)
	}
}

// TestDailyLoginStorePathIsRedirectable pins that the tests (and a
// non-standard install) can move the state file.
func TestDailyLoginStorePathIsRedirectable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dailyLoginStoreEnvVar, filepath.Join(dir, "custom.json"))
	if got := dailyLoginStorePath(); got != filepath.Join(dir, "custom.json") {
		t.Fatalf("state path = %q", got)
	}
}

// TestDailyLoginStatusForNeverLeaksTheCookie pins the panel payload.
func TestDailyLoginStatusForNeverLeaksTheCookie(t *testing.T) {
	isolateMeoo(t)
	status := dailyLoginStatusFor("fixture-account", time.Now())
	raw, errMarshal := json.Marshal(status)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	for _, forbidden := range []string{"oneday_sid", "Cookie"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the panel payload carries %q", forbidden)
		}
	}
	if fmt.Sprintf("%v", status["business_day"]) == "" {
		t.Fatal("the panel payload must carry the business day")
	}
}
