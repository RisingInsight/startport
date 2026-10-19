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

/* v1.7.2：全局「添加账号」在账号中心 = 平台添加向导（所有页签可见，不再隐藏）；
   CodeBuddy 登录弹窗只由页签内按钮（btnAcctAdd，文案随页签切换）打开——两条
   入口各有明确归属，消除"右上角按钮 = 唯一添加入口"的歧义。 */
function syncAcctTopbar() {
  if ($('btnAcctAdd')) $('btnAcctAdd').textContent = acctTab === 'codebuddy' ? '添加 CodeBuddy 账号' : '添加账号';
}

/* 全局「添加账号」按钮（顶栏）在账号中心视图的入口（app.js 分流到这里）：
   默认定位到当前页签的平台；CodeBuddy 页签没有向导平台，回落首个平台 loomy。 */
function acctGlobalAddAccount() {
  const p = (acctTab !== 'codebuddy' && ACCT_PLATFORMS[acctTab]) ? acctTab : 'loomy';
  openAcctAdd(p);
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
  // v1.7.0：首屏默认「浏览器登录」（支持平台）；不支持的平台上「粘贴」页签并提示。
  const plat = $('accAddPlatform').value;
  const capsBefore = acctBrowserSupported(plat);
  acctSetMode(capsBefore ? 'browser' : 'paste');
  acctSyncPlatformOptions();
  // 服务端能力开关异步到位后：仅当支持面判定翻转时才重跑模式（不打断进行中会话），
  // 并同步下拉的能力标记。
  acctLoadCapabilities().then(caps => {
    if (!caps) return;
    acctSyncPlatformOptions();
    if (acctBrowserSupported(plat) !== capsBefore
      && $('accAddVeil') && $('accAddVeil').classList.contains('on')) {
      acctSetMode(acctAddMode);
    }
  });
  veil.classList.add('on');
  setTimeout(() => {
    try {
      (acctAddMode === 'browser' ? $('abStart') : $('accAddCred')).focus();
    } catch (e) {}
  }, 60);
}

function closeAcctAdd() {
  stopAccLogin(true); // 进行中的登录会话：停轮询并通知后端取消（best-effort）
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
  $('btnAcctAdd').onclick = () => {
    if (acctTab === 'codebuddy') {
      openAdd(); // CodeBuddy 登录弹窗的唯一直达入口（v1.7.2 入口唯一化）
      return;
    }
    openAcctAdd(acctTab); // 平台页签：预选当前平台直接进向导
  };
}
if ($('btnCheckinAll')) $('btnCheckinAll').onclick = () => acctCheckinAll($('btnCheckinAll'));
if ($('btnExtDrawer')) $('btnExtDrawer').onclick = () => openAcctDrawer(!acctDrawerOpen);
if ($('btnExtDrawerClose')) $('btnExtDrawerClose').onclick = () => openAcctDrawer(false);
if ($('btnAcctConsistency')) $('btnAcctConsistency').onclick = acctConsistency;
if ($('accAddPlatform')) $('accAddPlatform').onchange = () => {
  acctAddSchemaUI();
  // 平台切换后重跑当前模式判定：切到不支持浏览器登录的平台自动落回「粘贴」
  // 并提示；切回来则解除置灰。会话随 UI 重置一并取消（防跨平台串会话）。
  acctSetMode(acctAddMode);
};
if ($('accAddSave')) $('accAddSave').onclick = acctAddSave;
if ($('accAddCancel')) $('accAddCancel').onclick = closeAcctAdd;
if ($('accAddVeil')) {
  $('accAddVeil').onclick = e => { if (e.target === $('accAddVeil')) closeAcctAdd(); };
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && $('accAddVeil').classList.contains('on')) closeAcctAdd();
  });
}


