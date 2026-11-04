import { useEffect } from 'react'

// Native details keep their keyboard/summary behaviour. Dismiss only the
// active disclosure, so Escape still reaches a drawer or modal underneath.
export function useMenuDismiss() {
  useEffect(() => {
    const openMenus = () => [...document.querySelectorAll<HTMLDetailsElement>('details.menu[open]')]
    const visible = (element: Element) => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden'
    const place = (menu: HTMLDetailsElement) => {
      const list = menu.querySelector<HTMLElement>(':scope > .menu-list')
      if (!list) return
      menu.style.setProperty('--menu-shift', '0px')
      menu.removeAttribute('data-menu-above')
      let rect = list.getBoundingClientRect()
      const below = innerHeight - rect.top - 12
      const above = menu.getBoundingClientRect().top - 18
      if (below < 160 && above > below) {
        menu.setAttribute('data-menu-above', '')
        rect = list.getBoundingClientRect()
      }
      const available = menu.hasAttribute('data-menu-above') ? rect.bottom - 12 : innerHeight - rect.top - 12
      menu.style.setProperty('--menu-height', `${Math.max(80, available)}px`)
      const shift = Math.max(12 - rect.left, Math.min(0, innerWidth - 12 - rect.right))
      menu.style.setProperty('--menu-shift', `${shift}px`)
    }
    const resize = () => openMenus().forEach(place)
    const outside = (event: PointerEvent) => {
      if (!(event.target instanceof Node)) return
      for (const menu of openMenus()) if (!menu.contains(event.target)) menu.open = false
    }
    const toggle = (event: Event) => {
      const menu = event.target
      if (!(menu instanceof HTMLDetailsElement) || !menu.matches('details.menu') || !menu.open) return
      place(menu)
      for (const other of openMenus()) {
        if (other !== menu && !other.contains(menu) && !menu.contains(other)) other.open = false
      }
    }
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      const menus = openMenus().filter(visible)
      const menu = menus.find((item) => item.contains(document.activeElement)) ?? menus[menus.length - 1]
      if (!menu) return
      const dialogs = [...document.querySelectorAll('[role="dialog"]')].filter(visible)
      if (dialogs.length && menu.closest('[role="dialog"]') !== dialogs[dialogs.length - 1]) return
      event.preventDefault()
      event.stopPropagation()
      menu.open = false
      menu.querySelector<HTMLElement>('summary')?.focus()
    }
    window.addEventListener('resize', resize)
    document.addEventListener('pointerdown', outside)
    document.addEventListener('toggle', toggle, true)
    document.addEventListener('keydown', escape)
    return () => {
      window.removeEventListener('resize', resize)
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('toggle', toggle, true)
      document.removeEventListener('keydown', escape)
    }
  }, [])
}
