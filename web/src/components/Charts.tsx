import { useState, type ReactNode } from 'react'
import { useWidth } from './ui'
import { t } from '../i18n'

// Small SVG charts shared by the statistics pages (same look as TrafficChart).

export type BarPoint = { label: string; tip: string; up: number; down: number }

// StackedBars: download (primary) under upload (warn) per point. With
// onSelect, dragging across bars selects a span (Grafana-style zoom) and a
// click selects one bar.
export function StackedBars({ pts, fmt, height = 170, onSelect }: { pts: BarPoint[]; fmt: (n: number) => string; height?: number; onSelect?: (a: number, b: number) => void }) {
  const [ref, W] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const [drag, setDrag] = useState<[number, number] | null>(null)
  const H = height, L = 62, B = H - 26, T = 8, R = Math.max(L + 10, W - 4)
  const max = Math.max(1, ...pts.map((p) => p.up + p.down))
  const bw = (R - L) / Math.max(1, pts.length)
  const y = (v: number) => (v / max) * (B - T)
  const every = Math.max(1, Math.ceil(pts.length / Math.max(2, Math.floor((R - L) / 48))))
  const h = hover !== null ? pts[hover] : null
  return (
    <div ref={ref} className="trend-wrap">
      {W > 0 && (
        <svg width={W} height={H} role="img">
          <g style={{ stroke: 'var(--line-2)' }}>{[0, 0.5, 1].map((f) => <line key={f} x1={L} x2={R} y1={B - f * (B - T)} y2={B - f * (B - T)} style={f === 0 ? { stroke: 'var(--input)' } : undefined} />)}</g>
          <g style={{ fill: 'var(--mute)' }} fontSize="11" fontFamily="IBM Plex Mono">
            {[0.5, 1].map((f) => <text key={f} x={L - 6} y={B - f * (B - T) + 4} textAnchor="end">{fmt(max * f)}</text>)}
            <text x={L - 6} y={B + 4} textAnchor="end">0</text>
            {pts.map((p, i) => (pts.length - 1 - i) % every === 0 && <text key={i} x={Math.min(L + bw * (i + 0.5), R - 2)} y={H - 8} textAnchor={L + bw * (i + 0.5) > R - 18 ? 'end' : 'middle'}>{p.label}</text>)}
          </g>
          {pts.map((p, i) => {
            const x = L + bw * i + bw * 0.15, w = Math.max(1, bw * 0.7)
            return <g key={i} opacity={hover === null || hover === i ? 1 : 0.45}>
              <rect x={x} y={B - y(p.down)} width={w} height={y(p.down)} rx={Math.min(2, w / 3)} style={{ fill: 'var(--pri)' }} />
              <rect x={x} y={B - y(p.down) - y(p.up)} width={w} height={y(p.up)} rx={Math.min(2, w / 3)} style={{ fill: 'var(--warn)' }} />
            </g>
          })}
          {drag && <rect x={L + bw * Math.min(...drag)} y={T} width={bw * (Math.abs(drag[1] - drag[0]) + 1)} height={B - T} style={{ fill: 'var(--pri)', opacity: 0.12 }} />}
          <rect x={L} y={T} width={R - L} height={B - T} fill="transparent" style={onSelect ? { cursor: 'crosshair', touchAction: 'pan-y' } : undefined}
            onPointerDown={(e) => { if (!onSelect || hover === null) return; e.currentTarget.setPointerCapture(e.pointerId); setDrag([hover, hover]) }}
            onPointerMove={(e) => {
              const b = e.currentTarget.getBoundingClientRect()
              const i = Math.min(pts.length - 1, Math.max(0, Math.floor(((e.clientX - b.left) / b.width) * pts.length)))
              setHover(i)
              if (drag) setDrag([drag[0], i])
            }}
            onPointerUp={() => { if (drag && onSelect) onSelect(Math.min(...drag), Math.max(...drag)); setDrag(null) }}
            onPointerLeave={() => { if (!drag) setHover(null) }} />
        </svg>
      )}
      {h && hover !== null && (
        <div className="tip" style={{ left: Math.min(Math.max(L + bw * (hover + 0.5), 70), W - 70), top: 0 }}>
          <div className="tip-t">{drag && drag[0] !== drag[1] ? `${pts[Math.min(...drag)].tip} ~ ${pts[Math.max(...drag)].tip}` : h.tip}</div>
          <div><i style={{ background: 'var(--pri)' }} />{t('下行')} <b>{fmt(h.down)}</b></div>
          <div><i style={{ background: 'var(--warn)' }} />{t('上行')} <b>{fmt(h.up)}</b></div>
        </div>
      )}
    </div>
  )
}

