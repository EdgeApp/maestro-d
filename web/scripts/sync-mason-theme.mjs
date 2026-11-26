// Reproduce the vendored theme from the named Mason checkout.
import { execFileSync } from 'node:child_process'
import { copyFileSync, mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const [, , flag, checkout] = process.argv
if (flag !== '--from' || !checkout) throw new Error('Usage: node scripts/sync-mason-theme.mjs --from <Mason checkout>')
const theme = fileURLToPath(new URL('../theme', import.meta.url))
mkdirSync(path.join(theme, 'fonts'), { recursive: true })
for (const file of ['tokens.css', 'components.css', 'pages.css']) {
  copyFileSync(path.join(checkout, 'src/web/theme', file), path.join(theme, file))
}
for (const font of ['geist-variable', 'jetbrains-mono-variable', 'material-symbols-outlined-subset']) {
  const file = font + '.woff2'
  copyFileSync(path.join(checkout, 'assets/fonts', file), path.join(theme, 'fonts', file))
}
for (const name of ['geist', 'jetbrains-mono', 'material-symbols']) {
  const file = 'LICENSE-' + name + '.txt'
  copyFileSync(path.join(checkout, 'assets/fonts', file), path.join(theme, 'fonts', file))
}
writeFileSync(path.join(theme, 'MASON_VERSION'), execFileSync('git', ['-C', checkout, 'rev-parse', 'HEAD']))
