// login_window_windows.go — open a login window for one account.
//
// WHY A WINDOW AT ALL: the reward needs a web session, and a web session can
// only be created by a human logging in (the AK cannot grant, the OAuth token
// is not a web session, and the Aliyun SSO page refuses every non-browser
// path — all measured 2026-11-21). The operator asked "can it be like workbuddy,
// never expiring": it cannot, but it CAN be one click — this file is that click.
//
// THE LAYOUT IS THE PLUGIN'S OWN: every account keeps its own Edge user-data
// profile at %USERPROFILE%\.meoo-cpa\edge-profile\<account id> (the same layout
// the cpa-ops daily-login scripts and the profile reader in session_cookie.go
// use). A dedicated profile is what keeps six accounts' sessions from colliding,
// and it means the window that opens is already scoped to THIS account.
//
// WHAT HAPPENS: the window opens at meoo.com. The operator logs in once (Aliyun
// SSO, SMS or password — the plugin never sees the password). The session lands
// in that profile's cookie store; the next claim attempt (the hourly sweep, or
// the panel button) picks it up — needs_login does not block the day.
//
// SAFETY: this spawns a browser the operator can see, pointing at meoo.com. It
// never runs headless, never passes credentials on the command line, and never
// touches any profile other than the account's own.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// loginWindowEdgePaths are the Edge executables tried in order.
var loginWindowEdgePaths = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
}

// loginWindowURL is where the window lands. The home page (not the login page
// directly) because the SPA decides which login flow to offer (SMS / password /
// QR), and because a landed session shows the account state — visible feedback
// that the login worked.
const loginWindowURL = "https://meoo.com/"

// loginWindowLock serializes window launches: two windows for the same profile
// would fight over the user-data-dir lock, and Edge would refuse the second.
var loginWindowLock sync.Mutex

// lastLoginWindowAt records when the last window was opened, per account. A
// window per click is what the operator wants, but a double-click should not
// open two.
var lastLoginWindowAt = map[string]time.Time{}

// openLoginWindow launches Edge with this account's own profile at meoo.com.
//
// It returns a human-readable status either way: launched (the operator should
// see the window), or the reason it could not (no Edge, profile locked by an
// existing window).
func openLoginWindow(account accountRecord) (string, error) {
	loginWindowLock.Lock()
	defer loginWindowLock.Unlock()

	// A window opened within the last 10 seconds: almost certainly a
	// double-click. Say so instead of spawning a second one.
	if last, ok := lastLoginWindowAt[account.ID]; ok && time.Since(last) < 10*time.Second {
		return "登录窗刚打开过（10 秒内），请看任务栏；没有再点一次。", nil
	}

	profileDir := filepath.Join(sessionProfileRoot(), account.ID)
	if errMkdir := os.MkdirAll(filepath.Join(profileDir, "Default"), 0o700); errMkdir != nil {
		return "", fmt.Errorf("创建该账号的专用浏览器目录失败: %w", errMkdir)
	}

	exe := ""
	for _, candidate := range loginWindowEdgePaths {
		if _, errStat := os.Stat(candidate); errStat == nil {
			exe = candidate
			break
		}
	}
	if exe == "" {
		return "", fmt.Errorf("本机没有找到 Edge（%s 都不存在）", strings.Join(loginWindowEdgePaths, " / "))
	}

	// --user-data-dir scopes the session to THIS account. No remote debugging:
	// the operator drives the window, the plugin reads the cookie store from
	// disk afterwards (session_cookie.go), so no debugging port is needed.
	cmd := exec.Command(exe,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		loginWindowURL,
	)
	if errStart := cmd.Start(); errStart != nil {
		return "", fmt.Errorf("启动 Edge 失败: %w", errStart)
	}
	// Detach: the window outlives this call, and the plugin must not wait for
	// the operator to finish logging in.
	safeGo("meoo-login-window-wait", func() { _ = cmd.Wait() })

	lastLoginWindowAt[account.ID] = time.Now()
	return "已在 Edge 中打开 meoo.com（账号「" + account.Label + "」的专用窗口）。请在窗口里登录；登录成功后点「领每日积分」即可到账。", nil
}

// loginWindowHintFor is the one-liner the panel shows with the launch button.
func loginWindowHintFor(account accountRecord, expiry sessionExpiry) string {
	switch {
	case !expiry.Found:
		return "该账号还没有登录会话"
	case expiry.DaysLeft < 0:
		return "会话已过期（" + expiry.ExpiresAt + "）——重新登录即可续上"
	case expiry.DaysLeft < 7:
		return fmt.Sprintf("会话还剩 %.1f 天（%s 到期）——建议尽快重新登录", expiry.DaysLeft, expiry.ExpiresAt)
	default:
		return fmt.Sprintf("会话还剩 %.1f 天（%s 到期）", expiry.DaysLeft, expiry.ExpiresAt)
	}
}
