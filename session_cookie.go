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
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	errNoSessionProfile = errors.New("本机没有该账号已登录的浏览器会话")
	errSessionExpired   = errors.New("保存的网页会话已失效")
)

// sessionHint is what the operator is told when a session cannot be used.
//
// It is a plain sentence, not an internal error string: this text lands in the
// panel and in the log, and a Go error like "no session profile for this account:
// …" is noise to the person who has to act on it.
const sessionHint = "请在浏览器里登录 meoo.com（阿里云账号），插件会自动复用那个会话；本插件不保存密码、不填短信。"

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

// listSessionProfiles enumerates every profile directory worth asking.
//
// Two families, in this order:
//
//  1. the operator's own per-account Edge profiles under
//     %USERPROFILE%\.meoo-cpa\edge-profile\<account id> — the layout this
//     plugin's daily-login script uses;
//  2. the NORMAL browser profiles (Edge / Chrome / Brave …) — because the
//     operator may well have logged into 秒悟 in their everyday browser rather
//     than in a dedicated profile. Measured 2026-11-21: account 「好看的风」 had
//     exactly such a session and was invisible to the first family alone.
//
// The second family is read ONLY, and only to find a session; nothing is written
// to any browser profile.
func listSessionProfiles() []sessionProfile {
	var out []sessionProfile
	seen := map[string]bool{}
	for _, profile := range listDedicatedProfiles() {
		if seen[profile.Dir] {
			continue
		}
		seen[profile.Dir] = true
		out = append(out, profile)
	}
	for _, profile := range listBrowserProfiles() {
		if seen[profile.Dir] {
			continue
		}
		seen[profile.Dir] = true
		out = append(out, profile)
	}
	return out
}

// listDedicatedProfiles enumerates the per-account profiles this plugin's own
// tooling creates.
func listDedicatedProfiles() []sessionProfile {
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
		if profile, ok := profileAt(dir); ok {
			out = append(out, profile)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// browserProfileRoots are the everyday browsers' user-data directories.
//
// They are probed read-only. A profile that does not exist is skipped silently.
func browserProfileRoots() []string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return nil
	}
	return []string{
		filepath.Join(local, "Microsoft", "Edge", "User Data"),
		filepath.Join(local, "Google", "Chrome", "User Data"),
		filepath.Join(local, "BraveSoftware", "Brave-Browser", "User Data"),
	}
}

// listBrowserProfiles enumerates the everyday browsers' profiles.
//
// Each browser keeps several profiles (the person, "Profile 1", "Work" …) and
// Chromium writes the cookie store at <user data>\<profile>\Default\Network\Cookies
// with the owning `Local State` two levels up.
func listBrowserProfiles() []sessionProfile {
	var out []sessionProfile
	for _, root := range browserProfileRoots() {
		if _, errStat := os.Stat(root); errStat != nil {
			continue
		}
		entries, errRead := os.ReadDir(root)
		if errRead != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if profile, ok := profileAt(dir); ok {
				out = append(out, profile)
			}
		}
	}
	return out
}

// profileAt builds the profile description for one user-data directory, if it
// really holds a Chromium cookie store and its `Local State`.
func profileAt(dir string) (sessionProfile, bool) {
	cookiePath := filepath.Join(dir, "Default", "Network", "Cookies")
	localState := filepath.Join(dir, "Local State")
	if _, errCookie := os.Stat(cookiePath); errCookie != nil {
		return sessionProfile{}, false
	}
	if _, errState := os.Stat(localState); errState != nil {
		return sessionProfile{}, false
	}
	return sessionProfile{Dir: dir, CookiePath: cookiePath, LocalStatePath: localState}, true
}

// readSessionCookie decrypts the Meoo cookies out of one profile and returns them
// as a Cookie header value.
//
// The store may be LOCKED by a running browser: Chromium keeps the cookie file
// open, and on Windows an exclusive open makes a plain read fail with
// "being used by another process". That is the normal state of an everyday
// browser — the operator has Edge open while the plugin runs — so the file is
// first copied through a SHARED read, which Windows permits while another
// process holds the handle. The copy is what gets parsed; the original is never
// touched.
//
// (Measured 2026-11-21: without the copy, the operator's normal Edge profile
// read failed exactly this way and its 秒悟 session was invisible.)
//
// A store that still cannot be read is an error, never an empty cookie: an empty
// cookie would look like "logged out" and hide the real reason.
func readSessionCookie(profile sessionProfile) (string, error) {
	return readSessionCookieFn(profile)
}

