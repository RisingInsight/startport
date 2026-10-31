'use strict';
/* ── keys.js：API 密钥（agent 接入）视图 ──────────────────────────────
   2026-10-13 自 app.js 拆出。自包含模块：只依赖 core.js 的 $ / esc / ago / api /
   toast，顶层绑定均有 if ($(...)) 守卫；loadKeys() 由 app.js 的路由 go() 调度。 */
/* ── API 密钥:agent 接入密钥管理 ───────────────────────────────────
   全钥只在创建响应里出现一次(网关只存 SHA-256 摘要);列表仅前缀。
   agent 密钥只开放 /v1/*,面板端点对它们一律 401(隔离边界在服务端)。 */
let akList = [];

async function loadKeys() {
  try {
    const d = await api('keys');
    akList = d.keys || [];
    renderKeys();
  } catch (e) {
    $('akNote').textContent = '—';
    $('akBody').innerHTML = '<tr><td colspan="8" class="empty">读取失败：' + esc(e.message) + '</td></tr>';
  }
  loadWizard();
}

/* akLimitText 限额列文案：周期中文短名 + 人类可读 token 数；未设限显示 —。 */
const AK_PERIOD_CN = { day: '日', week: '周', month: '月' };
function akLimitText(k) {
  const n = Number(k && k.limit_tokens || 0);
  if (!n) return '<span class="muted">—</span>';
  return fmtTok(n) + ' / ' + (AK_PERIOD_CN[k.period] || '日');
}

function renderKeys() {
  $('akNote').textContent = akList.length + ' 把密钥 · 上限 50';
  if (!akList.length) {
    $('akBody').innerHTML = '<tr><td colspan="8" class="empty">尚未创建任何密钥——点右上角「创建密钥」生成一把，填进你的 agent</td></tr>';
    return;
  }
  $('akBody').innerHTML = akList.map(k => {
    const st = k.disabled ? '<span class="tag cool">已禁用</span>' : '<span class="tag ok">启用中</span>';
    return '<tr>' +
      '<td class="mark" aria-hidden="true"></td>' +
      '<td><b>' + esc(k.name) + '</b><div class="note">' + st + '</div></td>' +
      '<td><code>' + esc(k.prefix) + '…</code></td>' +
      '<td>' + esc(ago(k.created_at)) + '</td>' +
      '<td>' + (k.last_used ? esc(ago(k.last_used)) : '—') + '</td>' +
      '<td class="num">' + fmtTok(k.used_count) + '</td>' +
      '<td class="num">' + akLimitText(k) + '</td>' +
      '<td class="c-acts">' +
        '<button class="xs" data-ak="rename" data-id="' + esc(k.id) + '">改名</button>' +
        '<button class="xs" data-ak="limit" data-id="' + esc(k.id) + '">限额</button>' +
        '<button class="xs" data-ak="toggle" data-id="' + esc(k.id) + '">' + (k.disabled ? '启用' : '禁用') + '</button>' +
        '<button class="xs" data-ak="del" data-id="' + esc(k.id) + '">删除</button>' +
      '</td></tr>';
  }).join('');
}

/* akForm 打开创建/改名弹窗(mode: create | rename, rename 带 id 与当前名) */
let akFormMode = { mode: 'create', id: null };
function akOpenForm(mode, k) {
  akFormMode = { mode, id: k ? k.id : null };
  $('akFormTitle').textContent = mode === 'create' ? '创建 API 密钥' : '重命名密钥';
  $('btnAkFormOk').textContent = mode === 'create' ? '创建' : '保存';
  $('akNameInput').value = k ? k.name : '';
  $('akFormErr').hidden = true;
  $('akFormVeil').classList.add('on');
  setTimeout(() => $('akNameInput').focus(), 60);
}
function akCloseForm() { $('akFormVeil').classList.remove('on'); }

async function akSubmitForm() {
  const name = $('akNameInput').value.trim();
  if (!name) { $('akFormErr').textContent = '名称不能为空'; $('akFormErr').hidden = false; return; }
  try {
    if (akFormMode.mode === 'create') {
      const d = await api('keys', { method: 'POST', body: JSON.stringify({ name }) });
      akCloseForm();
      loadKeys();
      $('akFullKey').value = d.full;
      $('akShowVeil').classList.add('on');
    } else {
      await api('keys/' + encodeURIComponent(akFormMode.id) + '/rename', { method: 'POST', body: JSON.stringify({ name }) });
      akCloseForm();
      loadKeys();
    }
  } catch (e) {
    $('akFormErr').textContent = e.message;
    $('akFormErr').hidden = false;
  }
}

