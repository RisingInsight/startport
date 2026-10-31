// checkin_frontend_test.go 一键签到的前端契约（v1.7.4）：
//   - acctCheckinBatchPlatforms 的范围语义（全部页签 = 全量；池页签 = 当前区域
//     的平台集合；平台页签 = 该平台）；
//   - acctCheckinAll 的请求形态（platforms 过滤）与结果 toast（成功汇总 / 失败
//     带账号与原因）；
//   - 一键签到按钮随范围联动（无可签账号置灰）。
//
// 用 node-vm 沙箱执行合并后的面板脚本（不触网；node 缺失则跳过）。
// 装置：argv[2] = 合并面板 JS，argv[3] = 测试片段（第二个脚本执行，覆盖
// api/toast 后驱动 acctCheckinAll，结果写回 this.__out）。
package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// acctCheckinBatchSnippet node 沙箱里的测试片段（在合并面板脚本之后执行）。
const acctCheckinBatchSnippet = `
this.__out = (async () => {
  const out = {};
  // 范围选择：全部页签 = 全量；池页签随区域；平台页签 = 该平台。
  acctTab = 'all'; acctPoolRegion = 'all';
  out.all = acctCheckinBatchPlatforms();
  acctTab = 'qoder-pool'; acctPoolRegion = 'cn';
  out.qoderCn = acctCheckinBatchPlatforms();
  acctTab = 'autoclaw-pool'; acctPoolRegion = 'all';
  out.autoclawAll = acctCheckinBatchPlatforms();
  acctTab = 'loomy'; acctPoolRegion = 'all';
  out.loomyTab = acctCheckinBatchPlatforms();
  // 一键签到：成功汇总 → ok toast + platforms 过滤体 + 列表刷新。
  const calls = []; const toasts = []; let reloads = 0;
  api = async (p, o) => { calls.push({ p: p, o: o || null }); return { ok: true,
    message: '一键签到完成：成功 1 · 已签过 1 · 失败 0 · 跳过 0',
    summary: { total: 2, ok: 1, already: 1, failed: 0, skipped: 0 },
    results: [{ id: 'a1', label: 'L1', platform: 'qoder', status: 'ok', reward: '100 credits', message: '签到成功' }] }; };
  toast = (m, t) => { toasts.push({ m: m, t: t }); };
  loadAcctAccounts = async () => { reloads++; };
  await acctCheckinAll(null);
  out.okPath = calls[0] && calls[0].p;
  out.okBody = (calls[0] && calls[0].o && calls[0].o.body) || '';
  out.okToast = toasts[0] && toasts[0].m;
  out.okToastCls = toasts[0] && toasts[0].t;
  out.okReloads = reloads;
  // 一键签到：失败 → bad toast 带首个账号与原因，仍刷新列表。
  calls.length = 0; toasts.length = 0; reloads = 0;
  api = async () => ({ ok: false,
    message: '一键签到完成：成功 0 · 已签过 0 · 失败 1 · 跳过 1',
    summary: { total: 2, ok: 0, already: 0, failed: 1, skipped: 1 },
    results: [{ id: 'a1', label: 'L1', platform: 'qoder', status: 'failed', message: '登录态已失效，请重新登录' }] });
  await acctCheckinAll(null);
  out.failToast = toasts[0] && toasts[0].m;
  out.failToastCls = toasts[0] && toasts[0].t;
  out.failReloads = reloads;
  // 按钮联动：无可签账号置灰；有则可点且文案为「一键签到」。
  const btn = { disabled: false, textContent: '', title: '' };
  document.getElementById = (id) => (id === 'btnAcctCheckinAll' ? btn : mk());
  syncAcctCheckinAllBtn([]);
  out.btnEmptyDisabled = btn.disabled;
  syncAcctCheckinAllBtn([{ capabilities: { checkin: true } }]);
  out.btnReady = !btn.disabled && btn.textContent === '一键签到' && btn.title.indexOf('1 个') >= 0;
  return out;
})();`

func TestAcctCheckinBatchFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; checkin batch frontend test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const snippet = fs.readFileSync(process.argv[3], 'utf8');
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
vm.runInContext(src, sandbox, { filename: 'panel.js' });
vm.runInContext(snippet, sandbox, { filename: 'snippet.js' });
sandbox.__out.then(out => { process.stdout.write(JSON.stringify(out)); process.exit(0); },
  e => { console.error(e && e.stack || e); process.exit(1); });`
	f, err := os.CreateTemp(t.TempDir(), "checkin-batch-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	sf, err := os.CreateTemp(t.TempDir(), "checkin-batch-snippet-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.WriteString(acctCheckinBatchSnippet); err != nil {
		t.Fatal(err)
	}
	sf.Close()
	raw, err := exec.Command(node, f.Name(), mergedAppJSPath(t), sf.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("checkin batch frontend test failed: %v\n%s", err, raw)
	}
	var out struct {
		All              []any  `json:"all"`
		QoderCn          []any  `json:"qoderCn"`
		AutoClawAll      []any  `json:"autoclawAll"`
		LoomyTab         []any  `json:"loomyTab"`
		OKPath           string `json:"okPath"`
		OKBody           string `json:"okBody"`
		OKToast          string `json:"okToast"`
		OKToastCls       string `json:"okToastCls"`
		OKReloads        int    `json:"okReloads"`
		FailToast        string `json:"failToast"`
		FailToastCls     string `json:"failToastCls"`
		FailReloads      int    `json:"failReloads"`
		BtnEmptyDisabled bool   `json:"btnEmptyDisabled"`
		BtnReady         bool   `json:"btnReady"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(out.All) != 0 {
		t.Errorf("全部页签应返回空数组（全量），实际 %v", out.All)
	}
	if len(out.QoderCn) != 1 || out.QoderCn[0] != "qoder" {
		t.Errorf("Qoder 池国内区域应只含 qoder，实际 %v", out.QoderCn)
	}
	if len(out.AutoClawAll) != 2 || out.AutoClawAll[0] != "autoclaw" || out.AutoClawAll[1] != "autoclaw-intl" {
		t.Errorf("AutoClaw 池全部区域应含国内+国际，实际 %v", out.AutoClawAll)
	}
	if len(out.LoomyTab) != 1 || out.LoomyTab[0] != "loomy" {
		t.Errorf("平台页签应只含该平台，实际 %v", out.LoomyTab)
	}
	if out.OKPath != "accounts/platform_checkin_all" {
		t.Errorf("请求路径 = %q", out.OKPath)
	}
	if !strings.Contains(out.OKBody, `"platforms"`) {
		t.Errorf("请求体应带 platforms 过滤: %q", out.OKBody)
	}
	if !strings.Contains(out.OKToast, "成功 1") || !strings.Contains(out.OKToast, "已签 1") || out.OKToastCls != "ok" {
		t.Errorf("成功 toast 异常: %q (%s)", out.OKToast, out.OKToastCls)
	}
	if out.OKReloads != 1 {
		t.Errorf("签到后应刷新账号列表，实际 %d 次", out.OKReloads)
	}
	if out.FailToastCls != "bad" || !strings.Contains(out.FailToast, "L1") ||
		!strings.Contains(out.FailToast, "登录态已失效") {
		t.Errorf("失败 toast 应带账号与原因: %q (%s)", out.FailToast, out.FailToastCls)
	}
	if out.FailReloads != 1 {
		t.Errorf("失败后也应刷新列表，实际 %d 次", out.FailReloads)
	}
	if !out.BtnEmptyDisabled {
		t.Errorf("无可签账号时按钮应置灰")
	}
	if !out.BtnReady {
		t.Errorf("有可签账号时按钮应可点且文案为「一键签到」")
	}
}

// TestAcctCheckinBatchHTMLContract 静态契约：按钮文案与请求键名钉住（纯 Go，
// 不依赖 node——改文案/键名必须同步这里）。
func TestAcctCheckinBatchHTMLContract(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="btnAcctCheckinAll"`) {
		t.Error("index.html 缺一键签到按钮 btnAcctCheckinAll")
	}
	if !strings.Contains(string(html), ">一键签到</button>") {
		t.Error("index.html 一键签到按钮文案应为「一键签到」")
	}
	js, err := os.ReadFile("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"accounts/platform_checkin_all",
		"{ platforms: platforms }",           // 请求体键名（范围过滤）
		"function acctCheckinBatchPlatforms", // 范围选择
		"function syncAcctCheckinAllBtn",     // 按钮联动
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("acct.js 缺 %q", want)
		}
	}
}
