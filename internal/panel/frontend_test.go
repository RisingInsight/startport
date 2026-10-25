package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// panelJSFiles 面板前端脚本的加载顺序（与 index.html 的 <script> 顺序一致）。
// 2026-10-13 拆分（core.js / app.js / keys.js）后，node harness 必须按此顺序
// 拼接后再以单文件形态执行——经典脚本共享全局作用域，拼接顺序错误会引入
// TDZ / 引用错误（如 app.js 顶层用 $ 而 $ 定义在 core.js）。
// 2026-10-18 增补 ext.js（外部渠道视图，自包含模块，依赖 core.js 的 $/esc/ago/api/dur）。
// 2026-10-18 增补 extwiz.js（外部渠道新建/编辑弹层 + 情报草稿；ext.js 在运行时调用
// 它导出的 openExtWizard，故必须排在 ext.js 之后加载）。
// 2026-10-18 增补 acct.js（账号中心视图 v1.6.0；依赖 core.js 的 $/esc/ago/api/toast，
// 并在运行时调用 app.js 的 go / ext.js 的 loadExt——排在两者之后加载）。
var panelJSFiles = []string{"core.js", "app.js", "keys.js", "ext.js", "extwiz.js", "captcha.js", "acct.js"}

// mergedAppJSPath 拼接全部面板 JS 为临时文件并返回路径（供 node harness 读取）。
// 这样所有按函数名切片 app.js 的既有测试在拆分后无需改动切法。
func mergedAppJSPath(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, name := range panelJSFiles {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sb.Write(b)
		sb.WriteString("\n;\n")
	}
	f, err := os.CreateTemp(t.TempDir(), "merged-panel-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

// TestAppJSSyntax 每个面板 JS 文件都必须能通过 JS 解析器语法校验。
//
// 为什么需要：JS 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	for _, name := range panelJSFiles {
		path, err := filepath.Abs(name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, "--check", path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s syntax error:\n%s", name, out)
		}
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/startport/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), mergedAppJSPath(t))
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"预计 2026-09-28 16:00 解封（剩余 2时00分） · 网关最快 1时00分 后重试","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// TestAppJSTTFBVisualization v1.7.1 首字延迟可视化：自适应格式、p50 聚合与
// 请求流水表的「首字」列。
//
// 为什么钉这些：TTFB 列的用户诉求是"一眼看出慢在上游还是网关"——格式必须自适应
// （秒级一位小数 / 毫秒）、无样本必须显示"—"（非流式与错误路径没有该维度），
// p50 只统计窗口内且 ttfb>0 的条目（旧条目/非流式混入会拉歪结论）。
func TestAppJSTTFBVisualization(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; TTFB formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function fmtTTFB');
const end = src.indexOf('function trimFixed');
if (start < 0 || end < 0 || end <= start) throw new Error('ttfb helpers not found');
const ctx = { Date, Number, String, Math, isNaN };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.fmtTTFB=fmtTTFB;this.reqTTFBP50=reqTTFBP50;', ctx);
const now = Date.now();
const iso = ms => new Date(now - ms).toISOString();
const rows = [
  { ttfb_ms: 850, time: iso(30 * 60000) },    // 30 分钟前
  { ttfb_ms: 3000, time: iso(30 * 60000) },
  { ttfb_ms: 900, time: iso(10 * 60000) },    // 10 分钟前
  { ttfb_ms: 5000, time: iso(2 * 3600000) },  // 2 小时前（1h 窗口外）
  { ttfb_ms: 6000, time: iso(3 * 3600000) },  // 3 小时前（1h 窗口外）
  { ttfb_ms: 0, time: iso(5 * 60000) },       // 非流式：无样本
];
process.stdout.write(JSON.stringify({
  ms: ctx.fmtTTFB(850),
  s: ctx.fmtTTFB(3000),
  zero: ctx.fmtTTFB(0),
  missing: ctx.fmtTTFB(undefined),
  p50_1h: ctx.reqTTFBP50(rows, 60 * 60 * 1000),
  p50_24h: ctx.reqTTFBP50(rows, 24 * 60 * 60 * 1000),
  p50_all: ctx.reqTTFBP50(rows, 0),
  p50_empty: ctx.reqTTFBP50([], 3600000),
  p50_nosample: ctx.reqTTFBP50([{ ttfb_ms: 0, time: iso(60000) }], 3600000),
}));`
	f, err := os.CreateTemp(t.TempDir(), "ttfb-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("ttfb formatting node test failed: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse node output: %v\n%s", err, out)
	}
	cases := map[string]any{
		"ms": "850ms", "s": "3.0s", "zero": "—", "missing": "—",
		"p50_1h":    900,  // 窗口内 [850,3000,900] → 900
		"p50_24h":   3000, // 加上 2h 前的 50000 → 中位 3000
		"p50_all":   3000,
		"p50_empty": 0, "p50_nosample": 0,
	}
	for k, want := range cases {
		// JSON 反序列化后数值一律是 float64；want 可能是 int/string，统一走文本比较。
		gotStr := ""
		switch v := got[k].(type) {
		case float64:
			gotStr = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			gotStr = v
		default:
			gotStr = fmt.Sprint(v)
		}
		wantStr := ""
		switch v := want.(type) {
		case float64:
			wantStr = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			wantStr = v
		default:
			wantStr = fmt.Sprint(v)
		}
		if gotStr != wantStr {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}

	// 请求流水表：表头「首字」列（耗时之后）+ app.js 接线。
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	if !strings.Contains(html, `<th class="num">首字</th>`) {
		t.Error("请求流水表缺「首字」列")
	}
	// 「首字」在用量视图等处也有出现：列序断言限定在请求流水表的表头段内。
	theadStart := strings.Index(html, `id="reqFilter"`)
	theadEnd := strings.Index(html, `id="reqBody"`)
	if theadStart < 0 || theadEnd < 0 || theadEnd <= theadStart {
		t.Fatal("index.html 缺请求流水表结构")
	}
	thead := html[theadStart:theadEnd]
	first, second := strings.Index(thead, "耗时"), strings.Index(thead, "首字")
	if first < 0 || second < 0 || second < first {
		t.Error("「首字」列应紧跟「耗时」列之后")
	}
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fmtTTFB", "reqTTFBP50", "首字p50", "ttfb_samples"} {
		if !strings.Contains(string(appJS), want) {
			t.Errorf("app.js 缺 TTFB 接线片段 %q", want)
		}
	}
	// tooltip 的首字/总时长对比只在 ttfb>0 时出现（既有 requestLogText 逐字节断言
	// 已覆盖无 ttfb 的形态，这里确认有 ttfb 的形态存在）。
	if !strings.Contains(string(appJS), "占总时长") {
		t.Error("requestLogText 缺首字 vs 总时长对比")
	}
}

// TestAcctPlatformChangeSequence v1.7.4 平台切换序列回归（用户实测 bug）：
// 向导内切平台必须完整重算——Tab 置灰/原因文案/智能默认，绝不残留上一平台的
// 状态（"loomy → qoder 后浏览器 Tab 仍置灰、原因文案还是 Loomy"的复现路径）。
//
// 做法：node + 功能性 DOM 桩执行 acct.js 全文（顶层绑定把 onchange 挂到桩上），
// 然后按真实用户路径驱动下拉切换序列，断言每个状态的四个可观测量。
func TestAcctPlatformChangeSequence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; platform change sequence test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
function makeEl(id) {
  return {
    id, value: '', textContent: '', hidden: false, disabled: false, title: '',
    placeholder: '', innerHTML: '', className: '', href: '', dataset: {}, style: {},
    options: [],
    classList: {
      _s: new Set(),
      add(c) { this._s.add(c); },
      remove(c) { this._s.delete(c); },
      toggle(c, f) {
        if (f === undefined) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); }
        else if (f) { this._s.add(c); } else { this._s.delete(c); }
      },
      contains(c) { return this._s.has(c); }
    },
    querySelectorAll() { return []; },
    querySelector() { return null; },
    focus() {}, appendChild() {}, remove() {}, select() {}
  };
}
const els = {};
const $ = id => els[id] || (els[id] = makeEl(id));
const modeBrowser = makeEl('modeBrowser'); modeBrowser.dataset.mode = 'browser';
const modePaste = makeEl('modePaste'); modePaste.dataset.mode = 'paste';
els.accAddMode = makeEl('accAddMode');
els.accAddMode.querySelectorAll = sel => sel.indexOf('button') >= 0 ? [modeBrowser, modePaste] : [];
els.accAddPlatform = makeEl('accAddPlatform');
els.accAddPlatform.options = ['codebuddy', 'loomy', 'autoclaw', 'autoclaw-intl', 'qoder', 'qoder-intl', 'custom']
  .map(v => { const o = makeEl('opt-' + v); o.value = v; return o; });
let ctxOpenedCodeBuddy = false;
const ctx = {
  $,
  document: { addEventListener() {}, createElement: () => makeEl('tmp'), body: { appendChild() {} } },
  api: () => Promise.reject(new Error('no api in test')),
  confirm: () => true,
  openAdd: () => { ctxOpenedCodeBuddy = true; }, // CodeBuddy 登录弹窗（app.js 提供）
  Date, Number, String, Math, isNaN, JSON, Promise, Array, Object, RegExp, Error,
  setTimeout, clearTimeout, setInterval, clearInterval, console,
};
ctx.window = ctx;
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: 'acct.js' });
if (typeof els.accAddPlatform.onchange !== 'function') throw new Error('platform onchange not bound');

function switchTo(p) { els.accAddPlatform.value = p; els.accAddPlatform.onchange(); }
function snap() {
  const on = els.accAddMode.querySelectorAll('button').filter(b => b.classList.contains('on'))
    .map(b => b.dataset.mode)[0] || '';
  return {
    mode: on,
    browserDisabled: !!modeBrowser.disabled,
    hint: els.accAddModeHint ? els.accAddModeHint.textContent : '',
    browserPanel: !els.accAddBrowserPanel.hidden,
    pastePanel: !els.accAddPastePanel.hidden,
    segHidden: !!(els.accAddMode && els.accAddMode.hidden),
    abStartText: els.abStart ? els.abStart.textContent : ''
  };
}
const seq = [];
switchTo('loomy');  seq.push({ step: 'loomy', ...snap() });
switchTo('qoder');  seq.push({ step: 'qoder', ...snap() });
switchTo('loomy');  seq.push({ step: 'loomy2', ...snap() });
switchTo('custom'); seq.push({ step: 'custom', ...snap() });
// v1.7.5：CodeBuddy 并入统一下拉——选中后主按钮应关向导并打开 CodeBuddy 弹窗。
switchTo('codebuddy'); seq.push({ step: 'codebuddy', ...snap() });
els.accAddVeil.classList.add('on'); // 模拟向导打开态
if (typeof els.abStart.onclick !== 'function') throw new Error('abStart onclick not bound');
els.abStart.onclick();
seq.push({
  step: 'codebuddy_confirm',
  mode: '', browserDisabled: false, hint: '',
  browserPanel: false, pastePanel: false,
  segHidden: false, abStartText: '',
  openedCodeBuddy: ctxOpenedCodeBuddy,
  veilClosed: !els.accAddVeil.classList.contains('on')
});
process.stdout.write(JSON.stringify(seq));`
	f, err := os.CreateTemp(t.TempDir(), "acct-switch-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	acctPath, err := filepath.Abs("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f.Name(), acctPath).CombinedOutput()
	if err != nil {
		t.Fatalf("platform change sequence test failed: %v\n%s", err, out)
	}
	var seq []struct {
		Step            string `json:"step"`
		Mode            string `json:"mode"`
		BrowserDisabled bool   `json:"browserDisabled"`
		Hint            string `json:"hint"`
		BrowserPanel    bool   `json:"browserPanel"`
		PastePanel      bool   `json:"pastePanel"`
		SegHidden       bool   `json:"segHidden"`
		AbStartText     string `json:"abStartText"`
		OpenedCodeBuddy bool   `json:"openedCodeBuddy"`
		VeilClosed      bool   `json:"veilClosed"`
	}
	if err := json.Unmarshal(out, &seq); err != nil {
		t.Fatalf("parse node output: %v\n%s", err, out)
	}
	type wantState struct {
		mode                                      string
		hintContains                              string
		browserDisabled, browserPanel, pastePanel bool
	}
	want := map[string]wantState{
		// loomy：不支持 → 粘贴 Tab + Loomy 原因 + 浏览器置灰。
		"loomy": {mode: "paste", hintContains: "Loomy 无登录流程", browserDisabled: true, browserPanel: false, pastePanel: true},
		// qoder：支持 → 智能默认切到浏览器登录 Tab，原因文案清空。
		"qoder": {mode: "browser", hintContains: "", browserDisabled: false, browserPanel: true, pastePanel: false},
		// 再切回 loomy：重新落粘贴 + Loomy 原因（双向都要正确）。
		"loomy2": {mode: "paste", hintContains: "Loomy 无登录流程", browserDisabled: true, browserPanel: false, pastePanel: true},
		// custom：兜底文案（无平台专属原因）。
		"custom": {mode: "paste", hintContains: "通用平台无统一登录", browserDisabled: true, browserPanel: false, pastePanel: true},
		// codebuddy（v1.7.5）：模式 Tab 区隐藏、无原因文案、主按钮 = 打开 CodeBuddy 弹窗。
		"codebuddy": {mode: "browser", hintContains: "", browserDisabled: false, browserPanel: true, pastePanel: false},
	}
	if len(seq) != 6 { // loomy / qoder / loomy2 / custom / codebuddy / codebuddy_confirm
		t.Fatalf("切换序列长度 = %d, want 6: %+v", len(seq), seq)
	}
	for _, st := range seq {
		if st.Step == "codebuddy_confirm" {
			continue // 合成步骤：click 后的分流断言在循环外做
		}
		w, ok := want[st.Step]
		if !ok {
			t.Fatalf("未知步骤 %q", st.Step)
		}
		if st.Mode != w.mode {
			t.Errorf("%s: mode = %q, want %q", st.Step, st.Mode, w.mode)
		}
		if st.BrowserDisabled != w.browserDisabled {
			t.Errorf("%s: browserDisabled = %v, want %v", st.Step, st.BrowserDisabled, w.browserDisabled)
		}
		if st.BrowserPanel != w.browserPanel {
			t.Errorf("%s: browserPanel = %v, want %v", st.Step, st.BrowserPanel, w.browserPanel)
		}
		if st.PastePanel != w.pastePanel {
			t.Errorf("%s: pastePanel = %v, want %v", st.Step, st.PastePanel, w.pastePanel)
		}
		if !strings.Contains(st.Hint, w.hintContains) {
			t.Errorf("%s: hint = %q, want 含 %q", st.Step, st.Hint, w.hintContains)
		}
	}
	// codebuddy 步骤的专属断言：模式 Tab 区隐藏 + 主按钮文案 + 点击分流。
	cb := seq[len(seq)-2]
	if cb.Step != "codebuddy" {
		t.Fatalf("倒数第二步应为 codebuddy, got %q", cb.Step)
	}
	if !cb.SegHidden {
		t.Error("codebuddy: 模式 Tab 区应隐藏（无粘贴形态，防灰 Tab 困惑）")
	}
	if cb.AbStartText != "打开 CodeBuddy 登录窗口" {
		t.Errorf("codebuddy: 主按钮文案 = %q", cb.AbStartText)
	}
	cc := seq[len(seq)-1]
	if cc.Step != "codebuddy_confirm" {
		t.Fatalf("最后一步应为 codebuddy_confirm, got %q", cc.Step)
	}
	if !cc.OpenedCodeBuddy {
		t.Error("codebuddy: 主按钮应打开 CodeBuddy 登录弹窗（openAdd）")
	}
	if !cc.VeilClosed {
		t.Error("codebuddy: 确认后应先关闭平台向导")
	}
}

