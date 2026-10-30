import { useId, useState } from 'react'
import { BASE, get, post } from '../api'
import { Field, Switch, useAction, useLoad, usePoll } from './ui'
import { datetime } from '../format'
import { t } from '../i18n'
import type { Node } from '../types'

// Staged upgrades: a canary batch first, then the rest a batch at a time,
// each batch watched for a while before the next. See internal/panel/rollout.go.

export type StagedOpts = { staged: boolean; canary: number; batchSize: number; soakSeconds: number; probeInbounds: boolean }
export const defaultStaged: StagedOpts = { staged: true, canary: 1, batchSize: 4, soakSeconds: 120, probeInbounds: false }

type RolloutNode = { state: 'upgrading' | 'ok' | 'failed' | 'skipped' | 'rolled_back' | 'rollback_pending' | 'rolling_back' | 'rollback_failed' | 'queued'; error?: string; at: string }
export type Rollout = {
  id: string; kind: 'agent' | 'core'; coreType?: string; version: string; nodeIds: string[]; canary: number; batchSize: number
  soakSeconds: number; state: 'running' | 'paused' | 'done' | 'failed' | 'canceled' | 'rolling_back'; cursor: number; batch: string[]
  healthySince?: string; results: Record<string, RolloutNode>; lastError?: string; createdAt: string; updatedAt: string
}

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, Number.isFinite(v) ? Math.round(v) : lo))

export function StagedFields({ value, onChange }: { value: StagedOpts; onChange: (v: StagedOpts) => void }) {
  const set = (p: Partial<StagedOpts>) => onChange({ ...value, ...p })
  return (
    <div className="ops-stack" style={{ gap: 8 }}>
      <div className="row"><Switch on={value.staged} onChange={(v) => set({ staged: v })} label={t('小批升级（推荐）')} /><span>{t('小批升级（推荐）')}</span></div>
      <div className="sub small" style={{ margin: 0 }}>{value.staged
        ? t('先升级首批节点，确认运行正常一段时间后再继续；有节点失败会立即停止，内核升级会把本批退回原版本。')
        : t('一次性升级全部所选节点。')}</div>
      {value.staged && (
        <div className="form-grid">
          <Field label={t('首批节点数')}><input className="input" type="number" min={1} max={20} value={value.canary} onChange={(e) => set({ canary: clamp(+e.target.value, 1, 20) })} /></Field>
          <Field label={t('之后每批')}><input className="input" type="number" min={1} max={20} value={value.batchSize} onChange={(e) => set({ batchSize: clamp(+e.target.value, 1, 20) })} /></Field>
          <Field label={t('每批观察（秒）')}><input className="input" type="number" min={30} max={3600} value={value.soakSeconds} onChange={(e) => set({ soakSeconds: clamp(+e.target.value, 30, 3600) })} /></Field>
          <Field label={t('入站探测')}><div className="row"><Switch on={value.probeInbounds} onChange={(v) => set({ probeInbounds: v })} label={t('升级后探测入站可连')} /><span className="small">{t('升级后探测入站可连')}</span></div></Field>
        </div>
      )}
    </div>
  )
}

export function startRollout(body: Record<string, unknown>, o: StagedOpts) {
  return post<Rollout>('/rollouts', { ...body, canary: o.canary, batchSize: o.batchSize, soakSeconds: o.soakSeconds, probeInbounds: o.probeInbounds })
}

const stateText = (s: Rollout['state']) => ({ running: t('进行中'), paused: t('已暂停'), done: t('已完成'), failed: t('已停止（失败）'), canceled: t('已取消'), rolling_back: t('回滚中') })[s]
const nodeText = (s: RolloutNode['state']) => ({ upgrading: t('观察中'), ok: t('成功'), failed: t('失败'), skipped: t('已跳过'), rolled_back: t('已退回'), rollback_pending: t('等待回滚'), rolling_back: t('回滚中'), rollback_failed: t('回滚未确认'), queued: t('等待') })[s]
const nodeTone: Record<RolloutNode['state'], string> = { upgrading: 'info-tag', ok: 'ok', failed: 'bad', skipped: 'mute', rolled_back: 'warn', rollback_pending: 'warn', rolling_back: 'warn', rollback_failed: 'bad', queued: 'mute' }

const dismissedKey = 'polaris.rollouts.dismissed:' + BASE
const readDismissed = (): string[] => {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(dismissedKey) ?? '[]')
    return Array.isArray(value) ? value.filter((x): x is string => typeof x === 'string').slice(-20) : []
  } catch { return [] }
}
const isActive = (r: Rollout) => ['running', 'paused', 'rolling_back'].includes(r.state)

/** Show ongoing work first. Finished results are compact, expandable and
 * dismissible in this browser without changing the stored upgrade record. */
