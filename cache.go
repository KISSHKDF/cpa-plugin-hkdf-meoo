// cache.go — the reading cache the panel is served from, and the panel's view
// model.
//
// ---------------------------------------------------------------------------
// Why a cache and not a live walk per render
// ---------------------------------------------------------------------------
//
// One Meoo account's walk is up to six upstream calls (identity, projects,
// cloud, environment, points, entitlement). Doing that per panel poll would make
// the operator wait on a dozen round trips every five minutes and would turn a
// site hiccup into a panel full of empty cards. The fleet already paid for that
// lesson once — the minimax panel "点进去卡半天" incident was a cold refresh that
// made 18 serial upstream calls per render (RESCUE-AUDIT-minimax-20261025.md).
//
// So: readings live here behind a TTL. A read inside the window is served from
// this cache with zero upstream calls; the explicit 刷新数据 action forces a
// fresh walk; a background tick keeps the cache warm so even the first render is
// a hit.
//
// ---------------------------------------------------------------------------
// The three outcomes, and what each does to the numbers
// ---------------------------------------------------------------------------
//
//	ok        — replace the numbers.
//	transient — KEEP the previous numbers (the site did not answer usably; a
//	            hiccup must not blank a card) and say they are stale.
//	dead      — DROP the numbers. A key the upstream refuses must not keep
//	            showing yesterday's balance as if it were current.
//
// AttemptAt (not FetchedAt) is what the TTL gate reads, so a failing account is
// retried on the next sweep rather than on every panel poll.
//
// ---------------------------------------------------------------------------
// What the panel never sees
// ---------------------------------------------------------------------------
//
// The parked snapshots hold numbers and display metadata only. The two secrets
// (user-level key, project-level AK) stay in accounts.json; accountView below
// carries tails and ids, and assembleView reads the tails from the account record
// rather than from here. TestAccountViewNeverEchoesFullKey is the gate on that.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cacheEnvVar redirects the readings cache (tests, non-standard installs).
const cacheEnvVar = "MEOO_CPA_CACHE"

// maxConcurrentReads bounds one sweep. Six calls per account, four accounts at a
// time: enough to make a multi-account panel feel instant, bounded enough that a
// panel poll can never become a traffic generator.
const maxConcurrentReads = 4

