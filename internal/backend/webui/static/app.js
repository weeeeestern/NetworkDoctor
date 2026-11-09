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
    if (s < 60) return '방금 전';
    if (s < 3600) return `${Math.floor(s / 60)}분 전`;
    if (s < 86400) return `${Math.floor(s / 3600)}시간 전`;
    return `${Math.floor(s / 86400)}일 전`;
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

  const cardHead = (title, sub) => `
    <div class="card-head">
      <h2 class="card-title">${esc(title)}</h2>
      ${sub ? `<span class="page-sub">${sub}</span>` : ''}
    </div>`;

  // ---------- list view ----------

  // Korean display names of the scenario skills (rule_id order).
  const SCENARIO_KO = {
    'network-congestion': '네트워크 혼잡',
    'kernel-network-bottleneck': '커널 네트워크 병목',
    'conntrack-exhaustion': 'conntrack 고갈',
    'coredns-degradation': 'CoreDNS 성능 저하',
    'dns-conntrack-correlation': 'DNS·conntrack 연쇄 장애',
    'node-localized-failure': '특정 노드 장애',
    'application-induced-network-bottleneck': '애플리케이션발 네트워크 병목',
    'networkpolicy-misconfiguration': 'NetworkPolicy 설정 오류',
  };
  const scenarioKo = (i) => SCENARIO_KO[i.scenario] || i.scenario || i.rule_id || i.incident_id;

  const CONFIDENCE_KO = { high: ['높음', 'done'], medium: ['보통', 'warning'], low: ['낮음', 'failed'] };
  const confidenceBadge = (c) => (CONFIDENCE_KO[c] ? badge(`신뢰도 ${CONFIDENCE_KO[c][0]}`, CONFIDENCE_KO[c][1]) : '');

  const STATUS_KO = { firing: '진행 중', resolved: '복구됨' };
  const statusBadgeKo = (st) => badge(STATUS_KO[st] || st, st === 'firing' ? 'firing' : st === 'resolved' ? 'resolved' : 'neutral');

  // The cause line under the symptom, by investigation state.
  function causeCell(i) {
    if (i.holmes_status === 'done' && i.cause) {
      const prefix = i.investigation_status === 'excluded' ? '이 시나리오 아님: '
        : i.investigation_status === 'inconclusive' ? '원인 불확실: ' : '';
      return `<div class="cause-line">→ ${esc(prefix)}${esc(i.cause)}</div>`;
    }
    const msg = { queued: '원인 분석 중…', running: '원인 분석 중…', failed: '원인 분석 실패' }[i.holmes_status];
    return msg ? `<div class="cause-line pending">${msg}</div>` : '';
  }

  async function renderList() {
    view.innerHTML = '<div class="loading">인시던트를 불러오는 중…</div>';
    let incidents;
    try {
      const data = await getJSON('/incidents');
      incidents = data.incidents || [];
    } catch (err) {
      view.innerHTML = `<div class="notice err">인시던트를 불러오지 못했습니다: ${esc(err.message)}</div>`;
      return;
    }

    incidents.sort((a, b) => new Date(b.starts_at) - new Date(a.starts_at));

    // Group members fold under their primary; a member whose primary is not
    // in the list (e.g. filtered out) stands as its own row.
    const isMember = (i) => i.correlation_id && i.correlation_id !== i.incident_id;
    const primaries = incidents.filter((i) => !isMember(i));
    const primaryIds = new Set(primaries.map((i) => i.incident_id));
    const members = {};
    for (const i of incidents) {
      if (isMember(i) && primaryIds.has(i.correlation_id)) (members[i.correlation_id] ||= []).push(i);
    }
    const rowsAll = incidents.filter((i) => !isMember(i) || !primaryIds.has(i.correlation_id));

    const firing = rowsAll.filter((i) => i.alert_status === 'firing').length;
    const investigated = rowsAll.filter((i) => i.holmes_status === 'done').length;

    const filtered = listState.filter === 'all'
      ? rowsAll
      : rowsAll.filter((i) => i.alert_status === listState.filter);

    const pages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
    listState.page = Math.min(listState.page, pages);
    const start = (listState.page - 1) * PAGE_SIZE;
    const pageItems = filtered.slice(start, start + PAGE_SIZE);

    const rows = pageItems.map((i) => {
      const group = members[i.incident_id] || [];
      return `
      <tr data-id="${esc(i.incident_id)}">
        <td>${statusBadgeKo(i.alert_status)}</td>
        <td>
          <div class="cell-main">${esc(i.symptom_summary || scenarioKo(i))}</div>
          ${causeCell(i)}
          <div class="cell-sub">${esc(scenarioKo(i))} ${i.holmes_status === 'done' ? confidenceBadge(i.confidence) : ''}</div>
          ${group.length ? `<div class="group-line">└ 함께 묶인 알림 ${group.length}건: ${esc(group.map((g) => g.symptom_summary || scenarioKo(g)).join(', '))}</div>` : ''}
        </td>
        <td>${esc((i.affected_nodes || []).join(', ') || '-')}</td>
        <td>
          <div>${fmtTime(i.starts_at)}</div>
          <div class="cell-sub">${ago(i.starts_at)}</div>
        </td>
      </tr>`;
    }).join('');

    const pageBtns = Array.from({ length: pages }, (_, k) => k + 1).map((p) =>
      `<button class="page-btn ${p === listState.page ? 'active' : ''}" data-page="${p}">${p}</button>`
    ).join('');

    const tabs = [['all', '전체'], ['firing', '진행 중'], ['resolved', '복구됨']].map(([key, label]) =>
      `<button class="tab ${listState.filter === key ? 'active' : ''}" data-filter="${key}">${label}</button>`
    ).join('');

    view.innerHTML = `
      <div class="page-head">
        <div>
          <h1 class="page-title">인시던트</h1>
          <div class="page-sub">NetworkDoctor가 감지한 네트워크 장애</div>
        </div>
      </div>

      <div class="dash-grid dash-grid-rev">
        <div class="stat-col">
          <div class="card stat-card">
            <div class="stat-k">진행 중</div>
            <div class="stat-v ${firing ? 'firing' : ''}">${firing}</div>
          </div>
          <div class="card stat-card">
            <div class="stat-k">원인 분석 완료</div>
            <div class="stat-v">${investigated}<span class="stat-of"> / ${rowsAll.length}</span></div>
          </div>
        </div>
        <div class="card chart-card">
          ${cardHead('최근 7일 현황', '')}
          <div class="chart-body">
            ${barChart(rowsAll)}
            <div class="legend legend-row">
              <div class="legend-item"><span class="dot" style="background:${CHART_NAVY}"></span>진행 중</div>
              <div class="legend-item"><span class="dot" style="background:${CHART_LAVENDER}"></span>복구됨</div>
            </div>
          </div>
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2 class="card-title">인시던트 목록</h2>
          <div class="tabs">${tabs}</div>
        </div>
        ${filtered.length === 0 ? '<div class="empty">인시던트가 없습니다</div>' : `
        <table class="incident-table">
          <thead>
            <tr><th>상태</th><th>증상 → 추정 원인</th><th>노드</th><th>발생</th></tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
        <div class="pagination">
          <span class="page-info">${filtered.length}건 중 ${start + 1}–${start + pageItems.length}</span>
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

  let detailPollTimer;
  let detailID = null;
  let requestedAnalysis = null;

  const analysisFor = (inc) => inc.analysis || {
    source_incident_id: inc.incident_id,
    source_holmes_status: inc.holmes_status,
    previous: Boolean(inc.holmes_result && ['queued', 'running', 'failed'].includes(inc.holmes_status)),
    result: inc.holmes_result,
  };

  const safeHTTPURL = (value) => {
    if (typeof value !== 'string') return '';
    try {
      const url = new URL(value);
      return ['https:', 'http:'].includes(url.protocol) ? url.href : '';
    } catch (_) { return ''; }
  };

  const incidentLink = (id) => `<a href="#/incident/${esc(encodeURIComponent(id))}" class="mono">${esc(id)}</a>`;

  function analysisCaption(analysis) {
    return analysis.shared
      ? `<p class="page-sub">Related group analysis · source ${incidentLink(analysis.source_incident_id)}</p>`
      : '';
  }

  function diagnosisSummary(inc, analysis) {
    const r = analysis.result;
    const cause = r && ((r.summary && r.summary.cause) || r.root_cause);
    const state = analysis.source_holmes_status || 'not run';
    const conclusion = r && r.investigation_status;
    const label = conclusion === 'excluded' ? 'Scenario excluded'
      : conclusion === 'inconclusive' ? 'Cause inconclusive'
      : conclusion === 'confirmed' ? 'Cause confirmed' : 'Analysis result';
    const pending = analysis.unavailable ? 'Related group analysis unavailable.'
      : ({queued: 'Analysis queued.', running: 'Analysis in progress.', failed: 'Analysis failed.', skipped: 'Analysis not run.'}[state] || 'No analysis result yet.');
    return `<div class="card">
      <div class="card-head"><h2 class="card-title">Symptom → cause</h2></div>
      <div class="card-body">
        <div class="section-sub">Symptom</div><div class="rc-text">${esc(inc.symptom_summary || inc.scenario || '-')}</div>
        <div class="rc-stats">
          ${rcStat('Occurred', fmtTime(inc.starts_at))}
          ${rcStat('Analysis', state)}
          ${r && r.confidence ? rcStat('Confidence', r.confidence, confidenceCls(r.confidence)) : ''}
        </div>
        ${analysisCaption(analysis)}
        ${cause ? `<div class="section-sub">${analysis.previous ? 'Previous analysis · ' : ''}${esc(label)}</div><div class="rc-text">${esc(cause)}</div>` : `<p class="page-sub">${esc(pending)}</p>`}
        ${analysis.previous ? `<p class="page-sub">Current analysis is ${esc(state)}. The earlier finding is shown until a new result is available.</p>` : ''}
      </div></div>`;
  }

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

  function rootCauseCard(inc, analysis) {
    const r = analysis.result;
    if (!r) {
      const msg = analysis.unavailable
        ? '<p class="page-sub">Related group analysis unavailable.</p>'
        : !analysis.shared && inc.holmes_error
        ? `<div class="notice err">Investigation failed: ${esc(inc.holmes_error)}</div>`
        : `<p class="page-sub">No investigation result yet (status: ${esc(analysis.source_holmes_status || 'not run')}).</p>`;
      return `<div class="card"><div class="card-head"><h2 class="card-title">Root cause</h2></div>
        <div class="card-body">${msg}</div></div>`;
    }
    const st = r.investigation_status;
    return `
      <div class="card">
        <div class="card-head"><h2 class="card-title">${analysis.previous ? 'Previous analysis' : 'Analysis details'}</h2></div>
        <div class="card-body">
          <div class="rc-stats">
            ${st ? rcStat('status', st, st === 'confirmed' ? 'good' : st === 'excluded' ? 'warn' : '') : ''}
            ${r.confidence ? rcStat('confidence', r.confidence, confidenceCls(r.confidence)) : ''}
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

  async function renderDetail(id, polling = false) {
    clearTimeout(detailPollTimer);
    if (!polling) view.innerHTML = '<div class="loading">Loading incident…</div>';
    let inc;
    try {
      inc = await getJSON(`/incidents/${encodeURIComponent(id)}`);
    } catch (err) {
      if (detailID !== id) return;
      if (polling) {
        const flash = document.getElementById('flash');
        if (flash) flash.innerHTML = `<div class="notice err">Could not refresh analysis: ${esc(err.message)}. Retrying…</div>`;
        detailPollTimer = setTimeout(() => { if (detailID === id) renderDetail(id, true); }, 2500);
        return;
      }
      view.innerHTML = `
        <a class="back-link" href="#/">← Incidents</a>
        <div class="notice err">Failed to load incident: ${esc(err.message)}</div>`;
      return;
    }

    if (detailID !== id) return;
    if (requestedAnalysis && requestedAnalysis.id === id &&
        ((inc.holmes_attempts || 0) > requestedAnalysis.attempt || ['queued', 'running'].includes(inc.holmes_status))) {
      requestedAnalysis = null;
    }
    const waitingToStart = requestedAnalysis && requestedAnalysis.id === id;
    const analysis = analysisFor(inc);
    const grafanaURL = safeHTTPURL(inc.links && inc.links.grafana);
    const sourceURL = safeHTTPURL(inc.generator_url);

    const title = (inc.alert_labels && inc.alert_labels.alertname) || inc.scenario || inc.incident_id;
    const labels = Object.entries(inc.alert_labels || {}).sort(([a], [b]) => a.localeCompare(b));
    const evidences = inc.evidence_metrics || [];

    const correlation = inc.correlation_id && inc.correlation_id !== inc.incident_id
      ? `grouped into ${incidentLink(inc.correlation_id)}`
      : (inc.correlated_incidents || []).length
        ? `primary of ${inc.correlated_incidents.map(incidentLink).join(', ')}`
        : '';

    view.innerHTML = `
      <a class="back-link" href="#/">← Incidents</a>
      <div class="detail-head">
        <div>
          <h1 class="detail-title">${esc(title)}</h1>
          <div class="badge-row">
            ${severityBadge(inc.severity)}
            ${statusBadge(inc.alert_status)}
            ${holmesBadge(analysis.source_holmes_status)}
            <span class="page-sub mono">${esc(inc.incident_id)}</span>
          </div>
        </div>
        <div class="btn-row">
          <button class="btn primary" id="btn-pdf">Download PDF</button>
          ${grafanaURL ? `<a class="btn" href="${esc(grafanaURL)}" target="_blank" rel="noopener noreferrer">View in Grafana</a>` : ''}
          <button class="btn" id="btn-holmes" ${waitingToStart ? 'disabled' : ''}>Reanalyze</button>
        </div>
      </div>
      <div id="flash">${waitingToStart ? '<div class="notice ok">Reanalysis queued. Waiting to start…</div>' : ''}</div>

      ${diagnosisSummary(inc, analysis)}

      ${rootCauseCard(inc, analysis)}

      <details>
      <summary class="section-sub">Technical details</summary>
      <div class="card">
        <div class="card-head"><h2 class="card-title">Incident information</h2></div>
        <div class="card-body">
          <p><a class="btn" href="/incidents/${esc(encodeURIComponent(inc.incident_id))}/report.md" target="_blank" rel="noopener noreferrer">Markdown report</a>
          ${sourceURL ? `<a class="btn" href="${esc(sourceURL)}" target="_blank" rel="noopener noreferrer">Source query</a>` : ''}</p>
          <div class="meta-grid">
            <div class="meta-item"><div class="k">Rule / scenario</div><div class="v">${esc(inc.rule_id || '-')} / ${esc(inc.scenario || '-')}</div></div>
            <div class="meta-item"><div class="k">Cluster</div><div class="v">${esc(inc.cluster || '-')}</div></div>
            <div class="meta-item"><div class="k">Started</div><div class="v">${fmtTime(inc.starts_at)}</div></div>
            <div class="meta-item"><div class="k">Ended</div><div class="v">${inc.ends_at ? fmtTime(inc.ends_at) : '-'}</div></div>
            <div class="meta-item"><div class="k">Nodes</div><div class="v">${esc((inc.affected_nodes || []).join(', ') || '-')}</div></div>
            <div class="meta-item"><div class="k">Services</div><div class="v">${esc((inc.affected_services || []).join(', ') || '-')}</div></div>
            <div class="meta-item"><div class="k">Recovery</div><div class="v">${esc(inc.recovery_status || '-')}</div></div>
            <div class="meta-item"><div class="k">Deliveries</div><div class="v">${esc(inc.delivery_count ?? '-')}</div></div>
            <div class="meta-item"><div class="k">This incident's investigation</div><div class="v">${esc(inc.holmes_status || 'not run')}</div></div>
            ${correlation ? `<div class="meta-item"><div class="k">Correlation</div><div class="v">${correlation}</div></div>` : ''}
          </div>
          <div class="rc-stats">
            ${inc.holmes_model ? rcStat('Model', inc.holmes_model) : ''}
            ${inc.holmes_tool_calls ? rcStat('Tool calls', inc.holmes_tool_calls) : ''}
            ${inc.holmes_total_tokens ? rcStat('Tokens', fmtTok(inc.holmes_total_tokens), '', `${fmtTok(inc.holmes_prompt_tokens || 0)} in · ${fmtTok(inc.holmes_completion_tokens || 0)} out`) : ''}
            ${inc.holmes_cost_usd ? rcStat('Cost', fmtCost(inc.holmes_cost_usd)) : ''}
          </div>
          ${inc.holmes_error ? `<p class="notice err">${esc(inc.holmes_error)}</p>` : ''}
        </div>
      </div>

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
      </div>` : ''}
      </details>`;

    document.getElementById('btn-pdf').addEventListener('click', () => downloadPDF(inc));
    document.getElementById('btn-holmes').addEventListener('click', () => reinvestigate(inc));
    if (waitingToStart || ['queued', 'running'].includes(analysis.source_holmes_status)) {
      detailPollTimer = setTimeout(() => { if (detailID === id) renderDetail(id, true); }, 2500);
    }
  }

  async function reinvestigate(inc) {
    const id = inc.incident_id;
    const flash = document.getElementById('flash');
    const btn = document.getElementById('btn-holmes');
    btn.disabled = true;
    try {
      const res = await fetch(`/incidents/${encodeURIComponent(id)}/holmes`, { method: 'POST' });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || res.statusText);
      if (detailID === id) {
        requestedAnalysis = { id, attempt: inc.holmes_attempts || 0 };
        await renderDetail(id);
      }
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
        pagebreak: { mode: ['css', 'legacy'] },
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
    clearTimeout(detailPollTimer);
    const hash = location.hash || '#/';
    const m = hash.match(/^#\/incident\/(.+)$/);
    detailID = m ? decodeURIComponent(m[1]) : null;
    if (requestedAnalysis && requestedAnalysis.id !== detailID) requestedAnalysis = null;
    if (m) renderDetail(detailID);
    else renderList();
    window.scrollTo(0, 0);
  }

  window.addEventListener('hashchange', route);
  route();
})();
