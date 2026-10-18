'use strict';
/* ── acct.js：账号中心（v1.6.0 · T4）────────────────────────────────
   自包含模块：只依赖 core.js 的 $ / esc / ago / api / toast。

   信息架构（架构 §7 / PRD §6）：
     - 平台分页签：CodeBuddy（复用原账号池视图 DOM，并表不同存储）/ Loomy /
       AutoClaw 国内·国际 / Qoder 国内·国际 / 通用平台；
     - 添加账号向导：选平台 → 按平台 schema 粘贴凭证 JSON → POST /api/accounts
       （服务端 account.ValidateCred 是权威校验，报错带字段名；前端只做同口径
       预检，减少一次往返）；
     - 签到：单账号 + 全平台（T3 契约：POST /api/accounts/{id}/checkin 与
       POST /api/accounts/checkin_all；T3 未上线时 404 优雅降级，不报错轰炸）；
     - 「渠道设置」高级抽屉：原「外部渠道」页全部能力（DOM 原样搬入，内部 id
       不变，ext.js/extwiz.js 零改动），主导航入口移除，#extchan 锚点重定向。

   安全红线：GET /api/accounts 只出状态不出 blob（服务端保证）；本文件不渲染
   任何 secret 字段，也不提供凭证编辑（改凭证 = 删了重加，P0 口径）。 */

/* 平台元数据（与 account.ValidateCred / 架构 §3.1 blob 表同口径）。 */
const ACCT_PLATFORMS = {
  loomy: {
    name: 'Loomy',
    sample: '{\n  "session": "<Loomy 客户端登录态里的 session，长度 ≥16>"\n}',
    schema: '必填 session（长度 ≥16）。登录失效（401 / 业务码 020002）时账号标记「待重登」，重新粘贴 session 即恢复。',
    required: ['session']
  },
  autoclaw: {
    name: 'AutoClaw 国内',
    sample: '{\n  "token": "<桌面端 openclaw.json 的 X-Authorization token>",\n  "refresh_token": "可选：有才能自动刷新",\n  "device_id": "可选",\n  "user_id": "可选"\n}',
    schema: '必填 token；refresh_token / device_id / user_id 可选——缺 refresh_token 时账号页会提示「无法自动刷新」。',
    required: ['token']
  },
  'autoclaw-intl': {
    name: 'AutoClaw 国际',
    sample: '{\n  "token": "<国际版 token>",\n  "refresh_token": "可选：有才能自动刷新"\n}',
    schema: '必填 token；可选字段同国内版。国际版无签到活动（签到列显示「平台无此活动」）。',
    required: ['token']
  },
  qoder: {
    name: 'Qoder 国内',
    sample: '{\n  "access_token": "dt-… 或 jt-…",\n  "user_id": "<COSY 签名用户标识>",\n  "machine_id": "可选",\n  "refresh_token": "可选"\n}',
    schema: '必填 access_token（dt-/jt- 前缀皆可）与 user_id（COSY 签名需要）；machine_id / refresh_token 可选。',
    required: ['access_token', 'user_id']
  },
  'qoder-intl': {
    name: 'Qoder 国际',
    sample: '{\n  "access_token": "dt-… 或 jt-…",\n  "user_id": "<用户标识>"\n}',
    schema: '必填同 Qoder 国内。国际版无签到活动。',
    required: ['access_token', 'user_id']
  },
  custom: {
    name: '通用平台',
    sample: '{\n  "base_url": "https://api.example.com",\n  "secret": "<API key>",\n  "protocol": "openai",\n  "auth": { "mode": "bearer" }\n}',
    schema: '必填 base_url（http(s)，过 SSRF 红线）与 secret；可选 protocol（openai/anthropic/gemini，缺省 openai）、auth{}、headers{}、allow_private。每个账号投影为一条独立 c-<id> 托管渠道。',
    required: ['base_url', 'secret']
  }
};

/* 账号五态（架构 §4.6）→ [文案, 徽标 class]。 */
const ACCT_STATES = {
  healthy: ['可用', 'ok'],
  cooling: ['冷却', 'warn'],
  invalid: ['失效', 'bad'],
  disabled: ['停用', 'mute'],
  unprojected: ['未投影', 'warn']
};

