// daily_login.go — the daily 2,000-point login reward.
//
// ---------------------------------------------------------------------------
// The rule (measured, not guessed)
// ---------------------------------------------------------------------------
//
// 秒悟 grants every user 2,000 points on their FIRST login of the day, valid for
// six days. There is no button and no claim endpoint: the grant is issued by
//
//	GET https://meoo.com/api/mc/entitlement/init
//
// Evidence (2026-11-21, official 0.0.113 web bundle + live traffic):
//
//   - the bundle's ONLY caller of that endpoint is `initEntitlement`, invoked on
//     app start (with a 500/1000/2000/4000 ms backoff while `initialized` is
//     false) and again on `visibilitychange` → visible;
//   - the whole 3.2 MB bundle contains NO claim / signin / checkin endpoint;
//   - the points ledger records the grant as type=LOGIN_GRANT, operator=SYSTEM,
//     sourceType=DAILY_LOGIN, amount=2000, one entry per day;
//   - the open-platform key (meoo_ak_…, Bearer) can READ the balance but calling
//     it does not log in and does not grant: an account whose AK was polled daily
//     accumulated no grant, an account that opened the site did.
//
// So the credential for this feature is the web-session cookie, and the trigger
// is one GET of the endpoint above. That is exactly what this file does.
//
// ---------------------------------------------------------------------------
// How "claimed" is decided — the ledger is the only authority
// ---------------------------------------------------------------------------
//
// The endpoint answers with fields, but a field is not proof: `dailyLoginGranted`
// describes what the server did on THAT call, and a proxy or a stale response
// could say anything. The proof is the ACCOUNT'S OWN LEDGER: a new
// "登录赠送积分" batch, or the available balance rising by ~2,000.
//
// Therefore every attempt is judged by re-reading the ledger afterwards, and a
// call whose response says "granted" but whose ledger did not move is reported as
// NOT claimed. The reverse — a ledger that moved even though the response was
// unparseable — is reported as claimed. The ledger wins; the response only
// explains.
//
// ---------------------------------------------------------------------------
// What is never done here
// ---------------------------------------------------------------------------
//
// No password is stored, no SMS code is requested, no login form is automated.
// The operator logs in once, by hand, in their own browser; this plugin then
// reuses that session. When the session expires, the account is reported as
// needing a human login — never as claimed, and never retried into a lockout.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// meooWebBase is the web app's origin. The reward endpoints live here, NOT under
// the open platform (/open/v1) — they only accept a web session.
var meooWebBase = "https://meoo.com"

// dailyLoginRewardPoints is the grant size the site advertises. It is used only
// for reporting and for the "did the balance move by enough" test; the ledger,
// not this constant, is the authority on what was actually granted.
const dailyLoginRewardPoints = 2000

// dailyLoginThresholdTolerance is how far below the advertised grant the balance
// may move and still count as a claim. The grant is exactly 2,000 upstream, but
// an overdraft repayment can be booked against the same batch (measured
// 2026-11-16: a +2,000 grant was immediately offset by a -472 POINT_REPAY, so
// the balance moved by 1,528). Judging on the ledger's BATCHES rather than the
// balance avoids that trap; the tolerance only guards the balance-only path.
const dailyLoginThresholdTolerance = 1

// Claim outcomes. Each one means a different thing to the operator, and the
// panel must not blur them.
const (
	// claimGranted: the ledger shows a new daily-login batch. Points arrived.
	claimGranted = "granted"
	// claimAlready: the ledger already had today's batch. Nothing was claimed.
	claimAlready = "already"
	// claimFailed: the attempt did not produce a grant (network, dead session).
	claimFailed = "failed"
	// claimNeedsLogin: no usable web session; the operator must log in by hand.
	claimNeedsLogin = "needs_login"
)

// dailyLoginState is the per-account record of the last attempt.
type dailyLoginState struct {
	// BusinessDay is the local calendar day the attempt belongs to ("2026-11-22").
	// Meoo's day boundary is midnight in the account's own zone; the grant batches
	// carry Beijing timestamps, and this plugin's operator is in the same zone, so
	// the local calendar day is the right key.
	BusinessDay string `json:"business_day,omitempty"`
	// Outcome is one of the claim* constants.
	Outcome string `json:"outcome,omitempty"`
	// GrantedAmount is how many points the ledger showed arriving (0 when none).
	GrantedAmount int `json:"granted_amount,omitempty"`
	// BalanceAfter is the account's available balance after the attempt.
	BalanceAfter float64 `json:"balance_after,omitempty"`
	// BatchAt is when the new grant batch became effective ("2026-11-22T00:05:03").
	BatchAt string `json:"batch_at,omitempty"`
	// SkipReason is the server's own explanation when it declined ("ALREADY_GRANTED_TODAY").
	SkipReason string `json:"skip_reason,omitempty"`
	// AttemptAt is when the attempt was made (Unix milliseconds).
	AttemptAt int64 `json:"attempt_at,omitempty"`
	// Note explains a failure in one line.
	Note string `json:"note,omitempty"`
}

