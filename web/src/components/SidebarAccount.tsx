import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Link, useLocation } from 'react-router-dom'
import { t } from '../i18n'
import { Icon } from './Icons'
import Avatar from './Avatar'
import AvatarSettings from './AvatarSettings'
import { Modal } from './ui'

// Render the disclosure outside the sidebar: its scroll container and mobile
// transform must not clip account actions on narrow or short screens.
export default function SidebarAccount({ name, role, avatarURL, folded, securityURL, preferences, onLogout }: {
  name: string; role: string; avatarURL?: string; folded: boolean; securityURL?: string
  preferences: ReactNode; onLogout: () => Promise<void>
}) {
  const trigger = useRef<HTMLButtonElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [avatarOpen, setAvatarOpen] = useState(false)
  const location = useLocation()
  const closeAvatar = () => { setAvatarOpen(false); trigger.current?.focus() }
  const signOut = async () => {
    if (busy) return
    setOpen(false)
    setBusy(true)
    try { await onLogout() } finally { setBusy(false) }
  }
  const place = () => {
    if (!trigger.current || !list.current) return
    const rect = trigger.current.getBoundingClientRect()
    const left = folded && innerWidth > 760 ? rect.right + 12 : rect.left
    const bottom = Math.max(12, innerHeight - rect.top + 8)
    list.current.style.left = `${Math.min(Math.max(12, left), innerWidth - Math.min(260, innerWidth - 24) - 12)}px`
    list.current.style.bottom = `${bottom}px`
    list.current.style.maxHeight = `${Math.max(80, innerHeight - bottom - 12)}px`
  }
  useLayoutEffect(() => setOpen(false), [folded, location.pathname, location.hash])
  useLayoutEffect(() => {
    if (open) { place(); list.current?.querySelector<HTMLElement>('button, a')?.focus() }
  }, [open])
  useEffect(() => {
    if (!open) return
    const outside = (event: Event) => {
      if (event.target instanceof Node && !trigger.current?.contains(event.target) && !list.current?.contains(event.target)) setOpen(false)
    }
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      event.preventDefault(); event.stopPropagation()
      setOpen(false); trigger.current?.focus()
    }
    window.addEventListener('resize', place)
    document.addEventListener('scroll', place, true)
    document.addEventListener('pointerdown', outside)
    document.addEventListener('focusin', outside)
    document.addEventListener('keydown', escape, true)
    return () => {
      window.removeEventListener('resize', place)
      document.removeEventListener('scroll', place, true)
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('focusin', outside)
      document.removeEventListener('keydown', escape, true)
    }
  }, [open, folded])
  return <div className="side-foot">
    <div className="side-account-bar">
      <button ref={trigger} type="button" className="side-account-trigger" aria-label={t('账号菜单')} aria-expanded={open} aria-controls="side-account-panel" title={folded ? `${name} · ${role}` : undefined} onClick={() => setOpen(!open)}>
        <Avatar src={avatarURL} name={name} />
        <span className="side-account-label"><b title={name}>{name}</b><small title={role}>{role}</small></span>
        <span className="side-account-chevron" aria-hidden="true"><Icon.Chevron /></span>
      </button>
      <button className="side-quick-logout" type="button" title={t('退出登录')} aria-label={t('退出登录')} disabled={busy} onClick={signOut}><Icon.Logout /></button>
    </div>
    {open && createPortal(<div ref={list} id="side-account-panel" className="menu-list side-account-panel" aria-label={t('账号操作')}>
      <div className="side-account-heading"><b title={name}>{name}</b><small>{role}</small></div>
      <button className="side-account-action" type="button" onClick={() => { setOpen(false); setAvatarOpen(true) }}><Icon.Users /><span>{t('账号头像')}</span></button>
      {securityURL && <Link className="side-account-action" to={securityURL} onClick={() => setOpen(false)}><Icon.Lock /><span>{t('账号安全')}</span><Icon.Chevron /></Link>}
      <div className="side-account-tools"><span>{t('外观与语言')}</span><div>{preferences}</div></div>
      <button className="side-account-action account-signout" type="button" disabled={busy} onClick={signOut}><Icon.Logout /><span>{t('退出登录')}</span></button>
    </div>, document.body)}
    {avatarOpen && <Modal title={t('账号头像')} onClose={closeAvatar} focusTrap><AvatarSettings compact onSaved={closeAvatar} /></Modal>}
  </div>
}
