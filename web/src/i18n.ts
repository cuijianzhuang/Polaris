import en from './locales/en'

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

const dict: Record<string, string> = en

export function t(s: string, vars?: Record<string, string | number | undefined | null>): string {
  let out = lang === 'en' ? (dict[s] ?? s) : s
  if (vars) out = out.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k] ?? '') : m))
  return out
}

// tr translates a message from the server: exact matches only, otherwise
// the original text.
export function tr(s: string): string {
  return lang === 'en' ? (dict[s] ?? s) : s
}