/* ══ 浏览器登录向导（v1.7.0）══════════════════════════════════════
   契约（后端已冻结，internal/panel/acctlogin.go；session_id 走 body 不走路径段）：
     POST /api/accounts/login/start          {platform, phone?}
       → {session_id, mode:"auth_url"|"sms"|…, login_url?, expires_at?}
     POST /api/accounts/login/wait           {session_id}
       → {status:"pending"|"success"|"failed", account_id?, error?}（前端 2s 轮询）
     POST /api/accounts/login/sms/send       {session_id}  → 200=已重发；400=频控/错误
     POST /api/accounts/login/sms/verify     {session_id, code}  → 200（凭证入池）/ 4xx
     POST /api/accounts/login/cancel         {session_id}  （取消后 wait 404）
   降级：501（acclogin 未启用）/ 404（旧二进制无路由）→ 提示改用粘贴凭证。
   平台支持面（spec §4 首批）：qoder / qoder-intl（授权链接+轮询）、
   autoclaw（手机验证码两步）；autoclaw-intl / loomy / custom 仅粘贴。 */

const ACCT_BROWSER_PLATFORMS = {
  qoder: 'Qoder 国内',
  'qoder-intl': 'Qoder 国际',
  autoclaw: 'AutoClaw 国内'
};
const ACCT_LOGIN_POLL_MS = 2000;   // wait 轮询间隔（规格 §3.1）
const ACCT_SMS_RESEND_SEC = 60;    // 发码倒计时（规格 R3 频控对策）

/* 灰 Tab 的按平台原因文案（v1.7.3 用户实测反馈：落在灰 Tab 上不知道为什么选不了）。
   未收录平台走兜底文案。 */
const ACCT_BROWSER_BLOCKED_REASON = {
  loomy: 'Loomy 无登录流程——请从客户端登录态复制 session 后用粘贴凭证',
  custom: '通用平台无统一登录——粘贴 API 凭证',
  'autoclaw-intl': '国际版需风控验证码，暂不支持浏览器登录'
};
const ACCT_BROWSER_BLOCKED_FALLBACK =
  '该平台暂不支持浏览器登录（当前支持：Qoder 国内/国际、AutoClaw 国内）';

/* 平台下拉选项的基础文案（acctSyncPlatformOptions 据此追加能力标记）。 */
const ACCT_PLATFORM_OPTION_LABELS = {
  loomy: 'Loomy（讯飞 · 双头 session）',
  autoclaw: 'AutoClaw 国内（智谱）',
  'autoclaw-intl': 'AutoClaw 国际（智谱）',
  qoder: 'Qoder 国内（阿里）',
  'qoder-intl': 'Qoder 国际（阿里）',
  custom: '通用平台（OpenAI / Anthropic / Gemini 兼容）'
};

/* acctSyncPlatformOptions 平台下拉追加能力标记（v1.7.3）：
   支持 → " · ✓ 浏览器登录"；不支持 → " · 粘贴凭证"。服务端 capabilities
   到位后重跑一次，标记随之更新（幂等：基名取自常量表，不叠加重后缀）。 */
function acctSyncPlatformOptions() {
  const sel = $('accAddPlatform');
  if (!sel) return;
  Array.from(sel.options).forEach(o => {
    const base = ACCT_PLATFORM_OPTION_LABELS[o.value];
    if (!base) return;
    o.textContent = acctBrowserSupported(o.value)
      ? base + ' · ✓ 浏览器登录'
      : base + ' · 粘贴凭证';
  });
}

let accAddMode = 'paste';
let accLogin = null; // {session, platform, timer, smsTimer, smsLeft, startedAt, lastStatusText}

/* 平台支持判定：服务端 capabilities（GET /api/accounts/login/capabilities，
   规格 §3.4「前端不硬编码」）优先；未拉到/端点缺失时回退内置常量。 */
let acctLoginCaps = null; // null = 未拉到（含 501/404：acclogin 未启用）

async function acctLoadCapabilities() {
  if (acctLoginCaps) return acctLoginCaps;
  try {
    const d = await api('accounts/login/capabilities');
    const caps = {};
    (d.platforms || []).forEach(p => { caps[p] = true; });
    acctLoginCaps = caps;
    return caps;
  } catch (e) {
    return null; // 拉取失败：保留内置回退，下次打开向导再试
  }
}

function acctBrowserSupported(p) {
  if (acctLoginCaps) return !!acctLoginCaps[p];
  return !!ACCT_BROWSER_PLATFORMS[p]; // 回退：与服务端 SupportedPlatforms 同清单
}

