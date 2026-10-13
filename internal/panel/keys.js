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
    $('akBody').innerHTML = '<tr><td colspan="7" class="empty">读取失败：' + esc(e.message) + '</td></tr>';
  }
}

function renderKeys() {
  $('akNote').textContent = akList.length + ' 把密钥 · 上限 50';
  if (!akList.length) {
    $('akBody').innerHTML = '<tr><td colspan="7" class="empty">尚未创建任何密钥——点右上角「创建密钥」生成一把，填进你的 agent</td></tr>';
    return;
  }
  $('akBody').innerHTML = akList.map(k => {
    const st = k.disabled ? '<span class="tag" style="color:var(--cool)">已禁用</span>' : '<span class="tag" style="color:var(--ok)">启用中</span>';
    return '<tr>' +
      '<td class="mark" aria-hidden="true"></td>' +
      '<td><b>' + esc(k.name) + '</b>' + (k.disabled ? '<div class="note">' + st + '</div>' : '<div class="note">' + st + '</div>') + '</td>' +
      '<td><code>' + esc(k.prefix) + '…</code></td>' +
      '<td>' + esc(ago(k.created_at)) + '</td>' +
      '<td>' + (k.last_used ? esc(ago(k.last_used)) : '—') + '</td>' +
      '<td class="num">' + fmtTok(k.used_count) + '</td>' +
      '<td class="c-acts">' +
        '<button class="xs" data-ak="rename" data-id="' + esc(k.id) + '">改名</button>' +
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

if ($('akBody')) $('akBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-ak]');
  if (!b) return;
  const id = b.dataset.id;
  const k = akList.find(x => x.id === id);
  if (!k) return;
  if (b.dataset.ak === 'rename') {
    akOpenForm('rename', k);
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

