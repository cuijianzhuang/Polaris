import { memo, useEffect, useState } from 'react'
import { bytes, bps, datetime, flag, uptime } from '../format'
import { t } from '../i18n'
import { attention, dateMs, daysLeft, freshLatency, freshTelemetry, nodeTargets, percent } from '../probeMonitor'
import type { ProbeNode, ProbeTarget, ProbeView } from '../probeMonitor'
import '../../../internal/panel/probe_assets/monitor.css'

export const carrierColor: Record<string, string> = { ct: '#3b82f6', cu: '#e8795b', cm: '#10b981' }
export const colorOf = (c: string) => carrierColor[c] ?? '#a78bfa'
export const usageColor = (p: number) => p >= 85 ? 'var(--pv-red)' : p >= 60 ? 'var(--pv-yellow)' : 'var(--pv-green)'
const lossColor = (v: number) => v >= 5 ? 'var(--pv-red)' : v >= 1 ? 'var(--pv-yellow)' : 'var(--pv-green)'
export const pingColor = (ms: number | null | undefined) => ms == null ? 'var(--pv-mute)' : ms <= 0 ? 'var(--pv-red)' : ms <= 80 ? 'var(--pv-green)' : ms <= 160 ? 'var(--pv-cyan)' : ms <= 260 ? 'var(--pv-yellow)' : 'var(--pv-red)'

// Advance from the server snapshot using elapsed browser time. A bad browser
// wall clock cannot make freshly reported telemetry appear years out of date.
export function useProbeClock(at?: string) {
  const [base, setBase] = useState({ server: Date.now(), received: performance.now() })
  const [now, setNow] = useState(performance.now())
  useEffect(() => { const received = performance.now(); setBase({ server: Number.isFinite(dateMs(at)) ? dateMs(at) : Date.now(), received }); setNow(received) }, [at])
  useEffect(() => { const id = setInterval(() => setNow(performance.now()), 5000); return () => clearInterval(id) }, [])
  return base.server + Math.max(0, now - base.received)
}

export function ProbeBadge({ n, now }: { n: ProbeNode; now: number }) {
  const state = !n.online ? 'off' : !freshTelemetry(n, now) ? 'stale' : n.status === 'error' ? 'error' : 'on'
  return <span className={'pv-st ' + state}><i />{state === 'off' ? t('离线') : state === 'stale' ? t('数据滞后') : state === 'error' ? t('配置异常') : t('在线')}</span>
}

export function NodeBadges({ n, now = Date.now() }: { n: ProbeNode; now?: number }) {
  const d = daysLeft(n.expireAt, now)
  const size = [n.cores ? n.cores + 'C' : '', n.memTotal ? bytes(n.memTotal).replace(' ', '') : '', n.diskTotal ? bytes(n.diskTotal).replace(' ', '') : ''].filter(Boolean).join(' / ')
  return <div className="pv-meta">
    {n.os && <span className="pv-badge pv-os" title={n.os}>{n.os}</span>}
    {size && <span className="pv-badge">{size}</span>}
    {(n.ipv4 || n.ipv6) && <span className="pv-badge v4">{n.ipv4 && n.ipv6 ? 'IPv4/6' : n.ipv4 ? 'IPv4' : 'IPv6'}</span>}
    {n.price && <span className="pv-badge cost">{n.price}</span>}
    {d != null && <span className={'pv-badge exp' + (d <= 7 ? ' late' : '')}>{d >= 0 ? t('剩 {n} 天', { n: d }) : t('已过期')}</span>}
  </div>
}

function Meter({ label, value }: { label: string; value: number | null }) {
  return <div className="pv-row"><span className="pv-k">{label}</span><span className="pv-track"><i style={{ width: (value == null ? 0 : percent(value)) + '%', background: usageColor(value ?? 0) }} /></span><span className="pv-val">{value == null ? '—' : value.toFixed(1) + '%'}</span></div>
}
function Ring({ label, value }: { label: string; value: number | null }) {
  const circumference = 2 * Math.PI * 31
  return <div className="pv-ring"><svg viewBox="0 0 80 80" role="img" aria-label={label + ' ' + (value == null ? t('等待上报') : value.toFixed(1) + '%')}><circle cx="40" cy="40" r="31" className="pv-ring-track" /><circle cx="40" cy="40" r="31" className="pv-ring-fill" style={{ stroke: value == null ? 'transparent' : usageColor(value), strokeDasharray: circumference, strokeDashoffset: circumference * (1 - percent(value ?? 0) / 100) }} /><text x="40" y="43" textAnchor="middle">{value == null ? '—' : value.toFixed(0) + '%'}</text></svg><span>{label}</span></div>
}

