'use strict'

// Placeholder until the cost-per-day section lands.
window.reportSections = window.reportSections || {}
window.reportSections.daily = (container) => {
  container.replaceChildren(el('p', { class: 'empty', text: 'em construção' }))
}
