package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture credentials. They are OBVIOUSLY fake on purpose (the release gate
// forbids anything shaped like a real credential in a test fixture): the shape is
// what matters here, never a value.
const (
	fakeUserKey    = "meoo_ak_TESTFIXTURE0000000000000001ABCD"
	fakeUserKey2   = "meoo_ak_TESTFIXTURE0000000000000002WXYZ"
	fakeProjectAK  = "meoo_project_service_v1.TESTFIXTURE.000000.1.abcdef"
	fakeProjectAK2 = "meoo_project_service_v1.TESTFIXTURE.000000.2.ghijkl"
)

// isolateMeoo points every external input at a temp path (so no test can read or
// write the machine's real account store) and every upstream base at a port that
// is provably closed, so a test that forgets to install a fake server fails
// locally instead of talking to meoo.com.
func isolateMeoo(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv(accountsEnvVar, filepath.Join(dir, "accounts.json"))
	t.Setenv(cacheEnvVar, filepath.Join(dir, "readings.json"))
	t.Setenv(dailyLoginStoreEnvVar, filepath.Join(dir, "daily-login.json"))
	t.Setenv(sessionProfileRootEnvVar, filepath.Join(dir, "profiles"))
	resetCacheForTest()
	resetAccountsForTest()
	resetConfigForTest()
	resetDailyLoginStatesForTest()
	resetAccountOpenIDCacheForTest()

	// A port that was just closed: any accidental walk is refused instantly, and
	// nothing leaves the machine.
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("cannot allocate a closed port for the offline guard: %v", errListen)
	}
	closed := "http://" + listener.Addr().String()
	if errClose := listener.Close(); errClose != nil {
		t.Fatalf("cannot close the offline guard listener: %v", errClose)
	}
	t.Cleanup(swapEndpoints(t, closed+"/open/v1", closed+"/gateway/v1"))

	// No test may start a real refresh timer: the tick would fire long after the
	// test finished and would read globals this suite is mutating (-race).
	setTimeAfterForTest(t, func(time.Duration) <-chan time.Time {
		return make(chan time.Time)
	})

	t.Cleanup(resetConfigForTest)
	t.Cleanup(resetCacheForTest)
}

// setTimeAfterForTest installs a clock seam for one test.
func setTimeAfterForTest(t *testing.T, fn func(time.Duration) <-chan time.Time) {
	t.Helper()
	timeAfterMu.Lock()
	previous := timeAfterFn
	timeAfterFn = fn
	timeAfterMu.Unlock()
	t.Cleanup(func() {
		timeAfterMu.Lock()
		timeAfterFn = previous
		timeAfterMu.Unlock()
	})
}

// swapEndpoints redirects both upstream bases and returns the restore function.
func swapEndpoints(t *testing.T, openPlatform, gateway string) func() {
	t.Helper()
	previousOpen := openPlatformBase
	previousGateway := gatewayBase
	openPlatformBase = openPlatform
	gatewayBase = gateway
	return func() {
		openPlatformBase = previousOpen
		gatewayBase = previousGateway
	}
}

// swapOpenPlatform aims the open-platform calls at a local server.
func swapOpenPlatform(t *testing.T, base string) {
	t.Helper()
	previous := openPlatformBase
	openPlatformBase = base
	t.Cleanup(func() { openPlatformBase = previous })
}

// swapGateway aims the gateway calls at a local server.
func swapGateway(t *testing.T, base string) {
	t.Helper()
	previous := gatewayBase
	gatewayBase = base
	t.Cleanup(func() { gatewayBase = previous })
}

// setConfigForTest mutates the effective config for one test and restores it.
func setConfigForTest(t *testing.T, mutate func(*pluginConfig)) {
	t.Helper()
	cfgMu.Lock()
	next := currentCfg
	mutate(&next)
	currentCfg = next
	cfgMu.Unlock()
	t.Cleanup(resetConfigForTest)
}

// readAccountsFileRaw returns the store exactly as it is on disk.
func readAccountsFileRaw(t *testing.T) string {
	t.Helper()
	buf, errRead := os.ReadFile(accountsPath())
	if errRead != nil {
		t.Fatalf("read account store: %v", errRead)
	}
	return string(buf)
}

