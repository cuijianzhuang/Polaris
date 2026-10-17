// Checks the English dictionary against the source:
//   node scripts/i18n-check.mjs [files...]
// - every t('…') / tr('…') key has an English entry
// - Chinese text outside t() and comments is reported (untranslated UI);
//   a line containing "i18n-ignore" is skipped
// Exit code 1 when anything is missing.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const root = new URL('../src/', import.meta.url).pathname
const walk = (d) => readdirSync(d).flatMap((f) => {
  const p = join(d, f)
  return statSync(p).isDirectory() ? (f === 'locales' ? [] : walk(p)) : /\.(tsx?|mts)$/.test(f) ? [p] : []
})
const files = process.argv.length > 2 ? process.argv.slice(2).map((f) => join(process.cwd(), f)) : walk(root)

const dict = new Set()
for (const f of readdirSync(join(root, 'locales/en'))) {
  const src = readFileSync(join(root, 'locales/en', f), 'utf8')
  for (const m of src.matchAll(/^\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")\s*:/gm)) {
    dict.add((m[1] ?? m[2]).replace(/\\(['"\\])/g, '$1'))
  }
}

const han = /[\u4e00-\u9fff]/
let missing = 0, raw = 0
for (const f of files) {
  const src = readFileSync(f, 'utf8')
  const rel = relative(root, f)
  for (const m of src.matchAll(/\bt[r]?\(\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)"|`((?:[^`\\$]|\\.)*)`)/g)) {
    const k = (m[1] ?? m[2] ?? m[3]).replace(/\\(['"`\\])/g, '$1')
    if (han.test(k) && !dict.has(k)) { missing++; console.log(`missing  ${rel}: ${k}`) }
  }
  // Strip comments and t()/tr() string arguments, then look for leftover Chinese.
  const stripped = src
    .split('\n').map((l) => (l.includes('i18n-ignore') ? '' : l)).join('\n')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:'"`])\/\/.*$/gm, '$1')
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, '')
    .replace(/\bt[r]?\(\s*(?:'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|`(?:[^`\\]|\\.)*`)/g, 't(')
  stripped.split('\n').forEach((line, i) => {
    if (han.test(line)) { raw++; console.log(`raw      ${rel}:${i + 1}: ${line.trim().slice(0, 120)}`) }
  })
}
console.log(`\n${missing} missing translations, ${raw} lines with untranslated Chinese`)
process.exit(missing || raw ? 1 : 0)
