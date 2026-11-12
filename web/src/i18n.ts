// UI language. Strings are written in Chinese and wrapped in t(); the
// Chinese text is the key into the English dictionary, so a missing entry
// falls back to Chinese instead of showing a key. Placeholders: {name}.

export type Lang = 'zh' | 'en'

const KEY = 'polaris.lang'

function detect(): Lang {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'zh' || v === 'en') return v
  } catch { /* storage blocked */ }
  return typeof navigator !== 'undefined' && !/^zh\b/i.test(navigator.language || 'zh') ? 'en' : 'zh'
}

let lang: Lang = detect()
if (typeof document !== 'undefined') document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en'

export function getLang(): Lang { return lang }

// setLang stores the choice and reloads: module-level labels are computed
// once, so a reload is the reliable way to switch every string.
export function setLang(l: Lang) {
  try { localStorage.setItem(KEY, l) } catch { /* ignore */ }
  if (l !== lang) location.reload()
}

// The English dictionary used to be one 322 KB chunk (4,344 lines) that
// English users had to download before the login screen could render.
// It is now split:
//   - "shell" files are small and loaded eagerly by loadLang(): the shell
//     dictionary (keys used by the app shell and the login page, moved
//     there from the files noted in shell.ts) plus core.ts and the small
//     named dictionaries (appearance, adminRoles, cdn*, websites, ...).
//   - the big files (g1-g9, operations.ts) are loaded per page, from
//     pageDicts below, before the page component is imported, and in the
//     background for everything else (loadRestInBackground).
// A key that has not loaded yet falls back to Chinese, exactly like a
// missing entry, so a slow or failed chunk can never blank the UI.
type DictModule = { default: Record<string, string> }
const loaders: Record<string, () => Promise<DictModule>> = {
  'shell.ts': () => import('./locales/en/shell'),
  'core.ts': () => import('./locales/en/core'),
  'adminRoles.ts': () => import('./locales/en/adminRoles'),
  'appearance.ts': () => import('./locales/en/appearance'),
  'recovery.ts': () => import('./locales/en/recovery'),
  'nodeBrowser.ts': () => import('./locales/en/nodeBrowser'),
  'userOnline.ts': () => import('./locales/en/userOnline'),
  'cdnSchedule.ts': () => import('./locales/en/cdnSchedule'),
  'cdnTunnel.ts': () => import('./locales/en/cdnTunnel'),
  'cdnTunnelAgent.ts': () => import('./locales/en/cdnTunnelAgent'),
  'cdnRemoteToken.ts': () => import('./locales/en/cdnRemoteToken'),
  'cdnPoolEditor.ts': () => import('./locales/en/cdnPoolEditor'),
  'websites.ts': () => import('./locales/en/websites'),
  'g1.ts': () => import('./locales/en/g1'),
  'g2.ts': () => import('./locales/en/g2'),
  'g3.ts': () => import('./locales/en/g3'),
  'g4.ts': () => import('./locales/en/g4'),
  'g5.ts': () => import('./locales/en/g5'),
  'g6.ts': () => import('./locales/en/g6'),
  'g7.ts': () => import('./locales/en/g7'),
  'g8.ts': () => import('./locales/en/g8'),
  'g9.ts': () => import('./locales/en/g9'),
  'operations.ts': () => import('./locales/en/operations'),
}

// Files loaded eagerly: the shell dictionary plus the small named files.
// Together they are ~40 KB of source (vs 384 KB for the whole dictionary).
const eagerFiles = ['shell.ts', 'core.ts', 'adminRoles.ts', 'appearance.ts', 'recovery.ts', 'nodeBrowser.ts', 'userOnline.ts', 'cdnSchedule.ts', 'cdnTunnel.ts', 'cdnTunnelAgent.ts', 'cdnRemoteToken.ts', 'cdnPoolEditor.ts', 'websites.ts']

