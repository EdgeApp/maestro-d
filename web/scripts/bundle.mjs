import { build } from 'esbuild'
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const web = fileURLToPath(new URL('..', import.meta.url))
const embedded = path.resolve(web, '../pkg/hubweb/dist')

async function bundle(dist) {
  const assets = path.join(dist, 'assets')
  await mkdir(assets, { recursive: true })
  const options = {
    absWorkingDir: web, bundle: true, minify: true, target: 'es2022',
    legalComments: 'none', metafile: true, outdir: assets, preserveSymlinks: true,
  }
  const js = await build({
    ...options, entryPoints: { app: 'src/main.tsx' }, entryNames: '[name]-[hash]',
    format: 'esm', jsx: 'automatic', define: { 'process.env.NODE_ENV': '"production"' },
  })
  const css = await build({
    ...options, entryPoints: { app: 'theme/dashboard.css' }, entryNames: '[name]-[hash]',
    external: ['/assets/fonts/*'],
  })
  await build({ ...options, entryPoints: ['src/theme-boot.ts'], format: 'iife' })
  await cp(path.join(web, 'theme/fonts'), path.join(assets, 'fonts'), { recursive: true })

  const outputName = result => path.basename(Object.keys(result.metafile.outputs)[0])
  const template = await readFile(path.join(web, 'index.html'), 'utf8')
  await writeFile(path.join(dist, 'index.html'), template.replace('{{script}}', outputName(js)).replace('{{style}}', outputName(css)))

  const packages = new Set(Object.keys(js.metafile.inputs).flatMap(input => {
    const match = /^node_modules\/((?:@[^/]+\/)?[^/]+)\//.exec(input)
    return match ? [match[1]] : []
  }))
  const notices = []
  for (const name of [...packages].sort()) {
    const license = await readFile(path.join(web, 'node_modules', name, 'LICENSE'), 'utf8')
    notices.push('## ' + name + '\n\n' + license.trim())
  }
  await writeFile(path.join(assets, 'THIRD_PARTY_LICENCES.md'), notices.join('\n\n') + '\n')
}

async function filenames(dir) {
  const entries = await readdir(dir, { withFileTypes: true, recursive: true })
  return entries.filter(entry => entry.isFile()).map(entry =>
    path.relative(dir, path.join(entry.parentPath, entry.name))).sort()
}

// Compare complete bundles, including stale or untracked files, without changing the checkout.
const fresh = await mkdtemp(path.join(tmpdir(), 'maestro-d-web-'))
try {
  await bundle(fresh)
  if (process.argv.includes('--check')) {
    const files = await filenames(fresh)
    const existing = await filenames(embedded)
    if (JSON.stringify(files) !== JSON.stringify(existing)) throw new Error('Embedded file list differs. Run make web.')
    for (const file of files) {
      if (!(await readFile(path.join(fresh, file))).equals(await readFile(path.join(embedded, file)))) {
        throw new Error(file + ' differs. Run make web.')
      }
    }
  } else {
    await rm(embedded, { recursive: true, force: true })
    await cp(fresh, embedded, { recursive: true })
  }
} finally {
  await rm(fresh, { recursive: true, force: true })
}
