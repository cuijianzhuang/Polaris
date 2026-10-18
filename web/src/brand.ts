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

// Remember each built-in URL/type so removing a custom logo restores both
// the favicon and the touch icon, regardless of the built-in image format.
const builtins = new Map<HTMLLinkElement, { href: string; type: string | null }>()
function applyFavicon(b: Brand) {
  const links = document.querySelectorAll<HTMLLinkElement>('link[data-polaris-icon]')
  for (const link of links) {
    if (!builtins.has(link)) builtins.set(link, { href: link.href, type: link.getAttribute('type') })
    const builtin = builtins.get(link)!
    link.href = b.logo ? logoURL(b.logo) : builtin.href
    if (b.logo || builtin.type === null) link.removeAttribute('type')
    else link.type = builtin.type
  }
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
