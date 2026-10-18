'use strict';
/* ── ext.js：外部渠道（多平台）视图 ──────────────────────────────────
   自包含模块：只依赖 core.js 的 $ / esc / ago / dur / api / toast。
   职责：
     - 渠道卡片流（协议徽标 / 派生状态 / 凭证 x/y / 模型数 / 最近成功 / 最近错误
       + 每凭证的 label/状态/尝试数（上游往返，含失败）/tokens/过期/最近错误）；
     - 测试连通、编辑、删除（删除/保存都走 extwiz.js 的 extMutateChannels，
       写路径只有 POST /startport/api/config）；
     - 额度情报表格 + 「生成草稿」（草稿由 extwiz.js 打开向导并预填）。
   本文件**绝不渲染 secret**：状态快照里根本没有该字段（CredSnapshot.Masked=true）。
   loadExt() 由 app.js 的路由 go() 调度。 */

let extChannels = [];
let extSummary = null;
let extIntelData = null;
let extIntelFilter = 'all';

/* 渠道派生状态 → [文案, 徽标配色]。 */
const EC_STATES = {
  available: ['可用', 'ok'],
  cooling: ['冷却中', 'warn'],
  disabled: ['已停用', 'mute'],
  all_invalid: ['全部凭证失效', 'bad'],
  all_cooling: ['全部凭证冷却', 'warn'],
  unavailable: ['不可用', 'bad']
};

/* 凭证状态 → [文案, 配色]。 */
const EC_CRED_STATES = {
  healthy: ['健康', 'ok'],
  cooling: ['冷却', 'warn'],
  invalid: ['失效', 'bad'],
  disabled: ['停用', 'mute']
};

async function loadExt() {
  try {
    const d = await api('ext_channels');
    extChannels = d.channels || [];
    extSummary = d.summary || null;
    renderExtChannels();
  } catch (e) {
    const box = $('ecCards');
    if (box) box.innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
  }
  try {
    const d = await api('ext_intel');
    extIntelData = d;
    renderExtIntel();
  } catch (e) {
    const body = $('eiBody');
    if (body) body.innerHTML = '<tr><td colspan="9" class="empty">情报读取失败：' + esc(e.message) + '</td></tr>';
  }
}

/* ── 渠道卡片 ─────────────────────────────────────────────────────── */

function renderExtChannels() {
  const note = $('ecNote'), summary = $('ecSummary'), box = $('ecCards');
  if (!note || !box) return;
  const active = extChannels.filter(c => !c.disabled);
  note.textContent = extChannels.length
    ? active.length + ' / ' + extChannels.length + ' 个渠道启用中'
    : '';
  if (summary) {
    summary.textContent = extSummary
      ? extSummary.channels + ' 条渠道 · ' + extSummary.credentials + ' 个凭证 · '
        + extSummary.usable + ' 可用 · ' + extSummary.abnormal + ' 异常'
      : '';
  }
  if (!extChannels.length) {
    box.innerHTML = '<div class="empty"><div class="big">还没有外部渠道</div>'
      + '点右上角「新建渠道」添加一个上游（协议 + base_url + 凭证），保存后即可用 '
      + '<code>ext:渠道名/模型名</code> 调用；也可以从下方「额度情报」里生成草稿。</div>';
    return;
  }
  box.innerHTML = extChannels.map(ch => renderChannelCard(ch)).join('');
}

