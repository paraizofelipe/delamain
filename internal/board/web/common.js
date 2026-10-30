'use strict'

// Shared by the board and the report. Every piece of data reaches the page through textContent or an
// attribute: nothing a run or an agent wrote is ever parsed as HTML.
function el(tag, props, ...children) {
  const node = document.createElement(tag)
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue
    if (k === 'class') node.className = v
    else if (k === 'text') node.textContent = v
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v)
    else node.setAttribute(k, v === true ? '' : String(v))
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue
    node.append(c instanceof Node ? c : document.createTextNode(String(c)))
  }
  return node
}

const $ = (id) => document.getElementById(id)

// 1234 -> 1.2K, 2500000 -> 2.5M
function fmtTokens(n) {
  return n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e3 ? (n / 1e3).toFixed(1) + 'K' : String(n)
}

// 75 -> 1m15s, 42 -> 42s
function fmtDuration(s) {
  return s >= 60 ? `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s` : `${s}s`
}

function fmtUSD(n) {
  return `US$ ${n.toFixed(2)}`
}
