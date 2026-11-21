package main

// build_entry_test.go — 2026.11.21.9 (workbuddy 对齐批次, audited 2026-11-22):
// source-level pins on scripts/build-ng.ps1's hardened sections, ported from
// workbuddy 2026.11.21.3. The PowerShell entry is what `make build` /
// `make test` and the ship pipeline both run; a silent regression in its param
// block or guards would surface only as a confusing build failure, so the
// contract is pinned where go test can see it. The pins fired for real here:
// this tree's Makefile shipped `make test` → `-TestOnly` while the param block
// never declared the switch — README advertised a command that died on a bogus
// named-parameter error.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildEntrySource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the package directory")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "scripts", "build-ng.ps1"))
	if err != nil {
		t.Fatalf("build-ng.ps1 unreadable: %v", err)
	}
	return string(raw)
}

// TestBuildEntryDeclaresTestOnlyParam: the shared Makefile template's
// `make test` target calls `build-ng.ps1 -TestOnly`. The param block shipped
// without that switch for its whole life, so `make test` died on a bogus
// named-parameter error — the shared-template defect workbuddy fixed in its
// 2026.11.21.3 and this tree inherited by omission.
func TestBuildEntryDeclaresTestOnlyParam(t *testing.T) {
	src := buildEntrySource(t)
	start := strings.Index(src, "param(")
	if start < 0 {
		t.Fatal("param( block not found in build-ng.ps1")
	}
	end := strings.Index(src[start:], "\n)")
	if end < 0 {
		t.Fatal("param( block never closes")
	}
	paramBlock := src[start : start+end]
	if !strings.Contains(paramBlock, "[switch]$TestOnly") {
		t.Fatal("build-ng.ps1 param block does not declare [switch]$TestOnly — `make test` (Makefile → build-ng.ps1 -TestOnly) is broken again")
	}
	// And the switch must actually short-circuit to a test-only run.
	if !strings.Contains(src, "go test -count=1 ./...") {
		t.Fatal("TestOnly branch must run `go test -count=1 ./...` (the same suite ship trusts)")
	}
}

// TestBuildEntryVersionLockstepGuard: a -Version that disagrees with the
// VERSION file used to build an artifact carrying an invented stamp while
// version_guard_test (source var) and this gate's injected-value check both
// stayed green. Two green lights, one lie — the mismatch must throw.
func TestBuildEntryVersionLockstepGuard(t *testing.T) {
	src := buildEntrySource(t)
	if !strings.Contains(src, "disagrees with VERSION") {
		t.Fatal("build-ng.ps1 no longer throws when -Version disagrees with the VERSION file — the double-green-light forgery path is open again")
	}
}

// TestBuildEntryToolchainGuard: a missing gcc under CGO_ENABLED=1 used to
// surface as a pile of undefined symbols (or a silently dropped main.go)
// instead of one clear sentence; the guard must name the compiler path.
func TestBuildEntryToolchainGuard(t *testing.T) {
	src := buildEntrySource(t)
	if !strings.Contains(src, "C compiler not found at") {
		t.Fatal("build-ng.ps1 lost the gcc pre-check — a missing C compiler will again fail as confusing undefined symbols instead of one clear sentence")
	}
}
