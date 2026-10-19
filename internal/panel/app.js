'use strict';
/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /startport/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用

const TITLES = { 'accounts-center': '账号中心', usage: '用量', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志', keys: 'API 密钥' };
/* 账号中心是「双 section 组合视图」：CodeBuddy 页签 = 原 view-accounts（DOM 原样），
   平台页签与渠道设置抽屉 = view-acctcenter。go() 对它做组级显隐（acct.js 管页签内切换）。 */
const ACCT_CENTER_SECTIONS = ['view-accounts', 'view-acctcenter'];
function go(v) {
  /* 离开账号中心时提示进行中的浏览器登录会话（v1.7.0；不阻塞导航，
     会话在后端仍有效，回到账号中心可继续查看）。 */
  if (view === 'accounts-center' && v !== 'accounts-center' && typeof acctLeaveCenterNotify === 'function') {
    acctLeaveCenterNotify();
  }
  /* 老锚点重定向（架构 §7.1，hash 同步到 #accounts-center）：
     #accounts → 账号中心 CodeBuddy 页签；#extchan → 账号中心 + 渠道设置抽屉展开。 */
  if (v === 'accounts' || v === 'extchan') {
    const redirectTab = v === 'accounts' ? 'codebuddy' : null;
    v = 'accounts-center';
    view = v; // 先落位：selectAcctTab/openAcctDrawer 里的 inCenter 判定依赖它
    if (typeof selectAcctTab === 'function') {
      if (redirectTab) selectAcctTab(redirectTab);
      else if (typeof openAcctDrawer === 'function') openAcctDrawer(true);
    }
    history.replaceState(null, '', '#accounts-center');
  }
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = v === 'accounts-center'
    ? !ACCT_CENTER_SECTIONS.includes(s.id) : s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v] || v;
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'keys') loadKeys();
  if (v === 'accounts-center' && typeof loadAcctCenter === 'function') loadAcctCenter();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  // 到期提醒卡片：默认可见，进页面即用 overview 快照渲染（零上游请求，随首页统计
  // 每轮刷新）。不再「打开账号池就打上游」——逐账号实时查在账号多时会卡死面板。
  // 完整逐包明细由卡片上的「检查」按钮显式触发（loadPackages/loadExpiry）。
  if (v === 'taskscenter') reattachQueueView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