export default function RolloutPanel({ nodes, refresh }: { nodes: Node[]; refresh?: number }) {
  const list = useLoad(() => get<{ items: Rollout[] }>('/rollouts'), [refresh])
  const items = list.data?.items ?? []
  const r = items.find(isActive) ?? items[0]
  const active = !!r && isActive(r)
  usePoll(list.reload, active ? 5000 : 60000)
  const { busy, run } = useAction()
  const [dismissed, setDismissed] = useState(readDismissed)
  const [expandedId, setExpandedId] = useState('')
  const detailsId = useId()
  // A changed result is new information and must be shown again. Active work
  // is never hidden, even if an old dismissal happens to match its key.
  const resultKey = r ? JSON.stringify([r.id, r.state, r.updatedAt]) : ''
  if (!r || (!active && (dismissed.includes(resultKey) || Date.now() - new Date(r.updatedAt).getTime() > 86400_000))) return null
  const expanded = active || expandedId === r.id
  const dismiss = () => {
    const next = [...new Set([...readDismissed(), ...dismissed, resultKey])].slice(-20)
    setDismissed(next)
    try { localStorage.setItem(dismissedKey, JSON.stringify(next)) } catch { /* keep the current view dismissible without storage */ }
  }
  const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id
  const finished = Object.values(r.results ?? {}).filter((x) => !['upgrading', 'queued', 'rollback_pending', 'rolling_back'].includes(x.state)).length
  const pct = Math.round((finished / Math.max(r.nodeIds.length, 1)) * 100)
  const act = (a: string, ok: string) => run(async () => { await post(`/rollouts/${r.id}/${a}`); list.reload() }, ok)
  const what = r.kind === 'agent' ? 'Agent' : r.coreType
  return (
    <section className="card" aria-label={t('升级任务')} style={{ marginBottom: 12 }}>
      <div className="ctitle" style={{ flexWrap: 'wrap', gap: 8, marginBottom: expanded ? undefined : 0 }}>
        <span>{t('小批升级：{what} → v{v}', { what: what ?? '', v: r.version.replace(/^v/, '') })} <span className={'tag ' + (r.state === 'failed' ? 'bad' : r.state === 'done' ? 'ok' : r.state === 'paused' ? 'warn' : 'info-tag')}>{stateText(r.state)}</span></span>
        <span className="row" style={{ flexWrap: 'wrap' }}>
          {!active && <>
            <button className="btn sm" type="button" aria-expanded={expanded} aria-controls={detailsId} onClick={() => setExpandedId(expanded ? '' : r.id)}>{expanded ? t('收起') : t('查看详情')}</button>
            <button className="btn sm" type="button" aria-label={t('关闭升级结果')} title={t('仅隐藏此结果，不会删除升级记录')} onClick={dismiss}>{t('关闭')}</button>
          </>}
          {r.state === 'running' && <button className="btn sm" type="button" disabled={busy} onClick={() => act('pause', t('已暂停'))}>{t('暂停')}</button>}
          {r.state === 'paused' && <button className="btn sm" type="button" disabled={busy} onClick={() => act('resume', t('已继续'))}>{t('继续')}</button>}
          {active && r.state !== 'rolling_back' && <button className="btn sm danger" type="button" disabled={busy} onClick={() => act('cancel', t('已取消'))}>{t('取消')}</button>}
        </span>
      </div>
      <div id={detailsId} hidden={!expanded}>
        <div className="sub small" style={{ margin: '0 0 8px' }}>
          {t('共 {n} 个节点 · 首批 {c} 个，之后每批 {b} 个 · 每批观察 {s} 秒 · {at} 创建', { n: r.nodeIds.length, c: r.canary, b: r.batchSize, s: r.soakSeconds, at: datetime(r.createdAt) })}
        </div>
        <div role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} style={{ height: 6, borderRadius: 3, background: 'var(--line)', overflow: 'hidden' }}>
          <div style={{ width: pct + '%', height: '100%', background: r.state === 'failed' ? 'var(--bad)' : 'var(--pri)' }} />
        </div>
        {r.state === 'running' && r.batch.length > 0 && (
          <div className="small" style={{ marginTop: 8 }}>{r.healthySince ? t('本批运行正常，观察中：') : t('本批升级中，等待恢复正常：')}{r.batch.map(name).join(t('、'))}</div>
        )}
        {r.lastError && <div className="note bad small" style={{ marginTop: 8 }}>{r.lastError}</div>}
        <div className="row" style={{ flexWrap: 'wrap', gap: 6, marginTop: 8 }}>
          {r.nodeIds.map((id) => {
            const res = r.results?.[id]
            return <span key={id} className={'tag ' + (res ? nodeTone[res.state] : 'mute')} title={res?.error}>{name(id)} · {res ? nodeText(res.state) : t('等待')}</span>
          })}
        </div>
      </div>
    </section>
  )
}
