# meoo

CPA (CLIProxyAPI) plugin — a read-only 秒悟 (Meoo) panel: points, plan, the project
each key owns, and the project-level AK the model gateway wants.

**Current version**: `2026.11.21.9` (date-style `YYYY.MM.DD.N`, see `../CONVENTIONS.md`)

## What it is

Meoo's chat traffic is configured in CPA as an OpenAI-compatible provider
(`api-keys` → `openai-compatibility` → `name: "秒悟"`) carrying a **project-level**
AK (`meoo_project_service_v1.…`). That arrangement has no readout anywhere in CPA's
own UI and cannot have one: the host's quota card is driven by
`/v0/management/auth-files`, and config-declared compatibility credentials are not
listable there. So the plugin owns its own panel — the same decision loomy recorded
in `docs/adr/ADR-0001`.

This plugin supplies the missing numbers:

| reading | where it comes from |
|---|---|
| accounts | the plugin's own store, `%USERPROFILE%\.meoo-cpa\accounts.json` |
| points / plan | `https://meoo.com/open/v1/cli-compat/account/{points,entitlement}` |
| project | `…/cli-compat/projects` → `…/{url_id}/cloud` → `…/cloud/environment` |
| project-level AK | `…/cloud/environment` → `meooProjectApiKey` |
| model catalogue | `https://api.meoo.host/meoo-ai/compatible-mode/v1/models` |
| **daily 2,000-point login reward** | `https://meoo.com/api/mc/entitlement/init` (web session) |

Reads are cached (default 60 minutes) and served to the panel from that cache, so
opening or polling the panel costs no upstream calls. The **刷新数据** button forces
one walk; a background tick does the same on the configured period. Only two calls
write anything upstream (create project / enable cloud), and both are gated by
`auto_provision`.

## The daily 2,000-point login reward

秒悟 grants every user **2,000 points on their first login of the day** (six days
valid). There is no claim button and no claim endpoint — the grant is issued by
`GET /api/mc/entitlement/init`, which the official web app calls on every load.
The open-platform key (`meoo_ak_…`) can read the balance but **calling it does not
log in and does not grant**: an account whose key was polled daily accumulated no
grant, an account that opened the site did.

So the reward needs the **web session cookie**. This plugin obtains it from the
operator's own browser profile on this machine:

```
%USERPROFILE%\.meoo-cpa\edge-profile\<account id>\Default\Network\Cookies
```

Chromium's classic `v10` scheme is unwrapped with the profile's own
`Local State` key (DPAPI, CurrentUser scope — only the Windows user who owns the
profile can read it, which is also why the credential never leaves this machine).
The reader is the one the 灵犀 tree already proved on this box (`sqlite_min.go`,
`chromium_cookie.go`, `dpapi_windows.go`); no new dependency, no cgo.

**What it does not do**: no password is stored, no SMS code is requested, no login
form is automated. The operator logs in once, by hand, in their own browser; the
plugin then reuses that session. A dead session is reported as *需要人工登录* —
never as a claim.

**Sessions expire every 30 days** (measured: a 11-16 login dies 12-16, and daily
calls do not extend it). Meoo's server accepts only a web session for the grant —
the open-platform key answers `API_KEY_ROUTE_FORBIDDEN` on the grant endpoint, an
OAuth token answers `NEED_LOGIN` (both measured) — so "never expires" like
workbuddy's refresh tokens is not on offer. What the plugin does instead:

- **each row shows the session's remaining days** (yellow under 7, red when dead);
- **the 登录 button opens the account's own Edge profile at meoo.com** — one
  click, log in, done; the next sweep picks the session up the same day.

**Which profile belongs to which account** is settled by identity, not by a
maintained table: the open platform reports `account.userId` and the web session
reports `open_id`, and they are the same number for the same person.

**How a claim is judged**: the account's own points ledger, before and after. A new
「登录赠送积分」 batch means the points arrived. The endpoint's own
`dailyLoginGranted` field is recorded but never believed on its own — a grant can
be booked against an overdraft repayment (measured: +2,000 immediately offset by
−472), so the judgement belongs on the batches, not on the balance.

Set `daily_login: false` to stop the plugin from reading any browser profile.

**When the browser route cannot reach the session** — Chromium holds its cookie
store locked while it runs, so an open everyday browser may be unreadable — the
operator can paste the session by hand (`POST /plugins/meoo/daily-login/session`,
or the **粘会话** button on each card). The cookie is verified against the
account's identity before it is stored and is never echoed back.

## Two credential families, never interchangeable

| use | credential | host |
|---|---|---|
| accounts / points / projects (this plugin) | user-level `meoo_ak_…` | `https://meoo.com/open/v1` |
| model gateway | project-level `meoo_project_service_v1.…` | `https://api.meoo.host/meoo-ai/compatible-mode/v1` |

Pasting the wrong family is refused **locally**, with the family named — upstream
answers 401 for either mistake, and a 401 would read as "this account is dead".

## Build