function renderAccounts(list) {
  const tb = $('accBody');
  // 「⋯」菜单开着时先不重建表格：否则按钮被换掉、菜单失去锚点（5 秒轮询会触发重渲染）。
  if (actsMenuEl && !actsMenuEl.hidden) return;
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    // 冷却原因备注：仅「正在冷却」时显示（与徽标倒计时互补）。恢复后一律不显示——
    // 后端 statusOf 已按查询期过滤、flusher 每 5s 扫除过期残留，这里是最后一道
    // 前端防线（覆盖到期与扫除之间的窗口）。
    const note = (s.reason && cool > 0)
      ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>'
      : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    // 本机观测消耗：上游对账有延迟时余额可能暂时不动，这里仍能看到消耗。
    // 仅展示用——不参与选号，也不改写剩余积分（上游一旦结算本项自动归零）。
    const obs = Number(s.credits_observed) > 0 ? Number(s.credits_observed) : 0;
    const obsHtml = obs > 0
      ? '<span class="of" title="本机观测消耗（上游余额尚未结算）"> −' + obs.toFixed(1) + '</span>' : '';
    const cred = (unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)))) + obsHtml;
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    if (obs > 0) {
      credTip += '\n本机观测已耗 ' + obs.toFixed(2) + ' 积分（上游余额尚未结算；上游一旦结算本项自动归零）';
    }
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred r" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num r">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num r">' + (s.in_flight || 0) + '</td>' +
      '<td class="num r usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num r" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      // 操作列：行内只留「1 个主操作 + ⋯」，其余收进浮层菜单。
      // 6 个文字按钮平铺会把这一列撑到 332px（占整表 1/3），并把行高从 52px 抬到 92px，
      // 换行后左缘还参差、危险操作（移除）抢走第一视线——都是"次要内容支配主要布局"。
      // 主操作：个人版给最高频的「签到」；企业版/国际版无签到能力，退回「余额」。
      // 两者都是 2 字按钮，宽度一致，所以最左侧位置在各类账号之间保持对齐。
      // 企业版无个人成长体系（签到 400 / 成长任务 403）；国际版同样无 CN 域对应端点。
      '<td class="acts"><div class="acts-wrap">' +
        // 主操作统一为「余额」：国内版与国际版都用它，按钮宽度一致，
        // 最左侧位置因此在各类账号之间天然对齐。
        // （企业版标签仍为「额度」——它走的是另一套接口 get-enterprise-user-usage，
        //   语义是"本周期已分配额度"而非"剩余积分"，强行合并会误导。）
        // 「签到」移入菜单：它是每日例行，且顶部已有「全部签到」批量入口兜底。
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>' +
        '<button class="xs ghost act-more" data-a="more" data-u="' + esc(s.uid) + '" title="更多操作" aria-haspopup="menu" aria-expanded="false">⋯</button>' +
        '<div class="acts-menu" role="menu" hidden>' +
          ((s.enterprise || s.realm === 'global') ? '' :
            '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
          ((s.enterprise || s.realm === 'global') ? '' :
            '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
          (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                  : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复</button>'
                              : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : (s.realm === 'global' ? '退出选号，但照常活跃上报 / 保活 / 刷新余额' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额')) + '">暂停</button>')) +
          '<div class="sep"></div>' +
          (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
          '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
        '</div>' +
      '</div></td></tr>';
  }).join('');
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  if (!rows || !rows.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  tb.innerHTML = rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
      '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join('');
  if (note) {
    const bad = rows.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : rows.length + ' 个模型部分限流';
  }
}

/* E3/E4 的刷新节流器：overview 每 5 秒轮询一次，但负载带与泳道不需要这么频繁，
   30 秒足够（且避免每 5 秒额外打两个接口）。首次调用必定执行。 */
let vizAt = 0;
async function refreshViz() {
  const now = Date.now();
  if (now - vizAt < 30000) return;
  vizAt = now;
  await Promise.all([loadUsageMini(), loadLive()]);
}

/* ── E2 需要关注：把指标翻译成动作 ──────────────────────────────────
   0 条时显示安静态——此时的"留白"本身就是健康信号，不要为了不空而刷存在感。 */
function renderAttn(d) {
  const box = $('attnBox'); if (!box) return;
  const items = [], now = Date.now();
  (d.accounts || []).forEach(s => {
    const nm = s.nickname || String(s.uid || '').slice(0, 8) || '未知';
    if (s.disabled) items.push({ sev: 2, html: '<b>' + esc(nm) + '</b> 已禁用，不参与选号' });
    const until = new Date(s.until || 0).getTime();
    if (!s.disabled && s.cooling && until > now)
      items.push({ sev: 1, html: '<b>' + esc(nm) + '</b> 冷却中，约 ' + Math.max(1, Math.round((until - now) / 60000)) + ' 分钟后恢复' });
    const rl = (s.rate_limited_models || []).length;
    if (rl) items.push({ sev: 1, html: '<b>' + esc(nm) + '</b> 上有 ' + rl + ' 个模型被限流' });
    // 积分到期预警：此前误把 credits_earliest_remaining（该批剩余积分量）当「天数」
    // 与 7 比较，语义全错。改走 attnEarliestExpiry 三级降级取数（见其注释），
    // 口径与下方积分到期卡完全一致；分档也对齐到期卡：≤3 天红（sev 2）、
    // ≤7 天琥珀（sev 1），更远不提醒。
    const ex = attnEarliestExpiry(s);
    if (ex && ex.days >= 0 && ex.days <= 7) {
      const word = ex.days === 0 ? '今天到期' : ex.days === 1 ? '明天到期' : ex.days + ' 天后到期';
      items.push({ sev: ex.days <= 3 ? 2 : 1, html: '<b>' + esc(nm) + '</b> 最早一批积分 ' + word +
        '（该批 ' + fmtTok(ex.remain) + ' 积分）' });
    }
    const le = new Date(s.last_err || 0).getTime();
    if (le && now - le < 3600000) items.push({ sev: 1, html: '<b>' + esc(nm) + '</b> 近 1 小时内有失败请求' });
  });
  items.sort((a, b) => b.sev - a.sev);
  if (!items.length) {
    box.className = 'attn ok';
    box.innerHTML = '<div class="row">全部正常 · 上次检查 ' + new Date().toTimeString().slice(0, 5) + '</div>';
    return;
  }
  box.className = 'attn ' + (items[0].sev >= 2 ? 'bad' : 'warn');
  // 行前色点由 .attn .row::before 提供（批次 3 改版），文本前缀不再需要。
  box.innerHTML = '<div class="row"><b>需要关注（' + items.length + '）</b></div>' +
    items.map(i => '<div class="row">' + i.html + '</div>').join('');
}

/* ── 负载趋势（原「近 7 天负载」）────────────────────────────────────
   柱 = 请求（红段 = 失败），线 = 积分消耗（右轴独立刻度）。带完整坐标轴
   （左轴请求 / 右轴积分 / 底部时间）与悬停富 tooltip（复用用量页的 usTip
   委托组件）；范围可选（24h/3天/7天/30天/全部历史），长范围自动按天聚合
   （复用用量页的 mergeDaily），短范围保持小时粒度——与用量页主图同口径。 */
let loadRangeHours = 168;
async function loadUsageMini() {
  try {
    const d = await api('usage?hours=' + loadRangeHours);
    const s = d.series || [];
    const host = $('loadChart'); if (!host) return;
    const note = $('loadNote');
    if (!s.length) { host.innerHTML = '<div class="us-empty">该范围暂无请求数据</div>'; if (note) note.textContent = '—'; return; }
    // 粒度自适应：>72h 或含折叠日桶 → 按天；否则小时（与用量页主图同判据）。
    const raw = [];
    for (const p of s) { const n = normUsagePoint(p); if (n) raw.push(n); }
    if (!raw.length) { host.innerHTML = '<div class="us-empty">该范围暂无请求数据</div>'; if (note) note.textContent = '—'; return; }
    const spanH = (raw[raw.length - 1].t - raw[0].t) / 3600e3;
    const daily = spanH > 72 || raw.some(p => p.scope === 'day');
    const pts = daily ? mergeDaily(s) : raw;

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
    // 网格 + 双轴刻度:左轴请求(ink),右轴积分(gold,与线同色系)
    for (let i = 0; i <= 4; i++) {
      const y = PT + ih - ih * i / 4;
      out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
        '" y2="' + y.toFixed(1) + '"/>';
      out += '<text class="lb" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
        '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
      out += '<text class="lb-r" x="' + (W - PR + 6) + '" y="' + (y + 3.5).toFixed(1) +
        '">' + fmtCredit(cMax * i / 4) + '</text>';
    }
    const yBase = PT + ih;
    pts.forEach((p, i) => {
      const x = xOf(p.t) - bw / 2;
      out += '<g class="usbarg" data-i="' + i + '">';
      const h = ih * (p.req / max);
      if (p.req > 0) {
        out += '<rect class="bar" x="' + x.toFixed(1) + '" y="' + (yBase - h).toFixed(1) +
          '" width="' + bw.toFixed(1) + '" height="' + Math.max(1, h).toFixed(1) + '"/>';
        if (p.err > 0) {
          const eh = Math.max(2, h * p.err / p.req);
          out += '<rect class="bar err" x="' + x.toFixed(1) + '" y="' + (yBase - eh).toFixed(1) +
            '" width="' + bw.toFixed(1) + '" height="' + eh.toFixed(1) + '"/>';
        }
      }
      out += '<rect class="usbar-hit" x="' + x.toFixed(1) + '" y="' + PT +
        '" width="' + bw.toFixed(1) + '" height="' + ih + '"/>';
      out += '</g>';
    });
    // 积分消耗折线(右轴)
    const line = pts.map(p => xOf(p.t).toFixed(1) + ',' + cyOf(p.credits).toFixed(1)).join(' ');
    out += '<polyline class="cred" points="' + line + '"/>';
    out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) + '" y2="' + yBase + '"/>';
    // x 轴刻度:4 个等距位置取最近实际点;小时粒度跨天时补日界线
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
      const p = pts[bi];
      const cx = xOf(p.t);
      const anchor = cx < PL + 16 ? 'start' : (cx > W - PR - 16 ? 'end' : 'middle');
      out += '<text class="lb" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
        '" y="' + (H - 8) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
    }
    if (!daily) {
      let prevDay = null;
      for (const p of pts) {
        const dd = new Date(p.t).getDate();
        if (prevDay !== null && dd !== prevDay) {
          out += '<line class="gl" x1="' + xOf(p.t).toFixed(1) + '" y1="' + PT + '" x2="' +
            xOf(p.t).toFixed(1) + '" y2="' + yBase + '" style="opacity:.45"/>';
        }
        prevDay = dd;
      }
    }
    out += '</svg>';
    host.innerHTML = out;
    usTipHide(); // 重渲染后旧 tooltip 已过期
    // 悬停富 tooltip(无点击跳转:这里的时间粒度与用量页范围选择是两套口径)
    bindChartTips(host, pts, { html: p => usLoadTipHTML(p, daily) });
    const t = d.totals || {};
    if (note) note.textContent = (t.requests || 0) + ' 请求 / ' + (t.errors || 0) + ' 失败' +
      (t.cache_hit_rate != null ? ' · 缓存命中 ' + t.cache_hit_rate.toFixed(1) + '%' : '');
    // 「近 7 天请求」是概览带的固定口径:只在 7 天范围时用它回写,其他范围不动
    const r7 = $('hsReq7d');
    if (r7 && loadRangeHours === 168) r7.textContent = t.requests || 0;
  } catch (e) { /* 增强项：失败不打断主流程 */ }
}

/* usLoadTipHTML 负载趋势的悬停明细:请求数(含失败) / 积分消耗 / 总 Tokens。 */
function usLoadTipHTML(p, daily) {
  const title = daily ? p.raw : (p.raw.length === 13 ? p.raw.replace('T', ' ') + ':00' : p.raw);
  return '<div class="t">' + esc(title) + '</div>' +
    usTipRow('请求数', String(p.req) + (p.err ? '（失败 ' + p.err + '）' : ''), 'sw-req') +
    usTipRow('积分消耗', fmtCredit(p.credits), 'sw-credit') +
    usTipRow('总 Tokens', fmtTok(p.tt), null);
}

/* 范围选择:切换即重拉(0 = 全部历史)。 */
if ($('loadRange')) $('loadRange').onchange = () => {
  loadRangeHours = Number($('loadRange').value) || 0;
  loadUsageMini();
};

/* ── E4 在途与最近请求泳道：每条泳道 = 一个账号，账号越少越清晰 ──────
   与「运行日志」页的边界：≤40 点、无翻页、无筛选、不显示 path/request_id 文本。 */
async function loadLive() {
  try {
    const d = await api('request_metrics');
    const st = $('liveStat');
    if (st) {
      // 三指标改成「数字大 + 标签小」的胶囊组；成功率按阈值染色（<60% 红、<85% 琥珀），
      // 差的数字一眼可见，不必去读小字。末尾追加静态图例，解释泳道里点形/点色的语义。
      const sr = d.success_rate;
      const srCls = sr == null ? '' : sr < 60 ? ' class="bad"' : sr < 85 ? ' class="warn"' : ' class="ok"';
      st.innerHTML = '<span class="ls"><b>' + (d.in_flight || 0) + '</b><i>在途</i></span>' +
        '<span class="ls"><b' + srCls + '>' + (sr != null ? sr + '%' : '—') + '</b><i>成功率</i></span>' +
        '<span class="ls"><b>' + (d.avg_duration_ms != null ? Math.round(d.avg_duration_ms) + 'ms' : '—') +
        '</b><i>平均耗时</i></span>' +
        '<span class="ls-lg"><i><b class="dot ok"></b>成功</i><i><b class="dot no"></b>失败</i>' +
        '<i><b class="dot retry"></b>有重试</i><i><b class="dot sz"></b>点越大 token 越多</i></span>';
    }
    const host = $('liveLanes'); if (!host) return;
    const note = $('liveNote');
    const recent = (d.recent || []).slice(0, 40);
    if (!recent.length) { host.innerHTML = '<div class="us-empty">暂无最近请求</div>'; if (note) note.textContent = '—'; return; }
    const names = [];
    recent.forEach(r => { const n = String(r.account || '').split('(')[0] || '未知';
      if (names.indexOf(n) < 0) names.push(n); });
    // 绘图区从 plotX 起：左边整段留给泳道带内的账号名，点不再和名字抢位置。
    const laneH = 30, W = 640, plotX = 110, padR = 10, H = names.length * laneH + 24;
    const t0 = new Date(recent[recent.length - 1].time).getTime();
    const t1 = new Date(recent[0].time).getTime();
    const span = Math.max(60000, t1 - t0);
    // SVG text 没有原生的省略号截断，长账号名在 JS 里截。
    const trim = n => (n.length > 10 ? n.slice(0, 10) + '…' : n);
    const hhmm = t => { const d = new Date(t);
      return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0'); };
    let out = '';
    names.forEach((n, li) => {
      const top = 4 + li * laneH;
      const y = top + laneH / 2;
      // 泳道背景带：名字和点圈进同一条带里，账号边界一眼可分（样式见 .lane-bg）。
      out += '<rect class="lane-bg" x="' + plotX + '" y="' + top + '" width="' +
        (W - plotX - padR) + '" height="' + (laneH - 4) + '" rx="6"/>';
      out += '<text class="lane-lb" x="8" y="' + (y + 3) + '">' + esc(trim(n)) + '</text>';
      recent.forEach(r => {
        const an = String(r.account || '').split('(')[0] || '未知';
        if (an !== n) return;
        const tt = new Date(r.time).getTime();
        const x = plotX + ((tt - t0) / span) * (W - plotX - padR);
        const tok = r.total_tokens || 0;
        const rad = Math.min(7, 2.5 + Math.sqrt(tok) / 45);
        const cy = y + ((li % 2) ? 5 : -5);
        out += '<circle class="pt ' + (r.ok === false ? 'no' : 'ok') + '" cx="' + x.toFixed(1) +
          '" cy="' + cy + '" r="' + rad.toFixed(1) + '"><title>' +
          esc(an + ' · ' + String(r.model || '') + ' · ' + Math.round(r.duration_ms || 0) + 'ms · ' + tok + ' tok' +
            ((r.attempts || 1) > 1 ? ' · 重试' + r.attempts + '次' : '')) + '</title></circle>';
        if ((r.attempts || 1) > 1)
          out += '<circle class="pt retry" cx="' + x.toFixed(1) + '" cy="' + cy +
            '" r="' + (rad + 2.6).toFixed(1) + '"/>';
      });
    });
    // 时间刻度只留首尾：左下角最早时刻，右端 now 竖虚线。不做逐小时网格——
    // 40 个点的分布读个大概就够，网格反而添噪。
    out += '<line class="now-line" x1="' + (W - padR) + '" y1="4" x2="' + (W - padR) + '" y2="' + (H - 16) + '"/>';
    out += '<text class="lane-lb" x="8" y="' + (H - 5) + '">' + hhmm(t0) + '</text>';
    out += '<text class="lane-lb" x="' + (W - padR) + '" y="' + (H - 5) + '" text-anchor="end">now</text>';
    host.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '">' + out + '</svg>';
    if (note) note.textContent = '最近 ' + recent.length + ' 条';
  } catch (e) { /* 增强项：失败不打断主流程 */ }
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
  /* 总额是次要值：包进 .of 缩小显示（与账号表同一指标的渲染口径一致）。
     不这样做的话主值与总额同号，整串约 208px，在 1280 宽下会被 ellipsis 截断。 */
  /* 不带空格：与账号表「4010/4461」的写法统一，且省下 32px 宽度让总额能全尺寸显示。 */
  if (totSum > 0) $('sCredits').innerHTML = esc(remSum) + '<span class="of">/' + esc(totSum) + '</span>';
  else $('sCredits').textContent = remSum;
    $('sSticky').textContent = d.sticky_sessions;
    // Hero 第 4 指标「最近到期」：全池最早到期批次的倒计时，按紧急度着色
    // （≤3 天红 / ≤7 天琥珀）。复用 attnEarliestExpiry 三级降级口径——与 E2
    // 预警同源，两处数字恒一致；纯本地计算，不打上游。
    {
      let best = null;
      for (const s of (d.accounts || [])) {
        const e = attnEarliestExpiry(s);
        if (e && (best === null || e.days < best.days)) best = e;
      }
      const val = $('hsExpiryVal'), kEl = $('hsExpiryK');
      if (val && kEl) {
        if (best) {
          val.textContent = best.days === 0 ? '今天' : best.days + ' 天';
          val.style.color = best.days <= 3 ? 'var(--bad)' : best.days <= 7 ? 'var(--warn)' : '';
          kEl.textContent = '最近到期 · 剩 ' + fmtTok(best.remain) + ' 积分';
        } else {
          val.textContent = '—';
          val.style.color = '';
          kEl.textContent = '最近到期';
        }
      }
    }
    // Hero 概览带：实时在途（池内在途请求合计）+ 可用率进度环 + 运行状态灯。
    // 全部由本函数已拿到的 overview 数据派生，不新增任何请求。
    const heroInflight = (d.accounts || []).reduce((a, s) => a + (s.in_flight || 0), 0);
    const hi = $('heroInflightVal');
    if (hi) hi.textContent = heroInflight;
    const hsInf = $('hsInflight');
    if (hsInf) hsInf.textContent = heroInflight;
    const hp = Math.round(d.total ? d.healthy / d.total * 100 : 0);
    const rp = $('hsAvail');
    if (rp) rp.textContent = d.total ? hp + '%' : '—';
    const heroPulse = $('heroPulse');
    if (heroPulse) heroPulse.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    const heroState = $('heroState');
    if (heroState) heroState.textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    // E2 需要关注（每次刷新都重算，纯本地派生）。
    renderAttn(d);
    // E3/E4：自节流刷新（首次必跑，之后 30 秒一次）。
    // 注意不能依赖 !quiet —— loadOverview 的多数调用（含初始加载）都传了 quiet=true。
    refreshViz();
    // navSub 现为固定品牌副标（"多账号补给港 · 控制台"），不再被版本号覆盖；
    // 版本号仅由侧栏底部 aboutVer 展示（顶栏版本·存储行已移除）。
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    // 侧栏底部"关于"块（元素可选，缺失时静默跳过）
    const aboutVer = $('aboutVer');
    if (aboutVer) aboutVer.textContent = 'v' + d.version;
    const aboutUptime = $('aboutUptime');
    if (aboutUptime) aboutUptime.textContent = dur(d.uptime_sec);
    const aboutAccounts = $('aboutAccounts');
    if (aboutAccounts) aboutAccounts.textContent = d.total;
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    renderUpdateBanner(d.update);
    // 到期卡片跟随首页统计同步刷新（用同一份响应里的到期快照，不额外发请求）。
    // 已有逐包明细缓存时不再覆盖——明细信息量更大，别把它降级成快照。
    if ($('expBox') && !lastPackages) renderExpirySnapshot(d);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

/* ── 更新提示 ─────────────────────────────────────────────────────── */
/* 只提示、不自动下载（后端 internal/update 的安全边界）。用户点 ✕ 后本次忽略，
 * 记住的是"该版本号"——下次有更高版本仍会再提示。 */
const LS_UPD_DISMISS = 'starport.updDismissed';
let updLatest = '';
function renderUpdateBanner(u) {
  const el = $('updBanner');
  if (!el) return;
  if (!u || !u.enabled || !u.available || !u.latest) { el.hidden = true; return; }
  if (lsGet(LS_UPD_DISMISS, 'wb2a.updDismissed') === u.latest) { el.hidden = true; return; }
  updLatest = u.latest;
  const ver = String(u.latest).replace(/^v/, '');
  $('updText').textContent = '有新版本 v' + ver + ' 可用（当前 ' + (u.current || '') + '）';
  const link = $('updLink');
  if (u.release_url) { link.href = u.release_url; link.hidden = false; }
  else { link.hidden = true; }
  el.hidden = false;
}
$('updClose').onclick = () => {
  if (updLatest) localStorage.setItem(LS_UPD_DISMISS, updLatest);
  $('updBanner').hidden = true;
};

/* ── 操作列的「⋯」浮层菜单 ─────────────────────────────────────────
   菜单内容来自行内隐藏的 .acts-menu 模板（克隆复用，避免重复拼 HTML）。
   浮层挂在 body 上并用 position:fixed 定位：sticky 列与 .tbl-wrap 的 overflow
   会裁掉子元素，而 .box 的 backdrop-filter 又会让 fixed 子元素失去视口定位。 */
let actsMenuEl = null;
function closeActsMenu() {
  if (actsMenuEl) actsMenuEl.hidden = true;
  document.querySelectorAll('#accBody button[data-a="more"]').forEach(b => b.setAttribute('aria-expanded', 'false'));
}
function openActsMenu(btn) {
  const tpl = btn.parentElement && btn.parentElement.querySelector('.acts-menu');
  if (!tpl) return;
  if (!actsMenuEl) {
    actsMenuEl = document.createElement('div');
    actsMenuEl.className = 'acts-menu';
    actsMenuEl.setAttribute('role', 'menu');
    document.body.appendChild(actsMenuEl);
  }
  actsMenuEl.innerHTML = tpl.innerHTML;
  actsMenuEl.hidden = false;
  const r = btn.getBoundingClientRect();
  const w = actsMenuEl.offsetWidth, h = actsMenuEl.offsetHeight;
  const left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8));
  let top = r.bottom + 6;
  if (top + h > innerHeight - 8) top = Math.max(8, r.top - h - 6);   // 下方放不下就上翻
  actsMenuEl.style.left = left + 'px';
  actsMenuEl.style.top = top + 'px';
  btn.setAttribute('aria-expanded', 'true');
}
addEventListener('scroll', closeActsMenu, true);   // 浮层不跟随滚动，滚了就关
addEventListener('resize', closeActsMenu);

document.addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  // 行内按钮与浮层菜单里的按钮走同一条链路；点到别处则收起菜单。
  if (!b || (!b.closest('#accBody') && !b.closest('.acts-menu'))) { closeActsMenu(); return; }
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'more') {                              // 「⋯」只负责开关菜单，不执行账号操作
    const wasOpen = b.getAttribute('aria-expanded') === 'true';
    closeActsMenu();
    if (!wasOpen) openActsMenu(b);
    return;
  }
  closeActsMenu();
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /startport/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   上游客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    $('mdNote').textContent = mdAll.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/startport/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  // 首字延迟汇总（v1.7.1）：均值取进程生命周期口径（reqlog Snapshot），
  // p50 取当前拉取窗口内的条目（1h/24h）——数据源就是已拉到的 ttfb_ms 字段，
  // 无样本（非流式为主/旧条目）时不显示，避免误导性的 0。
  let ttfbSummary = '';
  if (Number(m.ttfb_samples || 0) > 0) {
    ttfbSummary += ' · 首字 ' + fmtTTFB(m.avg_ttfb_ms);
  }
  const p50h = reqTTFBP50(entries, 60 * 60 * 1000);
  const p50d = reqTTFBP50(entries, 24 * 60 * 60 * 1000);
  if (p50d > 0) {
    ttfbSummary += ' · 首字p50 ' + fmtTTFB(p50h) + '（1h）/ ' + fmtTTFB(p50d) + '（24h）';
  }
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ttfbSummary +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + fmtTTFB(e && e.ttfb_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="11" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    e && Number(e.ttfb_ms || 0) > 0
      ? '首字 ' + fmtTTFB(e.ttfb_ms) + '（占总时长 '
        + Math.round(Number(e.ttfb_ms) / Math.max(1, Number(e.duration_ms || 0)) * 100) + '%）'
      : '',
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

/* 后端脱敏哨兵（与 panel.SecretSentinel 同值）：敏感字段（api_key）GET 时不回显
 * 真实值，以此占位。识别后清空输入框并改占位文案——留空 = 不下发 = 保留原值，
 * 与既有「空 = 不改」语义一致，绝不会把掩码当成新密钥写回。 */
const SECRET_SENTINEL = '__STARPORT_SECRET_SET__';

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else if (v === SECRET_SENTINEL) { el.value = ''; el.placeholder = '已设置（留空则不修改）'; }
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = lsGet(LS_KEY, 'wb2api.key');
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/startport/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
/* 顶部全局「添加账号」（v1.7.2 分流）：账号中心视图 = 平台添加向导
   （acct.js 的 acctGlobalAddAccount，默认当前页签的平台）；其它视图保持原
   CodeBuddy 登录弹窗。CodeBuddy 弹窗的唯一直达入口是账号中心 CodeBuddy 页签
   内的「添加 CodeBuddy 账号」按钮。 */
$('btnAdd').onclick = () => {
  if (view === 'accounts-center' && typeof acctGlobalAddAccount === 'function') {
    acctGlobalAddAccount();
    return;
  }
  openAdd();
};

$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts-center') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

/* fmtTTFB 首字延迟自适应格式（v1.7.1）：<1s 用毫秒（"850ms"），≥1s 用一位小数秒
   （"3.0s"——上游首字典型在秒级，一位小数已够分辨"慢在上游还是网关"）。
   缺失/零值 → "—"（非流式与错误路径没有该维度，不能显示误导性的 0ms）。 */
function fmtTTFB(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(1) + 's';
  return Math.round(ms) + 'ms';
}

/* reqTTFBP50 首字延迟 p50（纯函数）：只统计 sinceMs 窗口内（0 = 不限窗口）
   且 ttfb_ms>0 的条目；n 取最近邻秩 ceil(n/2)-1。返回 0 = 窗口内无样本。 */
function reqTTFBP50(entries, sinceMs) {
  const cutoff = sinceMs > 0 ? Date.now() - sinceMs : 0;
  const vals = [];
  for (const e of (entries || [])) {
    const v = Number(e && e.ttfb_ms || 0);
    if (!v) continue;
    if (cutoff && e && e.time) {
      const t = new Date(e.time).getTime();
      if (!Number.isFinite(t) || t < cutoff) continue;
    }
    vals.push(v);
  }
  if (!vals.length) return 0;
  vals.sort((a, b) => a - b);
  return vals[Math.ceil(vals.length / 2) - 1];
}
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="输入 ' + pp.toFixed(1) + '% · 输出 ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
function usRow(name, sub, a, mid, withPerf) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 10 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 7 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 7 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">输入</th><th class="num">输出</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x =>
    usRow(usDim === 'account' ? String(x.key || '').slice(0, 8) : x.key,
      usDim === 'account' ? (x.extra || '') : '',
      x,
      usDim === 'account' ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf)
  ).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(account) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // DeepSeek 式三张 hero 卡:消耗积分 / API 请求次数 / Tokens。原六张卡的信息
  // 压缩进副文与下方 usSubNote 一行(信息不丢,视觉聚焦在三个量级上)。
  const cacheTotal = Number(t.cache_hit_tokens || 0) + Number(t.cache_miss_tokens || 0);
  $('usStats').innerHTML =
    usKpi(fmtCredit(t.credits), '消耗积分', 'c-credit',
      t.credit_samples
        ? '有效样本 ' + fmtTok(t.credit_samples) + ' · ' + fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens)
        : '窗口内无积分样本') +
    usKpi(fmtTok(reqs), 'API 请求次数', '',
      errs ? '失败 ' + errs + ' · 成功率 ' + okRate.toFixed(1) + '%' : '全部成功') +
    usKpi(fmtTok(total), 'Tokens', '',
      '缓存命中率 ' + (cacheTotal > 0 ? trimFixed(Number(t.cache_hit_rate || 0).toFixed(1)) + '%' : '—'),
      '<div class="kbar"><i style="width:' + pctW(t.cache_hit_tokens) + ';background:var(--ok)"></i>' +
      '<i style="width:' + pctW(t.cache_miss_tokens) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--hi)"></i></div>');
  // 副指标行全中文:输入(含命中占比) / 输出 —— 与图表、明细表的口径逐字一致。
  $('usSubNote').textContent =
    '输入 ' + usPct(pt, total) + '（' + fmtTok(pt) + (pt > 0 && cacheTotal > 0 ? ' · 命中 ' + usPct(t.cache_hit_tokens, pt) : '') + '）' +
    ' · 输出 ' + usPct(ct, total) + '（' + fmtTok(ct) + '）' +
    ' · 平均延迟 ' + fmtMs(t.avg_latency_ms) +
    (t.avg_tokens_per_second ? ' · 吐字 ' + fmtRate(t.avg_tokens_per_second) : '');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  $('usMetricLabel').textContent = (US_METRICS[usMetric] || US_METRICS.credits).label;
  setUsLegend();
  renderUsageDim();
  renderCreditDim();
  renderUsageChart(usageData.series || []);
  renderUsageGroups();
  renderHealthChart(usageData.series || []);
  renderDayChip();
}

