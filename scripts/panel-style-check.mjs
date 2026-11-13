// Style regression check: the model-disable checkboxes must NOT inherit text-field
// sizing.
//
// This is the bug the user saw: a bare `input{flex:1;min-width:220px;border:...}`
// rule (written for the text boxes) also matched every <input type="checkbox">,
// turning the picker into a grid of enormous empty squares.
//
// A JS syntax gate cannot see this - the page parses, it just looks broken. So
// this check looks for the two things that make it possible:
//   1. a bare `input{...}` rule that is not scoped by type or class;
//   2. text inputs without a class (which is what forces you to write the bare rule).
import { readFileSync } from 'node:fs';
const P = process.argv[2] || 'D:\\CLAW\\work\\cpa-plugins\\oxion\\panel.html';
const html = readFileSync(P, 'utf8');

let fail = 0;
const failMsg = (m) => { console.error('FAIL ' + m); fail++; };

// 1. bare element-level input rule
{
  const bare = /(^|\n)\s*input\s*\{/m;
  if (bare.test(html)) {
    failMsg('存在裸 `input{...}` 规则：它会同时命中所有 checkbox，把模型列表撑成一格一个的巨型方块');
  }
}
{
  const bareFocus = /(^|\n)\s*input:focus\s*\{/m;
  if (bareFocus.test(html)) failMsg('存在裸 `input:focus{...}` 规则（同上）');
}

// 2. text/password inputs must carry a class
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

// 3. the model card's checkboxes must have an explicit intrinsic-size guard
{
  if (!/\.mg-item\s+input\[type="checkbox"\]/.test(html)) {
    failMsg('缺少 .mg-item input[type="checkbox"] 的尺寸护栏：下一个裸 input 规则会再次把列表撑爆');
  }
}

// 4. the card should have the sibling panels' structure
{
  for (const needle of ['modelCtlCard', 'btnModelSave', 'btnModelAll', 'btnModelNone', 'mg-group']) {
    if (!html.includes(needle)) failMsg('模型禁用卡片缺少 ' + needle);
  }
}

if (fail) {
  console.error(`\n${fail} 项样式契约不满足`);
  process.exit(1);
}
console.log('OK panel style: 无裸 input 规则；文本输入框都有 class；checkbox 有尺寸护栏；禁用卡片结构完整');
