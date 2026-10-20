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
    schema: '必填 token；可选字段同国内版。国际版有独立的每日签到活动。',
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
    schema: '必填同 Qoder 国内。国际版无每日签到；每日 Credits 领取需 Qoder 客户端风控身份（设备登录账号不支持）。',
    required: ['access_token', 'user_id']
  },
  trae: {
    name: 'TRAE（字节 · SOLO 国内）',
    sample: '{\n  "accessToken": "<Cloud-IDE-JWT>",\n  "uid": "<用户 ID>",\n  "machineId": "<设备指纹>",\n  "deviceId": "<设备指纹>",\n  "refreshToken": "可选：下一批刷新链用",\n  "variant": "solo"\n}',
    schema: '必填 accessToken；uid/machineId/deviceId 建议一并填写（上游按设备绑定）。当前仅支持国内 SOLO 通道；登录/签到/刷新在下一批接入。',
    required: ['accessToken']
  },
  custom: {
    name: '通用平台',
    sample: '{\n  "base_url": "https://api.example.com",\n  "secret": "<API key>",\n  "protocol": "openai",\n  "auth": { "mode": "bearer" }\n}',
    schema: '必填 base_url（http(s)，过 SSRF 红线）与 secret；可选 protocol（openai/anthropic/gemini，缺省 openai）、auth{}、headers{}、allow_private。每个账号投影为一条独立 c-<id> 托管渠道。',
    required: ['base_url', 'secret']
  }
};

/* 账号池视图：一个平台一个「XXX 账号池」入口，国内/国际是池内区域而不是两个
   独立下拉项。Qoder 与 AutoClaw 共用同一套池分析 UI。 */
const ACCT_POOLS = {
  'loomy-pool': {
    name: 'Loomy 账号池',
    short: 'Loomy',
    mark: 'L',
    logFilter: 'ext:loomy',
    regions: [
      { key: 'all', name: '全部', platform: 'loomy', prefix: 'ext:loomy/' }
    ]
  },
  'autoclaw-pool': {
    name: 'AutoClaw 账号池',
    short: 'AutoClaw',
    mark: 'A',
    multi: true,
    logFilter: 'ext:autoclaw',
    regions: [
      { key: 'all', name: '全部' },
      { key: 'cn', name: '国内', platform: 'autoclaw', prefix: 'ext:autoclaw/' },
      { key: 'intl', name: '国际', platform: 'autoclaw-intl', prefix: 'ext:autoclaw-intl/' }
    ]
  },
  'qoder-pool': {
    name: 'Qoder 账号池',
    short: 'Qoder',
    mark: 'Q',
    multi: true,
    logFilter: 'ext:qoder',
    regions: [
      { key: 'all', name: '全部' },
      { key: 'cn', name: '国内', platform: 'qoder', prefix: 'ext:qoder/' },
      { key: 'intl', name: '国际', platform: 'qoder-intl', prefix: 'ext:qoder-intl/' }
    ]
  },
  'trae-pool': {
    name: 'TRAE 账号池',
    short: 'TRAE',
    mark: 'T',
    logFilter: 'ext:trae',
    regions: [
      { key: 'all', name: '全部', platform: 'trae', prefix: 'ext:trae/' }
    ]
  },
  'custom-pool': {
    name: '通用平台账号池',
    short: '通用平台',
    mark: 'C',
    customModel: true,
    logFilter: 'ext:c-',
    regions: [
      { key: 'all', name: '全部', platform: 'custom' }
    ]
  }
};
const ACCT_PLATFORM_POOL = {};
Object.keys(ACCT_POOLS).forEach(k => ACCT_POOLS[k].regions.forEach(r => {
  if (r.platform) ACCT_PLATFORM_POOL[r.platform] = { pool: k, region: r.key };
}));

/* 账号五态（架构 §4.6）→ [文案, 徽标 class]。 */
const ACCT_STATES = {
  healthy: ['可用', 'ok'],
  cooling: ['冷却', 'warn'],
  invalid: ['失效', 'bad'],
  disabled: ['停用', 'mute'],
  paused: ['暂停', 'mute'],
  unprojected: ['未投影', 'warn']
};

/* 签到状态 → 文案。Qoder 中国版的周期是 10:00–次日 09:59（上游 campaign
   startAt/endAt 实测），其他平台按自然日；昨天的「已领」不能冒充今天已签。 */
function acctCheckinText(a) {
  const ck = a.checkin || {};
  if (!ck.supported) {
    return a.platform === 'qoder-intl'
      ? '<span class="tag mute">平台无此活动</span>'
        + '<div class="note">每日 Credits 需 Qoder 客户端风控身份；设备登录账号不可领</div>'
      : '<span class="tag mute">平台无此活动</span>';
  }
  const current = acctCheckinCurrent(a);
  const qoder = a.platform === 'qoder';
  const miss = qoder ? '本周期未签' : '今日未签';
  const signed = qoder ? '本周期已签' : '今日已签';
  const already = qoder ? '本周期已签' : '已签过';
  let st = '';
  if (ck.last_status === 'ok') {
    st = current ? '<span class="tag ok">' + signed + '</span>' : '<span class="tag mute">' + miss + '</span>';
  } else if (ck.last_status === 'already') {
    if (current) {
      st = '<span class="tag ok">' + already + '</span>';
      // Qoder 的 already 文案说“今日已领取”，在同周期重复点击时会误导；
      // 周期说明比上游文案更重要，直接由 acctQoderCycleNote 接管。
      if (!qoder && ck.last_message) st += '<div class="note">' + esc(ck.last_message) + '</div>';
    } else {
      st = '<span class="tag mute">' + miss + '</span>';
    }
  } else if (ck.last_status === 'failed') {
    st = current
      ? '<span class="tag bad">失败</span>'
        + (ck.last_error ? '<div class="note" style="color:var(--bad)">' + esc(ck.last_error) + '</div>' : '')
      : '<span class="tag mute">' + miss + '</span>';
  } else st = '<span class="note">未签到</span>';
  if (qoder) st += '<div class="note">' + esc(acctQoderCycleNote(current)) + '</div>';
  if (!qoder && ck.last_at && ck.last_status) {
    // 合计口径只在非失败态展示：失败时"累计 7 次"会把成功与失败混成一个数，
    // 让人看不出到底成功过几次（v1.7.5 用户实测困惑）。
    const total = (ck.last_status === 'failed' && ck.failed) ? '累计失败 ' + ck.failed + ' 次'
      : (ck.total ? '累计 ' + ck.total + ' 次' : '');
    const when = current ? ago(ck.last_at) : '上次 ' + ago(ck.last_at);
    st += '<div class="note">' + esc(when) + (total ? ' · ' + total : '') + '</div>';
  }
  return st;
}