// renderHealthChart 「缓存命中率 + TTFB」双联趋势图（共享时间轴，上下两栏）。
//
// 为什么单独画而不并入 Token 时序：那张图是 prompt/completion 堆叠柱，量纲是
// token；命中率是百分比、TTFB 是毫秒，混在一起两套纵轴会读不出趋势。
//
// 这是排查"缓存是否生效"的主要窗口——命中率突然掉到 0 而账号没变，就是上游前缀
// 缓存被冲掉（同账号多会话共用缓存键的典型症状）。
function renderHealthChart(series) {
  const host = $('usHealth');
  if (!host) return;
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    pts.push({
      t, scope: p.scope, raw: p.t,
      hit: p.cache_hit_rate,                      // 0-100，缺省 undefined（无缓存观测）
      ttfb: Number(p.avg_ttfb_ms || 0),
      req: Number(p.requests || 0),
    });
  }
  const withHit = pts.filter(p => p.hit != null);
  const withTTFB = pts.filter(p => p.ttfb > 0);
  if (!pts.length || (!withHit.length && !withTTFB.length)) {
    host.innerHTML = '<div class="us-empty">暂无缓存 / 延迟观测。发起一次对话后再刷新。</div>';
    if ($('usHealthNote')) $('usHealthNote').textContent = '—';
    return;
  }

  const W = 1200, PL = 58, PR = 14, ROW = 62, GAP = 20, PB = 26;
  const H = ROW * 2 + GAP + PB;
  const iw = W - PL - PR;

  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const bw = Math.max(2, Math.min(26, iw * (minGap / span) * 0.7));
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);

  const maxTTFB = Math.max(1, ...withTTFB.map(p => p.ttfb));
  const rowTop = i => i * (ROW + GAP);
  const rowBottom = i => rowTop(i) + ROW;

  const bars = (list, yOf, base, cls, tipFn) => list.map(p =>
    '<rect x="' + (xOf(p.t) - bw / 2).toFixed(1) + '" y="' + yOf(p.v).toFixed(1) +
    '" width="' + bw.toFixed(1) + '" height="' + Math.max(1, base - yOf(p.v)).toFixed(1) +
    '" rx="1.5" class="' + cls + '"><title>' + esc(tipFn(p)) + '</title></rect>').join('');

  // 上栏：缓存命中率（0-100%，越高越好 → 用 ok 色）
  const hitPts = withHit.map(p => ({ t: p.t, v: Math.max(0, Math.min(100, p.hit)), src: p }));
  const yHit = v => rowTop(0) + ROW - ROW * (v / 100);
  // 下栏：TTFB（ms，越低越好 → 用 accent 色）
  const ttfbPts = withTTFB.map(p => ({ t: p.t, v: p.ttfb, src: p }));
  const yTTFB = v => rowTop(1) + ROW - ROW * (v / maxTTFB);

  const label = (x, y, txt, cls) => '<text x="' + x + '" y="' + y + '" class="usax ' + (cls || '') +
    '">' + esc(txt) + '</text>';
  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';
  out += '<defs><linearGradient id="hGradHit" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.55"/></linearGradient>' +
    '<linearGradient id="hGradTTFB" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.55"/></linearGradient></defs>';

  // 上栏
  out += '<line x1="' + PL + '" y1="' + (rowTop(0) + ROW) + '" x2="' + (W - PR) + '" y2="' +
    (rowTop(0) + ROW) + '" class="usgrid"/>';
  out += bars(hitPts, yHit, rowBottom(0), 'ushit', p => fmtTokTimeLabel(p.src) + '　命中率 ' + p.v.toFixed(1) + '%');
  out += label(PL - 8, rowTop(0) + 10, '命中率', 'l t');
  out += label(PL - 8, rowTop(0) + 24, '100%', 'l');
  out += label(PL - 8, rowTop(0) + ROW, '0%', 'l');

  // 下栏
  const y1 = rowTop(1);
  out += '<line x1="' + PL + '" y1="' + (y1 + ROW) + '" x2="' + (W - PR) + '" y2="' + (y1 + ROW) +
    '" class="usgrid"/>';
  out += bars(ttfbPts, yTTFB, rowBottom(1), 'usttfb', p => fmtTokTimeLabel(p.src) + '　TTFB ' + p.v.toFixed(0) + 'ms');
  out += label(PL - 8, y1 + 10, 'TTFB', 'l t');
  out += label(PL - 8, y1 + 24, maxTTFB.toFixed(0) + 'ms', 'l');
  out += label(PL - 8, y1 + ROW, '0', 'l');

  // 共享 x 轴刻度（首/中/末）
  const marks = pts.length <= 2 ? pts : [pts[0], pts[Math.floor((pts.length - 1) / 2)], pts[pts.length - 1]];
  for (const m of marks) {
    out += label(xOf(m.t), H - 8, fmtTokTimeLabel(m), 'c');
  }
  out += '</svg>';
  host.innerHTML = out;

  if ($('usHealthNote')) {
    const avgHit = withHit.length ? withHit.reduce((s, p) => s + p.hit, 0) / withHit.length : 0;
    const avgTTFB = withTTFB.length ? withTTFB.reduce((s, p) => s + p.ttfb, 0) / withTTFB.length : 0;
    const zeros = withHit.filter(p => p.hit < 1).length;
    $('usHealthNote').textContent =
      withHit.length + ' 个命中率点（均值 ' + avgHit.toFixed(1) + '%）' +
      (zeros ? ' · ' + zeros + ' 个点掉到 0%' : '') +
      (withTTFB.length ? ' · TTFB 均值 ' + avgTTFB.toFixed(0) + 'ms' : '');
  }
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};