/* 模式切换（v1.7.3）：不支持浏览器登录的平台自动落「粘贴」Tab，且**始终**在
   页签下方显示按平台的原因文案 + 置灰 Tab 的 title 双保险——用户不再面对一个
   没有解释的灰 Tab（用户实测反馈 #2）。 */
function acctSetMode(mode) {
  const platform = $('accAddPlatform') ? $('accAddPlatform').value : 'loomy';
  const supported = acctBrowserSupported(platform);
  const blockedMsg = ACCT_BROWSER_BLOCKED_REASON[platform] || ACCT_BROWSER_BLOCKED_FALLBACK;
  const hint = $('accAddModeHint');
  if (mode === 'browser' && !supported) {
    mode = 'paste'; // 智能默认：不支持的平台不落在灰 Tab 上
  }
  accAddMode = mode;
  if ($('accAddMode')) {
    $('accAddMode').querySelectorAll('button[data-mode]').forEach(b => {
      b.classList.toggle('on', b.dataset.mode === mode);
      if (b.dataset.mode === 'browser') {
        b.disabled = !supported;
        b.title = supported ? '' : blockedMsg; // 双保险：置灰 + 悬停原因
      } else {
        b.disabled = false;
        b.title = '';
      }
    });
  }
  if (hint) {
    hint.hidden = supported;
    hint.textContent = supported ? '' : blockedMsg;
  }
  if ($('accAddBrowserPanel')) $('accAddBrowserPanel').hidden = mode !== 'browser';
  if ($('accAddPastePanel')) $('accAddPastePanel').hidden = mode !== 'paste';
  if ($('accAddSave')) $('accAddSave').hidden = mode === 'browser'; // 保存按钮只属粘贴流
  if (mode === 'browser') acctBrowserUIReset();
}

/* 浏览器面板的静态形态（按平台：qoder=授权链接流；autoclaw=短信流）。 */
function acctBrowserUIReset() {
  const platform = $('accAddPlatform').value;
  const isSms = platform === 'autoclaw';
  if ($('abHint')) {
    $('abHint').textContent = isSms
      ? '输入 AutoClaw 国内版绑定的手机号获取短信验证码；登录成功后账号自动入池并投影为托管渠道。'
      : '点击「获取授权链接」，在浏览器完成 ' + (ACCT_BROWSER_PLATFORMS[platform] || platform)
        + ' 登录；本窗口会自动检测登录结果（最长约 5 分钟）。';
  }
  if ($('abSmsRow')) $('abSmsRow').hidden = !isSms;
  if ($('abUrlRow')) $('abUrlRow').hidden = true;
  if ($('abStatus')) { $('abStatus').hidden = true; $('abStatus').textContent = ''; $('abStatus').style.color = ''; }
  if ($('abStart')) { $('abStart').hidden = false; $('abStart').textContent = isSms ? '发送验证码并开始登录' : '获取授权链接'; $('abStart').disabled = false; }
  if ($('abRestart')) $('abRestart').hidden = true;
  if ($('abCancelLogin')) $('abCancelLogin').hidden = true;
  if ($('abCode')) $('abCode').value = '';
  if ($('abPhone')) $('abPhone').disabled = false;
  stopAccLogin(true); // 换形态/换平台：旧轮询停止 + 通知后端取消旧会话（不留悬挂会话）
}

/* 停止轮询/倒计时；（cancel=true 时）通知后端取消会话——cancel 后 wait 立即 404，
   不能等超时（规格 §3.2）。全部 best-effort：失败不影响 UI。 */
function stopAccLogin(cancel) {
  if (accLogin) {
    if (accLogin.timer) { clearInterval(accLogin.timer); accLogin.timer = null; }
    if (accLogin.smsTimer) { clearInterval(accLogin.smsTimer); accLogin.smsTimer = null; }
    if (cancel && accLogin.session) {
      api('accounts/login/cancel', {
        method: 'POST',
        body: JSON.stringify({ session_id: accLogin.session })
      }).catch(() => {});
    }
  }
  accLogin = null;
}

/* 掩码手机号（规格 §3.3-3：前 3 后 4；观测/提示用）。 */
function acctMaskPhone(p) {
  p = String(p || '');
  return p.length === 11 ? p.slice(0, 3) + '****' + p.slice(7) : p.slice(0, 3) + '****';
}

