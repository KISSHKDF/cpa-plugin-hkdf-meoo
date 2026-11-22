package main

import (
	"math"
	"testing"
	"time"
)

// TestBloodBarIdentityAndGuards is the regression lock for the 2026.11.22.4
// blood-bar alignment (workbuddy 2026.10.25.1, operator ruling: 「但凡能对齐
// workbuddy 的严格对齐」). It pins the four lies the blood bar must never tell:
//
//	① identity: total_hp − damage_taken == available, per account, always;
//	② a claim that landed BEFORE today's first reading still counts as 今日进账
//	   (the day boundary is midnight, not the panel's first read — 2026-11-22
//	   operator ruling after the +10,000 vs +14,000 incident);
//	③ today_first base never stacks the ledger face value (the first reading
//	   already contains the dawn grant; stacking would double-count it);
//	④ a rolled-over overnight balance never produces a negative buff.
func TestBloodBarIdentityAndGuards(t *testing.T) {
	isolateMeoo(t)

	yesterday := previousDayOf(time.Now())

	t.Run("昨收盘+台账面额：identity 成立且黄段取台账", func(t *testing.T) {
		resetCacheForTest()
		prev := &accountSnapshot{
			HasNumbers: true, Available: 1000, Consumed: 500,
			DayKey: yesterday, DayStartConsumed: 400, DayStartAvailable: 1100,
			PrevCloseDay: yesterday, PrevCloseAvailable: 1000,
		}
		cacheEntries["a1"] = prev
		cacheLoaded = true

		now := time.Now()
		result := accountRead{
			Outcome: outcomeOK, HasNumbers: true,
			Available: ptrF(3000), Granted: ptrF(4000), Consumed: ptrF(500),
			LoginGrantToday: 2000,
		}
		next := applyResult(accountRecord{ID: "a1"}, result, now)

		base := next.PrevCloseAvailable
		total := base + next.BuffToday
		damage := total - next.Available
		if math.Abs(next.BuffToday-2000) > 0.001 {
			t.Fatalf("buff = %v, want 2000 (ledger evidence must survive a pre-first-read claim)", next.BuffToday)
		}
		if next.BuffSource != "yesterday_close" {
			t.Fatalf("base source = %q, want yesterday_close", next.BuffSource)
		}
		if total-next.Available < -0.001 || damage < -0.001 {
			t.Fatalf("identity broken: total %v − damage %v != available %v", total, damage, next.Available)
		}
		if next.Available > total {
			t.Fatalf("remaining %v > total_hp %v — the bar cannot draw that much blood", next.Available, total)
		}
	})

	t.Run("today_first 不得叠台账面额（防双计）", func(t *testing.T) {
		resetCacheForTest()
		// No previous day data at all: the first reading IS the base.
		now := time.Now()
		result := accountRead{
			Outcome: outcomeOK, HasNumbers: true,
			Available: ptrF(3000), Granted: ptrF(4000), Consumed: ptrF(500),
			LoginGrantToday: 2000, // dawn grant, already INSIDE the 3000
		}
		next := applyResult(accountRecord{ID: "a2"}, result, now)

		if next.BuffSource != "today_first" {
			t.Fatalf("base source = %q, want today_first (no yesterday close exists)", next.BuffSource)
		}
		if next.BuffToday > 0.001 {
			t.Fatalf("buff = %v, want 0: stacking the ledger face value on a today_first base double-counts the dawn grant", next.BuffToday)
		}
		total := next.DayStartAvailable + next.BuffToday
		if next.Available > total+0.001 {
			t.Fatalf("remaining %v > total_hp %v", next.Available, total)
		}
	})

	t.Run("昨夜余额不升反降：黄段不为负", func(t *testing.T) {
		resetCacheForTest()
		prev := &accountSnapshot{
			HasNumbers: true, Available: 1500, Consumed: 500,
			DayKey: yesterday, DayStartConsumed: 400, DayStartAvailable: 1600,
			PrevCloseDay: yesterday, PrevCloseAvailable: 1500,
		}
		cacheEntries["a3"] = prev
		cacheLoaded = true

		now := time.Now()
		// Today the upstream rolled its cycle: balance dropped, no grants.
		result := accountRead{
			Outcome: outcomeOK, HasNumbers: true,
			Available: ptrF(1200), Granted: ptrF(4000), Consumed: ptrF(500),
			LoginGrantToday: 0,
		}
		next := applyResult(accountRecord{ID: "a3"}, result, now)

		if next.BuffToday < 0 {
			t.Fatalf("buff = %v, want >= 0: a rolled-over overnight loss must not become a negative segment", next.BuffToday)
		}
		base := next.PrevCloseAvailable
		total := base + next.BuffToday
		damage := total - next.Available
		if damage < -0.001 {
			t.Fatalf("damage = %v, want >= 0", damage)
		}
		if next.Available > total+0.001 {
			t.Fatalf("remaining %v > total_hp %v — the identity must hold even after a roll", next.Available, total)
		}
	})
}

func ptrF(v float64) *float64 { return &v }
