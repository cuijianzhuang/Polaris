import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { Empty } from '../components/ui'
import { COUNTRY_PATHS, MAP_H, MAP_W } from '../assets/world-map'
import { ago, bps, flag, nodeStatus } from '../format'
import { clusterPoints, countryCode, countryNumeric, project, resolveNodeGeo, ringOffsets, type GeoPoint } from '../nodeGeo'
import type { Node } from '../types'
import { t } from '../i18n'

type Tone = 'ok' | 'warn' | 'bad'
const tone = (n: Node): Tone => (n.status === 'online' ? 'ok' : n.status === 'offline' || n.status === 'error' ? 'bad' : 'warn')
const toneRank: Record<Tone, number> = { bad: 0, warn: 1, ok: 2 }

const MAX_ZOOM = 14
const MARKER_GAP = 16 // px; closer markers are fanned out on a ring
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v))

interface View { k: number; tx: number; ty: number }
interface Placed { node: Node; geo: GeoPoint; px: number; py: number }

// The land never changes, so it is drawn once and not on every pan / zoom.
const Land = memo(function Land({ hot }: { hot: number[] }) {
  const on = new Set(hot)
  return (
    <>
      {Object.entries(COUNTRY_PATHS).map(([id, d]) => <path key={id} d={d} className={on.has(+id) ? 'nm-land on' : 'nm-land'} />)}
    </>
  )
})

function Popover({ n, geo, x, y, cw, ch, onClose }: { n: Node; geo: GeoPoint | null; x: number; y: number; cw: number; ch: number; onClose: () => void }) {
  const st = nodeStatus[n.status] ?? nodeStatus.pending
  const w = Math.min(272, cw - 16)
  const left = clamp(x - w / 2, 8, cw - w - 8)
  const below = y < 210
  const style = below ? { left, top: Math.min(y + 18, ch - 8), width: w } : { left, top: y - 16, width: w, transform: 'translateY(-100%)' }
  const addr = n.address || n.publicIpv4 || n.publicIpv6
  const src = geo ? { coords: t('经纬度'), city: t('按城市推算'), country: t('按国家推算') }[geo.source] : ''
  return (
    <div className="nm-pop" style={style} data-nopan role="dialog" aria-label={n.name}>
      <div className="nm-pop-h">
        <span className="nm-pop-name">{flag(n.flag || geo?.cc) && <span className="nm-flag">{flag(n.flag || geo?.cc)}</span>}{n.name}</span>
        <button type="button" className="icon-btn" aria-label={t('关闭')} onClick={onClose}>×</button>
      </div>
      <div className="row" style={{ gap: 8, margin: '4px 0 8px', flexWrap: 'wrap' }}>
        <span className={'st ' + st.cls} style={{ padding: '1px 8px', borderRadius: 999, fontSize: 12 }}>{st.label}</span>
        {n.region && <span className="mute" style={{ fontSize: 12.5 }}>{n.region}</span>}
      </div>
      <dl className="nm-kv">
        <dt>{t('地址')}</dt><dd className="mono">{addr || '—'}</dd>
        {n.connected
          ? <><dt>CPU / {t('内存')}</dt><dd className="mono">{n.system.cpu}% / {n.system.mem}%</dd>
            <dt>{t('实时速率')}</dt><dd className="mono">↓ {bps(n.system.rxBps)} ↑ {bps(n.system.txBps)}</dd></>
          : <><dt>{t('最后心跳')}</dt><dd>{n.lastSeen ? ago(n.lastSeen) : '—'}</dd></>}
        <dt>{t('入站')}</dt><dd className="mono">{n.inboundCount}</dd>
        {src && <><dt>{t('位置')}</dt><dd>{src}</dd></>}
      </dl>
      <Link to={'/nodes?id=' + n.id} className="nm-pop-link">{t('在节点页查看')} ›</Link>
    </div>
  )
}

