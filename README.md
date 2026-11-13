# meoo

CPA (CLIProxyAPI) plugin — a read-only 秒悟 (Meoo) panel: points, plan, the project
each key owns, and the project-level AK the model gateway wants.

**Current version**: `2026.11.13.2` (date-style `YYYY.MM.DD.N`, see `../CONVENTIONS.md`)

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

Reads are cached (default 60 minutes) and served to the panel from that cache, so
opening or polling the panel costs no upstream calls. The **刷新数据** button forces
one walk; a background tick does the same on the configured period. Only two calls
write anything upstream (create project / enable cloud), and both are gated by
`auto_provision`.

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
```

`enabled` is also honoured (default true) but is deliberately not part of the
declared config surface.

## Management API

| method | path | notes |
|---|---|---|
| GET | `/v0/management/plugins/meoo/accounts` | panel data, cache-first (zero upstream inside the window) |
| POST | `/v0/management/plugins/meoo/refresh` | 刷新数据: full walk now |
| GET | `/v0/management/plugins/meoo/usage` | the same payload, cache-only |
| POST | `/v0/management/plugins/meoo/accounts/add` | store one user-level key + optional label |
| POST | `/v0/management/plugins/meoo/accounts/remove` | drop one row by id |
| GET | `/v0/management/plugins/meoo/models` | gateway catalogue, unusable models marked |
| POST | `/v0/management/plugins/meoo/cache/reset` | drop cached readings + catalogue |
| GET | `/v0/management/plugins/meoo/health` | version, ABI latch, window, paths |

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
