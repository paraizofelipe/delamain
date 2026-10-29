'use strict'

const FLOW = new Set(['queue', 'setup', 'plan', 'implement', 'verify', 'review'])
const KIND_LABEL = { issue: 'issue', review: 'PR review', address: 'address', queued: 'fila' }

const state = {
  snapshot: null,
  filters: { kind: '', repo: '' },
  cards: new Map(), // key -> { sig, node }
  open: null, // run id in the drawer
  openSig: '',
}

try {
  const saved = JSON.parse(localStorage.getItem('delamain-board-filters') || '{}')
  state.filters.kind = saved.kind || ''
  state.filters.repo = saved.repo || ''
} catch (_) { /* no storage: the filters start empty */ }

// --- connection ------------------------------------------------------------

function connect() {
  const es = new EventSource('events')
  es.addEventListener('open', () => setConn(true, 'ao vivo'))
  es.addEventListener('snapshot', (e) => {
    $('banner').hidden = true
    setConn(true, 'ao vivo · ' + new Date().toLocaleTimeString())
    render(JSON.parse(e.data))
  })
  es.addEventListener('problem', (e) => {
    const { error } = JSON.parse(e.data)
    $('banner').textContent = 'não foi possível ler o estado: ' + error
    $('banner').hidden = false
  })
  // EventSource reconnects on its own; the dot shows the gap.
  es.addEventListener('error', () => setConn(false, 'reconectando…'))
}

function setConn(ok, text) {
  $('conn').className = 'conn ' + (ok ? 'ok' : 'bad')
  $('conn-text').textContent = text
}

// --- board -----------------------------------------------------------------

function render(snap) {
  state.snapshot = snap
  renderStatus(snap.status)
  renderRepoFilter(snap)
  const board = $('board')
  const seen = new Set()
  // Moving the cards drops the focus of the one a keyboard is on: it goes back once they are in place.
  const focusKey = document.activeElement?.dataset?.key
  for (const col of snap.columns) {
    let colNode = board.querySelector(`[data-col="${col.key}"]`)
    if (!colNode) {
      colNode = el('section', { class: `col ${col.key}${FLOW.has(col.key) ? ' flow' : ''}`, 'data-col': col.key },
        el('div', { class: 'col-head' },
          el('h2', {}, el('span', { text: col.label }), el('span', { class: 'count' })),
          el('div', { class: 'who', text: col.who })),
        el('div', { class: 'col-body' }))
      board.append(colNode)
    }
    const cards = col.cards.filter(visible)
    colNode.querySelector('.count').textContent = cards.length
    const body = colNode.querySelector('.col-body')
    const nodes = cards.map((c) => {
      seen.add(c.key)
      const sig = JSON.stringify(c) + col.key
      const cached = state.cards.get(c.key)
      if (cached && cached.sig === sig) return cached.node
      const node = cardNode(c, col.key)
      state.cards.set(c.key, { sig, node })
      return node
    })
    body.replaceChildren(...(nodes.length ? nodes : [el('div', { class: 'empty', text: '—' })]))
  }
  for (const key of state.cards.keys()) if (!seen.has(key)) state.cards.delete(key)
  if (focusKey) focusCard(focusKey)

  if (state.open !== null) {
    const card = findCard(state.open)
    const sig = card ? JSON.stringify(card) : ''
    if (sig !== state.openSig) {
      state.openSig = sig
      loadDetail(state.open)
    }
  }
}

function focusCard(key) {
  const node = $('board').querySelector(`[data-key="${CSS.escape(key)}"]`)
  if (node && node !== document.activeElement) node.focus()
}

function visible(c) {
  const { kind, repo } = state.filters
  if (kind && c.kind !== kind) return false
  if (repo && c.repo !== repo) return false
  return true
}

function findCard(id) {
  for (const col of state.snapshot?.columns || []) {
    for (const c of col.cards) if (c.id === id) return c
  }
  return null
}

function renderStatus(s) {
  const parts = [
    el('span', {}, 'daemon ', el('b', { class: s.daemon ? 'on' : 'off', text: s.daemon ? 'ativo' : 'parado' })),
    el('span', {}, 'slots ', el('b', { text: `${s.slots_in_use}/${s.slots_max}` }), s.queue_running ? ' · fila rodando' : ''),
  ]
  if (s.today) parts.push(el('span', {}, 'hoje ', el('b', { class: s.today_level, text: s.today + (s.today_limit ? ' de ' + s.today_limit : '') })))
  $('status').replaceChildren(...parts)
}

