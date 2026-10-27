import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import QRCode from 'qrcode'
import { Icon } from './Icons'
import { errMsg } from '../format'
import { t } from '../i18n'

// --- toasts ---
// A toast is a short message; it can carry an action (撤销), a countdown bar,
// or a progress bar that the caller updates (batch operations).
type ToastOpts = { action?: { label: string; fn: () => void }; ttl?: number; progress?: number; sticky?: boolean }
type Toast = { id: number; text: string; err?: boolean; leaving?: boolean } & ToastOpts
type ToastApi = {
  push: (text: string, err?: boolean, opts?: ToastOpts) => number
  update: (id: number, patch: Partial<Toast>) => void
  dismiss: (id: number) => void
}
const ToastCtx = createContext<(text: string, err?: boolean) => void>(() => {})
const ToastApiCtx = createContext<ToastApi>({ push: () => 0, update: () => {}, dismiss: () => {} })
export function useToast() { return useContext(ToastCtx) }
export function useToastApi() { return useContext(ToastApiCtx) }

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([])
  const dismiss = useCallback((id: number) => {
    setItems((x) => x.map((t) => (t.id === id ? { ...t, leaving: true } : t)))
    setTimeout(() => setItems((x) => x.filter((t) => t.id !== id)), 220)
  }, [])
  const push = useCallback((text: string, err?: boolean, opts?: ToastOpts) => {
    const id = Date.now() + Math.random()
    setItems((x) => [...x.slice(-4), { id, text, err, ...opts }])
    if (!opts?.sticky) setTimeout(() => dismiss(id), opts?.ttl ?? (err ? 6000 : 3000))
    return id
  }, [dismiss])
  const update = useCallback((id: number, patch: Partial<Toast>) => setItems((x) => x.map((t) => (t.id === id ? { ...t, ...patch } : t))), [])
  const simple = useCallback((text: string, err?: boolean) => { push(text, err) }, [push])
  const api = useRef<ToastApi>({ push, update, dismiss })
  api.current = { push, update, dismiss }
  return (
    <ToastApiCtx.Provider value={api.current}>
    <ToastCtx.Provider value={simple}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={'toast' + (t.err ? ' err' : '') + (t.leaving ? ' leaving' : '')}
            onClick={() => { if (!t.action && t.progress === undefined) dismiss(t.id) }}>
            <span className="toast-ico" aria-hidden="true">{t.progress !== undefined ? <span className="spinner" /> : t.err ? '!' : '✓'}</span>
            <span className="toast-text">{t.text}</span>
            {t.action && <button type="button" className="toast-act" onClick={(e) => { e.stopPropagation(); t.action!.fn(); dismiss(t.id) }}>{t.action.label}</button>}
            {t.action && t.ttl && <span className="toast-timer" style={{ animationDuration: t.ttl + 'ms' }} />}
            {t.progress !== undefined && <span className="toast-progress"><span style={{ width: `${Math.round(t.progress * 100)}%` }} /></span>}
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
    </ToastApiCtx.Provider>
  )
}

// Pending undoable actions run on their timer, or right away when the page
// is being left so nothing is silently dropped.
const pendingUndo = new Map<number, () => void>()
if (typeof window !== 'undefined') window.addEventListener('pagehide', () => { pendingUndo.forEach((f) => f()); pendingUndo.clear() })

// useUndo hides something at once and commits after a few seconds unless the
// toast's 撤销 is pressed: commit() does the real work, restore() puts it back.
export function useUndo() {
  const toastApi = useToastApi()
  return useCallback((text: string, commit: () => Promise<unknown>, restore: () => void, ms = 5000) => {
    let done = false
    const id = Date.now() + Math.random()
    const run = () => {
      if (done) return
      done = true
      pendingUndo.delete(id)
      commit().catch((e) => { restore(); toastApi.push(errMsg(e), true) })
    }
    pendingUndo.set(id, run)
    const timer = setTimeout(run, ms)
    toastApi.push(text, false, { ttl: ms, action: { label: t('撤销'), fn: () => { if (done) return; done = true; clearTimeout(timer); pendingUndo.delete(id); restore() } } })
  }, [toastApi])
}