/* abStatus 的统一写入口（颜色语义：'' 正常等待 / var(--bad) 失败 / var(--ink-3) 降级）。 */
function acctLoginStatus(msg, color) {
  const el = $('abStatus');
  if (!el) return;
  el.hidden = !msg;
  el.textContent = msg || '';
  el.style.color = color || '';
}

/* 登录服务不可用（501 = acclogin 未启用；404 = 旧二进制无路由）。 */
function acctLoginDegrade(m) {
  return m.indexOf('404') >= 0 || m.indexOf('501') >= 0 || m.indexOf('未启用') >= 0;
}

/* start：qoder（授权链接）与 autoclaw（手机发码）共用入口。
   start 可重入：旧会话由后端自动 cancel（规格 §3.2），前端只停自己的轮询。 */
async function acctLoginStart() {
  const platform = $('accAddPlatform').value;
  if (!acctBrowserSupported(platform)) return;
  const isSms = platform === 'autoclaw';
  const phone = ($('abPhone') ? $('abPhone').value : '').trim();
  if (isSms && !/^1\d{10}$/.test(phone)) {
    acctLoginStatus('请输入 11 位手机号', 'var(--bad)');
    return;
  }
  stopAccLogin(false); // 停旧轮询/倒计时；旧会话由 start 重入语义在后端 cancel
  acctLoginStatus(isSms ? '正在发送验证码…' : '正在获取授权链接…');
  const btn = $('abStart');
  if (btn) { btn.disabled = true; }
  try {
    const body = { platform: platform };
    if (isSms) body.phone = phone;
    const d = await api('accounts/login/start', { method: 'POST', body: JSON.stringify(body) });
    accLogin = {
      session: d.session_id || '',
      platform: platform,
      timer: null, smsTimer: null, smsLeft: 0,
      startedAt: Date.now(), lastStatusText: ''
    };
    if ($('abStart')) $('abStart').hidden = true;
    if ($('abRestart')) $('abRestart').hidden = false;
    if ($('abCancelLogin')) $('abCancelLogin').hidden = false;
    if ($('abPhone')) $('abPhone').disabled = true;
    if (isSms) {
      acctLoginStatus('验证码已发送至 ' + acctMaskPhone(phone) + '，请输入短信验证码');
      acctSmsCountdown();
      acctLoginPoll(); // 会话已创建：立即探一次，再进入 2s 轮询
      acctLoginStartTimer();
    } else if (d.login_url) {
      if ($('abUrl')) $('abUrl').textContent = d.login_url;
      if ($('abOpenBtn')) $('abOpenBtn').href = d.login_url;
      if ($('abUrlRow')) $('abUrlRow').hidden = false;
      acctLoginStatus('等待授权完成，自动检测中…');
      acctLoginStartTimer();
    } else {
      // 未带 login_url 的形态（如 device_code）：仍进入轮询，提示见状态区。
      acctLoginStatus('会话已创建，等待授权完成，自动检测中…');
      acctLoginStartTimer();
    }
  } catch (e) {
    const m = e.message || '';
    if (acctLoginDegrade(m)) {
      acctLoginStatus('浏览器登录服务未启用（后端 acclogin 未部署）；可改用「粘贴凭证」导入', 'var(--ink-3)');
    } else {
      acctLoginStatus('启动失败：' + m, 'var(--bad)');
    }
    if ($('abStart')) { $('abStart').hidden = false; $('abStart').disabled = false; }
  } finally {
    if (btn && accLogin && accLogin.session) { /* start 成功后按钮保持隐藏 */ }
    else if (btn) { btn.disabled = false; }
  }
}

/* wait 轮询（2s；pending 续轮；success/failed 终态；404 = 会话被取消或过期）。 */
function acctLoginStartTimer() {
  if (!accLogin) return;
  accLogin.timer = setInterval(() => acctLoginPoll(), ACCT_LOGIN_POLL_MS);
  acctLoginPoll();
}

