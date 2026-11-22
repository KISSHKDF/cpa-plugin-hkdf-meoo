package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestBackgroundGoroutinesAllGuarded pins the fleet goroutine discipline at the
// static level: every production `go func(` in non-test files must be the safeGo
// implementation body itself. A panic on an unguarded background goroutine aborts
// the WHOLE CPA process (goroutine_guard.go documents why), so this count is a
// process-liveness gate, not a style preference.
//
// When you add a legitimately new raw `go` statement (e.g. a WaitGroup-bound
// inline recover, as the autoclaw tree does), update expectedRawGoFuncs AND add
// the recover to the new goroutine — then note it here.
func TestBackgroundGoroutinesAllGuarded(t *testing.T) {
	const expectedRawGoFuncs = 1 // goroutine_guard.go safeGo implementation body

	pattern := regexp.MustCompile(`^\s*go func\(`)
	count := 0
	offenders := []string{}

	entries, errRead := os.ReadDir(".")
	if errRead != nil {
		t.Fatalf("read tree: %v", errRead)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		if len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		data, errFile := os.ReadFile(name)
		if errFile != nil {
			t.Fatalf("read %s: %v", name, errFile)
		}
		for i, line := range regexp.MustCompile(`\r?\n`).Split(string(data), -1) {
			if pattern.MatchString(line) {
				count++
				offenders = append(offenders, name+":"+itoa(i+1))
			}
		}
	}
	if count != expectedRawGoFuncs {
		t.Fatalf("raw `go func(` count = %d, want %d (every production goroutine must go through safeGo/safeCall); offenders: %v",
			count, expectedRawGoFuncs, offenders)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