const noSamples: (number | null)[] = Array(25).fill(null)
const bucketTime = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' })
const Buckets = memo(function Buckets({ values, losses, start, loss = false }: { values: (number | null)[]; losses?: (number | null)[]; start?: string; loss?: boolean }) {
  const max = Math.max(1, ...values.filter((v): v is number => v != null))
  const startAt = dateMs(start)
  return <span className={'pv-bk' + (loss ? ' pv-bk-loss' : '')}>
    {values.map((v, i) => {
      const failed = !loss && v == null && losses?.[i] === 100
      const when = Number.isFinite(startAt) ? bucketTime.format(startAt + i * 300000) + ' · ' : ''
      const tip = when + (failed ? t('本时段全部探测失败') : v == null ? t('本时段无样本') : loss ? t('探测失败率 {n}%', { n: v.toFixed(1) }) : t('平均 {n} ms', { n: v.toFixed(1) }))
      return <span key={i} className={failed ? 'failed' : v == null ? 'missing' : ''} title={tip}><i style={{ height: v == null ? 0 : loss ? Math.max(4, percent(v)) + '%' : Math.max(8, v / max * 100) + '%', background: loss ? lossColor(v ?? 0) : pingColor(v) }} /></span>
    })}
  </span>
})

export function LatencyRows({ n, targets, now = Date.now() }: { n: ProbeNode; targets: ProbeTarget[]; now?: number }) {
  const ts = nodeTargets(n, targets)
  if (!ts.length) return <div className="pv-lat-empty">{t('此节点未启用延迟探测')}</div>
  return <div className="pv-lat">
    <div className="pv-lat-head"><span>{t('网络质量')} <small>TCP</small></span><span title={t('延迟为最近一次探测，失败率按近 1 小时样本统计。')}>{t('延迟 / 失败率')}</span></div>
    {ts.slice(0,3).map((tg) => {
      const l = n.latency[tg.id], live = n.online && freshLatency(l, now)
      return <div className="pv-quality" key={tg.id}>
        <div className="pv-quality-title"><span className="pv-ln" title={tg.name}><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span><b style={{ color: live ? pingColor(l.ms) : 'var(--pv-mute)' }}>{!live ? l ? t('数据过期') : t('待采集') : l.ms > 0 ? l.ms.toFixed(l.ms < 1 ? 1 : 0) + ' ms' : t('超时')}</b><span style={{ color: live ? lossColor(l.loss) : 'var(--pv-mute)' }}>{live ? l.loss.toFixed(1) + '%' : '—'}</span></div>
        <div className="pv-quality-charts"><Buckets values={l?.spark ?? noSamples} losses={l?.sparkLoss} start={l?.sparkStart} /><Buckets values={l?.sparkLoss ?? noSamples} start={l?.sparkStart} loss /></div>
      </div>
    })}
    {ts.length > 3 && <div className="pv-lat-empty">{t('另有 {n} 个目标，点击查看', { n: ts.length - 3 })}</div>}
  </div>
}

export function NodeProbeCard({ n, targets, onOpen, view, now, showTraffic }: { n: ProbeNode; targets: ProbeTarget[]; onOpen: () => void; view: ProbeView; now: number; showTraffic: boolean }) {
  const fresh = freshTelemetry(n, now)
  const cpu = fresh ? n.cpu : null, mem = fresh ? n.mem : null, disk = fresh && (n.diskTotal ?? 0) > 0 ? n.disk : null
  return <button type="button" className={'pv-card' + (!n.online ? ' off' : attention(n, now) ? ' attention' : '')} onClick={onOpen} aria-label={t('查看节点 {name}', { name: n.name })}>
    <div className="pv-hd"><span className="pv-flag">{flag(n.flag) || '◎'}</span><span className="pv-identity"><strong className="pv-name">{n.name}</strong><span className="pv-region">{n.region || n.arch || '—'}</span></span><ProbeBadge n={n} now={now} /></div>
    <NodeBadges n={n} now={now} />
    {view === 'ring' ? <div className="pv-rings"><Ring label="CPU" value={cpu} /><Ring label="RAM" value={mem} /><Ring label="DISK" value={disk} /></div> : <div className="pv-rows"><Meter label="CPU" value={cpu} /><Meter label="RAM" value={mem} /><Meter label="DISK" value={disk} /></div>}
    <div className="pv-load"><span>LOAD <small>1m</small></span><b>{fresh ? n.load1.toFixed(2) : '—'}</b></div>
    <div className="pv-network"><span><small>{t('接收速率')}</small><b className="pv-dn">↓ {fresh ? bps(n.rxBps) : '—'}</b></span><span><small>{t('发送速率')}</small><b className="pv-up">↑ {fresh ? bps(n.txBps) : '—'}</b></span></div>
    {showTraffic && <div className="pv-traffic"><span>{t('本期流量')} <b>{bytes(n.netUsed)}</b></span><span>{n.netLimit > 0 ? '/ ' + bytes(n.netLimit) : t('不限')}</span>{n.netLimit > 0 && <span className="pv-track"><i style={{ width: percent(100 * n.netUsed / n.netLimit) + '%', background: usageColor(100 * n.netUsed / n.netLimit) }} /></span>}</div>}
    <LatencyRows n={n} targets={targets} now={now} />
    <div className="pv-card-foot"><span>{fresh ? t('运行 {time}', { time: uptime(n.uptime) }) : n.lastSeen ? datetime(n.lastSeen) : t('等待上报')}</span><span>{t('查看详情')} ↗</span></div>
  </button>
}

