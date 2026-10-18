'use strict';
/* ── extwiz.js：外部渠道「新建/编辑」弹层 + 情报草稿 + 配置回写 ───────
   只依赖 core.js（$(esc/api/toast)），不引入任何框架/构建步骤。

   写路径唯一：POST /startport/api/config，body 只提交 {ext_channels:[...]}。
   **绝不**调用 collectConfig()（那会把整个配置表单一起回写，覆盖用户没动过的
   字段）。渠道数组是整体替换（不做深合并），因此：
     - 未编辑的渠道必须原样带回（含脱敏哨兵 secret）；
     - 已保存凭证的 secret 在 GET config 里是哨兵 `__STARPORT_SECRET_SET__`，
       提交时原样带回，服务端 scrubSecretSentinels 按 (渠道名, label) 回填磁盘
       原值；
     - 新增凭证必须填 secret（否则服务端校验失败，前端就地报错）。
   本文件任何地方都不把 secret 写进 DOM 之外的日志/提示。 */

const EW_SENTINEL = '__STARPORT_SECRET_SET__';

let ewEditingName = null;   // 正在编辑的渠道名（null = 新建）
let ewLegacyAPIKey = false; // 编辑的是老配置（只有 api_key）：保存时带回哨兵
let ewDraftNative = false;  // 草稿来自 native_required 预设

/* ── 配置读写（唯一写路径的封装） ─────────────────────────────────── */

async function extRawChannels() {
  const d = await api('config');
  const cfg = (d && d.config) || {};
  return Array.isArray(cfg.ext_channels) ? cfg.ext_channels.slice() : [];
}

async function extMutateChannels(fn) {
  const list = await extRawChannels();
  const next = fn(list) || list;
  await api('config', { method: 'POST', body: JSON.stringify({ ext_channels: next }) });
}

async function extFindChannelConfig(name) {
  try {
    const list = await extRawChannels();
    return list.find(c => c && c.name === name) || null;
  } catch (e) {
    return null;
  }
}

/* ── 预设（情报草稿的建议值来源；base_url 恒为空） ────────────────── */

let extPresetCache = null;

async function extPresets() {
  if (!extPresetCache) {
    const d = await api('ext_presets');
    extPresetCache = d.presets || [];
  }
  return extPresetCache;
}

function ewNormKey(s) {
  return String(s == null ? '' : s).toLowerCase()
    .replace(/[\s\u3000\-_.()（）]/g, '')
    .replace(/(官方|api|cn)$/g, '');
}

function ewSlug(s) {
  const out = String(s == null ? '' : s).toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return out;
}

/* extDraftFromIntel 情报条目 → 渠道草稿（**不发任何网络请求**，只查本地预设表）。
   草稿不含 secret、base_url 为空——必须由用户确认后填（P0-10）。 */
async function extDraftFromIntel(it) {
  const presets = await extPresets();
  let p = null;
  if (it.preset_key) p = presets.find(x => x.key === it.preset_key) || null;
  if (!p) {
    const nk = ewNormKey(it.name);
    p = presets.find(x => ewNormKey(x.name) === nk || ewNormKey(x.key) === nk
      || (x.aliases || []).some(a => ewNormKey(a) === nk)) || null;
  }
  const name = (p && p.key) || ewSlug(it.name) || 'ch';
  const noteParts = [it.free_type, it.quota, it.validity,
    it.detail_url || it.entry_url || '',
    it.last_verified ? ('核实于 ' + it.last_verified) : ''].filter(Boolean);
  return {
    name: name,
    base_url: '',
    protocol: p ? p.protocol : 'openai',
    adapter: (p && p.adapter) || '',
    path: (p && p.path) || '',
    auth: (p && p.auth) || { mode: 'bearer' },
    headers: (p && p.headers) || {},
    models: (p && p.models) || [],
    aliases: {},
    note: noteParts.join(' · '),
    native_required: !!(p && p.native_required)
  };
}

/* ── 弹层 ─────────────────────────────────────────────────────────── */

function ewEl(id) { return $(id); }

