# Makefile - uniform command entry. The real build logic lives in
# scripts/build-ng.ps1 (the ONLY sanctioned build entry, see CONVENTIONS.md).
# 2026-10-23 directory normalisation: every tree shares the same targets.
.PHONY: all build test vet fmt check clean

PS    ?= powershell
BUILD ?= scripts/build-ng.ps1
# Gate location: resolved relative to the workspace root (../cpa-plugin-updater),
# overridable. A hardcoded absolute host path would bind `make check` to one
# machine's directory layout and fail with "file not found" (which reads like
# "gate failed") after any move.
CPA_OPS ?= ../cpa-plugin-updater
GATE    ?= $(CPA_OPS)/gates/release-check.ps1

all: build

build:
	$(PS) -NoProfile -ExecutionPolicy Bypass -File $(BUILD)

test:
	$(PS) -NoProfile -ExecutionPolicy Bypass -File $(BUILD) -TestOnly

vet:
	go vet ./...

fmt:
	gofmt -l .

check:
	@if (-not (Test-Path $(GATE))) { @echo GATE not found at $(GATE) - set CPA_OPS to the cpa-plugin-updater directory & exit 1 }
	$(PS) -NoProfile -ExecutionPolicy Bypass -File $(GATE) -Only meoo

clean:
	@if (Test-Path build-out) { Remove-Item build-out -Recurse -Force }