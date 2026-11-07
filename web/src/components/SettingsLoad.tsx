import { useState, type Dispatch, type ReactNode, type SetStateAction } from 'react'
import { get } from '../api'
import { t } from '../i18n'
import { useLoad } from './ui'

export type Settings = Record<string, any>
export async function readSettings(signal?: AbortSignal): Promise<Settings> {
  const data = await get<Settings>('/settings', signal)
  if (!data || typeof data !== 'object' || Array.isArray(data)) throw new Error(t('设置响应格式无效，请重试。'))
  return Object.fromEntries(Object.entries(data).map(([key, value]) => {
    // Configuration values are strings. Credential/readiness indicators are
    // boolean metadata and must keep false as false.
    const flag = key.endsWith('_set') || key === 'acme_ready' || key === 'acme_http_ready'
    return [key, !flag && (typeof value === 'boolean' || typeof value === 'number') ? String(value) : value]
  }))
}

type Load = { data: Settings | undefined; loading: boolean; error: string; reload: () => void }
// Failed reads are never rendered as an empty, editable configuration.
export function SettingsGate({ state, children, ready = true, compact = false }: { state: Load; children?: ReactNode; ready?: boolean; compact?: boolean }) {
  if (compact && state.error) return <span role="alert" className="small"><span>{t('无法读取设置，请重试。')}</span> <button className="btn sm" type="button" aria-label={t('重试')} disabled={state.loading} onClick={state.reload}>{t('重试')}</button></span>
  if (compact && (!state.data || !ready)) return <span role="status" aria-busy="true">{t('加载中…')}</span>
  if (state.error) return <div className="card" role="alert"><p>{t('无法读取设置，请重试。')}</p><p className="small mute">{state.error}</p><button className="btn" type="button" aria-label={t('重试')} disabled={state.loading} onClick={state.reload}>{t('重试')}</button></div>
  if (!state.data || !ready) return <div className="card" role="status" aria-busy="true">{t('加载中…')}</div>
  return children
}

// Derive initial fields from the actual response. Key local edits by that
// response, so polling unrelated runtime state never resets a switch draft.
export function useSettingsForm(defaults: Record<string, string>) {
  const state = useLoad(readSettings)
  const [draft, setDraft] = useState<{ source: Settings; fields: Record<string, string> } | null>(null)
  const initial = Object.fromEntries(Object.entries(defaults).map(([key, fallback]) => [key, String(state.data?.[key] ?? fallback)]))
  const fields = draft && draft.source === state.data ? draft.fields : initial
  const setFields: Dispatch<SetStateAction<Record<string, string>>> = (change) => {
    if (!state.data || state.loading || state.error) return
    const source = state.data
    setDraft((previous) => {
      const current = previous?.source === source ? previous.fields : initial
      return { source, fields: typeof change === 'function' ? change(current) : change }
    })
  }
  const dirty = !!state.data && JSON.stringify(fields) !== JSON.stringify(initial)
  return { state, fields, setFields, dirty, reset: () => setDraft(null) }
}
