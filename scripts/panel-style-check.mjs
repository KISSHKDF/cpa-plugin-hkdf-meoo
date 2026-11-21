// panel-style-check.mjs — meoo 面板的样式 + 骨架契约门（构建门；build-ng.ps1 无参调用）。
//
// ---------------------------------------------------------------------------
// 为什么有这道门
// ---------------------------------------------------------------------------
//
// 本文件原本是 oxion 的拷贝：它断言的是 OXION 自己的形状（`.mg-item
// input[type="checkbox"]`、`modelCtlCard`、`btnModelSave`、`btnModelAll`、
// `btnModelNone`、`mg-group`）。那种门在本树里永远不会变绿，只会教会所有人无视它
// （lingxi 的同类门就是这么写在自己的注释里的）。2026-11-13 把它改成 meoo 面板的
// 真实契约，同时保留它真正通用的那两条护栏：
//
//   1. 禁裸 `input{...}` 规则 —— 一条写给文本框的 `input{flex:1;min-width:220px}`
//      会同时命中每一个 checkbox，把列表撑成一格一个的巨型方块（用户真见过的样子；
//      页面解析完全正常，只是看起来很崩，所以 JS 语法门看不见它）；
//   2. checkbox 必须有显式宽高 —— 没有它，下一条裸 input 规则会再次把开关撑爆。
//
// 新增的第三条护栏来自面板 JS 的绑定形状：markup 里必须真的存在 JS 去取的 id，
// 否则绑定抛错、其后所有按钮失效（"点了没反应"这类事故的常见形态）。
//
// 用法： node scripts/panel-style-check.mjs        （0 = 干净，1 = 违约）
//        从插件树根跑；树根相对路径，理由与 panel-js-check.mjs 相同：写死别的插件树的
//        绝对路径曾让这道门去校验那棵树并报 OK，而本树的面板根本没被检查。
import { readFileSync } from 'node:fs';

const P = process.argv[2] || 'panel.html';
const html = readFileSync(P, 'utf8');

let fail = 0;
const failMsg = (m) => { console.error('FAIL ' + m); fail++; };

