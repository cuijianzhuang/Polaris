import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { get } from '../api'
import { Icon } from './Icons'
import type { Node, User } from '../types'
import { t } from '../i18n'
import { useEscape } from './ui'

export type PaletteItem = { id: string; label: string; hint?: string; icon: ReactNode; to: string; group: string }

// CommandPalette is the ⌘K / Ctrl+K quick switcher: pages, nodes and users
// (searched on the server as you type), all reachable from the keyboard.
export default function CommandPalette({ open, onClose, pages, nodes, searchUsers }: {
  open: boolean; onClose: () => void; pages: PaletteItem[]; nodes: Node[]; searchUsers: boolean
}) {
  const nav = useNavigate()
  const [q, setQ] = useState('')
  const [users, setUsers] = useState<User[]>([])
  const [sel, setSel] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  const dialog = useRef<HTMLDivElement>(null)
  const list = useRef<HTMLDivElement>(null)
  useEscape(onClose, open)
  useLayoutEffect(() => {
    if (!open) return
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setQ(''); setSel(0); input.current?.focus()
    return () => { if (previous?.isConnected) previous.focus() }
  }, [open])
  useEffect(() => {
    if (!open || !searchUsers || !q.trim()) { setUsers([]); return }
    let live = true
    const timer = setTimeout(() => {
      get<{ items: User[] }>(`/users?limit=6&q=${encodeURIComponent(q.trim())}`).then((r) => { if (live) setUsers(r.items ?? []) }).catch(() => {})
    }, 180)
    return () => { live = false; clearTimeout(timer) }
  }, [q, open, searchUsers])
  const items = useMemo(() => {
    const k = q.trim().toLowerCase()
    const hit = (s: string) => !k || s.toLowerCase().includes(k)
    const out: PaletteItem[] = pages.filter((p) => hit(p.label) || hit(p.to))
    out.push(...nodes.filter((n) => k && (hit(n.name) || hit(n.publicIpv4 ?? '') || hit(n.region ?? ''))).slice(0, 6).map((n) => ({
      id: 'n' + n.id, label: n.name, hint: [n.region, n.publicIpv4, n.status === 'online' ? t('在线') : n.status === 'offline' ? t('离线') : n.status].filter(Boolean).join(' · '),
      icon: <Icon.Server />, to: '/nodes?id=' + n.id, group: t('节点'),
    })))
    out.push(...users.map((u) => ({ id: 'u' + u.id, label: u.username, hint: u.remark || u.uuid.slice(0, 8), icon: <Icon.Users />, to: '/users?q=' + encodeURIComponent(u.username), group: t('用户') })))
    return out
  }, [q, pages, nodes, users])
  useEffect(() => { if (sel >= items.length) setSel(Math.max(0, items.length - 1)) }, [items, sel])
  useEffect(() => { list.current?.querySelector('.cp-item.on')?.scrollIntoView({ block: 'nearest' }) }, [sel])
  if (!open) return null
  const go = (it?: PaletteItem) => { if (!it) return; onClose(); nav(it.to) }
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setSel((s) => Math.min(items.length - 1, s + 1)); input.current?.focus() }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setSel((s) => Math.max(0, s - 1)); input.current?.focus() }
    else if (e.key === 'Enter' && e.target === input.current) { e.preventDefault(); go(items[sel]) }
    else if (e.key === 'Escape') { e.stopPropagation(); onClose() }
    else if (e.key === 'Tab') {
      const controls = dialog.current?.querySelectorAll<HTMLElement>('input, button:not(:disabled)')
      const first = controls?.[0], last = controls?.[controls.length - 1]
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus() }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus() }
    }
  }
  let last = ''
  return (
    <div className="overlay center cp-overlay" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div ref={dialog} className="cp" role="dialog" aria-modal="true" aria-label={t('快速跳转')} onKeyDown={onKey}>
        <div className="cp-search"><Icon.Search />
          <input ref={input} value={q} onChange={(e) => { setQ(e.target.value); setSel(0) }} placeholder={searchUsers ? t('搜索页面、节点、用户…') : t('搜索页面、节点…')} aria-label={t('搜索')} />
          <kbd>Esc</kbd>
        </div>
        <div className="cp-list" ref={list}>
          {items.length === 0 && <div className="cp-empty">{t('没有匹配的结果')}</div>}
          {items.map((it, i) => {
            const head = it.group !== last ? <div className="cp-group">{it.group}</div> : null
            last = it.group
            return <div key={it.id}>{head}
              <button type="button" className={'cp-item' + (i === sel ? ' on' : '')} onFocus={() => setSel(i)} onMouseMove={() => setSel(i)} onClick={() => go(it)}>
                <span className="cp-ico">{it.icon}</span><span className="cp-label">{it.label}</span>{it.hint && <span className="cp-hint">{it.hint}</span>}
              </button></div>
          })}
        </div>
        <div className="cp-foot"><span><kbd>↑</kbd><kbd>↓</kbd> {t('选择')}</span><span><kbd>Enter</kbd> {t('打开')}</span><span><kbd>Ctrl</kbd>/<kbd>⌘</kbd>+<kbd>K</kbd> {t('随时唤出')}</span></div>
      </div>
    </div>
  )
}