function renderChannelCard(c) {
  const st = EC_STATES[c.state] || [c.state || '未知', 'mute'];
  const modelCount = (c.models && c.models.length) || (c.auto_models && c.auto_models.length) || 0;
  const aliasCount = c.aliases ? Object.keys(c.aliases).length : 0;
  const lastOK = c.last_ok && !String(c.last_ok).startsWith('0001-') ? esc(ago(c.last_ok)) : '—';
  const err = c.last_error
    ? '<div class="note" style="color:var(--bad)">最近错误：' + esc(c.last_error) + '</div>' : '';
  const cool = c.cooling ? ' <span class="note">冷却 ' + dur(c.cool_remaining_sec) + '</span>' : '';
  const noteTxt = c.note ? '<div class="note">' + esc(c.note) + '</div>' : '';
  const credRows = (c.credentials || []).map(cred => renderCredRow(c.name, cred)).join('');
  let credTable;
  if (credRows) {
    credTable = '<table class="acc zebra" style="margin-top:10px">'
      + '<thead><tr><th>凭证</th><th>状态</th><th class="num" title="上游尝试次数（含被 429/401 淘汰的失败尝试）">尝试</th><th class="num">tokens</th>'
      + '<th>过期</th><th>最近错误</th><th class="c-acts"></th></tr></thead>'
      + '<tbody>' + credRows + '</tbody></table>';
  } else {
    credTable = '<div class="note" style="margin-top:8px">未配置凭证（不注入鉴权；自建内网聚合器场景）</div>';
  }
  return '<div style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin:0 0 14px">'
    + '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">'
    + '<b>' + esc(c.name) + '</b>'
    + '<span class="tag mute">' + esc(c.protocol || 'openai') + '</span>'
    // 原生适配器徽标：只在显式配置时出现（空 = 按协议，不缺省刷一个标签）。
    + (c.adapter ? ' <span class="tag" title="原生适配器">' + esc(c.adapter) + '</span>' : '')
    + (c.managed ? '<span class="tag mute" title="credentials 由账号中心投影，在此修改会被拒绝">账号中心托管</span>' : '')
    + '<span class="tag ' + st[1] + '">' + esc(st[0]) + '</span>' + cool
    + '<span class="grow" style="flex:1"></span>'
    + (!c.managed
      ? '<button type="button" class="xs" data-ec="migrate" data-name="' + esc(c.name) + '" title="把静态凭证转为账号条目挂入账号中心对应平台页签">迁入账号中心</button>'
      : '<button type="button" class="xs" data-ec="rollback" data-name="' + esc(c.name) + '" title="清除托管标记：账号从账号中心删除，凭证原样保留在渠道（可重新迁入）">回退</button>')
    + '<button type="button" class="xs" data-ec="probe" data-name="' + esc(c.name) + '">测试</button>'
    + '<button type="button" class="xs" data-ec="edit" data-name="' + esc(c.name) + '">编辑</button>'
    + '<button type="button" class="xs" data-ec="del" data-name="' + esc(c.name) + '">删除</button>'
    + '</div>'
    + '<div class="note" style="margin-top:6px">'
    + '<code>' + esc(c.base_url) + '</code>'
    + ' · 凭证 ' + (c.credentials_usable || 0) + '/' + (c.credentials_total || 0) + ' 可用'
    + (c.credentials_invalid ? ' · ' + c.credentials_invalid + ' 失效' : '')
    + (c.credentials_expiring_soon ? ' · ' + c.credentials_expiring_soon + ' 即将过期' : '')
    + ' · 模型 ' + modelCount + (aliasCount ? '（含 ' + aliasCount + ' 个别名）' : '')
    + ' · 最近成功 ' + lastOK
    + ' · 在途 ' + (c.in_flight || 0)
    + '</div>' + err + noteTxt + credTable
    + '</div>';
}

function renderCredRow(channelName, cred) {
  const st = EC_CRED_STATES[cred.state] || [cred.state || '未知', 'mute'];
  const expiry = cred.has_expiry
    ? (esc(cred.expires_at) + (cred.expiring_soon ? ' <span class="tag warn">即将过期</span>' : ''))
    : '—';
  const err = cred.last_error ? esc(cred.last_error) : '—';
  // 用量口径必须写明：进程内累计，重启清零（P0-9 降级方案的已知边界）。
  const tokens = (cred.total_tokens || 0).toLocaleString() + '<div class="note">本进程累计</div>';
  return '<tr>'
    + '<td><b>' + esc(cred.label) + '</b>'
    + (cred.auth_mode ? '<div class="note">' + esc(cred.auth_mode) + '</div>' : '') + '</td>'
    + '<td><span class="tag ' + st[1] + '">' + esc(st[0]) + '</span>'
    + (cred.cooling ? '<div class="note">' + dur(cred.cool_remaining_sec) + '</div>' : '')
    + (cred.disabled ? '<div class="note">配置停用</div>' : '') + '</td>'
    + '<td class="num">' + (cred.requests || 0) + '</td>'
    + '<td class="num">' + tokens + '</td>'
    + '<td>' + expiry + '</td>'
    + '<td>' + err + '</td>'
    + '<td><button type="button" class="xs" data-ec="probe" data-name="' + esc(channelName)
    + '" data-cred="' + esc(cred.label) + '">测试</button></td>'
    + '</tr>';
}

/* ── 卡片按钮（事件委托：卡片是 innerHTML 渲染，逐个绑定会丢） ────── */

if ($('ecCards')) {
  $('ecCards').onclick = async e => {
    const b = e.target.closest('button[data-ec]');
    if (!b) return;
    const name = b.dataset.name, cred = b.dataset.cred || '';
    if (b.dataset.ec === 'probe') {
      await probeExtChannel(name, cred, b);
    } else if (b.dataset.ec === 'edit') {
      const ch = await extFindChannelConfig(name);
      if (ch) openExtWizard(ch, null);
      else toast('读取渠道配置失败', 'bad');
    } else if (b.dataset.ec === 'del') {
      await deleteExtChannel(name);
    } else if (b.dataset.ec === 'migrate') {
      await migrateExtChannel(name, b);
    } else if (b.dataset.ec === 'rollback') {
      await rollbackExtChannel(name, b);
    }
  };
}