// TestAcctSingleRegionPools v1.7.6：Loomy / TRAE / 通用平台收成同构池入口。
//
// 钉住三件事：
//   - 单区域池不显示「全部/国内/国际」页签，Qoder / AutoClaw 仍显示；
//   - 通用平台逐账号渠道 c-<id> 的模型键 ext:c-<id>/<model> 能被匹配与剥前缀，
//     且不会误吞固定前缀平台（ext:qoder/...）；
//   - 旧锚点/行点击（loomy / trae / custom）仍映射到对应 *-pool 入口。
func TestAcctSingleRegionPools(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; single-region pool test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
function makeEl(id) {
  return {
    id, value: '', textContent: '', hidden: false, disabled: false, title: '',
    placeholder: '', innerHTML: '', className: '', href: '', dataset: {}, style: {},
    options: [],
    classList: {
      _s: new Set(),
      add(c) { this._s.add(c); },
      remove(c) { this._s.delete(c); },
      toggle(c, f) {
        if (f === undefined) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); }
        else if (f) { this._s.add(c); } else { this._s.delete(c); }
      },
      contains(c) { return this._s.has(c); }
    },
    querySelectorAll() { return []; },
    querySelector() { return null; },
    focus() {}, appendChild() {}, remove() {}, select() {}
  };
}
const els = {};
const $ = id => els[id] || (els[id] = makeEl(id));
const ctx = {
  $, view: 'accounts-center',
  history: { replaceState() {} },
  document: { addEventListener() {}, createElement: () => makeEl('tmp'), body: { appendChild() {} } },
  api: () => Promise.reject(new Error('no api in test')),
  confirm: () => true,
  esc: s => String(s == null ? '' : s),
  fmtTok: n => String(n), fmtCredit: n => String(n), ago: () => '—',
  toast: () => {},
  Date, Number, String, Math, isNaN, JSON, Promise, Array, Object, RegExp, Error,
  setTimeout, clearTimeout, setInterval, clearInterval, console,
};
ctx.window = ctx;
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: 'acct.js' });
function pick(step) {
  return {
    step,
    tabsHidden: !!els.acctRegionTabs.hidden,
    title: els.acctAllTitle.textContent,
    add: els.btnAcctAdd.textContent,
  };
}
const out = [];
ctx.selectAcctTab('loomy-pool');
out.push({ ...pick('loomy-pool'),
  loomy: ctx.acctQoderModelMatch('ext:loomy/session-a', 'all'),
  qoderLeak: ctx.acctQoderModelMatch('ext:qoder/qwen', 'all'),
  name: ctx.acctQoderModelName('ext:loomy/session-a') });
ctx.selectAcctTab('trae-pool');
out.push({ ...pick('trae-pool'),
  trae: ctx.acctQoderModelMatch('ext:trae/solo-1', 'all'),
  name: ctx.acctQoderModelName('ext:trae/solo-1') });
ctx.selectAcctTab('custom-pool');
out.push({ ...pick('custom-pool'),
  custom: ctx.acctQoderModelMatch('ext:c-abc123/gpt-4o', 'all'),
  emptyChannel: ctx.acctQoderModelMatch('ext:c-/gpt-4o', 'all'),
  name: ctx.acctQoderModelName('ext:c-abc123/gpt-4o') });
ctx.selectAcctTab('qoder-pool');
out.push({ ...pick('qoder-pool'),
  cn: ctx.acctQoderModelMatch('ext:qoder/qwen', 'all'),
  intl: ctx.acctQoderModelMatch('ext:qoder-intl/claude', 'all'),
  name: ctx.acctQoderModelName('ext:qoder-intl/claude') });
ctx.selectAcctTab('autoclaw-pool');
out.push({ ...pick('autoclaw-pool'),
  cn: ctx.acctQoderModelMatch('ext:autoclaw/glm-5.3', 'all') });