This tree has exactly one sanctioned build entry:

```
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/build-ng.ps1
```

or `make build`. The artifact lands in `build-out/meoo-<VERSION>-cshared.dll` and
never in the source root. The script runs, in order: cgo guard → gofmt → panel JS
check → panel style check → module pin check → go vet → go test →
`go build -buildmode=c-shared` → contract gate (artifact present + version stamp
embedded).

## Test

```
make test     # the same gates, test/vet only
make vet
make fmt
```

Manual, with the fleet's pinned toolchain:

```
& 'C:\Program Files\Go\bin\gofmt.exe' -l .      # must print nothing
& 'C:\Program Files\Go\bin\go.exe' vet ./...
& 'C:\Program Files\Go\bin\go.exe' test -count=1 ./...
```

## Release

The only release entry for this fleet is:

```
powershell -NoProfile -ExecutionPolicy Bypass -File D:\CLAW\work\cpa-plugin-updater\gates\ship.ps1 -Only meoo
```

`ship.ps1` owns the whole order: gates → commit → tag → clean rebuild → manifest →
immutable archive → transactional hot swap → live verification. Do not copy the DLL
by hand, do not PATCH `enabled` directly, do not re-use a version number.

## Configuration

```yaml
plugins:
    configs:
        meoo:
            refresh_minutes: 60      # 1..1440, cache TTL and background period
            auto_provision: true     # create a project / enable its cloud when missing
            project_name: CPA-Model-Gateway   # name used when one is created
            daily_login: true        # claim the daily 2,000-point login reward
```

`enabled` is also honoured (default true) but is deliberately not part of the
declared config surface.

## Management API

| method | path | notes |
|---|---|---|
| GET | `/v0/management/plugins/meoo/accounts` | panel data, cache-first (zero upstream inside the window) |
| POST | `/v0/management/plugins/meoo/refresh` | 刷新数据: full walk now |
| POST | `/v0/management/plugins/meoo/accounts/add` | store one user-level key + optional label |
| POST | `/v0/management/plugins/meoo/accounts/remove` | drop one row by id |
| GET | `/v0/management/plugins/meoo/models` | gateway catalogue for the model picker, unusable models marked (cache-first) |
| POST | `/v0/management/plugins/meoo/models/refresh` | 刷新目录: force a catalogue re-walk now |
| POST | `/v0/management/plugins/meoo/daily-login` | 立即领取每日积分: attempt the daily 2,000-point reward (all accounts, or one named by `account_id`) |
| GET | `/v0/management/plugins/meoo/daily-login` | the daily-reward state per account. Zero upstream calls |
| POST | `/v0/management/plugins/meoo/daily-login/session` | store a pasted web-session cookie for one account; verified against that account's identity, never echoed |
| DELETE | `/v0/management/plugins/meoo/daily-login/session` | drop the stored web-session cookie of one account |
| POST | `/v0/management/plugins/meoo/daily-login/login-window` | open the account's own Edge login window at meoo.com (one click before a session expires) |

Panel resource: `/v0/resource/plugins/meoo/panel`. Every account-facing verb answers
one shape: `summary` + `accounts` + `config` + `server_time` + `cache_path`.

The full key is **never** echoed: rows carry `id` (sha256 prefix), `key_tail` and
`project_ak_tail` (last four characters each).

## Files

| path | contents |
|---|---|
| `main.go` | C ABI exports, method dispatch, registration, background refresh, envelopes |
| `config.go` | YAML parsing (base64 / string / nested block), defaults and clamps |
| `accounts.go` | the account store: atomic write, key validation, tails and ids |
| `upstream.go` | the open platform + gateway clients, the discovery chain, redaction |
| `daily_login.go` | the daily 2,000-point reward: the grant call, the ledger judgement |
| `daily_login_store.go` | where the reward outcome is parked, and who runs the sweep |
| `session_cookie.go` | the web-session cookie: the browser profile, the reader |
| `sqlite_min.go` | a tiny read-only SQLite b-tree reader (no cgo, no driver) |
| `chromium_cookie.go` | Chromium v10 cookie decryption |
| `dpapi_windows.go` | the Windows DPAPI half of that decryption |
| `cache.go` | TTL cache, sweep, gateway-catalogue cache, panel view model |
| `management.go` | route table, dispatch, payload builders, health |
| `panel.go` | `//go:embed panel.html`, door regex, raw-HTML response |
| `abi_guard.go` | panic containment and the 256 MiB length gate at the C boundary |
| `goroutine_guard.go` | the only sanctioned way to start a background goroutine |
| `docs/adr/` | architecture decision records |
| `docs/deploy/` | deploy notes and rollback records |
| `docs/reports/` | audit / delivery reports |
| `scripts/` | build and verification scripts |
| `build-out/` | build outputs (gitignored) |

Root holds only `*.go`, `go.mod`/`go.sum`, `VERSION`, `CHANGELOG.md`, `LICENSE`,
`README.md`, `Makefile`, `.gitignore`, `.gitattributes`.