/* 绘图骨架约定(新旧主图/小图共用):
 * x 轴是**真实时间轴**，不是按序号等距。数据里存在 1 小时的间隔，也存在
 * 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时画得和 1 小时
 * 一样宽，让「什么时候用的」完全失真。
 * 不用 preserveAspectRatio="none"（横向拉伸会让柱子文字变形），SVG 以
 * width:100% 渲染、按 viewBox 高宽比自适应高度；时间轴用本地时间解析
 * （后端返回的就是本地时区），day 点按当天 00:00 参与定位，与 hour 点在
 * 同一个连续轴上——日桶本来就是那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

/* ── 用量图表(DeepSeek 式改版)──────────────────────────────────────
 * 主图三指标可切换:消耗积分(usage.credit 累计)/ API 请求次数 / Tokens。
 * 粒度自适应:窗口跨度 >72h(或含折叠日桶)时把小时点归并成**按天**柱——
 * 对齐 DeepSeek「近 30 天看天」的读法;≤72h 保持小时粒度(近 3 天看小时)。
 * 悬停显示 HTML 富 tooltip(日期/积分/请求/命中缓存/未命中缓存/输出),
 * 点击柱子跳到该天用量;分组小节(按模型|按账号)复用同一套 tooltip。
 *
 * 旧版是 <title> 原生 tooltip + 单一 Token 堆叠指标(<title> 必须包进 <g>,
 * issue #128)。改版后 tooltip 由宿主 div 上的委托式 mousemove 驱动,柱子带
 * data-i 供寻址;柱类名继续用 usbar 而不是 bar(原因见 TestAppJSUsageChartBarClass)。
 */

