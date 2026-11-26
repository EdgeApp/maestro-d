// Loaded before the stylesheet so the first paint follows the system appearance.
const scheme = window.matchMedia?.('(prefers-color-scheme: light)')
function applyTheme() {
  document.documentElement.dataset.theme = scheme?.matches ? 'light' : 'dark'
}
applyTheme()
scheme?.addEventListener('change', applyTheme)
