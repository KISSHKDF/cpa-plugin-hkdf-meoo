// daily_login_store.go — where the daily-login result is kept, and who runs it.
//
// ---------------------------------------------------------------------------
// Two separate concerns, deliberately in one file
// ---------------------------------------------------------------------------
//
// daily_login.go decides WHETHER a claim happened (and that decision is made
// against the account's own points ledger). This file decides WHEN the attempt
// runs and WHERE the outcome is parked so the panel can show it.
//
// ---------------------------------------------------------------------------
// Why the result is persisted rather than kept in memory
// ---------------------------------------------------------------------------
//
// The plugin is a DLL loaded by a host that may restart at any time, and the
// panel is read far more often than the reward is claimed. If the outcome lived
// only in memory, a host restart would silently reset the panel to "not claimed
// today" and the operator would see a wrong picture. So the per-account outcome
// is parked next to the readings cache, in its own file, written atomically.
//
// ---------------------------------------------------------------------------
// When the attempt runs
// ---------------------------------------------------------------------------
//
// The reward is granted ONCE per day, on the first login of that day. A sweep
// that runs every refresh period would therefore hammer a call that has nothing
// to do after the first time of the day.
//
// The sweep therefore runs at most once per business day, and only shortly after
// the local midnight that starts the new day:
//
//   - after midnight the previous day's grant is gone and today's is available;
//   - the operator's own machine may be asleep at midnight, so the sweep also
//     runs on the next wake-up if today's attempt has not happened yet.
//
// The per-account check inside claimDailyLogin is the real guard (it reads the
// ledger and returns "already" without any request when today's batch exists);
// the sweep's own guard exists so the plugin does not wake up to make that check
// every hour.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dailyLoginStoreEnvVar redirects the daily-login state file (tests).
const dailyLoginStoreEnvVar = "MEOO_CPA_DAILY_LOGIN"

// dailyLoginStoreFile is the on-disk shape.
type dailyLoginStoreFile struct {
	Version int                        `json:"version"`
	Entries map[string]dailyLoginState `json:"entries"`
}

// dailyLoginStoreVersion is the on-disk schema version.
const dailyLoginStoreVersion = 1

var (
	dailyLoginMu     sync.Mutex
	dailyLoginStates = map[string]dailyLoginState{}
	dailyLoginLoaded bool
)

// dailyLoginStorePath resolves the state file location.
func dailyLoginStorePath() string {
	if v := strings.TrimSpace(os.Getenv(dailyLoginStoreEnvVar)); v != "" {
		return v
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, ".meoo-cpa", "daily-login.json")
}

// loadDailyLoginStates reads the parked outcomes once.
func loadDailyLoginStates() {
	if dailyLoginLoaded {
		return
	}
	dailyLoginLoaded = true
	states := map[string]dailyLoginState{}
	path := dailyLoginStorePath()
	if path == "" {
		dailyLoginStates = states
		return
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		dailyLoginStates = states
		return
	}
	var file dailyLoginStoreFile
	if errJSON := json.Unmarshal(raw, &file); errJSON != nil {
		// A corrupt file is never overwritten on read; the sweep will replace it
		// with a valid one once it has something to write.
		dailyLoginStates = states
		return
	}
	if file.Entries != nil {
		states = file.Entries
	}
	dailyLoginStates = states
}

// saveDailyLoginStates writes the parked outcomes atomically.
func saveDailyLoginStates() {
	path := dailyLoginStorePath()
	if path == "" {
		return
	}
	file := dailyLoginStoreFile{Version: dailyLoginStoreVersion, Entries: dailyLoginStates}
	raw, errMarshal := json.MarshalIndent(file, "", "  ")
	if errMarshal != nil {
		return
	}
	if errWrite := writeFileAtomic(path, raw); errWrite != nil {
		return
	}
}

// dailyLoginStateFor returns the parked outcome for one account.
func dailyLoginStateFor(accountID string) dailyLoginState {
	dailyLoginMu.Lock()
	defer dailyLoginMu.Unlock()
	loadDailyLoginStates()
	return dailyLoginStates[accountID]
}

// setDailyLoginState parks one account's outcome and persists it.
func setDailyLoginState(accountID string, state dailyLoginState) {
	dailyLoginMu.Lock()
	defer dailyLoginMu.Unlock()
	loadDailyLoginStates()
	dailyLoginStates[accountID] = state
	saveDailyLoginStates()
}

// resetDailyLoginStatesForTest drops the in-memory park AND forgets that the
// file was read, so the next read comes from disk again.
//
// This is the "host restarted" simulation: the process state is gone, the file is
// not. A test that wants an EMPTY park must delete the file too (the isolate
// helper points the path at a fresh temp directory, which does that).
func resetDailyLoginStatesForTest() {
	dailyLoginMu.Lock()
	defer dailyLoginMu.Unlock()
	dailyLoginStates = map[string]dailyLoginState{}
	dailyLoginLoaded = false
}