/* 账号行状态（五态）：channel 缺失 = 未投影。 */
function acctStateOf(a) {
  if (a.disabled) return 'disabled';
  const ch = a.channel;
  if (!ch || !ch.exists) return 'unprojected';
  if (ch.state === 'disabled') return 'disabled';
  if (ch.state === 'paused') return 'paused';
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

let acctTab = 'all';
let acctPoolRegion = 'all';
let qoderLoadHours = 168;
let qoderVizAt = 0;
let qoderVizLoading = false;
let qoderVizSeq = 0;
let qoderPoolReq7d = null;
let qoderLatestError = null;
let acctAccounts = [];
let acctLoaded = false;
let acctDrawerOpen = false;
let acctLoading = false;
let acctSearch = '';
let acctStatusFilter = '';
let acctDetailID = '';
let acctBalanceTried = new Set();

/* 页签切换 / 抽屉开合的统一应用点（go() 与本模块都走这里，避免两处真值）。 */
function applyAcctTab() {
  const inCenter = view === 'accounts-center';
  if (!inCenter && $('acctDetailVeil')) closeAcctDetail();
  // 顶栏下拉框与当前平台保持同一真值（替代旧分页签）。
  if ($('acctPlatformSelect') && $('acctPlatformSelect').value !== acctTab) {
    $('acctPlatformSelect').value = acctTab;
  }
  // 抽屉优先：展开时盖住平台面板（CodeBuddy 视图也一并让位）。
  const showPanels = inCenter && !acctDrawerOpen;
  if ($('extDrawer')) $('extDrawer').hidden = !(inCenter && acctDrawerOpen);
  const isCodeBuddy = acctTab === 'codebuddy';
  const pool = ACCT_POOLS[acctTab];
  const isPool = !!pool;
  if ($('acctPanels')) $('acctPanels').hidden = !showPanels || isCodeBuddy;
  if ($('acctHeroWrap')) $('acctHeroWrap').hidden = !showPanels || isCodeBuddy || isPool;
  if ($('qoderHero')) $('qoderHero').hidden = !(isPool && showPanels);
  // CodeBuddy 页签 = 原 view-accounts（DOM 原样，仅显隐切换）。
  if ($('view-accounts')) $('view-accounts').hidden = !(inCenter && isCodeBuddy && !acctDrawerOpen);
  // 统一视图：除 CodeBuddy 外的所有选择都显示同一张表；下拉值即平台/池筛选条件。
  if ($('acctPanel-all')) $('acctPanel-all').hidden = !showPanels || isCodeBuddy;
  // 区域切换只在多区域池内显示（Qoder / AutoClaw：全部/国内/国际）；Loomy /
  // TRAE / 通用平台是单区域池，不摆一排永远只有「全部」的无效页签。
  if (pool && !pool.multi) acctPoolRegion = 'all';
  if ($('acctRegionTabs')) {
    $('acctRegionTabs').hidden = !(pool && pool.multi && showPanels);
    if (pool) {
      $('acctRegionTabs').querySelectorAll('button[data-region]').forEach(b => {
        b.classList.toggle('on', b.dataset.region === acctPoolRegion);
      });
    }
  }
  // 池分析区（负载趋势/最近请求/模型锁池）只在池内显示。
  if ($('acctPoolViz')) $('acctPoolViz').hidden = !(isPool && showPanels);
  Object.keys(ACCT_PLATFORMS).forEach(k => {
    const el = $('acctPanel-' + k);
    if (el) el.hidden = true; // 旧分平台面板保留 DOM 作回退，展示由统一视图接管
  });
  if (!isCodeBuddy) renderAcctUnified();
  syncAcctTopbar();
  if (isPool && showPanels) maybeLoadQoderViz();
  // 抽屉按钮文案随开合。
  if ($('btnExtDrawer')) $('btnExtDrawer').textContent = acctDrawerOpen ? '收起渠道设置 ▴' : '渠道设置 ▾';
}

/* 中文短名（通用平台）直接连写，拉丁短名（Qoder 等）两侧留空格。 */
function acctNameJoin(prefix, short, suffix) {
  const pad = /^[\x20-\x7E]+$/.test(short) ? ' ' : '';
  return prefix + pad + short + pad + suffix;
}

/* v1.7.2：全局「添加账号」在账号中心 = 平台添加向导（所有页签可见，不再隐藏）；
   CodeBuddy 登录弹窗只由页签内按钮（btnAcctAdd，文案随页签切换）打开——两条
   入口各有明确归属，消除"右上角按钮 = 唯一添加入口"的歧义。 */
function syncAcctTopbar() {
  if (!$('btnAcctAdd')) return;
  if (acctTab === 'codebuddy') {
    $('btnAcctAdd').textContent = '添加 CodeBuddy 账号';
    return;
  }
  const pool = ACCT_POOLS[acctTab];
  $('btnAcctAdd').textContent = pool ? acctNameJoin('添加', pool.short, '账号') : '添加账号';
}

/* 全局「添加账号」按钮（顶栏）在账号中心视图的入口（app.js 分流到这里）：
   池视图预选当前区域对应的平台；CodeBuddy 页签没有向导平台，回落首个平台。 */
function acctGlobalAddAccount() {
  const pool = ACCT_POOLS[acctTab];
  if (pool) {
    openAcctAdd(acctPoolDefaultPlatform(pool, acctPoolRegion));
    return;
  }
  const p = (acctTab !== 'codebuddy' && ACCT_PLATFORMS[acctTab]) ? acctTab : 'loomy';
  openAcctAdd(p);
}

/* go('accounts-center') 的入口（app.js 调用）。
   平台筛选（CodeBuddy / Qoder…）是账号中心内部状态，但会同步到地址栏：
   #accounts-center = 全部平台账号，#accounts-center/<platform> = 对应平台视图。
   这样从池视图输入 #accounts-center 会触发真实的路由变化，而不是停留在旧状态。 */
function loadAcctCenter() {
  selectAcctTab('all');
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
    acctAutoBalance();
  } catch (e) {
    const msg = e.message || '';
    if ($('acBody-all')) {
      $('acBody-all').innerHTML = '<tr><td colspan="10" class="empty">读取失败：' + esc(msg) + '</td></tr>';
    }
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
  renderAcctUnified();
  if (ACCT_POOLS[acctTab]) maybeLoadQoderViz(true);
}

/* ── 统一账号视图（全部/平台筛选） ─────────────────────────────────── */

function acctIsToday(iso) {
  if (!iso) return false;
  const d = new Date(iso);
  if (isNaN(d.getTime())) return false;
  const n = new Date();
  return d.getFullYear() === n.getFullYear() && d.getMonth() === n.getMonth() && d.getDate() === n.getDate();
}

/* 签到周期起点：Qoder 中国版是 10:00–次日 09:59（上游 campaign startAt/endAt
   实测），其余平台按自然日。 */
function acctCheckinCycleStart(platform, now) {
  const n = now || new Date();
  if (platform === 'qoder') {
    const start = new Date(n.getFullYear(), n.getMonth(), n.getDate(), 10, 0, 0, 0);
    if (n < start) start.setDate(start.getDate() - 1);
    return start;
  }
  return new Date(n.getFullYear(), n.getMonth(), n.getDate(), 0, 0, 0, 0);
}

function acctCheckinCurrent(a, now) {
  const ck = a.checkin || {};
  if (!ck.last_at || !ck.last_status) return false;
  const t = new Date(ck.last_at);
  if (isNaN(t.getTime())) return false;
  return t >= acctCheckinCycleStart(a.platform, now);
}

/* “今天”口径：Qoder 10:00 前的记录属于上一周期，不算今天的签到；
   其余平台按自然日。用于 Hero「今日签到」等自然日统计。 */
function acctCheckinToday(a, now) {
  const ck = a.checkin || {};
  if (!ck.last_at || !ck.last_status) return false;
  const t = new Date(ck.last_at);
  if (isNaN(t.getTime())) return false;
  if (a.platform === 'qoder') {
    const n = now || new Date();
    const ten = new Date(n.getFullYear(), n.getMonth(), n.getDate(), 10, 0, 0, 0);
    return t >= ten;
  }
  return acctIsToday(ck.last_at);
}

function acctQoderCycleNote(current, now) {
  const n = now || new Date();
  const ten = new Date(n.getFullYear(), n.getMonth(), n.getDate(), 10, 0, 0, 0);
  if (n < ten) return '当前周期截至 09:59；今日 10:00 开启新一轮';
  return current ? '当前周期 10:00–明日 09:59；下一轮明日 10:00'
    : '当前周期 10:00–明日 09:59，现在可签';
}

function acctPlatformName(a) {
  const meta = ACCT_PLATFORMS[a.platform];
  return (meta && meta.name) || a.platform_name || a.platform || '—';
}

/* 平台名即入口：CodeBuddy → 账号池；其余 → 该平台的筛选视图（顶部下拉同步）。
   平台名只出现一次，平台 key / 域信息交给备注或状态列，避免「CodeBuddy · codebuddy」堆叠。 */
function acctPlatformCell(a) {
  const name = acctPlatformName(a);
  const isCodeBuddy = a.source === 'codebuddy' || a.platform === 'codebuddy';
  const hint = isCodeBuddy ? '打开 CodeBuddy 账号池' : ('只看 ' + name + ' 账号');
  return '<button type="button" class="acct-plat" data-goto="' + esc(a.platform)
    + '" title="' + esc(hint) + '">' + esc(name) + '</button>';
}

function acctPlatformTD(a) {
  return '<td class="acct-plat-cell" data-goto="' + esc(a.platform) + '">' + acctPlatformCell(a) + '</td>';
}

function acctTabDisplayName(key) {
  if (key === 'all') return '全部平台账号';
  const pool = ACCT_POOLS[key];
  if (pool) return pool.name;
  const meta = ACCT_PLATFORMS[key];
  return (meta && meta.name) || key || '—';
}

function acctPoolPlatforms(pool, region) {
  if (!pool) return [];
  const out = [];
  pool.regions.forEach(r => {
    if (!r.platform) return;
    if (region && region !== 'all' && r.key !== region) return;
    if (!out.includes(r.platform)) out.push(r.platform);
  });
  return out;
}

function acctPoolDefaultPlatform(pool, region) {
  if (!pool) return 'loomy';
  const match = pool.regions.find(r => r.platform && r.key === region);
  if (match) return match.platform;
  const first = pool.regions.find(r => r.platform);
  return first ? first.platform : 'loomy';
}

function acctScopeAccounts() {
  if (acctTab === 'all') return acctAccounts;
  const pool = ACCT_POOLS[acctTab];
  if (pool) {
    const platforms = acctPoolPlatforms(pool, acctPoolRegion);
    return acctAccounts.filter(a => platforms.includes(a.platform));
  }
  return acctAccounts.filter(a => a.platform === acctTab);
}

function acctFilteredAccounts() {
  const q = (acctSearch || '').trim().toLowerCase();
  return acctScopeAccounts().filter(a => {
    if (acctStatusFilter && acctStateOf(a) !== acctStatusFilter) return false;
    if (!q) return true;
    return [a.label, a.id, a.note, a.platform, acctPlatformName(a)]
      .join(' ').toLowerCase().includes(q);
  });
}

function acctHeroMark(platform) {
  if (platform === 'all') {
    return '<svg viewBox="0 0 22 22"><circle cx="11" cy="11" r="4" fill="var(--gold)"/><ellipse cx="11" cy="11" rx="9.5" ry="3.6" fill="none" stroke="var(--accent)" stroke-width="1.2" transform="rotate(-18 11 11)"/><circle cx="19" cy="7.6" r="1.3" fill="var(--gold)"/></svg>';
  }
  const meta = {
    qoder: ['Q', '#4f8cff'], 'qoder-intl': ['Q', '#4f8cff'], 'qoder-pool': ['Q', '#4f8cff'],
    'autoclaw-pool': ['A', '#a78bfa'],
    'loomy-pool': ['L', '#2dd4bf'],
    'trae-pool': ['T', '#e5e7eb'],
    'custom-pool': ['C', '#94a3b8'],
    trae: ['T', '#e5e7eb'],
    autoclaw: ['A', '#a78bfa'], 'autoclaw-intl': ['A', '#a78bfa'],
    loomy: ['L', '#2dd4bf'],
    custom: ['C', '#94a3b8']
  }[platform] || [(platform || '?')[0].toUpperCase(), 'var(--accent)'];
  return '<span class="mono" style="color:' + meta[1] + '">' + esc(meta[0]) + '</span>';
}

function renderAcctHero(scope, counts) {
  const total = scope.length;
  const healthy = counts.healthy || 0;
  const isAll = acctTab === 'all';
  const pool = ACCT_POOLS[acctTab];
  const name = acctTabDisplayName(acctTab);
  if ($('acctHeroMark')) $('acctHeroMark').innerHTML = acctHeroMark(acctTab);
  if ($('acctHeroTitle')) $('acctHeroTitle').textContent = name;
  if ($('acctHeroSub')) $('acctHeroSub').textContent = isAll
    ? 'StarPort · 平台账号控制台'
    : (pool ? pool.short + (pool.multi ? ' · 国内 / 国际' : ' · 统一账号池') : name + ' · 平台账号');
  if ($('acctHeroState')) $('acctHeroState').textContent = total
    ? (healthy + ' 可用 / ' + total + ' 个账号') : '暂无账号';

  const inflight = scope.reduce((n, a) => n + ((a.channel && a.channel.in_flight) || 0), 0);
  const supported = scope.filter(a => (a.capabilities && a.capabilities.checkin)
    || (a.checkin && a.checkin.supported)).length;
  const okToday = scope.filter(a => a.checkin && ['ok', 'already'].includes(a.checkin.last_status)
    && acctCheckinToday(a)).length;
  if ($('acctHeroAvail')) $('acctHeroAvail').textContent = total ? Math.round(healthy / total * 100) + '%' : '—';
  if ($('acctHeroAccounts')) $('acctHeroAccounts').textContent = String(total);
  if ($('acctHeroInflight')) $('acctHeroInflight').textContent = String(inflight);
  if ($('acctHeroCheckin')) $('acctHeroCheckin').textContent = okToday + '/' + supported;
  if ($('acctHeroTotal')) $('acctHeroTotal').textContent = String(total);
  if ($('acctHeroHealthy')) $('acctHeroHealthy').textContent = String(healthy);
  if ($('acctHeroCooling')) $('acctHeroCooling').textContent = String(counts.cooling || 0);
  if ($('acctHeroInvalid')) $('acctHeroInvalid').textContent = String(counts.invalid || 0);
  if ($('acctHeroDisabled')) $('acctHeroDisabled').textContent = String(counts.disabled || 0);
  if ($('acctHeroPaused')) $('acctHeroPaused').textContent = String(counts.paused || 0);
  if ($('acctHeroUnprojected')) $('acctHeroUnprojected').textContent = String(counts.unprojected || 0);
}

/* Qoder 池 Hero：指标对齐 CodeBuddy（可用率 / 积分剩余总额 / 在途 / 近7天 / 最近到期）。
   数据来自账号余额快照与 Qoder 用量；Qoder 无粘性会话，图例用未投影替代。 */
function renderQoderHero(scope, counts) {
  scope = scope || acctScopeAccounts();
  const pool = ACCT_POOLS[acctTab];
  if ($('qoderHeroTitle')) $('qoderHeroTitle').textContent = pool ? pool.name : '账号池';
  if ($('qoderHeroSub')) $('qoderHeroSub').textContent = 'StarPort · ' + (pool ? pool.short : '') + ' 运行概览';
  if ($('qoderHeroMark')) $('qoderHeroMark').textContent = pool ? pool.mark : '?';
  if (!counts) {
    counts = { healthy: 0, cooling: 0, invalid: 0, disabled: 0, paused: 0, unprojected: 0 };
    scope.forEach(a => {
      const st = acctStateOf(a);
      counts[st] = (counts[st] || 0) + 1;
    });
  }
  const total = scope.length;
  const healthy = counts.healthy || 0;
  let remaining = 0, quota = 0;
  scope.forEach(a => {
    const b = a.balance;
    if (!b || typeof b.available !== 'number') return;
    remaining += b.available;
    if (typeof b.total === 'number' && b.total > 0) quota += b.total;
  });
  const inflight = scope.reduce((n, a) => n + ((a.channel && a.channel.in_flight) || 0), 0);
  if ($('qoderHsAvail')) $('qoderHsAvail').textContent = total ? Math.round(healthy / total * 100) + '%' : '—';
  if ($('qoderHsCredits')) {
    $('qoderHsCredits').innerHTML = total
      ? esc(Math.round(remaining * 100) / 100)
        + (quota > 0 ? '<span class="of">/' + esc(Math.round(quota * 100) / 100) + '</span>' : '')
      : '—';
  }
  if ($('qoderHsInflight')) $('qoderHsInflight').textContent = String(inflight);
  if ($('qoderHsReq7d')) $('qoderHsReq7d').textContent = qoderPoolReq7d == null ? '—' : String(qoderPoolReq7d);
  let best = null;
  scope.forEach(a => {
    const raw = a.balance && a.balance.expires_at;
    if (!raw) return;
    const t = new Date(raw);
    if (isNaN(t.getTime()) || t.getFullYear() > 2100) return;
    if (!best || t < best) best = t;
  });
  const val = $('qoderHsExpiryVal'), k = $('qoderHsExpiryK');
  if (val && k) {
    if (best) {
      const days = Math.ceil((best.getTime() - Date.now()) / 86400000);
      if (days < 0) { val.textContent = '已过期'; val.style.color = 'var(--bad)'; }
      else {
        val.textContent = days === 0 ? '今天' : days + ' 天';
        val.style.color = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : '';
      }
      k.textContent = '最近到期 · 剩 ' + fmtTok(remaining) + ' 积分';
    } else {
      val.textContent = '—';
      val.style.color = '';
      k.textContent = '最近到期';
    }
  }
  if ($('qoderHsTotal')) $('qoderHsTotal').textContent = String(total);
  if ($('qoderHsHealthy')) $('qoderHsHealthy').textContent = String(healthy);
  if ($('qoderHsCooling')) $('qoderHsCooling').textContent = String(counts.cooling || 0);
  if ($('qoderHsInvalid')) $('qoderHsInvalid').textContent = String(counts.invalid || 0);
  if ($('qoderHsDisabled')) $('qoderHsDisabled').textContent = String(counts.disabled || 0);
  if ($('qoderHsPaused')) $('qoderHsPaused').textContent = String(counts.paused || 0);
  if ($('qoderHsUnprojected')) $('qoderHsUnprojected').textContent = String(counts.unprojected || 0);
  if ($('qoderHeroPulse')) $('qoderHeroPulse').className = 'pulse' + (healthy > 0 ? '' : (total ? ' warn' : ' bad'));
  if ($('qoderHeroState')) $('qoderHeroState').textContent = healthy > 0 ? '服务正常' : (total ? '无可用账号' : '待添加账号');
  renderPoolExpiry(scope);
}

/* 池积分到期卡：账户级余额快照（Qoder/AutoClaw 都没有逐批到期明细）。 */
function renderPoolExpiry(scope) {
  const host = $('poolExpList');
  if (!host) return;
  const list = (scope || acctScopeAccounts()).slice();
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const rows = list.map(a => {
    const b = a.balance || {};
    const nm = esc(a.label || a.id || '');
    const unit = b.unit ? ' ' + esc(b.unit) : '';
    let ms = 0;
    if (b.expires_at) {
      const raw = String(b.expires_at);
      const t = new Date(raw.length > 10 ? raw : raw + 'T00:00:00');
      if (!isNaN(t.getTime()) && t.getFullYear() < 2100) ms = t.getTime();
    }
    if (!ms) {
      const avail = typeof b.available === 'number' ? esc(b.available) + unit : '—';
      return '<div class="pool-exp-row"><div class="pool-exp-hd"><span class="pool-exp-dot" style="background:var(--ink-3)"></span>'
        + '<span class="pool-exp-nm">' + nm + '</span></div>'
        + '<div class="pool-exp-main">暂无到期信息 · 余额 ' + avail
        + '<div class="pool-exp-note">账户级余额快照；长期有效或上游未提供</div></div></div>';
    }
    const days = Math.ceil((ms - today.getTime()) / 86400000);
    const avail = typeof b.available === 'number' ? b.available : 0;
    const daily = days > 0 ? Math.ceil(avail / days) : avail;
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const when = String(b.expires_at).slice(0, 10);
    const dayWord = days < 0 ? '已过期' : (days === 0 ? '今天到期' : days + ' 天后到期');
    return '<div class="pool-exp-row"><div class="pool-exp-hd"><span class="pool-exp-dot" style="background:' + cls + '"></span>'
      + '<span class="pool-exp-nm">' + nm + '</span>'
      + '<span class="pool-exp-days" style="color:' + cls + '">' + (days < 0 ? '已过期' : days + ' 天') + '</span></div>'
      + '<div class="pool-exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord + '）· 剩余 <b>' + fmtTok(avail) + '</b>' + unit
      + (days > 0 ? ' · 到期前日均需耗 ≥<b>' + fmtTok(daily) + '</b>' : '')
      + '<div class="pool-exp-note">账户级余额快照，非逐批；点「刷新余额」更新</div></div></div>';
  }).join('');
  host.innerHTML = rows || '<div class="empty">当前区域暂无账号</div>';
  if ($('poolExpNote')) {
    $('poolExpNote').textContent = list.length
      ? list.length + ' 个账号 · 快照（本地余额；点「刷新余额」重新查询）' : '';
  }
}
async function acctRefreshPoolBalances(btn) {
  const targets = acctScopeAccounts().filter(a => a.capabilities && a.capabilities.balance);
  if (!targets.length) { toast('当前区域没有支持余额查询的账号', 'mute'); return; }
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '刷新中…'; }
  let ok = 0, fail = 0;
  await Promise.all(targets.map(async a => {
    try {
      await api('accounts/' + encodeURIComponent(a.id) + '/platform_balance', { method: 'POST' });
      ok++;
    } catch (e) { fail++; }
  }));
  if (btn) { btn.disabled = false; btn.textContent = old; }
  toast('余额刷新完成：成功 ' + ok + (fail ? ' · 失败 ' + fail : ''), fail ? 'bad' : 'ok');
  await loadAcctAccounts();
}