/* 签到状态 → 文案。 */
function acctCheckinText(a) {
  const ck = a.checkin || {};
  if (!ck.supported) return '<span class="tag mute">平台无此活动</span>';
  let st = '';
  if (ck.last_status === 'ok') st = '<span class="tag ok">今日已签</span>';
  else if (ck.last_status === 'already') st = '<span class="tag ok">已签过</span>';
  else if (ck.last_status === 'failed') st = '<span class="tag bad">失败</span>'
    + (ck.last_error ? '<div class="note" style="color:var(--bad)">' + esc(ck.last_error) + '</div>' : '');
  else st = '<span class="note">未签到</span>';
  if (ck.last_at && ck.last_status) {
    st += '<div class="note">' + esc(ago(ck.last_at)) + (ck.total ? ' · 累计 ' + ck.total + ' 次' : '') + '</div>';
  }
  return st;
}

/* 账号行状态（五态）：channel 缺失 = 未投影。 */
function acctStateOf(a) {
  if (a.disabled) return 'disabled';
  const ch = a.channel;
  if (!ch || !ch.exists) return 'unprojected';
  if (ch.state === 'disabled') return 'disabled';
  if (ch.state === 'invalid') return 'invalid';
  if (ch.state === 'cooling') return 'cooling';
  return 'healthy';
}

function acctStateHtml(a) {
  const key = acctStateOf(a);
  const st = ACCT_STATES[key] || [key, 'mute'];
  let html = '<span class="tag ' + st[1] + '">' + esc(st[0]) + '</span>';
  const ch = a.channel;
  if (key === 'invalid' && ch && ch.invalid_reason) {
    const why = ch.invalid_reason === 'auth' ? '待重登' : (ch.invalid_reason === 'quota' ? '额度耗尽' : ch.invalid_reason);
    html += '<div class="note">' + esc(why) + '</div>';
  }
  if (key === 'cooling' && ch && ch.cool_remaining_sec > 0) {
    html += '<div class="note">' + dur(ch.cool_remaining_sec) + '</div>';
  }
  if (key === 'unprojected') {
    html += '<div class="note">托管渠道缺失，保存配置或重启后由 Reconcile 重建</div>';
  }
  return html;
}

/* ── 视图状态 ─────────────────────────────────────────────────────── */

let acctTab = 'codebuddy';
let acctAccounts = [];
let acctLoaded = false;
let acctDrawerOpen = false;
let acctLoading = false;

/* 页签切换 / 抽屉开合的统一应用点（go() 与本模块都走这里，避免两处真值）。 */
function applyAcctTab() {
  const inCenter = view === 'accounts-center';
  // 页签按钮态。
  if ($('acctTabs')) {
    $('acctTabs').querySelectorAll('button[data-acct]').forEach(b => {
      b.classList.toggle('on', b.dataset.acct === acctTab);
    });
  }
  // 抽屉优先：展开时盖住平台面板（CodeBuddy 视图也一并让位）。
  const showPanels = inCenter && !acctDrawerOpen;
  if ($('extDrawer')) $('extDrawer').hidden = !(inCenter && acctDrawerOpen);
  if ($('acctPanels')) $('acctPanels').hidden = !showPanels || acctTab === 'codebuddy';
  // CodeBuddy 页签 = 原 view-accounts（DOM 原样，仅显隐切换）。
  if ($('view-accounts')) $('view-accounts').hidden = !(inCenter && acctTab === 'codebuddy' && !acctDrawerOpen);
  Object.keys(ACCT_PLATFORMS).forEach(k => {
    const el = $('acctPanel-' + k);
    if (el) el.hidden = !showPanels || acctTab !== k;
  });
  syncAcctTopbar();
  // 抽屉按钮文案随开合。
  if ($('btnExtDrawer')) $('btnExtDrawer').textContent = acctDrawerOpen ? '收起渠道设置 ▴' : '渠道设置 ▾';
}

/* 顶部「添加账号」按钮只对 CodeBuddy 页签有意义（其它页签用页签内按钮）。 */
function syncAcctTopbar() {
  if ($('btnAdd')) $('btnAdd').hidden = (view === 'accounts-center' && acctTab !== 'codebuddy');
}

/* go('accounts-center') 的入口（app.js 调用）。 */
function loadAcctCenter() {
  applyAcctTab();
  loadAcctAccounts();
  if (acctDrawerOpen && typeof loadExt === 'function') loadExt();
}

/* ── 账号列表 ─────────────────────────────────────────────────────── */

async function loadAcctAccounts() {
  if (acctLoading) return;
  acctLoading = true;
  try {
    const d = await api('accounts');
    acctAccounts = d.accounts || [];
    acctLoaded = true;
    renderAcctAll();
  } catch (e) {
    const msg = e.message || '';
    Object.keys(ACCT_PLATFORMS).forEach(k => {
      const tb = $('acBody-' + k);
      if (tb) tb.innerHTML = '<tr><td colspan="7" class="empty">读取失败：' + esc(msg) + '</td></tr>';
    });
  } finally {
    acctLoading = false;
  }
}