// accountSnapshot is one account's latest known state, as parked in the cache.
type accountSnapshot struct {
	IdentityName string `json:"identity_name,omitempty"`
	Edition      string `json:"edition,omitempty"`

	// HasNumbers is false when there is nothing displayable (never read, or dead).
	HasNumbers bool    `json:"has_numbers"`
	Available  float64 `json:"available"`
	Granted    float64 `json:"granted"`
	Consumed   float64 `json:"consumed"`
	Frozen     float64 `json:"frozen"`

	PlanCode string `json:"plan_code,omitempty"`
	PlanName string `json:"plan_name,omitempty"`

	ProjectURLID   string `json:"project_url_id,omitempty"`
	ProjectName    string `json:"project_name,omitempty"`
	ProjectAKTail  string `json:"project_ak_tail,omitempty"`
	ProjectAKState string `json:"project_ak_state,omitempty"`
	CloudEnabled   bool   `json:"cloud_enabled"`

	Outcome    string `json:"outcome"`
	Note       string `json:"note,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// FetchedAt is when the numbers were actually read (age is measured from here).
	FetchedAt int64 `json:"fetched_at"`
	// AttemptAt is when we last tried (the TTL gate reads this one).
	AttemptAt int64 `json:"attempt_at"`
}

// ageSeconds is how old the displayed numbers are; -1 when never read.
func (s *accountSnapshot) ageSeconds(now time.Time) int64 {
	if s == nil || s.FetchedAt == 0 {
		return -1
	}
	return int64(now.Sub(time.UnixMilli(s.FetchedAt)).Seconds())
}

// stale reports whether the numbers survived a failed attempt.
func (s *accountSnapshot) stale() bool {
	return s != nil && s.HasNumbers && s.Outcome == outcomeTransient
}

// lastRefreshAt is the stamp the row shows, or nil when nothing was ever read.
func (s *accountSnapshot) lastRefreshAt() *string {
	if s == nil || s.FetchedAt == 0 {
		return nil
	}
	return nowPtr(time.UnixMilli(s.FetchedAt))
}

// cacheFile is the on-disk shape.
type cacheFile struct {
	UpdatedAt int64                       `json:"updated_at"`
	Entries   map[string]*accountSnapshot `json:"entries"`
}

var (
	cacheMu      sync.Mutex
	cacheEntries = map[string]*accountSnapshot{}
	cacheLoaded  bool
)

// cachePath resolves the readings-cache location.
//
// It sits next to the account store but is a SEPARATE file: the readings are
// disposable (清空缓存 drops them) while accounts.json is the operator's data.
// Sharing one file would make "clear the cache" a button that can destroy
// credentials.
func cachePath() string {
	if v := strings.TrimSpace(os.Getenv(cacheEnvVar)); v != "" {
		return v
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, ".meoo-cpa", "readings.json")
}

// cacheLoadOnce loads the file into memory on first use.
func cacheLoadOnce() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheLoaded {
		return
	}
	cacheLoaded = true
	path := cachePath()
	if path == "" {
		return
	}
	buf, errRead := os.ReadFile(path)
	if errRead != nil || len(buf) == 0 {
		return
	}
	var file cacheFile
	if errJSON := json.Unmarshal(buf, &file); errJSON != nil {
		// A corrupt readings cache is disposable; say so and start empty rather than
		// failing every panel read on it.
		log.Printf("%s: readings cache is not valid JSON (%s): %v — starting empty", providerName, path, errJSON)
		return
	}
	for id, entry := range file.Entries {
		if entry == nil || strings.TrimSpace(id) == "" {
			continue
		}
		cacheEntries[id] = entry
	}
}

// cacheSaveLocked persists the cache. Best effort: a write failure must not fail
// a read, and the panel keeps working from memory either way — but it leaves a
// trace, because a silently dropped write means the next start reads a stale file
// with nothing anywhere explaining why.
func cacheSaveLocked(now time.Time) {
	path := cachePath()
	if path == "" {
		return
	}
	if errDir := os.MkdirAll(filepath.Dir(path), 0o700); errDir != nil {
		log.Printf("%s: readings cache directory %s could not be created: %v", providerName, filepath.Dir(path), errDir)
		return
	}
	file := cacheFile{UpdatedAt: now.UnixMilli(), Entries: cacheEntries}
	buf, errMarshal := json.MarshalIndent(file, "", "  ")
	if errMarshal != nil {
		return
	}
	if errWrite := writeFileAtomic(path, append(buf, '\n')); errWrite != nil {
		log.Printf("%s: readings cache write failed (%s): %v", providerName, path, errWrite)
	}
}

// cacheGet returns the stored snapshot for one account (may be nil).
func cacheGet(id string) *accountSnapshot {
	cacheLoadOnce()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry := cacheEntries[strings.TrimSpace(id)]
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	return &copyEntry
}

// cacheFresh reports whether the entry may be served without an upstream read.
func cacheFresh(entry *accountSnapshot, ttl time.Duration, now time.Time) bool {
	if entry == nil || entry.AttemptAt == 0 {
		return false
	}
	return now.Sub(time.UnixMilli(entry.AttemptAt)) < ttl
}

// applyResult folds one walk into the cache and returns the new state.
//
// A dead key DROPS the numbers; a transient failure KEEPS them and marks them
// stale. Getting this backwards in either direction is the same class of bug: a
// card that confidently states something false.
func applyResult(account accountRecord, result accountRead, now time.Time) *accountSnapshot {
	cacheLoadOnce()
	cacheMu.Lock()
	defer cacheMu.Unlock()

	previous := cacheEntries[account.ID]
	next := &accountSnapshot{}
	if previous != nil {
		*next = *previous
	}
	// Metadata refreshes on every sweep: a re-provisioned account must not keep
	// showing the project it had last week.
	if result.IdentityName != "" {
		next.IdentityName = result.IdentityName
	}
	if result.Edition != "" {
		next.Edition = result.Edition
	}
	if result.PlanCode != "" {
		next.PlanCode = result.PlanCode
	}
	if result.PlanName != "" {
		next.PlanName = result.PlanName
	}
	if result.ProjectURLID != "" {
		next.ProjectURLID = result.ProjectURLID
	}
	if result.ProjectName != "" {
		next.ProjectName = result.ProjectName
	}
	if result.ProjectAK != "" {
		next.ProjectAKTail = keyTail(result.ProjectAK)
		next.ProjectAKState = "ok"
	}
	if next.ProjectAKState == "" {
		next.ProjectAKState = "missing"
	}
	next.CloudEnabled = result.CloudEnabled

	next.Outcome = result.Outcome
	next.Note = result.Note
	next.HTTPStatus = result.HTTPStatus
	next.AttemptAt = now.UnixMilli()

	switch result.Outcome {
	case outcomeOK:
		next.HasNumbers = true
		next.Available = derefFloat(result.Available)
		next.Granted = derefFloat(result.Granted)
		next.Consumed = derefFloat(result.Consumed)
		next.Frozen = derefFloat(result.Frozen)
		next.FetchedAt = now.UnixMilli()
	case outcomeDead:
		// The key is gone: yesterday's number is not "stale", it is wrong.
		next.HasNumbers = false
		next.Available = 0
		next.Granted = 0
		next.Consumed = 0
		next.Frozen = 0
		next.FetchedAt = 0
	default:
		// transient: keep whatever numbers we had, and keep FetchedAt so the card
		// can say how old they are.
		if next.Note == "" {
			next.Note = "读取失败（保留上次读数）"
		}
	}

	cacheEntries[account.ID] = next
	cacheSaveLocked(now)
	// The project leg may have learned a new url_id / project AK; that belongs in
	// the account store, which is the only place either secret is written.
	recordProject(account.ID, result.ProjectURLID, result.ProjectAK)

	copyEntry := *next
	return &copyEntry
}

// derefFloat turns an absent number into 0 for the cache's internal columns. The
// VIEW is what must distinguish "absent" from 0, and it does: it reads
// HasNumbers and the snapshot's columns together.
func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// cacheReset drops every cached reading, plus the gateway catalogue, which is
// cached on the same terms. It cannot change an upstream allowance and it never
// touches accounts.json.
func cacheReset() int {
	cacheLoadOnce()
	cacheMu.Lock()
	count := len(cacheEntries)
	cacheEntries = map[string]*accountSnapshot{}
	cacheSaveLocked(time.Now())
	cacheMu.Unlock()

	modelsMu.Lock()
	modelsCache = nil
	modelsMu.Unlock()
	return count
}

// cacheEntriesSnapshot returns a copy of all entries.
func cacheEntriesSnapshot() map[string]accountSnapshot {
	cacheLoadOnce()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	out := make(map[string]accountSnapshot, len(cacheEntries))
	for id, entry := range cacheEntries {
		if entry == nil {
			continue
		}
		out[id] = *entry
	}
	return out
}

// resetCacheForTest clears memory and the loaded flag (tests point MEOO_CPA_CACHE
// at a temp file first).
func resetCacheForTest() {
	cacheMu.Lock()
	cacheEntries = map[string]*accountSnapshot{}
	cacheLoaded = false
	cacheMu.Unlock()
	modelsMu.Lock()
	modelsCache = nil
	modelsMu.Unlock()
}

// -----------------------------------------------------------------------------
// Gateway catalogue cache
// -----------------------------------------------------------------------------

// modelsReading is the cached gateway catalogue.
type modelsReading struct {
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model,omitempty"`
	Note         string   `json:"note,omitempty"`
	FetchedAt    int64    `json:"fetched_at"`
}