async function acctLoginPoll() {
  if (!accLogin || !accLogin.session || accLogin.timer === undefined) return;
  let d = null;
  try {
    d = await api('accounts/login/wait', {
      method: 'POST',
      body: JSON.stringify({ session_id: accLogin.session })
    });
  } catch (e) {
    const m = e.message || '';
    if (acctLoginDegrade(m)) {
      acctLoginStopWithError('浏览器登录服务未启用（后端 acclogin 未部署）；可改用「粘贴凭证」导入', true);
      return;
    }
    if (m === 'HTTP 404' || m.indexOf('404') >= 0) {
      acctLoginStopWithError('登录会话已过期或已取消；请点击「重新获取」', false);
      return;
    }
    // 网络抖动：保持轮询（状态区不打扰），下个周期重试。
    return;
  }
  const st = d.status || 'pending';
  if (st === 'success') {
    const label = d.account_id ? ('（账号 ' + d.account_id + '）') : '';
    stopAccLogin(false);
    toast('登录成功，账号已入池并投影为托管渠道' + label, 'ok');
    closeAcctAdd();
    selectAcctTab(accLogin ? accLogin.platform : $('accAddPlatform').value);
    loadAcctAccounts();
    return;
  }
  if (st === 'failed') {
    acctLoginStopWithError('登录失败：' + (d.error || '原因未明') + '；可点击「重新获取」重试', false);
    return;
  }
  if (st === 'expired') {
    // 过期会话在后端保留 1 分钟供观察，wait 显式回 expired（区别于取消后的 404）：
    // UI 同「过期/取消」分支——展示原因 + 重新获取。
    acctLoginStopWithError('登录会话已过期，请点击『重新获取』', false);
    return;
  }
  // pending：更新等待时长（状态文本只在秒数变化时重写，避免闪烁）。
  const sec = Math.floor((Date.now() - accLogin.startedAt) / 1000);
  const text = '等待授权完成，自动检测中…（已等待 ' + sec + ' 秒）';
  if (text !== accLogin.lastStatusText) {
    accLogin.lastStatusText = text;
    acctLoginStatus(text);
  }
}

/* 终态统一收尾：停轮询、显示原因、亮出「重新获取」。keepPanel=true 时保留面板形态。 */
function acctLoginStopWithError(msg, degrade) {
  const platform = accLogin ? accLogin.platform : $('accAddPlatform').value;
  const wasSms = platform === 'autoclaw';
  stopAccLogin(false);
  acctLoginStatus(msg, degrade ? 'var(--ink-3)' : 'var(--bad)');
  if ($('abStart')) { $('abStart').hidden = false; $('abStart').disabled = false;
    $('abStart').textContent = wasSms ? '发送验证码并开始登录' : '获取授权链接'; }
  if ($('abRestart')) $('abRestart').hidden = false;
  if ($('abCancelLogin')) $('abCancelLogin').hidden = true;
  if ($('abPhone')) $('abPhone').disabled = false;
}

/* autoclaw：60s 发码倒计时（规格 R3：前端限频 + 后端 (device_id, phone) 60s 一次）。 */
function acctSmsCountdown() {
  if (!accLogin) return;
  accLogin.smsLeft = ACCT_SMS_RESEND_SEC;
  const btn = $('abSendCode');
  const tick = () => {
    if (!accLogin) { if (btn) { btn.disabled = false; btn.textContent = '发送验证码'; } return; }
    if (accLogin.smsLeft <= 0) {
      clearInterval(accLogin.smsTimer);
      accLogin.smsTimer = null;
      if (btn) { btn.disabled = false; btn.textContent = '重新发送'; }
      return;
    }
    if (btn) { btn.disabled = true; btn.textContent = '重新发送(' + accLogin.smsLeft + 's)'; }
    accLogin.smsLeft--;
  };
  tick();
  accLogin.smsTimer = setInterval(tick, 1000);
}

/* autoclaw：重发验证码（后端 sms/send）。200 = 已重发（重启 60s 倒计时）；
   400 = 频控/错误原文展示，**不**重启倒计时（规格 R3：频控窗口内不放行）。 */
