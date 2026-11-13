# build-ng.ps1 — the ONLY sanctioned way to build the oxion plugin dll.
#
# Template: the cline tree build script (2026-10-13). This tree had NO
# build entry point at all — every build was a bare `go build -buildmode=c-shared`,
# which skips every gate below. Two real incidents justify bringing them back:
#   · building with the system `go` instead of the pinned toolchain produced a
#     pile of "undefined: okEnvelope / hostCall" errors that looked like a broken
#     tree rather than a wrong compiler (2026-10-13);
#   · cline's build gate actually caught an unformatted main.go before deploy,
#     proving these gates are load-bearing, not ceremonial.
#
# Discipline copied from cline (kept, because each one can FAIL the build):
#   - the VERSION file is the single truth; -Version may not invent a stamp
#   - CGO_ENABLED=1 + an explicit CC, or Go silently DROPS main.go (the `import "C"`
#     file carrying the ABI exports) and the build fails as innocent "undefined"s
#   - never let the build fetch a different module version (GOPROXY=off)
#   - no proxy env vars: they silently break module/toolchain resolution
#   - the artifact stays in THIS directory; deploying and restarting CPA is the
#     controller's job, with the operator's go-ahead.
#
# Usage:  powershell -File build-ng.ps1 [-Version 0.2.1-v8.0] [-Out name.dll]
param(
    [string]$Version = "",
    [string]$Out = ""
)
# 2026-10-23 目录规范化：本脚本从树根移到 scripts\，所以 $PSScriptRoot 是 scripts\。
# 树根 = 上一级。所有相对路径都以 $treeRoot 为基准，不再依赖当前工作目录。
$treeRoot = Split-Path $PSScriptRoot -Parent
# 2026-11-06: SHA-256 helper (shared). Get-FileHash is NOT available when this
# script is launched as a separate process (the nested Commands.Utility assembly is
# not materialised in that path) -- see sha256.ps1 for the measurement and the fix.
. (Join-Path (Split-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) -Parent) 'cpa-plugin-updater\gates\sha256.ps1')
if (-not (Test-Path (Join-Path $treeRoot 'VERSION'))) { throw "treeRoot 推导失败：$treeRoot 下没有 VERSION" }
$ErrorActionPreference = 'Stop'
$root = $treeRoot
# ---- version: the VERSION file is the single truth --------------------------
$versionFile = (Get-Content (Join-Path $root 'VERSION') -Raw).Trim()
if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = $versionFile
} elseif ($Version -ne $versionFile) {
    throw "-Version '$Version' disagrees with VERSION '$versionFile'. Edit VERSION first (no BOM, no trailing space) and retry."
}
if ([string]::IsNullOrWhiteSpace($Out)) { $Out = "build-out\oxion-$Version-cshared.dll" }

# ---- toolchain --------------------------------------------------------------
$go = 'C:\Users\xbd\tools\go\go\bin\go.exe'
if (-not (Test-Path $go)) { throw "go.exe not found at $go" }
$gofmtBin = Join-Path (Split-Path $go -Parent) 'gofmt.exe'
$cc = 'C:\Users\xbd\tools\mingw64\bin\gcc.exe'
if (-not (Test-Path $cc)) { throw "C compiler not found at $cc — cgo cannot build main.go without it" }
$env:CGO_ENABLED = '1'
$env:CC = $cc
$env:GOPROXY = 'off'
$env:GOFLAGS = '-mod=mod'
foreach ($n in 'HTTP_PROXY','HTTPS_PROXY','http_proxy','https_proxy','ALL_PROXY','all_proxy') {
    Remove-Item "Env:$n" -ErrorAction SilentlyContinue
}

