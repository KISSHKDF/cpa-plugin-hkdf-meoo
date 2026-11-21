// session_cookie.go — the web-session credential behind the daily login reward.
//
// ---------------------------------------------------------------------------
// What this is for
// ---------------------------------------------------------------------------
//
// 秒悟 (Meoo) grants every user 2,000 points on their FIRST login of the day.
// There is no claim button and no claim endpoint: the grant is issued by
//
//	GET https://meoo.com/api/mc/entitlement/init
//
// which the official web app calls on every load (measured 2026-11-21 against the
// 0.0.113 web bundle: `initEntitlement` is the only caller, the response carries
// dailyLoginGranted / dailyLoginGrantAmount / dailyLoginSkipReason, and the whole
// 3.2 MB bundle contains no claim/signin/checkin endpoint at all).
//
// That endpoint does NOT accept the open-platform key this plugin already stores.
// The AK (`meoo_ak_…`, Bearer) can read `/open/v1/cli-compat/account/points`, but
// calling it does NOT log the account in and does NOT grant the points — measured
// directly: an account whose AK was read daily never accumulated a grant, while
// an account that actually opened the site did.
//
// So the credential for the reward is the WEB SESSION COOKIE. This file obtains
// it from the operator's own browser profile on this machine.
//
// ---------------------------------------------------------------------------
// Where the cookie comes from, and why it is safe
// ---------------------------------------------------------------------------
//
// The operator keeps one Chromium (Edge) user-data profile per Meoo account, at
//
//	%USERPROFILE%\.meoo-cpa\edge-profile\<account id>
//
// where <account id> is exactly this plugin's account id (sha256(user_key)[:16],
// see accounts.go). A profile that has ever logged in holds the session cookies
// in `Default\Network\Cookies`, encrypted with the classic Chromium v10 scheme
// (see chromium_cookie.go). Decrypting one needs the DPAPI-wrapped AES key from
// that profile's `Local State`, and DPAPI CurrentUser scope means ONLY the
// Windows user who owns the profile can unwrap it — the same user the CPA host
// runs as. The credential never leaves this machine and is never logged: this file
// returns lengths and hashes to its callers, and the cookie value itself goes
// straight into one request header.
//
// ---------------------------------------------------------------------------
// How a profile is matched to an account
// ---------------------------------------------------------------------------
//
// By IDENTITY, never by directory name alone: the profile's session is asked who
// it is (`GET /api/v1/user/me` → open_id) and the account's key is asked who it
// is (`GET /open/v1/cli-compat/account/points` → account.userId). Measured
// 2026-11-21: those two values are the SAME number (e.g. 1241690576128755 for
// account "1969"), so a profile can be attributed to an account without any
// operator-maintained mapping. A profile whose identity matches no account, or
// whose session is dead, is simply not used.
//
// ---------------------------------------------------------------------------
// What this file never does
// ---------------------------------------------------------------------------
//
// It never invents a session. If the profile is missing, locked, encrypted with
// the v20 app-bound scheme, or simply logged out, the caller gets an error and
// the account is reported as "needs a human login" — never as "claimed".
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sessionProfileRootEnvVar redirects the profile root (tests, non-standard
// installs). The production default is %USERPROFILE%\.meoo-cpa\edge-profile.
const sessionProfileRootEnvVar = "MEOO_CPA_SESSION_PROFILES"

// meooCookieHost is the host key every Meoo session cookie is stored under.
// Both ".meoo.com" and "meoo.com" appear; the caller matches on the suffix.
const meooCookieHost = "meoo.com"

// sessionCookieNames are the cookies that carry the Meoo login state.
//
//	oneday_sid           the session id (30-day max-age)
//	login_oneday_ticket  the login ticket
//	*.sig                their signatures
//
// The signature cookies matter: Meoo validates them, so a request that carries
// only `oneday_sid` is refused with NEED_LOGIN. The whole set is therefore read
// and replayed together — the same set a browser would send.
var sessionCookieNames = map[string]bool{
	"oneday_sid":              true,
	"oneday_sid.sig":          true,
	"login_oneday_ticket":     true,
	"login_oneday_ticket.sig": true,
	"cna":                     true,
	"lang":                    true,
	"lang.sig":                true,
	"tfstk":                   true,
	"isg":                     true,
	"xlly_s":                  true,
	"__meeo_device_id__":      true,
	"__meeo_visit_id__":       true,
}

// Errors the caller distinguishes. They are deliberately distinct: "no profile"
// (operator must log in once) and "session expired" (profile exists, cookie is
// stale) call for different advice.
var (
	errNoSessionProfile = errors.New("no session profile for this account")
	errSessionExpired   = errors.New("the stored web session is no longer valid")
)

// sessionProfile is one browser profile on this machine.
type sessionProfile struct {
	// Dir is the profile's user-data directory.
	Dir string
	// CookiePath is the Chromium cookie store inside it.
	CookiePath string
	// LocalStatePath is the `Local State` file that owns the cookie store.
	LocalStatePath string
}

// sessionProfileRoot resolves the directory that holds one profile per account.
func sessionProfileRoot() string {
	if v := strings.TrimSpace(os.Getenv(sessionProfileRootEnvVar)); v != "" {
		return v
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, ".meoo-cpa", "edge-profile")
}

// sessionProfileFor returns the profile belonging to one account id, if the
// operator has created one.
//
// The directory name IS the account id (accounts.go derives it from the key), so
// the lookup is a single stat — but the caller still has to prove the identity
// (see resolveSessionIdentity), because a directory can hold a stale or foreign
// session.
func sessionProfileFor(accountID string) (sessionProfile, error) {
	return sessionProfileForFn(accountID)
}

// sessionProfileForFn is the seam: production resolves the directory, a test
// substitutes a fixture.
var sessionProfileForFn = func(accountID string) (sessionProfile, error) {
	root := sessionProfileRoot()
	if root == "" {
		return sessionProfile{}, errNoSessionProfile
	}
	dir := filepath.Join(root, accountID)
	cookiePath := filepath.Join(dir, "Default", "Network", "Cookies")
	localState := filepath.Join(dir, "Local State")
	if _, errStat := os.Stat(cookiePath); errStat != nil {
		return sessionProfile{}, fmt.Errorf("%w: %s", errNoSessionProfile, accountID)
	}
	if _, errStat := os.Stat(localState); errStat != nil {
		return sessionProfile{}, fmt.Errorf("%w: %s has no Local State", errNoSessionProfile, accountID)
	}
	return sessionProfile{Dir: dir, CookiePath: cookiePath, LocalStatePath: localState}, nil
}

// listSessionProfiles enumerates every profile directory on this machine. Used by
// the identity sweep, which has to consider profiles whose directory name does
// not match any account (a renamed profile, a leftover from another key).
func listSessionProfiles() []sessionProfile {
	root := sessionProfileRoot()
	if root == "" {
		return nil
	}
	entries, errRead := os.ReadDir(root)
	if errRead != nil {
		return nil
	}
	var out []sessionProfile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		cookiePath := filepath.Join(dir, "Default", "Network", "Cookies")
		localState := filepath.Join(dir, "Local State")
		if _, errCookie := os.Stat(cookiePath); errCookie != nil {
			continue
		}
		if _, errState := os.Stat(localState); errState != nil {
			continue
		}
		out = append(out, sessionProfile{Dir: dir, CookiePath: cookiePath, LocalStatePath: localState})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// readSessionCookie decrypts the Meoo cookies out of one profile and returns them
// as a Cookie header value.
//
// The store may be LOCKED by a running browser (Chromium keeps it open); on
// Windows the file is still readable with a shared read, which is what the
// fleet's other Chromium reader does. A store that cannot be read is an error,
// never an empty cookie: an empty cookie would look like "logged out" and hide
// the real reason.
func readSessionCookie(profile sessionProfile) (string, error) {
	return readSessionCookieFn(profile)
}

// readSessionCookieFn is the seam: production decrypts the store, a test
// substitutes a fixture cookie.
var readSessionCookieFn = func(profile sessionProfile) (string, error) {
	aesKey, errKey := loadChromiumAESKey(profile.LocalStatePath)
	if errKey != nil {
		return "", fmt.Errorf("session key: %w", errKey)
	}
	db, errOpen := readSQLiteFile(profile.CookiePath)
	if errOpen != nil {
		return "", fmt.Errorf("session cookie store: %w", errOpen)
	}
	rows, errRows := db.sqliteReadTable("cookies")
	if errRows != nil {
		return "", fmt.Errorf("session cookie rows: %w", errRows)
	}
	var pairs []string
	for _, row := range rows {
		host := strings.TrimSpace(row["host_key"].string())
		name := strings.TrimSpace(row["name"].string())
		if !isMeooCookieHost(host) || !sessionCookieNames[name] {
			continue
		}
		blob := row["encrypted_value"].Bytes
		if len(blob) == 0 {
			continue
		}
		plain, errDecrypt := decryptChromiumCookieValue(aesKey, blob, host)
		if errDecrypt != nil {
			// One undecryptable row must not sink the whole session: another
			// cookie in the set may still carry a usable session.
			continue
		}
		value := strings.TrimSpace(plain)
		if value == "" {
			continue
		}
		pairs = append(pairs, name+"="+value)
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("%w: no decodable Meoo cookie in %s", errSessionExpired, profile.Dir)
	}
	return strings.Join(pairs, "; "), nil
}

// isMeooCookieHost reports whether a cookie host key belongs to Meoo.
// Chromium stores the same cookie under both "meoo.com" and ".meoo.com".
func isMeooCookieHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, ".")
	return host == meooCookieHost
}