export default function NodeMap() {
  const { nodes } = useNodes()
  // The map box only exists once there are nodes, so it is measured through a callback ref.
  const boxRef = useRef<HTMLDivElement | null>(null)
  const [box, setBox] = useState<HTMLDivElement | null>(null)
  const [cw, setCw] = useState(0)
  const boxCb = useCallback((el: HTMLDivElement | null) => { boxRef.current = el; setBox(el) }, [])
  useEffect(() => {
    if (!box) return
    const ro = new ResizeObserver(([e]) => setCw(Math.round(e.contentRect.width)))
    ro.observe(box)
    return () => ro.disconnect()
  }, [box])
  const phone = cw > 0 && cw < 640
  const ch = phone ? 300 : Math.round((cw * MAP_H) / MAP_W)
  const base = Math.max(cw / MAP_W, ch / MAP_H) // px per viewBox unit at zoom 1 (cover)
  const [view, setView] = useState<View>({ k: 1, tx: 0, ty: 0 })
  const [sel, setSel] = useState<string | null>(null)
  const [hot, setHot] = useState<string | null>(null)

  const geoOf = useMemo(() => {
    const m = new Map<string, GeoPoint | null>()
    for (const n of nodes) m.set(n.id, resolveNodeGeo(n))
    return m
  }, [nodes])
  const placed = useMemo<Placed[]>(() => nodes.flatMap((n) => {
    const geo = geoOf.get(n.id)
    if (!geo) return []
    const [px, py] = project(geo.lat, geo.lng)
    return [{ node: n, geo, px, py }]
  }), [nodes, geoOf])
  const unplaced = nodes.filter((n) => !geoOf.get(n.id))

  const fit = useCallback((v: View): View => {
    const k = clamp(v.k, 1, MAX_ZOOM)
    return { k, tx: clamp(v.tx, cw - base * k * MAP_W, 0), ty: clamp(v.ty, ch - base * k * MAP_H, 0) }
  }, [cw, ch, base])

  // Initial / reset view: the whole map, centred on the nodes when the
  // viewport is narrower than the map (phones).
  const home = useCallback((): View => {
    const mean = placed.length ? placed.reduce((s, p) => s + p.px, 0) / placed.length : MAP_W / 2
    return fit({ k: 1, tx: cw / 2 - base * mean, ty: 0 })
  }, [placed, cw, base, fit])
  const started = useRef(false)
  useEffect(() => {
    if (!cw) return
    if (!started.current && placed.length) { started.current = true; setView(home()); return }
    setView((v) => fit(v))
  }, [cw, ch, placed.length > 0]) // eslint-disable-line react-hooks/exhaustive-deps

  const viewRef = useRef(view)
  viewRef.current = view
  const zoomAt = (factor: number, x: number, y: number) => {
    const v = viewRef.current
    const k = clamp(v.k * factor, 1, MAX_ZOOM)
    const r = k / v.k
    setView(fit({ k, tx: x - (x - v.tx) * r, ty: y - (y - v.ty) * r }))
  }
  const zoomRef = useRef(zoomAt)
  zoomRef.current = zoomAt

  // Wheel zoom needs a non-passive listener to keep the page from scrolling.
  useEffect(() => {
    const el = box
    if (!el) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const r = el.getBoundingClientRect()
      zoomRef.current(Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.0018)), e.clientX - r.left, e.clientY - r.top)
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [box])

  // Drag to pan, two fingers to pinch.
  const ptrs = useRef(new Map<number, { x: number; y: number }>())
  const gest = useRef({ view, cx: 0, cy: 0, dist: 0, moved: false })
  const centre = () => {
    const r = boxRef.current!.getBoundingClientRect()
    const p = [...ptrs.current.values()]
    return {
      cx: p.reduce((s, q) => s + q.x, 0) / p.length - r.left, cy: p.reduce((s, q) => s + q.y, 0) / p.length - r.top,
      dist: p.length > 1 ? Math.hypot(p[0].x - p[1].x, p[0].y - p[1].y) : 0,
    }
  }
  const regrip = (fresh: boolean) => { gest.current = { view: viewRef.current, ...centre(), moved: fresh ? false : gest.current.moved } }
  const onDown = (e: React.PointerEvent) => {
    if ((e.target as HTMLElement).closest('[data-nopan]') || (e.pointerType === 'mouse' && e.button !== 0)) return
    boxRef.current!.setPointerCapture(e.pointerId)
    ptrs.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    regrip(ptrs.current.size === 1)
  }
  const onMove = (e: React.PointerEvent) => {
    if (!ptrs.current.has(e.pointerId)) return
    ptrs.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    const g = gest.current
    const c = centre()
    if (ptrs.current.size === 1) {
      if (Math.hypot(c.cx - g.cx, c.cy - g.cy) > 4) g.moved = true
      if (g.moved) setView(fit({ ...g.view, tx: g.view.tx + c.cx - g.cx, ty: g.view.ty + c.cy - g.cy }))
    } else if (g.dist > 0 && c.dist > 0) {
      g.moved = true
      const k = clamp(g.view.k * (c.dist / g.dist), 1, MAX_ZOOM)
      const r = k / g.view.k
      setView(fit({ k, tx: c.cx - (g.cx - g.view.tx) * r, ty: c.cy - (g.cy - g.view.ty) * r }))
    }
  }
  const onUp = (e: React.PointerEvent) => {
    ptrs.current.delete(e.pointerId)
    if (ptrs.current.size > 0) regrip(false)
  }
  const onBackground = (e: React.MouseEvent) => {
    if (!(e.target as HTMLElement).closest('[data-nopan]') && !gest.current.moved) setSel(null)
  }
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') setSel(null) }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [])

  // Markers: screen position, fanned out where they would overlap.
  const markers = useMemo(() => {
    const s = base * view.k
    const pts = placed.map((p) => ({ id: p.node.id, x: p.px * s + view.tx, y: p.py * s + view.ty }))
    const byId = new Map(placed.map((p) => [p.node.id, p]))
    const out: { p: Placed; x: number; y: number; gx: number; gy: number; size: number; first: boolean }[] = []
    for (const g of clusterPoints(pts, MARKER_GAP)) {
      const gx = g.reduce((a, i) => a + i.x, 0) / g.length
      const gy = g.reduce((a, i) => a + i.y, 0) / g.length
      const items = [...g].sort((a, b) => a.id.localeCompare(b.id))
      const off = ringOffsets(items.length, MARKER_GAP * 0.85)
      items.forEach((it, i) => out.push({ p: byId.get(it.id)!, x: gx + off[i][0], y: gy + off[i][1], gx, gy, size: items.length, first: i === 0 }))
    }
    return out
  }, [placed, view, base])

  const select = (id: string) => {
    setSel(id)
    const m = markers.find((x) => x.p.node.id === id)
    if (m && (m.x < 40 || m.y < 40 || m.x > cw - 40 || m.y > ch - 40)) {
      const v = viewRef.current
      setView(fit({ ...v, tx: v.tx + cw / 2 - m.x, ty: v.ty + ch / 2 - m.y }))
    }
  }

  const counts = useMemo(() => {
    const ccs = new Set<string>()
    for (const n of nodes) { const cc = geoOf.get(n.id)?.cc || countryCode(n); if (cc) ccs.add(cc) }
    return {
      total: nodes.length,
      online: nodes.filter((n) => n.status === 'online').length,
      offline: nodes.filter((n) => n.status === 'offline').length,
      error: nodes.filter((n) => n.status === 'error').length,
      other: nodes.filter((n) => n.status === 'pending' || n.status === 'syncing').length,
      countries: ccs,
    }
  }, [nodes, geoOf])
  const hotLand = useMemo(() => [...counts.countries].map(countryNumeric).filter(Boolean), [counts.countries])
  const list = useMemo(() => [...nodes].sort((a, b) => toneRank[tone(a)] - toneRank[tone(b)] || a.name.localeCompare(b.name)), [nodes])
  const selMarker = markers.find((m) => m.p.node.id === sel)
  const hotMarker = markers.find((m) => m.p.node.id === hot)
  const abnormal = counts.offline + counts.error

  return (
    <>
      <div className="top">
        <div><h1>{t('节点地图')}</h1><div className="sub">{t('全部节点的位置与在线状态 · 点击标记查看详情')}</div></div>
        <div className="row"><Link className="btn" to="/nodes">{t('管理节点')}</Link></div>
      </div>
      <section className="grid g4" aria-label={t('概览')}>
        <div className="card kpi"><span className="kpi-ico"><Icon.Server /></span><div className="label">{t('节点总数')}</div><div className="val">{counts.total}</div><div className="foot">{t('已定位 {a} · 未定位 {b}', { a: placed.length, b: unplaced.length })}</div></div>
        <div className="card kpi"><span className="kpi-ico ok"><Icon.Server /></span><div className="label">{t('在线')}</div><div className="val">{counts.online}<span className="of"> / {counts.total}</span></div><div className="foot">{counts.other ? t('{n} 个待接入或同步中', { n: counts.other }) : t('全部已接入')}</div></div>
        <div className="card kpi"><span className="kpi-ico bad"><Icon.Server /></span><div className="label">{t('离线 / 异常')}</div><div className="val" style={abnormal ? { color: 'var(--bad)' } : undefined}>{abnormal}</div><div className="foot">{t('{a} 个离线 · {b} 个下发失败', { a: counts.offline, b: counts.error })}</div></div>
        <div className="card kpi"><span className="kpi-ico violet"><Icon.Globe /></span><div className="label">{t('覆盖国家 / 地区')}</div><div className="val">{counts.countries.size}</div><div className="foot">{[...counts.countries].slice(0, 8).map((c) => flag(c)).join(' ') || '—'}</div></div>
      </section>
      {nodes.length === 0 ? (
        <section className="card"><Empty>{t('还没有节点，')}<Link to="/nodes?new=1">{t('添加第一个节点')}</Link></Empty></section>
      ) : (
        <div className="nm-layout">
          <section className="card nm-card" aria-label={t('节点地图')}>
            <div ref={boxCb} className="nm" style={{ height: ch || undefined, touchAction: view.k > 1 ? 'none' : 'pan-y' }}
              onPointerDown={onDown} onPointerMove={onMove} onPointerUp={onUp} onPointerCancel={onUp} onClick={onBackground}>
              {cw > 0 && (
                <svg width={cw} height={ch} role="img" aria-label={t('世界地图')} className="nm-svg">
                  <g transform={`translate(${view.tx} ${view.ty}) scale(${base * view.k})`}><Land hot={hotLand} /></g>
                </svg>
              )}
              {cw > 0 && markers.map((m) => {
                const n = m.p.node
                const visible = m.x > -20 && m.y > -20 && m.x < cw + 20 && m.y < ch + 20
                return visible && (
                  <span key={n.id}>
                    {m.size > 1 && m.first &&
                      <i className="nm-ring" style={{ left: m.gx, top: m.gy, width: MARKER_GAP * 1.9 * (m.size > 7 ? 2.4 : 1), height: MARKER_GAP * 1.9 * (m.size > 7 ? 2.4 : 1) }} />}
                    <button type="button" data-nopan className={`nm-marker ${tone(n)}${sel === n.id ? ' sel' : ''}${hot === n.id ? ' hot' : ''}`}
                      style={{ left: m.x, top: m.y }} aria-label={`${n.name} · ${(nodeStatus[n.status] ?? nodeStatus.pending).label}`}
                      onClick={() => select(n.id)} onMouseEnter={() => setHot(n.id)} onMouseLeave={() => setHot(null)} onFocus={() => setHot(n.id)} onBlur={() => setHot(null)} />
                  </span>
                )
              })}
              {hotMarker && hot !== sel && (
                <div className="nm-tip" style={{ left: clamp(hotMarker.x, 70, cw - 70), top: hotMarker.y - 14 }}>
                  {flag(hotMarker.p.node.flag || hotMarker.p.geo.cc)} <b>{hotMarker.p.node.name}</b>
                  <span className={'nm-tip-st ' + tone(hotMarker.p.node)}>{(nodeStatus[hotMarker.p.node.status] ?? nodeStatus.pending).label}</span>
                </div>
              )}
              {selMarker && <Popover n={selMarker.p.node} geo={selMarker.p.geo} x={selMarker.x} y={selMarker.y} cw={cw} ch={ch} onClose={() => setSel(null)} />}
              <div className="nm-ctl" data-nopan>
                <button type="button" aria-label={t('放大')} disabled={view.k >= MAX_ZOOM} onClick={() => zoomAt(1.6, cw / 2, ch / 2)}>+</button>
                <button type="button" aria-label={t('缩小')} disabled={view.k <= 1} onClick={() => zoomAt(1 / 1.6, cw / 2, ch / 2)}>−</button>
                <button type="button" aria-label={t('重置视图')} title={t('重置视图')} disabled={view.k === 1 && view.tx === home().tx} onClick={() => setView(home())}>⟲</button>
              </div>
              <div className="nm-legend" data-nopan>
                <span><i className="ok" />{t('在线')}</span><span><i className="bad" />{t('离线 / 异常')}</span><span><i className="warn" />{t('待接入 / 同步中')}</span>
              </div>
            </div>
          </section>
          <section className="card nm-list-card" aria-label={t('节点列表')}>
            <div className="ctitle"><span>{t('节点列表')}</span><span className="mute" style={{ fontSize: 12.5 }}>{nodes.length}</span></div>
            <ul className="nm-list" style={phone ? undefined : { maxHeight: Math.max(ch - 20, 280) }}>
              {list.map((n) => {
                const g = geoOf.get(n.id)
                const st = nodeStatus[n.status] ?? nodeStatus.pending
                return (
                  <li key={n.id}>
                    <button type="button" className={'nm-row' + (sel === n.id ? ' sel' : '')} disabled={!g}
                      onMouseEnter={() => setHot(n.id)} onMouseLeave={() => setHot(null)} onFocus={() => setHot(n.id)} onBlur={() => setHot(null)} onClick={() => select(n.id)}>
                      <i className={'nm-dot ' + tone(n)} />
                      <span className="nm-row-main">
                        <span className="nm-row-name">{flag(n.flag || g?.cc)} {n.name}</span>
                        <span className="mute nm-row-sub">{n.region || n.address || '—'}</span>
                      </span>
                      <span className={'nm-row-st ' + tone(n)}>{st.label}</span>
                    </button>
                  </li>
                )
              })}
            </ul>
          </section>
        </div>
      )}
      {unplaced.length > 0 && (
        <section className="card" aria-label={t('未定位的节点')}>
          <div className="ctitle"><span>{t('未定位的节点（在节点设置里填写国家代码或经纬度）')}</span></div>
          <div className="nm-unplaced">
            {unplaced.map((n) => <Link key={n.id} to={'/nodes?id=' + n.id} className="chip"><i className={'nm-dot ' + tone(n)} />{n.name}{n.region ? <span className="mute"> · {n.region}</span> : null}</Link>)}
          </div>
        </section>
      )}
    </>
  )
}