/* openExtWizard 打开弹层：channel 非空 = 编辑已保存渠道；draft 非空 = 草稿预填。 */
function openExtWizard(channel, draft) {
  const veil = ewEl('extWizVeil');
  if (!veil) return;
  ewEditingName = channel ? channel.name : null;
  ewLegacyAPIKey = !!(channel && channel.api_key);
  ewDraftNative = !!(draft && draft.native_required);
  const src = channel || draft || {};

  ewEl('ewTitle').textContent = channel ? ('编辑渠道 · ' + channel.name) : '新建渠道';
  ewEl('ewHint').textContent = channel
    ? '保存后立即生效（热重载）。留空的 secret 保持原值；改 label/secret = 视为新凭证。'
    : '基础信息 + 鉴权 + 模型/别名 + 凭证列表；保存后立即生效（热重载）。';
  ewEl('ewName').value = src.name || '';
  ewEl('ewName').disabled = false;
  ewEl('ewBase').value = src.base_url || '';
  ewEl('ewProtocol').value = src.protocol || 'openai';
  if (ewEl('ewAdapter')) ewEl('ewAdapter').value = src.adapter || '';
  ewEl('ewPath').value = src.path || '';
  ewEl('ewTimeout').value = src.timeout_seconds ? String(src.timeout_seconds) : '';
  const auth = src.auth || {};
  ewEl('ewAuthMode').value = auth.mode || '';
  ewEl('ewAuthParam').value = auth.param || '';
  ewEl('ewAuthHeader').value = auth.header || '';
  // 适配器与鉴权互斥：auth 字段填完再收窄 UI（顺序反了会把已填的 auth 留在禁用控件里）。
  ewSyncAdapter();
  ewEl('ewHeaders').value = Object.keys(src.headers || {})
    .map(k => k + ': ' + src.headers[k]).join('\n');
  ewEl('ewModels').value = (src.models || []).join('\n');
  ewEl('ewAliases').value = Object.keys(src.aliases || {})
    .map(k => k + '=' + src.aliases[k]).join('\n');
  ewEl('ewAllowPrivate').checked = !!src.allow_private;
  ewEl('ewNote').value = src.note || '';
  ewErr('', false);
  ewEl('ewProbeOut').textContent = '';

  // native_required 提示（草稿来自需原生适配的平台时置灰保存并说明原因）。
  const native = ewEl('ewNative');
  if (ewDraftNative) {
    native.hidden = false;
    native.textContent = '该平台凭证需要原生适配（签名/设备指纹/有状态转发），静态 key 直连会失败；'
      + '本弹层仍可保存配置，但请不要指望它能连通——建议改用官方标准入口或等待 P2 原生适配器。';
  } else {
    native.hidden = true;
    native.textContent = '';
  }
  ewEl('ewSave').disabled = false;

  // 凭证行：已保存渠道的 secret 是哨兵（GET config 脱敏），显示为空 + placeholder。
  const creds = ewEl('ewCreds');
  creds.innerHTML = '';
  (src.credentials || []).forEach(c => ewAddCredRow(c));
  if (!(src.credentials || []).length) ewAddCredRow(null);

  veil.classList.add('on');
  setTimeout(() => { try { ewEl('ewName').focus(); } catch (e) {} }, 60);
}

function closeExtWizard() {
  const veil = ewEl('extWizVeil');
  if (veil) veil.classList.remove('on');
  ewEditingName = null;
  ewLegacyAPIKey = false;
  ewDraftNative = false;
}

function ewErr(msg, show) {
  const el = ewEl('ewErr');
  if (!el) return;
  el.textContent = msg || '';
  el.hidden = !show;
}

/* ewSyncAdapter 原生适配器与「鉴权方式」互斥：选了适配器就置灰并清空 auth 字段。
   服务端对两者并存是硬错误（adapter 自己注入鉴权），这里提前把 UI 收窄，
   避免用户配完再被保存校验打回。 */
function ewSyncAdapter() {
  const sel = ewEl('ewAdapter');
  const on = !!(sel && sel.value);
  ['ewAuthMode', 'ewAuthParam', 'ewAuthHeader'].forEach(id => {
    const el = ewEl(id);
    if (!el) return;
    el.disabled = on;
    if (on && id === 'ewAuthMode') el.value = '';
  });
  const hint = ewEl('ewAuthHint');
  if (hint) {
    hint.textContent = on
      ? '已选原生适配器：鉴权由适配器注入（此处置灰）'
      : '选了原生适配器时此项由适配器接管（会置灰）';
  }
}

