// goroutine_guard.go — panic containment for background goroutines.
//
// WHY THIS FILE EXISTS (audit 2026-10-19, finding H2):
//
//	The recover in abi_guard.go only covers the goroutine that the host calls into.
//	Go aborts the whole process when ANY goroutine panics without a recover, so a
//	panic in a background loop is just as fatal as one at the C boundary — and the
//	ABI guard cannot see it.
//	This tree starts its background loops only through safeGo/safeCall (pinned by
//	goroutine_guard_test.go). This file is the documented way to start one.
//
// This deliberately does NOT swallow the panic silently: it logs with a stack so
// the defect is still findable. Swallowing would trade a loud crash for a quiet
// wrong answer, which is worse.

package main

import (
	"fmt"
	"log"
	"runtime/debug"
)

// safeGo runs fn on a new goroutine with a panic guard.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("%s: recovered panic in background goroutine %q: %s", providerName, name, panicDetail(r))
				log.Printf("%s: goroutine %q stack:\n%s", providerName, name, string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// safeGoErr is safeGo for the common "returns an error" shape.
func safeGoErr(name string, fn func() error) {
	safeGo(name, func() {
		if err := fn(); err != nil {
			log.Printf("%s: background goroutine %q ended with error: %v", providerName, name, err)
		}
	})
}

// safeCall runs fn inline (same goroutine) with a panic guard.
func safeCall(name string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered panic in %q: %s", providerName, name, panicDetail(r))
			ok = false
		}
	}()
	fn()
	return true
}

// safeCallErr is safeCall for the error-returning shape.
func safeCallErr(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered panic in %q: %s", providerName, name, panicDetail(r))
			err = fmt.Errorf("%s: recovered panic: %s", name, panicDetail(r))
		}
	}()
	return fn()
}