const US_METRICS = {
  credits: { label: '消耗积分', axis: fmtCredit },
  // 请求次数取整:均值线等位置曾把浮点均值原样打出(61.15384615384615)。
  requests: { label: 'API 请求次数', axis: v => fmtTok(Math.round(v)) },
  tokens: { label: 'Tokens', axis: v => fmtTok(Math.round(v)) },
};
let usMetric = 'credits';   // 主图当前指标:credits | requests | tokens
let usGroupDim = 'model';   // 分组小节维度:model | account | key

/* usMiniGrad 小图渐变定义(按实例带 gid 后缀:同页多张小图的渐变 id 不能重复,
 * 否则 url(#id) 全部解析到第一张图,后画的小图渐变失效)。 */
function usMiniGrad(id, cssVar) {
  return '<linearGradient id="' + id + '" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(' + cssVar + ');stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(' + cssVar + ');stop-opacity:.75"/></linearGradient>';
}

/* normUsagePoint 把后端 series 点(小时桶或日桶)归一为绘图点;时间解析失败返回 null。 */
function normUsagePoint(p) {
  const t = parsePointTime(p);
  if (t === null) return null;
  const pt = Number(p.prompt_tokens || 0);
  const ct = Number(p.completion_tokens || 0);
  return {
    t, scope: p.scope, raw: String(p.t || ''),
    pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
    req: Number(p.requests || 0), err: Number(p.errors || 0),
    credits: Number(p.credits || 0), creditSamples: Number(p.credit_samples || 0),
    ch: Number(p.cache_hit_tokens || 0), cm: Number(p.cache_miss_tokens || 0),
  };
}

/* mergeDaily 把 hour 点与折叠 day 点按日历天归并(DeepSeek 式按天柱)。
 * 同一天的多个小时桶累加(token/请求/积分/缓存明细);输出按天升序。 */
function mergeDaily(series) {
  const byDay = new Map();
  const out = [];
  for (const p of series || []) {
    const key = String(p.t || '').slice(0, 10);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(key)) continue;
    let e = byDay.get(key);
    if (!e) {
      e = { t: parsePointTime({ t: key, scope: 'day' }), scope: 'day', raw: key,
            pt: 0, ct: 0, tt: 0, req: 0, err: 0,
            credits: 0, creditSamples: 0, ch: 0, cm: 0 };
      byDay.set(key, e);
      out.push(e);
    }
    const n = normUsagePoint(p);
    if (!n) continue;
    e.pt += n.pt; e.ct += n.ct; e.tt += n.tt;
    e.req += n.req; e.err += n.err;
    e.credits += n.credits; e.creditSamples += n.creditSamples;
    e.ch += n.ch; e.cm += n.cm;
  }
  out.sort((a, b) => a.t - b.t);
  return out;
}

/* ── 富 tooltip(usTip):HTML 卡片替代 SVG 原生 <title> ─────────────────
 * 原生 <title> 有约 1s 延迟、样式不可控、塞不下「命中/未命中/输出」多行明细。
 * usTip 是 body 级单例(index.html 静态节点),由图表宿主上的委托式 mousemove
 * 驱动;无 DOM 的测试桩环境(uspTip 依赖 document/window)静默跳过。 */
function usTipRow(label, value, swatch) {
  return '<div class="r">' + (swatch ? '<i class="sw ' + swatch + '"></i>' : '') +
    '<span>' + esc(label) + '</span><b>' + esc(value) + '</b></div>';
}

/* usMainTipHTML 单个时间点(或天)的明细,行序对齐 DeepSeek 悬停卡:
 * 积分 → 请求(含失败) → 输入(命中缓存)/输入(未命中缓存) → 输出 → 总计。
 * 缓存未观测的旧样本不伪造两行,合成一行「输入」。 */
function usMainTipHTML(p, daily) {
  const title = daily ? p.raw : (p.raw.length === 13 ? p.raw.replace('T', ' ') + ':00' : p.raw);
  let rows = usTipRow('消耗积分', fmtCredit(p.credits), 'sw-credit') +
    usTipRow('请求次数', String(p.req) + (p.err ? '（失败 ' + p.err + '）' : ''), null);
  if (p.ch + p.cm > 0) {
    rows += usTipRow('输入(命中缓存)', fmtTok(p.ch), 'sw-hit') +
      usTipRow('输入(未命中缓存)', fmtTok(p.cm), 'sw-miss');
  } else if (p.pt > 0) {
    rows += usTipRow('输入', fmtTok(p.pt), 'sw-miss');
  }
  rows += usTipRow('输出', fmtTok(p.ct), 'sw-out') +
    usTipRow('总 Tokens', fmtTok(p.tt), null);
  return '<div class="t">' + esc(title) + '</div>' + rows;
}

let usTipEl = null;
function usTipElOf() {
  if (usTipEl === null) usTipEl = $('usTip') || false;
  return usTipEl || null;
}
function usTipShow(html, ev) {
  const el = usTipElOf();
  if (!el || typeof document === 'undefined' || !document.body) return;
  el.innerHTML = html;
  el.hidden = false;
  usTipMove(ev);
}
function usTipMove(ev) {
  const el = usTipElOf();
  if (!el || el.hidden || typeof window === 'undefined' || !ev) return;
  const pad = 14;
  let x = ev.clientX + pad, y = ev.clientY + pad;
  const w = el.offsetWidth || 220, h = el.offsetHeight || 120;
  if (x + w > window.innerWidth - 8) x = ev.clientX - w - pad;
  if (y + h > window.innerHeight - 8) y = ev.clientY - h - pad;
  el.style.left = Math.max(8, x) + 'px';
  el.style.top = Math.max(8, y) + 'px';
}
function usTipHide() {
  const el = usTipElOf();
  if (!el) return;
  el.hidden = true;
}

/* bindChartTips 图表宿主 div 上的委托式悬停/点击。sel 是可悬停分组的选择器
 * (柱状图 = g.usbarg,折线的数据点 = g.usptg);innerHTML 重建柱子无需重绑。 */
function bindChartTips(host, pts, opt) {
  if (!host) return;
  const sel = opt.sel || 'g.usbarg';
  host.onmousemove = ev => {
    const g = ev.target.closest ? ev.target.closest(sel) : null;
    if (!g) { usTipHide(); return; }
    const p = pts[Number(g.dataset.i)];
    if (!p) { usTipHide(); return; }
    usTipShow(opt.html(p), ev);
  };
  host.onmouseleave = () => usTipHide();
  if (opt.click) {
    host.onclick = ev => {
      const g = ev.target.closest ? ev.target.closest(sel) : null;
      if (!g) return;
      const p = pts[Number(g.dataset.i)];
      if (p) opt.click(p);
    };
  }
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const raw = [];
  for (const p of series || []) {
    const n = normUsagePoint(p);
    if (n) raw.push(n);
  }
  if (!raw.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }
  // 粒度自适应:>72h 或含折叠日桶 → 按天;否则保持小时(近 3 天窗口看小时更有用)。
  const spanH = (raw[raw.length - 1].t - raw[0].t) / 3600e3;
  const daily = spanH > 72 || raw.some(p => p.scope === 'day');
  const pts = daily ? mergeDaily(series) : raw;

  const m = US_METRICS[usMetric] || US_METRICS.credits;
  const stacked = usMetric === 'tokens';
  const val = p => usMetric === 'credits' ? p.credits : usMetric === 'requests' ? p.req : p.tt;

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => val(p)));
  const peak = pts.reduce((a, b) => (val(b) > val(a) ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + val(p), 0) / pts.length;
  $('usChartNote').textContent =
    (daily ? pts.length + ' 天' : pts.length + ' 个时点') +
    (val(peak) > 0
      ? ' · 峰值 ' + m.axis(val(peak)) + ' @ ' + fmtTokTimeLabel(peak) + ' · 均值 ' + m.axis(avg)
      : ' · 暂无该指标数据');

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部极轻微降不透明度（.85）——曾用 .6，浅色主题下
  // 底部被背景洗成两截色，看起来像"配色坏了"（用户反馈）。现在只保留一点
  // 纵深提示，主体接近纯色（DeepSeek 柱状图即纯色块）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  // 三套语义色板:积分=紫、请求=青(与缓存三色 绿/蓝/金 互不撞色)。
  out += '<defs>' +
    usMiniGrad('usGradCredit', '--viz-credit') +
    usMiniGrad('usGradReq', '--viz-req') +
    usMiniGrad('usGradHit', '--ok') +
    usMiniGrad('usGradMiss', '--accent') +
    usMiniGrad('usGradOut', '--hi') +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + m.axis(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + m.axis(avg) + '</text>';
  }

  // 柱子:每根包一个 g.usbarg(data-i 供委托 tooltip/点击寻址),顶端铺一层
  // 透明命中层铺满整列高度——悬停/点击不必精确落在细柱上。
  // 圆角只给堆叠顶端(贴轴的底边保持方角,柱子才像"立"在基线上)。
  // 类名用 usbar 而不是 bar:账号池的积分条是 .bar{height:3px},而 SVG2 里
  // height 是 rect 的 CSS 几何属性,同名类会把每根柱子压成 3px 高(踩过)。
  const yBase = PT + ih;
  pts.forEach((p, i) => {
    const x = xOf(p.t) - bw / 2;
    out += '<g class="usbarg" data-i="' + i + '">';
    if (stacked) {
      // Tokens 三段堆叠:输入(命中缓存,绿)/输入(未命中缓存,蓝)/输出(金)——
      // 与悬停明细、图例完全同一套语义色(此前是 prompt/completion 两段英文图例,
      // 与 tooltip 的缓存口径对不上,用户无法把颜色和数字对应起来)。
      const hit = Math.max(0, Math.min(p.ch, p.pt));
      const miss = Math.max(0, p.pt - hit);
      const segs = [
        ['usGradHit', ih * (hit / max)],
        ['usGradMiss', ih * (miss / max)],
        ['usGradOut', ih * (p.ct / max)],
      ];
      let yCur = yBase;
      segs.forEach(([g, h], k) => {
        if (h < 0.4) return;
        const isTop = segs.slice(k + 1).every(([, hh]) => hh < 0.4);
        yCur -= h;
        out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + yCur.toFixed(2) +
          '" width="' + bw.toFixed(2) + '" height="' + h.toFixed(2) +
          '" fill="url(#' + g + ')"' + (isTop ? ' rx="1.5"' : '') + '/>';
      });
    } else if (val(p) > 0) {
      const h = Math.max(1, ih * (val(p) / max));
      const grad = usMetric === 'credits' ? 'usGradCredit' : 'usGradReq';
      out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - h).toFixed(2) +
        '" width="' + bw.toFixed(2) + '" height="' + h.toFixed(2) +
        '" fill="url(#' + grad + ')" rx="1.5"/>';
    }
    out += '<rect class="usbar-hit" x="' + x.toFixed(2) + '" y="' + PT +
      '" width="' + bw.toFixed(2) + '" height="' + ih + '"/>';
    out += '</g>';
  });

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  if (val(peak) > 0) {
    const px = xOf(peak.t);
    const py = yOf(val(peak));
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + m.axis(val(peak)) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见。
  // 按天柱模式跳过:每根柱子都是一天,分隔线会画在每根柱子之间,纯噪音。
  if (!daily) {
    let prevDay = null;
    for (const p of pts) {
      const d = new Date(p.t).getDate();
      if (prevDay !== null && d !== prevDay) {
        const x = xOf(p.t).toFixed(1);
        out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
               (PT + ih) + '" style="opacity:.45"/>';
      }
      prevDay = d;
    }
  }

  out += '</svg>';
  host.innerHTML = out;
  usTipHide(); // 重渲染后旧 tooltip 的内容/坐标已过期,先隐藏,悬停会立即重新触发
  // 悬停富 tooltip + 点击跳当天(委托在宿主 div 上,重建 SVG 不需要重绑)。
  bindChartTips(host, pts, {
    html: p => usMainTipHTML(p, daily),
    click: p => jumpToUsageDay(p.raw.slice(0, 10)),
  });
}