function renderRepoFilter(snap) {
  const repos = new Set()
  for (const col of snap.columns) for (const c of col.cards) repos.add(c.repo)
  const select = $('filter-repo')
  const have = [...select.options].slice(1).map((o) => o.value)
  const want = [...repos].sort()
  if (state.filters.repo && !repos.has(state.filters.repo)) want.push(state.filters.repo)
  if (have.join('\n') === want.join('\n')) return
  select.replaceChildren(el('option', { value: '', text: 'todos' }), ...want.map((r) => el('option', { value: r, text: r })))
  select.value = state.filters.repo
}

function cardNode(c, col) {
  const queued = c.kind === 'queued'
  const human = col === 'human'
  const classes = ['card', queued && 'queued', queued && c.waiting && 'waiting', c.live && 'live', human && 'human', col === 'done' && 'done'].filter(Boolean)
  const meta = []
  if (queued && c.waiting) meta.push(el('span', { class: 'tag waiting', text: '⏳ aguardando ' + c.waiting }))
  if (c.kind !== 'issue' && !queued) meta.push(el('span', { class: `tag kind-${c.kind}`, text: KIND_LABEL[c.kind] || c.kind }))
  if (c.complexity) meta.push(el('span', { class: 'tag', text: c.complexity }))
  if (c.iteration) meta.push(el('span', { text: `iter ${c.iteration}/${c.max_iterations || '?'}` }))
  if (c.spend) meta.push(el('span', { text: c.spend }))
  if (human) meta.push(el('span', { class: 'tag flag', text: 'sua vez' }))

  const node = el(queued ? 'div' : 'button', { class: classes.join(' '), type: queued ? null : 'button', 'data-key': c.key },
    el('div', { class: 'card-top' },
      c.live ? el('span', { class: 'pulse', 'aria-label': 'rodando' }) : null,
      el('span', { class: 'id', text: queued ? c.issue : `#${c.id}` }),
      el('span', { text: queued ? c.repo : `${c.repo} ${c.issue}` })),
    el('p', { class: 'title', text: c.title || '(sem título)' }),
    meta.length ? el('div', { class: 'meta' }, meta) : null,
    c.outcome ? el('div', { class: 'reason' }, el('b', { text: c.outcome }), c.reason ? ': ' + c.reason : '') : null,
    c.rounds?.length ? el('div', { class: 'rounds' }, c.rounds.map(roundNode)) : null,
    c.hint && !queued ? el('div', { class: 'hint', text: c.hint }) : null,
    queued && c.hint ? el('div', { class: 'meta', text: c.hint }) : null)
  if (!queued) node.addEventListener('click', () => openDrawer(c.id))
  return node
}

function roundNode(r) {
  return el('div', { class: 'round' },
    el('span', { class: 'r', text: r.label }),
    r.chips.map((ch) => el('span', { class: `chip ${ch.tone}`, title: ch.title, text: ch.label })))
}

// --- drawer ----------------------------------------------------------------

// What stays behind the open drawer: inert, so Tab cannot leave the dialog for a card or a filter.
const BEHIND = ['.top', '#banner', '#board', '.foot']
let lastFocus = null
let lastFocusKey = null

function setBehindInert(inert) {
  for (const sel of BEHIND) document.querySelector(sel).inert = inert
}

function openDrawer(id) {
  lastFocus = document.activeElement
  lastFocusKey = lastFocus?.dataset?.key ?? null
  state.open = id
  state.openSig = JSON.stringify(findCard(id) || '')
  $('drawer-title').textContent = `#${id}`
  $('drawer-body').replaceChildren(el('p', { class: 'empty', text: 'carregando…' }))
  $('drawer').hidden = false
  $('scrim').hidden = false
  setBehindInert(true)
  $('drawer-close').focus()
  loadDetail(id)
}

function closeDrawer() {
  state.open = null
  $('drawer').hidden = true
  $('scrim').hidden = true
  setBehindInert(false)
  // A card that changed while the drawer was open is a new node: find it by its key.
  if (lastFocus?.isConnected) lastFocus.focus()
  else if (lastFocusKey) focusCard(lastFocusKey)
}

