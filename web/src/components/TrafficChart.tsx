import { useMemo, useState } from 'react'
import { get } from '../api'
import { bytes } from '../format'
import { useLoad, usePoll, useWidth } from './ui'

type Range = '24h' | '7d' | '30d'
const ranges: { k: Range; label: string; n: number; hour: boolean }[] = [
  { k: '24h', label: '24 小时', n: 24, hour: true },
  { k: '7d', label: '7 天', n: 7, hour: false },
  { k: '30d', label: '30 天', n: 30, hour: false },
]
const pad = (n: number) => String(n).padStart(2, '0')
const dayKey = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`

// TrafficChart shows hourly (24 h) or daily (7 / 30 d) traffic for a user or
// node (query: "user=<id>" or "node=<id>") as stacked bars with a tooltip.
export default function TrafficChart({ query, title }: { query: string; title: string }) {
  const [range, setRange] = useState<Range>('7d')
  const r = ranges.find((x) => x.k === range)!
  const from = useMemo(() => {
    const d = new Date()
    if (r.hour) { d.setMinutes(0, 0, 0); d.setHours(d.getHours() - (r.n - 1)) } else { d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - (r.n - 1)) }
    return d
  }, [r])
  const data = useLoad(() => get<{ series: { t: string; up: number; down: number }[] }>(
    `/stats/traffic?${query}&bucket=${r.hour ? 'hour' : 'day'}&from=${encodeURIComponent(from.toISOString())}`), [query, range])
  usePoll(data.reload, 30000)
  const pts = useMemo(() => {
    const m = new Map<string, { up: number; down: number }>()
    for (const p of data.data?.series ?? []) {
      const d = new Date(p.t)
      const k = r.hour ? `${dayKey(d)} ${pad(d.getHours())}` : dayKey(d)
      const v = m.get(k) ?? { up: 0, down: 0 }
      m.set(k, { up: v.up + p.up, down: v.down + p.down })
    }
    return Array.from({ length: r.n }, (_, i) => {
      const d = new Date(from)
      if (r.hour) d.setHours(d.getHours() + i); else d.setDate(d.getDate() + i)
      const k = r.hour ? `${dayKey(d)} ${pad(d.getHours())}` : dayKey(d)
      const v = m.get(k) ?? { up: 0, down: 0 }
      return { d, label: r.hour ? `${pad(d.getHours())}:00` : `${d.getMonth() + 1}/${d.getDate()}`, ...v }
    })
  }, [data.data, from, r])
  const [ref, W] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const up = pts.reduce((a, p) => a + p.up, 0), down = pts.reduce((a, p) => a + p.down, 0)
  const max = Math.max(1, ...pts.map((p) => p.up + p.down))
  const H = 150, L = 62, B = 124, T = 8, R = Math.max(L + 10, W - 4)
  const bw = (R - L) / pts.length
  const y = (v: number) => (v / max) * (B - T)
  const every = Math.max(1, Math.ceil(pts.length / Math.max(2, Math.floor((R - L) / 44))))
  const h = hover !== null ? pts[hover] : null
  return (
    <div className="tchart">
      <div className="ctitle">
        <span>{title}</span>
        <span className="seg-mini">{ranges.map((x) => <button key={x.k} type="button" className={range === x.k ? 'on' : ''} onClick={() => setRange(x.k)}>{x.label}</button>)}</span>
      </div>
      <div className="tchart-sum">
        <div><span>合计</span><b>{bytes(up + down)}</b></div>
        <div><span><i style={{ background: 'var(--pri)' }} />下行</span><b>{bytes(down)}</b></div>
        <div><span><i style={{ background: 'var(--warn)' }} />上行</span><b>{bytes(up)}</b></div>
        <div><span>峰值 / {r.hour ? '小时' : '天'}</span><b>{bytes(max === 1 ? 0 : max)}</b></div>
      </div>
      <div ref={ref} className="trend-wrap" style={{ marginTop: 8 }}>
        {W > 0 && (
          <svg width={W} height={H} role="img" aria-label={title}>
            <g style={{ stroke: 'var(--line-2)' }}>{[0, 0.5, 1].map((f) => <line key={f} x1={L} x2={R} y1={B - f * (B - T)} y2={B - f * (B - T)} style={f === 0 ? { stroke: 'var(--input)' } : undefined} />)}</g>
            <g style={{ fill: 'var(--mute)' }} fontSize="11" fontFamily="IBM Plex Mono">
              {[0.5, 1].map((f) => <text key={f} x={L - 6} y={B - f * (B - T) + 4} textAnchor="end">{bytes(max * f)}</text>)}
              <text x={L - 6} y={B + 4} textAnchor="end">0</text>
              {pts.map((p, i) => (pts.length - 1 - i) % every === 0 && <text key={i} x={L + bw * (i + 0.5)} y={142} textAnchor="middle">{p.label}</text>)}
            </g>
            {pts.map((p, i) => {
              const x = L + bw * i + bw * 0.15, w = Math.max(1, bw * 0.7)
              return <g key={i} opacity={hover === null || hover === i ? 1 : 0.45}>
                <rect x={x} y={B - y(p.down)} width={w} height={y(p.down)} rx={Math.min(2, w / 3)} style={{ fill: 'var(--pri)' }} />
                <rect x={x} y={B - y(p.down) - y(p.up)} width={w} height={y(p.up)} rx={Math.min(2, w / 3)} style={{ fill: 'var(--warn)' }} />
              </g>
            })}
            <rect x={L} y={T} width={R - L} height={B - T} fill="transparent"
              onPointerMove={(e) => { const b = e.currentTarget.getBoundingClientRect(); setHover(Math.min(pts.length - 1, Math.max(0, Math.floor(((e.clientX - b.left) / b.width) * pts.length)))) }}
              onPointerLeave={() => setHover(null)} />
          </svg>
        )}
        {h && hover !== null && (
          <div className="tip" style={{ left: Math.min(Math.max(L + bw * (hover + 0.5), 70), W - 70), top: 0 }}>
            <div className="tip-t">{r.hour ? `${dayKey(h.d)} ${h.label}` : dayKey(h.d)}</div>
            <div><i style={{ background: 'var(--pri)' }} />下行 <b>{bytes(h.down)}</b></div>
            <div><i style={{ background: 'var(--warn)' }} />上行 <b>{bytes(h.up)}</b></div>
          </div>
        )}
      </div>
    </div>
  )
}