/* setUsLegend 主图图例随指标切换。 */
function setUsLegend() {
  const el = $('usLegend');
  if (!el) return;
  if (usMetric === 'tokens') {
    el.innerHTML = '<i class="sw sw-hit"></i>输入(命中缓存)<i class="sw sw-miss"></i>输入(未命中缓存)' +
      '<i class="sw sw-out"></i>输出<span class="sep"></span><i class="sw sw-t"></i>均值';
  } else if (usMetric === 'requests') {
    el.innerHTML = '<i class="sw sw-req"></i>请求次数(含失败)<span class="sep"></span>点击柱子查看当天';
  } else {
    el.innerHTML = '<i class="sw sw-credit"></i>积分消耗<span class="sep"></span>点击柱子查看当天';
  }
}

/* ── 分组小节(DeepSeek 式):每组 = 请求折线 + Token 三段堆叠柱 ───────── */

/* drawMiniLine 请求次数折线(area 形态,x 为天,DeepSeek 蓝色曲线形态)。
 * 每个数据点带透明命中圆(r=9)供悬停;单点退化为一个圆点(线段画不成)。 */
function drawMiniLine(host, pts, gid) {
  if (!host) return;
  const data = [];
  for (const p of pts || []) {
    const n = normUsagePoint(p);
    if (n) data.push(n);
  }
  if (!data.length) {
    host.innerHTML = '<div class="us-empty">无请求</div>';
    return;
  }
  const W = 560, H = 170, PL = 44, PR = 12, PT = 14, PB = 24;
  const iw = W - PL - PR, ih = H - PT - PB;
  const t0 = data[0].t, t1 = data[data.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...data.map(p => p.req));
  const xOf = t => PL + (t - t0) / span * iw;
  const yOf = v => PT + ih - ih * (v / max);
  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';
  out += '<defs>' + usMiniGrad('usGradLine' + gid, '--viz-req') + '</defs>';
  for (let i = 0; i <= 2; i++) {
    const y = PT + ih - ih * i / 2;
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
      '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 5) + '" y="' + (y + 3.5).toFixed(1) +
      '" text-anchor="end">' + fmtTok(max * i / 2) + '</text>';
  }
  if (data.length > 1) {
    let path = '';
    data.forEach((p, i) => {
      path += (i ? 'L' : 'M') + xOf(p.t).toFixed(1) + ' ' + yOf(p.req).toFixed(1);
    });
    out += '<path d="' + path + 'L' + (W - PR).toFixed(1) + ' ' + (PT + ih) +
      'L' + PL + ' ' + (PT + ih) + 'Z" fill="url(#usGradLine' + gid + ')" stroke="none"/>';
    out += '<path class="usline" d="' + path + '"/>';
  }
  data.forEach((p, i) => {
    out += '<g class="usptg" data-i="' + i + '">' +
      '<circle class="usbar-hit" cx="' + xOf(p.t).toFixed(1) + '" cy="' + yOf(p.req).toFixed(1) + '" r="9"/>' +
      '<circle class="usdot" cx="' + xOf(p.t).toFixed(1) + '" cy="' + yOf(p.req).toFixed(1) + '" r="2.6"/></g>';
  });
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) +
    '" y2="' + (PT + ih) + '"/>';
  out += '<text class="tk" x="' + PL + '" y="' + (H - 6) + '">' +
    esc(String(data[0].raw).slice(5)) + '</text>';
  if (data.length > 1) {
    out += '<text class="tk" x="' + (W - PR) + '" y="' + (H - 6) + '" text-anchor="end">' +
      esc(String(data[data.length - 1].raw).slice(5)) + '</text>';
  }
  out += '</svg>';
  host.innerHTML = out;
  usTipHide();
  bindChartTips(host, data, {
    sel: 'g.usptg',
    html: p => usMainTipHTML(p, true),
    click: p => jumpToUsageDay(p.raw.slice(0, 10)),
  });
}

/* drawMiniTokens Token 三段堆叠柱:输入(命中缓存)/输入(其余输入)/输出。
 * 命中取 cache_hit_tokens(钳到输入内);未观测缓存的样本把整段输入归入
 * "未命中"段——堆高恒等于 prompt+completion,不因缺观测而变矮。 */
