/* NetworkDoctor incident dashboard.
 *
 * Hash routes:
 *   #/                 incident list (10 per page, status filter)
 *   #/incident/<id>    incident detail + PDF download
 */
(() => {
  'use strict';

  const PAGE_SIZE = 10;
  const view = document.getElementById('view');

  // list UI state kept across re-renders
  const listState = { filter: 'all', page: 1 };

  // ---------- helpers ----------

  const esc = (s) =>
    String(s ?? '').replace(/[&<>"']/g, (c) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));

  const fmtTime = (iso) => {
    if (!iso) return '-';
    const d = new Date(iso);
    const pad = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
           `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  };

  const ago = (iso) => {
    if (!iso) return '';
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 60) return `${Math.floor(s)}s ago`;
    if (s < 3600) return `${Math.floor(s / 60)}m ago`;
    if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
    return `${Math.floor(s / 86400)}d ago`;
  };

  const badge = (text, cls) =>
    text ? `<span class="badge ${cls}">${esc(text)}</span>` : '';

  const severityBadge = (sev) =>
    badge(sev || 'none', ['critical', 'warning', 'info'].includes(sev) ? sev : 'neutral');

  const statusBadge = (st) =>
    badge(st, st === 'firing' ? 'firing' : st === 'resolved' ? 'resolved' : 'neutral');

  const holmesBadge = (st) => {
    if (!st) return '<span class="badge neutral">not run</span>';
    const cls = ['done', 'running', 'queued', 'failed'].includes(st) ? st : 'neutral';
    return badge(st, cls);
  };

  const fmtTok = (n) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n));
  const fmtCost = (v) => `$${v >= 0.1 ? v.toFixed(2) : v.toFixed(3)}`;

  const getJSON = async (url) => {
    const res = await fetch(url);
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
    return body;
  };

  // ---------- charts (inline SVG, no deps) ----------

  const CHART_NAVY = 'var(--accent)';
  const CHART_LAVENDER = '#55639c';

  // Stacked bars of incident starts per day for the last 7 days.
  function barChart(incidents) {
    const days = [];
    for (let k = 6; k >= 0; k--) {
      const d = new Date();
      d.setHours(0, 0, 0, 0);
      d.setDate(d.getDate() - k);
      days.push({ t: d.getTime(), label: `${d.getMonth() + 1}/${d.getDate()}`, firing: 0, resolved: 0 });
    }
    for (const i of incidents) {
      const t = new Date(i.starts_at); t.setHours(0, 0, 0, 0);
      const day = days.find((d) => d.t === t.getTime());
      if (day) day[i.alert_status === 'resolved' ? 'resolved' : 'firing']++;
    }
    const max = Math.max(1, ...days.map((d) => d.firing + d.resolved));
    const W = 560, H = 196, base = 158, area = 122, bw = 42, step = (W - 60) / 7;
    const bars = days.map((d, i) => {
      const x = 30 + i * step + (step - bw) / 2;
      const hF = (d.firing / max) * area;
      const hR = (d.resolved / max) * area;
      const total = d.firing + d.resolved;
      return `
        ${hR ? `<rect x="${x}" y="${base - hF - hR}" width="${bw}" height="${hR}" rx="4" fill="${CHART_LAVENDER}"/>` : ''}
        ${hF ? `<rect x="${x}" y="${base - hF}" width="${bw}" height="${hF}" rx="4" fill="${CHART_NAVY}"/>` : ''}
        ${total ? `<text x="${x + bw / 2}" y="${base - hF - hR - 7}" text-anchor="middle" font-size="12" font-weight="600" style="fill:var(--text)">${total}</text>` : ''}
        <text x="${x + bw / 2}" y="${base + 20}" text-anchor="middle" font-size="11" style="fill:var(--muted)">${d.label}</text>`;
    }).join('');
    return `<svg viewBox="0 0 ${W} ${H}" class="chart-svg">
      <line x1="24" y1="${base}" x2="${W - 24}" y2="${base}" stroke="var(--border)"/>
      ${bars}</svg>`;
  }

  // Donut with a center total, returns {svg, legend} parts.
  function donut(parts) {
    const total = parts.reduce((s, p) => s + p.count, 0);
    const r = 46, C = 2 * Math.PI * r;
    let offset = C / 4; // start at 12 o'clock
    const rings = parts.filter((p) => p.count > 0).map((p) => {
      const seg = (p.count / total) * C;
      const el = `<circle cx="60" cy="60" r="${r}" fill="none" stroke="${p.color}" stroke-width="15"
        stroke-dasharray="${seg - 1.5} ${C - seg + 1.5}" stroke-dashoffset="${offset}"/>`;
      offset -= seg;
      return el;
    }).join('');
    const svg = `<svg viewBox="0 0 120 120" class="donut-svg">
      ${total ? rings : `<circle cx="60" cy="60" r="${r}" fill="none" stroke="var(--accent-soft)" stroke-width="15"/>`}
      <text x="60" y="58" text-anchor="middle" font-size="24" font-weight="700" style="fill:var(--text)">${total}</text>
      <text x="60" y="74" text-anchor="middle" font-size="9.5" style="fill:var(--muted)">incidents</text></svg>`;
    const legend = parts.map((p) =>
      `<div class="legend-item"><span class="dot" style="background:${p.color}"></span>${esc(p.label)}<b>${p.count}</b></div>`
    ).join('');
    return { svg, legend };
  }

  // Horizontal bars, Mixpanel "per category" style.
  function hbars(entries) {
    const max = Math.max(1, ...entries.map(([, n]) => n));
    return entries.map(([label, n]) => `
      <div class="hbar-row">
        <span class="hbar-label" title="${esc(label)}">${esc(label)}</span>
        <span class="hbar-track"><span class="hbar-fill" style="width:${(n / max) * 100}%"></span></span>
        <span class="hbar-val">${n}</span>
      </div>`).join('');
  }

  const cardHead = (title, sub) => `
    <div class="card-head">
      <h2 class="card-title">${esc(title)}</h2>
      ${sub ? `<span class="page-sub">${sub}</span>` : ''}
    </div>`;

  // ---------- list view ----------

  async function renderList() {
    view.innerHTML = '<div class="loading">Loading incidents…</div>';
    let incidents;
    try {
      const data = await getJSON('/incidents');
      incidents = data.incidents || [];
    } catch (err) {
      view.innerHTML = `<div class="notice err">Failed to load incidents: ${esc(err.message)}</div>`;
      return;
    }

    incidents.sort((a, b) => new Date(b.starts_at) - new Date(a.starts_at));

    const firing = incidents.filter((i) => i.alert_status === 'firing').length;
    const resolved = incidents.filter((i) => i.alert_status === 'resolved').length;
    const investigated = incidents.filter((i) => i.holmes_status === 'done').length;

    const filtered = listState.filter === 'all'
      ? incidents
      : incidents.filter((i) => i.alert_status === listState.filter);

    const pages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
    listState.page = Math.min(listState.page, pages);
    const start = (listState.page - 1) * PAGE_SIZE;
    const pageItems = filtered.slice(start, start + PAGE_SIZE);

    const rows = pageItems.map((i) => `
      <tr data-id="${esc(i.incident_id)}">
        <td>
          <div class="cell-main">${esc(i.scenario || i.rule_id || i.incident_id)}</div>
          <div class="cell-sub mono">${esc(i.incident_id)}</div>
        </td>
        <td>${severityBadge(i.severity)}</td>
        <td>${statusBadge(i.alert_status)}</td>
        <td>
          <div>${esc(i.symptom_summary || '-')}</div>
          <div class="cell-sub">${esc((i.affected_nodes || []).join(', '))}</div>
        </td>
        <td>
          ${holmesBadge(i.holmes_status)}
          ${i.holmes_total_tokens ? `<div class="cell-sub">${fmtTok(i.holmes_total_tokens)} tok${i.holmes_cost_usd ? ` · ${fmtCost(i.holmes_cost_usd)}` : ''}</div>` : ''}
        </td>
        <td>
          <div>${fmtTime(i.starts_at)}</div>
          <div class="cell-sub">${ago(i.starts_at)}</div>
        </td>
      </tr>`).join('');

    const pageBtns = Array.from({ length: pages }, (_, k) => k + 1).map((p) =>
      `<button class="page-btn ${p === listState.page ? 'active' : ''}" data-page="${p}">${p}</button>`
    ).join('');

    const tabs = [['all', 'All'], ['firing', 'Firing'], ['resolved', 'Resolved']].map(([key, label]) =>
      `<button class="tab ${listState.filter === key ? 'active' : ''}" data-filter="${key}">${label}</button>`
    ).join('');

    const sevDonut = donut([
      { label: 'critical', count: incidents.filter((i) => i.severity === 'critical').length, color: '#d87070' },
      { label: 'warning', count: incidents.filter((i) => i.severity === 'warning').length, color: '#cfa14a' },
      { label: 'other', count: incidents.filter((i) => !['critical', 'warning'].includes(i.severity)).length, color: '#8d9cc9' },
    ]);

    const nodeCounts = {};
    for (const i of incidents) for (const n of i.affected_nodes || []) nodeCounts[n] = (nodeCounts[n] || 0) + 1;
    const topNodes = Object.entries(nodeCounts).sort((a, b) => b[1] - a[1]).slice(0, 5);

    const coverage = incidents.length ? Math.round((investigated / incidents.length) * 100) : 0;

    view.innerHTML = `
      <div class="page-head">
        <div>
          <h1 class="page-title">Incidents</h1>
          <div class="page-sub">Network incidents detected by NetworkDoctor</div>
        </div>
      </div>

      <div class="dash-grid">
        <div class="card chart-card">
          ${cardHead('Incidents over time', 'last 7 days')}
          <div class="chart-body">
            ${barChart(incidents)}
            <div class="legend legend-row">
              <div class="legend-item"><span class="dot" style="background:${CHART_NAVY}"></span>firing <b>${firing}</b></div>
              <div class="legend-item"><span class="dot" style="background:${CHART_LAVENDER}"></span>resolved <b>${resolved}</b></div>
            </div>
          </div>
        </div>
        <div class="card chart-card">
          ${cardHead('By severity', '')}
          <div class="chart-body donut-wrap">
            ${sevDonut.svg}
            <div class="legend">${sevDonut.legend}</div>
          </div>
        </div>
      </div>

      <div class="dash-grid dash-grid-rev">
        <div class="card chart-card">
          ${cardHead('Investigated', '')}
          <div class="chart-body kpi-body">
            <div class="kpi-value">${coverage}%</div>
            <div class="page-sub">${investigated} of ${incidents.length} incidents have a completed root-cause investigation</div>
          </div>
        </div>
        <div class="card chart-card">
          ${cardHead('Affected nodes', `top ${topNodes.length}`)}
          <div class="chart-body">
            ${topNodes.length ? hbars(topNodes) : '<div class="page-sub">no node data</div>'}
          </div>
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2 class="card-title">Incident list</h2>
          <div class="tabs">${tabs}</div>
        </div>
        ${filtered.length === 0 ? '<div class="empty">No incidents</div>' : `
        <table>
          <thead>
            <tr>
              <th>Incident</th><th>Severity</th><th>Status</th>
              <th>Symptom</th><th>Investigation</th><th>Started</th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
        <div class="pagination">
          <span class="page-info">${start + 1}–${start + pageItems.length} of ${filtered.length}</span>
          <div class="page-btns">
            <button class="page-btn" data-page="${listState.page - 1}" ${listState.page <= 1 ? 'disabled' : ''}>‹</button>
            ${pageBtns}
            <button class="page-btn" data-page="${listState.page + 1}" ${listState.page >= pages ? 'disabled' : ''}>›</button>
          </div>
        </div>`}
      </div>`;

    view.querySelectorAll('tbody tr').forEach((tr) => {
      tr.addEventListener('click', () => { location.hash = `#/incident/${tr.dataset.id}`; });
    });
    view.querySelectorAll('.tab').forEach((t) => {
      t.addEventListener('click', () => {
        listState.filter = t.dataset.filter;
        listState.page = 1;
        renderList();
      });
    });
    view.querySelectorAll('.page-btn').forEach((b) => {
      b.addEventListener('click', () => {
        const p = Number(b.dataset.page);
        if (p >= 1 && p <= pages && p !== listState.page) { listState.page = p; renderList(); }
      });
    });
  }

  // ---------- detail view ----------

  const holmesList = (title, items) => {
    if (!Array.isArray(items) || items.length === 0) return '';
    const lis = items.map((it) => {
      if (typeof it === 'string') return `<li>${esc(it)}</li>`;
      if (it && typeof it === 'object') {
        const head = it.source || it.hypothesis || it.metric_or_tool || '';
        const body = it.observation || it.reason || '';
        return `<li>${head ? `<b>${esc(head)}</b>: ` : ''}${esc(body || JSON.stringify(it))}</li>`;
      }
      return `<li>${esc(String(it))}</li>`;
    }).join('');
    return `<div class="section-sub">${esc(title)}</div><ul class="plain">${lis}</ul>`;
  };

  // One labelled stat chip of the investigation summary row.
  const rcStat = (label, value, cls = '', sub = '') => `
    <div class="rc-stat">
      <div class="k">${esc(label)}</div>
      <div class="v ${cls}">${esc(value)}${sub ? ` <span class="sub">${esc(sub)}</span>` : ''}</div>
    </div>`;

  const confidenceCls = (c) =>
    c === 'high' ? 'good' : c === 'medium' ? 'warn' : c === 'low' ? 'bad' : '';

  function rootCauseCard(inc) {
    const r = inc.holmes_result;
    if (!r) {
      const msg = inc.holmes_error
        ? `<div class="notice err">Investigation failed: ${esc(inc.holmes_error)}</div>`
        : `<p class="page-sub">No investigation result yet (status: ${esc(inc.holmes_status || 'not run')}).</p>`;
      return `<div class="card"><div class="card-head"><h2 class="card-title">Root cause</h2></div>
        <div class="card-body">${msg}</div></div>`;
    }
    const st = r.investigation_status;
    return `
      <div class="card">
        <div class="card-head"><h2 class="card-title">Root cause</h2></div>
        <div class="card-body">
          <div class="rc-stats">
            ${st ? rcStat('status', st, st === 'confirmed' ? 'good' : st === 'excluded' ? 'warn' : '') : ''}
            ${r.confidence ? rcStat('confidence', r.confidence, confidenceCls(r.confidence)) : ''}
            ${inc.holmes_model ? rcStat('model', inc.holmes_model) : ''}
            ${inc.holmes_tool_calls ? rcStat('tool calls', inc.holmes_tool_calls) : ''}
            ${inc.holmes_total_tokens ? rcStat('tokens', fmtTok(inc.holmes_total_tokens), '', `${fmtTok(inc.holmes_prompt_tokens || 0)} in · ${fmtTok(inc.holmes_completion_tokens || 0)} out`) : ''}
            ${inc.holmes_cost_usd ? rcStat('cost', fmtCost(inc.holmes_cost_usd)) : ''}
          </div>
          <div class="rc-text">${esc(String(r.root_cause || '').trim())}</div>
          ${holmesList('Trigger evidence', r.trigger_evidence)}
          ${holmesList('Supporting evidence', r.supporting_evidence)}
          ${holmesList('Excluded alternatives', r.excluded_alternatives)}
          ${holmesList('Recommended actions', r.recommended_actions)}
          ${holmesList('Additional checks', r.additional_checks)}
        </div>
      </div>`;
  }

  async function renderDetail(id) {
    view.innerHTML = '<div class="loading">Loading incident…</div>';
    let inc;
    try {
      inc = await getJSON(`/incidents/${encodeURIComponent(id)}`);
    } catch (err) {
      view.innerHTML = `
        <a class="back-link" href="#/">← Incidents</a>
        <div class="notice err">Failed to load incident: ${esc(err.message)}</div>`;
      return;
    }

    const title = (inc.alert_labels && inc.alert_labels.alertname) || inc.scenario || inc.incident_id;
    const labels = Object.entries(inc.alert_labels || {}).sort(([a], [b]) => a.localeCompare(b));
    const evidences = inc.evidence_metrics || [];

    const correlation = inc.correlation_id && inc.correlation_id !== inc.incident_id
      ? `grouped into <a href="#/incident/${esc(inc.correlation_id)}" class="mono">${esc(inc.correlation_id)}</a>`
      : (inc.correlated_incidents || []).length
        ? `primary of ${inc.correlated_incidents.map((c) => `<a href="#/incident/${esc(c)}" class="mono">${esc(c)}</a>`).join(', ')}`
        : '';

    view.innerHTML = `
      <a class="back-link" href="#/">← Incidents</a>
      <div class="detail-head">
        <div>
          <h1 class="detail-title">${esc(title)}</h1>
          <div class="badge-row">
            ${severityBadge(inc.severity)}
            ${statusBadge(inc.alert_status)}
            ${holmesBadge(inc.holmes_status)}
            <span class="page-sub mono">${esc(inc.incident_id)}</span>
          </div>
        </div>
        <div class="btn-row">
          <button class="btn primary" id="btn-pdf">Download PDF</button>
          <a class="btn" href="/incidents/${encodeURIComponent(inc.incident_id)}/report.md" target="_blank">Markdown</a>
          <button class="btn" id="btn-holmes">Re-investigate</button>
        </div>
      </div>
      <div id="flash"></div>

      <div class="card">
        <div class="card-head"><h2 class="card-title">Overview</h2></div>
        <div class="card-body">
          <div class="meta-grid">
            <div class="meta-item"><div class="k">Rule / scenario</div><div class="v">${esc(inc.rule_id || '-')} / ${esc(inc.scenario || '-')}</div></div>
            <div class="meta-item"><div class="k">Cluster</div><div class="v">${esc(inc.cluster || '-')}</div></div>
            <div class="meta-item"><div class="k">Started</div><div class="v">${fmtTime(inc.starts_at)}</div></div>
            <div class="meta-item"><div class="k">Ended</div><div class="v">${inc.ends_at ? fmtTime(inc.ends_at) : '-'}</div></div>
            <div class="meta-item"><div class="k">Nodes</div><div class="v">${esc((inc.affected_nodes || []).join(', ') || '-')}</div></div>
            <div class="meta-item"><div class="k">Services</div><div class="v">${esc((inc.affected_services || []).join(', ') || '-')}</div></div>
            <div class="meta-item"><div class="k">Recovery</div><div class="v">${esc(inc.recovery_status || '-')}</div></div>
            <div class="meta-item"><div class="k">Deliveries</div><div class="v">${inc.delivery_count ?? '-'}</div></div>
            ${correlation ? `<div class="meta-item"><div class="k">Correlation</div><div class="v">${correlation}</div></div>` : ''}
          </div>
          ${inc.symptom_summary ? `<p style="margin:14px 0 0">${esc(inc.symptom_summary)}</p>` : ''}
        </div>
      </div>

      ${rootCauseCard(inc)}

      ${evidences.length ? `
      <div class="card">
        <div class="card-head"><h2 class="card-title">Evidence collected by NetworkDoctor</h2></div>
        <div class="card-body">
          ${evidences.map((e) => `
            <div class="evidence-item">
              <b>${esc(e.metric)}</b> — ${esc(e.observation)}
              <code class="evidence-q">${esc(e.query)}</code>
            </div>`).join('')}
        </div>
      </div>` : ''}

      ${labels.length ? `
      <div class="card">
        <div class="card-head"><h2 class="card-title">Alert labels</h2></div>
        <table class="kv-table">
          <tbody>
            ${labels.map(([k, v]) => `<tr><td class="mono">${esc(k)}</td><td class="mono">${esc(v)}</td></tr>`).join('')}
          </tbody>
        </table>
      </div>` : ''}`;

    document.getElementById('btn-pdf').addEventListener('click', () => downloadPDF(inc));
    document.getElementById('btn-holmes').addEventListener('click', () => reinvestigate(inc.incident_id));
  }

  async function reinvestigate(id) {
    const flash = document.getElementById('flash');
    const btn = document.getElementById('btn-holmes');
    btn.disabled = true;
    try {
      const res = await fetch(`/incidents/${encodeURIComponent(id)}/holmes`, { method: 'POST' });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || res.statusText);
      flash.innerHTML = '<div class="notice ok">Investigation queued. Refresh in a bit to see the result.</div>';
    } catch (err) {
      flash.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    } finally {
      btn.disabled = false;
    }
  }

  // Render the server-side Markdown report into #pdf-root and print it to
  // PDF. html2canvas needs the element genuinely visible (see style.css), so
  // the opaque overlay hides the page while it is shown.
  async function downloadPDF(inc) {
    const btn = document.getElementById('btn-pdf');
    const flash = document.getElementById('flash');
    const root = document.getElementById('pdf-root');
    const overlay = document.getElementById('pdf-overlay');
    btn.disabled = true;
    const prev = btn.textContent;
    btn.textContent = 'Generating…';
    try {
      const res = await fetch(`/incidents/${encodeURIComponent(inc.incident_id)}/report.md`);
      if (!res.ok) throw new Error(`failed to fetch report (${res.status})`);
      const md = await res.text();

      overlay.classList.add('show');
      root.innerHTML = marked.parse(md);
      root.style.display = 'block';

      await html2pdf().set({
        margin: [12, 12, 14, 12],
        filename: `networkdoctor-${inc.incident_id}.pdf`,
        image: { type: 'jpeg', quality: 0.95 },
        html2canvas: { scale: 2, useCORS: true },
        jsPDF: { unit: 'mm', format: 'a4', orientation: 'portrait' },
        pagebreak: { mode: ['avoid-all', 'css', 'legacy'] },
      }).from(root).save();
    } catch (err) {
      flash.innerHTML = `<div class="notice err">PDF generation failed: ${esc(err.message)}</div>`;
    } finally {
      root.style.display = '';
      root.innerHTML = '';
      overlay.classList.remove('show');
      btn.disabled = false;
      btn.textContent = prev;
    }
  }

  // ---------- router ----------

  function route() {
    const hash = location.hash || '#/';
    const m = hash.match(/^#\/incident\/(.+)$/);
    if (m) renderDetail(decodeURIComponent(m[1]));
    else renderList();
    window.scrollTo(0, 0);
  }

  window.addEventListener('hashchange', route);
  route();
})();