// Which big dictionary files each page needs for its own t() keys.
// Derived from the page files' direct t() usage (see the per-page audit in
// the batch5 report); files contributing fewer than 5 keys to a page are
// left to the background load. Shared components' keys arrive the same
// way. Keys are never removed from the dictionary, only loaded later.
export const pageDicts: Record<string, string[]> = {
  Audit: ['g5.ts'],
  Certificates: ['g2.ts'],
  Chains: ['g2.ts', 'g4.ts', 'g6.ts', 'g8.ts', 'g9.ts', 'operations.ts'],
  Convert: ['g6.ts', 'operations.ts'],
  Cores: ['g3.ts', 'g7.ts'],
  Costs: ['g7.ts', 'g8.ts'],
  DNSRules: ['g2.ts', 'g6.ts'],
  Dashboard: ['g1.ts'],
  Domains: ['g3.ts', 'g6.ts'],
  Extensions: ['g3.ts', 'g5.ts', 'g6.ts', 'g7.ts', 'g8.ts', 'g9.ts'],
  Forwards: ['g2.ts', 'g4.ts', 'g6.ts'],
  Inbounds: ['g4.ts', 'g6.ts', 'g9.ts', 'operations.ts'],
  LicensePage: ['g7.ts', 'g8.ts'],
  Login: [],
  NodeMap: ['g8.ts'],
  Nodes: ['g3.ts', 'g4.ts', 'g6.ts', 'g7.ts', 'g8.ts'],
  Plans: ['g1.ts', 'g6.ts', 'g7.ts', 'g8.ts', 'operations.ts'],
  PluginPage: [],
  Probe: ['g3.ts', 'g4.ts', 'g6.ts', 'g8.ts', 'operations.ts'],
  Settings: ['g5.ts'],
  Shop: ['g1.ts', 'g4.ts', 'g6.ts', 'g8.ts'],
  StaffWorkspace: ['g5.ts'],
  Stats: ['g1.ts'],
  SubTemplates: ['g6.ts'],
  Telegram: ['g5.ts', 'g6.ts'],
  Templates: ['g4.ts', 'g6.ts', 'g9.ts'],
  Tickets: ['g5.ts'],
  Users: ['g1.ts', 'g4.ts', 'g6.ts'],
  Websites: [],
}

let dict: Record<string, string> = {}
const loaded = new Set<string>()
const loading = new Map<string, Promise<void>>()

async function loadFiles(files: string[]): Promise<void> {
  if (lang !== 'en') return
  await Promise.all(files.map((f) => {
    if (loaded.has(f)) return Promise.resolve()
    let p = loading.get(f)
    if (!p) {
      p = loaders[f]().then((m) => { Object.assign(dict, m.default); loaded.add(f) }).catch(() => {
        // Chunk failed to load: Chinese is always complete.
      })
      loading.set(f, p)
    }
    return p
  }))
}

// loadLang must finish before any module that calls t() at load time is
// imported (main.tsx imports the app after it). It now only waits for the
// small shell dictionaries, not the whole dictionary.
export async function loadLang(): Promise<void> {
  if (lang !== 'en') return
  try {
    await loadFiles(eagerFiles)
  } catch {
    lang = 'zh' // dictionary failed to load: Chinese is always complete
  }
}

// Load a page's dictionary files before its component is imported.
export function ensurePageDict(page: string): Promise<void> {
  return loadFiles(pageDicts[page] ?? [])
}

// Load every remaining dictionary file in the background, then tell
// subscribers so the app re-renders with the full dictionary. Started
// from main.tsx once the app is rendering.
let restStarted = false
const listeners = new Set<() => void>()
export function onDictReady(cb: () => void): () => void {
  listeners.add(cb)
  return () => { listeners.delete(cb) }
}
export function loadRestInBackground(): void {
  if (lang !== 'en' || restStarted) return
  restStarted = true
  const rest = Object.keys(loaders).filter((f) => !eagerFiles.includes(f))
  void loadFiles(rest).then(() => { listeners.forEach((cb) => { try { cb() } catch { /* ignore */ } }) })
}

// A key may carry a context after "@@" (t('关闭@@off')) when one Chinese
// word needs two English translations; Chinese shows the part before "@@".
export function t(s: string, vars?: Record<string, string | number | undefined | null>): string {
  const at = s.indexOf('@@')
  const zh = at > 0 ? s.slice(0, at) : s
  let out = lang === 'en' ? (dict[s] ?? zh) : zh
  if (vars) {
    out = out.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k] ?? '') : m))
    if (lang === 'en') out = out.replace(singular, '1 $1') // "1 users" → "1 user"
  }
  return out
}

// Nouns the English strings count with a placeholder; "1 <noun>s" is made singular.
const singular = /\b1 (user|node|inbound|outbound|day|group|device|item|record|target|step|time|fallback|rule|chain|certificate|member|hour|minute|second|proxy group|rule set|conn)s\b/g

// tr translates a message from the server: exact matches only, otherwise
// the original text.
export function tr(s: string): string {
  return lang === 'en' ? (dict[s] ?? s) : s
}