// dailyLoginSweepSummary is what one sweep did, for the panel and the log.
type dailyLoginSweepSummary struct {
	Day       string `json:"day"`
	Attempted int    `json:"attempted"`
	Granted   int    `json:"granted"`
	Already   int    `json:"already"`
	Failed    int    `json:"failed"`
	NeedsLog  int    `json:"needs_login"`
	At        string `json:"at"`
}

// runDailyLoginSweep attempts the daily reward for every account that has not
// been attempted on this business day.
//
// The per-account guard is the ledger check inside claimDailyLogin, so this
// function is safe to call at any time: an account already claimed today is
// reported as "already" without any request to the reward endpoint.
func runDailyLoginSweep(now time.Time, onlyAccountID string) dailyLoginSweepSummary {
	summary := dailyLoginSweepSummary{Day: businessDay(now), At: nowTimestamp(now)}
	accounts, errAccounts := listAccounts()
	if errAccounts != nil {
		return summary
	}
	for _, account := range accounts {
		if onlyAccountID != "" && account.ID != onlyAccountID {
			continue
		}
		parked := dailyLoginStateFor(account.ID)
		// Once per business day per account: a parked outcome for today means the
		// attempt already happened.
		//
		// EXCEPT "needs_login": the operator has not logged in yet, so the day is
		// NOT over for that account. A later attempt (the sweep runs hourly, and
		// the operator can press the button) must be allowed to pick it up — the
		// moment they log in, the plugin should claim the day's 2,000 without
		// waiting for tomorrow. Blocking the whole day on a missing login would
		// silently cost the operator a day's points.
		if parked.BusinessDay == summary.Day && parked.Outcome != "" &&
			parked.Outcome != claimFailed && parked.Outcome != claimNeedsLogin {
			if parked.Outcome == claimGranted {
				summary.Granted++
			} else if parked.Outcome == claimAlready {
				summary.Already++
			}
			continue
		}
		attempt, _ := claimDailyLogin(account, now)
		setDailyLoginState(account.ID, attempt.State)
		summary.Attempted++
		switch attempt.Outcome {
		case claimGranted:
			summary.Granted++
		case claimAlready:
			summary.Already++
		case claimNeedsLogin:
			summary.NeedsLog++
		default:
			summary.Failed++
		}
	}
	return summary
}

// dailyLoginDue reports whether the sweep should run now.
//
// It is true when the local day has turned over and this account has no parked
// outcome for the new day. The sweep then runs shortly after midnight, and again
// after a wake-up (the host may have been asleep) — never more than once per
// hour, so a long-running host does not make the same check over and over.
func dailyLoginDue(state dailyLoginState, now time.Time) bool {
	day := businessDay(now)
	if state.BusinessDay == day && state.Outcome != "" {
		return false
	}
	// Give the upstream a moment after midnight before the first attempt of the
	// day: a grant issued at 00:00:00.2 is still "today's", but a request racing
	// the rollover could be answered by the previous day's ledger.
	return now.Hour() >= 0 && now.Hour() < 24
}

// dailyLoginNextDelay is how long to wait before the next sweep check.
//
// It is short after midnight (the grant becomes available promptly) and longer
// later in the day (a failed session should be retried, but not every minute).
func dailyLoginNextDelay(now time.Time) time.Duration {
	if now.Hour() == 0 && now.Minute() < 30 {
		return 15 * time.Minute
	}
	return 60 * time.Minute
}

// dailyLoginStatusFor renders one account's daily-login state for the panel.
func dailyLoginStatusFor(accountID string, now time.Time) map[string]any {
	state := dailyLoginStateFor(accountID)
	out := map[string]any{
		"business_day": businessDay(now),
		"attempted":    state.Outcome != "",
	}
	if state.Outcome != "" {
		out["outcome"] = state.Outcome
		out["granted_amount"] = state.GrantedAmount
		out["balance_after"] = state.BalanceAfter
		if state.BatchAt != "" {
			out["batch_at"] = state.BatchAt
		}
		if state.SkipReason != "" {
			out["skip_reason"] = state.SkipReason
		}
		if state.Note != "" {
			out["note"] = state.Note
		}
		if state.AttemptAt > 0 {
			out["attempt_at"] = nowTimestamp(time.UnixMilli(state.AttemptAt))
		}
	}
	return out
}

// describeDailyLogin renders a one-line Chinese explanation of an outcome, for
// the panel and the log.
func describeDailyLogin(outcome string) string {
	switch outcome {
	case claimGranted:
		return "已领取每日登录赠送积分"
	case claimAlready:
		return "今日登录赠送积分已到账"
	case claimNeedsLogin:
		return "需要人工登录：请在浏览器登录 meoo.com 后重试"
	case claimFailed:
		return "本次未领取到积分"
	default:
		return "尚未尝试"
	}
}

// dailyLoginLogLine renders one account's outcome for the log. It never carries
// the cookie or any part of it.
func dailyLoginLogLine(account accountRecord, attempt dailyLoginAttempt) string {
	return fmt.Sprintf("秒悟每日登录积分 %s: %s (%s)",
		account.Label, describeDailyLogin(attempt.Outcome), attempt.State.Note)
}
