// Brand: the site title and custom logo (paid feature "branding") from
// /info, shared by the sidebar, the login page and the favicon.
import { useEffect, useState } from 'react'
import { API, get } from './api'

export interface Brand { title: string; logo: string } // logo: version hash, "" = built-in logo

let cached: Promise<Brand> | null = null
const subs = new Set<(b: Brand) => void>()

export const logoURL = (v: string) => `${API}/brand/logo?v=${encodeURIComponent(v)}`

function load(): Promise<Brand> {
  if (!cached) {
    cached = get<{ title?: string; logo?: string }>('/info')
      .then((i) => ({ title: i.title || 'Polaris', logo: i.logo || '' }))
      .catch(() => ({ title: 'Polaris', logo: '' }))
    cached.then(applyFavicon)
  }
  return cached
}

// The built-in favicon in index.html is kept to restore it.
let builtin: string | null = null
function applyFavicon(b: Brand) {
  const links = Array.from(document.querySelectorAll<HTMLLinkElement>('link[rel="icon"]'))
  const svg = links.find((l) => l.type === 'image/svg+xml')
  if (!svg) return
  if (builtin === null) builtin = svg.href
  svg.removeAttribute('type')
  svg.href = b.logo ? logoURL(b.logo) : builtin
  if (!b.logo) svg.type = 'image/svg+xml'
}

// refreshBrand reloads after the logo or title changed.
export function refreshBrand() {
  cached = null
  load().then((b) => subs.forEach((f) => f(b)))
}

export function useBrand(): Brand {
  const [b, set] = useState<Brand>({ title: 'Polaris', logo: '' })
  useEffect(() => {
    let live = true
    load().then((x) => { if (live) set(x) })
    subs.add(set)
    return () => { live = false; subs.delete(set) }
  }, [])
  return b
}
