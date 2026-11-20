package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// These tests pin the 2026-11-20 fix for byte-slice truncation in this tree.
// Three places used to cut with a BYTE index:
//
//	main.go truncate()        — account label (a human note), identity display
//	                            name, upstream error text, panic details
//	config.go clampProjectName — the operator's project name, POSTed upstream
//
// A CJK rune is 3 bytes and an emoji 4, so those cuts regularly landed inside a
// rune; the orphan tail became U+FFFD at JSON serialization and reached the panel
// card or the upstream create-project call.

// TestTruncateUTF8BytesNeverSplitsARune is the CJK case. The fixture is built so
// that the byte limit provably lands inside a rune — the self-check below fails
// the test if that ever stops being true, because then it would no longer be
// testing the defect at all.
func TestTruncateUTF8BytesNeverSplitsARune(t *testing.T) {
	runes := []rune("中文备注说明")
	s := strings.Repeat(string(runes), 6) // 24 runes, 72 bytes
	const limit = 26                      // = 8*3 + 2 -> inside the 9th rune

	if utf8.ValidString(s[:limit]) {
		t.Fatalf("fixture does not exercise the defect: %q is already valid UTF-8", s[:limit])
	}

	got := truncateUTF8Bytes(s, limit)

	if len(got) > limit {
		t.Fatalf("truncated string is %d bytes, want <= %d", len(got), limit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("truncated string carries a U+FFFD replacement char: %q", got)
	}
	// Exactly the 8 whole runes that fit; the 9th had to be dropped entirely.
	if want := s[:24]; got != want {
		t.Fatalf("cut = %q, want %q", got, want)
	}
	if n := utf8.RuneCountInString(got); n != 8 {
		t.Fatalf("kept %d runes, want 8", n)
	}
	// The JSON round trip is where the orphan tail used to surface.
	raw, errMarshal := json.Marshal(got)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if strings.ContainsRune(string(raw), utf8.RuneError) {
		t.Fatalf("serialized string carries a U+FFFD: %s", raw)
	}
}

// TestTruncateUTF8BytesNeverSplitsAnEmoji is the 4-byte case: a byte index that
// is fine for every 3-byte rune can still land inside an astral-plane character.
func TestTruncateUTF8BytesNeverSplitsAnEmoji(t *testing.T) {
	const lock = "🔒" // U+1F512, 4 bytes
	s := strings.Repeat(lock, 5)

	for _, tc := range []struct {
		limit int
		want  string
		// naiveBroken says whether s[:limit] itself splits the rune. It must be
		// true for every mid-rune case, otherwise the case proves nothing.
		naiveBroken bool
	}{
		{6, lock, true},        // 4 + 2 -> inside the 2nd emoji, back off to 4
		{5, lock, true},        // 4 + 1 -> inside the 2nd emoji, back off to 4
		{4, lock, false},       // exactly one rune (boundary, not a defect case)
		{9, lock + lock, true}, // 8 + 1 -> inside the 3rd emoji, back off to 8
	} {
		if got, broken := utf8.ValidString(s[:tc.limit]), tc.naiveBroken; got == broken {
			t.Fatalf("fixture mismatch at limit %d: naive slice valid=%v, want broken=%v", tc.limit, got, broken)
		}
		got := truncateUTF8Bytes(s, tc.limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d: not valid UTF-8: %q", tc.limit, got)
		}
		if got != tc.want {
			t.Fatalf("limit %d: got %q want %q", tc.limit, got, tc.want)
		}
	}
	// A bound that lands mid-rune must never leave a partial character behind,
	// and the encoding/json view must agree.
	for _, limit := range []int{1, 2, 3, 5, 6, 7} {
		raw, errMarshal := json.Marshal(truncateUTF8Bytes(s, limit))
		if errMarshal != nil {
			t.Fatalf("limit %d: marshal: %v", limit, errMarshal)
		}
		if strings.ContainsRune(string(raw), utf8.RuneError) {
			t.Fatalf("limit %d: serialized string carries a U+FFFD: %s", limit, raw)
		}
	}
}

// TestTruncateUTF8BytesMatchesNaiveSliceOnASCII pins the no-regression case: for
// ASCII text the safe cut must be byte-for-byte what the old `s[:n]` produced, so
// nothing that was already fine changes.
func TestTruncateUTF8BytesMatchesNaiveSliceOnASCII(t *testing.T) {
	ascii := "code=401 upstream refused the user key, retry later"
	for _, limit := range []int{0, 1, 4, 17, len(ascii) - 1, len(ascii)} {
		if got, want := truncateUTF8Bytes(ascii, limit), ascii[:limit]; got != want {
			t.Fatalf("ASCII cut at %d changed: got %q want %q", limit, got, want)
		}
	}
}

// TestTruncateUTF8BytesEdgeCases covers the boundaries the helper promises: an
// empty result for a non-positive limit, a pass-through below the limit, an exact
// boundary keeping the rune, a mid-rune limit dropping it entirely, and — for a
// limit that is at least one maximal rune (utf8.UTFMax) — never returning "" when
// there was a whole character to keep.
func TestTruncateUTF8BytesEdgeCases(t *testing.T) {
	if got := truncateUTF8Bytes("", 10); got != "" {
		t.Fatalf("empty input must stay empty, got %q", got)
	}
	if got := truncateUTF8Bytes("abc", 0); got != "" {
		t.Fatalf("limit 0 must yield empty, got %q", got)
	}
	if got := truncateUTF8Bytes("abc", -1); got != "" {
		t.Fatalf("negative limit must yield empty, got %q", got)
	}
	if got := truncateUTF8Bytes("abc", 10); got != "abc" {
		t.Fatalf("short input must pass through, got %q", got)
	}
	// Exact rune boundary: the rune is kept.
	if got := truncateUTF8Bytes("中文", 6); got != "中文" {
		t.Fatalf("exact boundary must keep the whole rune, got %q", got)
	}
	// Mid-rune: the partial rune is dropped, not truncated.
	if got := truncateUTF8Bytes("中文", 5); got != "中" {
		t.Fatalf("mid-rune cut must drop the partial rune, got %q", got)
	}
	// limit below one maximal rune: a complete character or "" — never a fragment.
	if got := truncateUTF8Bytes("🔒", 3); got != "" {
		t.Fatalf("limit 3 cannot hold a 4-byte rune, want \"\", got %q", got)
	}
	if got := truncateUTF8Bytes("中", 2); got != "" {
		t.Fatalf("limit 2 cannot hold a 3-byte rune, want \"\", got %q", got)
	}
	// limit >= utf8.UTFMax must keep at least one whole character.
	if got := truncateUTF8Bytes("🔒abc", 5); got != "🔒a" {
		t.Fatalf("limit 5 must keep the emoji plus one byte, got %q", got)
	}
	if got := truncateUTF8Bytes("🔒🔒", 4); got != "🔒" {
		t.Fatalf("limit 4 must keep one whole emoji, got %q", got)
	}
}

// TestTruncateNeverEmitsBrokenUTF8 closes the loop at the wrapper every call site
// uses: the account label is a human note and is routinely Chinese.
func TestTruncateNeverEmitsBrokenUTF8(t *testing.T) {
	long := strings.Repeat("账号备注：上游返回了一段中文说明。", 8)
	const limit = 80 // accounts.go bounds the label at 80 bytes

	got := truncate(long, limit)

	if len(got) > limit+len("…") {
		t.Fatalf("bounded string is %d bytes, want <= %d", len(got), limit+len("…"))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("bounded string is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("bounded string carries a U+FFFD: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated string must show that it was truncated: %q", got)
	}
	if !strings.HasPrefix(got, "账号备注") {
		t.Fatalf("the surviving prefix was mangled: %q", got)
	}
	raw, errMarshal := json.Marshal(got)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if strings.ContainsRune(string(raw), utf8.RuneError) {
		t.Fatalf("serialized string carries a U+FFFD: %s", raw)
	}
}

// TestNoteNeverEmitsBrokenUTF8 covers the other real caller of the bounded cut:
// note() redacts and bounds upstream text before it is copied into a card note.
func TestNoteNeverEmitsBrokenUTF8(t *testing.T) {
	long := strings.Repeat("项目级 AK 读取失败：上游拒绝了这个用户级 key。", 10)

	got := note(long)

	if !utf8.ValidString(got) {
		t.Fatalf("note is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("note carries a U+FFFD: %q", got)
	}
	if len(got) > upstreamExcerptLen+len("…") {
		t.Fatalf("note is %d bytes, want <= %d", len(got), upstreamExcerptLen+len("…"))
	}
}

// TestPanicDetailNeverEmitsBrokenUTF8 is a real end-of-chain assertion: a panic
// value is flattened into an error envelope and a log line, and our own panic
// strings are Chinese. The detail used to be cut at byte 240 (abi_guard.go).
//
// The "panic: " prefix is load-bearing: it shifts the phase by 7 bytes, so the
// 240-byte cut lands INSIDE a 3-byte rune (240 is a multiple of 3, so a pure-CJK
// fixture would sit exactly on a boundary and prove nothing).
func TestPanicDetailNeverEmitsBrokenUTF8(t *testing.T) {
	raw := "panic: " + strings.Repeat("上游网关返回了无法解析的中文错误，请检查用户级 key", 40)
	if utf8.ValidString(raw[:240]) {
		t.Fatalf("fixture does not exercise the defect: the naive cut is valid UTF-8")
	}

	detail := panicDetail(raw)

	if len(detail) > 244 {
		t.Fatalf("panic detail is %d bytes, want <= 244", len(detail))
	}
	if !utf8.ValidString(detail) {
		t.Fatalf("panic detail is not valid UTF-8: %q", detail)
	}
	if strings.ContainsRune(detail, utf8.RuneError) {
		t.Fatalf("panic detail carries a U+FFFD: %q", detail)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Fatalf("a truncated detail must show that it was truncated: %q", detail)
	}
}

// TestClampProjectNameNeverSplitsARune pins config.go: the name is POSTed to the
// upstream create-project endpoint, so an invalid-UTF-8 name was sent to the site
// and then persisted there. The 64-BYTE ceiling must stay exactly what it was.
func TestClampProjectNameNeverSplitsARune(t *testing.T) {
	name := strings.Repeat("智能体网关项目", 8) // 7 runes * 8 = 56 runes, 168 bytes
	if len(name) <= projectNameMaxLen {
		t.Fatalf("fixture does not exceed the cap: %d bytes", len(name))
	}
	// The naive cut this replaces would have split a rune: assert that, so the
	// case cannot silently stop covering the defect.
	if utf8.ValidString(name[:projectNameMaxLen]) {
		t.Fatalf("fixture does not exercise the defect: the naive cut is valid UTF-8")
	}

	got := clampProjectName(name)

	if len(got) > projectNameMaxLen {
		t.Fatalf("clamped name is %d bytes, want <= %d", len(got), projectNameMaxLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clamped name is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("clamped name carries a U+FFFD: %q", got)
	}
	if !strings.HasPrefix(name, got) {
		t.Fatalf("clamped name is not a prefix of the input: %q", got)
	}
	// The ceiling itself is unchanged: an over-long ASCII name still fills it
	// exactly (management_test.go pins the same thing through the config path).
	if ascii := strings.Repeat("n", 200); len(clampProjectName(ascii)) != projectNameMaxLen {
		t.Fatalf("an over-long ASCII name must still reach exactly %d bytes, got %d",
			projectNameMaxLen, len(clampProjectName(ascii)))
	}
	// A name that fits is returned untouched.
	if short := "CPA-Model-Gateway"; clampProjectName(short) != short {
		t.Fatalf("a fitting name must pass through: %q", clampProjectName(short))
	}
}
