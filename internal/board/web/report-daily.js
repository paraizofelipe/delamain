'use strict'

// The cost of each day as a stacked bar, one segment per role. Roles keep this order, bottom to top,
// on every bar; a role the page does not know is left out rather than turned into a class.
const DAILY_ROLES = ['planner', 'implementer', 'reviewer', 'pr-reviewer', 'responder']

// Plot margins inside the viewBox: the y axis labels sit on the left, the dd/mm labels below.
const DAILY_H = 220
const DAILY_PAD = { top: 8, right: 8, bottom: 22, left: 64 }

function svgEl(tag, attrs, ...children) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag)
  for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, String(v))
  node.append(...children)
  return node
}

const dayCost = (day, role) => Math.max(0, (day.by_role || {})[role] || 0)
const dayTotal = (day) => DAILY_ROLES.reduce((sum, r) => sum + dayCost(day, r), 0)

// The smallest step of 1, 2, 2.5 or 5 x 10^n that splits [0, max] into at most 4 intervals. Never
// under a cent: the axis labels have two decimals.
function dailyStep(max) {
  const mag = Math.max(0.01, Math.pow(10, Math.floor(Math.log10(max / 4))))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (Math.ceil(max / (m * mag) - 1e-9) <= 4) return m * mag
  }
  return 10 * mag
}

// 2026-10-30 -> 30/10
const dayLabel = (day) => `${day.slice(8, 10)}/${day.slice(5, 7)}`

// A rect whose top corners are rounded: the top segment of a bar.
function roundedTop(x, y, w, h, r) {
  r = Math.min(r, w / 2, h)
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`
}

window.reportSections = window.reportSections || {}
window.reportSections.daily = (container, report) => {
  const days = (report && report.daily) || []
  const total = days.reduce((sum, d) => sum + dayTotal(d), 0)
  if (!days.length || total <= 0) {
    container.replaceChildren(el('p', { class: 'empty', text: 'sem custo no período' }))
    return
  }

  // Drawn at the width the panel has now, so the text keeps its size on a phone and on a wide screen.
  const W = Math.max(240, Math.round(container.clientWidth) || 720)
  const plotW = W - DAILY_PAD.left - DAILY_PAD.right
  const plotH = DAILY_H - DAILY_PAD.top - DAILY_PAD.bottom
  const base = DAILY_PAD.top + plotH

  const max = Math.max(...days.map(dayTotal))
  const step = dailyStep(max)
  const top = step * Math.ceil(max / step - 1e-9)
  const y = (v) => base - (v / top) * plotH

  const svg = svgEl('svg', {
    'viewBox': `0 0 ${W} ${DAILY_H}`,
    role: 'img',
    'aria-label': `Custo por dia: total ${fmtUSD(total)}`,
  })

  for (let v = 0; v <= top + step / 2; v += step) {
    svg.append(
      svgEl('line', { class: 'grid', x1: DAILY_PAD.left, x2: W - DAILY_PAD.right, y1: y(v), y2: y(v) }),
      svgEl('text', { class: 'axis', x: DAILY_PAD.left - 6, y: y(v), 'text-anchor': 'end', 'dominant-baseline': 'middle' }, fmtUSD(v)))
  }

  const slot = plotW / days.length
  const barW = Math.max(1, slot * 0.7)
  const every = Math.max(1, Math.ceil(days.length / 6))
  days.forEach((day, i) => {
    const x = DAILY_PAD.left + i * slot + (slot - barW) / 2
    const segs = DAILY_ROLES.filter((r) => dayCost(day, r) > 0)
    let acc = 0
    segs.forEach((role, j) => {
      const v = dayCost(day, role)
      const y0 = y(acc + v)
      const h = y(acc) - y0
      acc += v
      svg.append(j === segs.length - 1
        ? svgEl('path', { class: 'seg r-' + role, d: roundedTop(x, y0, barW, h, 3) })
        : svgEl('rect', { class: 'seg r-' + role, x, y: y0, width: barW, height: h }))
    })
    if (i % every === 0) {
      svg.append(svgEl('text', { class: 'axis', x: x + barW / 2, y: DAILY_H - 6, 'text-anchor': 'middle' }, dayLabel(day.day)))
    }
  })

  // One invisible column per day, over the bars, so the whole height of a day answers the pointer.
  const tip = el('div', { class: 'daily-tip', hidden: true })
  days.forEach((day, i) => {
    const hit = svgEl('rect', { class: 'hit', x: DAILY_PAD.left + i * slot, y: DAILY_PAD.top, width: slot, height: plotH })
    hit.addEventListener('pointermove', (ev) => showDailyTip(container, tip, day, ev))
    svg.append(hit)
  })
  svg.addEventListener('pointerleave', () => { tip.hidden = true })

  const present = DAILY_ROLES.filter((r) => days.some((d) => dayCost(d, r) > 0))
  const legend = el('div', { class: 'daily-legend' },
    present.map((r) => el('span', { class: 'item' }, el('span', { class: 'swatch r-' + r }), r)))

  container.replaceChildren(legend, svg, tip)
}

function showDailyTip(container, tip, day, ev) {
  tip.replaceChildren(
    el('div', { class: 'head' }, el('span', { text: dayLabel(day.day) }), el('span', { text: fmtUSD(dayTotal(day)) })),
    ...DAILY_ROLES.filter((r) => dayCost(day, r) > 0).map((r) => el('div', { class: 'row' },
      el('span', { class: 'swatch r-' + r }), el('span', { class: 'role', text: r }), el('span', { text: fmtUSD(dayCost(day, r)) }))))
  tip.hidden = false

  // Beside the pointer, flipped to its left near the right edge so it never widens the page.
  const box = container.getBoundingClientRect()
  let x = ev.clientX - box.left + 12
  if (x + tip.offsetWidth > box.width) x = Math.max(0, ev.clientX - box.left - tip.offsetWidth - 12)
  const y = Math.max(0, ev.clientY - box.top - tip.offsetHeight - 12)
  tip.style.setProperty('left', `${x}px`)
  tip.style.setProperty('top', `${y}px`)
}