/* ewAddCredRow 渲染一行凭证。cred 非空 = 来自已保存配置（secret 可能为哨兵）。 */
function ewAddCredRow(cred) {
  const box = ewEl('ewCreds');
  if (!box) return;
  const c = cred || {};
  const masked = c.secret === EW_SENTINEL;
  const row = document.createElement('div');
  row.className = 'ew-cred';
  row.style.cssText = 'display:flex;gap:8px;align-items:flex-end;flex-wrap:wrap;margin:0 0 8px;'
    + 'padding:8px;border:1px solid var(--line-soft);border-radius:8px';
  row.dataset.masked = masked ? '1' : '0';
  row.dataset.auth = c.auth ? JSON.stringify(c.auth) : '';
  row.innerHTML =
    '<label class="fld" style="flex:1;min-width:110px"><span class="lb">label</span>'
    + '<input data-k="label" autocomplete="off" placeholder="primary"></label>'
    + '<label class="fld" style="flex:2;min-width:170px"><span class="lb">secret</span>'
    + '<input data-k="secret" type="password" autocomplete="new-password" placeholder="'
    + (masked ? '已设置（留空则不修改）' : 'sk-…') + '"></label>'
    + '<label class="fld" style="flex:1;min-width:110px"><span class="lb">expires_at</span>'
    + '<input data-k="expires_at" autocomplete="off" placeholder="2026-12-31"></label>'
    + '<label class="fld" style="flex:0 0 auto"><span class="lb">停用</span>'
    + '<input data-k="disabled" type="checkbox"></label>'
    + '<button type="button" class="xs" data-k="rm">移除</button>';
  row.querySelector('[data-k="label"]').value = c.label || '';
  row.querySelector('[data-k="expires_at"]').value = c.expires_at || '';
  row.querySelector('[data-k="disabled"]').checked = !!c.disabled;
  row.querySelector('[data-k="rm"]').onclick = () => {
    if (box.querySelectorAll('.ew-cred').length <= 1) {
      // 保留至少一行（空行 = 不配置凭证）。
      row.querySelector('[data-k="label"]').value = '';
      row.querySelector('[data-k="secret"]').value = '';
      row.dataset.masked = '0';
      return;
    }
    row.remove();
  };
  box.appendChild(row);
}

/* ── 表单 → JSON ─────────────────────────────────────────────────── */

function ewParseKV(text, sep) {
  const out = {};
  String(text || '').split(/\n+/).forEach(line => {
    line = line.trim();
    if (!line) return;
    const i = line.indexOf(sep);
    if (i <= 0) return;
    const k = line.slice(0, i).trim();
    const v = line.slice(i + 1).trim();
    if (k && v) out[k] = v;
  });
  return out;
}

function ewParseList(text) {
  return String(text || '').split(/[\n,]+/).map(s => s.trim()).filter(Boolean);
}

/* ewCollect 收集表单为 ext_channels[] 条目。strict=true（保存）时校验必填项。 */
function ewCollect(strict) {
  const name = String(ewEl('ewName').value || '').trim();
  const base = String(ewEl('ewBase').value || '').trim();
  if (strict) {
    if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(name)) {
      throw new Error('渠道名只允许小写字母/数字/连字符/下划线，且以字母或数字开头（≤64）');
    }
    if (!base) throw new Error('base_url 不能为空（草稿不替你猜地址）');
  }
  const ch = { name: name };
  if (base) ch.base_url = base;
  ch.protocol = ewEl('ewProtocol').value || 'openai';
  // 原生适配器（空 = 按协议）。非空时 auth 一律不发：服务端对两者并存是硬错误，
  // 且 ui 已把鉴权控件置灰（ewSyncAdapter）。
  const adapter = String((ewEl('ewAdapter') || {}).value || '').trim();
  if (adapter) ch.adapter = adapter;
  const path = String(ewEl('ewPath').value || '').trim();
  if (path) ch.path = path;
  const tmo = parseInt(ewEl('ewTimeout').value, 10);
  ch.timeout_seconds = Number.isFinite(tmo) && tmo > 0 ? tmo : 0;
  ch.models = ewParseList(ewEl('ewModels').value);
  const aliases = ewParseKV(ewEl('ewAliases').value, '=');
  if (Object.keys(aliases).length) ch.aliases = aliases;
  const headers = ewParseKV(ewEl('ewHeaders').value, ':');
  if (Object.keys(headers).length) ch.headers = headers;
  if (ewEl('ewAllowPrivate').checked) ch.allow_private = true;
  const note = String(ewEl('ewNote').value || '').trim();
  if (note) ch.note = note;

  const mode = adapter ? '' : (ewEl('ewAuthMode').value || '');
  if (mode) {
    const auth = { mode: mode };
    if (mode === 'query') auth.param = String(ewEl('ewAuthParam').value || '').trim() || 'key';
    if (mode === 'custom') {
      auth.header = String(ewEl('ewAuthHeader').value || '').trim();
      if (strict && !auth.header) throw new Error('鉴权方式 custom 必须填写 header 名');
    }
    ch.auth = auth;
  }

  const creds = [];
  const rows = ewEl('ewCreds') ? ewEl('ewCreds').querySelectorAll('.ew-cred') : [];
  rows.forEach(row => {
    const label = row.querySelector('[data-k="label"]').value.trim();
    const secret = row.querySelector('[data-k="secret"]').value;
    const expires = row.querySelector('[data-k="expires_at"]').value.trim();
    const disabled = row.querySelector('[data-k="disabled"]').checked;
    if (!label && !secret) return; // 空行 = 忽略
    if (strict && !label) throw new Error('凭证 label 不能为空（渠道内唯一）');
    const item = { label: label };
    if (secret) {
      item.secret = secret;
    } else if (row.dataset.masked === '1') {
      // 已保存凭证：原样带回哨兵，服务端按 (渠道名, label) 回填磁盘原值。
      item.secret = EW_SENTINEL;
    } else if (strict) {
      throw new Error('凭证 ' + (label || '(未命名)') + ' 需要填写 secret（新凭证不能用空值）');
    }
    if (expires) item.expires_at = expires;
    if (disabled) item.disabled = true;
    // 原生适配器独占鉴权：不再回传凭证级 auth（服务端拒绝两者并存）。
    if (!adapter && row.dataset.auth) {
      try { item.auth = JSON.parse(row.dataset.auth); } catch (e) {}
    }
    creds.push(item);
  });
  if (creds.length) ch.credentials = creds;
  return ch;
}