async function acctSmsResend() {
  if (!accLogin || !accLogin.session) {
    // 会话未建立：走 start 的首发路径（带手机号建会话）。
    await acctLoginStart();
    return;
  }
  const btn = $('abSendCode');
  if (btn) btn.disabled = true;
  try {
    await api('accounts/login/sms/send', {
      method: 'POST',
      body: JSON.stringify({ session_id: accLogin.session })
    });
    acctLoginStatus('验证码已重新发送，请查收短信');
    acctSmsCountdown(); // 200：重启 60s 倒计时
  } catch (e) {
    const m = e.message || '';
    if (acctLoginDegrade(m)) {
      acctLoginStopWithError('浏览器登录服务未启用（后端 acclogin 未部署）', true);
    } else {
      acctLoginStatus('重新发送失败：' + m, 'var(--bad)'); // 400：不重启倒计时
      if (btn) btn.disabled = false;
    }
  }
}

/* 发码按钮统一入口：会话未建 → start（首发）；已建 → sms/send（重发）。 */
async function acctSmsSendOrResend() {
  if (accLogin && accLogin.session) {
    await acctSmsResend();
    return;
  }
  await acctLoginStart();
}

/* autoclaw：验证码提交（sms_verify 200 = 凭证已入池；4xx = 原因透出，会话保留可重输）。 */
async function acctSmsVerify() {
  if (!accLogin || !accLogin.session) {
    acctLoginStatus('请先发送验证码', 'var(--bad)');
    return;
  }
  const code = ($('abCode') ? $('abCode').value : '').trim();
  if (!/^\d{6}$/.test(code)) {
    acctLoginStatus('请输入 6 位数字验证码', 'var(--bad)');
    return;
  }
  const btn = $('abSubmit');
  if (btn) btn.disabled = true;
  try {
    await api('accounts/login/sms/verify', {
      method: 'POST',
      body: JSON.stringify({ session_id: accLogin.session, code: parseInt(code, 10) })
    });
    acctLoginStatus('验证码通过，正在确认登录结果…');
    acctLoginStartTimer(); // 立即探一次 wait，再进入 2s 轮询直到 success
  } catch (e) {
    const m = e.message || '';
    if (acctLoginDegrade(m)) {
      acctLoginStopWithError('浏览器登录服务未启用（后端 acclogin 未部署）', true);
    } else {
      // 验证码错误/过期：会话保留，可直接改号重输（不清空手机号）。
      acctLoginStatus('验证码校验失败：' + m + '；请重输或重新发送', 'var(--bad)');
    }
  } finally {
    if (btn) btn.disabled = false;
  }
}

/* 复制授权链接（clipboard 优先，execCommand 兜底——CSP 下 execCommand 仍可用）。 */
async function acctCopyLoginUrl() {
  const url = $('abUrl') ? $('abUrl').textContent : '';
  if (!url) return;
  try {
    await navigator.clipboard.writeText(url);
    toast('链接已复制', 'ok');
  } catch (e) {
    try {
      const ta = document.createElement('textarea');
      ta.value = url;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
      toast('链接已复制', 'ok');
    } catch (e2) {
      toast('复制失败，请手动选择链接复制', 'bad');
    }
  }
}

/* 用户主动取消登录（向导内按钮）。 */
function acctLoginCancel() {
  stopAccLogin(true);
  acctBrowserUIReset();
  toast('登录会话已取消', 'mute');
}

/* 离开账号中心时的进行中会话提示（app.js 的 go() 钩子；不阻塞导航）。 */
function acctLeaveCenterNotify() {
  if (accLogin && accLogin.session) {
    toast('提示：有进行中的浏览器登录会话，切走后仍会继续；可回到「账号中心」查看，或在向导内取消', 'mute');
  }
}

/* ── 浏览器登录事件绑定（顶层守卫） ──────────────────────────────── */

if ($('accAddMode')) {
  $('accAddMode').onclick = e => {
    const b = e.target.closest('button[data-mode]');
    if (!b || b.disabled) return;
    acctSetMode(b.dataset.mode);
  };
}
if ($('abStart')) $('abStart').onclick = acctLoginStart;
if ($('abRestart')) $('abRestart').onclick = acctLoginStart; // start 可重入（旧会话自动 cancel）
if ($('abCancelLogin')) $('abCancelLogin').onclick = acctLoginCancel;
if ($('abSendCode')) $('abSendCode').onclick = acctSmsSendOrResend; // 首发=start；重发=sms/send（60s 限频）
if ($('abSubmit')) $('abSubmit').onclick = acctSmsVerify;
if ($('abCopyBtn')) $('abCopyBtn').onclick = acctCopyLoginUrl;
