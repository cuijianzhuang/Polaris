// Panel themes: light / dark / follow-system plus an accent palette,
// stored per browser and applied as data-mode / data-accent on <html>.
// An exclusive style (paid feature "themes", e.g. black gold) is applied as
// data-style and brings its own dark palette.
import { useEffect, useState } from 'react'
import { t } from './i18n'

export type ThemeMode = 'light' | 'dark' | 'system'
export type ThemeStyle = 'default' | 'blackgold'
export interface Theme { mode: ThemeMode; accent: string; style: ThemeStyle }

export const styles: { id: ThemeStyle; label: string; premium: boolean }[] = [
  { id: 'default', label: t('默认'), premium: false },
  { id: 'blackgold', label: t('黑金'), premium: true },
]

export const accents: { id: string; label: string; color: string }[] = [
  { id: 'teal', label: t('青碧'), color: '#0E7490' },
  { id: 'blue', label: t('海蓝'), color: '#2563EB' },
  { id: 'violet', label: t('紫罗兰'), color: '#7C3AED' },
  { id: 'green', label: t('松绿'), color: '#15803D' },
  { id: 'rose', label: t('玫红'), color: '#E11D48' },
  { id: 'orange', label: t('橙'), color: '#C2410C' },
  { id: 'slate', label: t('石墨'), color: '#27272A' },
]

const KEY = 'polaris.theme'
const media = () => window.matchMedia?.('(prefers-color-scheme: dark)')

export function loadTheme(): Theme {
  const t: Theme = { mode: 'light', accent: 'slate', style: 'default' }
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '{}')
    if (v.mode === 'light' || v.mode === 'dark' || v.mode === 'system') t.mode = v.mode
    if (accents.some((a) => a.id === v.accent)) t.accent = v.accent
    if (styles.some((x) => x.id === v.style)) t.style = v.style
  } catch { /* storage unavailable */ }
  return t
}

// premiumOK turns off a paid style once the license is known not to cover
// it; until then the stored choice shows (also on the login page).
let premiumOK = true

export function applyTheme(t: Theme) {
  const style = t.style !== 'default' && premiumOK ? t.style : 'default'
  const dark = style !== 'default' || t.mode === 'dark' || (t.mode === 'system' && !!media()?.matches)
  const el = document.documentElement
  el.dataset.mode = dark ? 'dark' : 'light'
  el.dataset.accent = t.accent
  if (style === 'default') delete el.dataset.style
  else el.dataset.style = style
}

export function setPremiumThemes(ok: boolean) {
  if (premiumOK === ok) return
  premiumOK = ok
  applyTheme(current)
}

let current = loadTheme()
const subs = new Set<(t: Theme) => void>()

export function setTheme(t: Theme) {
  current = t
  try { localStorage.setItem(KEY, JSON.stringify(t)) } catch { /* ignore */ }
  applyTheme(t)
  subs.forEach((f) => f(t))
}

// Re-apply when the OS switches between light and dark.
media()?.addEventListener?.('change', () => { if (current.mode === 'system') applyTheme(current) })

export function useTheme(): [Theme, (t: Theme) => void] {
  const [t, set] = useState(current)
  useEffect(() => { subs.add(set); return () => { subs.delete(set) } }, [])
  return [t, setTheme]
}

// isDark reports the mode currently shown.
export const isDark = () => document.documentElement.dataset.mode === 'dark'
