import { JSDOM } from 'jsdom'
import { readFileSync } from 'node:fs'

const dom = new JSDOM('<html><body></body></html>', { url: 'https://hub.example:9400/' })
for (const key of ['window', 'document', 'HTMLElement', 'navigator']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key] })
}
globalThis.IS_REACT_ACT_ENVIRONMENT = true
const style = document.createElement('style')
style.textContent = ['tokens.css', 'components.css', 'pages.css', 'dashboard.css'].map(file =>
  readFileSync(new URL('../theme/' + file, import.meta.url), 'utf8').replace(/^@import.*$/gm, '')).join('\n')
document.head.append(style)