function acctBalanceHtml(a) {
  if (a.balance && typeof a.balance.available === 'number') {
    const total = (typeof a.balance.total === 'number' && a.balance.total > 0
      && a.balance.total !== a.balance.available)
      ? ' / ' + esc(a.balance.total) : '';
    return esc(a.balance.available) + total + (a.balance.unit ? ' ' + esc(a.balance.unit) : '')
      + (a.balance.plan_name ? '<div class="note">' + esc(a.balance.plan_name) + '</div>' : '');
  }
  return '<span class="note">—</span>';
}

function acctAttentionHtml(scope) {
  if (!scope.length) return { cls: 'attn ok', html: '<div class="row">暂无平台账号</div>' };
  const items = [];
  scope.forEach(a => {
    const st = acctStateOf(a);
    if (st === 'invalid') {
      items.push({ cls: 'bad', tag: a.health_reason || '失效', label: a.label });
    } else if (st === 'unprojected') {
      items.push({ cls: 'warn', tag: '未投影', label: a.label });
    } else if (st === 'cooling') {
      items.push({ cls: 'warn', tag: '冷却', label: a.label });
    } else if (a.checkin && a.checkin.last_status === 'failed' && acctCheckinCurrent(a)) {
      items.push({ cls: 'warn', tag: '签到失败', label: a.label, detail: a.checkin.last_error || '' });
    }
  });
  if (!items.length) {
    return { cls: 'attn ok', html: '<div class="row">全部平台账号状态正常</div>' };
  }
  const bad = items.some(i => i.cls === 'bad');
  const rows = items.slice(0, 6).map(i =>
    '<div class="row"><span class="tag ' + i.cls + '">' + esc(i.tag) + '</span><b>' + esc(i.label) + '</b>'
    + (i.detail ? '<span class="note">' + esc(i.detail) + '</span>' : '') + '</div>').join('');
  return {
    cls: 'attn ' + (bad ? 'bad' : 'warn'),
    html: '<div class="row"><b>需要关注（' + items.length + '）</b></div>' + rows
  };
}

