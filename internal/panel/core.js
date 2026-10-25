'use strict';
/* ── core.js：全局基础设施（状态 / 主题 / 界面风格 / 工具函数 / trange / 密钥门）──
   2026-10-13 自 app.js 拆出。本文件只含基础设施，不依赖任何视图模块，必须第一个
   加载；视图模块（app.js / keys.js）可随意引用此处标识符，反之不行。
   加载顺序见 index.html：core.js → app.js → keys.js。 */
/* ── 状态 ─────────────────────────────────────────────────────────── */
/* localStorage 品牌键（starport.*）。旧键（wb2api.* / wb2a.* / pkSortMode）是
 * 品牌改名前的历史值：读取时若新键缺失则回落到旧键并写回新键（迁移），保证
 * 用户已存的 api_key、主题偏好、忽略的版本号、排序偏好不丢失。 */
function lsGet(newKey, oldKey) {
  const v = localStorage.getItem(newKey);
  if (v) return v;                                   // 新键已有值：直接返回
  const old = localStorage.getItem(oldKey);          // 否则回读旧键（历史值）
  if (old) { localStorage.setItem(newKey, old); return old; }   // 迁移并写回新键
  return v;
}
const LS_KEY = 'starport.key', LS_THEME = 'starport.theme';
let theme = lsGet(LS_THEME, 'wb2api.theme') || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 界面风格（三档）与图标轨 ─────────────────────────────────────────
   风格：compact 紧凑 / cozy 舒适（默认）/ vivid 炫酷。切换 <html data-style>，
   持久化键 starport.style（沿用 starport.* 命名与 lsGet 旧键迁移风格）。
   图标轨：点击展开/收起左侧导航（72px ⇄ 208px），切换 <html data-rail>，
   持久化键 starport.rail。两者都只碰 documentElement 的 data-*，不影响数据逻辑。 */
const LS_STYLE = 'starport.style', LS_RAIL = 'starport.rail';
const UI_STYLES = ['compact', 'cozy', 'vivid'];
let uiStyle = lsGet(LS_STYLE, 'wb2api.style') || 'cozy';
if (UI_STYLES.indexOf(uiStyle) < 0) uiStyle = 'cozy';   // localStorage 脏值回落默认
/* 默认展开：只有用户显式收起过（存了 'closed'）才进收起态。 */
let railOpen = lsGet(LS_RAIL, 'wb2api.rail') !== 'closed';
function applyStyle() {
  document.documentElement.dataset.style = uiStyle;
  document.querySelectorAll('#styleSwitch button').forEach(b =>
    b.classList.toggle('on', b.dataset.ui === uiStyle));
}
function applyRail() {
  document.documentElement.dataset.rail = railOpen ? 'open' : 'closed';
  const b = $('btnRail');
  if (!b) return;
  b.setAttribute('aria-expanded', railOpen ? 'true' : 'false');
  const lbl = b.querySelector('.lbl');
  if (lbl) lbl.textContent = railOpen ? '收起导航' : '展开导航';
}
if ($('styleSwitch')) $('styleSwitch').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ui]');
  if (!b) return;
  uiStyle = b.dataset.ui;
  localStorage.setItem(LS_STYLE, uiStyle);
  applyStyle();
});
if ($('btnRail')) $('btnRail').onclick = () => {
  railOpen = !railOpen;
  localStorage.setItem(LS_RAIL, railOpen ? 'open' : 'closed');
  applyRail();
};
applyStyle();
applyRail();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = lsGet(LS_KEY, 'wb2api.key');
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/startport/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['yesterday', '昨天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['month', '本月'],
  ['lastmonth', '上月'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from, to, open, calY, calM, pickStart }

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

/* trDateKey 本地日期键 "YYYY-MM-DD"（日历的 data-d 与区间判定统一用它）。 */
function trDateKey(d) {
  const p2 = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate());
}

