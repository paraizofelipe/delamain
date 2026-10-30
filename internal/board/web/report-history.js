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

  function kv(pairs) {
    return el('dl', { class: 'kv' }, pairs.flatMap(([k, v]) => [el('dt', { text: k }), el('dd', { text: v })]))
  }

  const SESSION_HEAD = ['agente', 'iteração', 'modelo', 'início', 'duração', 'tokens', 'custo']

  // The numbers of each segment of the timeline, and what the run spent outside the agents.
  function detail(r) {
    const start = ms(r.started_at)
    const sessions = r.sessions || []
    const agents = sessions.reduce((sum, s) => sum + s.seconds, 0)
    return el('details', { class: 'detail', open: true },
      el('summary', { text: 'sessões' }),
      el('div', { class: 'detail-body' },
        el('div', { class: 'table-wrap' }, el('table', { class: 'sessions' },
          el('thead', {}, el('tr', {}, SESSION_HEAD.map((h, i) => el('th', { class: i >= 4 ? 'num' : null, text: h })))),
          el('tbody', {}, sessions.map((s) => el('tr', {},
            el('td', {}, paint(el('span', { class: 'dot' }), s.role), ' ', s.role),
            el('td', { text: s.iteration > 0 ? String(s.iteration) : '—' }),
            el('td', { text: s.model || 'padrão' }),
            el('td', { class: 'mono', text: since(s, start) }),
            el('td', { class: 'num', text: fmtDuration(s.seconds) }),
            el('td', { class: 'num', text: fmtTokens(s.tokens) }),
            el('td', { class: 'num', text: fmtUSD(s.cost_usd) })))))),
        el('div', { class: 'side' },
          kv([
            ['iterações', String(r.iterations)],
            ['tempo dos agentes', fmtDuration(agents)],
            // Sessions that overlap can add up to more than the run lasted.
            ['tempo fora dos agentes', fmtDuration(Math.max(0, r.seconds - agents))],
            ['tokens', fmtTokens(r.tokens)],
          ]),
          el('a', { href: './', text: 'abrir no quadro' }))))
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
        el('div', { class: 'num cost', text: fmtUSD(r.cost_usd) })),
      detail(r))
  }

  // Bumped on every call of the section, which report.js makes on every filter change.
  let generation = 0

  // The roles on the timelines drawn so far: the known ones in their fixed order, then any other.
  function legend(v) {
    const roles = v.roles
    const order = [...ROLES.filter((role) => roles.has(role)), ...[...roles].filter((role) => !ROLES.includes(role))]
    v.legend.replaceChildren(...order.map((role) => el('span', { class: 'item' }, paint(el('span', { class: 'dot' }), role), ' ', role)))
  }

  // Appends the runs not on the page yet: a run that moved up a page under new data is drawn once.
  function append(v, runs) {
    const now = Date.now()
    for (const r of runs) {
      if (v.shown.has(r.id)) continue
      v.shown.add(r.id)
      for (const s of r.sessions || []) v.roles.add(s.role)
      v.list.append(row(r, now))
    }
    v.more.hidden = v.next === null
    legend(v)
  }

  async function loadMore(v) {
    v.more.disabled = true
    v.error.hidden = true
    try {
      const page = await v.api.page(v.next)
      if (v.gen !== generation) return
      v.next = page.next_offset ?? null
      append(v, page.history || [])
    } catch (err) {
      if (v.gen !== generation) return
      // The rows already drawn stay; the button is there to try again.
      v.error.textContent = 'não foi possível carregar mais execuções: ' + (err.message || err)
      v.error.hidden = false
    } finally {
      v.more.disabled = false
    }
  }

  window.reportSections = window.reportSections || {}
  window.reportSections.history = (container, report, api) => {
    const gen = ++generation
    if (!(report.history || []).length) {
      container.replaceChildren(el('p', { class: 'empty', text: 'nenhuma execução no período' }))
      return
    }
    const v = {
      gen,
      api,
      shown: new Set(),
      roles: new Set(),
      legend: el('div', { class: 'legend' }),
      next: report.next_offset ?? null,
      list: el('div', { class: 'history-runs' }),
      error: el('p', { class: 'error', hidden: true }),
    }
    v.more = el('button', { type: 'button', class: 'more', text: 'carregar mais 20', onclick: () => loadMore(v) })
    append(v, report.history || [])
    container.replaceChildren(
      v.legend,
      el('div', { class: 'history-head' }, HEAD.map((h) => el('span', { text: h }))),
      v.list, v.error, v.more)
  }
})()
