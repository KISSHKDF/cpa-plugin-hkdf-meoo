// panel-js-check.mjs — build gate: hand the panel's JavaScript to a real engine.
//
// ---------------------------------------------------------------------------
// Why this gate exists (it is not ceremony)
// ---------------------------------------------------------------------------
//
// The sibling tree hit this on 2026-10-14: a string replacement ate one token in
// the panel HTML, the whole page's JS became a syntax error, and nothing
// rendered. `go build`, `go vet` and `go test` were all green — because none of
// them look inside an embedded string.
//
// This tree hit it again on 2026-10-25, differently: a helper script wrote
//   .match(/^(\/v\d+)\//)
// through a JS template literal, which consumed the backslashes and produced
//   .match(/^(/vd+)//)
// — an invalid regex. The entire main <script> block failed to parse, so EVERY
// button on the panel went dead. The reported symptom was "the import button
// does nothing", which pointed nowhere near the cause.
//
// And the check that was supposed to catch it passed, because it took the FIRST
// <script> block only: this panel has two (a small theme sync before body paint,
// then the main IIFE). So the lesson is twofold:
//
//   1. a syntax check must run every script block, not the first one;
//   2. it must run as a build gate, or the next person re-learns it.
//
// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------
//
//   node scripts/panel-js-check.mjs        (0 = clean, 1 = broken)
//
// Run from the plugin tree root; build-ng.ps1 calls it.
import { readFileSync, writeFileSync, rmSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const ROOT = process.cwd();
const PANEL = join(ROOT, 'panel.html');

let html;
try {
  html = readFileSync(PANEL, 'utf8');
} catch (err) {
  console.error('FAIL 读不到 panel.html：' + err.message);
  process.exit(1);
}

// ---------------------------------------------------------------------------
// 1. Extract EVERY <script> block
// ---------------------------------------------------------------------------
const scripts = [];
{
  let from = 0;
  for (;;) {
    const open = html.indexOf('<script', from);
    if (open < 0) break;
    const openEnd = html.indexOf('>', open);
    if (openEnd < 0) break;
    const close = html.indexOf('</script>', openEnd);
    if (close < 0) break;
    const body = html.slice(openEnd + 1, close);
    // A <script src=...> has no body; skip it rather than "checking" nothing.
    if (body.trim() !== '') scripts.push(body);
    from = close + '</script>'.length;
  }
}
if (!scripts.length) {
  console.error('FAIL 面板里一个内联 <script> 都没有');
  process.exit(1);
}

// ---------------------------------------------------------------------------
// 2. Structural self-check: the render skeleton must be present
// ---------------------------------------------------------------------------
// A script that parses but references a node the HTML never declares still
// throws at load time, and then every later binding is skipped — the same
// "dead button" outcome with no syntax error to point at.
// 2026-11-13 (meoo tree creation): this array used to carry oxion's 18 ids, because
// the script was copied from that tree. A gate that asserts ANOTHER plugin's markup
// can only go green by fabricating ids the panel does not have, i.e. it teaches
// everyone to ignore the gate — the same rot lingxi recorded when it hit this copy,
// and the reason lingxi wrote its own. Replaced with meoo's real skeleton, derived
// from panel.html and measured with this very script:
//   "2 个 <script> 块语法通过，22 个 id，0 个 el() 引用全部命中".
const requiredIds = [
  'summaryBox', 'grid', 'svtime', 'toasts',
  'authBox', 'keyInput', 'metaHint', 'refreshBtn',
  'filterBar', 'cntAll', 'cntReadable', 'cntIn', 'cntOut', 'cntStale', 'cntDead', 'autoPoll',
  'modalMask', 'modalTitle', 'modalBody', 'modalFoot',
  'addKey', 'addLabel',
];
const declaredIds = new Set();
for (const m of html.matchAll(/id="([A-Za-z0-9_]+)"/g)) declaredIds.add(m[1]);

const missingIds = requiredIds.filter((id) => !declaredIds.has(id));
if (missingIds.length) {
  console.error('FAIL 面板缺少骨架元素：' + missingIds.join(' | '));
  process.exit(1);
}

// ---------------------------------------------------------------------------
// 3. Every el("...") must resolve against a declared id
// ---------------------------------------------------------------------------
const touched = new Set();
for (const s of scripts) {
  for (const m of s.matchAll(/el\("([A-Za-z0-9_]+)"\)/g)) touched.add(m[1]);
}
const dangling = [...touched].filter((id) => !declaredIds.has(id));
if (dangling.length) {
  console.error('FAIL 脚本引用了 HTML 里不存在的 id（绑定会抛错，后续按钮全哑）：' + dangling.join(' | '));
  process.exit(1);
}

// ---------------------------------------------------------------------------
// 4. Known "eaten escape" shapes
// ---------------------------------------------------------------------------
// A backslash lost in transit turns a valid regex into an invalid one. These are
// NOT general regex validators: they are the specific scars left by the two
// incidents this gate was written for, expressed as plain substring searches so
// the gate itself cannot fall to the same escaping problem it is hunting.
const eaten = [
  '.match(/^(/',          // a group opened and never closed: backslashes eaten
  '.replace(/[^',         // same, for a character-class opener
];
for (const s of scripts) {
  for (const needle of eaten) {
    if (s.includes(needle)) {
      console.error('FAIL 反斜杠被吃掉的形态残留：找到 ' + JSON.stringify(needle));
      process.exit(1);
    }
  }
}

// ---------------------------------------------------------------------------
// 5. Syntax-check each block with the real engine
// ---------------------------------------------------------------------------
let failed = 0;
scripts.forEach((body, idx) => {
  const tmp = join(tmpdir(), `oxion-panel-${process.pid}-${idx}.js`);
  try {
    writeFileSync(tmp, body, 'utf8');
    const r = spawnSync(process.execPath, ['--check', tmp], { encoding: 'utf8' });
    if (r.status !== 0) {
      failed++;
      console.error(`FAIL <script> 块#${idx}（${body.length} 字符）语法错误：`);
      const err = (r.stderr || '').split(/\r?\n/).slice(0, 8).join('\n');
      console.error(err);
    }
  } finally {
    try { rmSync(tmp, { force: true }); } catch { /* best effort */ }
  }
});

if (failed) {
  console.error(`FAIL ${failed}/${scripts.length} 个 <script> 块语法错误`);
  process.exit(1);
}

console.log(`OK panel JS: ${scripts.length} 个 <script> 块语法通过，${declaredIds.size} 个 id，${touched.size} 个 el() 引用全部命中`);
