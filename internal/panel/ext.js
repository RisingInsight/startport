'use strict';
/* ── ext.js：外部渠道（多平台）视图 ──────────────────────────────────
   自包含模块：只依赖 core.js 的 $ / esc / ago / api / toast。
   职责：只读观测（渠道运行态 + 额度情报）；渠道的增删改走「配置」视图的
   ext_channels 表单（保存即热重载），本视图不提供写入口——避免两处写配置。
   loadExt() 由 app.js 的路由 go() 调度。 */

let extChannels = [];
let extIntelData = null;
let extIntelFilter = 'all';

async function loadExt() {
  try {
    const d = await api('ext_channels');
    extChannels = d.channels || [];
    renderExtChannels();
  } catch (e) {
    const body = $('ecBody');
    if (body) body.innerHTML = '<tr><td colspan="6" class="empty">读取失败：' + esc(e.message) + '</td></tr>';
  }
  try {
    const d = await api('ext_intel');
    extIntelData = d;
    renderExtIntel();
  } catch (e) {
    const body = $('eiBody');
    if (body) body.innerHTML = '<tr><td colspan="7" class="empty">情报读取失败：' + esc(e.message) + '</td></tr>';
  }
}

function renderExtChannels() {
  const note = $('ecNote'), body = $('ecBody');
  if (!note || !body) return;
  const active = extChannels.filter(c => !c.disabled);
  note.textContent = extChannels.length
    ? active.length + ' / ' + extChannels.length + ' 个渠道启用中 · 路由前缀 ext:<渠道名>/<模型名>'
    : '';
  if (!extChannels.length) {
    body.innerHTML = '<tr><td colspan="6" class="empty"><div class="big">还没有外部渠道</div>' +
      '在「配置」视图的 ext_channels 里添加一个 OpenAI 兼容上游（base_url + api_key），' +
      '保存后即可用 <code>ext:渠道名/模型名</code> 调用它</div></td></tr>';
    return;
  }
  body.innerHTML = extChannels.map(c => {
    let tag;
    if (c.disabled) tag = '<span class="tag" style="color:var(--ink-3)">已停用</span>';
    else if (c.cooling) tag = '<span class="tag" style="color:var(--warn)">冷却 ' + dur(c.cool_remaining_sec) + '</span>';
    else tag = '<span class="tag" style="color:var(--ok)">可用</span>';
    const models = (c.models && c.models.length ? c.models : (c.auto_models || []));
    const modelTxt = models.length
      ? models.slice(0, 4).map(esc).join(', ') + (models.length > 4 ? ' 等 ' + models.length + ' 个' : '')
      : '<span style="color:var(--ink-3)">待发现（首次调用 /v1/models 时自动拉取）</span>';
    const err = c.last_error
      ? '<div class="note" style="color:var(--bad)">' + esc(c.last_error) + '</div>' : '';
    return '<tr>' +
      '<td><b>' + esc(c.name) + '</b><div class="note"><code>' + esc(c.base_url) + '</code></div>' + err + '</td>' +
      '<td>' + tag + '</td>' +
      '<td>' + modelTxt + '</td>' +
      '<td class="num">' + (c.in_flight || 0) + '</td>' +
      '<td>' + (c.last_ok && !c.last_ok.startsWith('0001-') ? esc(ago(c.last_ok)) : '—') + '</td>' +
      '<td><code style="font-size:11px">ext:' + esc(c.name) + '/模型名</code></td>' +
      '</tr>';
  }).join('');
}

function renderExtIntel() {
  const note = $('eiNote'), body = $('eiBody'), meta = $('eiMeta');
  if (!note || !body) return;
  const items = (extIntelData && extIntelData.items) || [];
  const st = (extIntelData && extIntelData.status) || {};
  if (meta) {
    meta.textContent = items.length
      ? '共 ' + items.length + ' 条 · 拉取于 ' + ago(extIntelData.fetched_at) +
        (extIntelData.source_updated_at ? ' · 数据源更新 ' + esc(extIntelData.source_updated_at) : '')
      : '';
  }
  if (!st.enabled) {
    note.textContent = '';
    body.innerHTML = '<tr><td colspan="7" class="empty">额度情报未启用（config.ext_intel.enabled=false 或未配置 url）</td></tr>';
    return;
  }
  if (!items.length) {
    note.textContent = '';
    const err = extIntelData && extIntelData.last_error;
    body.innerHTML = '<tr><td colspan="7" class="empty">' +
      (err ? '拉取失败：' + esc(err) : '暂无数据，等待下一轮拉取') + '</td></tr>';
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
    const ft = it.free_type ? '<span class="tag" style="color:' + ftColor(it.free_type) + '">' + esc(it.free_type) + '</span>' : '';
    const entry = it.entry_url
      ? '<a href="' + esc(it.entry_url) + '" target="_blank" rel="noopener noreferrer">领取 →</a>' : '';
    const detail = it.detail_url
      ? ' <a href="' + esc(it.detail_url) + '" target="_blank" rel="noopener noreferrer">详情</a>' : '';
    return '<tr>' +
      '<td><b>' + esc(it.name) + '</b>' +
        (it.vendor ? '<div class="note">' + esc(it.vendor) + '</div>' : '') + '</td>' +
      '<td>' + ft + '</td>' +
      '<td>' + esc(it.quota || '—') + (it.validity ? '<div class="note">' + esc(it.validity) + '</div>' : '') + '</td>' +
      '<td>' + esc(it.modality || '—') + '</td>' +
      '<td>' + (it.gates && it.gates.length ? it.gates.map(esc).join('、') : '—') + '</td>' +
      '<td>' + (it.last_verified ? esc(ago(it.last_verified)) : '—') + '</td>' +
      '<td>' + entry + detail + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="7" class="empty">该分类暂无条目</td></tr>';
}

/* 情报分类切换（事件委托：按钮由 innerHTML 渲染，逐个绑定会丢）。
   顶层守卫：keys.js 模式——元素不存在（老 index.html）时静默跳过。 */
if ($('eiFilters')) {
  $('eiFilters').onclick = e => {
    const b = e.target.closest('button[data-eif]');
    if (!b) return;
    extIntelFilter = b.dataset.eif;
    $('eiFilters').querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
    renderExtIntel();
  };
}