// TestAccountsFileIsCreatedEmptyOnFirstStart pins the "never invent an account"
// discipline: a fresh start leaves a real, empty store on disk.
func TestAccountsFileIsCreatedEmptyOnFirstStart(t *testing.T) {
	isolateMeoo(t)

	accounts, errList := listAccounts()
	if errList != nil {
		t.Fatalf("listAccounts on a fresh store: %v", errList)
	}
	if len(accounts) != 0 {
		t.Fatalf("a fresh store must hold zero accounts, got %d", len(accounts))
	}
	if _, errStat := os.Stat(accountsPath()); errStat != nil {
		t.Fatalf("the store file must exist after a first read: %v", errStat)
	}
	raw := readAccountsFileRaw(t)
	if !strings.Contains(raw, `"version": 1`) {
		t.Fatalf("the store must carry its schema version, got %s", raw)
	}
	if strings.Contains(raw, userKeyPrefix) {
		t.Fatalf("the fresh store must not carry any preset credential: %s", raw)
	}
}

// TestAddAccountStoresKeyAndDerivesID pins the row id contract: content-derived,
// stable and short.
func TestAddAccountStoresKeyAndDerivesID(t *testing.T) {
	isolateMeoo(t)

	record, errAdd := addAccount(fakeUserKey, "  好看的风  ")
	if errAdd != nil {
		t.Fatalf("addAccount: %v", errAdd)
	}
	if record.ID != accountID(fakeUserKey) {
		t.Fatalf("row id %q is not the key's digest %q", record.ID, accountID(fakeUserKey))
	}
	if len(record.ID) != 16 {
		t.Fatalf("row id %q must be 16 hex characters", record.ID)
	}
	if record.Label != "好看的风" {
		t.Fatalf("label %q was not trimmed/stored", record.Label)
	}
	if record.AddedAt == "" {
		t.Fatal("added_at must be stamped")
	}

	// The key itself lives in the store — that is the one place it may live.
	if raw := readAccountsFileRaw(t); !strings.Contains(raw, fakeUserKey) {
		t.Fatalf("the store must persist the key: %s", raw)
	}
}

// TestAddAccountSameKeyUpdatesLabelInsteadOfDuplicating: the row id is derived
// from the key, so pasting the same key twice cannot create two rows.
func TestAddAccountSameKeyUpdatesLabelInsteadOfDuplicating(t *testing.T) {
	isolateMeoo(t)

	first, errFirst := addAccount(fakeUserKey, "第一版")
	if errFirst != nil {
		t.Fatalf("first add: %v", errFirst)
	}
	recordProject(first.ID, "proj-url-id-1", fakeProjectAK)

	second, errSecond := addAccount(fakeUserKey, "第二版")
	if errSecond != nil {
		t.Fatalf("second add: %v", errSecond)
	}
	if second.ID != first.ID {
		t.Fatalf("the same key produced two ids: %q vs %q", first.ID, second.ID)
	}
	if second.Label != "第二版" {
		t.Fatalf("the label was not updated: %q", second.Label)
	}
	if second.ProjectAK != fakeProjectAK || second.ProjectURLID != "proj-url-id-1" {
		t.Fatalf("re-adding a key must not lose what the walk discovered: %+v", second)
	}
	accounts, errList := listAccounts()
	if errList != nil {
		t.Fatalf("list: %v", errList)
	}
	if len(accounts) != 1 {
		t.Fatalf("store holds %d rows for one key, want 1", len(accounts))
	}
	// An empty label must not blank the operator's note.
	third, errThird := addAccount(fakeUserKey, "")
	if errThird != nil {
		t.Fatalf("third add: %v", errThird)
	}
	if third.Label != "第二版" {
		t.Fatalf("an empty label must keep the previous one, got %q", third.Label)
	}
}

