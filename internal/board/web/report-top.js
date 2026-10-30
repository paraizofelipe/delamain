'use strict'

// The most expensive runs and agents of the period. The API already ranks every list by cost: the
// page keeps its order. The whole file is one scope so its names cannot clash with the other sections.
;(() => {
  // [icon, label, badge tone] per pipeline.Outcome.
  const OUTCOMES = { approved: ['✓', 'aprovada', 'good'], exhausted: ['↻', 'máx. iterações', 'warn'], needs_info: ['?', 'precisa de info', 'warn'], halted: ['■', 'interrompida', 'stop'] }

  // Only these reach --role: a role from the data never becomes a CSS token.
  const ROLES = ['planner', 'implementer', 'reviewer', 'pr-reviewer', 'responder']
  const MODES = [['roles', 'por papel'], ['sessions', 'por sessão']]

  let mode = 'roles'
  try {
    const saved = localStorage.getItem('delamain-report-top-mode')
    if (MODES.some(([m]) => m === saved)) mode = saved
  } catch (_) { /* no storage: the agents start by role */ }

  let last = null

  const empty = () => el('p', { class: 'empty', text: 'nada no período' })

  const pct = (share) => Math.min(100, Math.max(0, share * 100 || 0))

  // A bar of share 0..1 of the panel's reference value.
  function bar(share, role) {
    const node = el('span', { class: 'bar' })
    node.style.setProperty('--w', `${pct(share).toFixed(1)}%`)
    if (role !== undefined) paint(node, role)
    return node
  }

  function paint(node, role) {
    node.style.setProperty('--role', ROLES.includes(role) ? `var(--r-${role})` : 'var(--muted)')
    return node
  }

  const roleCell = (role) => el('td', { class: 'role' }, paint(el('span', { class: 'dot' }), role), ' ', role)

  // A run still going has no outcome yet; one the map does not know shows its own name.
  function outcome(o) {
    if (!o) return '—'
    const [icon, label, tone] = OUTCOMES[o] || ['', o, 'warn']
    return el('span', { class: 'badge ' + tone, text: icon ? `${icon} ${label}` : label })
  }

  function renderRuns(target, runs) {
    if (!runs.length) return target.replaceChildren(empty())
    const max = Math.max(...runs.map((r) => r.cost_usd))
    target.replaceChildren(el('table', { class: 'ranking' },
      el('thead', {}, el('tr', {},
        el('th', { class: 'pos', text: '#' }), el('th', { class: 'what', text: 'execução' }), el('th', { class: 'secondary', text: 'resultado' }),
        el('th', { class: 'secondary', text: 'tempo' }), el('th', { class: 'num', text: 'custo' }), el('th', { class: 'num', text: 'tokens' }))),
      el('tbody', {}, runs.map((r, i) => el('tr', {},
        el('td', { class: 'pos', text: String(i + 1) }),
        el('td', { class: 'what' },
          el('div', {}, el('span', { class: 'ref', text: r.ref }), ' ', el('span', { class: 'meta', text: 'run ' + r.id + ' · ' + r.repo })),
          el('div', { class: 'title', title: r.title, text: r.title }),
          bar(max > 0 ? r.cost_usd / max : 0)),
        el('td', { class: 'secondary' }, outcome(r.outcome)),
        el('td', { class: 'secondary', text: fmtDuration(r.seconds) }),
        el('td', { class: 'num', text: fmtUSD(r.cost_usd) }),
        el('td', { class: 'num', text: fmtTokens(r.tokens) }))))))
  }

  function rolesTable(roles) {
    const total = roles.reduce((sum, r) => sum + r.cost_usd, 0)
    return el('table', { class: 'ranking' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'papel' }), el('th', { class: 'num secondary', text: 'sessões' }), el('th', { class: 'secondary', text: 'tempo' }),
        el('th', { class: 'num', text: 'médio' }), el('th', { class: 'num', text: 'custo' }), el('th', { class: 'share', text: '% do total' }))),
      el('tbody', {}, roles.map((r) => {
        const share = total > 0 ? r.cost_usd / total : 0
        return el('tr', {},
          roleCell(r.role),
          el('td', { class: 'num secondary', text: String(r.sessions) }),
          el('td', { class: 'secondary', text: fmtDuration(r.seconds) }),
          el('td', { class: 'num', text: fmtUSD(r.sessions ? r.cost_usd / r.sessions : 0) }),
          el('td', { class: 'num', text: fmtUSD(r.cost_usd) }),
          el('td', { class: 'share' }, el('span', { class: 'mono', text: `${Math.round(pct(share))}%` }), bar(share, r.role)))
      })))
  }

  function sessionsTable(sessions) {
    return el('table', { class: 'ranking' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'papel' }), el('th', { text: 'execução' }), el('th', { class: 'secondary', text: 'modelo' }),
        el('th', { class: 'secondary', text: 'tempo' }), el('th', { class: 'num', text: 'custo' }), el('th', { class: 'num', text: 'tokens' }))),
      el('tbody', {}, sessions.map((s) => el('tr', {},
        roleCell(s.role),
        el('td', {}, el('span', { class: 'ref', text: s.run_ref }), s.iteration ? ' · iter ' + s.iteration : ' · —'),
        el('td', { class: 'mono secondary', text: s.model || 'padrão' }),
        el('td', { class: 'secondary', text: fmtDuration(s.seconds) }),
        el('td', { class: 'num', text: fmtUSD(s.cost_usd) }),
        el('td', { class: 'num', text: fmtTokens(s.tokens) })))))
  }

  function renderAgents(target, report) {
    const list = mode === 'sessions' ? report.top_sessions || [] : report.top_roles || []
    const toggle = el('div', { class: 'periods', role: 'group', 'aria-label': 'agrupar agentes' },
      MODES.map(([m, label]) => el('button', {
        type: 'button',
        'aria-pressed': String(m === mode),
        text: label,
        onclick: () => {
          if (m === mode) return
          mode = m
          try { localStorage.setItem('delamain-report-top-mode', mode) } catch (_) { /* optional */ }
          renderAgents(target, last)
        },
      })))
    const body = !list.length ? empty() : mode === 'sessions' ? sessionsTable(list) : rolesTable(list)
    target.replaceChildren(toggle, body)
  }

  window.reportSections = window.reportSections || {}
  window.reportSections.top = (container, report) => {
    last = report
    renderRuns(container.querySelector('#top-runs'), report.top_runs || [])
    renderAgents(container.querySelector('#top-agents'), report)
  }
})()