var (
	modelsMu    sync.Mutex
	modelsCache *modelsReading
)

// firstProjectAK returns the project-level key of the first account that has one.
// The catalogue is per-project on this gateway, and every project on one account
// serves the same model list, so one is enough.
func firstProjectAK(accounts []accountRecord) string {
	for _, account := range accounts {
		if key := strings.TrimSpace(account.ProjectAK); key != "" {
			return key
		}
	}
	return ""
}

// loadModels returns the gateway catalogue, cached behind the same TTL as the
// readings. force reloads it regardless of the window.
//
// A failed reload keeps the previous catalogue and reports why: a model card that
// empties itself because one request timed out is a card that lies.
func loadModels(force bool) *modelsReading {
	accounts, _ := listAccounts()
	ttl := currentRefreshTTL()
	now := time.Now()

	modelsMu.Lock()
	cached := modelsCache
	modelsMu.Unlock()
	if !force && cached != nil && now.Sub(time.UnixMilli(cached.FetchedAt)) < ttl {
		return cached
	}

	projectAK := firstProjectAK(accounts)
	if projectAK == "" {
		empty := &modelsReading{
			Models:    []string{},
			Note:      "还没有任何账号带项目级 AK：先添加账号并刷新数据，发现链会取到它。",
			FetchedAt: now.UnixMilli(),
		}
		if cached != nil {
			return cached
		}
		return empty
	}

	catalogue, errFetch := fetchGatewayModels(projectAK)
	if errFetch != nil {
		if cached != nil {
			kept := *cached
			kept.Note = "模型目录刷新失败，显示上次结果：" + note(errFetch.Error())
			return &kept
		}
		return &modelsReading{
			Models:    []string{},
			Note:      "模型目录读取失败：" + note(errFetch.Error()),
			FetchedAt: now.UnixMilli(),
		}
	}

	reading := &modelsReading{
		Models:       append([]string{}, catalogue.Models...),
		DefaultModel: strings.TrimSpace(catalogue.DefaultModel),
		FetchedAt:    now.UnixMilli(),
	}
	modelsMu.Lock()
	modelsCache = reading
	modelsMu.Unlock()
	return reading
}

