(function () {
  'use strict';
  // Service status view of the public /status page: a customer-facing
  // availability summary fed by /status/service. The detailed monitor view
  // keeps its own script (status.js); this one also owns the view tabs and,
  // in service-only mode (no monitor script on the page), the theme toggle.
  const $ = (id) => document.getElementById(id);
  const esc = (v) => String(v == null ? '' : v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const taiwanFlag = $('taiwan-flag').innerHTML;
  const flagText = (text) => esc(text).replaceAll('🇹🇼', taiwanFlag);
  const flag = (cc) => {
    cc = String(cc || '').trim().toUpperCase();
    if (/^[\u{1F1E6}-\u{1F1FF}]{2}$/u.test(cc)) return flagText(cc);
    return cc === 'TW' ? taiwanFlag : /^[A-Z]{2}$/.test(cc) ? String.fromCodePoint(...cc.split('').map((v) => 127397 + v.charCodeAt(0))) : '◎';
  };
  // Latency figures stay in the ink colour (the number itself carries the
  // magnitude); only the failure rate keeps the shared severity colours,
  // as the one alert signal per carrier.
  const lc = (v) => v > 3 ? 'var(--pv-red)' : v > 1 ? 'var(--pv-orange)' : v > .2 ? 'var(--pv-yellow)' : 'var(--pv-green)';

  // --- theme (service-only mode only; status.js owns it otherwise) ---
  if (window.SVC_ONLY) {
    const themeKey = 'polaris.theme', legacyThemeKey = 'polaris.status.theme';
    const systemDark = () => typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches;
    const applyTheme = (value) => {
      const dark = value === 'dark' || (value !== 'light' && systemDark());
      document.body.dataset.theme = value;
      $('theme').textContent = dark ? '切换浅色' : '切换深色';
      $('theme').setAttribute('aria-pressed', String(dark));
    };
    let theme = 'auto';
    try {
      const raw = localStorage.getItem(themeKey) || localStorage.getItem(legacyThemeKey);
      if (raw) {
        let mode = raw;
        try { const v = JSON.parse(raw); if (v && typeof v === 'object') mode = v.mode; } catch (_) {}
        if (mode === 'light' || mode === 'dark') theme = mode;
        else if (mode === 'system') theme = 'auto';
      }
    } catch (_) {}
    applyTheme(theme);
    $('theme').addEventListener('click', () => {
      theme = (theme === 'dark' || (theme === 'auto' && systemDark())) ? 'light' : 'dark';
      try {
        let stored = {};
        try { const v = JSON.parse(localStorage.getItem(themeKey) || '{}'); if (v && typeof v === 'object') stored = v; } catch (_) {}
        localStorage.setItem(themeKey, JSON.stringify({ ...stored, mode: theme }));
        localStorage.removeItem(legacyThemeKey);
      } catch (_) {}
      applyTheme(theme);
    });
  }

  // --- view tabs ---
  let view = 'service';
  if (!window.SVC_ONLY) {
    view = location.hash === '#monitor' ? 'monitor' : 'service';
    window.__svcMonitorActive = view === 'monitor';
    const apply = () => {
      $('view-service').hidden = view !== 'service';
      $('view-monitor').hidden = view !== 'monitor';
      $('tab-service').classList.toggle('on', view === 'service');
      $('tab-monitor').classList.toggle('on', view === 'monitor');
      $('tab-service').setAttribute('aria-pressed', String(view === 'service'));
      $('tab-monitor').setAttribute('aria-pressed', String(view === 'monitor'));
      window.__svcMonitorActive = view === 'monitor';
    };
    const go = (v) => {
      view = v;
      try { history.replaceState(null, '', v === 'monitor' ? '#monitor' : location.pathname); } catch (_) {}
      apply();
      if (v === 'service') load();
    };
    $('tab-service').addEventListener('click', () => go('service'));
    $('tab-monitor').addEventListener('click', () => go('monitor'));
    window.addEventListener('hashchange', () => { const v = location.hash === '#monitor' ? 'monitor' : 'service'; if (v !== view) { view = v; apply(); if (v === 'service') load(); } });
    apply();
  }

  // --- service snapshot ---
  let last = null, received = 0, busy = false;
  const now = () => last ? Date.parse(last.at) + Math.max(0, performance.now() - received) : Date.now();
  const stateText = { online: '在线', degraded: '降级', offline: '离线' };
  function latencyCell(l) {
    const ms = l.ms == null ? '—' : l.ms.toFixed(l.ms < 1 ? 1 : 0) + ' ms';
    return '<span class="svc-lat"><span class="svc-lat-name">' + esc(l.name) + '</span><b>' + ms + '</b><span class="svc-loss" style="color:' + lc(l.loss || 0) + '">' + (l.loss || 0).toFixed(1) + '%</span></span>';
  }
  // Carriers segment (present only when the snapshot carries it): the
  // three networks are the customer-facing summary of the same samples
  // the per-target latency list shows, so when any carrier has data the
  // segment replaces that list instead of duplicating it under longer
  // target names. A carrier with no figure keeps an em-dash placeholder
  // so the three columns stay aligned within the row.
  const carrierName = { ct: '电信', cu: '联通', cm: '移动' };
  function carrierCell(k, c) {
    if (!c || (c.ms == null && !c.line)) {
      return '<span class="svc-lat svc-lat-none"><span class="svc-lat-name">' + carrierName[k] + '</span><b>—</b></span>';
    }
    const ms = c.ms == null ? '—' : c.ms.toFixed(c.ms < 1 ? 1 : 0) + ' ms';
    return '<span class="svc-lat"><span class="svc-lat-name">' + carrierName[k] + '</span><b>' + ms + '</b>' +
      (c.loss != null ? '<span class="svc-loss" style="color:' + lc(c.loss) + '">' + c.loss.toFixed(1) + '%</span>' : '') +
      (c.line ? '<span class="svc-line">' + esc(String(c.line).toUpperCase()) + '</span>' : '') + '</span>';
  }
  function carriersHTML(n) {
    if (!n.carriers) return '';
    const any = ['ct', 'cu', 'cm'].some((k) => { const c = n.carriers[k]; return c && (c.ms != null || c.line); });
    if (!any) return '';
    return '<span class="svc-nets">' + ['ct', 'cu', 'cm'].map((k) => carrierCell(k, n.carriers[k])).join('') + '</span>';
  }
  function netsHTML(n) {
    const carriers = carriersHTML(n);
    if (carriers) return carriers;
    const lats = (n.latency || []).map(latencyCell).join('');
    return lats ? '<span class="svc-nets">' + lats + '</span>' : '<span class="svc-nets svc-none">暂无网络数据</span>';
  }
  function uptimeHTML(n) {
    const f = (v) => v == null ? '数据不足' : v.toFixed(1) + '%';
    let h = '<span class="svc-up"><span>可用率 7 日 <b>' + f(n.uptime7d) + '</b> · 30 日 <b>' + f(n.uptime30d) + '</b></span>';
    if (Array.isArray(n.uptimeDays) && n.uptimeDays.length) {
      h += '<span class="svc-days">' + n.uptimeDays.map((v) => {
        if (v == null) return '<i style="height:15%;background:var(--pv-track)"></i>';
        const col = v >= 99 ? 'var(--pv-green)' : v >= 95 ? 'var(--pv-yellow)' : v >= 90 ? 'var(--pv-orange)' : 'var(--pv-red)';
        return '<i title="' + v.toFixed(1) + '%" style="height:' + Math.max(15, v) + '%;background:' + col + '"></i>';
      }).join('') + '</span>';
    }
    return h + '</span>';
  }
  function draw() {
    if (!last) return;
    const s = last.summary || { total: 0, online: 0, state: 'ok' };
    const banner = $('svc-banner');
    banner.className = 'svc-banner ' + s.state;
    banner.innerHTML = '<strong>' + ({ ok: '全部服务正常', partial: '部分服务异常', major: '大面积服务异常' }[s.state] || '服务状态') + '</strong><span>' + s.online + ' / ' + s.total + ' 节点在线</span>';
    $('svc-note').textContent = last.note || '';
    $('svc-note').hidden = !last.note;
    const groups = new Map();
    for (const n of last.nodes || []) {
      const key = n.region || '';
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(n);
    }
    let h = '';
    for (const [region, ns] of groups) {
      const on = ns.filter((n) => n.state === 'online').length;
      h += '<section class="svc-group"><h2><span>' + esc(region || '未分组') + '</span><small>' + on + '/' + ns.length + ' 在线</small></h2>' + ns.map((n) =>
        '<div class="svc-row"><span class="svc-id"><span class="svc-dot ' + n.state + '"></span><span class="svc-flag">' + flag(n.flag) + '</span><span class="svc-name">' + flagText(n.name) + '</span><span class="svc-state">' + (stateText[n.state] || n.state) + '</span></span>' + netsHTML(n) + uptimeHTML(n) + '</div>'
      ).join('') + '</section>';
    }
    $('svc-groups').innerHTML = h || '<div class="pv-empty">暂无公开节点</div>';
    $('svc-at').textContent = new Date(last.at).toLocaleTimeString();
    markStale();
  }
  function markStale() {
    if (!last) return;
    $('svc-stale').hidden = !(last.stale || now() - Date.parse(last.at) > 120000);
  }
  async function load() {
    if (busy || document.hidden || (!window.SVC_ONLY && view !== 'service')) return;
    busy = true;
    const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch('/status/service', { signal: controller.signal });
      if (!response.ok) throw new Error('unavailable');
      const data = await response.json();
      if (!Array.isArray(data.nodes) || !Number.isFinite(Date.parse(data.at))) throw new Error('invalid data');
      last = data; received = performance.now();
      $('svc-error').hidden = true;
      draw();
    } catch (_) {
      if (!last) { $('svc-error').hidden = false; $('svc-groups').innerHTML = ''; }
      else markStaleForce();
    } finally { clearTimeout(timeout); busy = false; }
  }
  function markStaleForce() { $('svc-stale').hidden = false; }
  $('svc-retry').addEventListener('click', load);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) load(); });
  load();
  setInterval(load, 30000);
  setInterval(markStale, 5000);
})();
