// Checks the English dictionary against the source:
//   node scripts/i18n-check.mjs [files...] [--fail-orphans]
// - every t('…') / tr('…') key has an English entry
// - Chinese text outside t() and comments is reported (untranslated UI);
//   a line containing "i18n-ignore" is skipped
// - dictionary entries never referenced from source are reported as orphans
//   (informational by default; add --fail-orphans to exit 1 on them).
//   Server messages translated via tr() never appear as literals in the
//   frontend, so keep those (and any other intentional entries) in
//   i18n-orphan-allowlist.txt, one key per line.
// Exit code 1 when anything is missing, and on orphans when --fail-orphans.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const failOrphans = process.argv.includes('--fail-orphans')
const cliFiles = process.argv.slice(2).filter((a) => !a.startsWith('--'))

const root = new URL('../src/', import.meta.url).pathname
const walk = (d) => readdirSync(d).flatMap((f) => {
  const p = join(d, f)
  return statSync(p).isDirectory() ? (f === 'locales' ? [] : walk(p)) : /\.(tsx?|mts)$/.test(f) ? [p] : []
})
const files = cliFiles.length > 0 ? cliFiles.slice().map((f) => join(process.cwd(), f)) : walk(root)

// Dictionary: Chinese key -> declaring file under locales/en.
const dict = new Map()
for (const f of readdirSync(join(root, 'locales/en'))) {
  const src = readFileSync(join(root, 'locales/en', f), 'utf8')
  for (const m of src.matchAll(/^\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")\s*:/gm)) {
    dict.set((m[1] ?? m[2]).replace(/\\(['"\\])/g, '$1'), f)
  }
}

// Intentional dictionary entries that are not literals in the frontend source
// (for example server messages translated via tr()).
const allowFile = new URL('./i18n-orphan-allowlist.txt', import.meta.url)
let allow = new Set()
try {
  allow = new Set(readFileSync(allowFile, 'utf8').split('\n').map((s) => s.trim()).filter((s) => s && !s.startsWith('#')))
} catch {}

const han = /[\u4e00-\u9fff]/
const keyRe = /\bt[r]?\(\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)"|`((?:[^`\\$]|\\.)*)`)/g
let missing = 0, raw = 0
const used = new Set()
for (const f of files) {
  const src = readFileSync(f, 'utf8')
  const rel = relative(root, f)
  for (const m of src.matchAll(keyRe)) {
    const k = (m[1] ?? m[2] ?? m[3]).replace(/\\(['"`\\])/g, '$1')
    used.add(k)
    if (han.test(k) && !dict.has(k)) { missing++; console.log(`missing  ${rel}: ${k}`) }
  }
  // Strip comments and t()/tr() string arguments, then look for leftover Chinese.
  const stripped = src
    .split('\n').map((l) => (l.includes('i18n-ignore') ? '' : l)).join('\n')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:'"`])\/\/.*$/gm, '$1')
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, '')
    .replace(keyRe, 't(')
  stripped.split('\n').forEach((line, i) => {
    if (han.test(line)) { raw++; console.log(`raw      ${rel}:${i + 1}: ${line.trim().slice(0, 120)}`) }
  })
}

const orphans = [...dict.entries()].filter(([k]) => han.test(k) && !used.has(k) && !allow.has(k))
for (const [k, f] of orphans) console.log(`orphan   ${f}: ${k}`)
const orphanCount = orphans.length

console.log(`\n${missing} missing translations, ${raw} lines with untranslated Chinese, ${orphanCount} orphaned dictionary entries${allow.size ? ` (${allow.size} allowlisted)` : ''}`)
process.exit(missing || raw || (failOrphans && orphanCount) ? 1 : 0)