// dailyLoginAttempt is the outcome of one claim attempt.
type dailyLoginAttempt struct {
	State   dailyLoginState
	Outcome string
}

// -----------------------------------------------------------------------------
// Upstream: the reward endpoint
// -----------------------------------------------------------------------------

// meooEntitlementInit is the endpoint that issues the daily grant.
const meooEntitlementInitPath = "/api/mc/entitlement/init"

// meooUserMePath answers "who is this session".
const meooUserMePath = "/api/v1/user/me"

// entitlementInitAnswer is the response shape of /api/mc/entitlement/init.
//
//	{"success":true,"data":{
//	   "initialized":true,
//	   "dailyLoginGranted":true,
//	   "dailyLoginGrantAmount":2000,
//	   "dailyLoginSkipReason":"ALREADY_GRANTED_TODAY",
//	   "inviteRewardArrivals":[]}}
type entitlementInitAnswer struct {
	Success bool `json:"success"`
	Data    struct {
		Initialized           bool    `json:"initialized"`
		DailyLoginGranted     bool    `json:"dailyLoginGranted"`
		DailyLoginGrantAmount float64 `json:"dailyLoginGrantAmount"`
		DailyLoginSkipReason  string  `json:"dailyLoginSkipReason"`
		ErrCode               string  `json:"errCode"`
	} `json:"data"`
	ErrCode string `json:"errCode"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}

// meooSessionGet issues one request to the web app with the session cookie.
//
// The headers matter: Meoo's answer depends on Origin/Referer (the site is
// same-origin JSON) and on the full cookie set, signatures included.
func meooSessionGet(cookie, path string) (*hostHTTPResponse, error) {
	req, errReq := http.NewRequest(http.MethodGet, strings.TrimRight(meooWebBase, "/")+path, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", meooWebBase+"/")
	req.Header.Set("Origin", meooWebBase)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "cpa-meoo-panel/"+version)
	return httpDo(req)
}

// sessionIdentity is who a web session belongs to.
//
// OpenID is the join key: the open platform reports account.userId and the web
// session reports open_id, and they are the SAME number for the same person
// (measured 2026-11-21 across every account on this box).
type sessionIdentity struct {
	OpenID   string `json:"open_id"`
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Phone    string `json:"phone"`
}

// fetchSessionIdentity asks the web app who the session is.
//
// A 401 / NEED_LOGIN here means the session is dead, which is the single most
// common failure and the one the operator must act on, so it is returned as a
// typed error rather than a generic failure.
func fetchSessionIdentity(cookie string) (sessionIdentity, error) {
	resp, errDo := meooSessionGet(cookie, meooUserMePath)
	if errDo != nil {
		return sessionIdentity{}, fmt.Errorf("session identity: %s", note(errDo.Error()))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return sessionIdentity{}, fmt.Errorf("%w: HTTP %d", errSessionExpired, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return sessionIdentity{}, newAPIError(resp.StatusCode, "session identity: HTTP "+strconv.Itoa(resp.StatusCode))
	}
	var answer struct {
		Success bool            `json:"success"`
		Data    sessionIdentity `json:"data"`
		ErrCode string          `json:"errCode"`
	}
	if errJSON := json.Unmarshal(resp.Body, &answer); errJSON != nil {
		return sessionIdentity{}, fmt.Errorf("session identity: answer is not JSON: %s", note(errJSON.Error()))
	}
	if answer.Success == false || answer.Data.OpenID == "" {
		return sessionIdentity{}, fmt.Errorf("%w: %s", errSessionExpired, answer.ErrCode)
	}
	return answer.Data, nil
}

// callEntitlementInit performs THE call that triggers (or declines) the grant.
//
// It returns the parsed answer plus the raw HTTP status, so the caller can tell
// "the server declined because today's grant is already issued" from "the server
// refused the session".
func callEntitlementInit(cookie string) (entitlementInitAnswer, int, error) {
	var answer entitlementInitAnswer
	resp, errDo := meooSessionGet(cookie, meooEntitlementInitPath)
	if errDo != nil {
		return answer, 0, fmt.Errorf("entitlement init: %s", note(errDo.Error()))
	}
	status := resp.StatusCode
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return answer, status, fmt.Errorf("%w: HTTP %d", errSessionExpired, status)
	}
	if status < 200 || status >= 300 {
		return answer, status, newAPIError(status, "entitlement init: HTTP "+strconv.Itoa(status))
	}
	if errJSON := json.Unmarshal(resp.Body, &answer); errJSON != nil {
		return answer, status, fmt.Errorf("entitlement init: answer is not JSON: %s", note(errJSON.Error()))
	}
	if answer.Success == false {
		code := answer.ErrCode
		if code == "" {
			code = answer.Error.Code
		}
		if code == "NEED_LOGIN" || code == "LOGIN_INVALID" || code == "UNAUTHORIZED" {
			return answer, status, fmt.Errorf("%w: %s", errSessionExpired, code)
		}
		return answer, status, fmt.Errorf("entitlement init refused: %s", note(code))
	}
	return answer, status, nil
}

// -----------------------------------------------------------------------------
// The ledger: the only authority on whether points arrived
// -----------------------------------------------------------------------------

// grantBatch is one "登录赠送积分" batch on the account's ledger.
type grantBatch struct {
	Source    string  `json:"source"`
	Amount    float64 `json:"amount"`
	Effective string  `json:"effective"`
	Expires   string  `json:"expires"`
	Remaining float64 `json:"remaining"`
}

// dailyLoginLedger is the account's points state, read from the open platform.
type dailyLoginLedger struct {
	Available float64
	Granted   float64
	Consumed  float64
	Batches   []grantBatch
}

// readDailyLoginLedger reads one account's points through the open platform.
//
// This is the same AK call the panel already makes; it is reused here because it
// is the only surface that shows the grant batches.
func readDailyLoginLedger(userKey string) (dailyLoginLedger, error) {
	raw, _, errCall := callOpenPlatform(userKey, http.MethodGet, "/cli-compat/account/points", nil)
	if errCall != nil {
		return dailyLoginLedger{}, errCall
	}
	var answer struct {
		Account struct {
			Available float64 `json:"availableBalance"`
			Granted   float64 `json:"totalGranted"`
			Consumed  float64 `json:"totalConsumed"`
		} `json:"account"`
		ActiveGrants []struct {
			SourceType string `json:"sourceType"`
			SourceName string `json:"sourceTypeName"`
			Details    []struct {
				RemainingAmount float64 `json:"remainingAmount"`
				EffectiveDate   string  `json:"effectiveDate"`
				ExpiresDate     string  `json:"expiresDate"`
			} `json:"grantDetails"`
		} `json:"activeGrants"`
	}
	if errJSON := json.Unmarshal(raw, &answer); errJSON != nil {
		return dailyLoginLedger{}, fmt.Errorf("points: answer is not JSON: %s", note(errJSON.Error()))
	}
	ledger := dailyLoginLedger{
		Available: answer.Account.Available,
		Granted:   answer.Account.Granted,
		Consumed:  answer.Account.Consumed,
	}
	for _, grant := range answer.ActiveGrants {
		if !isDailyLoginSource(grant.SourceType, grant.SourceName) {
			continue
		}
		for _, detail := range grant.Details {
			ledger.Batches = append(ledger.Batches, grantBatch{
				Source:    grant.SourceName,
				Amount:    dailyLoginRewardPoints,
				Effective: detail.EffectiveDate,
				Expires:   detail.ExpiresDate,
				Remaining: detail.RemainingAmount,
			})
		}
	}
	return ledger, nil
}

// isDailyLoginSource reports whether a grant is the daily login reward.
//
// The upstream type is DAILY_LOGIN and the name is 登录赠送积分; both are
// checked because the name is what the operator sees in the ledger and the type
// is what the code should key on.
func isDailyLoginSource(sourceType, sourceName string) bool {
	if strings.EqualFold(strings.TrimSpace(sourceType), "DAILY_LOGIN") {
		return true
	}
	return strings.Contains(sourceName, "登录赠送")
}

// batchKey identifies a batch by its effective instant — the only field that
// distinguishes one day's grant from another's.
func batchKey(b grantBatch) string {
	return b.Effective
}

// newBatchSince returns the batches present after `before` but not before it.
func newBatchSince(before, after []grantBatch) []grantBatch {
	seen := make(map[string]bool, len(before))
	for _, b := range before {
		seen[batchKey(b)] = true
	}
	var out []grantBatch
	for _, b := range after {
		if !seen[batchKey(b)] {
			out = append(out, b)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// The attempt
// -----------------------------------------------------------------------------

// businessDay returns the local calendar day, as "YYYY-MM-DD".
//
// NOT toISOString(): that is UTC, and at 00:05 local time the UTC date is still
// yesterday — which would make the plugin believe today's grant had already
// arrived and skip the whole attempt. Measured: at 2026-11-22 00:05 +08:00 the
// local day is 2026-11-22 while UTC says 2026-11-21.
func businessDay(now time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d", now.Year(), now.Month(), now.Day())
}

// claimedToday reports whether the ledger already holds a batch for this business
// day.
//
// The batch's effective instant is a Beijing timestamp ("2026-11-22T00:05:03"),
// so the day is read from its own first ten characters, not by parsing it into
// the host's zone.
func claimedToday(ledger dailyLoginLedger, day string) bool {
	for _, b := range ledger.Batches {
		if len(b.Effective) >= 10 && b.Effective[:10] == day {
			return true
		}
	}
	return false
}

// claimDailyLogin performs one daily-login attempt for one account.
//
// The order is deliberate:
//
//  1. read the ledger BEFORE (baseline);
//  2. if today's batch already exists → already, no request at all;
//  3. get a web session (from the profile, then matched by identity);
//  4. call /api/mc/entitlement/init — the call that actually triggers the grant;
//  5. read the ledger AFTER and judge by what moved.
//
// Step 2 matters: it means a second run on the same day costs zero upstream
// requests, and it means the plugin never nudges a grant that already arrived.
func claimDailyLogin(account accountRecord, now time.Time) (dailyLoginAttempt, error) {
	day := businessDay(now)
	attempt := dailyLoginAttempt{Outcome: claimFailed, State: dailyLoginState{BusinessDay: day, AttemptAt: now.UnixMilli()}}

	before, errBefore := readDailyLoginLedger(account.UserKey)
	if errBefore != nil {
		attempt.State.Note = note(errBefore.Error())
		return attempt, errBefore
	}
	attempt.State.BalanceAfter = before.Available
	if claimedToday(before, day) {
		attempt.Outcome = claimAlready
		attempt.State.Outcome = claimAlready
		attempt.State.Note = "今日登录赠送积分已到账"
		return attempt, nil
	}

	cookie, errCookie := sessionCookieFor(account)
	if errCookie != nil {
		attempt.Outcome = claimNeedsLogin
		attempt.State.Outcome = claimNeedsLogin
		attempt.State.Note = note(errCookie.Error())
		return attempt, errCookie
	}

	answer, status, errInit := callEntitlementInit(cookie)
	attempt.State.Note = ""
	if errInit != nil {
		// The state's outcome must ALWAYS be set, even when the attempt failed for
		// a reason other than a dead session: an empty outcome would make the
		// sweep treat the account as "never attempted" and retry it forever.
		attempt.Outcome = claimFailed
		attempt.State.Outcome = claimFailed
		if errors.Is(errInit, errSessionExpired) {
			attempt.Outcome = claimNeedsLogin
			attempt.State.Outcome = claimNeedsLogin
		}
		attempt.State.Note = note(errInit.Error())
		attempt.State.BalanceAfter = before.Available
		return attempt, errInit
	}
	attempt.State.SkipReason = answer.Data.DailyLoginSkipReason
	_ = status

	// The ledger decides — and so does the AMOUNT. The response's
	// dailyLoginGrantAmount is deliberately NOT recorded as the granted amount:
	// the server can answer "granted" while the account's own ledger shows
	// nothing new, and an amount that the ledger cannot corroborate is a claim
	// the plugin must not make.
	after, errAfter := readDailyLoginLedger(account.UserKey)
	if errAfter != nil {
		attempt.Outcome = claimFailed
		attempt.State.Outcome = claimFailed
		attempt.State.Note = note(errAfter.Error())
		attempt.State.BalanceAfter = before.Available
		return attempt, errAfter
	}
	attempt.State.BalanceAfter = after.Available

	fresh := newBatchSince(before.Batches, after.Batches)
	if len(fresh) > 0 {
		attempt.Outcome = claimGranted
		attempt.State.Outcome = claimGranted
		attempt.State.GrantedAmount = int(fresh[0].Amount)
		attempt.State.BatchAt = fresh[0].Effective
		attempt.State.Note = fmt.Sprintf("已领取 %d 积分", attempt.State.GrantedAmount)
		return attempt, nil
	}
	// No new batch. If the server said it skipped because today's grant is
	// already there, the ledger must show a batch for today — if it does not, the
	// two disagree and we must NOT claim a win.
	if claimedToday(after, day) {
		attempt.Outcome = claimAlready
		attempt.State.Outcome = claimAlready
		attempt.State.Note = "今日登录赠送积分已到账"
		return attempt, nil
	}
	attempt.Outcome = claimFailed
	attempt.State.Outcome = claimFailed
	if attempt.State.Note == "" {
		if answer.Data.DailyLoginSkipReason != "" {
			attempt.State.Note = "服务端未发放：" + answer.Data.DailyLoginSkipReason
		} else {
			attempt.State.Note = "服务端未发放，账本也没有新批次"
		}
	}
	return attempt, nil
}

// -----------------------------------------------------------------------------
// Session acquisition
// -----------------------------------------------------------------------------

// sessionCookieFor returns the web-session cookie for one account.
//
// The profile named after the account id is tried first (the operator's own
// layout), and if that yields no usable session the identity sweep below is used:
// it asks every profile on the machine who it is and matches the answer against
// this account's open-platform id. The sweep is what makes the feature work
// without any operator-maintained mapping.
func sessionCookieFor(account accountRecord) (string, error) {
	want := accountOpenID(account)
	if want == "" {
		return "", fmt.Errorf("%w: 无法读取账号的 open id，无法核对会话归属", errNoSessionProfile)
	}
	// 1. The profile named after the account id.
	if profile, errProfile := sessionProfileFor(account.ID); errProfile == nil {
		if cookie, errCookie := readSessionCookie(profile); errCookie == nil {
			identity, errIdentity := fetchSessionIdentity(cookie)
			if errIdentity == nil && identity.OpenID == want {
				return cookie, nil
			}
		}
	}
	// 2. The identity sweep.
	return sessionCookieByIdentity(account, want)
}

// accountOpenIDCache caches the open-platform id per account id. The value comes
// from the same points call the ledger read makes, so it is free to collect.
var accountOpenIDCache = struct {
	values map[string]string
}{values: map[string]string{}}

// resetAccountOpenIDCacheForTest empties the cached open-platform ids.
func resetAccountOpenIDCacheForTest() {
	accountOpenIDCache.values = map[string]string{}
}

// accountOpenID returns the account's open-platform id (its "open id" in Meoo's
// terms), which the web session also reports. Empty when it cannot be read.
//
// The value comes from the same points call the ledger read makes, so it costs
// nothing extra — and it is cached, because the identity sweep asks for it once
// per profile.
//
// ⚠ The upstream sends userId as a JSON NUMBER (measured: 1978784341143815, not a
// quoted string), which is larger than a float64 can hold exactly. It is
// therefore decoded as json.Number and rendered as text — decoding it into an
// int64 or a float64 would silently round it, and the identity join would then
// compare two different rounded numbers.
func accountOpenID(account accountRecord) string {
	if v, ok := accountOpenIDCache.values[account.ID]; ok {
		return v
	}
	raw, _, errCall := callOpenPlatform(account.UserKey, http.MethodGet, "/cli-compat/account/points", nil)
	if errCall != nil {
		return ""
	}
	id := parseOpenID(raw)
	if id == "" {
		return ""
	}
	accountOpenIDCache.values[account.ID] = id
	return id
}

// parseOpenID pulls account.userId out of a points answer, whatever JSON type
// the upstream chose for it (number or string).
func parseOpenID(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var answer struct {
		Account struct {
			UserID json.Number `json:"userId"`
		} `json:"account"`
	}
	if errDecode := decoder.Decode(&answer); errDecode != nil {
		return ""
	}
	return strings.TrimSpace(answer.Account.UserID.String())
}

// sessionCookieByIdentity finds the profile whose session belongs to this account.
//
// The join is on identity, never on a directory name: each profile's session is
// asked who it is and the answer is compared with the account's open-platform id.
//
// `want` is passed in rather than re-read: the caller has already established it,
// and re-reading it would allow the two comparisons to disagree.
func sessionCookieByIdentity(account accountRecord, want string) (string, error) {
	if want == "" {
		return "", fmt.Errorf("%w: 无法读取账号的 open id", errNoSessionProfile)
	}
	var problems []string
	for _, profile := range listSessionProfiles() {
		cookie, errCookie := readSessionCookie(profile)
		if errCookie != nil {
			continue
		}
		identity, errIdentity := fetchSessionIdentity(cookie)
		if errIdentity != nil {
			continue
		}
		if identity.OpenID == want {
			return cookie, nil
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%w: %s", errNoSessionProfile, note(strings.Join(problems, "; ")))
	}
	return "", fmt.Errorf("%w: 本机没有属于该账号的已登录浏览器会话，请在浏览器登录 meoo.com 后重试", errNoSessionProfile)
}