Push-Location $root
try {
    Write-Host "[0/6] cgo guard: main.go must actually be compiled"
    $cgoList = & $go list -f '{{range .CgoFiles}}[{{.}}]{{end}}' .
    if ($cgoList -notmatch '\[main\.go\]') {
        throw "main.go is NOT in CgoFiles (got: '$cgoList') — cgo is off, the C ABI exports would be missing. CGO_ENABLED=$($env:CGO_ENABLED) CC=$($env:CC)"
    }
    if ((& $go list -f '{{.Name}}' .) -ne 'main') { throw "package is not named main" }
    Write-Host "  CgoFiles=$cgoList  CGO_ENABLED=$($env:CGO_ENABLED) CC=$($env:CC)"

    Write-Host "[1/6] gofmt (must be clean in-tree)"
    if (Test-Path $gofmtBin) {
        $flagged = @(& $gofmtBin -l . 2>$null)
        Write-Host "  flagged here: $($flagged.Count)"
        if ($flagged.Count -gt 0) {
            $flagged | ForEach-Object { Write-Host "    $_" }
            throw "gofmt -l flags files in this tree - format them first"
        }
    }

    # Panel JS is an embedded string: gofmt/vet/test/build never look inside it.
    # A syntax error there blanks the entire page while every Go gate stays green
    # (hit twice across two trees - once by a shell replacement, once by a
    # template literal eating regex backslashes). Run a real engine over it.
    Write-Host "[2a/6] panel JS: every <script> block must parse"
    $panelCheck = Join-Path $root 'scripts\panel-js-check.mjs'
    if (-not (Test-Path $panelCheck)) { throw "panel-js-check.mjs is missing - the panel would ship unchecked" }
    & node $panelCheck
    if ($LASTEXITCODE -ne 0) { throw "panel JS check failed - the page would ship blank (see the FAIL lines above)" }
    # JS gates cannot see CSS. A bare `input{...}` rule once turned every checkbox
    # in the model list into a 220px bordered square - the page parsed fine and
    # looked broken. Check the style contract too.
    $styleCheck = Join-Path $root 'scripts\panel-style-check.mjs'
    if (-not (Test-Path $styleCheck)) { throw "panel-style-check.mjs is missing" }
    & node $styleCheck
    if ($LASTEXITCODE -ne 0) { throw "panel style check failed - the panel would render wrong (see the FAIL lines above)" }

    Write-Host "[2/6] pinned module version must not drift"
    $gomod = Get-Content (Join-Path $root 'go.mod') -Raw
    if ($gomod -notmatch 'router-for-me/CLIProxyAPI/v8 v8\.0\.16') {
        throw "go.mod no longer pins CLIProxyAPI v8.0.16 - review before building"
    }
    Write-Host "  pinned: require github.com/router-for-me/CLIProxyAPI/v8 v8.0.16"

    Write-Host "[3/6] go vet ./..."
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed" }

    Write-Host "[4/6] go test -count=1 ./... (excluded_models, redirect guard, refresh singleflight, quota)"
    & $go test -count=1 -timeout 300s ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed" }

    Write-Host "[5/6] go build -buildmode=c-shared"
    # Observed on 2026-10-13: the link step intermittently fails with
    #   ld.exe: export_file.def:1: syntax error
    #   linker script file '...export_file.def (...fix_debug_gdb_scripts.ld)'
    #     appears multiple times
    # while the very same `go build` invoked by hand from the same shell,
    # repeatedly, always succeeds. Every environment factor checked (go version,
    # CC, CGO_ENABLED, GOPROXY/GOFLAGS, proxy vars, cwd, relative vs absolute
    # -o, disk space) is identical between the two paths, so the cause is not
    # understood. Retrying once is a pragmatic guard against a transient link
    # failure; if it persists the build still fails loudly and nothing ships.
    # Build to a SHORT name, then rename. This is not cosmetic.
#
# Root cause found 2026-10-13 by bisection: with a second dot in the -o name the
# link step fails EVERY time, reproducibly, on any package:
#     a-v1.dll      -> exit 0
#     a-v1.0.dll    -> exit 1   ld.exe: export_file.def:1: syntax error
#     a-v1.0.0.dll  -> exit 1
# The linker mis-parses the generated export file when the output name carries
# two dots (it looks like a version triple and confuses the .def handling).
# The host-deployed artifacts are named oxion-v0.2.1.dll — also two dots —
# which is exactly why they must be produced by renaming, never by -o.
# This is the same rule the project already follows for cline ("short -o name"),
# now with the mechanism understood rather than merely remembered.
New-Item -ItemType Directory -Force -Path (Join-Path $root 'build-out') | Out-Null
$shortName = "build-out\oxion.dll"
$shortPath = Join-Path $root $shortName
$finalPath = Join-Path $root $Out

$built = $false
for ($attempt = 1; $attempt -le 2; $attempt++) {
    & $go build -buildmode=c-shared -trimpath -buildvcs=false -o $shortName .
    if ($LASTEXITCODE -eq 0) { $built = $true; break }
    if ($attempt -lt 2) {
        Write-Host "  build failed (attempt $attempt); retrying once"
        Start-Sleep -Seconds 3
    }
}
if (-not $built) { throw "go build failed twice — refusing to ship a half-linked artifact" }
Move-Item -Path $shortPath -Destination $finalPath -Force
$headerPath = [System.IO.Path]::ChangeExtension($shortName, '.h')
if (Test-Path (Join-Path $root $headerPath)) {
    Move-Item -Path (Join-Path $root $headerPath) -Destination ([System.IO.Path]::ChangeExtension($finalPath, '.h')) -Force
}

    Write-Host "[6/6] contract gate (artifact present + version stamp)"
    $artifact = Join-Path $root $Out
    if (-not (Test-Path $artifact)) { throw "artifact missing: $artifact" }
    $size = (Get-Item $artifact).Length
    $sha = (Get-Sha256Hex $artifact)

    # The stamp must be embedded in the artifact, not merely declared in source:
    # a DLL that self-reports the wrong version makes the management plane lie.
    $bytes = [System.IO.File]::ReadAllBytes($artifact)
    $ascii = [System.Text.Encoding]::ASCII.GetString($bytes)
    if ($ascii -notmatch [regex]::Escape($Version)) {
        throw "version stamp '$Version' not found inside $Out - the artifact would misreport its version"
    }

    Write-Host "OK $Out"
    Write-Host "  size=$size sha256=$sha"
    Write-Host "  version=$Version (VERSION file == embedded stamp)"
    Write-Host ""
    Write-Host "[done] artifact : $artifact"
    Write-Host "       NOTE: deploy + CPA restart are the controller's steps, not this script's."
} finally {
    Pop-Location
}