// ---------------------------------------------------------------------------
// 0. CSS 块扫描（选择器 → 声明），后面几条护栏共用
// ---------------------------------------------------------------------------
// 只有 <style> 里的东西算 CSS：把 JS 模板里的字符串也当样式读，会得到假的命中。
const styleMatch = html.match(/<style[^>]*>([\s\S]*?)<\/style>/i);
if (!styleMatch) {
  console.error('FAIL 面板里没有内联 <style> 块');
  process.exit(1);
}
const css = styleMatch[1];
// 注释先清掉：选择器扫描是"取到上一个 } 为止"，留着注释会把 /* ... */ 也算进选择器，
// 于是 `.bar{...}` 永远匹配不上 `.bar`（这条门自己踩过一次）。
const cssClean = css.replace(/\/\*[\s\S]*?\*\//g, '');
const cssBlocks = [];
for (const m of cssClean.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
  cssBlocks.push({ selector: m[1].trim(), body: m[2] });
}

// ---------------------------------------------------------------------------
// 1. 裸元素级 input 规则（会命中 checkbox）
// ---------------------------------------------------------------------------
{
  const bare = /(^|\n)\s*input\s*\{/m;
  if (bare.test(html)) {
    failMsg('存在裸 `input{...}` 规则：它会同时命中所有 checkbox，把开关撑成一格一个的巨型方块');
  }
  const bareFocus = /(^|\n)\s*input:focus\s*\{/m;
  if (bareFocus.test(html)) failMsg('存在裸 `input:focus{...}` 规则（同上）');
}

// ---------------------------------------------------------------------------
// 2. 文本/密码输入框必须带 class（否则只能靠裸 input 规则去写样式）
// ---------------------------------------------------------------------------
{
  const inputs = [...html.matchAll(/<input\b[^>]*>/g)].map((m) => m[0]);
  for (const tag of inputs) {
    const type = (tag.match(/type="([^"]+)"/) || [, 'text'])[1].toLowerCase();
    if (type === 'checkbox' || type === 'radio') continue;
    if (!/\bclass="/.test(tag)) {
      failMsg('文本输入框没有 class（会迫使 CSS 写裸 input 规则，进而污染 checkbox）：' + tag.slice(0, 90));
    }
  }
}

// ---------------------------------------------------------------------------
// 3. checkbox 必须有显式宽高
// ---------------------------------------------------------------------------
{
  const sized = cssBlocks.filter((b) => /input\s*\[type="checkbox"\]/.test(b.selector));
  if (!sized.length) {
    failMsg('没有任何 input[type="checkbox"] 的尺寸护栏：下一条裸 input 规则会再次把开关撑爆');
  } else {
    const decl = sized.map((b) => b.body).join(';');
    if (!/width\s*:/.test(decl) || !/height\s*:/.test(decl)) {
      failMsg('input[type="checkbox"] 的规则没有同时给出 width 与 height：' + decl.trim());
    }
  }
}

// ---------------------------------------------------------------------------
// 4. meoo 面板的真实契约
// ---------------------------------------------------------------------------
// 4a. .bar 必须靠"什么都不写"取得 flex-start 左对齐；任何一条推右的声明都会把
//     主操作按钮顶到右边，而外观上它仍然是"一个按钮"，看不出是哪条规则干的。
{
  const bar = cssBlocks.filter((b) => b.selector === '.bar');
  if (!bar.length) {
    failMsg('找不到 .bar 规则（动作条的左对齐就靠它）');
  } else {
    const body = bar.map((b) => b.body).join('');
    for (const banned of ['justify-content', 'margin-left', 'order:', 'text-align:right']) {
      if (body.includes(banned)) failMsg('.bar 里出现了 ' + banned + ' —— 会把动作按钮从左边推开');
    }
    for (const need of ['display:flex', 'flex-wrap:wrap']) {
      if (!body.includes(need)) failMsg('.bar 缺少 ' + need);
    }
  }
}

// 4b. .bar 的第一个子元素必须是主操作（刷新数据）。
{
  const i = html.indexOf('<div class="bar">');
  const j = i < 0 ? -1 : html.indexOf('</div>', i);
  if (i < 0 || j < 0) {
    failMsg('找不到 <div class="bar">…</div>（动作条）');
  } else {
    const inner = html.slice(i + '<div class="bar">'.length, j).trim();
    if (!inner.startsWith('<button id="refreshBtn"')) {
      failMsg('.bar 的第一个子元素必须是 <button id="refreshBtn"…>（主操作左起第一位），实际：' + inner.slice(0, 90));
    }
    for (const verb of ['刷新数据', '模型', '加账号']) {
      if (!inner.includes(verb)) failMsg('动作条缺少动词：' + verb);
    }
  }
}

// 4c. 筛选行必须是「筛选签 → 细分隔线 → 全局开关」。顺序反了就成了"开关在筛选左边"，
//     而视觉上两侧都只是胶囊，看不出谁是谁。
{
  const open = '<div class="filter-bar" id="filterBar">';
  const i = html.indexOf(open);
  const j = i < 0 ? -1 : html.indexOf('</div>', i);
  if (i < 0 || j < 0) {
    failMsg('找不到 ' + open + '…</div>（筛选行）');
  } else {
    const inner = html.slice(i + open.length, j);
    const tag = inner.indexOf('class="ftag active"');
    const sep = inner.indexOf('class="filter-sep"');
    const auto = inner.indexOf('class="toggle auto-checkin"');
    if (!(tag >= 0 && sep >= 0 && auto >= 0 && tag < sep && sep < auto)) {
      failMsg('筛选行必须是「筛选签 → 细分隔线 → 全局开关」，实际 tag=' + tag + ' sep=' + sep + ' auto=' + auto);
    }
    if (!inner.includes('<span class="cnt"')) failMsg('筛选签里没有计数徽标 <span class="cnt"');
  }
}

// 4d. house 的卡片/进度条骨架必须有；账号卡的网格是 house 的 440px 版式。
{
  for (const needle of [
    'grid-template-columns:repeat(auto-fill,minmax(440px,1fr))',
    'class="pb-track"',
    'class="cd-card',
    '<div class="summary card" id="summaryBox"',
  ]) {
    if (!html.includes(needle)) failMsg('面板缺少 house 骨架：' + needle);
  }
}

// 4e. 状态胶囊三个变体必须各自独立成规则 —— 只写 .cd-pill 的话，失效账号会画成
//     "在读"的样子（浏览器不报错、不告警，只是不画）。
{
  for (const variant of ['.cd-pill.ok{', '.cd-pill.dead{', '.cd-pill.stale{']) {
    if (!css.includes(variant)) failMsg('缺少独立规则 ' + variant + '（状态胶囊会退化成同一种颜色）');
  }
}

// 4f. meoo 特有的两条内容契约：CPA 交叉核对 + 不可用模型的显式标注。
{
  if (!html.includes('/config/api-keys/openai-compatibility')) {
    failMsg('面板不再做 CPA 交叉核对（/config/api-keys/openai-compatibility）：「已在 CPA / 未挂进 CPA」会永远显示未知');
  }
  for (const needle of ['已在 CPA', '未挂进 CPA']) {
    if (!html.includes(needle)) failMsg('面板缺少交叉核对结论：' + needle);
  }
  if (!html.includes('不可用（Token Plan only）')) {
    failMsg('面板不再标注 unusable 的模型（qwen4-max-preview 这类会看起来像可用通道）');
  }
}

// 4g. 反向锁：本门以前断言的是 oxion 的骨架，那些 id 不许回到 meoo 的面板里。
//     它们一旦出现，说明又是一次跨树拷贝，而本门随后断言的东西可能已经与本树无关。
{
  for (const oxion of ['mg-item', 'modelCtlCard', 'btnModelSave', 'btnModelAll', 'btnModelNone', 'mg-group']) {
    if (html.includes(oxion)) failMsg('meoo 面板里出现了 oxion 的骨架名：' + oxion + '（跨树拷贝残留）');
  }
}

// ---------------------------------------------------------------------------
// 5. JS 去取的 id 必须在 markup 里真的存在
// ---------------------------------------------------------------------------
// 语法门只证明脚本能解析；一个拼错的 id 会让绑定抛错，其后所有按钮失效——这正是
// "导入按钮点了没反应"那类报障的形态。
{
  const declared = new Set();
  for (const m of html.matchAll(/id="([A-Za-z0-9_-]+)"/g)) declared.add(m[1]);
  const referenced = new Set();
  for (const m of html.matchAll(/getElementById\(\s*['"]([A-Za-z0-9_-]+)['"]\s*\)/g)) referenced.add(m[1]);
  for (const m of html.matchAll(/\$\(\s*['"]([A-Za-z0-9_-]+)['"]\s*\)/g)) referenced.add(m[1]);
  const dangling = [...referenced].filter((id) => !declared.has(id));
  if (dangling.length) {
    failMsg('脚本引用了 markup 里不存在的 id（绑定会抛错，其后所有按钮全哑）：' + dangling.join(' | '));
  }
  // 反向：动作条与筛选行的关键 id 必须在，否则上面的形状断言只是"恰好还有字"。
  for (const id of ['refreshBtn', 'filterBar', 'summaryBox', 'grid', 'toasts', 'modalMask', 'keyInput', 'autoPoll']) {
    if (!declared.has(id)) failMsg('面板缺少关键 id：' + id);
  }
}

if (fail) {
  console.error(`\n${fail} 项样式契约不满足`);
  process.exit(1);
}
console.log('OK panel style: 无裸 input 规则；文本输入框都有 class；checkbox 有显式宽高；'
  + '动作条左对齐且主操作在首位；house 骨架与状态胶囊变体齐全；'
  + 'CPA 交叉核对与不可用模型标注在位；JS 取用的 id 全部存在');