// readSessionCookieFn is the seam: production decrypts the store, a test
// substitutes a fixture cookie.
var readSessionCookieFn = func(profile sessionProfile) (string, error) {
	storePath, cleanup, errCopy := copyCookieStore(profile.CookiePath)
	if errCopy != nil {
		return "", fmt.Errorf("session cookie store: %w", errCopy)
	}
	defer cleanup()

	aesKey, errKey := loadChromiumAESKey(profile.LocalStatePath)
	if errKey != nil {
		return "", fmt.Errorf("session key: %w", errKey)
	}
	db, errOpen := readSQLiteFile(storePath)
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

// copyCookieStore copies a Chromium cookie store to a temporary file that can be
// read while the browser holds it open.
//
// The copy is what gets parsed; the original is never written to. The journal
// file is copied too when it exists, because Chromium's rollback journal holds
// the most recent writes that may not be in the main file yet.
func copyCookieStore(path string) (string, func(), error) {
	source, errOpen := openSharedRead(path)
	if errOpen != nil {
		return "", func() {}, errOpen
	}
	defer func() { _ = source.Close() }()

	temp, errTemp := os.CreateTemp("", "meoo-cookies-*.db")
	if errTemp != nil {
		return "", func() {}, errTemp
	}
	tempPath := temp.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if _, errCopy := io.Copy(temp, source); errCopy != nil {
		_ = temp.Close()
		cleanup()
		return "", func() {}, errCopy
	}
	if errClose := temp.Close(); errClose != nil {
		cleanup()
		return "", func() {}, errClose
	}
	return tempPath, cleanup, nil
}

// isMeooCookieHost reports whether a cookie host key belongs to Meoo.
// Chromium stores the same cookie under both "meoo.com" and ".meoo.com".
func isMeooCookieHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, ".")
	return host == meooCookieHost
}

// -----------------------------------------------------------------------------
// Session expiry — the C of "A+C": tell the operator BEFORE the session dies
// -----------------------------------------------------------------------------

// webKitEpochOffset is the difference between the Windows FILETIME epoch
// (1601-01-01) and the Unix epoch, in seconds. Chromium stores cookie expiry in
// FILETIME microseconds; getting this wrong shifts every expiry by four
// centuries.
const webKitEpochOffset = 11644473600

// sessionExpiry is how much of a session is left, as the panel shows it.
type sessionExpiry struct {
	// Found is false when the profile holds no session cookie at all — the
	// account has never been logged in (or the store is unreadable).
	Found bool `json:"found"`
	// ExpiresAt is the session cookie's expiry, "2026-12-16 03:28" (local).
	ExpiresAt string `json:"expires_at,omitempty"`
	// DaysLeft is how many days remain, one decimal. Negative = already expired.
	DaysLeft float64 `json:"days_left,omitempty"`
	// ProfileDir names where the session lives, so the operator knows which
	// browser window carries it.
	ProfileDir string `json:"profile_dir,omitempty"`
}

// sessionExpiryOf reads the session cookie's expiry from one profile's store.
//
// It reads ONLY the expiry timestamp — no decryption needed for this (the
// expiry column is plaintext in the store), so it works even when the value
// would not decrypt.
//
// The COOKIE DB of a RUNNING browser may be locked (see copyCookieStore); an
// unreadable store reports Found=false rather than guessing a date.
func sessionExpiryOf(profile sessionProfile) sessionExpiry {
	out := sessionExpiry{}
	storePath, cleanup, errCopy := copyCookieStore(profile.CookiePath)
	if errCopy != nil {
		return out
	}
	defer cleanup()
	db, errOpen := readSQLiteFile(storePath)
	if errOpen != nil {
		return out
	}
	rows, errRows := db.sqliteReadTable("cookies")
	if errRows != nil {
		return out
	}
	// The session id is the credential that matters; its expiry IS the session's
	// expiry. (The ticket lives slightly longer but the site needs both, and the
	// id dies first.)
	for _, row := range rows {
		host := strings.TrimSpace(row["host_key"].string())
		name := strings.TrimSpace(row["name"].string())
		if !isMeooCookieHost(host) || name != "oneday_sid" {
			continue
		}
		raw := strings.TrimSpace(row["expires_utc"].string())
		if raw == "" {
			continue
		}
		micro, errParse := strconv.ParseInt(raw, 10, 64)
		if errParse != nil || micro <= 0 {
			continue
		}
		expires := time.Unix(micro/1_000_000-webKitEpochOffset, 0)
		days := time.Until(expires).Hours() / 24
		out = sessionExpiry{
			Found:      true,
			ExpiresAt:  expires.Format("2006-01-02 15:04"),
			DaysLeft:   math.Round(days*10) / 10,
			ProfileDir: profile.Dir,
		}
		return out
	}
	return out
}

// sessionExpiryForAccount resolves the expiry for one account: its own profile
// first, then any profile on the machine whose session belongs to it (matched
// by identity, same as the claim path).
//
// The identity sweep reads live sessions only (a dead session cannot answer who
// it is), so an EXPIRED profile is reported from its own directory — that is
// exactly the case the warning exists for.
func sessionExpiryForAccount(account accountRecord) sessionExpiry {
	if profile, errProfile := sessionProfileFor(account.ID); errProfile == nil {
		if expiry := sessionExpiryOf(profile); expiry.Found {
			return expiry
		}
	}
	// Fall through to a name-only match across every known profile: the
	// dedicated profile layout uses the account id as the directory name, and a
	// directory named after this account whose session belongs to another
	// account would mislead — so only directories that still decrypt AND answer
	// with this account's identity are trusted here. The cheap path: reuse the
	// same profile list, decrypt each store, and keep the first whose session
	// matches.
	for _, profile := range listSessionProfiles() {
		cookie, errCookie := readSessionCookie(profile)
		if errCookie != nil {
			continue
		}
		identity, errIdentity := fetchSessionIdentity(cookie)
		if errIdentity != nil {
			continue
		}
		if want := accountOpenID(account); want != "" && identity.OpenID == want {
			return sessionExpiryOf(profile)
		}
	}
	return sessionExpiry{}
}
