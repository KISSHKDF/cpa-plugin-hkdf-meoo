package main

import (
	"regexp"
	"strings"
	"testing"
)

// 面板头部布局守卫（loomy/panel_layout_test.go 的移植，2026-11-13 换成 meoo 的面板常量）。
//
// 旧版头部的缺陷是结构性的，不是配色问题：标题块本身是一个 flex 行，里面放了
// `.grow{flex:1}` 当弹簧，把唯一的按钮顶到了最右边。所以本守卫不看颜色，只钉住
// **让按钮落在左边的那几个结构事实**，任何一条被改回去都会 FAIL：
//
//  1. `.bar` 里不许出现 justify-content / margin-left / order: / text-align:right ——
//     house reference (workbuddy/panel.html) 靠"什么都不写"取得 flex-start 左对齐；
//     这两者任一出现就会把按钮推回右侧，正是用户抱怨的那个样子。
//  2. #refreshBtn 必须是 `.bar` 的第一个子元素（主操作在左起第一位）。
//  3. 不再存在 `.grow` 弹簧（旧布局的病灶）。
//  4. 筛选胶囊行存在，且顺序是「筛选签 → 细分隔线 → 全局开关」。
//  5. 主题必须跟随宿主（data-theme 镜像 + :root 亮色默认），不许写死深色。
//  6. 卡片网格用 house 的 minmax(440px)，汇总卡用 .summary/.pb-*。
//
// meoo 特有：动作条上的六个动词、筛选行里的「已在 CPA / 未挂进 CPA」（面板侧交叉
// 核对的结果），以及状态胶囊三个变体的独立规则，都在这里一并钉住。
func TestPanelHeaderFollowsHouseLayout(t *testing.T) {
	html := string(panelHTML)

	// (1) .bar 的声明块里不许有 justify-content / margin-left / order / text-align:right。
	barCSS := declBlock(t, html, ".bar{")
	for _, banned := range []string{"justify-content", "margin-left", "order:", "text-align:right"} {
		if strings.Contains(barCSS, banned) {
			t.Fatalf(".bar 里出现了 %q —— 会把按钮从左边推开（house reference 靠 flex-start 左对齐）：%s", banned, barCSS)
		}
	}
	if !strings.Contains(barCSS, "display:flex") || !strings.Contains(barCSS, "flex-wrap:wrap") {
		t.Fatalf(".bar 的 flex 布局被改动：%s", barCSS)
	}

	// (2) #refreshBtn 是 .bar 第一个子元素。
	barHTML := elementInnerHTML(t, html, `<div class="bar">`)
	firstTag := strings.TrimSpace(barHTML)
	if !strings.HasPrefix(firstTag, `<button id="refreshBtn"`) {
		t.Fatalf(".bar 的第一个子元素必须是刷新数据按钮（主操作左起第一位），实际开头：%.90q", firstTag)
	}
	if strings.Contains(html, `id="refreshBtn"`) == false {
		t.Fatal("刷新数据按钮不见了")
	}
	// meoo 的四个动词都必须挂在动作条上（少一个就是少一条管理路径；
	// 读缓存 2026.11.21.4、诊断/清空缓存 2026.11.21.5 起移除——主人裁定冗余）。
	for _, action := range []string{"刷新数据", "模型", "加账号"} {
		if !strings.Contains(barHTML, action) {
			t.Errorf("动作条缺少动词 %q", action)
		}
	}

	// (3) 旧布局的弹簧不许回来。
	if strings.Contains(html, `class="grow"`) || strings.Contains(html, ".grow{") {
		t.Fatal("`.grow` 弹簧回来了 —— 它正是把按钮顶到右边的原因")
	}

	// (4) 筛选胶囊行：签 → 分隔线 → 全局开关。
	filterHTML := elementInnerHTML(t, html, `<div class="filter-bar" id="filterBar">`)
	for _, needle := range []string{`class="ftag active"`, `class="filter-sep"`, `class="toggle auto-checkin"`} {
		if !strings.Contains(filterHTML, needle) {
			t.Fatalf("筛选行缺少 %q", needle)
		}
	}
	tagIdx := strings.Index(filterHTML, `class="ftag active"`)
	sepIdx := strings.Index(filterHTML, `class="filter-sep"`)
	autoIdx := strings.Index(filterHTML, `class="toggle auto-checkin"`)
	if !(tagIdx < sepIdx && sepIdx < autoIdx) {
		t.Fatalf("筛选行顺序必须为「筛选签 → 分隔线 → 开关」，实际 tag=%d sep=%d auto=%d", tagIdx, sepIdx, autoIdx)
	}
	// 计数徽标必须挂在签内（数据驱动，不是写死的数字）。
	if !strings.Contains(filterHTML, `<span class="cnt"`) {
		t.Fatal("筛选签里没有计数徽标")
	}
	// 面板侧的交叉核对结果必须有地方筛（已在 CPA / 未挂进 CPA）。
	for _, pill := range []string{"已在 CPA", "未挂进 CPA"} {
		if !strings.Contains(filterHTML, pill) {
			t.Errorf("筛选行缺少交叉核对筛选 %q", pill)
		}
	}

	// (5) 主题跟随宿主：亮色默认 + data-theme 覆盖 + 父窗同步。
	for _, needle := range []string{
		`--bg:#faf9f5`,               // :root 亮色（paper）
		`:root[data-theme="white"]`,  // 亮色变体
		`:root[data-theme="dark"]`,   // 深色变体
		`getAttribute(ATTR)`,         // 读父窗（宿主）的 data-theme
		`window.__lyThemeSync`,       // 启动时补挂同步/观察器
		`prefers-color-scheme: dark`, // 独立打开时的兜底
		`var ATTR="data-theme";`,     // 属性名与宿主一致
		`window.self!==window.top`,   // 只有真被嵌入才读父窗
	} {
		if !strings.Contains(html, needle) {
			t.Fatalf("主题跟随宿主的实现缺少 %q", needle)
		}
	}
	if strings.Contains(html, "color-scheme:light}") && strings.Contains(html, ":root{") && !strings.Contains(html, ":root[data-theme=\"dark\"]") {
		t.Error("面板只剩一套亮色主题，丢了宿主深色跟随")
	}

	// (6) 卡片网格与汇总卡照 house 的尺寸/类名。
	for _, needle := range []string{
		`grid-template-columns:repeat(auto-fill,minmax(440px,1fr))`,
		`<div class="summary card" id="summaryBox"`,
		`class="summary-hd"`,
		`class="pb-track"`,
		`class="cd-card`,
	} {
		if !strings.Contains(html, needle) {
			t.Fatalf("布局缺少 house 元素 %q", needle)
		}
	}

	// 按钮层级：主操作是裸 button，其余是 button.sec（house 的 .sec 变体）。
	for _, needle := range []string{
		`button{background:var(--acc);color:var(--on-acc);border:0;border-radius:8px;padding:8px 14px;`,
		`button.sec{background:var(--btn-sec);color:var(--fg)}`,
	} {
		if !strings.Contains(html, needle) {
			t.Fatalf("按钮层级不符合 house 规范：缺少 %q", needle)
		}
	}
}

