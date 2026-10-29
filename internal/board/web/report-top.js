'use strict'

// Placeholder until the most expensive runs and agents land.
window.reportSections = window.reportSections || {}
window.reportSections.top = (container) => {
  for (const sel of ['#top-runs', '#top-agents']) {
    container.querySelector(sel).replaceChildren(el('p', { class: 'empty', text: 'em construção' }))
  }
}