if ($('btnEcNew')) {
  $('btnEcNew').onclick = () => openExtWizard(null, null);
}

/* probeExtChannel 只读探测：成功显示延迟 + 模型数，失败显示脱敏摘要。 */
async function probeExtChannel(name, credentialLabel, btn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '测试中…'; }
  try {
    const body = { name: name };
    if (credentialLabel) body.credential_label = credentialLabel;
    const d = await api('ext_channels/probe', { method: 'POST', body: JSON.stringify(body) });
    const r = d.result || {};
    if (r.ok) {
      toast(name + (credentialLabel ? '/' + credentialLabel : '') + ' 连通正常：'
        + (r.latency_ms || 0) + 'ms · ' + (r.models_count || 0) + ' 个模型', 'ok');
    } else {
      toast('探测失败：' + (r.error || ('HTTP ' + (r.status || '?'))), 'bad');
    }
  } catch (e) {
    toast('探测失败：' + e.message, 'bad');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* 迁入/回退（账号中心 v1.6.0 · T5，契约冻结）。
   migrate：200 {ok,migrated[],total}；400 两态（部分失败 {error,failed[]} /
   D1 平台冲突 {error}——均为预期业务拒绝，error 原文已含指路，不重试）；
   409 已托管；404 渠道不存在；501 账号中心未启用。
   rollback：200 {ok,removed_accounts,message}（账号从账号中心删除、凭证原样
   保留在渠道）；409 非托管；404/501 同上。
   二次确认语义由前端做（架构 §7.3）。 */
function extEndpointUnavailable(m) {
  return m.indexOf('404') >= 0 || m.indexOf('501') >= 0
    || m.indexOf('账号中心未启用') >= 0 || m.indexOf('渠道不存在') >= 0;
}

async function migrateExtChannel(name, btn) {
  if (!confirm('把渠道 ' + name + ' 的每条凭证迁入账号中心？\n迁入后凭证由账号中心托管（可一键回退）。')) return;
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '迁入中…'; }
  try {
    const d = await api('channels/' + encodeURIComponent(name) + '/migrate', { method: 'POST' });
    const n = d.total || (d.migrated ? d.migrated.length : 0);
    toast('已迁入账号中心：' + n + ' 条凭证转为账号，托管渠道已生效', 'ok');
    // custom 渠道的已知形态：账号中心按"每账号一条"投影出独立的 c-<id> 托管
    // 渠道，原渠道会短暂并存（非托管同名不接管的孤儿语义）——提示用户可删除。
    const ch = (typeof extChannels !== 'undefined' ? extChannels : []).find(x => x && x.name === name);
    if (ch && (ch.protocol || 'openai') === 'openai' && (ch.adapter || '') === ''
      && name.indexOf('c-') !== 0 && /^custom|^ch-/.test(ch.name) === false) {
      // 仅在无法确认平台归属时保守提示；原生平台原地转托管，无此现象。
    }
    loadExt();
  } catch (e) {
    const m = e.message || '';
    if (m.indexOf('无需迁入') >= 0) {
      toast(m, 'mute');
      loadExt();
    } else if (extEndpointUnavailable(m)) {
      toast('迁入功能不可用：' + m, 'mute');
    } else {
      // 400（部分失败 / D1 平台冲突）：error 原文已含字段级原因与指路，直接展示。
      toast(m, 'bad');
    }
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* rollbackExtChannel 回退：托管三键清除、账号从账号中心删除、凭证零数据损失。 */
async function rollbackExtChannel(name, btn) {
  if (!confirm('回退渠道 ' + name + '？\n· 对应账号将从账号中心删除\n· 凭证原样保留在渠道里（零数据损失）\n· 之后可重新迁入')) return;
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '回退中…'; }
  try {
    const d = await api('channels/' + encodeURIComponent(name) + '/rollback', { method: 'POST' });
    toast(d.message || ('已回退为普通渠道（' + (d.removed_accounts || 0) + ' 个账号已从账号中心移除）'), 'ok');
    loadExt();
  } catch (e) {
    const m = e.message || '';
    if (m.indexOf('无需回退') >= 0) {
      toast(m, 'mute');
      loadExt();
    } else if (extEndpointUnavailable(m)) {
      toast('回退功能不可用：' + m, 'mute');
    } else {
      toast(m, 'bad');
    }
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* deleteExtChannel 删除渠道：整表回写（未动的渠道原样带回，含哨兵 secret）。 */
async function deleteExtChannel(name) {
  if (!confirm('删除渠道 ' + name + '？该渠道的所有凭证与运行态会一并丢弃。')) return;
  try {
    await extMutateChannels(list => list.filter(c => (c && c.name) !== name));
    toast('已删除渠道 ' + name, 'ok');
    loadExt();
  } catch (e) {
    toast('删除失败：' + e.message, 'bad');
  }
}

/* ── 额度情报 ─────────────────────────────────────────────────────── */

function renderExtIntel() {
  const note = $('eiNote'), body = $('eiBody'), meta = $('eiMeta');
  if (!body) return;
  const items = (extIntelData && extIntelData.items) || [];
  const st = (extIntelData && extIntelData.status) || {};
  if (meta) {
    meta.textContent = items.length
      ? '共 ' + items.length + ' 条 · 拉取于 ' + ago(extIntelData.fetched_at)
        + (extIntelData.source_updated_at ? ' · 数据源更新 ' + esc(extIntelData.source_updated_at) : '')
      : '';
  }
  if (!st.enabled) {
    if (note) note.textContent = '';
    body.innerHTML = '<tr><td colspan="9" class="empty">额度情报未启用（config.ext_intel.enabled=false 或未配置 url）</td></tr>';
    return;
  }
  if (!items.length) {
    if (note) note.textContent = '';
    const err = extIntelData && extIntelData.last_error;
    body.innerHTML = '<tr><td colspan="9" class="empty">'
      + (err ? '拉取失败：' + esc(err) : '暂无数据，等待下一轮拉取') + '</td></tr>';
    return;
  }
  // 分类过滤：all / model（大模型 API）/ tool（工具额度）。
  const shown = items.filter(it => {
    if (extIntelFilter === 'all') return true;
    if (extIntelFilter === 'model') return it.category === 'model' || it.category === 'event';
    return it.category === 'tool';
  });
  // 免费类型徽标配色：限时/注册=warn，长期/免费层=ok，每日=accent。
  const ftColor = ft => {
    if (!ft) return 'var(--ink-3)';
    if (ft.includes('限时') || ft.includes('注册')) return 'var(--warn)';
    if (ft.includes('长期') || ft.includes('免费层')) return 'var(--ok)';
    return 'var(--accent)';
  };
  body.innerHTML = shown.map(it => {
    const idx = items.indexOf(it);
    const ft = it.free_type ? '<span class="tag" style="color:' + ftColor(it.free_type) + '">' + esc(it.free_type) + '</span>' : '';
    const entry = it.entry_url
      ? '<a href="' + esc(it.entry_url) + '" target="_blank" rel="noopener noreferrer">领取 →</a>' : '';
    const detail = it.detail_url
      ? ' <a href="' + esc(it.detail_url) + '" target="_blank" rel="noopener noreferrer">详情</a>' : '';
    // 草稿按钮：native_required 置灰（平台凭证需原生适配，静态 key 接不了）。
    let draftBtn;
    if (it.native_required) {
      draftBtn = '<button type="button" class="xs" disabled title="该平台凭证需要原生适配（签名/指纹/有状态），本期不能直接接入">需原生适配</button>';
    } else if (it.configured) {
      draftBtn = '<span class="tag ok">已接入</span>';
    } else {
      draftBtn = '<button type="button" class="xs" data-draft="' + idx + '">生成草稿</button>';
    }
    return '<tr>'
      + '<td><b>' + esc(it.name) + '</b>'
      + (it.vendor ? '<div class="note">' + esc(it.vendor) + '</div>' : '') + '</td>'
      + '<td>' + ft + '</td>'
      + '<td>' + esc(it.quota || '—') + (it.validity ? '<div class="note">' + esc(it.validity) + '</div>' : '') + '</td>'
      + '<td>' + esc(it.modality || '—') + '</td>'
      + '<td>' + (it.gates && it.gates.length ? it.gates.map(esc).join('、') : '—') + '</td>'
      + '<td>' + (it.last_verified ? esc(ago(it.last_verified)) : '—') + '</td>'
      + '<td>' + entry + detail + '</td>'
      + '<td>' + draftBtn + '</td>'
      + '</tr>';
  }).join('') || '<tr><td colspan="9" class="empty">该分类暂无条目</td></tr>';
}

if ($('eiBody')) {
  $('eiBody').onclick = async e => {
    const b = e.target.closest('button[data-draft]');
    if (!b) return;
    const items = (extIntelData && extIntelData.items) || [];
    const it = items[Number(b.dataset.draft)];
    if (!it) return;
    try {
      const draft = await extDraftFromIntel(it);
      openExtWizard(null, draft);
    } catch (err) {
      toast('生成草稿失败：' + err.message, 'bad');
    }
  };
}

/* 情报分类切换（事件委托：按钮由 innerHTML 渲染，逐个绑定会丢）。
   顶层守卫：元素不存在（老 index.html）时静默跳过。 */
if ($('eiFilters')) {
  $('eiFilters').onclick = e => {
    const b = e.target.closest('button[data-eif]');
    if (!b) return;
    extIntelFilter = b.dataset.eif;
    $('eiFilters').querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
    renderExtIntel();
  };
}