// runBatch runs one request per item (a few at a time) with a progress toast.
export async function runBatch<T>(api: ToastApi, label: string, items: T[], fn: (item: T) => Promise<unknown>, concurrency = 4) {
  const id = api.push(`${label} 0/${items.length}`, false, { sticky: true, progress: 0 })
  let done = 0, failed = 0, next = 0
  const worker = async () => {
    while (next < items.length) {
      const it = items[next++]
      try { await fn(it) } catch { failed++ }
      done++
      api.update(id, { text: `${label} ${done}/${items.length}`, progress: done / items.length })
    }
  }
  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, worker))
  api.update(id, { text: failed ? t('{label}完成，{failed} 项失败', { label, failed }) : t('{label}完成（{n} 项）', { label, n: items.length }), progress: undefined, err: failed > 0 })
  setTimeout(() => api.dismiss(id), failed ? 6000 : 2500)
  return { done, failed }
}

// useAction wraps an async action with error toasts and a busy flag.
export function useAction() {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const run = useCallback(async <T,>(fn: () => Promise<T>, ok?: string): Promise<T | undefined> => {
    setBusy(true)
    try {
      const r = await fn()
      if (ok) toast(ok)
      return r
    } catch (e) {
      toast(errMsg(e), true)
      return undefined
    } finally {
      setBusy(false)
    }
  }, [toast])
  return { busy, run }
}

// useLoad fetches data and exposes a reload function.
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = []): { data: T | undefined; error: string; reload: () => void; loading: boolean } {
  const [data, setData] = useState<T>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const fnRef = useRef(fn)
  fnRef.current = fn
  useEffect(() => {
    let live = true
    setLoading(true)
    fnRef.current().then((d) => { if (live) { setData(d); setError('') } })
      .catch((e) => { if (live) setError(errMsg(e)) })
      .finally(() => { if (live) setLoading(false) })
    return () => { live = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tick, ...deps])
  return { data, error, loading, reload: () => setTick((t) => t + 1) }
}

// Refresh interval of live pages, one setting for all of them (0 = off).
const REFRESH_KEY = 'polaris.refresh'
export const refreshChoices = [2000, 5000, 10000, 15000, 30000, 60000, 0]
export function useRefresh(def: number, key = REFRESH_KEY): [number, (ms: number) => void] {
  const [ms, setMs] = useState(() => {
    try { const v = Number(localStorage.getItem(key)); if (localStorage.getItem(key) !== null && refreshChoices.includes(v)) return v } catch { /* storage unavailable */ }
    return def
  })
  return [ms, (v) => { setMs(v); try { localStorage.setItem(key, String(v)) } catch { /* private mode */ } }]
}

// RefreshPicker chooses how often a live page reloads; reload now is the button next to it.
export function RefreshPicker({ ms, onChange, onNow }: { ms: number; onChange: (ms: number) => void; onNow: () => void }) {
  return (
    <span className="row" style={{ gap: 6, flexWrap: 'nowrap' }}>
      <button className="btn sm" type="button" onClick={onNow} title={t('立即刷新')} aria-label={t('立即刷新')}>↻</button>
      <select className="input" style={{ width: 'auto', minHeight: 32 }} value={ms} onChange={(e) => onChange(Number(e.target.value))} aria-label={t('自动刷新频率')}>
        {refreshChoices.map((v) => <option key={v} value={v}>{v ? t('每 {n} 秒刷新', { n: v / 1000 }) : t('不自动刷新')}</option>)}
      </select>
    </span>
  )
}

// usePoll re-runs reload every ms while the tab is visible, and right away
// when the tab comes back, so live numbers keep moving without a refresh.
export function usePoll(reload: () => void, ms: number) {
  const ref = useRef(reload)
  ref.current = reload
  useEffect(() => {
    if (!ms) return // 0 = automatic refresh is off
    const t = setInterval(() => { if (!document.hidden) ref.current() }, ms)
    const vis = () => { if (!document.hidden) ref.current() }
    document.addEventListener('visibilitychange', vis)
    return () => { clearInterval(t); document.removeEventListener('visibilitychange', vis) }
  }, [ms])
}

// useWidth tracks an element's width so charts draw at real pixel size (text
// stays legible on phones instead of shrinking with a fixed viewBox).
export function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  const [w, setW] = useState(0)
  useEffect(() => {
    if (!ref.current) return
    const ro = new ResizeObserver(([e]) => setW(Math.round(e.contentRect.width)))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  return [ref, w] as const
}

// --- overlays ---

// useClosing delays onClose until the exit animation has played.
function useClosing(onClose: () => void): [boolean, () => void] {
  const [closing, setClosing] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current) }, [])
  const close = useCallback(() => {
    if (timer.current) return
    setClosing(true)
    timer.current = setTimeout(onClose, 170)
  }, [onClose])
  return [closing, close]
}