if ($('btnAkCreate')) $('btnAkCreate').onclick = () => akOpenForm('create', null);
if ($('btnAkFormCancel')) $('btnAkFormCancel').onclick = akCloseForm;
if ($('btnAkFormOk')) $('btnAkFormOk').onclick = akSubmitForm;
if ($('akNameInput')) $('akNameInput').addEventListener('keydown', e => { if (e.key === 'Enter') akSubmitForm(); });
if ($('akFormVeil')) $('akFormVeil').addEventListener('click', e => { if (e.target === $('akFormVeil')) akCloseForm(); });
if ($('btnAkShowDone')) $('btnAkShowDone').onclick = () => $('akShowVeil').classList.remove('on');
if ($('btnAkCopy')) $('btnAkCopy').onclick = () => {
  copyText($('akFullKey').value).then(() => toast('已复制', 'ok')).catch(() => {});
};

/* ── 限额弹窗：设置/清除该密钥的周期 token 上限（0 或留空 = 不限） ── */
let akLimitTarget = null;
function akOpenLimit(k) {
  akLimitTarget = k;
  $('akLimitTitle').textContent = '设置「' + k.name + '」的用量限额';
  $('akLimitInput').value = Number(k.limit_tokens || 0) || '';
  $('akLimitPeriod').value = k.period || 'day';
  $('akLimitErr').hidden = true;
  $('akLimitVeil').classList.add('on');
  setTimeout(() => $('akLimitInput').focus(), 60);
}
function akCloseLimit() { $('akLimitVeil').classList.remove('on'); akLimitTarget = null; }

async function akSubmitLimit() {
  if (!akLimitTarget) return;
  const raw = String($('akLimitInput').value || '').trim();
  const n = Number(raw);
  if (raw !== '' && (!Number.isFinite(n) || n < 0)) {
    $('akLimitErr').textContent = '限额须为非负数字（留空 = 不限）';
    $('akLimitErr').hidden = false;
    return;
  }
  const limitTokens = Math.floor(n || 0);
  const period = limitTokens > 0 ? $('akLimitPeriod').value : '';
  try {
    await api('keys/' + encodeURIComponent(akLimitTarget.id) + '/limit', {
      method: 'POST',
      body: JSON.stringify({ limit_tokens: limitTokens, period: period }),
    });
    akCloseLimit();
    loadKeys();
    toast(limitTokens > 0 ? '限额已保存：' + limitTokens.toLocaleString() + ' tokens / ' + (AK_PERIOD_CN[period] || '日') : '限额已清除（不限）', 'ok');
  } catch (e) {
    $('akLimitErr').textContent = e.message;
    $('akLimitErr').hidden = false;
  }
}

if ($('btnAkLimitCancel')) $('btnAkLimitCancel').onclick = akCloseLimit;
if ($('btnAkLimitOk')) $('btnAkLimitOk').onclick = akSubmitLimit;
if ($('akLimitInput')) $('akLimitInput').addEventListener('keydown', e => { if (e.key === 'Enter') akSubmitLimit(); });
if ($('akLimitVeil')) $('akLimitVeil').addEventListener('click', e => { if (e.target === $('akLimitVeil')) akCloseLimit(); });

/* ── 接入向导：生成指向本网关的 agent 配置片段 ───────────────────────
   agent 清单（配置文件路径按服务端 OS）来自 /api/wizard/agents；网关地址取
   location.origin（面板与 /v1 同端口）；密钥用户自填、仅本地拼装。 */
let wizAgentRows = [];
let wizAgentId = '';