export type Series = { name: string; color: string; values: (number | null)[] }

// Lines: one or more series over shared x labels.
// segments splits a series at missing values (indexes of each run).
function segments(vs: (number | null)[]): number[][] {
  const out: number[][] = []
  let cur: number[] = []
  vs.forEach((v, i) => { if (v == null) { if (cur.length) out.push(cur); cur = [] } else cur.push(i) })
  if (cur.length) out.push(cur)
  return out
}

export function Lines({ labels, series, fmt, max: fixedMax, height = 160 }: { labels: string[]; series: Series[]; fmt: (n: number) => string; max?: number; height?: number }) {
  const [ref, W] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const H = height, L = 62, B = H - 26, T = 8, R = Math.max(L + 10, W - 4)
  const n = labels.length
  const max = fixedMax ?? Math.max(1, ...series.flatMap((s) => s.values.filter((v): v is number => v != null)))
  const x = (i: number) => L + (n <= 1 ? 0 : (i * (R - L)) / (n - 1))
  const y = (v: number) => B - (Math.min(v, max) / max) * (B - T)
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor((R - L) / 64))))
  let tip: ReactNode = null
  if (hover !== null && n > 0) {
    tip = (
      <div className="tip" style={{ left: Math.min(Math.max(x(hover), 70), W - 70), top: 0 }}>
        <div className="tip-t">{labels[hover]}</div>
        {series.map((s) => <div key={s.name}><i style={{ background: s.color }} />{s.name} <b>{s.values[hover] == null ? '—' : fmt(s.values[hover] as number)}</b></div>)}
      </div>
    )
  }
  return (
    <div ref={ref} className="trend-wrap">
      {W > 0 && (
        <svg width={W} height={H} role="img">
          <g style={{ stroke: 'var(--line-2)' }}>{[0, 0.5, 1].map((f) => <line key={f} x1={L} x2={R} y1={B - f * (B - T)} y2={B - f * (B - T)} style={f === 0 ? { stroke: 'var(--input)' } : undefined} />)}</g>
          <g style={{ fill: 'var(--mute)' }} fontSize="11" fontFamily="IBM Plex Mono">
            {[0.5, 1].map((f) => <text key={f} x={L - 6} y={B - f * (B - T) + 4} textAnchor="end">{fmt(max * f)}</text>)}
            <text x={L - 6} y={B + 4} textAnchor="end">0</text>
            {labels.map((l, i) => (n - 1 - i) % every === 0 && <text key={i} x={x(i)} y={H - 8} textAnchor={i === n - 1 ? 'end' : i === 0 ? 'start' : 'middle'}>{l.slice(-5)}</text>)}
          </g>
          {series.map((s) => segments(s.values).map((seg, k) => seg.length === 1
            ? <circle key={s.name + k} cx={x(seg[0])} cy={y(s.values[seg[0]] as number)} r={1.6} fill={s.color} />
            : <polyline key={s.name + k} fill="none" stroke={s.color} strokeWidth={1.6} strokeLinejoin="round"
              points={seg.map((i) => `${x(i)},${y(s.values[i] as number)}`).join(' ')} />))}
          {hover !== null && <line x1={x(hover)} x2={x(hover)} y1={T} y2={B} style={{ stroke: 'var(--line)' }} />}
          <rect x={L} y={T} width={R - L} height={B - T} fill="transparent"
            onPointerMove={(e) => { const b = e.currentTarget.getBoundingClientRect(); setHover(Math.min(n - 1, Math.max(0, Math.round(((e.clientX - b.left) / b.width) * (n - 1))))) }}
            onPointerLeave={() => setHover(null)} />
        </svg>
      )}
      {tip}
    </div>
  )
}