// Escape closes only the topmost drawer / modal (a scanner opened from the
// inbound drawer must not take the drawer and its unsaved form with it).
const escStack: { current: () => void }[] = []
export function useEscape(onClose: () => void, enabled = true) {
  const ref = useRef(onClose)
  ref.current = onClose
  useEffect(() => {
    if (!enabled) return
    const entry = ref
    escStack.push(entry)
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape' && escStack[escStack.length - 1] === entry) entry.current() }
    window.addEventListener('keydown', h)
    return () => { window.removeEventListener('keydown', h); escStack.splice(escStack.indexOf(entry), 1) }
  }, [enabled])
}

export function Drawer({ title, onClose, children, footer, wide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  const [closing, close] = useClosing(onClose)
  useEscape(close)
  return (
    <div className={'overlay' + (closing ? ' closing' : '')} onMouseDown={(e) => { if (e.target === e.currentTarget) close() }}>
      <aside className={'drawer' + (wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={typeof title === 'string' ? title : undefined}>
        <div className="dhead"><h2>{title}</h2><button className="icon-btn" type="button" aria-label={t('关闭')} onClick={close}><Icon.Close /></button></div>
        {children}
        {footer && <div className="dfoot">{footer}</div>}
      </aside>
    </div>
  )
}

export function Modal({ title, onClose, children, footer, wide, xwide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean; xwide?: boolean }) {
  const [closing, close] = useClosing(onClose)
  useEscape(close)
  // Portalled: a modal opened from a drawer must not be laid out inside it
  // (the drawer's animation transform would clip a fixed overlay).
  return createPortal(
    <div className={'overlay center' + (closing ? ' closing' : '')} onMouseDown={(e) => { if (e.target === e.currentTarget) close() }}>
      <div className={'modal' + (xwide ? ' xwide' : wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={typeof title === 'string' ? title : undefined}>
        <div className="dhead"><h2>{title}</h2><button className="icon-btn" type="button" aria-label={t('关闭')} onClick={close}><Icon.Close /></button></div>
        {children}
        {footer && <div className="dfoot">{footer}</div>}
      </div>
    </div>,
    document.body,
  )
}

const ConfirmCtx = createContext<(msg: string, opts?: { danger?: boolean; ok?: string }) => Promise<boolean>>(async () => false)
export function useConfirm() { return useContext(ConfirmCtx) }

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<{ msg: string; danger?: boolean; ok?: string; resolve: (v: boolean) => void } | null>(null)
  const ask = useCallback((msg: string, opts?: { danger?: boolean; ok?: string }) =>
    new Promise<boolean>((resolve) => setState({ msg, ...opts, resolve })), [])
  const close = (v: boolean) => { state?.resolve(v); setState(null) }
  return (
    <ConfirmCtx.Provider value={ask}>
      {children}
      {state && (
        <Modal title={t('确认操作')} onClose={() => close(false)} footer={<>
          <button className="btn" type="button" onClick={() => close(false)}>{t('取消')}</button>
          <button className={'btn ' + (state.danger ? 'danger solid' : 'pri')} type="button" onClick={() => close(true)}>{state.ok ?? t('确定')}</button>
        </>}>
          <div style={{ whiteSpace: 'pre-wrap' }}>{state.msg}</div>
        </Modal>
      )}
    </ConfirmCtx.Provider>
  )
}

// --- small pieces ---
export function Field({ label, hint, children, full, htmlFor }: { label: ReactNode; hint?: ReactNode; children: ReactNode; full?: boolean; htmlFor?: string }) {
  return (
    <div className={'field' + (full ? ' full' : '')}>
      <label className="lbl" htmlFor={htmlFor}>{label}</label>
      {children}
      {hint && <div className="hint">{hint}</div>}
    </div>
  )
}

export function Switch({ on, onChange, label, disabled }: { on: boolean; onChange: (v: boolean) => void; label: string; disabled?: boolean }) {
  return <button type="button" role="switch" aria-checked={on} aria-label={label} disabled={disabled} className={'switch' + (on ? ' on' : '')} onClick={() => onChange(!on)} />
}

// copyText copies to the clipboard. navigator.clipboard only exists in a
// secure context (HTTPS or localhost); panels opened over plain http://IP
// fall back to a hidden textarea and execCommand.
export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(text); return } catch { /* fall back */ }
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  const sel = document.getSelection()
  const prev = sel && sel.rangeCount > 0 ? sel.getRangeAt(0) : null
  ta.select()
  ta.setSelectionRange(0, text.length) // iOS
  let ok = false
  try { ok = document.execCommand('copy') } catch { ok = false }
  document.body.removeChild(ta)
  if (prev && sel) { sel.removeAllRanges(); sel.addRange(prev) }
  if (!ok) throw new Error('copy failed')
}

export function Copy({ text, label = t('复制') }: { text: string; label?: string }) {
  const toast = useToast()
  return (
    <button className="btn sm" type="button" onClick={async () => {
      try { await copyText(text); toast(t('已复制')) } catch { window.prompt(t('复制失败，请手动复制：'), text) }
    }}>{label}</button>
  )
}

export function CopyBox({ text }: { text: string }) {
  return <div className="copybox"><code>{text}</code><Copy text={text} /></div>
}

export function QR({ text, size = 200 }: { text: string; size?: number }) {
  const [src, setSrc] = useState('')
  useEffect(() => { QRCode.toDataURL(text, { width: size * 2, margin: 1 }).then(setSrc).catch(() => setSrc('')) }, [text, size])
  return src ? <img src={src} width={size} height={size} alt={t('二维码')} style={{ display: 'block', margin: '0 auto', borderRadius: 8 }} /> : null
}

export function Empty({ children, icon }: { children: ReactNode; icon?: ReactNode }) {
  return <div className="empty"><div className="empty-ico" aria-hidden="true">{icon ?? <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M3 7l9-4 9 4-9 4-9-4z" /><path d="M3 12l9 4 9-4M3 17l9 4 9-4" /></svg>}</div>{children}</div>
}

// Section is a collapsible group in a long form: a numbered header, a
// one-line summary of its values while collapsed, and the fields.
export function Section({ step, title, summary, defaultOpen = true, children }: { step?: number; title: string; summary?: ReactNode; defaultOpen?: boolean; children: ReactNode }) {
  return (
    <details className="fsec" open={defaultOpen}>
      <summary>
        {step !== undefined && <span className="fsec-n">{step}</span>}
        <span className="fsec-t">{title}</span>
        {summary && <span className="fsec-s">{summary}</span>}
        <span className="fsec-c" aria-hidden="true">›</span>
      </summary>
      <div className="fsec-b">{children}</div>
    </details>
  )
}

// SkeletonRows is a placeholder table while a list loads.
export function SkeletonRows({ rows = 5, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="sk-rows" aria-busy="true" aria-label={t('加载中')}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="sk-row">{Array.from({ length: cols }, (_, j) => <span key={j} className="skeleton" style={{ flex: j === 0 ? 2 : 1, opacity: 1 - i * 0.12 }} />)}</div>
      ))}
    </div>
  )
}

