'use strict'

// The most expensive runs and agents of the period. The API already ranks every list by cost: the
// page keeps its order. The whole file is one scope so its names cannot clash with the other sections.
;(() => {
  // [icon, label, badge tone] per pipeline.Outcome.
  const OUTCOMES = { approved: ['✓', 'aprovada', 'good'], exhausted: ['↻', 'máx. iterações', 'warn'], needs_info: ['?', 'precisa de info', 'warn'], halted: ['■', 'interrompida', 'stop'] }

  const empty = () => el('p', { class: 'empty', text: 'nada no período' })

  // A bar of share 0..1 of the panel's reference value.
  function bar(share) {
    const node = el('span', { class: 'bar' })
    node.style.setProperty('--w', `${Math.min(100, Math.max(0, share * 100 || 0)).toFixed(1)}%`)
    return node
  }

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
        el('th', { class: 'pos', text: '#' }), el('th', { class: 'what', text: 'execução' }), el('th', { text: 'resultado' }),
        el('th', { text: 'tempo' }), el('th', { class: 'num', text: 'custo' }), el('th', { class: 'num', text: 'tokens' }))),
      el('tbody', {}, runs.map((r, i) => el('tr', {},
        el('td', { class: 'pos', text: String(i + 1) }),
        el('td', { class: 'what' },
          el('div', {}, el('span', { class: 'ref', text: r.ref }), ' ', el('span', { class: 'meta', text: 'run ' + r.id + ' · ' + r.repo })),
          el('div', { class: 'title', title: r.title, text: r.title }),
          bar(max > 0 ? r.cost_usd / max : 0)),
        el('td', {}, outcome(r.outcome)),
        el('td', { text: fmtDuration(r.seconds) }),
        el('td', { class: 'num', text: fmtUSD(r.cost_usd) }),
        el('td', { class: 'num', text: fmtTokens(r.tokens) }))))))
  }

  window.reportSections = window.reportSections || {}
  window.reportSections.top = (container, report) => {
    renderRuns(container.querySelector('#top-runs'), report.top_runs || [])
    container.querySelector('#top-agents').replaceChildren(el('p', { class: 'empty', text: 'em construção' }))
  }
})()
