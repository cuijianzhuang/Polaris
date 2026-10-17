// Panel themes: light / dark / follow-system plus an accent palette,
// stored per browser and applied as data-mode / data-accent on <html>.
import { useEffect, useState } from 'react'

export type ThemeMode = 'light' | 'dark' | 'system'
export interface Theme { mode: ThemeMode; accent: string }

export const accents: { id: string; label: string; color: string }[] = [
  { id: 'teal', label: '青碧', color: '#0E7490' },
  { id: 'blue', label: '海蓝', color: '#2563EB' },
  { id: 'violet', label: '紫罗兰', color: '#7C3AED' },
  { id: 'green', label: '松绿', color: '#15803D' },
  { id: 'rose', label: '玫红', color: '#E11D48' },
  { id: 'orange', label: '橙', color: '#C2410C' },
  { id: 'slate', label: '石墨', color: '#475569' },
]

const KEY = 'polaris.theme'
const media = () => window.matchMedia?.('(prefers-color-scheme: dark)')

export function loadTheme(): Theme {
  const t: Theme = { mode: 'light', accent: 'teal' }
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '{}')
    if (v.mode === 'light' || v.mode === 'dark' || v.mode === 'system') t.mode = v.mode
    if (accents.some((a) => a.id === v.accent)) t.accent = v.accent
  } catch { /* storage unavailable */ }
  return t
}

export function applyTheme(t: Theme) {
  const dark = t.mode === 'dark' || (t.mode === 'system' && !!media()?.matches)
  const el = document.documentElement
  el.dataset.mode = dark ? 'dark' : 'light'
  el.dataset.accent = t.accent
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