function drawMiniTokens(host, pts, gid) {
  if (!host) return;
  const data = [];
  for (const p of pts || []) {
    const n = normUsagePoint(p);
    if (n) data.push(n);
  }
  if (!data.length) {
    host.innerHTML = '<div class="us-empty">无数据</div>';
    return;
  }
  const segs = p => {
    const hit = Math.max(0, Math.min(p.ch, p.pt));
    const rest = Math.max(0, p.pt - hit);
    return { hit, rest, out: p.ct, tt: p.tt || (hit + rest + p.ct) };
  };
  const W = 560, H = 170, PL = 44, PR = 12, PT = 14, PB = 24;
  const iw = W - PL - PR, ih = H - PT - PB;
  const t0 = data[0].t, t1 = data[data.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...data.map(p => segs(p).tt));
  let minGap = Infinity;
  for (let i = 1; i < data.length; i++) minGap = Math.min(minGap, data[i].t - data[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const bw = Math.max(3, Math.min(34, iw * (minGap / span) * 0.7));
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);
  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';
  out += '<defs>' +
    usMiniGrad('usGradHit' + gid, '--ok') +
    usMiniGrad('usGradMiss' + gid, '--accent') +
    usMiniGrad('usGradOut' + gid, '--hi') +
    '</defs>';
  for (let i = 0; i <= 2; i++) {
    const y = PT + ih - ih * i / 2;
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
      '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 5) + '" y="' + (y + 3.5).toFixed(1) +
      '" text-anchor="end">' + fmtTok(max * i / 2) + '</text>';
  }
  const yBase = PT + ih;
  data.forEach((p, i) => {
    const s = segs(p);
    const x = xOf(p.t) - bw / 2;
    out += '<g class="usbarg" data-i="' + i + '">';
    const parts = [
      ['usGradHit' + gid, s.hit],
      ['usGradMiss' + gid, s.rest],
      ['usGradOut' + gid, s.out],
    ];
    let yCur = yBase;
    parts.forEach(([g, v], k) => {
      const h = ih * (v / max);
      if (h < 0.4) return;
      // 圆角只给最顶段:上方没有可见段时它就是堆叠顶端。
      const isTop = parts.slice(k + 1).every(([, vv]) => ih * (vv / max) < 0.4);
      yCur -= h;
      out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + yCur.toFixed(2) +
        '" width="' + bw.toFixed(2) + '" height="' + h.toFixed(2) +
        '" fill="url(#' + g + ')"' + (isTop ? ' rx="1.5"' : '') + '/>';
    });
    out += '<rect class="usbar-hit" x="' + x.toFixed(2) + '" y="' + PT +
      '" width="' + bw.toFixed(2) + '" height="' + ih + '"/>';
    out += '</g>';
  });
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
    '" y2="' + yBase + '"/>';
  out += '<text class="tk" x="' + PL + '" y="' + (H - 6) + '">' +
    esc(String(data[0].raw).slice(5)) + '</text>';
  if (data.length > 1) {
    out += '<text class="tk" x="' + (W - PR) + '" y="' + (H - 6) + '" text-anchor="end">' +
      esc(String(data[data.length - 1].raw).slice(5)) + '</text>';
  }
  out += '</svg>';
  host.innerHTML = out;
  usTipHide();
  bindChartTips(host, data, {
    html: p => usMainTipHTML(p, true),
    click: p => jumpToUsageDay(p.raw.slice(0, 10)),
  });
}

/* renderUsageGroups 分组小节:按模型 | 按账号(数据来自后端 model_series /
 * account_series,窗口内按天)。组头汇总行 = 请求/消耗/Tokens 合计。 */
function renderUsageGroups() {
  const host = $('usGroups');
  if (!host) return;
  const d = usageData || {};
  const isModel = usGroupDim === 'model';
  const isKey = usGroupDim === 'key';
  const groups = isModel ? (d.model_series || []) : isKey ? (d.key_series || []) : (d.account_series || []);
  const tabsEl = $('usGroupTabs');
  if (tabsEl) tabsEl.innerHTML = usTabsHtml([['model', '按模型'], ['account', '按账号'], ['key', '按密钥']], usGroupDim, {
    model: (d.model_series || []).length,
    account: (d.account_series || []).length,
    key: (d.key_series || []).length,
  });
  const note = $('usGroupsNote');
  if (!groups.length) {
    host.innerHTML = '<div class="us-empty">窗口内暂无分组数据。发起一次对话后再刷新。</div>';
    if (note) note.textContent = '—';
    return;
  }
  if (note) note.textContent = groups.length + (isModel ? ' 个模型' : isKey ? ' 把密钥' : ' 个账号') + ' · 按天 · 点击小图跳到当天';
  host.innerHTML = groups.map((g, gi) => {
    const key = String(g.key || '');
    const title = isModel ? (key || '—')
      : isKey ? (key || '管理密钥')
      : (g.nickname || key.slice(0, 8) || '—');
    let sub = '';
    if (isKey) sub = key ? 'agent 密钥' : '管理密钥（api_key）';
    else if (!isModel) sub = [g.realm || '', key.slice(0, 8)].filter(Boolean).join(' · ');
    let req = 0, cre = 0, tt = 0;
    for (const p of (g.points || [])) {
      req += Number(p.requests || 0);
      cre += Number(p.credits || 0);
      tt += Number(p.total_tokens || 0);
    }
    // 按密钥维度:管理密钥组带历史口径说明;每组带「隐藏」按钮(仅收起显示,
    // 桶数据不动;已隐藏的组在底部条里可恢复)。
    const hideBtn = isKey
      ? '<button class="xs" data-akhide="' + esc(key) + '" title="仅收起这组显示,数据保留">隐藏</button>'
      : '';
    const mgmtHint = isKey && !key
      ? '<span class="hint" title="经管理密钥(api_key)鉴权的流量:含密钥功能上线前的全部历史用量,以及仍在用 api_key 的调用">含上线前历史</span>'
      : '';
    return '<div class="us-group">' +
      '<div class="us-group-hd"><span class="us-group-title">' + esc(title) + '</span>' +
      mgmtHint +
      (sub ? '<span class="note">' + esc(sub) + '</span>' : '') +
      '<span class="grow"></span>' +
      '<span class="us-group-sum">请求 ' + fmtTok(req) + ' · 消耗 ' + fmtCredit(cre) + 'c · Tokens ' + fmtTok(tt) + '</span>' +
      hideBtn + '</div>' +
      '<div class="us-group-grid">' +
      '<div class="us-mini"><div class="us-mini-hd"><span class="lb">API 请求次数</span><span class="grow"></span><span class="note">' + fmtTok(req) + '</span></div><div class="us-mini-body" id="usg-req-' + gi + '"></div></div>' +
      '<div class="us-mini"><div class="us-mini-hd"><span class="lb">Tokens</span><span class="grow"></span><span class="note">' + fmtTok(tt) + '</span></div><div class="us-mini-body" id="usg-tok-' + gi + '"></div></div>' +
      '</div></div>';
  }).join('');
  groups.forEach((g, gi) => {
    drawMiniLine($('usg-req-' + gi), g.points || [], 'l' + gi);
    drawMiniTokens($('usg-tok-' + gi), g.points || [], 't' + gi);
  });
  // 已隐藏的密钥分组(服务端已从 key_series 里滤掉,这里给出恢复入口)。
  const hiddenKeys = (isKey ? (d.hidden_keys || []) : []).filter(n => !groups.some(g => g.key === n));
  const strip = $('usHiddenStrip');
  if (strip) {
    if (isKey && hiddenKeys.length) {
      strip.hidden = false;
      strip.innerHTML = '已隐藏 ' + hiddenKeys.length + ' 组: ' + hiddenKeys.map(n =>
        '<span class="us-hidden-item">' + esc(n || '管理密钥') +
        '<button class="xs" data-akshow="' + esc(n) + '">显示</button></span>').join('');
    } else {
      strip.hidden = true;
      strip.innerHTML = '';
    }
  }
}

/* 隐藏/恢复密钥分组:POST 到 keys/hidden(纯显示偏好),完成后重拉用量。 */
async function akToggleGroupHidden(name, hidden) {
  try {
    await api('keys/hidden', { method: 'POST', body: JSON.stringify({ name: name, hidden: hidden }) });
    await loadUsage();
  } catch (e) { toast(e.message, 'err'); }
}

/* ── 当天下钻:点击柱子 → 该天 00:00→次日 00:00;chip 提供返回与查看请求记录 ── */
let usDrillPrev = null;
function jumpToUsageDay(dayKey) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(dayKey || '')) return;
  const st = trangeState('usRange');
  if (st.preset !== 'custom') usDrillPrev = { preset: st.preset, from: st.from, to: st.to };
  const p = dayKey.split('-').map(Number);
  const from = new Date(p[0], p[1] - 1, p[2], 0, 0, 0, 0);
  const to = new Date(from);
  to.setDate(to.getDate() + 1);
  to.setSeconds(to.getSeconds() - 1); // 单日 = 00:00:00 → 23:59:59(与日历口径一致)
  st.preset = 'custom';
  st.from = from;
  st.to = to;
  trangeRender('usRange');
  loadUsage();
}

/* usDayChipHTML 当前范围恰好是单日(某天 00:00 → 次日 00:00)时渲染下钻 chip;
 * 其余范围返回空串(chip 隐藏)——chip 是「当天视图」的标识,不是常驻控件。 */
function usDayChipHTML() {
  const st = trangeState('usRange');
  if (st.preset !== 'custom' || !st.from || !st.to) return '';
  const spanH = (st.to - st.from) / 3600e3;
  if (spanH < 23.9 || spanH > 24.1) return '';
  if (st.from.getHours() || st.from.getMinutes() || st.from.getSeconds()) return '';
  const p2 = n => String(n).padStart(2, '0');
  const key = st.from.getFullYear() + '-' + p2(st.from.getMonth() + 1) + '-' + p2(st.from.getDate());
  return '<span class="day">' + esc(key) + '</span>' +
    '<button type="button" data-act="back">返回</button>' +
    '<button type="button" data-act="logs">查看当天请求记录</button>';
}
function renderDayChip() {
  const el = $('usDayChip');
  if (!el) return;
  const html = usDayChipHTML();
  el.innerHTML = html;
  el.hidden = !html;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

/* loadUsageBalance 余额条:积分余额/总额度/即将到期 + 账号数,来自 overview
 * (与账号池同源,纯内存不打包上游)。overviewData 已有(账号页先加载)则零请求复用;
 * 失败隐藏余额条——它是增强项,不阻塞用量页。 */
async function loadUsageBalance() {
  const el = $('usBalance');
  if (!el) return;
  try {
    if (!overviewData) overviewData = await api('overview');
    const accs = overviewData.accounts || [];
    const rem = accs.reduce((a, s) => a + (Number(s.credits) || 0), 0);
    const tot = accs.reduce((a, s) => a + (Number(s.credits_total) || 0), 0);
    const exp = accs.reduce((a, s) => a + (Number(s.credits_expiring) || 0), 0);
    el.innerHTML =
      '<span><span class="bk">积分余额</span><span class="bv">' + fmtCredit(rem) + '</span>' +
      (tot > 0 ? '<span class="bv of"> / ' + fmtCredit(tot) + '</span>' : '') + '</span>' +
      (exp > 0 ? '<span><span class="bk">即将到期</span><span class="bv">' + fmtCredit(exp) + '</span></span>' : '') +
      '<span><span class="bk">账号</span><span class="bv">' + accs.length + '</span></span>';
    el.hidden = false;
  } catch (e) {
    el.hidden = true;
  }
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const [d] = await Promise.all([api('usage?' + q.toString()), loadUsageBalance()]);
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    usDrillPrev = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usSubNote').textContent = '—';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usGroupsNote').textContent = '—';
    $('usGroups').innerHTML = '';
    $('usDayChip').hidden = true;
    $('usBalance').hidden = true;
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="10" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
// 默认「近 30 天」(DeepSeek 同款):主图按天柱,与分组小节的按天序列同口径;
// 原默认近 3 天在旧版"小时柱"下才直观,改版后首屏信息密度太低。
if ($('usRange')) trangeBind('usRange', loadUsage, '720');
// 主图指标切换:纯重渲染不发请求(series 已在内存),三指标共用一次拉取的数据。
if ($('usMetricTabs')) $('usMetricTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-metric]');
  if (!b || !US_METRICS[b.dataset.metric]) return;
  usMetric = b.dataset.metric;
  $('usMetricTabs').querySelectorAll('button[data-metric]').forEach(x => x.classList.toggle('on', x === b));
  if (usageData) renderUsage(usageData);
});
// 分组小节维度切换:同上,纯重渲染。
if ($('usGroupTabs')) $('usGroupTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usGroupDim = ['model', 'account', 'key'].includes(b.dataset.dim) ? b.dataset.dim : 'model';
  renderUsageGroups();
});
if ($('usGroups')) $('usGroups').addEventListener('click', ev => {
  const hide = ev.target.closest('button[data-akhide]');
  if (hide) { akToggleGroupHidden(hide.dataset.akhide, true); return; }
  const show = ev.target.closest('button[data-akshow]');
  if (show) akToggleGroupHidden(show.dataset.akshow, false);
});
if ($('usHiddenStrip')) $('usHiddenStrip').addEventListener('click', ev => {
  const show = ev.target.closest('button[data-akshow]');
  if (show) akToggleGroupHidden(show.dataset.akshow, false);
});
// 当天下钻 chip:「返回」回到跳转前的范围(没记过则回近 30 天);
// 「查看当天请求记录」把同一区间带到运行日志视图(reqlog 归档默认保留 7 天,
// 更早的日期如实显示为空)。
if ($('usDayChip')) $('usDayChip').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-act]');
  if (!b) return;
  if (b.dataset.act === 'back') {
    const prev = usDrillPrev || { preset: '720', from: trangeMidnight(), to: new Date() };
    usDrillPrev = null;
    const st = trangeState('usRange');
    st.preset = prev.preset;
    st.from = prev.from;
    st.to = prev.to;
    trangeRender('usRange');
    loadUsage();
  } else if (b.dataset.act === 'logs') {
    const st = trangeState('usRange');
    const rst = trangeState('reqRange');
    rst.preset = 'custom';
    rst.from = st.from;
    rst.to = st.to;
    trangeRender('reqRange');
    go('logs');
  }
});

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// EXPIRY_TIERS 到期紧迫度固定档（天，从近到远）。这些档钉在图表最上方，让短周期
// 风险一眼可见——否则最小的一档可能是「14 天」，即将过期的那部分会被淹没。
// 每笔余额归入**第一个**满足 days <= tier.days 的档；都不满足则按精确天数单独出行。
const EXPIRY_TIERS = [
  { days: 1, label: '1 天内' },
  { days: 7, label: '7 天内' },
];