let detailSeq = 0
async function loadDetail(id) {
  const seq = ++detailSeq
  try {
    const res = await fetch(`api/runs/${encodeURIComponent(id)}`, { cache: 'no-store' })
    if (!res.ok) throw new Error(await res.text())
    const d = await res.json()
    if (seq !== detailSeq || state.open !== id) return
    renderDetail(d)
  } catch (err) {
    if (seq !== detailSeq) return
    $('drawer-body').replaceChildren(el('p', { class: 'error', text: String(err.message || err) }))
  }
}

function renderDetail(d) {
  const c = d.card
  $('drawer-title').textContent = `#${c.id} · ${c.repo} ${c.issue} — ${c.title || ''}`
  const body = $('drawer-body')
  const closed = new Set([...body.querySelectorAll('details[data-key]')].filter((x) => !x.open).map((x) => x.dataset.key))
  const scroll = body.scrollTop

  const sections = [summarySection(d)]
  if (d.plan || d.plan_error) sections.push(planSection(d))
  for (const it of d.iterations) sections.push(iterationSection(it))
  if (d.pr_review || (c.kind === 'review' && d.artifact_error)) sections.push(prReviewSection(d))
  if (d.responses || (c.kind === 'address' && d.artifact_error)) sections.push(responsesSection(d))
  if (d.agents.length) sections.push(agentsSection(d.agents))
  if (d.report) sections.push(block('report', 'Relatório', null, el('pre', { text: d.report })))

  body.replaceChildren(...sections)
  for (const det of body.querySelectorAll('details[data-key]')) if (closed.has(det.dataset.key)) det.open = false
  body.scrollTop = scroll
}

function block(key, title, badge, ...content) {
  return el('details', { open: true, 'data-key': key },
    el('summary', {}, el('span', { text: title }), badge), ...content)
}

function badge(text, tone) {
  return text ? el('span', { class: `badge ${tone}`, text }) : null
}

function verdictBadge(v) {
  const tone = { approve: 'good', matches: 'good', changes_requested: 'warn', partial: 'warn', diverges: 'stop' }[v] || 'warn'
  const text = { approve: 'aprovado', changes_requested: 'mudanças pedidas' }[v] || v
  return badge(text, tone)
}

function kv(pairs) {
  const dl = el('dl', { class: 'kv' })
  for (const [k, v] of pairs) {
    if (v === undefined || v === null || v === '' || (Array.isArray(v) && !v.length)) continue
    dl.append(el('dt', { text: k }), el('dd', {}, v instanceof Node ? v : Array.isArray(v) ? v.join(', ') : String(v)))
  }
  return dl
}

function list(items, fn) {
  if (!items?.length) return null
  return el('ul', {}, items.map((x) => el('li', {}, fn ? fn(x) : String(x))))
}

function copyButton(text) {
  return el('button', {
    class: 'copy', type: 'button', text: 'copiar',
    onclick: (e) => {
      navigator.clipboard?.writeText(text).then(() => { e.target.textContent = 'copiado' }, () => {})
    },
  })
}

function prLink(url) {
  try {
    const u = new URL(url)
    if (u.protocol !== 'https:') return url
    return el('a', { href: u.href, target: '_blank', rel: 'noopener noreferrer', text: u.href })
  } catch (_) {
    return url
  }
}

function summarySection(d) {
  const c = d.card
  return el('section', {},
    kv([
      ['tipo', KIND_LABEL[c.kind] || c.kind],
      ['estado', c.state],
      ['coluna', d.column],
      ['resultado', c.outcome ? `${c.outcome}${c.reason ? ': ' + c.reason : ''}` : ''],
      ['iteração', c.iteration ? `${c.iteration}/${c.max_iterations || '?'}` : ''],
      ['complexidade', c.complexity],
      ['branch', c.branch],
      ['PR/MR', c.pr ? prLink(c.pr) : ''],
      ['gasto', c.spend],
      ['diretório', d.run_dir],
      ['próximo passo', c.hint ? el('span', {}, el('code', { text: c.hint }), ' ', copyButton(c.hint)) : ''],
    ]))
}

