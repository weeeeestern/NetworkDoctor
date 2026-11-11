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

  const scenarioName = (i) => i.scenario_name || i.scenario || i.rule_id || i.incident_id;

  // Chip colour per confidence; the Korean label comes from the API.
  const CONFIDENCE_CLS = { high: 'done', medium: 'warning', low: 'failed' };
  const confidenceBadge = (i) => (i.confidence_name ? badge(`신뢰도 ${i.confidence_name}`, CONFIDENCE_CLS[i.confidence] || 'neutral') : '');

  const STATUS_KO = { firing: '진행 중', resolved: '복구됨' };
  const statusBadge = (st) => badge(STATUS_KO[st] || st, st === 'firing' ? 'firing' : st === 'resolved' ? 'resolved' : 'neutral');

  // The cause line under the symptom, by investigation state. The label is
  // only spelled out when the conclusion is not a plain likely cause.
  function causeCell(i) {
    const pending = { queued: '원인 분석 중…', running: '원인 분석 중…' }[i.holmes_status];
    if (pending) return `<div class="cause-line pending">${pending}</div>`;
    const h = i.headline || {};
    if (h.cause) {
      const prefix = h.cause_label && h.cause_label !== '추정 원인' ? `${h.cause_label}: ` : '';
      return `<div class="cause-line">→ ${esc(prefix)}${esc(h.cause)}</div>`;
    }
    return i.holmes_status === 'failed' ? '<div class="cause-line pending">원인 분석 실패</div>' : '';
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
        <td>${statusBadge(i.alert_status)}</td>
        <td>
          <div class="cell-main">${esc((i.headline && i.headline.symptom) || scenarioName(i))}</div>
          ${causeCell(i)}
          <div class="cell-sub">${esc(scenarioName(i))} ${i.holmes_status === 'done' ? confidenceBadge(i) : ''}</div>
          ${group.length ? `<div class="group-line">└ 함께 묶인 알림 ${group.length}건: ${esc(group.map((g) => (g.headline && g.headline.symptom) || scenarioName(g)).join(', '))}</div>` : ''}
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

  const HOLMES_KO = {
    done: '분석 완료', running: '분석 중', queued: '분석 대기', failed: '분석 실패',
    skipped: '분석 생략', grouped: '그룹으로 분석', unavailable: '그룹 분석 없음',
  };
  const holmesName = (st) => HOLMES_KO[st] || '분석 안 함';
  const holmesBadge = (st) => badge(holmesName(st), ['done', 'running', 'queued', 'failed'].includes(st) ? st : 'neutral');

  // The diagnosis shown for this incident: its own, or its group's shared
  // one (the server resolves it; older servers only send holmes_result).
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

  // One evidence / exclusion / action entry of the Holmes result, which the
  // schema allows as either a string or a {source|hypothesis, observation|reason} map.
  const itemParts = (it) => {
    if (it && typeof it === 'object') {
      return {
        head: it.hypothesis || it.source || it.metric_or_tool || '',
        body: it.observation || it.reason || JSON.stringify(it),
      };
    }
    return { head: '', body: String(it ?? '') };
  };
  const hasItems = (v) => Array.isArray(v) && v.length > 0;

  const evidenceList = (items, cls) => items.map((it) => {
    const { head, body } = itemParts(it);
    return `<div class="ev-item ${cls}">${esc(body)}${head ? `<div class="ev-src mono">${esc(head)}</div>` : ''}</div>`;
  }).join('');

  function summaryCard(inc, analysis, grafanaURL) {
    const h = inc.headline || {};
    const r = analysis.result;
    const state = analysis.source_holmes_status;
    const waiting = { queued: '원인 분석 대기 중…', running: '원인 분석 중…' }[state];
    let causeBlock;
    if (h.cause) {
      causeBlock = `<div class="sum-k">${analysis.previous ? '이전 분석 · ' : ''}${esc(h.cause_label)}</div><div class="sum-v cause">${esc(h.cause)}</div>`;
    } else {
      const msg = analysis.unavailable ? '함께 묶인 대표 인시던트의 분석을 찾을 수 없습니다'
        : waiting || (state === 'failed' ? '원인 분석에 실패했습니다' : '아직 분석하지 않았습니다');
      causeBlock = `<div class="sum-k">추정 원인</div><div class="sum-v pending">${esc(msg)}</div>`;
    }
    return `
      <div class="card summary-card">
        <div class="sum-k">증상</div>
        <div class="sum-v">${esc(h.symptom || inc.symptom_summary || inc.scenario_name)}</div>
        ${causeBlock}
        ${analysis.previous && waiting ? `<p class="page-sub">새 분석이 끝날 때까지 이전 결과를 보여줍니다 (${esc(holmesName(state))}).</p>` : ''}
        ${analysis.shared ? `<p class="page-sub">함께 묶인 ${incidentLink(analysis.source_incident_id)} 인시던트의 분석 결과입니다.</p>` : ''}
        <div class="sum-foot">
          <div class="badge-row">
            ${r ? confidenceBadge({ confidence: r.confidence, confidence_name: inc.confidence_name }) : ''}
            ${badge(`${fmtTime(inc.starts_at)} 발생`, 'neutral')}
          </div>
          ${grafanaURL ? `<a class="grafana-link" href="${esc(grafanaURL)}" target="_blank" rel="noopener noreferrer">Grafana에서 자세히 보기 →</a>` : ''}
        </div>
      </div>`;
  }

  function analysisCards(inc, analysis) {
    const r = analysis.result;
    if (!r) {
      return !analysis.shared && inc.holmes_error
        ? `<div class="notice err">원인 분석 실패: ${esc(inc.holmes_error)}</div>`
        : '';
    }
    const card = (title, body) => `
      <div class="card">
        <div class="card-head"><h2 class="card-title">${title}</h2></div>
        <div class="card-body">${body}</div>
      </div>`;
    const out = [];
    // Without summary.cause the headline already is root_cause.
    const rc = String(r.root_cause || '').trim();
    if (rc && rc !== (inc.headline || {}).cause) out.push(card('① 원인 분석', `<p class="rc-text">${esc(rc)}</p>`));
    if (hasItems(r.trigger_evidence) || hasItems(r.supporting_evidence)) {
      out.push(card('② 근거', `
        ${hasItems(r.trigger_evidence) ? `<div class="section-sub">알림을 일으킨 신호</div>${evidenceList(r.trigger_evidence, 'trigger')}` : ''}
        ${hasItems(r.supporting_evidence) ? `<div class="section-sub">함께 움직인 신호</div>${evidenceList(r.supporting_evidence, 'support')}` : ''}`));
    }
    if (hasItems(r.excluded_alternatives)) {
      out.push(card('③ 배제한 원인', r.excluded_alternatives.map((it) => {
        const { head, body } = itemParts(it);
        return `<div class="exc-row">${head ? `<s class="exc-name">${esc(head)}</s>` : ''}<span>${esc(body)}</span></div>`;
      }).join('')));
    }
    if (hasItems(r.recommended_actions) || hasItems(r.additional_checks)) {
      const li = (items) => items.map((it) => `<li>${esc(itemParts(it).body)}</li>`).join('');
      out.push(card('④ 권장 조치', `
        ${hasItems(r.recommended_actions) ? `<ol class="actions">${li(r.recommended_actions)}</ol>` : ''}
        ${hasItems(r.additional_checks) ? `<div class="section-sub">추가로 확인하면 좋은 것</div><ul class="plain">${li(r.additional_checks)}</ul>` : ''}`));
    }
    return out.join('');
  }

  // One labelled stat chip of the investigation cost row.
  const rcStat = (label, value, sub = '') => `
    <div class="rc-stat">
      <div class="k">${esc(label)}</div>
      <div class="v">${esc(value)}${sub ? ` <span class="sub">${esc(sub)}</span>` : ''}</div>
    </div>`;

  function detailsFold(inc, open) {
    const labels = Object.entries(inc.alert_labels || {}).sort(([a], [b]) => a.localeCompare(b));
    const evidences = inc.evidence_metrics || [];
    const sourceURL = safeHTTPURL(inc.generator_url);
    const correlation = inc.correlation_id && inc.correlation_id !== inc.incident_id
      ? `${incidentLink(inc.correlation_id)}에 묶임`
      : (inc.correlated_incidents || []).length
        ? `대표 인시던트 · ${inc.correlated_incidents.map(incidentLink).join(', ')}`
        : '';
    const meta = [
      ['룰 / 시나리오', `${esc(inc.rule_id || '-')} / ${esc(inc.scenario || '-')}`],
      ['클러스터', esc(inc.cluster || '-')],
      ['발생', fmtTime(inc.starts_at)],
      ['복구', inc.ends_at ? fmtTime(inc.ends_at) : '-'],
      ['노드', esc((inc.affected_nodes || []).join(', ') || '-')],
      ['서비스', esc((inc.affected_services || []).join(', ') || '-')],
      ['알림 요약', esc(inc.symptom_summary || '-')],
      ['알림 수신', `${esc(inc.delivery_count ?? '-')}회`],
      ['이 인시던트의 분석', esc(holmesName(inc.holmes_status))],
    ];
    if (correlation) meta.push(['상관 그룹', correlation]);
    const cost = [
      inc.holmes_model ? rcStat('모델', inc.holmes_model) : '',
      inc.holmes_tool_calls ? rcStat('도구 호출', `${inc.holmes_tool_calls}회`) : '',
      inc.holmes_total_tokens ? rcStat('토큰', fmtTok(inc.holmes_total_tokens), `입력 ${fmtTok(inc.holmes_prompt_tokens || 0)} · 출력 ${fmtTok(inc.holmes_completion_tokens || 0)}`) : '',
      inc.holmes_cost_usd ? rcStat('비용', fmtCost(inc.holmes_cost_usd)) : '',
    ].join('');
    return `
      <details class="card fold"${open ? ' open' : ''}>
        <summary>상세 정보</summary>
        <div class="card-body">
          <div class="meta-grid">
            ${meta.map(([k, v]) => `<div class="meta-item"><div class="k">${k}</div><div class="v">${v}</div></div>`).join('')}
          </div>
          ${cost ? `<div class="section-sub">분석 비용</div><div class="rc-stats">${cost}</div>` : ''}
          ${inc.holmes_error ? `<p class="notice err">${esc(inc.holmes_error)}</p>` : ''}
          ${evidences.length ? `<div class="section-sub">NetworkDoctor 수집 지표</div>
            ${evidences.map((e) => `
              <div class="evidence-item">
                <b>${esc(e.metric)}</b>: ${esc(e.observation)}
                <code class="evidence-q">${esc(e.query)}</code>
              </div>`).join('')}` : ''}
          ${labels.length ? `<div class="section-sub">알림 레이블</div>
            <table class="kv-table"><tbody>
              ${labels.map(([k, v]) => `<tr><td class="mono">${esc(k)}</td><td class="mono">${esc(v)}</td></tr>`).join('')}
            </tbody></table>` : ''}
          <div class="section-sub fold-links">
            <a href="/incidents/${esc(encodeURIComponent(inc.incident_id))}/report.md" target="_blank" rel="noopener noreferrer">Markdown 레포트 열기</a>
            ${sourceURL ? `<a href="${esc(sourceURL)}" target="_blank" rel="noopener noreferrer">원본 쿼리 보기</a>` : ''}
          </div>
        </div>
      </details>`;
  }

  async function renderDetail(id, polling = false) {
    clearTimeout(detailPollTimer);
    if (!polling) view.innerHTML = '<div class="loading">인시던트를 불러오는 중…</div>';
    let inc;
    try {
      inc = await getJSON(`/incidents/${encodeURIComponent(id)}`);
    } catch (err) {
      if (detailID !== id) return;
      if (polling) {
        const flash = document.getElementById('flash');
        if (flash) flash.innerHTML = `<div class="notice err">분석 상태를 새로 고치지 못했습니다: ${esc(err.message)}. 다시 시도합니다…</div>`;
        detailPollTimer = setTimeout(() => { if (detailID === id) renderDetail(id, true); }, 2500);
        return;
      }
      view.innerHTML = `
        <a class="back-link" href="#/">← 인시던트 목록</a>
        <div class="notice err">인시던트를 불러오지 못했습니다: ${esc(err.message)}</div>`;
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
    // Keep the fold open across polling re-renders.
    const foldOpen = polling && Boolean(view.querySelector('details.fold')?.open);

    view.innerHTML = `
      <a class="back-link" href="#/">← 인시던트 목록</a>
      <div class="detail-head">
        <div>
          <h1 class="detail-title">${esc(inc.scenario_name || inc.incident_id)}</h1>
          <div class="badge-row">
            ${statusBadge(inc.alert_status)}
            ${holmesBadge(analysis.source_holmes_status)}
            <span class="page-sub mono">${esc(inc.incident_id)}</span>
          </div>
        </div>
        <div class="btn-row">
          <button class="btn primary" id="btn-pdf">PDF 다운로드</button>
          <button class="btn" id="btn-holmes" ${waitingToStart ? 'disabled' : ''}>재분석</button>
        </div>
      </div>
      <div id="flash">${waitingToStart ? '<div class="notice ok">재분석을 요청했습니다. 시작을 기다리는 중…</div>' : ''}</div>

      ${summaryCard(inc, analysis, grafanaURL)}
      ${analysisCards(inc, analysis)}
      ${detailsFold(inc, foldOpen)}`;

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
    btn.textContent = '생성 중…';
    try {
      const res = await fetch(`/incidents/${encodeURIComponent(inc.incident_id)}/report.md`);
      if (!res.ok) throw new Error(`레포트를 가져오지 못했습니다 (${res.status})`);
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
      flash.innerHTML = `<div class="notice err">PDF 생성 실패: ${esc(err.message)}</div>`;
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