export function ErrorNote({ error }: { error?: string }) {
  return error ? <div className="note bad">{error}</div> : null
}

// TagInput edits a list of short strings as removable chips; Enter, comma,
// space or paste adds, Backspace on an empty input removes the last one.
export function TagInput({ value, onChange, placeholder, id }: { value: string[]; onChange: (v: string[]) => void; placeholder?: string; id?: string }) {
  const [text, setText] = useState('')
  const add = (raw: string) => {
    const items = raw.split(/[\s,，]+/).map((x) => x.trim()).filter(Boolean)
    if (!items.length) return
    const next = [...value]
    for (const x of items) if (!next.some((v) => v.toLowerCase() === x.toLowerCase())) next.push(x)
    onChange(next)
    setText('')
  }
  return (
    <div className="tags" onClick={(e) => (e.currentTarget.querySelector('input') as HTMLInputElement | null)?.focus()}>
      {value.map((v, i) => (
        <span key={v} className="tag-item mono">{v}
          <button type="button" aria-label={t('移除 {v}', { v })} onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </span>
      ))}
      <input id={id} className="mono" value={text} placeholder={value.length ? '' : placeholder}
        onChange={(e) => { const v = e.target.value; if (/[\s,，]$/.test(v)) add(v); else setText(v) }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') { e.preventDefault(); add(text) }
          if (e.key === 'Backspace' && !text && value.length) onChange(value.slice(0, -1))
        }}
        onBlur={() => add(text)}
        onPaste={(e) => { e.preventDefault(); add(text + ' ' + e.clipboardData.getData('text')) }} />
    </div>
  )
}

// lines <-> textarea helpers
export const toLines = (v?: string[]) => (v ?? []).join('\n')
export const fromLines = (s: string) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean)