// -----------------------------------------------------------------------------
// Sweep
// -----------------------------------------------------------------------------

// refreshInFlight makes refreshAll single-flight across the manual /refresh path
// and the background tick.
var refreshInFlight sync.Mutex

// refreshAll walks every account whose TTL has expired.
//
// force=true is the 刷新数据 path and the background tick: it ignores the TTL.
//
// Single-flight: the manual POST /refresh and the background tick can otherwise
// overlap (most visibly when a manual refresh outlasts the tick period), doubling
// the concurrent upstream traffic. The loser returns immediately with
// refreshed=0 and skips the sweep; the next tick picks the work up, so nothing is
// lost.
func refreshAll(force bool) (int, error) {
	if !refreshInFlight.TryLock() {
		return 0, nil
	}
	defer refreshInFlight.Unlock()

	accounts, errAccounts := listAccounts()
	if errAccounts != nil {
		return 0, errAccounts
	}
	ttl := currentRefreshTTL()
	now := time.Now()

	due := make([]accountRecord, 0, len(accounts))
	for _, account := range accounts {
		if force || !cacheFresh(cacheGet(account.ID), ttl, now) {
			due = append(due, account)
		}
	}
	if len(due) == 0 {
		return 0, nil
	}

	results := make([]accountRead, len(due))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentReads)
	for i := range due {
		wg.Add(1)
		index, account := i, due[i]
		// safeGo keeps a panic in one account's walk from taking down the host
		// process (goroutine_guard.go documents why that matters); the semaphore and
		// the WaitGroup live inside the guarded closure so a panic still releases the
		// slot and still counts the worker as done.
		safeGo("meoo-read-account", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[index] = readAccount(account)
		})
	}
	wg.Wait()

	for i := range due {
		result := results[i]
		if result.Outcome == "" {
			// safeGo swallowed a panic: record it as transient rather than leaving the
			// account permanently "never read".
			result = accountRead{Outcome: outcomeTransient, Note: "读取时发生内部错误"}
		}
		applyResult(due[i], result, time.Now())
	}
	return len(due), nil
}

// -----------------------------------------------------------------------------
// View model
// -----------------------------------------------------------------------------