function acctUsageCells(a) {
  const s = a.stats || {};
  const req = Number(s.requests || 0);
  const err = Number(s.errors || 0);
  const ok = Math.max(0, req - err);
  const sf = (req || err) ? (ok + ' / ' + err) : '<span class="note">—</span>';
  const tok = Number(s.total_tokens || 0);
  const credits = Number(s.credits || 0);
  let usage = '';
  if (tok > 0) usage += fmtTok(tok) + ' tok';
  if (credits > 0) usage += (usage ? ' · ' : '') + fmtCredit(credits);
  return { sf: sf, usage: usage || '<span class="note">—</span>' };
}

function renderAcctUnifiedRow(a) {
  const cap = a.capabilities || {};
  const ch = a.channel || {};
  const inFlight = ch.in_flight || 0;
  const lastOK = ch.last_ok || ch.last_used;
  const u = acctUsageCells(a);
  const isCodeBuddy = a.source === 'codebuddy' || a.platform === 'codebuddy';
  if (isCodeBuddy) {
    return '<tr' + (a.disabled ? ' style="opacity:.55"' : '') + ' data-acct-id="' + esc(a.id) + '">'
      + '<td><b>' + esc(a.label) + '</b>' + (a.note ? '<div class="note">' + esc(a.note) + '</div>' : '')
      + '<div class="note"><code>' + esc(a.id) + '</code></div></td>'
      + acctPlatformTD(a)
      + '<td>' + acctStateHtml(a) + (a.health_reason ? '<div class="note">' + esc(a.health_reason) + '</div>' : '') + '</td>'
      + '<td>' + acctBalanceHtml(a) + '</td>'
      + '<td>' + acctCheckinText(a) + '</td>'
      + '<td>' + inFlight + '</td>'
      + '<td>' + u.sf + '</td>'
      + '<td>' + u.usage + '</td>'
      + '<td>' + (lastOK ? esc(ago(lastOK)) : '<span class="note">—</span>') + '</td>'
      + '<td class="c-acts"><button type="button" class="xs" data-act="pool" data-id="' + esc(a.id) + '">账号池</button></td>'
      + '</tr>';
  }
  const acts = [
    '<button type="button" class="xs" data-act="detail" data-id="' + esc(a.id) + '">详情</button>',
    '<button type="button" class="xs" data-act="probe" data-id="' + esc(a.id) + '">测试</button>'
  ];
  if (cap.refresh || a.refresh_supported) {
    acts.push('<button type="button" class="xs" data-act="refresh" data-id="' + esc(a.id) + '">刷新</button>');
  }
  if (cap.balance) {
    acts.push('<button type="button" class="xs" data-act="balance" data-id="' + esc(a.id) + '">余额</button>');
  }
  if (cap.checkin || (a.checkin && a.checkin.supported)) {
    acts.push('<button type="button" class="xs" data-act="checkin" data-id="' + esc(a.id) + '">签到</button>');
  }
  acts.push('<button type="button" class="xs" data-act="toggle" data-id="' + esc(a.id) + '">'
    + (a.disabled ? '启用' : '停用') + '</button>');
  acts.push('<button type="button" class="xs" data-act="del" data-id="' + esc(a.id) + '">删除</button>');
  return '<tr' + (a.disabled ? ' style="opacity:.55"' : '') + ' data-acct-id="' + esc(a.id) + '">'
    + '<td><b>' + esc(a.label) + '</b>'
    + (a.note ? '<div class="note">' + esc(a.note) + '</div>' : '')
    + '<div class="note"><code>' + esc(a.id) + '</code></div></td>'
    + acctPlatformTD(a)
    + '<td>' + acctStateHtml(a) + (a.health_reason ? '<div class="note">' + esc(a.health_reason) + '</div>' : '') + '</td>'
    + '<td>' + acctBalanceHtml(a) + '</td>'
    + '<td>' + acctCheckinText(a) + '</td>'
    + '<td>' + inFlight + '</td>'
    + '<td>' + u.sf + '</td>'
    + '<td>' + u.usage + '</td>'
    + '<td>' + (lastOK ? esc(ago(lastOK)) : '<span class="note">—</span>') + '</td>'
    + '<td class="c-acts">' + acts.join('') + '</td>'
    + '</tr>';
}

