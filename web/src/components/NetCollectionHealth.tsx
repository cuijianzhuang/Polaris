import { useEffect, useRef, useState } from 'react'
import { datetime } from '../format'
import { t } from '../i18n'
import type { NetCollectionHealth as Health } from '../types'

function duration(seconds: number) {
  if (seconds >= 3600) return t('{n} 小时', { n: Math.floor(seconds / 3600) })
  if (seconds >= 60) return t('{n} 分钟', { n: Math.floor(seconds / 60) })
  return t('{n} 秒', { n: Math.floor(seconds) })
}

function reasonLabel(reason?: string) {
  return ({ initial: t('首次有效采样'), boot_changed: t('主机启动来源变化'), interfaces_changed: t('网络接口变化'),
    format_changed: t('采样格式变化'), source_changed: t('采样来源变化'), counters_decreased: t('网卡累计计数下降') } as Record<string, string>)[reason ?? ''] ?? t('未记录')
}

export default function NetCollectionHealth({ health, compact, onRefresh }: { health?: Health; compact?: boolean; onRefresh?: () => void }) {
  // Advance the server-reported age with a monotonic clock. An offline node
  // may stop producing websocket updates, and client wall clocks can differ.
  const reference = useRef({ snapshot: health?.checkedAt, received: performance.now() })
  const [, tick] = useState(0)
  if (reference.current.snapshot !== health?.checkedAt) reference.current = { snapshot: health?.checkedAt, received: performance.now() }
  const hasSample = !!health?.lastSampleAt
  useEffect(() => {
    if (!hasSample) return
    const timer = setInterval(() => tick((n) => n + 1), 10000)
    return () => clearInterval(timer)
  }, [hasSample])
  if (!health) return null
  const age = (health.sampleAgeSeconds ?? 0) + Math.max(0, (performance.now() - (health.receivedAtMonotonic ?? reference.current.received)) / 1000)
  const stale = hasSample && (health.state === 'stale' || age >= health.staleAfterSeconds)
  const stateLabel = !hasSample ? t('暂无有效采样') : stale ? t('采样较旧') : t('采样正常')
  const badge = <span className={'tag ' + (!hasSample ? '' : stale ? 'warn' : 'ok')}>{stateLabel}</span>
  const old = health.mode === 'legacy' || health.mode === 'epoch'
  if (compact) return <div style={{ marginTop: 6 }}>
    {badge}
    {hasSample && <div className="small mute">{stale ? t('已 {duration} 无成功采样', { duration: duration(age) }) : t('最近成功：{at}', { at: datetime(health.lastSampleAt) })}</div>}
    {old && <div className="small" style={{ color: 'var(--warn)' }}>{t('建议升级 Agent 完善来源识别')}</div>}
  </div>
  const modes = { boot: t('主机与网卡来源识别'), epoch: t('仅网卡来源识别'), legacy: t('旧版累计计数'), unknown: t('未确认采样格式') }
  return <section className="card" aria-label={t('网卡流量采样')}>
    <div className="ctitle"><span>{t('网卡流量采样')} {badge}</span>{onRefresh && <button className="btn sm" type="button" onClick={onRefresh}>{t('刷新')}</button>}</div>
    {stale && <div className="note warn">{t('已 {duration} 无成功采样', { duration: duration(age) })}</div>}
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 12, marginTop: 10 }}>
      <div><div className="small mute">{t('最近成功采样')}</div><div className="mono small">{datetime(health.lastSampleAt)}</div></div>
      <div><div className="small mute">{t('采样来源识别')}</div><div>{modes[health.mode]}</div></div>
      <div><div className="small mute">{t('最近基线重建')}</div><div>{reasonLabel(health.baselineReason)}</div><div className="mono small mute">{datetime(health.baselineAt)}</div></div>
      <div><div className="small mute">{t('最近长采样间隔')}</div><div>{health.lastGapAt ? duration(health.lastGapSeconds) : t('未记录')}</div>{health.lastGapAt && <div className="small mute">{t('恢复于 {at}', { at: datetime(health.lastGapAt) })}</div>}</div>
    </div>
    {!hasSample && <p className="hint">{t('等待有效网卡流量样本；节点在线不代表流量采样正常。')}</p>}
    {old && <p className="hint">{t('建议升级 Agent 完善来源识别')}</p>}
    <p className="hint">{t('超过 {duration} 无成功采样时标记为较旧。长间隔不代表丢量：相同来源的累计计数可继续补齐；来源变化期间的流量无法准确还原。', { duration: duration(health.staleAfterSeconds) })}</p>
  </section>
}