/* ── 保存 / 探测 ─────────────────────────────────────────────────── */

async function ewSave() {
  let ch;
  const editing = ewEditingName;
  try {
    ch = ewCollect(true);
    // 老配置（只有 api_key）编辑：向导不展示也不修改 api_key，保存时带回脱敏
    // 哨兵，服务端 scrubSecretSentinels 按渠道名回填磁盘原值。一旦用户配了
    // credentials，就不再带 api_key（两者互斥，服务端会拒绝同时出现）。
    // 改渠道名时无法按名回填，要求用户显式添加凭证（否则会静默丢 key）。
    if (editing && ewLegacyAPIKey && !(ch.credentials && ch.credentials.length)) {
      if (editing !== ch.name) {
        throw new Error('老配置渠道改名前请先添加凭证：api_key 按渠道名回填，改名会导致密钥丢失');
      }
      ch.api_key = EW_SENTINEL;
    }
  } catch (e) {
    ewErr(e.message, true);
    return;
  }
  ewErr('', false);
  try {
    await extMutateChannels(list => {
      const out = [];
      let replaced = false;
      for (const c of list) {
        if (!c) continue;
        if (editing && c.name === editing) { out.push(ch); replaced = true; continue; }
        if (c.name === ch.name) {
          if (editing) throw new Error('渠道名已存在：' + ch.name);
          throw new Error('渠道名已存在：' + ch.name);
        }
        out.push(c);
      }
      if (!replaced) out.push(ch);
      return out;
    });
    toast((editing ? '已更新渠道 ' : '已创建渠道 ') + ch.name, 'ok');
    closeExtWizard();
    loadExt();
  } catch (e) {
    ewErr('保存失败：' + e.message, true);
  }
}

async function ewProbe() {
  const out = ewEl('ewProbeOut');
  const btn = ewEl('ewProbe');
  if (!out) return;
  out.textContent = '测试中…';
  if (btn) btn.disabled = true;
  try {
    let body;
    if (ewEditingName) {
      // 已保存渠道：用服务端配置探测（表单里 secret 是哨兵，绝不能发上游）。
      const rows = ewEl('ewCreds').querySelectorAll('.ew-cred');
      const label = rows.length ? rows[0].querySelector('[data-k="label"]').value.trim() : '';
      body = { name: ewEditingName };
      if (label) body.credential_label = label;
    } else {
      const ch = ewCollect(false);
      if (!ch.base_url) throw new Error('先填 base_url');
      body = { channel: ch };
    }
    const d = await api('ext_channels/probe', { method: 'POST', body: JSON.stringify(body) });
    const r = d.result || {};
    if (r.ok) {
      out.style.color = 'var(--ok)';
      out.textContent = '连通正常 · ' + (r.latency_ms || 0) + 'ms · ' + (r.models_count || 0) + ' 个模型'
        + (r.models_sample && r.models_sample.length ? '（' + r.models_sample.slice(0, 3).join(', ') + '…）' : '');
    } else {
      out.style.color = 'var(--bad)';
      out.textContent = '探测失败：' + (r.error || ('HTTP ' + (r.status || '?')));
    }
  } catch (e) {
    out.style.color = 'var(--bad)';
    out.textContent = '探测失败：' + e.message;
  } finally {
    if (btn) btn.disabled = false;
  }
}

/* ── 事件绑定（顶层守卫：元素不存在时静默跳过） ───────────────────── */

if (ewEl('extWizVeil')) {
  if (ewEl('ewAddCred')) ewEl('ewAddCred').onclick = () => ewAddCredRow(null);
  if (ewEl('ewAdapter')) ewEl('ewAdapter').onchange = ewSyncAdapter;
  if (ewEl('ewSave')) ewEl('ewSave').onclick = ewSave;
  if (ewEl('ewProbe')) ewEl('ewProbe').onclick = ewProbe;
  if (ewEl('ewCancel')) ewEl('ewCancel').onclick = closeExtWizard;
  ewEl('extWizVeil').onclick = e => {
    if (e.target === ewEl('extWizVeil')) closeExtWizard();
  };
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && ewEl('extWizVeil').classList.contains('on')) closeExtWizard();
  });
}