// pkSegOrder 分段排序：先按到期时刻（无到期时间的排最后），再按账号、来源，
// 保证同一档内各账号色块的顺序稳定。
function pkSegOrder(a, b) {
  return (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
    a.accountName.localeCompare(b.accountName) ||
    a.source.localeCompare(b.source);
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
//
// 额外固定 EXPIRY_TIERS 档：命中档位的分段不进普通分桶（同一笔不重复计数），
// 由 renderExpiryDistribution 钉在图表顶部，作为"临期风险"的常驻指标。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  const tiers = EXPIRY_TIERS.map(t => ({ days: t.days, label: t.label, credits: 0, segments: [] }));
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      const tier = tiers.find(t => segment.days <= t.days);
      if (tier) {
        tier.credits += segment.amount;
        tier.segments.push(segment);
        continue;
      }
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) row.segments.sort(pkSegOrder);
  for (const tier of tiers) tier.segments.sort(pkSegOrder);
  return { rows, tiers, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const segNode = (segment, total) => {
    const color = colors.get(segment.uid) || 'var(--accent)';
    const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
      pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
      segment.accountName;
    return '<span class="pk-expiry-seg" style="--seg-color:' + color +
      ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
      ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
      ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
  };
  const rowHtml = (label, credits, segments, cls) => {
    const total = credits || 1;
    return '<div class="pk-expiry-row' + (cls ? ' ' + cls : '') + '"><span>' + esc(label) +
      '</span><div class="pk-expiry-track">' + segments.map(s => segNode(s, total)).join('') +
      '</div><b>' + esc(fmtTok(credits)) + '</b></div>';
  };
  // 临期档位常驻：最紧的一档（1 天内）强提醒，其余次一级；=0 置灰显示 0
  // ——"当前无临期积分"同样是有用信息，不必逐行去扫。
  const tierHtml = summary.tiers.map((t, i) => rowHtml(t.label, t.credits, t.segments,
    t.credits > 0 ? (i === 0 ? 'is-urgent' : 'is-near') : 'is-empty')).join('');
  const rows = summary.rows
    .map(row => rowHtml(row.days === 0 ? '已到期' : row.days + ' 天', row.credits, row.segments))
    .join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  const chart = tierHtml + rows;
  $('pkExpiry').innerHTML = (chart
    ? hdr + '<div class="pk-expiry-chart">' + chart + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，排序规则由视图顶部的选择器决定（默认到期近的在前）
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（' +
      esc(PK_SORT_LABELS[pkSortMode] || '') + '）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'starport.pkSortMode';
let pkSortMode = lsGet(LS_PK_SORT, 'pkSortMode') || 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
if ($('pkSort')) {
  $('pkSort').value = pkSortMode;
  if ($('pkSort').value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  $('pkSort').onchange = () => {
    pkSortMode = $('pkSort').value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供首页到期卡片与排序切换复用
    lastPackagesAt = Date.now();
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

function renderExpiry(d) {
  const list = (d.accounts || []);
  const today = new Date(); today.setHours(0, 0, 0, 0);
  // 按「最近到期」升序排（issue #125）。
  //
  // 后端 /startport/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
  // global 排在末尾，看起来像"按域分组"，其实只是余额顺序。这里再排一次，让
  // "最快过期的排最前"这个唯一重要的顺序成立，且不分域。
  //
  // 排序键用最早到期批次的日期（expBatches 已按日期升序，故 [0] 即最早）。
  // 查询失败的账号、以及 7 天内无到期的账号没有可比较的到期时间，统一排在最后，
  // 保持它们原本的相对顺序（稳定排序）。
  const keyed = list.map(a => {
    let key = null;
    if (!a.error) {
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) key = bs[0].date;
    }
    return { a, key };
  });
  keyed.sort((x, y) => {
    if (x.key === y.key) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key < y.key ? -1 : 1;
  });
  const rows = keyed.map(({ a }) => {
    if (a.error) {
      return '<div class="exp-row"><div class="exp-hd"><span class="exp-dot" style="background:var(--ink-3)"></span>' +
        '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span></div>' +
        '<div class="exp-days" style="color:var(--bad)">查询失败</div>' +
        '<div class="exp-main err">' + esc(a.error) + '</div></div>';
    }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    if (!bs.length) {
      return '<div class="exp-row"><div class="exp-hd"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span></div>' +
        '<div class="exp-days" style="color:var(--ink-3)">7 天内无到期</div>' +
        '<div class="exp-main">积分批次健康，暂无临期消耗压力</div></div>';
    }
    const first = bs[0];
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7)
      .reduce((s, b) => s + b.remain, 0);
    // 危险度：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿。
    // 上游扣包是 FEFO（按失效时刻升序，实测两号口径一致）：这些快过期批次正是
    // 被消耗得最快的，日均需耗给的是「哪怕单靠这个账号的自然流量也能对齐」的参照。
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    return '<div class="exp-row"><div class="exp-hd"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span></div>' +
      '<div class="exp-days" style="color:' + cls + '">' + dayWord + '</div>' +
      '<div class="exp-main">最近到期 <b>' + esc(first.date) + '</b> · 该批 <b>' + fmtTok(first.remain) +
      '</b> 积分 · 到期前日均需耗 ≥<b>' + fmtTok(daily) + '</b>' +
      (week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : '') +
      (rest ? '<div class="note">' + rest + '</div>' : '') +
      '</div></div>';
  }).join('');
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  $('expNote').textContent = (lastPackagesAt && ageMin > 0)
    ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，可点「检查」刷新'
    : list.length + ' 个账号 · 实时查询上游';
  $('expBox').hidden = false;
}

// attnEarliestExpiry 为 E2「需要关注」取某账号最早到期批次的 {days, remain}，
// 没有可用数据时返回 null（不产生预警条目）。入参 s 是 overview 的账号条目。
//
// 三级降级：
// ① lastPackages 共享缓存里有该账号的实时包明细 → 与 renderExpiry 完全同口径
//    （同一 expBatches/expDaysLeft，过滤已到期批次后取最早一批）；
// ② 退回 overview 汇总字段 credits_earliest_expiry，按「日历日差」折算天数
//    （先把时刻归到本地当日 00:00 再取差，与 expDaysLeft 同口径），剩余量取
//    credits_earliest_remaining；
// ③ 都没有 → null。
//
// 两处易错点：第②级必须过滤零值——Go 的 omitempty 对非指针 time.Time 无效，
// 无数据时序列化为 "0001-01-01T00:00:00Z"，new Date() 能解析出有效但荒谬的
// 远古时间，getFullYear() < 2000 即视为无数据；天数不能直接拿
// credits_earliest_remaining（那是剩余积分量）来比，renderAttn 曾因此全错。
function attnEarliestExpiry(s) {
  if (lastPackages && Array.isArray(lastPackages.accounts)) {
    const a = lastPackages.accounts.find(x => x && x.uid === s.uid && !x.error);
    if (a) {
      const today = new Date(); today.setHours(0, 0, 0, 0);
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) return { days: expDaysLeft(bs[0].date, today), remain: bs[0].remain };
    }
  }
  const t = s.credits_earliest_expiry ? new Date(s.credits_earliest_expiry) : null;
  if (!t || isNaN(t.getTime()) || t.getFullYear() < 2000) return null;
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const day = new Date(t); day.setHours(0, 0, 0, 0);
  return { days: Math.round((day - today) / 86400000), remain: s.credits_earliest_remaining };
}

/* 快照版到期提醒：不查上游，直接用 /overview 已有的 credits_earliest_expiry /
   credits_earliest_remaining（由签到与余额后台刷新写入 state.json，默认每 5 分钟
   更新一次）。打开面板即可看到「最近哪批要过期」，成本与首页统计条相同为零。

   与 renderExpiry（逐包明细版）的差异：这里只有**最早一批**的日期与数量，没有
   后续批次、也没法算「7 天内合计」——那是 packages 接口才有的信息。因此卡片上
   保留「检查」按钮作为增强入口，用户想要完整明细再显式触发一次上游查询。 */
function renderExpirySnapshot(d) {
  const list = (d && d.accounts) || [];
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const keyed = list.map(a => {
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    let key = null, ms = 0;
    if (a.credits_earliest_expiry) {
      const t = new Date(a.credits_earliest_expiry);
      if (!isNaN(t.getTime())) { ms = t.getTime(); key = ms; }
    }
    return { a, key, ms };
  });
  keyed.sort((x, y) => {
    if (x.key === null && y.key === null) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key - y.key;
  });
  const rows = keyed.map(({ a, ms }) => {
    const nm = esc(a.nickname || String(a.uid || '').slice(0, 8));
    if (!ms || !a.credits_earliest_remaining) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + nm + '</span>' +
        '<span class="exp-main">无近期到期批次</span></div>';
    }
    const days = Math.max(0, Math.ceil((ms - today.getTime()) / 86400000));
    const remain = Number(a.credits_earliest_remaining || 0);
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const when = new Date(ms).toISOString().slice(0, 10);
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + nm + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      '<div class="note">仅最早一批，点「检查」看全部到期批次</div>' +
      '</span></div>';
  }).join('');
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  $('expNote').textContent = list.length + ' 个账号 · 快照（来自本地状态，点「检查」拉实时明细）';
  $('expBox').hidden = false;
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  $('expBox').hidden = false;
  if (!$('expList').children.length) $('expList').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('expNote').textContent = '查询中…';
  try {
    const d = await api('packages');
    lastPackages = d;                            // 与「积分构成」视图共用同一份缓存
    lastPackagesAt = Date.now();
    renderExpiry(d);
  } catch (e) {
    $('expNote').textContent = '查询失败：' + esc(e.message);
  }
  expFetching = false;
}

if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);

// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);
