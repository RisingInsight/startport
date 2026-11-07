'use strict';
/* ── diagnose_ui.js：一键诊断（借鉴 magpie 的诊断面板思路）────────
   账号中心「一键诊断」按钮 → GET /startport/api/diagnose → 弹窗展示结果。
   数据含：池健康、各平台上游 DNS/连通性、外部渠道状态、近期风控拦截。 */
if ($('btnDiag')) $('btnDiag').onclick = async () => {
  $('diagVeil').classList.add('on');
  const btn = $('btnDiag');
  btn.disabled = true; btn.textContent = '诊断中…';
  try {
    const d = await api('diagnose');
    const lines = [];
    lines.push('版本：' + (d.version || '—'));
    const p = d.pool || {};
    lines.push('账号池：总 ' + (p.total||0) + ' · 健康 ' + (p.healthy||0) + ' · 冷却 ' + (p.cooling||0) + ' · 禁用 ' + (p.disabled||0));
    lines.push('');
    lines.push('── 平台上游连通性 ──');
    for (const pl of (d.platforms || [])) {
      lines.push('  ' + pl.platform + ' (' + pl.host + '): ' + (pl.ok ? '✓ ' + pl.tcp : '✗ ' + pl.tcp));
    }
    if ((d.ext_channels || []).length) {
      lines.push('');
      lines.push('── 外部渠道 ──');
      for (const ch of d.ext_channels) {
        lines.push('  ' + ch.name + ': ' + (ch.ok ? '✓ ' : '✗ ') + ch.state);
      }
    }
    lines.push('');
    lines.push('近 24h 风控拦截: ' + (d.recent_blocks_24h || 0) + ' 次 · 请求: ' + (d.recent_requests_24h || 0) + ' 条');
    $('diagResult').textContent = lines.join('\n');
  } catch (e) {
    $('diagResult').textContent = '诊断失败：' + e.message;
  } finally {
    btn.disabled = false; btn.textContent = '重新诊断';
  }
};
