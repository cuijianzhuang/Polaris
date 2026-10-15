import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import QRCode from 'qrcode'
import { Icon } from './Icons'
import { errMsg } from '../format'

// --- toasts ---
type Toast = { id: number; text: string; err?: boolean }
const ToastCtx = createContext<(text: string, err?: boolean) => void>(() => {})
export function useToast() { return useContext(ToastCtx) }

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([])
  const push = useCallback((text: string, err?: boolean) => {
    const id = Date.now() + Math.random()
    setItems((x) => [...x, { id, text, err }])
    setTimeout(() => setItems((x) => x.filter((t) => t.id !== id)), err ? 6000 : 3000)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => <div key={t.id} className={'toast' + (t.err ? ' err' : '')}>{t.text}</div>)}
      </div>
    </ToastCtx.Provider>
  )
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

// --- overlays ---
function useEscape(onClose: () => void) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [onClose])
}

export function Drawer({ title, onClose, children, footer, wide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  useEscape(onClose)
  return (
    <div className="overlay" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <aside className={'drawer' + (wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={typeof title === 'string' ? title : undefined}>
        <div className="dhead"><h2>{title}</h2><button className="icon-btn" type="button" aria-label="关闭" onClick={onClose}><Icon.Close /></button></div>
        {children}
        {footer && <div className="dfoot">{footer}</div>}
      </aside>
    </div>
  )
}

export function Modal({ title, onClose, children, footer, wide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  useEscape(onClose)
  return (
    <div className="overlay center" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div className={'modal' + (wide ? ' wide' : '')} role="dialog" aria-modal="true">
        <div className="dhead"><h2>{title}</h2><button className="icon-btn" type="button" aria-label="关闭" onClick={onClose}><Icon.Close /></button></div>
        {children}
        {footer && <div className="dfoot">{footer}</div>}
      </div>
    </div>
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
        <Modal title="确认操作" onClose={() => close(false)} footer={<>
          <button className="btn" type="button" onClick={() => close(false)}>取消</button>
          <button className={'btn ' + (state.danger ? 'danger solid' : 'pri')} type="button" onClick={() => close(true)}>{state.ok ?? '确定'}</button>
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

export function Switch({ on, onChange, label }: { on: boolean; onChange: (v: boolean) => void; label: string }) {
  return <button type="button" role="switch" aria-checked={on} aria-label={label} className={'switch' + (on ? ' on' : '')} onClick={() => onChange(!on)} />
}

export function Copy({ text, label = '复制' }: { text: string; label?: string }) {
  const toast = useToast()
  return (
    <button className="btn sm" type="button" onClick={async () => {
      try { await navigator.clipboard.writeText(text); toast('已复制') } catch { toast('复制失败，请手动选择', true) }
    }}>{label}</button>
  )
}

export function CopyBox({ text }: { text: string }) {
  return <div className="copybox"><code>{text}</code><Copy text={text} /></div>
}

export function QR({ text, size = 200 }: { text: string; size?: number }) {
  const [src, setSrc] = useState('')
  useEffect(() => { QRCode.toDataURL(text, { width: size * 2, margin: 1 }).then(setSrc).catch(() => setSrc('')) }, [text, size])
  return src ? <img src={src} width={size} height={size} alt="二维码" style={{ display: 'block', margin: '0 auto', borderRadius: 8 }} /> : null
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

export function ErrorNote({ error }: { error?: string }) {
  return error ? <div className="note bad">{error}</div> : null
}

// lines <-> textarea helpers
export const toLines = (v?: string[]) => (v ?? []).join('\n')
export const fromLines = (s: string) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean)