async function loadWizard() {
  try {
    const d = await api('wizard/agents');
    wizAgentRows = d.agents || [];
  } catch (e) {
    wizAgentRows = [];
  }
  const sel = $('wizAgent');
  if (!sel) return;
  if (!wizAgentRows.length) {
    sel.innerHTML = '<option value="">（向导不可用）</option>';
    $('wizSnippet').textContent = '—';
    return;
  }
  sel.innerHTML = wizAgentRows.map(a =>
    '<option value="' + esc(a.id) + '">' + esc(a.name) + '</option>').join('');
  if (!wizAgentRows.some(a => a.id === wizAgentId)) wizAgentId = wizAgentRows[0].id;
  sel.value = wizAgentId;
  renderWizard();
}

/* wizSnippetFor 按 agent 拼配置片段。模型名/密钥为空时用显眼占位符，
   提醒复制前补齐。 */
function wizSnippetFor(a, model, key) {
  const base = location.origin;
  if (a.id === 'codex') {
    return '# 追加到 ' + a.config_path + '\n' +
      'model_provider = "starport"\n' +
      'model = "' + model + '"\n' +
      '\n' +
      '[model_providers.starport]\n' +
      'name = "StarPort 星港"\n' +
      'base_url = "' + base + '/v1"\n' +
      'wire_api = "responses"   # 也可用 "chat"（/v1/chat/completions）\n' +
      'env_key = "STARPORT_API_KEY"\n' +
      '\n' +
      '# 设置环境变量后重启 Codex：\n' +
      'STARPORT_API_KEY=' + key;
  }
  if (a.id === 'claude') {
    return '// 并入 ' + a.config_path + ' 的 env（保留已有键）\n' +
      '{\n' +
      '  "env": {\n' +
      '    "ANTHROPIC_BASE_URL": "' + base + '",\n' +
      '    "ANTHROPIC_AUTH_TOKEN": "' + key + '",\n' +
      '    "ANTHROPIC_MODEL": "' + model + '"\n' +
      '  }\n' +
      '}\n' +
      '// 重启 Claude Code 生效';
  }
  // 通用 OpenAI 兼容
  return '# 通用 OpenAI 兼容环境变量\n' +
    'OPENAI_BASE_URL=' + base + '/v1\n' +
    'OPENAI_API_KEY=' + key + '\n' +
    '\n' +
    '# 请求体里指定模型：\n' +
    '{ "model": "' + model + '", "messages": [ … ] }';
}

function renderWizard() {
  const a = wizAgentRows.find(x => x.id === wizAgentId);
  if (!a) return;
  const model = ($('wizModel').value || '').trim() || '（填模型名）';
  const key = ($('wizKey').value || '').trim() || 'sp-……（粘贴你的密钥）';
  $('wizAgentNote').textContent =
    (a.config_path ? '配置文件：' + a.config_path + '。' : '') + (a.note || '');
  $('wizSnippet').textContent = wizSnippetFor(a, model, key);
}

if ($('wizAgent')) $('wizAgent').onchange = () => {
  wizAgentId = $('wizAgent').value;
  renderWizard();
};
if ($('wizModel')) $('wizModel').oninput = renderWizard;
if ($('wizKey')) $('wizKey').oninput = renderWizard;
if ($('btnWizCopy')) $('btnWizCopy').onclick = () => {
  const text = $('wizSnippet') && $('wizSnippet').textContent || '';
  if (!text || text === '—') return;
  copyText(text).then(() => toast('片段已复制', 'ok')).catch(() => {});
};

if ($('akBody')) $('akBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-ak]');
  if (!b) return;
  const id = b.dataset.id;
  const k = akList.find(x => x.id === id);
  if (!k) return;
  if (b.dataset.ak === 'rename') {
    akOpenForm('rename', k);
  } else if (b.dataset.ak === 'limit') {
    akOpenLimit(k);
  } else if (b.dataset.ak === 'toggle') {
    try {
      await api('keys/' + encodeURIComponent(id) + '/disable', { method: 'POST', body: JSON.stringify({ disabled: !k.disabled }) });
      loadKeys();
    } catch (e) { toast(e.message, 'err'); }
  } else if (b.dataset.ak === 'del') {
    if (!confirm('删除后该密钥立即失效，使用它的 agent 会收到 401，需要换用其他密钥。确认删除「' + k.name + '」？')) return;
    try {
      await api('keys/' + encodeURIComponent(id) + '/delete', { method: 'POST' });
      loadKeys();
    } catch (e) { toast(e.message, 'err'); }
  }
});

