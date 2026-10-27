import { useEffect } from 'react'

// Safari keeps the layout viewport tall while its keyboard shrinks the visual one.
// Hide redundant bottom navigation and place form actions above the obscured area.
export function useMobileKeyboard() {
  useEffect(() => {
    const viewport = window.visualViewport
    if (!viewport) return
    let frame = 0
    const update = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        const active = document.activeElement
        const textInput = active instanceof HTMLElement && active.matches('textarea, [contenteditable="true"], input:not([type="checkbox"]):not([type="radio"]):not([type="file"]):not([type="button"]):not([type="submit"]):not([type="range"]):not([type="color"])')
        const inset = Math.max(0, window.innerHeight - viewport.height - viewport.offsetTop)
        const open = !!textInput && window.innerWidth <= 760 && inset > 120
        document.documentElement.toggleAttribute('data-keyboard', open)
        document.documentElement.style.setProperty('--keyboard-inset', `${open ? inset : 0}px`)
      })
    }
    viewport.addEventListener('resize', update)
    viewport.addEventListener('scroll', update)
    document.addEventListener('focusin', update)
    document.addEventListener('focusout', update)
    update()
    return () => {
      cancelAnimationFrame(frame)
      viewport.removeEventListener('resize', update); viewport.removeEventListener('scroll', update)
      document.removeEventListener('focusin', update); document.removeEventListener('focusout', update)
      document.documentElement.removeAttribute('data-keyboard')
      document.documentElement.style.removeProperty('--keyboard-inset')
    }
  }, [])
}