export function ProbeSummary({ nodes, now, showTraffic }: { nodes: ProbeNode[]; now: number; showTraffic: boolean }) {
  const on = nodes.filter((n) => n.online), fresh = nodes.filter((n) => freshTelemetry(n, now)), stale = on.length - fresh.length
  const rx = fresh.reduce((a, n) => a + n.rxBps, 0), tx = fresh.reduce((a, n) => a + n.txBps, 0)
  const cpu = fresh.length ? fresh.reduce((a, n) => a + n.cpu, 0) / fresh.length : null
  const mem = fresh.length ? fresh.reduce((a, n) => a + n.mem, 0) / fresh.length : null
  const regions = new Set(nodes.map((n) => n.flag).filter(Boolean)).size
  const tile = (label: string, value: string, hint: string, color = '') => <div className="pv-tile"><div className="k">{label}</div><div className={'v ' + color}>{value}</div><div className="pv-tile-hint">{hint}</div></div>
  return <div className="pv-sum">
    {tile(t('在线节点'), `${on.length} / ${nodes.length}`, t('离线 {off} · 数据滞后 {stale}', { off: nodes.length - on.length, stale }), 'g')}
    {tile(t('总接收速率'), fresh.length ? bps(rx) : '—', t('仅统计有效上报'), 'g')}
    {tile(t('总发送速率'), fresh.length ? bps(tx) : '—', t('仅统计有效上报'), 'b')}
    {tile(t('平均 CPU'), cpu == null ? '—' : cpu.toFixed(1) + '%', t('有效上报 {n} 台', { n: fresh.length }), 'c')}
    {tile(t('平均内存'), mem == null ? '—' : mem.toFixed(1) + '%', t('有效上报 {n} 台', { n: fresh.length }))}
    {showTraffic ? tile(t('本期流量'), bytes(nodes.reduce((a, n) => a + n.netUsed, 0)), t('按各节点计费周期统计'), 'y') : tile(t('覆盖地区'), String(regions), t('按公开节点统计'))}
  </div>
}

export function ProbeTable({ nodes, targets, onOpen, now, showTraffic }: { nodes: ProbeNode[]; targets: ProbeTarget[]; onOpen: (id: string) => void; now: number; showTraffic: boolean }) {
  const mini = (v: number | null) => <span className="pv-mini"><span className="pv-track"><i style={{ width: (v == null ? 0 : percent(v)) + '%', background: usageColor(v ?? 0) }} /></span>{v == null ? '—' : v.toFixed(0) + '%'}</span>
  return <div className="pv-scroll"><table className="pv-table"><thead><tr><th>{t('节点')}</th><th>{t('状态')}</th><th>CPU</th><th>RAM</th><th>DISK</th><th>{t('接收 / 发送')}</th>{showTraffic && <th>{t('本期流量')}</th>}<th>{t('网络质量')}</th><th>{t('运行')}</th></tr></thead><tbody>{nodes.map((n) => {
    const fresh = freshTelemetry(n, now)
    return <tr key={n.id}><td><button className="pv-node-link" onClick={() => onOpen(n.id)}>{flag(n.flag)} {n.name}</button></td><td><ProbeBadge n={n} now={now} /></td><td>{mini(fresh ? n.cpu : null)}</td><td>{mini(fresh ? n.mem : null)}</td><td>{mini(fresh && (n.diskTotal ?? 0) > 0 ? n.disk : null)}</td><td><span className="pv-dn">↓ {fresh ? bps(n.rxBps) : '—'}</span><br /><span className="pv-up">↑ {fresh ? bps(n.txBps) : '—'}</span></td>{showTraffic && <td>{bytes(n.netUsed)}{n.netLimit > 0 && <small className="pv-muted"> / {bytes(n.netLimit)}</small>}</td>}<td><div className="pv-pings">{nodeTargets(n, targets).map((tg) => { const l = n.latency[tg.id], live = n.online && freshLatency(l, now); return <span key={tg.id} title={tg.name} style={{ color: live ? pingColor(l.ms) : 'var(--pv-mute)' }}>{tg.name} {live ? l.ms > 0 ? l.ms.toFixed(l.ms < 1 ? 1 : 0) + ' ms' : t('超时') : '—'}</span> })}</div></td><td>{fresh ? uptime(n.uptime) : '—'}</td></tr>
  })}</tbody></table></div>
}
