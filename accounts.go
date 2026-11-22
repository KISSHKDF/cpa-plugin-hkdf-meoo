// accounts.go — the plugin-owned account store.
//
// ---------------------------------------------------------------------------
// Why this plugin keeps its own account file
// ---------------------------------------------------------------------------
//
// The credential is a USER-level Meoo key (meoo_ak_...). It belongs to the
// operator, not to CPA: the host's auth store only holds credentials that a
// host-side auth provider parsed, and that is not this plugin's shape (it
// declares no auth_provider at all). So the keys live in one small JSON file the
// plugin owns:
//
//	%USERPROFILE%\.meoo-cpa\accounts.json
//
// On-disk discipline, all of it load-bearing:
//
//   - the write is atomic (temp file + os.Rename), so a crash mid-write cannot
//     leave a half-parsed account file behind — the failure mode that loses
//     every account at once;
//   - the directory is 0700 and the file 0600: both credential families in here
//     are secrets;
//   - a corrupt file is NEVER overwritten. Mutations refuse and say so; silently
//     replacing an unparsable file with an empty one would destroy the store on
//     the way to "fixing" it.
//
// ---------------------------------------------------------------------------
// What may be echoed
// ---------------------------------------------------------------------------
//
// The full key NEVER leaves this file. The management API and the panel see only:
//
//	id         sha256(user_key)[:16] — stable, content-derived, safe to correlate
//	key_tail   the last four characters (enough to tell two keys apart by eye)
//	label      the operator's note
//
// accountView below is the ONLY type that crosses that boundary, and
// TestAccountViewNeverEchoesFullKey pins it against a real marshalled payload.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Credential prefixes. The two families are NOT interchangeable: sending the
// project-level key to the open platform (or the user-level key to the gateway)
// is an authentication failure that reads like a dead account, so both shapes
// are recognised locally and refused with a message that names the mismatch.
const (
	userKeyPrefix    = "meoo_ak_"
	projectKeyPrefix = "meoo_project_service_v1."
)

// accountsEnvVar redirects the account store (tests, non-standard installs).
const accountsEnvVar = "MEOO_CPA_ACCOUNTS"

// accountsFileVersion is the on-disk schema version.
const accountsFileVersion = 1

// keyTailLen is how many trailing characters of a secret are safe to echo. Four
// is enough to tell two keys apart by eye and far too little to use.
const keyTailLen = 4

// accountRecord is one stored account, exactly as it is serialised.
type accountRecord struct {
	ID string `json:"id"`
	// Label is the operator's note. Never derived from the credential.
	Label string `json:"label,omitempty"`
	// UserKey is the USER-level open-platform key. Secret: never echoed.
	UserKey string `json:"user_key"`
	// ProjectURLID / ProjectAK are filled in by the discovery walk (upstream.go)
	// and written back here, so a restart does not have to re-walk the project leg
	// just to reach the gateway. ProjectAK is a project-level key. Secret.
	ProjectURLID string `json:"project_url_id,omitempty"`
	ProjectAK    string `json:"project_ak,omitempty"`
	// SessionCookie is the WEB SESSION cookie for this account — the credential
	// the daily 2,000-point login reward needs (the open-platform key cannot
	// trigger it; see daily_login.go).
	//
	// It is OPTIONAL: when it is empty the plugin looks for a logged-in browser
	// profile on this machine (session_cookie.go). It is stored only when the
	// operator pastes one, for the case where the browser route cannot reach the
	// session — e.g. the everyday browser holds its cookie store locked while it
	// runs, which is the normal state of an open browser.
	//
	// Secret: never echoed. The panel shows only whether one is present.
	SessionCookie string `json:"session_cookie,omitempty"`
	AddedAt       string `json:"added_at,omitempty"`
}

// keyTail is the only part of a secret that may be echoed.
func keyTail(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= keyTailLen {
		return key
	}
	return key[len(key)-keyTailLen:]
}

// accountID is the stable, content-derived row id. It is a sha256 prefix rather
// than a counter so that pasting the same key twice cannot create two rows, and
// so the id itself leaks nothing about the key.
func accountID(userKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(userKey)))
	return hex.EncodeToString(sum[:])[:16]
}

// accountsPath resolves the account file location.
func accountsPath() string {
	if v := strings.TrimSpace(os.Getenv(accountsEnvVar)); v != "" {
		return v
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, ".meoo-cpa", "accounts.json")
}

// accountsFile is the on-disk shape.
type accountsFile struct {
	Version  int             `json:"version"`
	Accounts []accountRecord `json:"accounts"`
}