function renderAcctAll() {
  const total = acctAccounts.length;
  const bad = acctAccounts.filter(a => ['invalid', 'unprojected'].includes(acctStateOf(a))).length;
  const usable = acctAccounts.filter(a => acctStateOf(a) === 'healthy').length;
  if ($('acctNote')) {
    $('acctNote').textContent = total
      ? total + ' 个平台账号 · ' + usable + ' 可用' + (bad ? ' · ' + bad + ' 异常' : '')
      : '暂无平台账号';
  }
  Object.keys(ACCT_PLATFORMS).forEach(k => renderAcctPlatform(k));
}

function renderAcctPlatform(platform) {
  const tb = $('acBody-' + platform);
  if (!tb) return;
  const list = acctAccounts.filter(a => a.platform === platform);
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7" class="empty"><div class="big">还没有 ' + esc(ACCT_PLATFORMS[platform].name)
      + ' 账号</div>点上方「添加账号」粘贴凭证 JSON，保存后自动投影为托管渠道</div></tr>';
    return;
  }
  tb.innerHTML = list.map(a => renderAcctRow(a)).join('');
}

function renderAcctRow(a) {
  const st = acctStateHtml(a);
  const balance = a.balance && typeof a.balance.available === 'number'
    ? esc(a.balance.available) + (a.balance.unit ? ' ' + esc(a.balance.unit) : '')
      + (a.balance.plan_name ? '<div class="note">' + esc(a.balance.plan_name) + '</div>' : '')
    : '<span class="note">—</span>';
  const disabled = !!a.disabled;
  const actions =
    '<button type="button" class="xs" data-act="toggle" data-id="' + esc(a.id) + '">'
    + (disabled ? '启用' : '停用') + '</button>'
    + '<button type="button" class="xs" data-act="probe" data-id="' + esc(a.id) + '">测试</button>'
    + ((a.checkin && a.checkin.supported)
      ? '<button type="button" class="xs" data-act="checkin" data-id="' + esc(a.id) + '">签到</button>' : '')
    + '<button type="button" class="xs" data-act="del" data-id="' + esc(a.id) + '">删除</button>';
  return '<tr' + (disabled ? ' style="opacity:.55"' : '') + '>'
    + '<td><b>' + esc(a.label) + '</b>'
    + (a.note ? '<div class="note">' + esc(a.note) + '</div>' : '')
    + '<div class="note"><code>' + esc(a.id) + '</code></div></td>'
    + '<td>' + st + '</td>'
    + '<td>' + balance + '</td>'
    + '<td>' + acctCheckinText(a) + '</td>'
    + '<td>' + (a.created_at ? esc(ago(a.created_at)) : '—') + '</td>'
    + '<td>' + actions + '</td>'
    + '</tr>';
}

/* ── 行动作（事件委托：行由 innerHTML 渲染，逐个绑定会丢） ────────── */

function bindAcctTable(platform) {
  const tb = $('acBody-' + platform);
  if (!tb) return;
  tb.onclick = async e => {
    const b = e.target.closest('button[data-act]');
    if (!b) return;
    const id = b.dataset.id;
    const a = acctAccounts.find(x => x.id === id);
    if (b.dataset.act === 'toggle') {
      await acctSetState(id, !(a && a.disabled), b);
    } else if (b.dataset.act === 'probe') {
      await acctProbe(id, b);
    } else if (b.dataset.act === 'checkin') {
      await acctCheckin(id, b);
    } else if (b.dataset.act === 'del') {
      await acctDelete(id, a ? a.label : id);
    }
  };
}
Object.keys(ACCT_PLATFORMS).forEach(bindAcctTable);

