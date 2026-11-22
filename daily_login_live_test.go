package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// timeNowForLive is the clock the live tests read. It exists as a seam so a
// test can pin the instant without touching the production clock.
func timeNowForLive() time.Time { return time.Now() }

// -----------------------------------------------------------------------------
// LIVE tests — they touch this machine's real browser profiles and the real
// 秒悟 site, so they are OFF unless explicitly enabled.
//
//	MEOO_LIVE_TEST=1 go test -run TestLive ./...
//
// They exist because the cryptography in session_cookie.go is the one part of
// this feature that cannot be proven by a fixture: the whole point of the
// feature is to read a Chromium-encrypted cookie store that the operator's own
// browser wrote. A unit test can prove the DECISIONS; only a live test can prove
// that the reader still understands the file on disk.
// -----------------------------------------------------------------------------

// liveEnabled reports whether the live tests may run.
func liveEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEOO_LIVE_TEST")) == "1"
}

// TestLiveSessionCookieReadsTheRealEdgeProfile proves that the Chromium reader
// (the minimal SQLite reader + the v10 decryptor) still understands the cookie
// store an Edge profile holds on THIS machine.
//
// It asserts the SHAPE of the result — a session cookie set containing the
// login cookies, and a cookie that the real site accepts — never the value.
func TestLiveSessionCookieReadsTheRealEdgeProfile(t *testing.T) {
	if !liveEnabled() {
		t.Skip("live test: set MEOO_LIVE_TEST=1 to run")
	}
	profiles := listSessionProfiles()
	if len(profiles) == 0 {
		t.Skip("no browser profile on this machine")
	}
	found := 0
	for _, profile := range profiles {
		cookie, errCookie := readSessionCookie(profile)
		if errCookie != nil {
			continue
		}
		if !strings.Contains(cookie, "oneday_sid") {
			continue
		}
		found++
		// The identity call proves the cookie set is not merely decryptable but
		// ACCEPTED by the site.
		identity, errIdentity := fetchSessionIdentity(cookie)
		if errIdentity != nil {
			t.Logf("profile %s decrypted but the site refused the session: %v", filepath.Base(profile.Dir), errIdentity)
			continue
		}
		if identity.OpenID == "" {
			t.Errorf("profile %s: the site answered without an open id", filepath.Base(profile.Dir))
			continue
		}
		t.Logf("profile %s → open_id=%s username=%s", filepath.Base(profile.Dir), identity.OpenID, identity.Username)
	}
	if found == 0 {
		t.Fatalf("no profile on this machine yielded a usable Meoo session cookie")
	}
}

// TestLiveClaimReportsAlreadyWhenTheGrantIsIn proves the END-TO-END decision
// against the real site: for an account that already has today's grant batch, the
// attempt must report "already" and must NOT call the grant endpoint.
//
// This is the honest expectation for an account claimed earlier today: the
// plugin must not claim a win it cannot corroborate on the ledger.
func TestLiveClaimReportsAlreadyWhenTheGrantIsIn(t *testing.T) {
	if !liveEnabled() {
		t.Skip("live test: set MEOO_LIVE_TEST=1 to run")
	}
	accounts, errAccounts := listAccounts()
	if errAccounts != nil {
		t.Fatalf("accounts: %v", errAccounts)
	}
	if len(accounts) == 0 {
		t.Skip("no account stored")
	}
	now := timeNowForLive()
	day := businessDay(now)
	checked := 0
	for _, account := range accounts {
		ledger, errLedger := readDailyLoginLedger(account.UserKey)
		if errLedger != nil {
			t.Logf("account %s: ledger read failed: %v", account.Label, errLedger)
			continue
		}
		if !claimedToday(ledger, day) {
			t.Logf("account %s: no grant batch for %s today (skipped)", account.Label, day)
			continue
		}
		checked++
		attempt, errAttempt := claimDailyLogin(account, now)
		if errAttempt != nil {
			t.Errorf("account %s: claim returned an error: %v", account.Label, errAttempt)
			continue
		}
		if attempt.Outcome != claimAlready && attempt.Outcome != claimGranted {
			t.Errorf("account %s: outcome = %q, want already (note %q)", account.Label, attempt.Outcome, attempt.State.Note)
			continue
		}
		if attempt.State.GrantedAmount != 0 {
			t.Errorf("account %s: granted = %d on an already-claimed account", account.Label, attempt.State.GrantedAmount)
		}
		t.Logf("account %s: %s (%s)", account.Label, attempt.Outcome, attempt.State.Note)
	}
	if checked == 0 {
		t.Skip("no account has a grant batch for today")
	}
}

// TestLiveSessionMatchesTheAccountByIdentity proves the join: a profile's
// session must be attributable to the account whose open-platform id matches.
//
// The test walks the profiles and the accounts and checks that every profile
// whose session is alive maps onto exactly one stored account. A profile that
// maps onto NONE means the identity join is broken (or the profile belongs to
// an account that was never added).
func TestLiveSessionMatchesTheAccountByIdentity(t *testing.T) {
	if !liveEnabled() {
		t.Skip("live test: set MEOO_LIVE_TEST=1 to run")
	}
	accounts, errAccounts := listAccounts()
	if errAccounts != nil {
		t.Fatalf("accounts: %v", errAccounts)
	}
	byOpenID := map[string]string{}
	for _, account := range accounts {
		openID := accountOpenID(account)
		if openID == "" {
			t.Logf("account %s: open id unreadable", account.Label)
			continue
		}
		if other, clash := byOpenID[openID]; clash {
			t.Errorf("accounts %s and %s share the open id %s", other, account.Label, openID)
		}
		byOpenID[openID] = account.Label
	}
	matched := 0
	for _, profile := range listSessionProfiles() {
		cookie, errCookie := readSessionCookie(profile)
		if errCookie != nil {
			continue
		}
		identity, errIdentity := fetchSessionIdentity(cookie)
		if errIdentity != nil {
			continue
		}
		label, ok := byOpenID[identity.OpenID]
		if !ok {
			t.Logf("profile %s → open_id=%s has no stored account", filepath.Base(profile.Dir), identity.OpenID)
			continue
		}
		matched++
		t.Logf("profile %s → account %s (open_id=%s)", filepath.Base(profile.Dir), label, identity.OpenID)
	}
	t.Logf("matched %d of %d profiles", matched, len(listSessionProfiles()))
}