// TestAddAccountRefusesTheWrongCredentialFamily is the "两套凭据不可混用" gate: a
// project-level key pasted as an account key is refused locally, with a message
// that names the mistake, instead of becoming a 401 that looks like a dead
// account.
func TestAddAccountRefusesTheWrongCredentialFamily(t *testing.T) {
	isolateMeoo(t)

	cases := []struct {
		name    string
		key     string
		wantSub string
	}{
		{"project-level key", fakeProjectAK, "项目级"},
		{"no prefix at all", "sk-abcdef0123456789", userKeyPrefix},
		{"empty", "   ", "required"},
		{"two values on one line", fakeUserKey + " " + fakeUserKey2, "whitespace"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, errAdd := addAccount(testCase.key, "x"); errAdd == nil {
				t.Fatalf("addAccount(%q) must be refused", testCase.key)
			} else if !strings.Contains(errAdd.Error(), testCase.wantSub) {
				t.Fatalf("the refusal must name the problem (%q): %v", testCase.wantSub, errAdd)
			}
		})
	}
	accounts, errList := listAccounts()
	if errList != nil {
		t.Fatalf("list: %v", errList)
	}
	if len(accounts) != 0 {
		t.Fatalf("a refused add must store nothing, got %+v", accounts)
	}
}

// TestRemoveAccountDropsOnlyTheNamedRow.
func TestRemoveAccountDropsOnlyTheNamedRow(t *testing.T) {
	isolateMeoo(t)

	first, _ := addAccount(fakeUserKey, "甲")
	second, _ := addAccount(fakeUserKey2, "乙")

	removed, errRemove := removeAccount(first.ID)
	if errRemove != nil || !removed {
		t.Fatalf("removeAccount(%s) = %v, %v", first.ID, removed, errRemove)
	}
	accounts, _ := listAccounts()
	if len(accounts) != 1 || accounts[0].ID != second.ID {
		t.Fatalf("the wrong row survived removal: %+v", accounts)
	}
	again, errAgain := removeAccount(first.ID)
	if errAgain != nil {
		t.Fatalf("removing an unknown id must not error: %v", errAgain)
	}
	if again {
		t.Fatal("removing an unknown id must report removed=false")
	}
	if _, errEmpty := removeAccount("  "); errEmpty == nil {
		t.Fatal("an empty id must be refused")
	}
}

// TestCorruptStoreIsRefusedNotOverwritten: silently replacing an unparsable store
// with an empty one would destroy every account on the way to "fixing" it.
func TestCorruptStoreIsRefusedNotOverwritten(t *testing.T) {
	isolateMeoo(t)
	if errWrite := os.WriteFile(accountsPath(), []byte("{ this is not json"), 0o600); errWrite != nil {
		t.Fatalf("seed a corrupt store: %v", errWrite)
	}

	if _, errList := listAccounts(); errList == nil {
		t.Fatal("a corrupt store must be reported, not read as empty")
	}
	if _, errAdd := addAccount(fakeUserKey, "x"); errAdd == nil {
		t.Fatal("a corrupt store must refuse a write rather than clobber the file")
	}
	raw := readAccountsFileRaw(t)
	if !strings.Contains(raw, "this is not json") {
		t.Fatalf("the corrupt file was overwritten: %s", raw)
	}
}

// TestAccountViewNeverEchoesFullKey is the meoo twin of loomy's
// TestAccountViewNeverEchoesSessionToken: the marshalled panel payload must not
// contain either credential, while still telling the operator which key is which.
func TestAccountViewNeverEchoesFullKey(t *testing.T) {
	isolateMeoo(t)

	record, errAdd := addAccount(fakeUserKey, "好看的风")
	if errAdd != nil {
		t.Fatalf("addAccount: %v", errAdd)
	}
	recordProject(record.ID, "l1l7coxwlq0f", fakeProjectAK)

	available, granted, consumed := 11146.0, 12000.0, 907.0
	applyResult(record, accountRead{
		Outcome:      outcomeOK,
		HasNumbers:   true,
		Available:    &available,
		Granted:      &granted,
		Consumed:     &consumed,
		PlanCode:     "free",
		PlanName:     "Free 免费版",
		ProjectURLID: "l1l7coxwlq0f",
		ProjectName:  "CPA-Model-Gateway",
		ProjectAK:    fakeProjectAK,
	}, time.Now())

	raw, errMarshal := json.Marshal(assembleView(0, true))
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	payload := string(raw)
	for _, secret := range []string{fakeUserKey, fakeProjectAK} {
		if strings.Contains(payload, secret) {
			t.Fatalf("the panel payload echoed a full credential (%s…)", secret[:12])
		}
	}
	if !strings.Contains(payload, keyTail(fakeUserKey)) {
		t.Fatalf("the payload must still identify the key by its tail: %s", payload)
	}
	if !strings.Contains(payload, keyTail(fakeProjectAK)) {
		t.Fatalf("the payload must still identify the project key by its tail: %s", payload)
	}
	for _, key := range []string{`"id"`, `"key_tail"`, `"project_ak_tail"`, `"project_ak_state"`, `"readable"`, `"age_seconds"`, `"last_refresh_at"`} {
		if !strings.Contains(payload, key) {
			t.Errorf("the row is missing %s: %s", key, payload)
		}
	}
	if strings.Contains(payload, `"project_ak":`) {
		t.Fatalf("a full project key field must not exist in the view: %s", payload)
	}
}