function renderAcctUnified() {
  const scope = acctScopeAccounts();
  const counts = { healthy: 0, cooling: 0, invalid: 0, disabled: 0, paused: 0, unprojected: 0 };
  scope.forEach(a => {
    const st = acctStateOf(a);
    counts[st] = (counts[st] || 0) + 1;
  });
  const platforms = new Set(scope.map(a => a.platform)).size;

  if ($('acctAllTitle')) {
    $('acctAllTitle').textContent = acctTab === 'all' ? '全部平台账号'
      : (ACCT_POOLS[acctTab] ? ACCT_POOLS[acctTab].name : acctTabDisplayName(acctTab) + ' 账号');
  }
  if ($('acctAllNote')) {
    const pool = ACCT_POOLS[acctTab];
    const region = pool ? pool.regions.find(r => r.key === acctPoolRegion) : null;
    const scopeText = pool
      ? (pool.multi
        ? (region && region.key !== 'all' ? ' · ' + region.name + '版' : ' · 国内 / 国际')
        : '')
      : ' · ' + platforms + ' 个平台';
    $('acctAllNote').textContent = scope.length
      ? scope.length + ' 个账号' + scopeText : '暂无平台账号';
  }
  renderAcctHero(scope, counts);
  if (ACCT_POOLS[acctTab]) renderQoderHero(scope, counts);

  const attn = acctAttentionHtml(scope);
  if ($('acctAttn')) {
    $('acctAttn').className = attn.cls;
    $('acctAttn').innerHTML = attn.html;
  }

  const filtered = acctFilteredAccounts();
  if ($('acctFilterNote')) {
    $('acctFilterNote').textContent = (acctStatusFilter || acctSearch)
      ? '筛选后 ' + filtered.length + ' 个' : '';
  }
  const tb = $('acBody-all');
  if (!tb) return;
  if (!scope.length) {
    const name = acctTab === 'all' ? '平台'
      : (ACCT_POOLS[acctTab] ? ACCT_POOLS[acctTab].short : acctTabDisplayName(acctTab));
    tb.innerHTML = '<tr><td colspan="10" class="empty"><div class="big">' + esc(acctNameJoin('还没有', name, '账号'))
      + '</div>点「添加账号」选择平台并粘贴凭证，保存后自动投影为托管渠道</div></tr>';
    return;
  }
  if (!filtered.length) {
    tb.innerHTML = '<tr><td colspan="10" class="empty">没有符合筛选条件的账号</td></tr>';
    return;
  }
  tb.innerHTML = filtered.map(renderAcctUnifiedRow).join('');
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
    + (a.refresh_supported
      ? '<button type="button" class="xs" data-act="refresh" data-id="' + esc(a.id) + '">刷新</button>' : '')
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

/* ── Qoder 池分析：负载趋势 / 在途与最近请求 / 模型可用性 ───────────
   外部渠道的用量按渠道记账（ext:qoder），逐账号成功/失败/用量暂不可得；
   这里用 model_series + request_logs 过滤出 Qoder 的渠道总量。 */
function acctQoderModelMatch(model, region) {
  const pool = ACCT_POOLS[acctTab];
  if (!pool) return false;
  const m = String(model || '');
  // 通用平台每账号一条渠道（c-<accountID>），模型键形如 ext:c-<id>/<model>；
  // 渠道名逐账号不同，不能像固定平台那样按单前缀匹配。
  if (pool.customModel) return /^ext:c-[^/]+\//.test(m);
  if (region === 'all') return pool.regions.some(r => r.prefix && m.indexOf(r.prefix) === 0);
  const r = pool.regions.find(x => x.key === region);
  return !!(r && r.prefix && m.indexOf(r.prefix) === 0);
}
function acctQoderModelName(key) {
  const pool = ACCT_POOLS[acctTab];
  const s = String(key || '');
  if (pool && pool.customModel) {
    const m = s.match(/^ext:c-[^/]+\/(.+)$/);
    if (m) return m[1];
  }
  for (const r of (pool && pool.regions) || []) {
    if (r.prefix && s.indexOf(r.prefix) === 0) return s.slice(r.prefix.length);
  }
  return s;
}
function acctQoderModelRegion(key) {
  const pool = ACCT_POOLS[acctTab];
  if (pool && pool.customModel) return pool.regions[0] || null;
  for (const r of (pool && pool.regions) || []) {
    if (r.prefix && String(key).indexOf(r.prefix) === 0) return r;
  }
  return null;
}
function acctQoderInFlight() {
  return acctScopeAccounts().reduce((n, a) => n + ((a.channel && a.channel.in_flight) || 0), 0);
}
function acctLogAccount(r) {
  const id = String(r.account || '');
  if (!id) return null;
  return acctAccounts.find(a => a.id === id || a.id.indexOf(id) === 0 || id.indexOf(a.id.slice(0, 8)) === 0) || null;
}
function acctLogAccountName(r) {
  const a = acctLogAccount(r);
  return a ? (a.label || a.id) : (String(r.account || '') || '未知账号');
}
function acctQoderReqTotal(usage) {
  let req = 0;
  for (const g of (usage && usage.model_series) || []) {
    if (!acctQoderModelMatch(g.key, acctPoolRegion)) continue;
    for (const p of g.points || []) req += Number(p.requests || 0);
  }
  return req;
}
function maybeLoadQoderViz(force) {
  if (!ACCT_POOLS[acctTab]) return;
  if (!$('acctPoolViz') || $('acctPoolViz').hidden) return;
  const now = Date.now();
  if (!force && now - qoderVizAt < 30000) return;
  if (qoderVizLoading) return;
  loadQoderPoolViz();
}
async function loadQoderPoolViz() {
  const pool = ACCT_POOLS[acctTab];
  if (!pool || qoderVizLoading) return;
  qoderVizLoading = true;
  const seq = ++qoderVizSeq;
  try {
    const usage7Promise = qoderLoadHours === 168 ? null : api('usage?hours=168').catch(() => null);
    const failPromise = api('request_logs?model=' + encodeURIComponent(pool.logFilter || '')
      + '&outcome=http_error&limit=20').catch(() => ({ entries: [] }));
    const metricsPromise = api('request_metrics').catch(() => ({}));
    const [usage, usage7, logs, fails, metrics] = await Promise.all([
      api('usage?hours=' + qoderLoadHours),
      usage7Promise,
      api('request_logs?model=' + encodeURIComponent(pool.logFilter || '') + '&limit=80').catch(() => ({ entries: [] })),
      failPromise,
      metricsPromise,
    ]);
    if (seq !== qoderVizSeq || !ACCT_POOLS[acctTab]) return;
    // 内存指标优先：归档落盘有延迟，资源受限/断开这类原因要立即显示。
    const memErr = ((metrics && metrics.recent) || [])
      .filter(r => r.ok === false && r.error && acctQoderModelMatch(r.model, acctPoolRegion));
    const arcErr = ((fails && fails.entries) || [])
      .filter(r => acctQoderModelMatch(r.model, acctPoolRegion) && r.error);
    qoderLatestError = memErr[0] || arcErr[0] || null;
    if (qoderLoadHours === 168) qoderPoolReq7d = acctQoderReqTotal(usage);
    else qoderPoolReq7d = usage7 ? acctQoderReqTotal(usage7) : null;
    renderQoderLoad(usage);
    renderQoderLive(logs);
    renderQoderModels(usage);
    renderQoderHero();
    qoderVizAt = Date.now();
  } catch (e) {
    /* 增强区：失败不打断账号中心 */
  } finally {
    qoderVizLoading = false;
  }
}
function renderQoderLoad(usage) {
  const host = $('qoderLoadChart');
  if (!host) return;
  const note = $('qoderLoadNote');
  const byT = new Map();
  for (const g of (usage && usage.model_series) || []) {
    if (!acctQoderModelMatch(g.key, acctPoolRegion)) continue;
    for (const p of g.points || []) {
      const key = String(p.t || '');
      if (!key) continue;
      const cur = byT.get(key) || { t: key, scope: p.scope || 'day', requests: 0, errors: 0,
        total_tokens: 0, prompt_tokens: 0, completion_tokens: 0, credits: 0 };
      cur.requests += Number(p.requests || 0);
      cur.errors += Number(p.errors || 0);
      cur.total_tokens += Number(p.total_tokens || 0);
      cur.prompt_tokens += Number(p.prompt_tokens || 0);
      cur.completion_tokens += Number(p.completion_tokens || 0);
      cur.credits += Number(p.credits || 0);
      byT.set(key, cur);
    }
  }
  const series = [...byT.values()].sort((a, b) => String(a.t).localeCompare(String(b.t)));
  const pts = [];
  for (const p of series) { const n = normUsagePoint(p); if (n) pts.push(n); }
  if (!pts.length) {
    const name = (ACCT_POOLS[acctTab] && ACCT_POOLS[acctTab].short) || '账号池';
    host.innerHTML = '<div class="us-empty">该范围暂无 ' + esc(name) + ' 请求数据</div>';
    if (note) note.textContent = '—';
    return;
  }
  const req = series.reduce((n, p) => n + Number(p.requests || 0), 0);
  const err = series.reduce((n, p) => n + Number(p.errors || 0), 0);
  const cred = series.reduce((n, p) => n + Number(p.credits || 0), 0);
  if (note) note.textContent = req + ' 请求 / ' + err + ' 失败'
    + (cred > 0 ? ' · ' + fmtCredit(cred) + ' 积分' : ' · 渠道未上报积分');
  renderQoderLoadSvg(host, pts);
}
function renderQoderLoadSvg(host, pts) {
  const W = 1200, H = 260, PL = 52, PR = 60, PT = 16, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...pts.map(p => p.req));
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const bw = Math.max(2, Math.min(34, iw * (minGap / span) * 0.7));
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);
  const cMax = Math.max(0.0001, ...pts.map(p => p.credits));
  const cyOf = v => PT + ih - ih * (v / cMax);
  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - ih * i / 4;
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="lb" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
    out += '<text class="lb-r" x="' + (W - PR + 6) + '" y="' + (y + 3.5).toFixed(1) + '">' + fmtCredit(cMax * i / 4) + '</text>';
  }
  const yBase = PT + ih;
  pts.forEach((p, i) => {
    const x = xOf(p.t) - bw / 2;
    out += '<g class="usbarg" data-i="' + i + '">';
    const h = ih * (p.req / max);
    if (p.req > 0) {
      out += '<rect class="bar" x="' + x.toFixed(1) + '" y="' + (yBase - h).toFixed(1) + '" width="' + bw.toFixed(1) + '" height="' + Math.max(1, h).toFixed(1) + '"/>';
      if (p.err > 0) {
        const eh = Math.max(2, h * p.err / p.req);
        out += '<rect class="bar err" x="' + x.toFixed(1) + '" y="' + (yBase - eh).toFixed(1) + '" width="' + bw.toFixed(1) + '" height="' + eh.toFixed(1) + '"/>';
      }
    }
    out += '<rect class="usbar-hit" x="' + x.toFixed(1) + '" y="' + PT + '" width="' + bw.toFixed(1) + '" height="' + ih + '"/>';
    out += '</g>';
  });
  const line = pts.map(p => xOf(p.t).toFixed(1) + ',' + cyOf(p.credits).toFixed(1)).join(' ');
  out += '<polyline class="cred" points="' + line + '"/>';
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) + '" y2="' + yBase + '"/>';
  const TICKS = Math.min(4, pts.length);
  const used = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const dd = Math.abs(pts[i].t - target);
      if (dd < best) { best = dd; bi = i; }
    }
    if (used.has(bi)) continue;
    used.add(bi);
    const p = pts[bi], cx = xOf(p.t);
    const anchor = cx < PL + 16 ? 'start' : (cx > W - PR - 16 ? 'end' : 'middle');
    out += '<text class="lb" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) + '" y="' + (H - 8) +
      '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }
  out += '</svg>';
  host.innerHTML = out;
  usTipHide();
  bindChartTips(host, pts, { html: p => usLoadTipHTML(p, true) });
}
function renderQoderLive(logs) {
  const st = $('qoderLiveStat'), host = $('qoderLiveLanes'), note = $('qoderLiveNote');
  const recent = ((logs && logs.entries) || [])
    .filter(r => acctQoderModelMatch(r.model, acctPoolRegion)).slice(0, 40);
  const latestErr = qoderLatestError || recent.find(r => r.ok === false && r.error);
  if ($('qoderLiveWarn')) {
    $('qoderLiveWarn').textContent = latestErr
      ? '最新失败：' + latestErr.error + (latestErr.time ? ' · ' + ago(latestErr.time) : '')
      : '';
    $('qoderLiveWarn').style.color = latestErr ? 'var(--bad)' : '';
  }
  const inFlight = acctQoderInFlight();
  const ok = recent.filter(r => r.ok !== false).length;
  const sr = recent.length ? Math.round(ok / recent.length * 100) : null;
  const avg = recent.length
    ? Math.round(recent.reduce((n, r) => n + Number(r.duration_ms || 0), 0) / recent.length) : null;
  if (st) {
    const srCls = sr == null ? '' : sr < 60 ? ' class="bad"' : sr < 85 ? ' class="warn"' : ' class="ok"';
    st.innerHTML = '<span class="ls"><b>' + inFlight + '</b><i>在途</i></span>'
      + '<span class="ls"><b' + srCls + '>' + (sr != null ? sr + '%' : '—') + '</b><i>成功率</i></span>'
      + '<span class="ls"><b>' + (avg != null ? avg + 'ms' : '—') + '</b><i>平均耗时</i></span>'
      + '<span class="ls-lg"><i><b class="dot ok"></b>成功</i><i><b class="dot no"></b>失败</i>'
      + '<i><b class="dot retry"></b>有重试</i><i><b class="dot sz"></b>点越大 token 越多</i></span>';
  }
  if (!host) return;
  if (!recent.length) {
    const name = (ACCT_POOLS[acctTab] && ACCT_POOLS[acctTab].short) || '账号池';
    host.innerHTML = '<div class="us-empty">暂无最近 ' + esc(name) + ' 请求</div>';
    if (note) note.textContent = '—';
    return;
  }
  const names = [];
  recent.forEach(r => {
    const n = acctLogAccountName(r);
    if (names.indexOf(n) < 0) names.push(n);
  });
  const laneH = 30, W = 640, plotX = 110, padR = 10, H = names.length * laneH + 24;
  const t0 = new Date(recent[recent.length - 1].time).getTime();
  const t1 = new Date(recent[0].time).getTime();
  const span = Math.max(60000, t1 - t0);
  const trim = n => (n.length > 14 ? n.slice(0, 14) + '…' : n);
  const hhmm = t => { const d = new Date(t); return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0'); };
  let out = '';
  names.forEach((n, li) => {
    const top = 4 + li * laneH, y = top + laneH / 2;
    out += '<rect class="lane-bg" x="' + plotX + '" y="' + top + '" width="' + (W - plotX - padR) + '" height="' + (laneH - 4) + '" rx="6"/>';
    out += '<text class="lane-lb" x="8" y="' + (y + 3) + '">' + esc(trim(n)) + '</text>';
    recent.forEach(r => {
      const rn = acctLogAccountName(r);
      if (rn !== n) return;
      const tt = new Date(r.time).getTime();
      const x = plotX + ((tt - t0) / span) * (W - plotX - padR);
      const tok = r.total_tokens || 0;
      const rad = Math.min(7, 2.5 + Math.sqrt(tok) / 45);
      const cy = y + ((li % 2) ? 5 : -5);
      out += '<circle class="pt ' + (r.ok === false ? 'no' : 'ok') + '" cx="' + x.toFixed(1) + '" cy="' + cy +
        '" r="' + rad.toFixed(1) + '"><title>' + esc(n + ' · ' + String(r.model || '') + ' · ' +
          Math.round(r.duration_ms || 0) + 'ms · ' + tok + ' tok' +
          ((r.attempts || 1) > 1 ? ' · 重试' + r.attempts + '次' : '')) + '</title></circle>';
      if ((r.attempts || 1) > 1) {
        out += '<circle class="pt retry" cx="' + x.toFixed(1) + '" cy="' + cy + '" r="' + (rad + 2.6).toFixed(1) + '"/>';
      }
    });
  });
  out += '<line class="now-line" x1="' + (W - padR) + '" y1="4" x2="' + (W - padR) + '" y2="' + (H - 16) + '"/>';
  out += '<text class="lane-lb" x="8" y="' + (H - 5) + '">' + hhmm(t0) + '</text>';
  out += '<text class="lane-lb" x="' + (W - padR) + '" y="' + (H - 5) + '" text-anchor="end">now</text>';
  host.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '">' + out + '</svg>';
  if (note) note.textContent = '最近 ' + recent.length + ' 条 · 按账号';
}
function renderQoderModels(usage) {
  const tb = $('qoderMlBody'), note = $('qoderMlNote');
  if (!tb) return;
  const pool = ACCT_POOLS[acctTab];
  let keys = [...new Set(((usage && usage.model_series) || [])
    .map(g => g.key)
    .filter(k => acctQoderModelMatch(k, acctPoolRegion)))].sort();
  // 通用平台的模型键带逐账号渠道名（ext:c-<id>/<model>）：同名模型会重复成行，
  // 锁池表按裸模型名聚合，与固定渠道池的「一行一模型」保持同构。
  if (pool && pool.customModel) {
    const seen = new Set();
    keys = keys.filter(k => {
      const n = acctQoderModelName(k);
      if (seen.has(n)) return false;
      seen.add(n);
      return true;
    });
  }
  if (!keys.length) {
    tb.innerHTML = '<tr><td colspan="8" class="empty">当前范围暂无 '
      + esc(pool ? pool.short : '账号池') + ' 模型请求，无法估算锁池</td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const scope = acctScopeAccounts();
  tb.innerHTML = keys.map(key => {
    const region = acctQoderModelRegion(key);
    const platform = (region && region.platform) || (pool ? acctPoolDefaultPlatform(pool, 'all') : '');
    const accounts = scope.filter(a => a.platform === platform);
    const total = accounts.length;
    const healthy = accounts.filter(a => acctStateOf(a) === 'healthy').length;
    const st = healthy ? ['ok', '可用'] : (total ? ['warn', '没号可用'] : ['mute', '无账号']);
    const scopeName = pool && pool.multi ? '当前区域' : '当前平台';
    const reason = healthy ? '外部渠道未上报逐模型锁池' : (total ? scopeName + '无可用账号' : scopeName + '无账号');
    // 单区域池（Loomy / TRAE / 通用平台）没有国内/国际之分，realm 直接展示池名。
    const realm = pool && pool.multi ? (region ? region.name + '版' : '—') : (pool ? pool.short : '—');
    return '<tr>'
      + '<td>' + esc(acctQoderModelName(key)) + '</td>'
      + '<td><span class="realm-tag">' + esc(realm) + '</span></td>'
      + '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>'
      + '<td>' + healthy + ' / ' + total + '</td>'
      + '<td>0</td><td>—</td><td>—</td>'
      + '<td><div class="note">' + esc(reason) + '</div></td>'
      + '</tr>';
  }).join('');
  if (note) note.textContent = '按账号可用性估算；' + (pool ? pool.short : '该平台') + ' 未上报逐模型锁池';
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
    } else if (b.dataset.act === 'refresh') {
      await acctRefresh(id, b);
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

/* acctRefresh 手动刷新登录态（AutoClaw / Qoder）：服务端刷新 → 回写 →
   立即重投影 managed 渠道。用于令牌临期自救与真实账号验收。 */
async function acctRefresh(id, btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '刷新中…'; }
  try {
    await api('accounts/' + encodeURIComponent(id) + '/refresh', { method: 'POST' });
    toast('登录态已刷新并同步到托管渠道', 'ok');
    await loadAcctAccounts();
  } catch (e) {
    toast('刷新失败：' + e.message, 'bad');
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
  acctOnPlatformChange();
  // 服务端能力开关异步到位后：仅当支持面判定翻转时才重跑（不打断进行中会话），
  // 重跑同样走统一入口——智能默认按真实支持面重新生效。
  acctLoadCapabilities().then(caps => {
    if (!caps) return;
    if (acctBrowserSupported(plat) !== capsBefore
      && $('accAddVeil') && $('accAddVeil').classList.contains('on')) {
      acctOnPlatformChange();
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

/* 页签切换（app.js 的锚点重定向也调用）；同步地址栏，保证 URL 能表达当前平台。
   Qoder 国内/国际是同一池的两个区域：旧 key 自动映射到 qoder-pool + 区域。 */
function selectAcctTab(key) {
  let next = key;
  const mapped = ACCT_PLATFORM_POOL[key];
  if (mapped) {
    next = mapped.pool;
    acctPoolRegion = mapped.region;
    qoderPoolReq7d = null;
    qoderLatestError = null;
  } else if (ACCT_POOLS[key]) {
    next = key;
    if (key !== acctTab) acctPoolRegion = 'all';
    qoderPoolReq7d = null;
    qoderLatestError = null;
  } else {
    acctPoolRegion = 'all';
  }
  acctTab = (next === 'codebuddy' || next === 'all' || ACCT_PLATFORMS[next] || ACCT_POOLS[next]) ? next : 'all';
  applyAcctTab();
  if (view === 'accounts-center') {
    history.replaceState(null, '', acctTab === 'all' ? '#accounts-center' : '#accounts-center/' + acctTab);
  }
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

/* ── 统一账号表：行点击 / 详情抽屉 ─────────────────────────────────── */

function bindAcctUnifiedTable() {
  const tb = $('acBody-all');
  if (!tb) return;
  tb.onclick = async e => {
    const gotoEl = e.target.closest('[data-goto]');
    if (gotoEl) {
      e.stopPropagation();
      selectAcctTab(gotoEl.dataset.goto);
      return;
    }
    const btn = e.target.closest('button[data-act]');
    const tr = e.target.closest('tr[data-acct-id]');
    const id = (btn && btn.dataset.id) || (tr && tr.dataset.acctId);
    if (!id) return;
    const a = acctAccounts.find(x => x.id === id);
    /* 行点击 = 进入该平台视图（CodeBuddy → 池；Qoder 等 → 平台筛选），与平台名一致；
       详情走显式「详情」按钮，避免两套点击语义。 */
    if (!btn) {
      if (a) selectAcctTab(a.platform);
      return;
    }
    e.stopPropagation();
    if (btn.dataset.act === 'pool') { selectAcctTab('codebuddy'); return; }
    switch (btn.dataset.act) {
      case 'detail': await openAcctDetail(id); break;
      case 'toggle': await acctSetState(id, !(a && a.disabled), btn); break;
      case 'probe': await acctProbe(id, btn); break;
      case 'refresh': await acctRefresh(id, btn); break;
      case 'balance': await acctBalance(id, btn); break;
      case 'checkin': await acctCheckin(id, btn); break;
      case 'del': await acctDelete(id, a ? a.label : id); break;
    }
  };
}

function acctDetailKV(k, v) {
  return '<div><div class="note">' + esc(k) + '</div><div>' + v + '</div></div>';
}

function renderAcctDetail(a) {
  if (!$('acctDetailBody') || !a) return;
  const ch = a.channel || {};
  const cap = a.capabilities || {};
  const tags = [];
  if (cap.login) tags.push('<span class="tag ok">浏览器登录</span>');
  if (cap.checkin) tags.push('<span class="tag ok">平台签到</span>');
  if (cap.refresh) tags.push('<span class="tag ok">可刷新</span>');
  if (cap.balance) tags.push('<span class="tag ok">余额查询</span>');
  if (cap.probe) tags.push('<span class="tag mute">可探测</span>');
  const history = (a.checkin && a.checkin.history) || [];
  const histHtml = history.length
    ? '<div class="tbl-wrap"><table class="acc zebra"><thead><tr><th>时间</th><th>结果</th><th>奖励</th><th>说明</th></tr></thead><tbody>'
      + history.slice(0, 20).map(r => '<tr><td>' + esc(ago(r.at)) + '</td><td>' + esc(r.status)
        + '</td><td>' + esc(r.reward || '—') + '</td><td>' + esc(r.message || r.error || '—') + '</td></tr>').join('')
      + '</tbody></table></div>'
    : '<div class="note">暂无签到记录</div>';
  const actions = [
    '<button type="button" class="xs" data-detail-act="probe" data-id="' + esc(a.id) + '">测试</button>'
  ];
  if (cap.refresh || a.refresh_supported) {
    actions.push('<button type="button" class="xs" data-detail-act="refresh" data-id="' + esc(a.id) + '">刷新</button>');
  }
  if (cap.balance) {
    actions.push('<button type="button" class="xs" data-detail-act="balance" data-id="' + esc(a.id) + '">余额</button>');
  }
  if (cap.checkin || (a.checkin && a.checkin.supported)) {
    actions.push('<button type="button" class="xs" data-detail-act="checkin" data-id="' + esc(a.id) + '">签到</button>');
  }
  actions.push('<button type="button" class="xs" data-detail-act="toggle" data-id="' + esc(a.id) + '">'
    + (a.disabled ? '启用' : '停用') + '</button>');
  actions.push('<button type="button" class="xs" data-detail-act="del" data-id="' + esc(a.id) + '">删除</button>');

  if ($('acctDetailTitle')) $('acctDetailTitle').textContent = a.label || '账号详情';
  if ($('acctDetailSub')) $('acctDetailSub').textContent = acctPlatformName(a) + ' · ' + a.id;
  if ($('acctDetailNote')) $('acctDetailNote').textContent = a.health_reason || '';
  $('acctDetailBody').innerHTML =
    '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">'
    + acctStateHtml(a) + (a.health_reason ? '<span class="note">' + esc(a.health_reason) + '</span>' : '')
    + '</div>'
    + '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px 18px;margin-top:14px;font-size:var(--fs-sm)">'
    + acctDetailKV('平台', '<button type="button" class="acct-plat" data-detail-goto="' + esc(a.platform) + '" title="只看该平台账号">' + esc(acctPlatformName(a)) + '</button>')
    + acctDetailKV('账号 ID', '<code>' + esc(a.id) + '</code>')
    + acctDetailKV('添加时间', esc(a.created_at || '—'))
    + acctDetailKV('备注', esc(a.note || '—'))
    + acctDetailKV('余额', a.balance ? (esc(a.balance.available) + (a.balance.unit ? ' ' + esc(a.balance.unit) : '')) : '—')
    + acctDetailKV('最近使用', ch.last_used ? esc(ago(ch.last_used)) : '—')
    + '</div>'
    + '<div class="note" style="margin-top:14px">平台能力</div>'
    + '<div style="display:flex;gap:6px;flex-wrap:wrap;margin-top:4px">' + (tags.join('') || '<span class="note">—</span>') + '</div>'
    + '<div class="note" style="margin-top:14px">托管渠道</div>'
    + '<div style="font-size:var(--fs-sm);margin-top:4px">'
    + (ch.exists
      ? ('渠道 <code>' + esc(ch.name || '') + '</code> · ' + (ch.managed ? '托管' : '普通')
        + ' · 凭证 ' + esc(ch.credentials_in_channel || 0)
        + (ch.state ? ' · 状态 ' + esc(ch.state) : '')
        + (ch.cool_remaining_sec ? ' · 冷却剩余 ' + esc(dur(ch.cool_remaining_sec)) : '')
        + (ch.invalid_reason ? ' · 原因 ' + esc(ch.invalid_reason === 'auth' ? '待重登' : (ch.invalid_reason === 'quota' ? '额度耗尽' : ch.invalid_reason)) : ''))
      : '<span class="bad">托管渠道缺失（保存配置或重启后由 Reconcile 重建）</span>')
    + '</div>'
    + '<div class="note" style="margin-top:14px">签到历史（最近 20 条）</div>'
    + '<div style="margin-top:4px">' + histHtml + '</div>'
    + '<div style="display:flex;gap:8px;flex-wrap:wrap;margin-top:16px">' + actions.join('') + '</div>';
}

async function openAcctDetail(id) {
  acctDetailID = id;
  const veil = $('acctDetailVeil');
  if (!veil) return;
  const summary = acctAccounts.find(a => a.id === id);
  if (summary) renderAcctDetail(summary);
  veil.classList.add('on');
  try {
    const d = await api('accounts/' + encodeURIComponent(id));
    if (acctDetailID === id) renderAcctDetail(d);
  } catch (e) {
    if ($('acctDetailNote')) $('acctDetailNote').textContent = '详情读取失败：' + e.message;
  }
}

function closeAcctDetail() {
  acctDetailID = '';
  const veil = $('acctDetailVeil');
  if (veil) veil.classList.remove('on');
}

/* acctAutoBalance 首次进入账号中心时自动拉一次余额（仅缺快照且有能力的账号）；
   每页生命周期只尝试一次，失败不打扰，用户可手动点「余额」。 */
function acctAutoBalance() {
  if (!acctAccounts.length) return;
  // 每个账号只自动试一次：新添加的账号会在下一次 loadAcctAccounts 被捞到；
  // 查询失败的账号不无限重试，仍可手动点「余额」。
  const targets = acctAccounts.filter(a => a.capabilities && a.capabilities.balance
    && !a.balance && !acctBalanceTried.has(a.id));
  if (!targets.length) return;
  targets.forEach(a => acctBalanceTried.add(a.id));
  Promise.all(targets.map(a => api('accounts/' + encodeURIComponent(a.id) + '/platform_balance', { method: 'POST' })
    .catch(() => null))).then(rs => {
      if (rs.some(Boolean)) loadAcctAccounts();
    });
}

async function acctBalance(id, btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '查询中…'; }
  try {
    const d = await api('accounts/' + encodeURIComponent(id) + '/platform_balance', { method: 'POST' });
    const b = d.balance || {};
    toast('余额已更新：' + (b.available != null
      ? (Math.round(b.available * 100) / 100) + (b.unit ? ' ' + b.unit : '') : '—'), 'ok');
    await loadAcctAccounts();
  } catch (e) {
    toast('余额查询失败：' + e.message, 'bad');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

async function acctRefreshAll(btn) {
  const list = acctScopeAccounts().filter(a => a.capabilities && a.capabilities.refresh);
  if (!list.length) { toast('当前范围内没有可刷新的账号（需要 refresh_token）', 'mute'); return; }
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; }
  let ok = 0, fail = 0;
  for (let i = 0; i < list.length; i++) {
    if (btn) btn.textContent = '刷新中 ' + (i + 1) + '/' + list.length;
    try {
      await api('accounts/' + encodeURIComponent(list[i].id) + '/refresh', { method: 'POST' });
      ok++;
    } catch (e) { fail++; }
  }
  if (btn) { btn.disabled = false; btn.textContent = old; }
  toast('刷新完成：成功 ' + ok + (fail ? ' · 失败 ' + fail : ''), fail ? 'bad' : 'ok');
  await loadAcctAccounts();
}

/* ── 事件绑定（顶层守卫：元素不存在时静默跳过） ───────────────────── */

if ($('acctPlatformSelect')) {
  $('acctPlatformSelect').onchange = () => selectAcctTab($('acctPlatformSelect').value);
}
if ($('btnAcctAdd')) {
  $('btnAcctAdd').onclick = () => {
    if (acctTab === 'codebuddy') {
      openAdd(); // CodeBuddy 登录弹窗的唯一直达入口（v1.7.2 入口唯一化）
      return;
    }
    acctGlobalAddAccount();
  };
}
if ($('acctRegionTabs')) {
  $('acctRegionTabs').onclick = e => {
    const b = e.target.closest('button[data-region]');
    if (!b) return;
    acctPoolRegion = b.dataset.region || 'all';
    qoderVizAt = 0;
    qoderPoolReq7d = null;
    qoderLatestError = null;
    applyAcctTab();
  };
}
if ($('poolExpRefresh')) {
  $('poolExpRefresh').onclick = () => acctRefreshPoolBalances($('poolExpRefresh'));
}
if ($('qoderLoadRange')) {
  $('qoderLoadRange').onchange = () => {
    qoderLoadHours = Number($('qoderLoadRange').value) || 0;
    qoderVizAt = 0;
    maybeLoadQoderViz(true);
  };
}
if ($('acctSearch')) $('acctSearch').oninput = () => {
  acctSearch = $('acctSearch').value || '';
  renderAcctUnified();
};
if ($('acctStatusFilter')) $('acctStatusFilter').onchange = () => {
  acctStatusFilter = $('acctStatusFilter').value || '';
  renderAcctUnified();
};
if ($('btnAcctRefreshAll')) $('btnAcctRefreshAll').onclick = () => acctRefreshAll($('btnAcctRefreshAll'));
bindAcctUnifiedTable();
if ($('acctDetailClose')) $('acctDetailClose').onclick = closeAcctDetail;
if ($('acctDetailVeil')) {
  $('acctDetailVeil').onclick = e => { if (e.target === $('acctDetailVeil')) closeAcctDetail(); };
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && $('acctDetailVeil').classList.contains('on')) closeAcctDetail();
  });
}
if ($('acctDetailBody')) {
  $('acctDetailBody').onclick = async e => {
    const gotoEl = e.target.closest('[data-detail-goto]');
    if (gotoEl) {
      e.stopPropagation();
      closeAcctDetail();
      selectAcctTab(gotoEl.dataset.detailGoto);
      return;
    }
    const b = e.target.closest('button[data-detail-act]');
    if (!b) return;
    const id = b.dataset.id || acctDetailID;
    const a = acctAccounts.find(x => x.id === id) || {};
    switch (b.dataset.detailAct) {
      case 'probe': await acctProbe(id, b); break;
      case 'refresh': await acctRefresh(id, b); break;
      case 'balance': await acctBalance(id, b); break;
      case 'checkin': await acctCheckin(id, b); break;
      case 'toggle': await acctSetState(id, !a.disabled, b); break;
      case 'del':
        await acctDelete(id, a.label || id);
        // 用户取消 confirm 时账号仍在：保持抽屉；删除成功才关闭。
        if (!acctAccounts.find(x => x.id === id)) closeAcctDetail();
        else await openAcctDetail(id);
        return;
    }
    await openAcctDetail(id);
  };
}
if ($('btnCheckinAll')) $('btnCheckinAll').onclick = () => acctCheckinAll($('btnCheckinAll'));
if ($('btnExtDrawer')) $('btnExtDrawer').onclick = () => openAcctDrawer(!acctDrawerOpen);
if ($('btnExtDrawerClose')) $('btnExtDrawerClose').onclick = () => openAcctDrawer(false);
if ($('btnAcctConsistency')) $('btnAcctConsistency').onclick = acctConsistency;
if ($('accAddPlatform')) $('accAddPlatform').onchange = acctOnPlatformChange;
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
   autoclaw（手机验证码两步）、autoclaw-intl（阿里云滑块 + 回环 OAuth）；
   loomy / custom 仅粘贴。 */

const ACCT_BROWSER_PLATFORMS = {
  qoder: 'Qoder 国内',
  'qoder-intl': 'Qoder 国际',
  autoclaw: 'AutoClaw 国内',
  'autoclaw-intl': 'AutoClaw 国际',
  trae: 'TRAE（SOLO 国内）'
};
const ACCT_LOGIN_POLL_MS = 2000;   // wait 轮询间隔（规格 §3.1）
const ACCT_SMS_RESEND_SEC = 60;    // 发码倒计时（规格 R3 频控对策）

/* 灰 Tab 的按平台原因文案（v1.7.3 用户实测反馈：落在灰 Tab 上不知道为什么选不了）。
   未收录平台走兜底文案。 */
const ACCT_BROWSER_BLOCKED_REASON = {
  loomy: 'Loomy 无登录流程——请从客户端登录态复制 session 后用粘贴凭证',
  custom: '通用平台无统一登录——粘贴 API 凭证'
};
const ACCT_BROWSER_BLOCKED_FALLBACK =
  '该平台暂不支持浏览器登录（当前支持：Qoder 国内/国际、AutoClaw 国内/国际、TRAE）';

/* 平台下拉选项的基础文案（acctSyncPlatformOptions 据此追加能力标记）。 */
const ACCT_PLATFORM_OPTION_LABELS = {
  codebuddy: 'CodeBuddy（WorkBuddy）',
  loomy: 'Loomy（讯飞 · 双头 session）',
  autoclaw: 'AutoClaw 国内（智谱）',
  'autoclaw-intl': 'AutoClaw 国际（智谱）',
  qoder: 'Qoder 国内（阿里）',
  'qoder-intl': 'Qoder 国际（阿里）',
  trae: 'TRAE（字节 · SOLO 国内）',
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

/* acctOnPlatformChange 平台切换的**统一入口**（v1.7.4：向导打开与下拉 change
   都走它，杜绝两处逻辑漂移导致的"切平台后状态不刷新/原因文案张冠李戴"）。
   完整重算：下拉能力标记 → Tab 置灰 → 原因文案（支持平台=清空，绝不残留
   其它平台的文案）→ 智能默认 Tab（支持→浏览器登录；不支持→粘贴凭证）。

   语义约定：**平台切换视为重新开始**——用户在旧平台手动选的「粘贴凭证」Tab
   不被继承（新平台支持面不同，继承只会复刻"灰 Tab / 错误 Tab"的困惑）；
   同一平台内手动切 Tab 的选择仍被尊重（切 Tab 不经过本函数）。 */
function acctOnPlatformChange() {
  acctAddSchemaUI();
  acctSyncPlatformOptions();
  const platform = $('accAddPlatform') ? $('accAddPlatform').value : 'loomy';
  acctSetMode(acctBrowserSupported(platform) ? 'browser' : 'paste');
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
  // CodeBuddy 走专用登录弹窗（openAdd），不走 acclogin——服务端 capabilities
  // 清单不含它，这里恒视为支持（前端内置，v1.7.5）。
  if (p === 'codebuddy') return true;
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
  // v1.7.5：CodeBuddy 没有粘贴凭证形态——选中它时隐藏模式 Tab 区与原因文案区，
  // 只留一句「将打开 CodeBuddy 登录窗口」的说明（见 acctBrowserUIReset），
  // 避免再出现"没有解释的灰 Tab"。
  const isCodeBuddy = platform === 'codebuddy';
  if ($('accAddMode')) $('accAddMode').hidden = isCodeBuddy;
  if (hint) { hint.hidden = supported || isCodeBuddy; }
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
    hint.hidden = supported || isCodeBuddy;
    hint.textContent = supported || isCodeBuddy ? '' : blockedMsg;
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
  const isIntl = platform === 'autoclaw-intl';
  const isCodeBuddy = platform === 'codebuddy';
  if ($('abHint')) {
    $('abHint').textContent = isCodeBuddy
      ? '将打开 CodeBuddy 登录窗口（国内 / 国际版可在其中选择）；完成登录后账号自动入池并投影为托管渠道。'
      : isSms
        ? '输入 AutoClaw 国内版绑定的手机号获取短信验证码；登录成功后账号自动入池并投影为托管渠道。'
        : isIntl
          ? '点击「获取授权链接」后会先弹出阿里云滑块验证；通过后跳转 AutoClaw 国际授权页，完成后本窗口自动检测并入池（最长约 5 分钟）。'
          : '点击「获取授权链接」，在浏览器完成 ' + (ACCT_BROWSER_PLATFORMS[platform] || platform)
            + ' 登录；本窗口会自动检测登录结果（最长约 5 分钟）。';
  }
  if ($('abSmsRow')) $('abSmsRow').hidden = !isSms;
  if ($('abUrlRow')) $('abUrlRow').hidden = true;
  if ($('abStatus')) { $('abStatus').hidden = true; $('abStatus').textContent = ''; $('abStatus').style.color = ''; }
  if ($('abStart')) {
    $('abStart').hidden = false;
    $('abStart').textContent = isCodeBuddy ? '打开 CodeBuddy 登录窗口'
      : (isSms ? '发送验证码并开始登录' : '获取授权链接');
    $('abStart').disabled = false;
  }
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
    let d;
    if (platform === 'autoclaw-intl') {
      if (typeof AliyunCaptcha === 'undefined') throw new Error('滑块组件未加载，请刷新页面后重试');
      acctLoginStatus('请完成阿里云滑块验证…');
      const cfg = await api('accounts/login/captcha?platform=autoclaw-intl');
      d = await AliyunCaptcha.solve(cfg, async captcha => {
        acctLoginStatus('风控通过，正在创建授权会话…');
        return await api('accounts/login/start', {
          method: 'POST',
          body: JSON.stringify({ platform: platform, captcha: captcha, vendor: 'zai' })
        });
      });
    } else {
      const body = { platform: platform };
      if (isSms) body.phone = phone;
      d = await api('accounts/login/start', { method: 'POST', body: JSON.stringify(body) });
    }
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
/* 浏览器面板主按钮（v1.7.5 分流）：CodeBuddy → 关闭平台向导并打开 CodeBuddy
   登录弹窗（openAdd，与 CodeBuddy 页签内按钮行为完全一致）；其它平台 → 登录
   会话启动（acctLoginStart）。 */
function acctBrowserPrimaryAction() {
  const platform = $('accAddPlatform') ? $('accAddPlatform').value : '';
  if (platform === 'codebuddy') {
    closeAcctAdd(); // 关闭平台向导
    openAdd();      // 打开 CodeBuddy 登录弹窗（app.js，国内/国际版）
    return;
  }
  acctLoginStart();
}
if ($('abStart')) $('abStart').onclick = acctBrowserPrimaryAction;
if ($('abRestart')) $('abRestart').onclick = acctLoginStart; // start 可重入（旧会话自动 cancel）
if ($('abCancelLogin')) $('abCancelLogin').onclick = acctLoginCancel;
if ($('abSendCode')) $('abSendCode').onclick = acctSmsSendOrResend; // 首发=start；重发=sms/send（60s 限频）
if ($('abSubmit')) $('abSubmit').onclick = acctSmsVerify;
if ($('abCopyBtn')) $('abCopyBtn').onclick = acctCopyLoginUrl;

/* 账号列表静默轮询（只读本地状态，不打上游）：后台签到/余额刷新后最多 1 分钟
   反映到页面；从后台切回时立即刷新，避免隔天回来仍看到昨天的签到结果。 */
function acctPollDue() {
  return typeof view !== 'undefined' && view === 'accounts-center'
    && acctTab !== 'codebuddy' && !acctLoading;
}
const acctPollTimer = setInterval(() => {
  if (!acctPollDue()) return;
  if (typeof document.visibilityState === 'string' && document.visibilityState === 'hidden') return;
  loadAcctAccounts();
}, 60000);
// Node 测试桩里 unref 让进程能正常退出；浏览器返回数字，无此方法。
if (acctPollTimer && typeof acctPollTimer.unref === 'function') acctPollTimer.unref();
document.addEventListener('visibilitychange', () => {
  if (acctPollDue() && document.visibilityState === 'visible') loadAcctAccounts();
});

/* 侧栏显示实际加载的前端构建号：在本脚本执行时读取自身 ?v=（app.js 执行时
   acct.js 的 <script> 尚未解析，查询会落空）。 */
(function () {
  const s = document.currentScript
    || (typeof document.querySelector === 'function' ? document.querySelector('script[src*="acct.js"]') : null);
  if (!s || !$('aboutUI')) return;
  try {
    $('aboutUI').textContent = new URL(s.src, location.href).searchParams.get('v') || '—';
  } catch (_) { /* 桩环境 / 异常 URL：保持占位 */ }
})();

/* 初始路由放在本文件末尾：确保 acctTab / loadAcctCenter / selectAcctTab 已初始化，
   避免 app.js 的早期调用踩 TDZ，也避免慢网络下默认视图未应用。 */
if (typeof initialRoute === 'function') initialRoute();