function planSection(d) {
  const p = d.plan
  if (!p) return block('plan', 'Plano · planner', badge('ilegível', 'stop'), el('p', { class: 'error', text: d.plan_error }))
  const cx = p.complexity || {}
  return block('plan', 'Plano · planner', badge(p.kind, 'good'),
    el('p', { text: p.summary }),
    d.plan_error ? el('p', { class: 'error', text: d.plan_error }) : null,
    el('h4', { text: 'Complexidade' }),
    kv([['componentes', cx.components], ['mudança', cx.change], ['dependências', cx.dependencies], ['testes', cx.tests], ['por quê', cx.rationale]]),
    p.assumptions?.length ? el('h4', { text: 'Premissas' }) : null, list(p.assumptions),
    p.slices?.length ? el('h4', { text: 'Fatias' }) : null,
    list(p.slices, (s) => el('div', {},
      el('b', { text: s.id }), ' — ', s.behavior,
      kv([['seam', s.seam], ['arquivos', s.files], ['testes', s.tests]]))),
    p.acceptance?.length ? el('h4', { text: 'Critérios de aceite' }) : null,
    list(p.acceptance, (a) => el('div', {}, a.criterion, a.tests?.length ? el('div', { class: 'where', text: a.tests.join(', ') }) : null)),
    p.regression_tests?.length ? el('h4', { text: 'Testes de regressão' }) : null, list(p.regression_tests),
    p.security?.length ? el('h4', { text: 'Ameaças' }) : null,
    list(p.security, (t) => el('div', {}, el('b', { text: t.threat }), ' → ', t.mitigation)),
    p.risks?.length ? el('h4', { text: 'Riscos' }) : null, list(p.risks))
}

function iterationSection(it) {
  const parts = []
  if (it.impl || it.impl_error) {
    parts.push(el('h4', { text: 'Implementação · implementer' }))
    if (it.impl_error) parts.push(el('p', { class: 'error', text: it.impl_error }))
    const im = it.impl
    if (im) {
      parts.push(list(im.slices, (s) => el('div', {},
        el('span', { class: `chip ${s.status === 'done' ? 'pass' : 'changes'}`, text: s.status }), ' ', el('b', { text: s.id }),
        s.commits?.length ? el('span', { class: 'where', text: ` · ${s.commits.length} commit(s)` }) : null)))
      parts.push(kv([
        ['suíte', im.full_suite?.command ? `${im.full_suite.passed} ok · ${im.full_suite.failed} falhas · ${im.full_suite.skipped} puladas` : ''],
        ['lint', im.lint?.command ? (im.lint.ok ? 'ok' : 'falhou') : ''],
        ['typecheck', im.typecheck?.command ? (im.typecheck.ok ? 'ok' : 'falhou') : ''],
      ]))
      if (im.feedback_addressed?.length) {
        parts.push(el('h4', { text: 'Feedback tratado' }))
        parts.push(list(im.feedback_addressed, (f) => el('div', {}, f.item, el('div', { class: 'where', text: f.resolution }))))
      }
      if (im.pending?.length) parts.push(el('h4', { text: 'Pendente' }), list(im.pending))
    }
  }
  if (it.gates.length) {
    parts.push(el('h4', { text: 'Verificação · gates' }))
    for (const g of it.gates) {
      parts.push(el('div', {}, el('span', { class: `chip ${g.exit === 0 ? 'pass' : 'fail'}`, text: g.exit === 0 ? '✓' : `✗ ${g.exit}` }), ' ', el('code', { text: g.name })))
      if (g.tail) parts.push(el('pre', { text: g.tail }))
    }
  }
  if (it.review || it.review_error) {
    parts.push(el('h4', { text: 'Revisão · reviewer' }))
    if (it.review_error) parts.push(el('p', { class: 'error', text: it.review_error }))
    if (it.review) parts.push(...axesNodes(it.review.axes))
  }
  return block(`iter-${it.n}`, `Iteração ${it.n}`, it.verdict ? verdictBadge(it.verdict) : null, ...parts)
}

const AXIS_AGENT = { spec: 'spec-reviewer', standards: 'standards-reviewer', security: 'security-reviewer' }

