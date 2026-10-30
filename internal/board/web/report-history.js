'use strict'

// The history of the period: a row per run with its agent sessions on a timeline of the run. The API
// already sends each run's sessions in start order: the page keeps it. The whole file is one scope so its
// names cannot clash with the other sections.
;(() => {
  // [icon, label, badge tone] per pipeline.Outcome.
  const OUTCOMES = { approved: ['✓', 'aprovada', 'good'], exhausted: ['↻', 'máx. iterações', 'warn'], needs_info: ['?', 'precisa de info', 'warn'], halted: ['■', 'interrompida', 'stop'] }

  // Only these reach --role and a class name: a role or kind from the data never becomes a CSS token.
  const ROLES = ['planner', 'implementer', 'reviewer', 'pr-reviewer', 'responder']
  const KINDS = ['issue', 'review', 'address']

  const HEAD = ['Run', 'Resultado', 'Linha do tempo dos agentes', 'Duração', 'Custo']

  const ms = (t) => new Date(t).getTime()
  const pad = (n) => String(n).padStart(2, '0')
  const pct = (n) => Math.min(100, Math.max(0, n || 0))

  function paint(node, role) {
    node.style.setProperty('--role', ROLES.includes(role) ? `var(--r-${role})` : 'var(--muted)')
    return node
  }

  // dd/mm hh:mm in local time.
  function when(t) {
    const d = new Date(t)
    return `${pad(d.getDate())}/${pad(d.getMonth() + 1)} ${pad(d.getHours())}:${pad(d.getMinutes())}`
  }

  // How long after the run began a session began: +42s, +12m, +1h05m.
  function since(s, start) {
    const secs = Math.max(0, Math.floor((ms(s.started_at) - start) / 1000))
    if (secs < 60) return `+${secs}s`
    if (secs < 3600) return `+${Math.floor(secs / 60)}m`
    return `+${Math.floor(secs / 3600)}h${pad(Math.floor((secs % 3600) / 60))}m`
  }

  // A run still going has no outcome yet; one the map does not know shows its own name.
  function outcome(o) {
    if (!o) return '—'
    const [icon, label, tone] = OUTCOMES[o] || ['', o, 'warn']
    return el('span', { class: 'badge ' + tone, text: icon ? `${icon} ${label}` : label })
  }

  const running = (s) => !s.ended_at || s.state === 'running'

  function tip(s, start) {
    const who = s.iteration > 0 ? s.role + ' · iter ' + s.iteration : s.role
    return [who, since(s, start), fmtDuration(s.seconds), s.model || 'padrão', fmtTokens(s.tokens) + ' tokens', fmtUSD(s.cost_usd)].join(' · ')
  }

  // A session sits on the run's track where it began, as wide as it lasted; one still running lasts until now.
  function segment(s, start, span, now) {
    const left = pct(((ms(s.started_at) - start) / span) * 100)
    const width = Math.min(pct((((s.ended_at ? ms(s.ended_at) : now) - ms(s.started_at)) / span) * 100), 100 - left)
    const label = s.iteration > 0 ? `${s.role} ${s.iteration}` : s.role
    const node = paint(el('span', { class: running(s) ? 'seg running' : 'seg', title: tip(s, start) }, width >= 14 ? label : null), s.role)
    node.style.setProperty('left', left.toFixed(2) + '%')
    node.style.setProperty('width', width.toFixed(2) + '%')
    return node
  }

  function track(r, now) {
    const start = ms(r.started_at)
    const span = Math.max((r.ended_at ? ms(r.ended_at) : now) - start, 1)
    return el('div', { class: 'track' }, (r.sessions || []).map((s) => segment(s, start, span, now)))
  }

  function row(r, now) {
    const kind = KINDS.includes(r.kind) ? el('span', { class: 'tag kind-' + r.kind, text: r.kind }) : null
    return el('details', { class: 'run' },
      el('summary', {},
        el('div', { class: 'what' },
          el('div', { class: 'line' }, el('span', { class: 'ref', text: r.ref }), kind, el('span', { class: 'title', title: r.title, text: r.title })),
          el('div', { class: 'meta', text: 'run ' + r.id + ' · ' + r.repo + ' · ' + when(r.started_at) })),
        el('div', { class: 'outcome' }, outcome(r.outcome)),
        track(r, now),
        el('div', { class: 'num dur', text: fmtDuration(r.seconds) }),
        el('div', { class: 'num cost', text: fmtUSD(r.cost_usd) })))
  }

  window.reportSections = window.reportSections || {}
  window.reportSections.history = (container, report) => {
    const now = Date.now()
    container.replaceChildren(
      el('div', { class: 'history-head' }, HEAD.map((h) => el('span', { text: h }))),
      el('div', { class: 'history-runs' }, (report.history || []).map((r) => row(r, now))))
  }
})()