// accountView is one row of the panel. It carries NO credential: key_tail and
// project_ak_tail are the last four characters, and id is a sha256 prefix.
//
// Every field of the agreed row is emitted unconditionally (no omitempty) except
// identity_name, which is this plugin's own addition: a panel that has to guard
// `row.note === undefined` is a panel that will one day render "undefined".
// Absent NUMBERS stay null, which is why they are pointers.
type accountView struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	KeyTail        string   `json:"key_tail"`
	IdentityName   string   `json:"identity_name,omitempty"`
	PlanCode       string   `json:"plan_code"`
	PlanName       string   `json:"plan_name"`
	Available      *float64 `json:"available"`
	Granted        *float64 `json:"granted"`
	Consumed       *float64 `json:"consumed"`
	ProjectURLID   string   `json:"project_url_id"`
	ProjectName    string   `json:"project_name"`
	ProjectAKTail  string   `json:"project_ak_tail"`
	ProjectAKState string   `json:"project_ak_state"`
	CloudEnabled   bool     `json:"cloud_enabled"`
	Readable       bool     `json:"readable"`
	Outcome        string   `json:"outcome"`
	Note           string   `json:"note"`
	Stale          bool     `json:"stale"`
	AgeSeconds     int64    `json:"age_seconds"`
	LastRefreshAt  *string  `json:"last_refresh_at"`

	// DailyLogin is the state of today's 2,000-point login reward for this
	// account: the outcome (granted / already / failed / needs_login), how many
	// points arrived and when the batch became effective.
	//
	// It is a nested object rather than two flat fields because the operator's
	// question is "did today's 2,000 arrive, and if not why not" — and the answer
	// is a small record, not a scalar.
	DailyLogin map[string]any `json:"daily_login,omitempty"`
}

// meooSummary is the header of the panel.
type meooSummary struct {
	Version         string   `json:"version"`
	Accounts        int      `json:"accounts"`
	Readable        int      `json:"readable"`
	Dead            int      `json:"dead"`
	Stale           int      `json:"stale"`
	TotalAvailable  *float64 `json:"total_available"`
	TotalGranted    *float64 `json:"total_granted"`
	TotalConsumed   *float64 `json:"total_consumed"`
	RefreshedCount  int      `json:"refreshed_count"`
	RefreshMinutes  int      `json:"refresh_minutes"`
	ServedFromCache bool     `json:"served_from_cache"`
	LastAttemptAt   *string  `json:"last_attempt_at"`
	Notes           []string `json:"notes"`
}

// buildView sweeps what the TTL says is due and then assembles the payload.
//
// force=false still walks accounts the TTL considers due, so a cold start is not
// an empty panel; every other account is served straight from the cache, which is
// what makes the panel poll free.
func buildView(force bool) map[string]any {
	refreshed, _ := refreshAll(force)
	return assembleView(refreshed, false)
}

// usageView is the zero-upstream view: it never walks, only reads the cache.
func usageView() map[string]any {
	return assembleView(0, true)
}