// TestKeyTailHandlesShortValues: a truncated tail is never an error, and never a
// whole short secret by accident on the wire (the caller decides what to show).
func TestKeyTailHandlesShortValues(t *testing.T) {
	if got := keyTail("  meoo_ak_ABCD  "); got != "ABCD" {
		t.Fatalf("keyTail trimmed value = %q", got)
	}
	if got := keyTail("ab"); got != "ab" {
		t.Fatalf("a short value must be returned whole, got %q", got)
	}
	if got := keyTail(""); got != "" {
		t.Fatalf("empty tail = %q", got)
	}
}

// TestAccountIDIsStableAndIgnoresWhitespace.
func TestAccountIDIsStableAndIgnoresWhitespace(t *testing.T) {
	if accountID(fakeUserKey) != accountID("  "+fakeUserKey+"  ") {
		t.Fatal("the row id must ignore surrounding whitespace")
	}
	if accountID(fakeUserKey) == accountID(fakeUserKey2) {
		t.Fatal("different keys must produce different ids")
	}
	if accountID(fakeUserKey) == fakeUserKey {
		t.Fatal("the row id must not be the key itself")
	}
}

// TestRecordProjectOnlyWritesWhatWasLearned: a failed project leg must not erase a
// previously good project AK.
func TestRecordProjectOnlyWritesWhatWasLearned(t *testing.T) {
	isolateMeoo(t)

	record, errAdd := addAccount(fakeUserKey, "甲")
	if errAdd != nil {
		t.Fatalf("add: %v", errAdd)
	}
	recordProject(record.ID, "proj-1", fakeProjectAK)
	recordProject(record.ID, "", "")

	accounts, _ := listAccounts()
	if len(accounts) != 1 {
		t.Fatalf("store rows = %d", len(accounts))
	}
	if accounts[0].ProjectAK != fakeProjectAK || accounts[0].ProjectURLID != "proj-1" {
		t.Fatalf("an empty write-back erased known project data: %+v", accounts[0])
	}
	recordProject(record.ID, "proj-2", fakeProjectAK2)
	accounts, _ = listAccounts()
	if accounts[0].ProjectAK != fakeProjectAK2 || accounts[0].ProjectURLID != "proj-2" {
		t.Fatalf("a new project was not written back: %+v", accounts[0])
	}
}

// TestAtomicWriteLeavesNoTempFile: the temp file must not survive a successful
// write (a stray accounts.json.tmp would be read by nothing, forever).
func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	isolateMeoo(t)
	if _, errAdd := addAccount(fakeUserKey, "甲"); errAdd != nil {
		t.Fatalf("add: %v", errAdd)
	}
	if _, errStat := os.Stat(accountsPath() + ".tmp"); !os.IsNotExist(errStat) {
		t.Fatalf("the temp file survived the write: %v", errStat)
	}
}

// TestProjectAKStateNeverReturnsTheKey.
func TestProjectAKStateNeverReturnsTheKey(t *testing.T) {
	if got := projectAKState("  "); got != "missing" {
		t.Fatalf("projectAKState(empty) = %q", got)
	}
	if got := projectAKState(fakeProjectAK); got != "ok" {
		t.Fatalf("projectAKState(key) = %q", got)
	}
	if strings.Contains(projectAKState(fakeProjectAK), fakeProjectAK) {
		t.Fatal("projectAKState must not return the key")
	}
}