/* initCalNow 日历定位到 [上一月, 当前月]:右月 = 当前(「外面的时间」),
   左月 = 之前的一个月——与 DeepSeek 开放平台的布局一致。 */
function initCalNow(st) {
  const n = new Date();
  const d0 = new Date(n.getFullYear(), n.getMonth() - 1, 1);
  st.calY = d0.getFullYear();
  st.calM = d0.getMonth();
}

// trangeRender 渲染选择器：按钮（显示当前口径）+ 弹层（左快捷列表 + 右双月日历）。
// 弹层只在 st.open 时渲染；点击弹层外自动收起（bindTrOutsideOnce 全局一次）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  host.innerHTML = '<button type="button" class="tr-btn"><span>' + esc(trangeBtnLabel(id)) +
    '</span><svg class="tr-caret" viewBox="0 0 10 6" aria-hidden="true"><path d="M1 1l4 4 4-4"/></svg></button>' +
    (st.open ? trangePopHtml(id) : '');
  const btn = host.querySelector('.tr-btn');
  if (btn) btn.onclick = ev => {
    ev.stopPropagation();
    st.open = !st.open;
    if (st.open && !st.calY) initCalNow(st);
    trangeRender(id);
  };
  if (st.open) bindTrPop(id, host);
  bindTrOutsideOnce();
}

/* trangeBtnLabel 按钮上的紧凑口径：预设用名称；自定义日期区间（00:00→23:59:59
   的自然日形态）显示 "M-D → M-D"，其余自定义（保留旧精度）回退 trangeLabel。 */