// TestPanelClassesUsedByMarkupHaveStyles is the regression lock for the defect the
// house reference paid for: a card markup emitted `<span class="cd-badge intl">`
// while the stylesheet had NO `.cd-badge.intl` rule, so the badge rendered as plain
// body text. A missing rule produces no error and no warning — the browser just
// silently does not paint — and every literal-string assertion in the layout test
// above stayed green. The screenshot is the only thing that caught it.
//
// So: every class this panel's markup (static markup AND the JS templates) writes
// must have a matching CSS rule, and every variant class needs a rule of its own.
//
// meoo 的两个具体教训：
//   - 状态胶囊的 ok / dead / stale 是三条不同语义，必须各有独立规则；
//   - 交叉核对的 ok / no / unknown 同理（`未知` 不能靠 `.cd-badge` 的基础样式）。
func TestPanelClassesUsedByMarkupHaveStyles(t *testing.T) {
	html := string(panelHTML)

	// Classes the renderer emits (or that the static markup carries) and which
	// must therefore have a visible style of their own.
	required := []string{
		// chrome
		"wrap", "sub", "card", "field-input", "bar", "sec", "toggle",
		"filter-bar", "ftag", "active", "cnt", "filter-sep", "auto-checkin",
		"loading", "grid", "spin",
		// summary card
		"summary", "summary-hd", "summary-title", "summary-meta",
		"pb", "pb-label", "pb-track", "pb-bar", "pb-meta", "pb-warn",
		// account card
		"cd-card", "cd-dead", "cd-row1", "cd-name-group", "cd-name", "cd-badge",
		"cd-ops", "cd-pill", "cd-meta", "cd-mi", "cd-lbl", "cd-val",
		"cd-credit-line", "cd-credit", "cd-bar", "cd-seg", "cd-msg",
		// toast + modal + tables
		"toasts", "toast", "modal-mask", "modal", "modal-hd", "modal-x",
		"modal-bd", "modal-ft", "hint", "ut", "num",
	}
	for _, cls := range required {
		// A rule may be written as `.cd-badge{...}` or `.cd-badge .x{...}`; both
		// mean the class is styled. Match the selector token, not a whole line.
		if !regexp.MustCompile(`\.` + regexp.QuoteMeta(cls) + `[^a-zA-Z0-9_-]`).MatchString(html) {
			t.Errorf("markup 用到了 class %q，但样式表里没有对应规则（浏览器会静默不画）", cls)
		}
	}

	// Variant classes must have a rule of their own, not just the base one:
	// `.cd-pill{...}` does not style `.cd-pill.dead`, which is exactly how a badge
	// ships unstyled. Restricted to the `cd-` family (like loomy) because
	// `class="summary card"` is deliberately served by the grouped `.card,.summary`
	// selector; every non-`cd-` variant this panel uses is asserted explicitly below.
	reVariant := regexp.MustCompile(`class="(cd-[a-z]+)((?: [a-z-]+)+)"`)
	for _, m := range reVariant.FindAllStringSubmatch(html, -1) {
		base, rest := m[1], strings.TrimSpace(m[2])
		for _, variant := range strings.Fields(rest) {
			if variant == "" || strings.Contains(variant, "$") {
				continue
			}
			if !strings.Contains(html, "."+base+"."+variant+"{") {
				t.Errorf("代码里出现变体 %q，但没有 .%s.%s 规则（只会继承基础样式，等于没生效）", base+" "+variant, base, variant)
			}
		}
	}

	// The rules the variants depend on, named one by one. Each must be a rule of
	// its OWN (the `{` immediately after), so a grouped selector cannot satisfy it
	// by accident.
	requiredVariants := []string{
		"cd-card.cd-dead",  // 失效账号整卡降透明度
		"cd-pill.ok",       // 在读
		"cd-pill.dead",     // 失效
		"cd-pill.stale",    // 读数过期
		"cd-badge.ok",      // 已在 CPA
		"cd-badge.no",      // 未挂进 CPA
		"cd-badge.unknown", // 未知（宿主配置接口读不到）
		"cd-mi.right",      // 卡内右列
		"cd-val.warn",      // 过期读数标红
		"cd-credit.dim",    // 已耗/总量那行
		"cd-seg.used",      // 进度条的「已耗」段
		"cd-msg.err",       // 错误/过期说明
		"ftag.active",      // 当前筛选
		"pb-meta.pb-warn",  // 汇总卡里的 notes[]
		"modal-mask.show",  // 弹窗可见态
		"toast.show",       // toast 滑入
		"toast.ok",         // toast 三态
		"toast.warn",
		"toast.err",
	}
	for _, variant := range requiredVariants {
		if !strings.Contains(html, "."+variant+"{") {
			t.Errorf("缺少 .%s{ 独立规则（变体不能只靠基础类）", variant)
		}
	}
}