// 旧入口映射：行点击 / 旧 hash 传平台 key，最终落到同构池。
ctx.selectAcctTab('custom');
out.push({ ...pick('custom-legacy'), mappedToPool: els.acctAllTitle.textContent === '通用平台账号池' });
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "acct-pools-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	acctPath, err := filepath.Abs("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, f.Name(), acctPath).CombinedOutput()
	if err != nil {
		t.Fatalf("single-region pool test failed: %v\n%s", err, raw)
	}
	var got []struct {
		Step         string `json:"step"`
		TabsHidden   bool   `json:"tabsHidden"`
		Title        string `json:"title"`
		Add          string `json:"add"`
		Loomy        bool   `json:"loomy"`
		Trae         bool   `json:"trae"`
		Custom       bool   `json:"custom"`
		EmptyChannel bool   `json:"emptyChannel"`
		QoderLeak    bool   `json:"qoderLeak"`
		Cn           bool   `json:"cn"`
		Intl         bool   `json:"intl"`
		Name         string `json:"name"`
		MappedToPool bool   `json:"mappedToPool"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse node output: %v\n%s", err, raw)
	}
	if len(got) != 6 {
		t.Fatalf("steps = %d, want 6: %+v", len(got), got)
	}
	byStep := map[string]int{}
	for i, s := range got {
		byStep[s.Step] = i
	}
	for _, step := range []string{"loomy-pool", "trae-pool", "custom-pool"} {
		s := got[byStep[step]]
		if !s.TabsHidden {
			t.Errorf("%s: 单区域池不应显示国内/国际页签", step)
		}
	}
	if s := got[byStep["loomy-pool"]]; !s.Loomy || s.QoderLeak || s.Name != "session-a" || s.Title != "Loomy 账号池" || s.Add != "添加 Loomy 账号" {
		t.Errorf("loomy-pool 状态不对: %+v", s)
	}
	if s := got[byStep["trae-pool"]]; !s.Trae || s.Name != "solo-1" || s.Title != "TRAE 账号池" {
		t.Errorf("trae-pool 状态不对: %+v", s)
	}
	if s := got[byStep["custom-pool"]]; !s.Custom || s.EmptyChannel || s.QoderLeak || s.Name != "gpt-4o" || s.Title != "通用平台账号池" || s.Add != "添加通用平台账号" {
		t.Errorf("custom-pool 状态不对: %+v", s)
	}
	if s := got[byStep["qoder-pool"]]; s.TabsHidden || !s.Cn || !s.Intl || s.Name != "claude" {
		t.Errorf("qoder-pool 应保留区域页签且匹配双区: %+v", s)
	}
	if s := got[byStep["autoclaw-pool"]]; s.TabsHidden || !s.Cn {
		t.Errorf("autoclaw-pool 应保留区域页签: %+v", s)
	}
	if s := got[byStep["custom-legacy"]]; !s.TabsHidden || !s.MappedToPool || s.Title != "通用平台账号池" {
		t.Errorf("旧入口 custom 未映射到 custom-pool: %+v", s)
	}
}

// 请求记录行必须紧凑、可读，并带上调用来源（IP / UA）；来源缺失时以 — 兜底。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
const cached = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  noSource: ctx.requestLogText(noSource),
  cached: ctx.requestLogText(cached),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	cached := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `,"cached":` + strconv.Quote(cached) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 请求记录筛选：IP / UA / 模型 / 账号 / 请求 ID 的包含匹配（空格分词 AND）+ 结果精确匹配。
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: '示例(uid8)', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: '甲(uid9)', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
  all: pick({ q: '', outcome: '' }),
  byIP: pick({ q: '203.0.113', outcome: '' }),
  byUA: pick({ q: 'chrome/120', outcome: '' }),
  byModel: pick({ q: 'glm', outcome: '' }),
  multiKw: pick({ q: 'glm success', outcome: '' }),
  multiMiss: pick({ q: 'glm chrome', outcome: '' }),
  byOutcome: pick({ q: '', outcome: 'http_error' }),
  combined: pick({ q: '198.51', outcome: 'http_error' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q 对 outcome 不参与匹配（outcome 有独立下拉），multiKw 里的 success 命中不了任何字段。
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// 模型按条件查询：域 / 能力 / 档位 / 价格 / 关键词，以及倍率、上下文、输出排序。
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue;', ctx);
const models = [
  { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['视觉'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: '夜间折扣', context_length: 1000000, max_output_tokens: 131000 },
  { id: 'cn:hy3', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: '限时免费', context_length: 192000, max_output_tokens: 64000 },
  { id: 'global:hy3', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
  { id: 'cn:auto', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
  all: ids(models),
  realm: filter({ realm: 'cn' }),
  tool: filter({ cap: 'tool' }),
  vision: filter({ cap: 'vision' }),
  reasoning: filter({ cap: 'reasoning' }),
  isDefault: filter({ cap: 'default' }),
  effortOff: filter({ effort: 'off' }),
  effortLow: filter({ effort: 'low' }),
  free: filter({ promo: 'free' }),
  promo: filter({ promo: 'promo' }),
  discount: filter({ promo: 'discount' }),
  q: filter({ q: 'glm zhipu' }),
  qMiss: filter({ q: 'glm nosuch' }),
  sortRate: filter({ sort: 'rate' }),
  sortContext: filter({ sort: 'context' }),
  sortOutput: filter({ sort: 'output' }),
  sortName: filter({ sort: 'name' }),
  rateFree: ctx.mdRateValue(models[1]),
  rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["cn:glm-5.2","cn:hy3","global:hy3","cn:auto"],` +
		`"realm":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"tool":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"vision":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"reasoning":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"isDefault":["cn:auto"],` +
		`"effortOff":["cn:glm-5.2"],` +
		`"effortLow":["cn:hy3"],` +
		`"free":["cn:hy3"],` +
		`"promo":["cn:glm-5.2","cn:hy3"],` +
		`"discount":["cn:glm-5.2"],` +
		`"q":["cn:glm-5.2"],` +
		`"qMiss":[],` +
		`"sortRate":["cn:hy3","global:hy3","cn:glm-5.2","cn:auto"],` +
		`"sortContext":["cn:glm-5.2","global:hy3","cn:auto","cn:hy3"],` +
		`"sortOutput":["global:hy3","cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"sortName":["cn:auto","cn:glm-5.2","cn:hy3","global:hy3"],` +
		`"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// 外部渠道模型：realm=ext 只看 ext: 条目；关键词能匹配平台/适配器。
func TestAppJSModelFilterExt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch;', ctx);
const models = [
  { id: 'ext:qoder/Qwen3.8-Max', source: 'ext', provider: 'qoder', platform_name: 'Qoder 国内', adapter: 'qoder' },
  { id: 'ext:trae/glm-5.2', source: 'ext', provider: 'trae', platform_name: 'TRAE', adapter: 'trae' },
  { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu' },
];
const ids = f => models.filter(m => ctx.mdMatch(m, f)).map(m => m.id);
process.stdout.write(JSON.stringify({
  ext: ids({ realm: 'ext' }),
  qoder: ids({ realm: 'ext:qoder' }),
  trae: ids({ realm: 'ext:trae' }),
  q: ids({ q: 'qoder' }),
  cn: ids({ realm: 'cn' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-ext-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("ext model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"ext":["ext:qoder/Qwen3.8-Max","ext:trae/glm-5.2"],` +
		`"qoder":["ext:qoder/Qwen3.8-Max"],"trae":["ext:trae/glm-5.2"],` +
		`"q":["ext:qoder/Qwen3.8-Max"],"cn":["cn:glm-5.2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("ext model filter=%s\nwant %s", out, want)
	}
}

// 用量时序图的柱体类名不得叫 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
// height 是 rect 的 CSS 几何属性——同名类会把每根柱子压成 3px 高，图看起来"没数据"。
// 这个坑只能在浏览器里看出来，所以在这里钉住类名。
//
// DeepSeek 式改版后主图三指标切换：默认「消耗积分」为单段柱（每点 1 根 usbar），
// 「Tokens」为 prompt/completion 双段堆叠（每点 2 根）。两种模式都在此钉住。
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, Map,
  document: undefined,
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart=renderUsageChart; this.setUsMetric=v=>{usMetric=v;};', ctx);
// 三段堆叠(命中/未命中/输出)要求同量级数据:跨 3 个数量级时小段会被
// 0.4px 可见阈值裁掉(该阈值是防 0 高度矩形污染 DOM 的,不是 bug)。
const series = [
  { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 24000, completion_tokens: 6000, total_tokens: 30000, requests: 1, credits: 0.5, cache_hit_tokens: 18000, cache_miss_tokens: 6000 },
  { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 27000, completion_tokens: 7000, total_tokens: 34000, requests: 39, credits: 2.5, cache_hit_tokens: 21000, cache_miss_tokens: 6000 },
  { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 30000, completion_tokens: 8000, total_tokens: 38000, requests: 200, credits: 12, cache_hit_tokens: 24000, cache_miss_tokens: 6000 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
const count = (s, re) => (s.match(re) || []).length;
const creditsBars = count(svg, /class="usbar"/g);
const creditsHits = count(svg, /class="usbar-hit"/g);
const creditsGroups = count(svg, /<g class="usbarg"/g);
const creditsTitles = count(svg, /<title>/g);
ctx.setUsMetric('tokens');
ctx.renderUsageChart(series);
const svgTok = host.innerHTML;
process.stdout.write(JSON.stringify({
  hasUsbar: svg.includes('class="usbar"'),
  hasBareBar: /class="bar"/.test(svg),
  creditsBars, creditsHits, creditsGroups, creditsTitles,
  tokensBars: count(svgTok, /class="usbar"/g),
  tokensHitMissOut: ['usGradHit', 'usGradMiss', 'usGradOut'].every(g => svgTok.includes('fill="url(#' + g + ')"')),
  hasPeak: svg.includes('峰值'),
  hasAvg: svg.includes('均值'),
  emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"creditsBars":3,"creditsHits":3,"creditsGroups":3,"creditsTitles":0,"tokensBars":9,"tokensHitMissOut":true,"hasPeak":true,"hasAvg":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// 时间范围控件：预设 → 查询参数的映射。要点：
//   - 「今天」必须发浏览器本地时区的 00:00（服务端时区未必一致），且不带 to；
//   - 滚动预设 rolling=true 发 hours（服务端整点对齐），rolling=false 折算成 from；
//   - 「全部历史」两者都不发；「自定义」发用户挑的 from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
  document: { getElementById: () => host },
  $: () => host,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
  ctx.trangeState('t').preset = preset;
  return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
  const m = /(?:^|&)from=(\d+)/.exec(qs);
  return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
  todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
  todayNoTo: !/to=/.test(todayQ),
  rolling24: q('24', true),
  rolling72: q('72', true),
  rolling0: q('0', true),
  log0: q('0', false),
  log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
  log24HasHours: /hours=/.test(q('24', false)),
  log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
  custom: (function () {
    const st = ctx.trangeState('t');
    st.preset = 'custom';
    st.from = new Date(2026, 8, 30, 9, 0, 0);
    st.to = new Date(2026, 8, 30, 18, 30, 0);
    return ctx.trangeQuery('t', true).toString();
  })(),
  labelCustom: ctx.trangeLabel('t'),
  labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		// rolling0 必须是 "hours=0"（显式全部历史）。此前期望值是空串——那恰好把
		// issue #121 的错误行为固化成了断言：空 query 会被后端的 72 小时缺省接管，
		// 于是「全部历史」显示成「近 3 天」。
		// log0 仍为空：请求记录端点没有缺省窗口，不传 from/to 就是全部历史。
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"hours=0","log0":"",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"今天"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// 配置表单与 CFG_MAP 必须一一对应，且面板声称"可在线改"的热生效键必须真的
// 出现在表单里。
//
// 为什么需要：`logging.request_client_info` 曾经在表单里存在过，后来在某次改动中
// 被连带删掉，而 Go 侧的配置键、livecfg 热生效通路、README 的描述都还在——面板
// 少了一个开关而 Go 测试全绿，只有人肉点开配置页才会发现。这里把"表单字段 ↔
// CFG_MAP"与"关键热改键必须在表单里"两条都钉住。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	src, err := os.ReadFile(mergedAppJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// CFG_MAP 块（下面两条检查共用）。
	mapBlock := js[strings.Index(js, "const CFG_MAP = {"):]
	mapBlock = mapBlock[:strings.Index(mapBlock, "\n};")]
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），只有行首
	// 那个带换行缩进。按「前面是行首或分隔符」判定才不漏。
	inMap := func(name string) bool {
		return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `:\s*\[`).MatchString(mapBlock)
	}

	// 1) 表单里的每个 name 都要有 CFG_MAP 条目（否则收集/回填都拿不到它）。
	form := html[strings.Index(html, `<form id="cfgForm">`):]
	form = form[:strings.Index(form, "</form>")]
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatch(form, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}
	for n := range names {
		if !inMap(n) {
			t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃）", n)
		}
	}

	// 2) CFG_MAP 里的每个键都要在表单里有控件（否则回填/保存是空转）。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(mapBlock, -1) {
		if !names[m[1]] {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有对应控件", m[1])
		}
	}

	// 3) 明确断言这一个键：后端有配置项、README 说面板可改，UI 不能少。
	if !strings.Contains(js, "request_client_info: ['logging', 'request_client_info']") {
		t.Error("CFG_MAP 缺 request_client_info 条目")
	}
	if !names["request_client_info"] {
		t.Error("配置表单缺「记录调用来源」开关（logging.request_client_info）")
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表，
// 以及临期固定档（1 天内 / 7 天内，含已到期）单独归集、不重复计数。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 1700000000000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'week', remain: 20, expires_at: now + 5 * day },
    { name: 'later', remain: 50, expires_at: now + 31 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'expired-b', remain: 35, expires_at: now - day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  tiers: out.tiers.map(t => ({ label: t.label, credits: t.credits, segments: t.segments.length })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	// days<=1 的三段（soon-a 30、soon-b 20、expired-b 35）进「1 天内」；
	// week(5 天/20) 进「7 天内」；later(31 天/50) 留在普通分桶。三者互斥、不重复计数。
	const want = `{"rows":[{"days":31,"credits":50}],"tiers":[{"label":"1 天内","credits":85,"segments":3},{"label":"7 天内","credits":20,"segments":1}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：漏发会让面板显示"已保存"
// 而 config.json 里的值没变（issue #102 附带发现 2）。
//
// 同时钉住反面：其余文本字段空串仍然不下发。这条同样重要——若哪天为了修上面那个
// 问题改成"所有空串都发"，表单里任何一个没填的框都会变成"请清空"，静默抹掉配置。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk('secret'),
  user_agent: mk(''),
  prompt_file: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectConfig = collectConfig;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out, 'listen'),
  has(out.schedule, 'checkin_hours'),
  out.api_key
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值, prompt.file 已发, 其值, listen 未发, checkin_hours 未发, api_key]
	const want = `[true,"",true,"",false,false,"secret"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySortedByExpiry 到期提醒按「最近到期」升序，且不分域（issue #125）。
//
// 后端 /startport/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
// global 排在末尾，看上去像"按域分组"，实际只是余额顺序。本用例刻意按这个形态构造
// 输入（余额高的到期最晚、global 余额最低），若前端不重排就会保持该顺序而失败。
//
// 日期按「今天 +N 天」生成而不是写死：写死的话过了那天用例就会自己失效。
func TestAppJSExpirySortedByExpiry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry sort test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sink = { innerHTML: '', textContent: '', hidden: true };
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: () => sink,
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpiry = renderExpiry;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
const pack = (days, amount) => ({ end_time: iso(days) + ' 00:00:00', remain: amount });
// 模拟后端顺序：余额降序 → CN 高余额在前且到期最晚，global 最少且最快到期。
const d = { accounts: [
  { uid: 'a', nickname: 'cn-late',  realm: 'cn',     packages: [pack(16, 900)] },
  { uid: 'b', nickname: 'cn-mid',   realm: 'cn',     packages: [pack(12, 700)] },
  { uid: 'c', nickname: 'gl-soon',  realm: 'global', packages: [pack(2, 500)] },
  { uid: 'd', nickname: 'no-expiry', realm: 'cn',    packages: [pack(-3, 100)] },
  { uid: 'e', nickname: 'broken',   realm: 'cn',     error: 'offline' },
] };
ctx.renderExpiry(d);
const names = [...sink.innerHTML.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
process.stdout.write(JSON.stringify(names));`
	f, err := os.CreateTemp(t.TempDir(), "expsort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("expiry sort node test failed: %v\n%s", err, out)
	}
	// 最快到期的排最前（跨域）；无 7 天内到期的与查询失败的排最后。
	const want = `["gl-soon","cn-mid","cn-late","no-expiry","broken"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry order=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySnapshotRenders 到期卡片用 overview 快照即可渲染（issue #137）。
//
// 此前的死锁：卡片在 index.html 上写死 hidden，而唯一解除隐藏的语句在
// renderExpiry() 末尾——该函数只有点「检查」才被调用。按钮本身就在隐藏卡片里，
// 用户看不见也点不到，于是"积分到期提醒"这个功能等于不存在。
//
// 修复引入 renderExpirySnapshot()：吃 /overview 响应里的 credits_earliest_expiry /
// credits_earliest_remaining（state.json 现成数据，零上游请求），并显式设
// expBox.hidden = false。本用例钉住两件事：
//  1. 只给 overview 形态的输入（没有 packages 字段）也能出内容；
//  2. 渲染后 expBox.hidden 必须为 false——把这句删掉（还原 bug）测试就必须 FAIL。
func TestAppJSExpirySnapshotRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderExpirySnapshot');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const day = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  return d.toISOString();
};
// overview 形态：没有 packages 数组，只有到期快照字段。
const d = { accounts: [
  { uid: 'aaaaaaaa-1', nickname: '秋',     credits: 4099,
    credits_earliest_expiry: day(2),  credits_earliest_remaining: 499 },
  { uid: 'bbbbbbbb-2', nickname: '秋千易', credits: 3243,
    credits_earliest_expiry: day(30), credits_earliest_remaining: 43 },
  { uid: 'cccccccc-3', nickname: '长期包', credits: 900,
    credits_earliest_expiry: null,    credits_earliest_remaining: 0 },
] };
ctx.renderExpirySnapshot(d);
const html = sinks['expList'] ? sinks['expList'].innerHTML : '';
const names = [...html.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
// 2 天到期的应排最前（红），30 天其次，无到期信息的排最后。
const wantNames = JSON.stringify(['秋', '秋千易', '长期包']);
const gotNames = JSON.stringify(names);
process.stdout.write(JSON.stringify({
  visible: sinks['expBox'] ? sinks['expBox'].hidden === false : false,
  rows: names.length,
  order: gotNames === wantNames,
  got: gotNames,
  want: wantNames,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expsnap-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":true,"rows":3,"order":true,"got":"[\"秋\",\"秋千易\",\"长期包\"]","want":"[\"秋\",\"秋千易\",\"长期包\"]"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry snapshot=%s\nwant %s（visible 必须为 true，否则卡片仍隐藏）",
			strings.TrimSpace(string(out)), want)
	}
}

// TestPanelIndexExpiryBoxNotHidden 到期卡片在 HTML 骨架里不得带 hidden 属性（issue #137）。
//
// 与上一个用例互补：app.js 侧有 renderExpirySnapshot 兜底，HTML 侧若仍写死 hidden，
// 用户在首次 overview 返回前（以及 JS 加载失败时）看到的依然是空白。这个断言就是
// 那个曾经卡住整个功能的属性——把它加回去测试必须 FAIL。
func TestPanelIndexExpiryBoxNotHidden(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// 定位到期卡片那一行，只检查它自己，避免误伤其他合法的 hidden 元素。
	idx := strings.Index(string(b), `id="expBox"`)
	if idx < 0 {
		t.Fatal(`index.html missing id="expBox"`)
	}
	lineStart := strings.LastIndex(string(b[:idx]), "\n") + 1
	lineEnd := strings.Index(string(b[idx:]), "\n")
	if lineEnd < 0 {
		lineEnd = len(b) - idx
	}
	line := string(b[lineStart : idx+lineEnd])
	if strings.Contains(line, "hidden") {
		t.Fatalf(`expBox 元素不得带 hidden（issue #137 死锁根因），实际行：%s`, strings.TrimSpace(line))
	}
}