// assembleView builds the payload from the cache plus whatever the sweep just
// stored. It performs NO upstream reads of its own.
func assembleView(refreshed int, cacheOnly bool) map[string]any {
	now := time.Now()
	ttl := currentRefreshTTL()
	accounts, errAccounts := listAccounts()
	snapshots := cacheEntriesSnapshot()

	rows := make([]accountView, 0, len(accounts))
	summary := meooSummary{
		Version:         version,
		Accounts:        len(accounts),
		RefreshMinutes:  int(ttl.Minutes()),
		RefreshedCount:  refreshed,
		ServedFromCache: refreshed == 0,
		Notes:           []string{},
	}
	var totalAvailable, totalGranted, totalConsumed float64
	var lastAttempt time.Time
	missingProjectAK := 0
	for _, account := range accounts {
		view := accountView{
			ID:             account.ID,
			Label:          account.Label,
			KeyTail:        keyTail(account.UserKey),
			ProjectURLID:   account.ProjectURLID,
			ProjectAKTail:  keyTail(account.ProjectAK),
			ProjectAKState: projectAKState(account.ProjectAK),
			Outcome:        "unknown",
			AgeSeconds:     -1,
		}
		snapshot, okSnapshot := snapshots[account.ID]
		if okSnapshot {
			if snapshot.IdentityName != "" {
				view.IdentityName = snapshot.IdentityName
			}
			if snapshot.ProjectURLID != "" {
				view.ProjectURLID = snapshot.ProjectURLID
			}
			if snapshot.ProjectName != "" {
				view.ProjectName = snapshot.ProjectName
			}
			if snapshot.ProjectAKTail != "" {
				view.ProjectAKTail = snapshot.ProjectAKTail
			}
			if snapshot.ProjectAKState != "" {
				view.ProjectAKState = snapshot.ProjectAKState
			}
			view.CloudEnabled = snapshot.CloudEnabled
			view.PlanCode = snapshot.PlanCode
			view.PlanName = snapshot.PlanName
			view.Outcome = snapshot.Outcome
			view.Note = snapshot.Note
			view.Stale = snapshot.stale()
			view.AgeSeconds = snapshot.ageSeconds(now)
			view.LastRefreshAt = snapshot.lastRefreshAt()
			if attempt := time.UnixMilli(snapshot.AttemptAt); snapshot.AttemptAt > 0 && attempt.After(lastAttempt) {
				lastAttempt = attempt
			}
			if snapshot.HasNumbers {
				available := snapshot.Available
				granted := snapshot.Granted
				consumed := snapshot.Consumed
				view.Available = &available
				view.Granted = &granted
				view.Consumed = &consumed
				view.Readable = true
				summary.Readable++
				totalAvailable += available
				totalGranted += granted
				totalConsumed += consumed
			}
			if snapshot.Outcome == outcomeDead {
				summary.Dead++
			}
			if view.Stale {
				summary.Stale++
			}
			if view.ProjectAKState != "ok" {
				missingProjectAK++
			}
		} else {
			view.Note = "尚未读取：点「刷新数据」走一次发现链。"
			if view.ProjectAKState != "ok" {
				missingProjectAK++
			}
		}
		// The daily-login state is attached for every row, claimed or not: the
		// operator must be able to see that today's reward did NOT arrive, not
		// just that it did.
		view.DailyLogin = dailyLoginStatusFor(account.ID, now)
		rows = append(rows, view)
	}
	if summary.Readable > 0 {
		// Absent is not zero: with nothing readable the totals stay null rather than
		// claiming an account's balance is 0.
		summary.TotalAvailable = &totalAvailable
		summary.TotalGranted = &totalGranted
		summary.TotalConsumed = &totalConsumed
	}
	summary.LastAttemptAt = nowPtr(lastAttempt)
	if errAccounts != nil {
		summary.Notes = append(summary.Notes, "账号存储读取失败："+note(errAccounts.Error()))
	}
	if len(accounts) == 0 {
		summary.Notes = append(summary.Notes, "还没有添加任何 Meoo 账号：把用户级 meoo_ak_… 粘进面板的「加账号」。")
	}
	if missingProjectAK > 0 {
		summary.Notes = append(summary.Notes, fmt.Sprintf(
			"%d 个账号还没有项目级 AK（模型网关通道因此不可用）：点「刷新数据」重走发现链，或在账号下先建好项目与云服务。",
			missingProjectAK))
	}

	payload := map[string]any{
		"summary":     summary,
		"accounts":    rows,
		"config":      configEcho(),
		"server_time": nowTimestamp(now),
		"cache_path":  cachePath(),
	}
	if cacheOnly {
		payload["mode"] = "cache"
		payload["note"] = "本视图只读缓存，不做任何上游调用；每行 age_seconds 是读数的年龄。"
	}
	return payload
}

// projectAKState reports whether the stored project-level key can serve the model
// gateway. It reads the key only to decide, and never echoes it.
func projectAKState(projectAK string) string {
	if strings.TrimSpace(projectAK) == "" {
		return "missing"
	}
	return "ok"
}