// TestPanelDialogsAreSelfDrawn keeps the native confirm() out of the page: on
// WebView2 a native dialog freezes the whole document, which this fleet has
// already paid for once. meoo's destructive verbs (清空缓存 / 移除) must therefore
// both go through the self-drawn modal, and the add-account form too.
func TestPanelDialogsAreSelfDrawn(t *testing.T) {
	if strings.Contains(string(panelHTML), "confirm(") {
		t.Fatal("面板用了原生 confirm()（WebView2 里会冻住整页）；清空缓存必须走自绘弹窗")
	}
	for _, needle := range []string{
		`class="modal-mask"`,
		`function openModal(`,
		// meoo 特有：另外两个写动作也不能走原生框。
		`function askRemove(`,
		`function askAdd(`,
	} {
		if !strings.Contains(string(panelHTML), needle) {
			t.Fatalf("自绘弹窗缺少 %q", needle)
		}
	}
}

// TestCreditBarsDrainFromTheRight is the regression lock for a user-visible
// direction bug (2026-11-16): the account card drew the USED slice first and the
// REMAINING slice second, so the green bar grew from the left edge as credits
// were spent — it read as "credits are being eaten from the left". Loomy, the
// house reference for this panel family, draws the opposite way: remaining first
// (green, anchored left), used last (grey, hanging on the right edge), so the bar
// shortens from the right. Only the JS template decides that order — both
// segments share one class family, so no string-presence or CSS assertion can
// catch a swap. Hence the positional assertions, plus the summary bar's own
// direction (it used to be a "used progress" bar, i.e. the opposite reading of
// the card bars on the same card).
func TestCreditBarsDrainFromTheRight(t *testing.T) {
	html := string(panelHTML)
	card := sliceBetween(t, html, "function card(a){", "\nfunction renderGrid(")

	const remainSeg = `<span class="cd-seg" style="flex:'`
	const usedSeg = `<span class="cd-seg used" style="flex:'`
	iRemain := strings.Index(card, remainSeg)
	iUsed := strings.Index(card, usedSeg)
	if iRemain < 0 {
		t.Fatalf("账号卡的进度条不再画「剩下」段（%q）", remainSeg)
	}
	if iUsed < 0 {
		t.Fatalf("账号卡的进度条不再画「已耗」段（%q）", usedSeg)
	}
	if iRemain > iUsed {
		t.Fatal("账号卡把「已耗」段画在「剩下」段之前：绿条会从左往右长，看起来像积分从左边被吃掉（loomy 是剩下的贴左、用掉的挂右端）")
	}
	if !strings.Contains(card, "remain.toFixed(2)") || !strings.Contains(card, "used.toFixed(2)") {
		t.Fatalf("进度条的两个段必须分别用 remain / used 的宽度，实际：%s", card)
	}
	// 零宽段不画：used=0 时若仍发一个 flex:0 的灰段，轨道右端会多出 3px 灰边
	//（.cd-seg 有 min-width:3px 的下限），看上去像"已经用掉一点"。
	if !strings.Contains(card, "(remain>0 ?") || !strings.Contains(card, "(used>0 ?") {
		t.Fatal("进度条没有按 >0 过滤零宽段：min-width 会画出假的已耗/剩余边角")
	}

	// 汇总条与卡片条同一口径：宽度 = 可用占比，颜色也按可用占比取。
	if !strings.Contains(html, "总可用 / 总总量") {
		t.Fatal("汇总条的标题不是「总可用 / 总总量」：卡片条已是可用口径，汇总条必须同口径")
	}
	if strings.Contains(html, "总已耗 / 总总量") {
		t.Fatal("汇总条仍在用「总已耗 / 总总量」的已耗进度口径")
	}
	if !strings.Contains(html, "barColor(pct)") || strings.Contains(html, "barColor(100-pct)") {
		t.Fatal("汇总条的颜色仍按「已耗」取（barColor(100-pct)），与卡片条方向相反")
	}

	// loomy 的段宽有 3px 下限；meoo 原来写 0，小额剩余会渲染成空条。
	if !strings.Contains(declBlock(t, html, ".cd-seg{"), "min-width:3px") {
		t.Fatal(".cd-seg 没有 3px 下限（与 loomy 不一致）：小额剩余会渲染成空条")
	}
}

// sliceBetween returns the text between two markers, failing the test when either
// marker is missing (so a renamed function cannot make a positional assertion
// silently pass on an empty string).
func sliceBetween(t *testing.T, s, open, until string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("找不到起点 %q", open)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, until)
	if j < 0 {
		t.Fatalf("起点 %q 之后找不到终点 %q", open, until)
	}
	return rest[:j]
}

// declBlock returns the declaration block that starts with marker ("}." or "};"),
// so a CSS assertion can be scoped to one selector instead of the whole sheet.
func declBlock(t *testing.T, html, marker string) string {
	t.Helper()
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("找不到 CSS 规则 %q", marker)
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("CSS 规则 %q 没有闭合", marker)
	}
	return rest[:j]
}

// elementInnerHTML returns the markup between an opening tag and its first `</div>`,
// which is enough to assert child order in these flat header containers.
func elementInnerHTML(t *testing.T, html, openTag string) string {
	t.Helper()
	i := strings.Index(html, openTag)
	if i < 0 {
		t.Fatalf("找不到容器 %q", openTag)
	}
	rest := html[i+len(openTag):]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		t.Fatalf("容器 %q 没有闭合", openTag)
	}
	return rest[:j]
}