// Error values callers and tests match on.
var (
	errNoAccountsPath = errors.New("cannot resolve the Meoo account store path (no user home directory)")
	errStoreCorrupt   = errors.New("the Meoo account store exists but is not valid JSON; refusing to overwrite it")
)

var accountsMu sync.Mutex

// emptyAccountsFile is what a first start writes. It deliberately contains no
// accounts: this plugin never invents or imports a credential.
var emptyAccountsFile = accountsFile{Version: accountsFileVersion, Accounts: []accountRecord{}}

// ensureAccountsFile creates the store (and its directory) when absent. A no-op
// when the file already exists — including when it exists but is corrupt, which
// is a condition for the reader to report, not for this function to "fix".
func ensureAccountsFile() error {
	path := accountsPath()
	if path == "" {
		return errNoAccountsPath
	}
	if _, errStat := os.Stat(path); errStat == nil {
		return nil
	} else if !os.IsNotExist(errStat) {
		return errStat
	}
	if errDir := os.MkdirAll(filepath.Dir(path), 0o700); errDir != nil {
		return errDir
	}
	buf, errMarshal := json.MarshalIndent(emptyAccountsFile, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	return writeFileAtomic(path, append(buf, '\n'))
}

// readAccountsFile parses the store. A missing file is created first, so a first
// start has a real (empty) store on disk rather than a phantom path in the panel.
func readAccountsFile() (accountsFile, error) {
	if errEnsure := ensureAccountsFile(); errEnsure != nil {
		return emptyAccountsFile, errEnsure
	}
	path := accountsPath()
	buf, errRead := os.ReadFile(path)
	if errRead != nil {
		return emptyAccountsFile, errRead
	}
	if len(strings.TrimSpace(string(buf))) == 0 {
		return emptyAccountsFile, nil
	}
	var file accountsFile
	if errJSON := json.Unmarshal(buf, &file); errJSON != nil {
		return emptyAccountsFile, fmt.Errorf("%w (%s): %v", errStoreCorrupt, path, errJSON)
	}
	if file.Accounts == nil {
		file.Accounts = []accountRecord{}
	}
	if file.Version == 0 {
		file.Version = accountsFileVersion
	}
	return file, nil
}

// writeAccountsFile persists the store atomically.
func writeAccountsFile(file accountsFile) error {
	path := accountsPath()
	if path == "" {
		return errNoAccountsPath
	}
	if errDir := os.MkdirAll(filepath.Dir(path), 0o700); errDir != nil {
		return errDir
	}
	file.Version = accountsFileVersion
	if file.Accounts == nil {
		file.Accounts = []accountRecord{}
	}
	buf, errMarshal := json.MarshalIndent(file, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	return writeFileAtomic(path, append(buf, '\n'))
}

// writeFileAtomic writes path via a temp file in the same directory and renames
// it into place, so a reader never observes a partial document.
func writeFileAtomic(path string, payload []byte) error {
	temp := path + ".tmp"
	if errWrite := os.WriteFile(temp, payload, 0o600); errWrite != nil {
		return errWrite
	}
	if errRename := os.Rename(temp, path); errRename != nil {
		// Leave a trace instead of a silently dropped update, and do not leave the
		// temp file behind for the next run to trip over.
		_ = os.Remove(temp)
		return errRename
	}
	return nil
}

// listAccounts returns the stored accounts in a deterministic order (oldest
// first, then by id) so the panel does not reshuffle on every poll.
func listAccounts() ([]accountRecord, error) {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	file, errRead := readAccountsFile()
	if errRead != nil {
		return nil, errRead
	}
	out := make([]accountRecord, len(file.Accounts))
	copy(out, file.Accounts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AddedAt != out[j].AddedAt {
			return out[i].AddedAt < out[j].AddedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// validateUserKey accepts only a user-level open-platform key and explains any
// other shape. Rejecting the project-level family HERE (rather than at the first
// 401) is the whole point: the two are easy to confuse and the upstream error
// for the wrong one is indistinguishable from an expired account.
func validateUserKey(userKey string) (string, error) {
	key := strings.TrimSpace(userKey)
	if key == "" {
		return "", errors.New("user_key is required")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("user_key contains whitespace — it looks like more than one value was pasted")
	}
	if strings.HasPrefix(key, projectKeyPrefix) {
		return "", fmt.Errorf("这是项目级 key（PROJECT-level，%s…），不是用户级 key —— 面板需要以 %q 开头的用户级（开放平台）key", projectKeyPrefix, userKeyPrefix)
	}
	if !strings.HasPrefix(key, userKeyPrefix) {
		return "", fmt.Errorf("user_key 必须以 %q 开头（用户级 开放平台 key）；两套凭据不可混用", userKeyPrefix)
	}
	return key, nil
}

// addAccount stores one user-level key. Pasting a key that is already stored
// updates its label instead of creating a second row for the same account — the
// row id is derived from the key, so "the same key" is decidable.
func addAccount(userKey, label string) (accountRecord, error) {
	key, errKey := validateUserKey(userKey)
	if errKey != nil {
		return accountRecord{}, errKey
	}
	id := accountID(key)
	label = truncate(strings.TrimSpace(label), 80)

	accountsMu.Lock()
	defer accountsMu.Unlock()
	file, errRead := readAccountsFile()
	if errRead != nil {
		return accountRecord{}, errRead
	}
	record := accountRecord{
		ID:      id,
		Label:   label,
		UserKey: key,
		AddedAt: nowTimestamp(time.Now()),
	}
	for i := range file.Accounts {
		if file.Accounts[i].ID == id {
			// Keep what the walk already discovered; refresh the human fields.
			record.ProjectURLID = file.Accounts[i].ProjectURLID
			record.ProjectAK = file.Accounts[i].ProjectAK
			record.AddedAt = file.Accounts[i].AddedAt
			// The stored web session belongs to the ACCOUNT, not to the key
			// paste: re-adding the same key must not drop it (that would silently
			// cost the operator the daily reward).
			record.SessionCookie = file.Accounts[i].SessionCookie
			if label == "" {
				record.Label = file.Accounts[i].Label
			}
			file.Accounts[i] = record
			if errWrite := writeAccountsFile(file); errWrite != nil {
				return accountRecord{}, errWrite
			}
			return record, nil
		}
	}
	file.Accounts = append(file.Accounts, record)
	if errWrite := writeAccountsFile(file); errWrite != nil {
		return accountRecord{}, errWrite
	}
	return record, nil
}

// removeAccount drops one row by id. It never touches CPA's config.
func removeAccount(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("id is required")
	}
	accountsMu.Lock()
	defer accountsMu.Unlock()
	file, errRead := readAccountsFile()
	if errRead != nil {
		return false, errRead
	}
	kept := make([]accountRecord, 0, len(file.Accounts))
	removed := false
	for _, record := range file.Accounts {
		if record.ID == id {
			removed = true
			continue
		}
		kept = append(kept, record)
	}
	if !removed {
		return false, nil
	}
	file.Accounts = kept
	if errWrite := writeAccountsFile(file); errWrite != nil {
		return false, errWrite
	}
	return true, nil
}

// findAccount returns one stored account by id.
func findAccount(id string) (accountRecord, error) {
	id = strings.TrimSpace(id)
	accounts, errList := listAccounts()
	if errList != nil {
		return accountRecord{}, errList
	}
	for _, record := range accounts {
		if record.ID == id {
			return record, nil
		}
	}
	return accountRecord{}, fmt.Errorf("no account with id %q", id)
}

// setAccountSessionCookie stores (or clears, with an empty value) the web-session
// cookie for one account.
//
// The cookie is a secret: it is written to the account file with the same
// discipline as the key and is never echoed back.
func setAccountSessionCookie(id, cookie string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	accountsMu.Lock()
	defer accountsMu.Unlock()
	file, errRead := readAccountsFile()
	if errRead != nil {
		return errRead
	}
	found := false
	for i := range file.Accounts {
		if file.Accounts[i].ID == id {
			file.Accounts[i].SessionCookie = strings.TrimSpace(cookie)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no account with id %q", id)
	}
	return writeAccountsFile(file)
}

// recordProject stores what the discovery walk learned about one account's
// project. Only the fields actually discovered are written, so a failed project
// leg cannot erase a previously good project AK.
func recordProject(id, projectURLID, projectAK string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	accountsMu.Lock()
	defer accountsMu.Unlock()
	file, errRead := readAccountsFile()
	if errRead != nil {
		return
	}
	changed := false
	for i := range file.Accounts {
		if file.Accounts[i].ID != id {
			continue
		}
		if projectURLID != "" && file.Accounts[i].ProjectURLID != projectURLID {
			file.Accounts[i].ProjectURLID = projectURLID
			changed = true
		}
		if projectAK != "" && file.Accounts[i].ProjectAK != projectAK {
			file.Accounts[i].ProjectAK = projectAK
			changed = true
		}
	}
	if !changed {
		return
	}
	if errWrite := writeAccountsFile(file); errWrite != nil {
		// Best effort: a read must not fail because a write-back did — but it must
		// leave a trace, or the next start silently re-walks the project leg.
		log.Printf("%s: account project write-back failed (id=%s): %v", providerName, id, errWrite)
	}
}

// resetAccountsForTest removes the store so a test starts from "first start".
func resetAccountsForTest() {
	path := accountsPath()
	if path == "" {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".tmp")
}
