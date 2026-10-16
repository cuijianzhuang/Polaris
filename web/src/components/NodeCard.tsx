import { useEffect, useState, type ReactNode } from 'react'
import { bps, bytes, coreShort, coreTagClass, nodeStatus } from '../format'
import type { Node, NodeTraffic } from '../types'

// Rate history per node, filled from heartbeats as they arrive (kept across
// page switches for the session).
const MAX = 40
const history = new Map<string, { at: string; rx: number; tx: number }[]>()
const subs = new Set<() => void>()

export function recordRates(nodes: Node[]) {
  let changed = false
  for (const n of nodes) {
    if (!n.connected || !n.lastSeen) continue
    const h = history.get(n.id) ?? []
    if (h.length && h[h.length - 1].at === n.lastSeen) continue
    h.push({ at: n.lastSeen, rx: n.system.rxBps, tx: n.system.txBps })
    if (h.length > MAX) h.shift()
    history.set(n.id, h)
    changed = true
  }
  if (changed) subs.forEach((f) => f())
}

function useHistory(id: string) {
  const [, tick] = useState(0)
  useEffect(() => { const f = () => tick((x) => x + 1); subs.add(f); return () => { subs.delete(f) } }, [])
  return history.get(id) ?? []
}

function Ring({ value, label }: { value: number; label: string }) {
  const v = Math.max(0, Math.min(100, value || 0))
  const r = 17, c = 2 * Math.PI * r
  const color = v >= 85 ? 'var(--bad)' : v >= 60 ? 'var(--warn)' : 'var(--ok)'
  return (
    <div className="ring" title={`${label} ${v.toFixed(1)}%`}>
      <svg width="44" height="44" viewBox="0 0 44 44" aria-hidden="true">
        <circle cx="22" cy="22" r={r} fill="none" style={{ stroke: 'var(--line-2)' }} strokeWidth="4" />
        <circle cx="22" cy="22" r={r} fill="none" stroke={color} strokeWidth="4" strokeLinecap="round"
          strokeDasharray={`${(v / 100) * c} ${c}`} transform="rotate(-90 22 22)" style={{ transition: 'stroke-dasharray .6s var(--ease-out)' }} />
      </svg>
      <span className="ring-v">{Math.round(v)}<small>%</small></span>
      <span className="ring-l">{label}</span>
    </div>
  )
}

function Spark({ id }: { id: string }) {
  const h = useHistory(id)
  const W = 120, H = 36
  if (h.length < 2) return <div className="spark-empty">收集速率中…</div>
  const max = Math.max(1, ...h.map((p) => p.rx + p.tx))
  const pts = h.map((p, i) => `${((i / (MAX - 1)) * W + (W - ((h.length - 1) / (MAX - 1)) * W)).toFixed(1)},${(H - 2 - ((p.rx + p.tx) / max) * (H - 6)).toFixed(1)}`)
  const first = pts[0].split(',')[0]
  return (
    <svg className="spark" width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
      <path d={`M${first},${H} L${pts.join(' L')} L${W},${H} Z`} style={{ fill: 'color-mix(in srgb, var(--pri) 14%, transparent)' }} />
      <path d={`M${pts.join(' L')}`} fill="none" style={{ stroke: 'var(--pri)' }} strokeWidth="1.6" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
    </svg>
  )
}

// NodeCard is one tile of the node wall: status, live rate with a sparkline,
// CPU and memory rings, today's traffic and cores.
export default function NodeCard({ n, on, onClick, traffic, badge }: { n: Node; on: boolean; onClick: () => void; traffic?: NodeTraffic; badge?: ReactNode }) {
  const st = nodeStatus[n.status] ?? nodeStatus.pending
  const addr = n.address || n.domain || n.publicIpv4 || n.publicIpv6
  return (
    <button type="button" className={'ncard' + (on ? ' on' : '') + (n.connected ? '' : ' down')} aria-pressed={on} onClick={onClick}>
      <div className="nc-head">
        <span className="nc-name"><span className={'dot' + (n.status === 'online' ? ' live' : '')} style={{ background: st.color, color: st.color }} />{n.flag ? n.flag + ' ' : ''}{n.name}</span>
        <span className={'st ' + st.cls}>{st.label}</span>
      </div>
      <div className="nc-sub">{[n.region, addr].filter(Boolean).join(' · ') || <span style={{ color: 'var(--warn-ink)' }}>无公网地址</span>}{badge}</div>
      {n.connected ? (
        <div className="nc-body">
          <div className="nc-rate">
            <div className="nc-rate-v"><span>↓ {bps(n.system.rxBps)}</span><span>↑ {bps(n.system.txBps)}</span></div>
            <Spark id={n.id} />
          </div>
          <Ring value={n.system.cpu} label="CPU" />
          <Ring value={n.system.mem} label="内存" />
        </div>
      ) : <div className="nc-off">{n.status === 'pending' ? '等待 Agent 接入' : '节点离线，暂无实时数据'}</div>}
      <div className="nc-foot">
        <span className="mono">{traffic ? `今日 ${bytes(traffic.todayUp + traffic.todayDown)}` : '今日 —'}</span>
        <span>{n.cores.map((c) => <span key={c.id} className={coreTagClass(c.type)}>{coreShort[c.type]}</span>)}</span>
      </div>
    </button>
  )
}