function axesNodes(axes) {
  return (axes || []).map((a) => {
    const blocking = (a.findings || []).filter((f) => f.severity === 'critical' || f.severity === 'major').length
    const tone = !a.executed ? 'stop' : blocking ? 'warn' : 'good'
    const text = !a.executed ? 'não executado' : blocking ? `${blocking} bloqueante(s)` : 'aprovado'
    return el('div', { class: 'axis' },
      el('div', {}, el('b', { text: a.axis }), ' ', el('span', { class: 'where', text: AXIS_AGENT[a.axis] || '' }), ' ', badge(text, tone)),
      (a.findings || []).map(findingNode))
  })
}

function findingNode(f) {
  const where = f.file ? `${f.file}${f.line ? ':' + f.line : ''}` : ''
  return el('div', { class: `finding ${f.severity}` },
    el('div', {}, el('span', { class: 'badge ' + ({ critical: 'stop', major: 'warn' }[f.severity] || ''), text: f.severity }),
      f.category ? ' ' + f.category : '', where ? el('span', { class: 'where', text: ' · ' + where }) : null),
    el('div', { text: f.message }),
    f.rule ? el('div', { class: 'where', text: 'regra: ' + f.rule }) : null,
    f.evidence ? el('pre', { text: f.evidence }) : null)
}

function prReviewSection(d) {
  const r = d.pr_review
  if (!r) return block('pr', 'Review do PR · pr-reviewer', badge('ilegível', 'stop'), el('p', { class: 'error', text: d.artifact_error }))
  return block('pr', 'Review do PR · pr-reviewer', verdictBadge(r.adherence),
    el('p', { text: r.summary }),
    kv([['veredito', r.verdict], ['aderência', r.adherence]]),
    ...axesNodes(r.axes),
    r.dropped?.length ? el('h4', { text: 'Descartados pelo verificador' }) : null,
    list(r.dropped, (x) => el('div', {}, x.message, el('div', { class: 'where', text: `${x.file || ''}${x.line ? ':' + x.line : ''} — ${x.reason}` }))),
    r.learnings?.length ? el('h4', { text: 'Aprendizados' }) : null, list(r.learnings))
}

function responsesSection(d) {
  const r = d.responses
  if (!r) return block('responses', 'Respostas · responder', badge('ilegível', 'stop'), el('p', { class: 'error', text: d.artifact_error }))
  const tone = { valid: 'good', unclear: 'warn', invalid: 'stop' }
  return block('responses', 'Respostas · responder', null,
    list(r.responses, (x) => el('div', {},
      badge(x.verdict, tone[x.verdict] || 'warn'), x.fixed ? ' corrigido' : '',
      el('div', { class: 'where', text: x.thread_id }), el('div', { text: x.reply }))),
    r.learnings?.length ? el('h4', { text: 'Aprendizados' }) : null, list(r.learnings))
}

function agentsSection(agents) {
  const seconds = (a) => {
    const end = a.ended_at ? new Date(a.ended_at) : new Date()
    return Math.max(0, Math.round((end - new Date(a.started_at)) / 1000))
  }
  const tone = { running: 'warn', done: 'good', failed: 'stop' }
  return block('agents', 'Agentes', null,
    el('table', { class: 'agents' },
      el('thead', {}, el('tr', {}, ['role', 'estado', 'duração', 'tokens', 'custo'].map((h) => el('th', { text: h })))),
      el('tbody', {}, agents.map((a) => el('tr', {},
        el('td', { text: a.role }),
        el('td', {}, badge(a.state, tone[a.state] || 'warn')),
        el('td', { text: fmtDuration(seconds(a)) }),
        el('td', { text: fmtTokens(a.tokens) }),
        el('td', { text: fmtUSD(a.cost_usd) }))))))
}

// --- wiring ----------------------------------------------------------------

function saveFilters() {
  try { localStorage.setItem('delamain-board-filters', JSON.stringify(state.filters)) } catch (_) { /* optional */ }
}

for (const [id, key] of [['filter-kind', 'kind'], ['filter-repo', 'repo']]) {
  const select = $(id)
  select.value = state.filters[key]
  select.addEventListener('change', () => {
    state.filters[key] = select.value
    saveFilters()
    state.cards.clear()
    if (state.snapshot) render(state.snapshot)
  })
}
$('drawer-close').addEventListener('click', closeDrawer)
$('scrim').addEventListener('click', closeDrawer)
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && state.open !== null) closeDrawer() })

connect()