// TestAppJSChartTooltipDelegated 图表 tooltip 必须是"每根柱子独立寻址"的委托结构。
//
// 演进史:issue #128 时代 <title> 平铺在 <svg> 根下,整张图共用一个 tooltip——
// 浏览器取第一个,悬停任何柱子都显示同一份数据,当时的修复是把 <title> 包进 <g>。
// DeepSeek 式改版后换成 HTML 富 tooltip(命中/未命中/输出多行明细,<title> 塞不下),
// 寻址机制变为:g.usbarg[data-i] + 宿主 div 上的委托 mousemove。
//
// 本用例钉住新机制的结构不变量:①分组数 == 数据点数;②每组都带 data-i;
// ③不再有任何 <title>(残留会被浏览器渲染成原生 tooltip,和富 tooltip 叠加);
// ④透明命中层(usbar-hit)数量 == 数据点数——没有它,悬停必须精确落在细柱上。
func TestAppJSChartTooltipDelegated(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; chart tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const chartStart = src.indexOf('function renderUsageChart');
if (start < 0 || chartStart < 0) throw new Error('chart functions not found');
let end = src.indexOf('\nfunction ', chartStart + 10);
if (end < 0) end = src.length;
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('fmt helpers not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart = renderUsageChart; this.usMainTipHTML = usMainTipHTML;', ctx);
const series = [];
for (let h = 0; h < 5; h++) {
  const d = new Date(); d.setHours(d.getHours() - (4 - h), 0, 0, 0);
  const iso = d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
              String(d.getDate()).padStart(2, '0') + 'T' + String(d.getHours()).padStart(2, '0');
  series.push({ t: iso, scope: 'hour', prompt_tokens: (h + 1) * 100,
                completion_tokens: (h + 1) * 10, total_tokens: (h + 1) * 110, requests: h + 1,
                credits: 0.1 * (h + 1), cache_hit_tokens: (h + 1) * 80, cache_miss_tokens: (h + 1) * 20 });
}
ctx.renderUsageChart(series);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const count = (s, re) => (s.match(re) || []).length;
const tip = ctx.usMainTipHTML({
  raw: '2026-10-05T14', req: 3, err: 1, credits: 1.5,
  pt: 100, ct: 20, tt: 120, ch: 80, cm: 20,
}, false);
process.stdout.write(JSON.stringify({
  points: series.length,
  groups: count(svg, /<g class="usbarg"/g),
  dataIdx: count(svg, /data-i="/g),
  titles: count(svg, /<title>/g),
  hitLayer: count(svg, /class="usbar-hit"/g),
  tipRows: count(tip, /class="r"/g),
  tipHit: tip.includes('输入(命中缓存)'),
  tipMiss: tip.includes('输入(未命中缓存)'),
  tipOut: tip.includes('输出'),
  tipFail: tip.includes('失败'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "charttip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("chart tooltip node test failed: %v\n%s", err, out)
	}
	const want = `{"points":5,"groups":5,"dataIdx":5,"titles":0,"hitLayer":5,"tipRows":6,"tipHit":true,"tipMiss":true,"tipOut":true,"tipFail":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("chart tooltip structure=%s\nwant %s（groups/data-i/hitLayer 应等于数据点数，<title> 应为 0）",
			strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSUsageDailyMerge 按天归并(DeepSeek 式主图的粒度自适应)与分组小节的数据形状。
//
// 要点:①小时桶按日历天归并,同天多桶累加(请求/token/积分/缓存明细各字段都对);
// ②>72h 的窗口主图自动切按天柱(柱组数 == 天数,说明行带「天」字);
// ③缓存未观测的样本在 tooltip 里合成一行「输入」,不伪造两行。
func TestAppJSUsageDailyMerge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; daily merge test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.mergeDaily = mergeDaily; this.renderUsageChart = renderUsageChart; this.usMainTipHTML = usMainTipHTML;', ctx);
const mkHour = (day, hour, req, credits, ch, cm) => ({
  t: '2026-10-0' + day + 'T' + String(hour).padStart(2, '0'), scope: 'hour',
  requests: req, credits: credits, prompt_tokens: 100 * req, completion_tokens: 10 * req,
  total_tokens: 110 * req, cache_hit_tokens: ch, cache_miss_tokens: cm,
});
const merged = ctx.mergeDaily([
  mkHour(1, 9, 2, 1.0, 150, 50),
  mkHour(1, 15, 3, 0.5, 0, 0),
  mkHour(2, 10, 1, 0.2, 80, 20),
]);
ctx.renderUsageChart([
  mkHour(1, 9, 2, 1.0, 150, 50),
  mkHour(1, 15, 3, 0.5, 0, 0),
  mkHour(2, 10, 1, 0.2, 80, 20),
  mkHour(3, 10, 1, 0.1, 0, 0),
  mkHour(4, 10, 1, 0.1, 0, 0),
  mkHour(5, 10, 1, 0.1, 0, 0),
]);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const note = sinks['usChartNote'] ? sinks['usChartNote'].textContent : '';
const count = (s, re) => (s.match(re) || []).length;
const noCacheTip = ctx.usMainTipHTML({ raw: '2026-10-05', req: 1, err: 0, credits: 0.1, pt: 100, ct: 10, tt: 110, ch: 0, cm: 0 }, true);
process.stdout.write(JSON.stringify({
  days: merged.length,
  day1: { raw: merged[0].raw, req: merged[0].req, credits: merged[0].credits, tt: merged[0].tt, ch: merged[0].ch },
  dailyBars: count(svg, /<g class="usbarg"/g),
  noteSaysDays: note.includes('天'),
  noCacheRows: count(noCacheTip, /class="r"/g),
  noCacheSingleInput: noCacheTip.includes('输入') && !noCacheTip.includes('命中缓存'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "dailymg-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("daily merge node test failed: %v\n%s", err, out)
	}
	const want = `{"days":2,"day1":{"raw":"2026-10-01","req":5,"credits":1.5,"tt":550,"ch":150},"dailyBars":5,"noteSaysDays":true,"noCacheRows":5,"noCacheSingleInput":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("daily merge=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSAttnExpiryAlert E2 积分预警的取数口径。
//
// renderAttn 此前直接拿 overview 的 credits_earliest_remaining（该批剩余积分量）
// 当「天数」与 7 比较，语义全错。改由 attnEarliestExpiry 三级降级取数：
// ① lastPackages 共享缓存有该账号的实时包明细 → 与到期卡完全同口径（同一
//
//	expBatches/expDaysLeft，过滤已到期批次后取最早）；
//
// ② 退回 overview 的 credits_earliest_expiry，按日历日差折算天数；零值
//
//	"0001-01-01T00:00:00Z" 视为无数据（Go 的 omitempty 对非指针 time.Time 无效）；
//
// ③ 都没有 → null，不产生预警条目。
//
// 切片锚点与 TestAppJSExpirySortedByExpiry 保持一致：attnEarliestExpiry 必须落在
// 'function expBatches' 与 'async function loadExpiry' 之间（位置红线，用例内校验）。
func TestAppJSAttnExpiryAlert(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; attn expiry test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const fnAt = src.indexOf('function attnEarliestExpiry');
if (fnAt < start || fnAt > end) throw new Error('attnEarliestExpiry must live inside expiry region');
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: () => ({ innerHTML: '', textContent: '', hidden: true }),
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.attnEarliestExpiry = attnEarliestExpiry;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
const out = [];
// ① 缓存有该账号且最早批次 5 天后 → 与到期卡同口径 {days:5}。
ctx.lastPackages = { accounts: [
  { uid: 'a', packages: [{ end_time: iso(5) + ' 00:00:00', remain: 1234 }] },
  { uid: 'err', error: 'offline', packages: [{ end_time: iso(1) + ' 00:00:00', remain: 99 }] },
] };
out.push(ctx.attnEarliestExpiry({ uid: 'a' }));
// 查询失败的账号不吃缓存明细，退回 overview 汇总（未来 1 天）。
out.push(ctx.attnEarliestExpiry({ uid: 'err', credits_earliest_expiry: iso(1) + 'T12:00:00',
  credits_earliest_remaining: 99 }));
// ② 缓存无数据 + overview 零值时间 → null（不产生预警）。
ctx.lastPackages = null;
out.push(ctx.attnEarliestExpiry({ uid: 'a', credits_earliest_expiry: '0001-01-01T00:00:00Z',
  credits_earliest_remaining: 500 }));
// ③ overview 未来 3 天 → 按日历日差折算 days:3。
const fut = new Date(); fut.setDate(fut.getDate() + 3); fut.setHours(12, 0, 0, 0);
out.push(ctx.attnEarliestExpiry({ uid: 'a', credits_earliest_expiry: fut.toISOString(),
  credits_earliest_remaining: 888 }));
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "attnexp-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("attn expiry node test failed: %v\n%s", err, out)
	}
	const want = `[{"days":5,"remain":1234},{"days":1,"remain":99},null,{"days":3,"remain":888}]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("attn expiry results=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSAccountCoolReasonNote 账号行「冷却原因」备注的渲染规则（2026-10-13 回归）：
// 仅「正在冷却」时显示 reason（与徽标倒计时互补）；已恢复（until 在过去）/ until
// 零值 / 无 reason 一律不显示——后端 statusOf 按查询期过滤 + flusher 5s 扫除双保险
// 之外，这里是前端最后一道防线（用户明确要求：恢复了就不要显示）。
func TestAppJSAccountCoolReasonNote(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cool reason note test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const mk = () => ({ innerHTML: '', textContent: '', hidden: false, dataset: {}, style: {},
  classList: { add() {}, remove() {}, toggle() {} },
  addEventListener() {}, removeEventListener() {}, focus() {}, blur() {}, click() {},
  querySelector: () => null, querySelectorAll: () => [], appendChild() {}, setAttribute() {}, removeAttribute() {} });
const sinks = { accBody: mk() };
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: '' }, history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: {
    querySelectorAll: () => [], querySelector: () => inert,
    getElementById: id => (sinks[id] || (sinks[id] = mk())),
    addEventListener() {}, documentElement: inert, head: inert, body: inert,
    createElement: () => inert, cookie: '',
  },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.renderAccounts = renderAccounts;', sandbox, { filename: 'panel.js' });
const past = new Date(Date.now() - 3600e3).toISOString();
const future = new Date(Date.now() + 600e3).toISOString();
const zero = '0001-01-01T00:00:00Z';
const base = { uid: 'aaaaaaaa-1111-2222-3333-444444444444', nickname: 't', realm: 'cn', credits: 100, credits_total: 200, success_count: 1, err_total: 0, in_flight: 0, token_usage: {}, last_success: past };
sandbox.renderAccounts([
  Object.assign({}, base, { reason: '429 rate limit', cool_remaining_sec: 600, until: future }),
  Object.assign({}, base, { uid: 'bbbbbbbb-1111', reason: '429 rate limit', cool_remaining_sec: 0, until: past }),
  Object.assign({}, base, { uid: 'cccccccc-1111', reason: '429 rate limit', cool_remaining_sec: 0, until: zero }),
  Object.assign({}, base, { uid: 'dddddddd-1111', reason: '', cool_remaining_sec: 0, until: zero }),
]);
const rows = sinks.accBody.innerHTML.split('<tr').slice(1);
const out = {
  cooling_plain: rows[0].includes('429 rate limit'),
  recovered_hidden: !rows[1].includes('429 rate limit') && !rows[1].includes('恢复'),
  zero_until_hidden: !rows[2].includes('429 rate limit'),
  no_reason_hidden: !rows[3].includes('429'),
};
process.stdout.write(JSON.stringify(out));
process.exit(0);`
	f, err := os.CreateTemp(t.TempDir(), "coolnote-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("cool reason note node test failed: %v\n%s", err, out)
	}
	const want = `{"cooling_plain":true,"recovered_hidden":true,"zero_until_hidden":true,"no_reason_hidden":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("cool reason note=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAcctCheckinTodayOnly 签到状态必须按自然日展示：昨天的“已领取”不能当作今天已签。
func TestAcctCheckinTodayOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; checkin display test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const mk = () => ({ innerHTML: '', textContent: '', value: '', hidden: false, dataset: {}, style: {},
  classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  addEventListener() {}, removeEventListener() {}, focus() {}, blur() {}, click() {},
  querySelector: () => null, querySelectorAll: () => [], appendChild() {}, setAttribute() {}, removeAttribute() {} });
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: '#accounts-center' }, history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert,
    getElementById: () => mk(), addEventListener() {}, documentElement: inert, head: inert, body: inert,
    createElement: () => inert, cookie: '', visibilityState: 'visible' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.acctCheckinText = acctCheckinText;\nthis.acctCheckinCycleStart = acctCheckinCycleStart;\nthis.acctCheckinToday = acctCheckinToday;', sandbox, { filename: 'panel.js' });
const now = new Date();
const at = d => new Date(now.getFullYear(), now.getMonth(), now.getDate() + d, 12, 0, 0).toISOString();
const call = ck => sandbox.acctCheckinText({ checkin: Object.assign({ supported: true }, ck) });
const qstart = (h, m) => sandbox.acctCheckinCycleStart('qoder', new Date(2026, 9, 20, h, m));
const qat = (h, m) => ({ platform: 'qoder', checkin: { supported: true, last_status: 'already', last_at: new Date(2026, 9, 20, h, m).toISOString() } });
const out = {
  today_already: call({ last_status: 'already', last_at: at(0), last_message: '今日已领取 100 credits' }),
  stale_already: call({ last_status: 'already', last_at: at(-1), last_message: '今日已领取 100 credits' }),
  today_failed: call({ last_status: 'failed', last_at: at(0), last_error: 'boom' }),
  stale_failed: call({ last_status: 'failed', last_at: at(-1), last_error: 'boom' }),
  unsupported: call({ supported: false }),
  qoder_before: qstart(8, 36).getTime() === new Date(2026, 9, 19, 10, 0).getTime(),
  qoder_edge: qstart(9, 59).getTime() === new Date(2026, 9, 19, 10, 0).getTime(),
  qoder_after: qstart(10, 1).getTime() === new Date(2026, 9, 20, 10, 0).getTime(),
  qoder_today_before: sandbox.acctCheckinToday(qat(8, 36), new Date(2026, 9, 20, 9, 0)),
  qoder_today_prev: sandbox.acctCheckinToday(qat(8, 36), new Date(2026, 9, 20, 10, 1)),
  qoder_today_after: sandbox.acctCheckinToday(qat(10, 30), new Date(2026, 9, 20, 11, 0)),
};
process.stdout.write(JSON.stringify(out));
process.exit(0);`
	f, err := os.CreateTemp(t.TempDir(), "checkin-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, err := exec.Command(node, f.Name(), mergedAppJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("checkin display node test failed: %v\n%s", err, raw)
	}
	var out struct {
		TodayAlready     string `json:"today_already"`
		StaleAlready     string `json:"stale_already"`
		TodayFailed      string `json:"today_failed"`
		StaleFailed      string `json:"stale_failed"`
		Unsupported      string `json:"unsupported"`
		QoderBefore      bool   `json:"qoder_before"`
		QoderEdge        bool   `json:"qoder_edge"`
		QoderAfter       bool   `json:"qoder_after"`
		QoderTodayBefore bool   `json:"qoder_today_before"`
		QoderTodayPrev   bool   `json:"qoder_today_prev"`
		QoderTodayAfter  bool   `json:"qoder_today_after"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if !strings.Contains(out.TodayAlready, "已签过") || !strings.Contains(out.TodayAlready, "今日已领取") {
		t.Errorf("今日 already = %q", out.TodayAlready)
	}
	if !strings.Contains(out.StaleAlready, "今日未签") || strings.Contains(out.StaleAlready, "今日已领取") || !strings.Contains(out.StaleAlready, "上次") {
		t.Errorf("昨日 already 不应冒充今日: %q", out.StaleAlready)
	}
	if !strings.Contains(out.TodayFailed, "失败") || !strings.Contains(out.TodayFailed, "boom") {
		t.Errorf("今日 failed = %q", out.TodayFailed)
	}
	if !strings.Contains(out.StaleFailed, "今日未签") || strings.Contains(out.StaleFailed, "boom") {
		t.Errorf("昨日 failed 不应显示为今日失败: %q", out.StaleFailed)
	}
	if !strings.Contains(out.Unsupported, "平台无此活动") {
		t.Errorf("unsupported = %q", out.Unsupported)
	}
	if !out.QoderBefore || !out.QoderEdge || !out.QoderAfter {
		t.Errorf("Qoder 周期边界错误: before=%v edge=%v after=%v", out.QoderBefore, out.QoderEdge, out.QoderAfter)
	}
	if out.QoderTodayBefore || out.QoderTodayPrev || !out.QoderTodayAfter {
		t.Errorf("Qoder 自然日判断错误: before=%v prev=%v after=%v", out.QoderTodayBefore, out.QoderTodayPrev, out.QoderTodayAfter)
	}
}

// TestPanelTrangeCalFlexBasis .tr-cal 的 flex-basis 必须为 0。
// auto 基准 = 内容宽（双月 ≈500px），与快捷列表（132px）相加超过 withcal 的
// 660px 时 flex-wrap 把日历挤到第二行并拉满全宽——弹层暴涨 ~230px，「飘」在
// 页面中间（2026-10-13 用户截图反馈，已修）。此断言防回归。
func TestPanelTrangeCalFlexBasis(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	idx := strings.Index(html, ".tr-cal {")
	if idx < 0 {
		t.Fatal("index.html missing .tr-cal rule")
	}
	rule := html[idx : idx+strings.Index(html[idx:], "}")]
	if !strings.Contains(rule, "flex: 1 1 0") {
		t.Fatalf(".tr-cal must use flex-basis 0 (otherwise calendar wraps below quick list): %s", rule)
	}
}

// TestExtChannelWizardDOM 外部渠道向导/卡片 DOM 与「不渲染 secret」断言。
//
// 为什么钉这些：
//   - 向导元素缺失 = 前端按钮点了没反应（app.js 的 go() 仍会调 loadExt，页面看似正常）；
//   - ext.js 只要出现 `.secret` 读取，就可能把秘密值渲染进 DOM（XSS/截图泄露面）。
func TestExtChannelWizardDOM(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, id := range []string{
		"extDrawer", "ecCards", "ecSummary", "btnEcNew",
		"extWizVeil", "ewName", "ewProtocol", "ewAdapter", "ewBase", "ewPath",
		"ewAuthMode", "ewAuthParam", "ewAuthHeader", "ewAuthHint", "ewHeaders",
		"ewModels", "ewAliases", "ewCreds", "ewAddCred", "ewNote",
		"ewProbe", "ewProbeOut", "ewSave", "ewCancel", "ewErr", "ewNative",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺外部渠道向导元素 id=%q", id)
		}
	}
	// 原生适配器下拉：空值 = 按协议，另有 loomy 一项。
	adapterSel := html[strings.Index(html, `id="ewAdapter"`):]
	adapterSel = adapterSel[:strings.Index(adapterSel, "</select>")]
	if !strings.Contains(adapterSel, `value=""`) || !strings.Contains(adapterSel, `value="loomy"`) {
		t.Error("ewAdapter 应有「默认（按协议）」与 loomy 两个选项")
	}
	// 情报表新增「草稿」列（表头 9 列，空态 colspan 必须同步，否则表格错位）。
	if !strings.Contains(html, "<th>草稿</th>") {
		t.Error("情报表缺「草稿」列")
	}
	if !strings.Contains(html, `colspan="9"`) {
		t.Error("情报空态 colspan 未同步为 9")
	}
	// extwiz.js 必须被 index.html 引入且排在 ext.js 之后。
	extIdx := strings.Index(html, `src="ext.js`)
	wizIdx := strings.Index(html, `src="extwiz.js`)
	if extIdx < 0 || wizIdx < 0 || wizIdx < extIdx {
		t.Error("index.html 必须按 ext.js → extwiz.js 顺序引入脚本")
	}

	// ext.js 不得读取任何 secret 字段（快照里本来也没有；防回归）。
	extJS, err := os.ReadFile("ext.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{".secret", "credentials[].secret", "api_key"} {
		if strings.Contains(string(extJS), bad) {
			t.Errorf("ext.js 出现了敏感字段引用 %q（状态快照不含秘密值，禁止渲染）", bad)
		}
	}
	// 向导 DOM 不得出现 api_key 输入框（老字段只读保留，向导只写 credentials）。
	wizHTML := html[strings.Index(html, `id="extWizVeil"`):]
	wizHTML = wizHTML[:strings.Index(wizHTML, "<!-- 配置 -->")]
	if strings.Contains(wizHTML, "api_key") {
		t.Error("向导不应出现 api_key 字段（凭证统一走 credentials[]）")
	}

	// 原生适配器接线：向导写 adapter、卡片渲染 adapter 徽标。
	wizJS, err := os.ReadFile("extwiz.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ewAdapter", "ewSyncAdapter", "ch.adapter"} {
		if !strings.Contains(string(wizJS), want) {
			t.Errorf("extwiz.js 缺原生适配器接线 %q", want)
		}
	}
	if !strings.Contains(string(extJS), "c.adapter") {
		t.Error("ext.js 应在渠道卡片上渲染 adapter 徽标")
	}
}

// 添加账号弹窗的入口归属断言（v1.7.2）：全局「添加账号」按钮在账号中心视图
// 分流到平台添加向导，CodeBuddy 登录弹窗只由页签内按钮打开——弹窗内保留一句
// 中性说明即可，「前往账号中心」跳转兜底按钮随之删除（入口唯一化，不再歧义）。
func TestAddAccountModalScope(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	// 中性说明：只说清"这个弹窗管 CodeBuddy"，其它平台去账号中心页签旁的按钮。
	if !strings.Contains(html, "此弹窗用于登录 CodeBuddy（WorkBuddy）账号池账号") {
		t.Error("添加账号弹窗缺中性归属说明")
	}
	if !strings.Contains(html, "对应平台页签旁的「添加账号」") {
		t.Error("说明应指到平台页签旁的「添加账号」按钮")
	}
	// 跳转兜底按钮已删（入口唯一化）：DOM 与 app.js 接线都不应再有。
	if strings.Contains(html, `id="addGoExtchan"`) {
		t.Error("addGoExtchan 按钮应已删除（v1.7.2 入口唯一化）")
	}
	if strings.Contains(html, "请到左侧「账号中心」") {
		t.Error("弹窗说明不应再让用户跳转（全局按钮已分流）")
	}
	// 向导首屏明确「选择要添加的平台」。
	if !strings.Contains(html, "选择要添加的平台") {
		t.Error("向导首屏缺「选择要添加的平台」文案")
	}
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(appJS)
	// 全局按钮分流：账号中心视图 → 平台向导；否则 → CodeBuddy 登录弹窗。
	for _, want := range []string{
		"$('btnAdd').onclick", "view === 'accounts-center'", "acctGlobalAddAccount", "openAdd()",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺全局按钮分流接线 %q", want)
		}
	}
	// 跳转兜底接线已删。
	if strings.Contains(s, "$('addGoExtchan')") {
		t.Error("app.js 仍保留 addGoExtchan 接线（应随按钮一起删除）")
	}
	// acct.js：全局入口（默认当前页签平台）+ 页签内按钮分流 + CodeBuddy 按钮文案。
	acctJS, err := os.ReadFile("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	a := string(acctJS)
	for _, want := range []string{
		"function acctGlobalAddAccount",
		"'添加 CodeBuddy 账号'",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("acct.js 缺入口唯一化接线 %q", want)
		}
	}
	// v1.7.3 灰 Tab 修复：按平台原因文案（三条 + 兜底）、置灰 title 双保险、
	// 智能默认（不支持的平台不落在灰 Tab）、下拉能力标记。
	// v1.7.4：平台切换统一入口——onchange 与向导打开都必须走 acctOnPlatformChange
	// （完整重算 Tab 置灰/原因文案/智能默认，杜绝"切平台后状态不刷新"）。
	for _, want := range []string{
		"function acctOnPlatformChange",
		"$('accAddPlatform').onchange = acctOnPlatformChange",
		"acctOnPlatformChange();", // 向导打开与能力翻转都走统一入口
	} {
		if !strings.Contains(a, want) {
			t.Errorf("acct.js 缺平台切换统一入口接线 %q", want)
		}
	}
	for _, want := range []string{
		"ACCT_BROWSER_BLOCKED_REASON",
		"Loomy 无登录流程——请从客户端登录态复制 session 后用粘贴凭证",
		"通用平台无统一登录——粘贴 API 凭证",
		"该平台暂不支持浏览器登录（当前支持：Qoder 国内/国际、AutoClaw 国内/国际、TRAE）",
		"b.title = supported ? '' : blockedMsg", // 置灰 + title 双保险
		"mode = 'paste'; // 智能默认：不支持的平台不落在灰 Tab 上",
		"acctSyncPlatformOptions",
		"✓ 浏览器登录",
		"· 粘贴凭证",
		// AutoClaw 国际版：滑块 + 回环 OAuth（不再是"仅粘贴"灰 Tab）。
		"'autoclaw-intl': 'AutoClaw 国际'",
		"accounts/login/captcha?platform=autoclaw-intl",
		"AliyunCaptcha.solve",
		"请完成阿里云滑块验证",
		// TRAE（v1.7.6）：浏览器登录接入（PKCE + 回环回调），不再灰 Tab。
		"trae: 'TRAE（SOLO 国内）'",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("acct.js 缺灰 Tab 修复接线 %q", want)
		}
	}
	if strings.Contains(a, "TRAE 浏览器登录下一批接入") {
		t.Error("TRAE 已接入浏览器登录，灰 Tab 原因文案不应残留")
	}
	// captcha.js（v1.7.6）：业务失败不得谎报滑块失败——否则阿里云 SDK 弹
	// "验证失败，请刷新"死循环，掩盖后端真实错误（实测 qZTAw7vhEb 闭环）。
	captchaJS, err := os.ReadFile("captcha.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"captchaResult: true, bizResult: false", // 业务失败 → bizResult=false
		"业务失败 ≠ 滑块失败",
	} {
		if !strings.Contains(string(captchaJS), want) {
			t.Errorf("captcha.js 缺回调语义修复 %q", want)
		}
	}
	// acct.js 页签内按钮：codebuddy → openAdd()（弹窗唯一直达入口）。
	if !strings.Contains(a, "openAdd(); // CodeBuddy 登录弹窗的唯一直达入口") {
		t.Error("acct.js 的 btnAcctAdd 应在 codebuddy 页签打开 CodeBuddy 登录弹窗")
	}
	// v1.7.5：CodeBuddy 并入统一平台下拉——首项 + ✓ 标记同款 + 选中走专用弹窗
	// （不走 acclogin，capabilities 不含它，前端内置）。
	if selStart := strings.Index(html, `id="accAddPlatform"`); selStart >= 0 {
		selEnd := strings.Index(html[selStart:], "</select>")
		if selEnd < 0 {
			t.Fatal("accAddPlatform select 未闭合")
		}
		sel := html[selStart : selStart+selEnd]
		firstOpt := strings.Index(sel, `<option value="codebuddy">CodeBuddy（WorkBuddy）</option>`)
		if firstOpt < 0 {
			t.Error("平台下拉缺 CodeBuddy 首项")
		} else if strings.Index(sel, "<option ") != firstOpt {
			t.Error("CodeBuddy 应是平台下拉的第一项")
		}
	}
	for _, want := range []string{
		"acctBrowserPrimaryAction",
		"打开 CodeBuddy 登录窗口",
		"不走 acclogin", // CodeBuddy 特判注释（capabilities 不含它）
		"codebuddy: 'CodeBuddy（WorkBuddy）'",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("acct.js 缺 CodeBuddy 并入接线 %q", want)
		}
	}
}

// TestAccountCenterDOM 账号中心（v1.6.0 T4）的 DOM 与接线断言。
//
// 为什么钉这些：
//   - 导航迁移是行为变更：主导航必须有「账号中心」且不再有「账号池 / 外部渠道」
//     直达项（外部渠道降级为渠道设置抽屉，PRD §6）；
//   - 老锚点重定向（#accounts / #extchan）是书签兼容承诺，漏了就断链；
//   - 4 处同步纪律（embed / 路由 / script 标签 / panelJSFiles）缺一即 404 或测试漏检。
func TestAccountCenterDOM(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// 导航：账号中心在，账号池 / 外部渠道直达项已移除。
	if !strings.Contains(html, `data-view="accounts-center"`) {
		t.Error("主导航缺「账号中心」入口")
	}
	if strings.Contains(html, `data-view="accounts"`) || strings.Contains(html, `data-view="extchan"`) {
		t.Error("主导航不应再有 账号池 / 外部渠道 直达项（已迁移/降级）")
	}

	// 视图骨架：双 section 组合 + 顶栏平台下拉 + 平台面板 + Hero + 抽屉。
	for _, id := range []string{"view-accounts", "view-acctcenter", "acctPlatformSelect", "acctHeroWrap",
		"acctHeroAccounts", "acctPanels",
		"btnAcctAdd", "btnCheckinAll", "btnExtDrawer", "btnExtDrawerClose", "btnAcctConsistency",
		"extDrawer"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺账号中心元素 id=%q", id)
		}
	}
	// 平台下拉选项（CodeBuddy + 5 个同构池：Loomy / AutoClaw / Qoder / TRAE /
	// 通用平台）。Qoder / AutoClaw 的国内/国际是池内区域，单区域池不重复成项。
	selStart := strings.Index(html, `id="acctPlatformSelect"`)
	if selStart < 0 {
		t.Fatal("index.html 缺 acctPlatformSelect")
	}
	selEnd := strings.Index(html[selStart:], "</select>")
	if selEnd < 0 {
		t.Fatal("acctPlatformSelect 未闭合")
	}
	sel := html[selStart : selStart+selEnd]
	for _, want := range []string{
		`<option value="qoder-pool">Qoder 账号池</option>`,
		`<option value="autoclaw-pool">AutoClaw 账号池</option>`,
		`<option value="loomy-pool">Loomy 账号池</option>`,
		`<option value="trae-pool">TRAE 账号池</option>`,
		`<option value="custom-pool">通用平台账号池</option>`,
	} {
		if !strings.Contains(sel, want) {
			t.Errorf("平台下拉缺同构池选项 %q", want)
		}
	}
	for _, old := range []string{`<option value="loomy">`, `<option value="trae">`, `<option value="custom">`} {
		if strings.Contains(sel, old) {
			t.Errorf("单区域平台不应再作为独立下拉项 %q（应并入 *-pool 入口）", old)
		}
	}
	if !strings.Contains(html, `id="acctRegionTabs"`) {
		t.Error("缺账号池区域切换 acctRegionTabs")
	}
	// Qoder 池分析区：负载趋势 / 在途与最近请求 / 模型锁池。
	for _, id := range []string{"acctPoolViz", "qoderLoadBox", "qoderLoadChart", "qoderLoadRange",
		"qoderLiveBox", "qoderLiveLanes", "qoderMlBox", "qoderMlBody",
		"qoderHero", "qoderHsCredits", "qoderHsReq7d", "qoderHsExpiryVal"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("缺 Qoder 池分析元素 id=%q", id)
		}
	}
	for _, k := range []string{"codebuddy", "loomy", "autoclaw", "autoclaw-intl", "qoder", "qoder-intl", "trae", "custom"} {
		if !strings.Contains(html, `<option value="`+k+`">`) {
			t.Errorf("缺平台下拉选项 %q", k)
		}
		if k != "codebuddy" {
			if !strings.Contains(html, `id="acctPanel-`+k+`"`) || !strings.Contains(html, `id="acBody-`+k+`"`) {
				t.Errorf("缺平台 %q 的面板/表体", k)
			}
		}
	}

	// 添加账号向导 DOM。
	for _, id := range []string{"accAddVeil", "accAddPlatform", "accAddCred", "accAddSchema",
		"accAddLabel", "accAddNote", "accAddErr", "accAddSave", "accAddCancel"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺添加账号向导元素 id=%q", id)
		}
	}

	// 老渠道视图的内部 id 必须原样保留（ext.js/extwiz.js 零改动的前提）。
	for _, id := range []string{"ecCards", "ecSummary", "ecNote", "eiFilters", "eiBody", "eiMeta"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("渠道设置抽屉缺原外部渠道 id=%q（ext.js 依赖）", id)
		}
	}

	// v1.7.2 入口唯一化：全局按钮分流 + CodeBuddy 弹窗中性说明（详见
	// TestAddAccountModalScope，这里只防 DOM 层面的旧文案/旧按钮回归）。
	if strings.Contains(html, `id="addGoExtchan"`) || strings.Contains(html, "请到左侧「") {
		t.Error("添加账号弹窗残留旧引导/跳转按钮（v1.7.2 已入口唯一化）")
	}

	// 脚本加载顺序：acct.js 必须在 extwiz.js 之后（运行时调用其同域函数）。
	acctIdx := strings.Index(html, `src="acct.js`)
	wizIdx := strings.Index(html, `src="extwiz.js`)
	if acctIdx < 0 || wizIdx < 0 || acctIdx < wizIdx {
		t.Error("index.html 必须按 … extwiz.js → acct.js 顺序引入脚本")
	}

	// 4 处同步纪律：embed 与静态路由（index.go / panel.go）。
	indexGo, err := os.ReadFile("index.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexGo), "acct.js") {
		t.Error("index.go 的 go:embed 缺 acct.js（二进制里没有该文件 = 浏览器 404）")
	}
	panelGo, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(panelGo), `GET /startport/acct.js`) {
		t.Error("panel.go 缺 GET /startport/acct.js 静态路由")
	}

	// app.js 接线：老锚点重定向 + 组级显隐 + 标题表。
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ACCT_CENTER_SECTIONS", `v === 'accounts' || v === 'extchan'`, "'accounts-center': '账号中心'"} {
		if !strings.Contains(string(appJS), want) {
			t.Errorf("app.js 缺账号中心接线片段 %q", want)
		}
	}

	// 浏览器登录向导（v1.7.0）：双方式 Tab + 两条流（qoder 授权链接 / autoclaw 短信）
	// 的 DOM 与接线断言。
	for _, id := range []string{
		"accAddMode", "accAddModeHint", "accAddBrowserPanel", "accAddPastePanel",
		"abHint", "abSmsRow", "abPhone", "abSendCode", "abCode", "abSubmit",
		"abUrlRow", "abUrl", "abCopyBtn", "abOpenBtn", "abStatus",
		"abStart", "abRestart", "abCancelLogin",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺浏览器登录向导元素 id=%q", id)
		}
	}
	acctJS, err := os.ReadFile("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"accounts/login/capabilities", // 服务端能力开关（规格 §3.4 不硬编码）
		"ACCT_BROWSER_PLATFORMS",      // 平台支持面（qoder 双区 / autoclaw 国内）
		"ACCT_LOGIN_POLL_MS = 2000",   // wait 轮询间隔 2s（规格 §3.1）
		"ACCT_SMS_RESEND_SEC = 60",    // 发码倒计时 60s（规格 R3）
		"accounts/login/start",        // 会话创建端点
		"accounts/login/wait",
		"accounts/login/sms/send",
		"accounts/login/sms/verify",
		"accounts/login/cancel",
		"等待授权完成，自动检测中",          // 轮询期 UI 文案
		"acctBrowserSupported",  // 模式切换的平台支持判定
		"acctLeaveCenterNotify", // 离开账号中心的会话提示钩子
	} {
		if !strings.Contains(string(acctJS), want) {
			t.Errorf("acct.js 缺浏览器登录接线片段 %q", want)
		}
	}
	// 契约回归：session_id 不得再走路径段（联调冻结为 body 形态）。
	for _, bad := range []string{"+ '/wait'", "+ '/sms_verify'", "+ '/cancel'"} {
		if strings.Contains(string(acctJS), bad) {
			t.Errorf("acct.js 出现路径段形态的登录端点 %q（应为 body 传 session_id）", bad)
		}
	}
	// app.js 必须在 go() 里挂acctLeaveCenterNotify 钩子（页面切换提示）。
	for _, want := range []string{"acctLeaveCenterNotify"} {
		if !strings.Contains(string(appJS), want) {
			t.Errorf("app.js 缺浏览器登录会话提示钩子 %q", want)
		}
	}

	// 迁入/回退接线（T5 契约冻结）：ext.js 必须直连两个端点，且回退前有确认语义
	// （架构 §7.3：二次确认由前端做）。managed 渠道显示「回退」、非托管显示「迁入」。
	extJS, err := os.ReadFile("ext.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`data-ec="migrate"`, `data-ec="rollback"`,
		"channels/' + encodeURIComponent(name) + '/migrate",
		"channels/' + encodeURIComponent(name) + '/rollback",
		"无需迁入", "无需回退", // 409 幂等语义的前端处理
	} {
		if !strings.Contains(string(extJS), want) {
			t.Errorf("ext.js 缺迁入/回退接线片段 %q", want)
		}
	}
}

// TestIndexHTMLUniqueIDs index.html 的所有 id= 必须唯一。
//
// 为什么钉住：经典脚本用 $(id)（= getElementById）绑事件——文档里出现两个同名
// id 时 getElementById 恒返回**第一个**，于是后加载模块的绑定覆盖先加载模块的
// 绑定、后一个同名按钮永远拿不到事件。实际踩坑（2026-10）：账号中心「全平台签到」
// 与 CodeBuddy 视图「全部签到」同名，结果前者是死按钮、后者执行了错误接口。
// 正则取 id="..." 字面量即可：面板 HTML 是模板产物，不存在动态拼接 id。
func TestIndexHTMLUniqueIDs(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\bid="([A-Za-z][\w-]*)"`)
	seen := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		id := m[1]
		if prev, dup := seen[id]; dup {
			t.Errorf("index.html 存在重复 id=%q（首现于第 %d 处匹配）", id, prev)
			continue
		}
		seen[id] = len(seen) + 1
	}
	// 两个签到按钮必须各自成 id 且都存在（防止有人改回去）。
	for _, want := range []string{"btnCheckinAll", "btnAcctCheckinAll"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("index.html 缺 id=%q", want)
		}
	}
}
