'use strict'

const PERIODS = ['7d', '30d', '90d', 'all']

const filters = { period: '30d', kind: '', repo: '' }

try {
  const saved = JSON.parse(localStorage.getItem('delamain-report-filters') || '{}')
  if (PERIODS.includes(saved.period)) filters.period = saved.period
  filters.kind = saved.kind || ''
  filters.repo = saved.repo || ''
} catch (_) { /* no storage: the filters start at their defaults */ }

function saveFilters() {
  try { localStorage.setItem('delamain-report-filters', JSON.stringify(filters)) } catch (_) { /* optional */ }
}

// --- data ------------------------------------------------------------------

async function fetchReport(offset) {
  const q = new URLSearchParams({ period: filters.period })
  if (filters.repo) q.set('repo', filters.repo)
  if (filters.kind) q.set('kind', filters.kind)
  if (offset) q.set('offset', String(offset))
  const res = await fetch('api/report?' + q, { cache: 'no-store' })
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText)
  return res.json()
}

let loadSeq = 0
async function load() {
  const seq = ++loadSeq
  try {
    const rep = await fetchReport(0)
    if (seq !== loadSeq) return
    $('banner').hidden = true
    render(rep)
  } catch (err) {
    if (seq !== loadSeq) return
    $('banner').textContent = 'não foi possível ler o relatório: ' + (err.message || err)
    $('banner').hidden = false
  }
}

function render(rep) {
  // A saved repo the period no longer has would filter by something the select cannot show.
  if (filters.repo && !(rep.repos || []).includes(filters.repo)) {
    filters.repo = ''
    saveFilters()
    load()
    return
  }
  renderRepoFilter(rep.repos || [])
  renderKPIs(rep.kpis)
}

function renderRepoFilter(repos) {
  const select = $('filter-repo')
  select.replaceChildren(el('option', { value: '', text: 'todos' }), ...repos.map((r) => el('option', { value: r, text: r })))
  select.value = filters.repo
}

// --- KPIs ------------------------------------------------------------------

const approvedRate = (k) => (k.runs ? (k.approved / k.runs) * 100 : null)

const KPIS = [
  { label: 'execuções', value: (k) => k.runs, format: String },
  { label: 'custo total', value: (k) => k.cost_usd, format: fmtUSD, cost: true },
  { label: 'custo médio por run', value: (k) => k.avg_cost_usd, format: fmtUSD, cost: true },
  { label: 'duração mediana', value: (k) => k.median_seconds, format: fmtDuration },
  { label: '% aprovadas', value: approvedRate, format: (n) => `${Math.round(n)}%`, points: true },
]

function renderKPIs(kpis) {
  $('kpis').replaceChildren(...KPIS.map((d) => {
    const cur = d.value(kpis)
    return el('div', { class: 'kpi' },
      el('div', { class: 'label', text: d.label }),
      el('div', { class: 'value', text: cur === null ? '—' : d.format(cur) }),
      kpis.previous ? deltaNode(d, cur, d.value(kpis.previous)) : null)
  }))
}

// The change against the previous period of the same length. Only cost has a good direction: down.
function deltaNode(d, cur, prev) {
  if (cur === null || prev === null) return el('div', { class: 'delta', text: '— vs anterior' })
  const diff = cur - prev
  let text
  if (diff === 0) text = '= vs anterior'
  else if (d.points) text = `${diff > 0 ? '▲' : '▼'} ${Math.abs(Math.round(diff))} p.p. vs anterior`
  else if (prev === 0) text = `${diff > 0 ? '▲' : '▼'} vs anterior`
  else text = `${diff > 0 ? '▲' : '▼'} ${Math.abs(Math.round((diff / prev) * 100))}% vs anterior`
  const tone = d.cost && diff > 0 ? ' worse' : d.cost && diff < 0 ? ' better' : ''
  return el('div', { class: 'delta' + tone, text })
}

// --- wiring ----------------------------------------------------------------

function markPeriod() {
  for (const b of $('filter-period').querySelectorAll('button')) {
    b.setAttribute('aria-pressed', String(b.dataset.period === filters.period))
  }
}

for (const b of $('filter-period').querySelectorAll('button')) {
  b.addEventListener('click', () => {
    if (filters.period === b.dataset.period) return
    filters.period = b.dataset.period
    markPeriod()
    saveFilters()
    load()
  })
}

for (const [id, key] of [['filter-kind', 'kind'], ['filter-repo', 'repo']]) {
  const select = $(id)
  select.value = filters[key]
  select.addEventListener('change', () => {
    filters[key] = select.value
    saveFilters()
    load()
  })
}

markPeriod()
load()
