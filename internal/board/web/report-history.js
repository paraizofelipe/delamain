'use strict'

// Placeholder until the history section lands.
window.reportSections = window.reportSections || {}
window.reportSections.history = (container) => {
  container.replaceChildren(el('p', { class: 'empty', text: 'em construção' }))
}
