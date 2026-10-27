import { createContext, useCallback, useContext, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { useBlocker } from 'react-router-dom'
import { errMsg } from '../format'
import { t } from '../i18n'
import { Modal, useToast } from './ui'

type Draft = { label: string; dirty: boolean; busy?: boolean; invalid?: boolean; save?: () => Promise<boolean>; discard?: () => void }
type DraftRef = { current: Draft }
const DraftContext = createContext<{ register: (id: string, ref: DraftRef) => () => void; update: () => void; leave: () => Promise<boolean> }>({ register: () => () => {}, update: () => {}, leave: async () => true })

// Drafts and secrets live only in mounted form state. Nothing is persisted to storage.
export function SettingsDraftProvider({ children }: { children: ReactNode }) {
  const entries = useRef(new Map<string, DraftRef>())
  const pending = useRef<((leave: boolean) => void) | null>(null)
  const [open, setOpen] = useState(false)
  const [saving, setSaving] = useState(false)
  const savingRef = useRef(false)
  const [error, setError] = useState('')
  const [, setVersion] = useState(0)
  const update = useCallback(() => setVersion((v) => v + 1), [])
  const drafts = () => [...entries.current.values()].map((ref) => ref.current).filter((d) => d.dirty || d.busy)
  const register = useCallback((id: string, ref: DraftRef) => {
    entries.current.set(id, ref)
    return () => { entries.current.delete(id) }
  }, [])
  const leave = useCallback(() => {
    if (!drafts().length) return Promise.resolve(true)
    if (pending.current) return Promise.resolve(false)
    setError(''); setOpen(true)
    return new Promise<boolean>((resolve) => { pending.current = resolve })
  }, [])
  const finish = (ok: boolean, discard = true) => {
    if (savingRef.current) return
    if (ok && discard) drafts().forEach((d) => d.discard?.())
    const resolve = pending.current
    pending.current = null; setOpen(false); resolve?.(ok)
  }
  const blocker = useBlocker(({ currentLocation, nextLocation }) => drafts().length > 0 &&
    (currentLocation.pathname !== nextLocation.pathname || currentLocation.search !== nextLocation.search || currentLocation.hash !== nextLocation.hash))
  const blockerRef = useRef(blocker)
  blockerRef.current = blocker
  useEffect(() => {
    if (blocker.state !== 'blocked') return
    void leave().then((ok) => { const current = blockerRef.current; if (current.state === 'blocked') { if (ok) current.proceed(); else current.reset() } })
  }, [blocker.state, leave])
  useEffect(() => {
    const unload = (e: BeforeUnloadEvent) => { if (drafts().length) { e.preventDefault(); e.returnValue = '' } }
    window.addEventListener('beforeunload', unload)
    return () => { window.removeEventListener('beforeunload', unload); pending.current?.(false) }
  }, [])
  const active = drafts()
  const busy = active.some((d) => d.busy)
  const canSave = active.every((d) => !!d.save && !d.invalid && !d.busy)
  const saveAndLeave = async () => {
    if (savingRef.current || !canSave) return
    savingRef.current = true; setSaving(true); setError('')
    let ok = true
    try {
      for (const d of drafts()) { if (d.save && !await d.save()) { ok = false; break } }
    } catch (e) { ok = false; setError(errMsg(e)) }
    savingRef.current = false; setSaving(false)
    if (ok) finish(true, false)
    else setError((old) => old || t('保存失败，输入已保留。请重试或继续编辑。'))
  }
  return <DraftContext.Provider value={{ register, update, leave }}>
    {children}
    {open && <Modal focusTrap instantClose dismissDisabled={saving} title={t('有尚未保存的更改')} onClose={() => finish(false)} footer={<>
      <button className="btn" type="button" disabled={saving} onClick={() => finish(false)} data-initial-focus>{t('继续编辑')}</button>
      <button className="btn danger" type="button" disabled={saving || busy} onClick={() => finish(true)}>{t('放弃修改')}</button>
      <button className="btn pri" type="button" disabled={saving || !canSave} onClick={saveAndLeave}>{saving ? t('保存中…') : t('保存后离开')}</button>
    </>}>
      <p>{active.map((d) => d.label).join(t('、'))}</p>
      {busy ? <p className="hint">{t('正在提交操作，请等待完成。')}</p> : !canSave && <p className="hint">{t('请先在当前页面修正输入或逐项应用更改。')}</p>}
      {error && <div className="note bad" role="alert">{error}</div>}
    </Modal>}
  </DraftContext.Provider>
}

export function useSettingsDraft(draft: Draft) {
  const id = useId()
  const { register, update } = useContext(DraftContext)
  const ref = useRef(draft)
  ref.current = draft
  useLayoutEffect(() => register(id, ref), [id, register])
  useLayoutEffect(update, [update, draft.dirty, draft.busy, draft.invalid, draft.label])
}
export const useSettingsLeave = () => useContext(DraftContext).leave

// The lock takes effect before React renders, so fast clicks cannot submit twice.
export function useSettingsSave() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const lock = useRef(false)
  const toast = useToast()
  const run = async (fn: () => Promise<unknown>, message = t('设置已保存')) => {
    if (lock.current) return false
    lock.current = true; setBusy(true); setError('')
    try { await fn(); toast(message); return true }
    catch (e) { setError(errMsg(e)); return false }
    finally { lock.current = false; setBusy(false) }
  }
  return { busy, error, run }
}

export function SettingsSaveBar({ dirty, busy, loading = false, error, disabled, onSave, label = t('保存') }: {
  dirty: boolean; busy: boolean; loading?: boolean; error?: string; disabled?: boolean; onSave: () => unknown; label?: string
}) {
  return <div className="settings-savebar" aria-busy={busy || loading}>
    <div className="row"><button className="btn pri" type="button" disabled={busy || loading || disabled || !dirty} onClick={onSave}>{busy ? t('保存中…') : label}</button>
      <span className="hint" role="status">{busy ? t('保存中…') : loading ? t('加载中…') : error ? t('保存失败，输入已保留') : dirty ? t('有尚未保存的更改') : t('设置已保存')}</span></div>
    {error && <div className="note bad" role="alert">{error}</div>}
  </div>
}