async function acctSetState(id, disabled, btn) {
  if (btn) { btn.disabled = true; }
  try {
    await api('accounts/' + encodeURIComponent(id) + '/state', {
      method: 'POST', body: JSON.stringify({ disabled: disabled })
    });
    toast(disabled ? '账号已停用（托管渠道同步下线）' : '账号已启用', 'ok');
    await loadAcctAccounts();
  } catch (e) {
    toast('操作失败：' + e.message, 'bad');
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function acctDelete(id, label) {
  if (!confirm('删除账号 ' + label + '？其凭证会同时从托管渠道移除，无法恢复。')) return;
  try {
    await api('accounts/' + encodeURIComponent(id), { method: 'DELETE' });
    toast('已删除账号 ' + label, 'ok');
    await loadAcctAccounts();
  } catch (e) {
    toast('删除失败：' + e.message, 'bad');
  }
}

/* acctProbe 只读探测（沿用 P0-7 语义：不落盘、不污染状态）。 */
async function acctProbe(id, btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '测试中…'; }
  try {
    const d = await api('accounts/' + encodeURIComponent(id) + '/probe', { method: 'POST' });
    const r = d.result || {};
    if (r.ok) {
      toast('凭证有效：' + (r.latency_ms || 0) + 'ms · ' + (r.models_count || 0) + ' 个模型', 'ok');
    } else {
      toast('探测失败：' + (r.error || ('HTTP ' + (r.status || '?'))), 'bad');
    }
  } catch (e) {
    toast('探测失败：' + e.message, 'bad');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* ── 签到（T3 已实现契约；路由缺失时优雅降级） ──────────────────────
   单账号：POST /api/accounts/{id}/platform_checkin → {ok, result:{status,reward,message}}
   全平台：POST /api/accounts/platform_checkin_all   → {ok, message}（串行执行）
   501 = 调度器未注入；404 = 二进制里没有 T3 路由（旧版本面板/新配置混部）。 */

function acctCheckinDegrade(e) {
  const m = (e && e.message) || '';
  return m === 'HTTP 404' || /404/.test(m) || /平台签到未启用/.test(m);
}

async function acctCheckin(id, btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const d = await api('accounts/' + encodeURIComponent(id) + '/platform_checkin', { method: 'POST' });
    const r = d.result || {};
    const st = r.status || '';
    if (st === 'ok') toast('签到成功' + (r.reward ? '：' + r.reward : ''), 'ok');
    else if (st === 'already') toast('今日已签过', 'ok');
    else if (st === 'failed') toast('签到失败：' + (r.message || '未知原因'), 'bad');
    else toast('签到完成', 'ok');
    await loadAcctAccounts();
  } catch (e) {
    if (acctCheckinDegrade(e)) toast('签到服务尚未启用（后端签到子系统未部署）', 'mute');
    else toast('签到失败：' + e.message, 'bad');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

async function acctCheckinAll(btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const d = await api('accounts/platform_checkin_all', { method: 'POST' });
    toast(d.message || '全平台签到已执行；逐账号结果见签到状态列', 'ok');
    await loadAcctAccounts();
  } catch (e) {
    if (acctCheckinDegrade(e)) toast('签到服务尚未启用（后端签到子系统未部署）', 'mute');
    else toast('批量签到失败：' + e.message, 'bad');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* ── 添加账号向导 ─────────────────────────────────────────────────── */

function openAcctAdd(platform) {
  const veil = $('accAddVeil');
  if (!veil) return;
  $('accAddPlatform').value = ACCT_PLATFORMS[platform] ? platform : 'loomy';
  $('accAddLabel').value = '';
  $('accAddNote').value = '';
  $('accAddCred').value = '';
  acctAddErr('', false);
  acctAddSchemaUI();
  veil.classList.add('on');
  setTimeout(() => { try { $('accAddCred').focus(); } catch (e) {} }, 60);
}

function closeAcctAdd() {
  const veil = $('accAddVeil');
  if (veil) veil.classList.remove('on');
}

function acctAddErr(msg, show) {
  const el = $('accAddErr');
  if (!el) return;
  el.textContent = msg || '';
  el.hidden = !show;
}

function acctAddSchemaUI() {
  const p = $('accAddPlatform').value;
  const meta = ACCT_PLATFORMS[p];
  if (!meta) return;
  $('accAddSchema').textContent = meta.schema;
  $('accAddCred').placeholder = meta.sample;
}

/* acctValidateBlob 与 account.ValidateCred 同口径的**预检**（服务端权威）。
   返回错误文案（带字段名）或空串。 */
function acctValidateBlob(platform, obj) {
  const meta = ACCT_PLATFORMS[platform];
  if (!meta) return '未知平台 ' + platform;
  for (const f of meta.required) {
    const v = obj[f];
    if (typeof v !== 'string' || !v.trim()) return '凭证字段 ' + f + ': 不能为空';
  }
  if (platform === 'loomy' && String(obj.session).trim().length < 16) {
    return '凭证字段 session: 过短（长度 ≥16）';
  }
  if (platform === 'custom') {
    const base = String(obj.base_url || '').trim();
    if (!/^https?:\/\//.test(base)) return '凭证字段 base_url: 必须是 http(s) URL';
    if (obj.protocol && !['openai', 'anthropic', 'gemini'].includes(String(obj.protocol))) {
      return '凭证字段 protocol: 只允许 openai / anthropic / gemini';
    }
  }
  return '';
}

async function acctAddSave() {
  const platform = $('accAddPlatform').value;
  const meta = ACCT_PLATFORMS[platform];
  let blob;
  try {
    blob = JSON.parse($('accAddCred').value);
  } catch (e) {
    acctAddErr('凭证不是合法 JSON：' + e.message, true);
    return;
  }
  if (!blob || typeof blob !== 'object' || Array.isArray(blob)) {
    acctAddErr('凭证必须是 JSON 对象（{"field": "value"} 形态）', true);
    return;
  }
  const pre = acctValidateBlob(platform, blob);
  if (pre) {
    acctAddErr(pre, true);
    return;
  }
  acctAddErr('', false);
  const body = { platform: platform, cred: blob };
  const label = $('accAddLabel').value.trim();
  const note = $('accAddNote').value.trim();
  if (label) body.label = label;
  if (note) body.note = note;
  const btn = $('accAddSave');
  btn.disabled = true;
  if ($('accAddOut')) $('accAddOut').textContent = '保存中…';
  try {
    const d = await api('accounts', { method: 'POST', body: JSON.stringify(body) });
    toast('已添加 ' + (meta ? meta.name : platform) + ' 账号' + (d.label ? '「' + d.label + '」' : '') + '，托管渠道已投影', 'ok');
    closeAcctAdd();
    acctLoaded = false;
    selectAcctTab(platform);
    await loadAcctAccounts();
  } catch (e) {
    // 服务端校验失败：400 且报错带字段名（account.ValidateCred），原样透出。
    acctAddErr('保存失败：' + e.message, true);
  } finally {
    btn.disabled = false;
    if ($('accAddOut')) $('accAddOut').textContent = '';
  }
}

/* ── 渠道设置抽屉 ─────────────────────────────────────────────────── */

function openAcctDrawer(open) {
  acctDrawerOpen = !!open;
  applyAcctTab();
  if (acctDrawerOpen && typeof loadExt === 'function') loadExt();
}

/* 页签切换（app.js 的锚点重定向也调用）。 */
function selectAcctTab(key) {
  acctTab = (key === 'codebuddy' || ACCT_PLATFORMS[key]) ? key : 'codebuddy';
  applyAcctTab();
  if (acctTab !== 'codebuddy') loadAcctAccounts();
}

/* ── 一致性检查（只读） ───────────────────────────────────────────── */

async function acctConsistency() {
  try {
    const d = await api('accounts/consistency');
    const platforms = d.platforms || [];
    const bad = platforms.filter(p => !p.consistent);
    if (!platforms.length) { toast('暂无平台账号，无需一致性检查', 'mute'); return; }
    if (!bad.length) {
      toast('一致性检查通过：' + platforms.length + ' 个平台的账号与托管渠道全部对齐', 'ok');
      return;
    }
    toast('发现 ' + bad.length + ' 个平台不一致：'
      + bad.map(p => p.platform + (p.missing && p.missing.length ? '（缺 ' + p.missing.length + ' 凭证）' : '')
        + (p.orphans && p.orphans.length ? '（孤儿 ' + p.orphans.length + '）' : '')).join('、')
      + '；保存配置或重启后由 Reconcile 修复', 'bad');
  } catch (e) {
    toast('一致性检查失败：' + e.message, 'bad');
  }
}

/* ── 事件绑定（顶层守卫：元素不存在时静默跳过） ───────────────────── */

if ($('acctTabs')) {
  $('acctTabs').onclick = e => {
    const b = e.target.closest('button[data-acct]');
    if (!b) return;
    selectAcctTab(b.dataset.acct);
  };
}
if ($('btnAcctAdd')) {
  $('btnAcctAdd').onclick = () => openAcctAdd(acctTab !== 'codebuddy' ? acctTab : 'loomy');
}
if ($('btnCheckinAll')) $('btnCheckinAll').onclick = () => acctCheckinAll($('btnCheckinAll'));
if ($('btnExtDrawer')) $('btnExtDrawer').onclick = () => openAcctDrawer(!acctDrawerOpen);
if ($('btnExtDrawerClose')) $('btnExtDrawerClose').onclick = () => openAcctDrawer(false);
if ($('btnAcctConsistency')) $('btnAcctConsistency').onclick = acctConsistency;
if ($('accAddPlatform')) $('accAddPlatform').onchange = acctAddSchemaUI;
if ($('accAddSave')) $('accAddSave').onclick = acctAddSave;
if ($('accAddCancel')) $('accAddCancel').onclick = closeAcctAdd;
if ($('accAddVeil')) {
  $('accAddVeil').onclick = e => { if (e.target === $('accAddVeil')) closeAcctAdd(); };
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && $('accAddVeil').classList.contains('on')) closeAcctAdd();
  });
}