function trangeBtnLabel(id) {
  const st = trangeState(id);
  if (st.preset !== 'custom') {
    const f = TRANGE_PRESETS.find(p => p[0] === st.preset);
    return f ? f[1] : '';
  }
  if (!st.from || !st.to) return '自定义…';
  const dayForm = st.from.getHours() === 0 && st.from.getMinutes() === 0 &&
    st.to.getHours() === 23 && st.to.getMinutes() === 59;
  if (dayForm) {
    const md = d => (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0');
    return md(st.from) + ' → ' + md(st.to);
  }
  return trangeLabel(id);
}

/* trangePopHtml 弹层：左快捷列表（含全部历史）+ 右双月日历 + 底部提示行。 */
function trangePopHtml(id) {
  const st = trangeState(id);
  // 日历只在「自定义…」模式下出现:默认弹层只有快捷口径列表(窄、干净),
  // 点自定义才在右侧展开双月日历(DeepSeek 同布局)。
  const showCal = st.preset === 'custom';
  const quick = TRANGE_PRESETS.map(([v, label]) =>
    '<button type="button" class="tr-q' + (st.preset === v ? ' on' : '') + '" data-q="' + v + '">' +
    esc(label) + (st.preset === v ? '<svg class="tr-check" viewBox="0 0 10 8" aria-hidden="true"><path d="M1 4l3 3 5-6"/></svg>' : '') +
    '</button>').join('');
  const now = new Date();
  const todayKey = trDateKey(now);
  const pick = st.pickStart || null;
  const fromKey = st.from ? trDateKey(st.from) : null;
  const toKey = st.to ? trDateKey(st.to) : null;
  let cal = '';
  for (let m = 0; m < 2; m++) {
    const y = st.calY + Math.floor((st.calM + m) / 12);
    const mo = (st.calM + m) % 12;
    const first = new Date(y, mo, 1);
    const lead = first.getDay(); // 0 = 周日,与 DeepSeek 的「日一二三四五六」对齐
    const dim = new Date(y, mo + 1, 0).getDate();
    let days = '';
    for (let b = 0; b < lead; b++) days += '<i></i>';
    for (let d = 1; d <= dim; d++) {
      const dte = new Date(y, mo, d);
      const key = trDateKey(dte);
      let cls = 'tr-d';
      if (dte > now) cls += ' dis';                 // 没到达的日子不可选
      if (key === todayKey) cls += ' today';
      if (fromKey && key === fromKey) cls += ' edge';
      if (toKey && key === toKey) cls += ' edge';
      if (fromKey && toKey && key > fromKey && key < toKey) cls += ' inr';
      if (pick && key === trDateKey(pick)) cls += ' edge';
      days += '<button type="button" class="' + cls + '" data-d="' + key + '"' +
        (cls.includes('dis') ? ' tabindex="-1"' : '') + '>' + d + '</button>';
    }
    const navL = m === 0 ? '<button type="button" class="tr-nav" data-nav="prev" aria-label="上个月">‹</button>'
      : '<span class="tr-nav ghost">‹</span>';
    const navR = m === 1 ? '<button type="button" class="tr-nav" data-nav="next" aria-label="下个月">›</button>'
      : '<span class="tr-nav ghost">›</span>';
    cal += '<div class="tr-mon"><div class="tr-mon-h">' + navL +
      '<span>' + y + ' 年 ' + (mo + 1) + ' 月</span>' + navR + '</div>' +
      '<div class="tr-wk">' + '日一二三四五六'.split('').map(c => '<i>' + c + '</i>').join('') + '</div>' +
      '<div class="tr-days">' + days + '</div></div>';
  }
  // 提示行刻意短句:这行的宽度会参与 max-content 撑宽弹层,长句曾把弹层
  // 撑出一条大空白带(日历只占左半,右侧全空)。
  const hint = st.pickStart
    ? '已选 ' + trDateKey(st.pickStart).slice(5) + ' · 点另一个日期完成（再点同一天 = 单日）'
    : '点两天生成区间 · 灰色为未到达的日子';
  return '<div class="tr-pop' + (showCal ? ' withcal' : '') + '"><div class="tr-quick' +
    (showCal ? '' : ' solo') + '">' + quick + '</div>' +
    (showCal ? '<div class="tr-cal"><div class="tr-mons">' + cal + '</div>' +
      '<div class="tr-rangefoot">' + trRangeFootHtml(id) + '</div></div>' +
      '<div class="tr-hint">' + esc(hint) + '</div>' : '') +
    '</div>';
}

/* trRangeFootHtml 日历列底部的区间摘要条：实时反映当前 from → to 与跨度天数
   （点选过程中即时更新，与 hint 行的操作引导互补）。同时承担「填充」职责——
   字体度量差异下快捷列表比日历高出一截，摘要条把日历列撑满，不留右下角空白
   （2026-10-13 用户反馈「红框处特别空」）。 */
function trRangeFootHtml(id) {
  const st = trangeState(id);
  const f = st.from, t = st.to;
  const fmt = d => (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  let txt = '未选择区间';
  if (f && t) {
    const dayOf = d => new Date(d.getFullYear(), d.getMonth(), d.getDate());
    const days = Math.max(1, Math.round((dayOf(t) - dayOf(f)) / 86400000) + 1);
    txt = fmt(f) + '  →  ' + fmt(t) + '（' + days + ' 天）';
  } else if (f) {
    txt = fmt(f) + '  →  待选择结束日';
  }
  return '<span class="tr-rf-k">当前区间</span><span class="tr-rf-v">' + esc(txt) + '</span>';
}

/* bindTrPop 弹层内交互:快捷口径立即生效并收起;「自定义…」停留进入日历点选;
   日历两天完成区间(自动交换首尾),同日两次 = 单日。弹层内点击不冒泡到
   document 的"点外收起"监听。 */
function bindTrPop(id, host) {
  const pop = host.querySelector('.tr-pop');
  if (!pop) return;
  pop.addEventListener('click', ev => ev.stopPropagation());
  pop.querySelectorAll('.tr-q').forEach(b => {
    b.onclick = () => {
      const st = trangeState(id);
      st.preset = b.dataset.q;
      st.pickStart = null;
      if (st.preset === 'custom') {
        initCalNow(st); // 每次进入自定义都重定位到 [上一月, 当前月],不残留历史翻页位置
        trangeRender(id); // 停留在弹层,进入日历点选
        return;
      }
      st.open = false;
      trangeRender(id);
      trangeEmit(id);
    };
  });
  pop.querySelectorAll('[data-nav]').forEach(b => {
    b.onclick = () => {
      const st = trangeState(id);
      const d0 = new Date(st.calY, st.calM + (b.dataset.nav === 'next' ? 1 : -1), 1);
      st.calY = d0.getFullYear();
      st.calM = d0.getMonth();
      trangeRender(id);
    };
  });
  pop.querySelectorAll('.tr-d:not(.dis)').forEach(b => {
    b.onclick = () => {
      const st = trangeState(id);
      const parts = b.dataset.d.split('-').map(Number);
      const d00 = new Date(parts[0], parts[1] - 1, parts[2]);
      if (!st.pickStart) {
        st.pickStart = d00;
        st.preset = 'custom';
        st.from = d00;
        st.to = null;
        trangeRender(id);
        return;
      }
      const a = st.pickStart, b2 = d00;
      const lo = a <= b2 ? a : b2;
      const hi = a <= b2 ? b2 : a;
      st.from = new Date(lo.getFullYear(), lo.getMonth(), lo.getDate());
      st.to = new Date(hi.getFullYear(), hi.getMonth(), hi.getDate(), 23, 59, 59);
      st.pickStart = null;
      st.open = false;
      trangeRender(id);
      trangeEmit(id);
    };
  });
}

/* bindTrOutsideOnce 点击弹层外收起全部弹层(全局只绑一次;测试桩无 DOM 时跳过)。 */
let trangeOutsideBound = false;
function bindTrOutsideOnce() {
  if (trangeOutsideBound || typeof document === 'undefined' || !document.addEventListener) return;
  trangeOutsideBound = true;
  document.addEventListener('click', () => {
    let dirty = false;
    trangeStates.forEach(st => { if (st.open) { st.open = false; dirty = true; } });
    if (dirty) trangeStates.forEach((st, hid) => trangeRender(hid));
  });
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/昨天/本月/上月/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发（滚动端点显式 hours=0，见下）。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === 'yesterday') {
    const y = new Date(); y.setDate(y.getDate() - 1); y.setHours(0, 0, 0, 0);
    const end = new Date(y); end.setDate(end.getDate() + 1); end.setSeconds(end.getSeconds() - 1);
    q.set('from', sec(y));
    q.set('to', sec(end));
    return q;
  }
  if (st.preset === 'month') {
    const n = new Date();
    q.set('from', sec(new Date(n.getFullYear(), n.getMonth(), 1)));
    return q;
  }
  if (st.preset === 'lastmonth') {
    const n0 = new Date(); n0.setDate(1); n0.setHours(0, 0, 0, 0);
    const p0 = new Date(n0); p0.setMonth(p0.getMonth() - 1);
    const end = new Date(n0); end.setSeconds(end.getSeconds() - 1);
    q.set('from', sec(p0));
    q.set('to', sec(end));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}

function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
    // 首屏是在**没有密钥**的情况下渲染的：各视图的加载器都在那次 401 里失败，并把
    // 错误写死进了 DOM（账号中心会一直显示「读取失败：密钥无效或未填写」，点刷新
    // 也回不来）。这里重放一次当前路由，让当前视图用新密钥重新取数。
    window.dispatchEvent(new HashChangeEvent('hashchange'));
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });
// 输入框包在 <form> 里（只为让浏览器/密码管理器识别成登录字段），但页面 CSP 是
// form-action 'none'（全站无表单提交目标）——回车会触发隐式提交，浏览器每次都报
// 「violates the following CSP directive: form-action 'none'」到控制台，看起来像
// 面板坏了。提交路径统一走上面的 keydown → btnKey.click()，这里直接掐掉默认提交。
if ($('keyInput').form) $('keyInput').form.addEventListener('submit', e => e.preventDefault());

