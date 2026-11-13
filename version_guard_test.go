package main

import (
	"os"
	"strings"
	"testing"
)

// TestVersionLockstepWithVersionFile keeps VERSION, the source stamp and the
// eventual git tag from drifting apart: the release gate checks the same thing,
// and a mismatch means the artifact would self-report a version nobody shipped.
func TestVersionLockstepWithVersionFile(t *testing.T) {
	buf, errRead := os.ReadFile("VERSION")
	if errRead != nil {
		t.Fatalf("read VERSION: %v", errRead)
	}
	if got := strings.TrimSpace(string(buf)); got != version {
		t.Fatalf("VERSION=%q but var version=%q — they must be byte-for-byte equal", got, version)
	}
}

// TestVersionShape enforces the date-epoch convention (YYYY.MM.DD.N) with no
// generation suffix.
func TestVersionShape(t *testing.T) {
	parts := strings.Split(version, ".")
	if len(parts) != 4 {
		t.Fatalf("version %q must have four dot-separated parts (YYYY.MM.DD.N)", version)
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			t.Fatalf("version %q has a non-numeric part %q", version, part)
		}
	}
}

// TestProviderNameIsPluginID pins the id used by routes, config keys and the
// plugin directory: they must all be the same string.
func TestProviderNameIsPluginID(t *testing.T) {
	if providerName != "loomy" {
		t.Fatalf("providerName=%q; the tree, the config block and every route literal say \"loomy\"", providerName)
	}
}
