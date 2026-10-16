import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { get } from '../api'
import { useNodes } from '../App'
import { useLoad, usePoll, useWidth } from '../components/ui'
import TagTraffic from '../components/TagTraffic'
import { Icon } from '../components/Icons'
import { bps, bytes, coreLabel, coreShort, coreTagClass, nodeStatus, ago } from '../format'
import type { Node, NodeTraffic } from '../types'

interface Overview {
  nodes: { online: number; total: number; timeout: number; error?: number }
  users: { online: number; active: number; total: number }
  today: { Up: number; Down: number }
  month: { Up: number; Down: number }
  rate: { rxBps: number; txBps: number; peakNode: string }
  trend: { date: string; up: number; down: number }[]
  topUsers: { userId: string; username: string; up: number; down: number }[]
  attention: { warnDays: number; expiring: number; limited: number; expired: number; near: number }
  cores: Record<string, number>
}

const weekday = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

function Trend({ data }: { data: Overview['trend'] }) {
  const [ref, W] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const H = 220, L = 56, R = Math.max(L + 10, W - 12), T = 14, B = 190
  const max = Math.max(1, ...data.map((d) => Math.max(d.up, d.down)))
  const x = (i: number) => L + (i * (R - L)) / Math.max(1, data.length - 1)
  const y = (v: number) => B - (v / max) * (B - T)
  const pts = (k: 'up' | 'down') => data.map((d, i) => `${x(i).toFixed(1)} ${y(d[k]).toFixed(1)}`)
  const line = (k: 'up' | 'down') => 'M' + pts(k).join(' L')
  const area = (k: 'up' | 'down') => `${line(k)} L${x(data.length - 1).toFixed(1)} ${B} L${L} ${B} Z`
  const every = Math.max(1, Math.ceil(data.length / Math.max(2, Math.floor((R - L) / 56))))
  const label = (d: string) => data.length <= 7 ? weekday[new Date(d + 'T00:00:00').getDay()] : d.slice(5)
  const onMove = (e: React.PointerEvent<SVGRectElement>) => {
    const box = e.currentTarget.getBoundingClientRect()
    const i = Math.round(((e.clientX - box.left) / box.width) * (data.length - 1))
    setHover(Math.min(data.length - 1, Math.max(0, i)))
  }
  const h = hover !== null ? data[hover] : null
  return (
    <div ref={ref} className="trend-wrap">
      {W > 0 && (
        <svg className="trend" width={W} height={H} role="img" aria-label={`近 ${data.length} 天流量趋势`}>
          <defs>
            <linearGradient id="tr-down" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stopColor="var(--pri)" stopOpacity=".22" /><stop offset="1" stopColor="var(--pri)" stopOpacity="0" /></linearGradient>
          </defs>
          <g style={{ stroke: 'var(--line-2)' }}>{[0, 0.5, 1].map((f) => <line key={f} x1={L} x2={R} y1={B - f * (B - T)} y2={B - f * (B - T)} style={f === 0 ? { stroke: 'var(--input)' } : undefined} />)}</g>
          <g style={{ fill: 'var(--mute)' }} fontSize="11" fontFamily="IBM Plex Mono">
            {[0, 0.5, 1].map((f) => <text key={f} x={L - 8} y={B - f * (B - T) + 4} textAnchor="end">{f ? bytes(max * f) : '0'}</text>)}
            {data.map((d, i) => (data.length - 1 - i) % every === 0 &&
              <text key={d.date} x={x(i)} y={210} textAnchor="middle">{i === data.length - 1 ? '今天' : label(d.date)}</text>)}
          </g>
          <path d={area('down')} fill="url(#tr-down)" />
          <path d={line('down')} fill="none" style={{ stroke: 'var(--pri)' }} strokeWidth="2.5" strokeLinejoin="round" />
          <path d={line('up')} fill="none" style={{ stroke: 'var(--warn)' }} strokeWidth="2" strokeDasharray="5 4" strokeLinejoin="round" />
          {h && hover !== null && <g>
            <line x1={x(hover)} x2={x(hover)} y1={T} y2={B} style={{ stroke: 'var(--input)' }} strokeDasharray="3 3" />
            <circle cx={x(hover)} cy={y(h.down)} r="4" style={{ fill: 'var(--card)', stroke: 'var(--pri)' }} strokeWidth="2" />
            <circle cx={x(hover)} cy={y(h.up)} r="3.5" style={{ fill: 'var(--card)', stroke: 'var(--warn)' }} strokeWidth="2" />
          </g>}
          <rect x={L} y={T} width={R - L} height={B - T} fill="transparent" onPointerMove={onMove} onPointerLeave={() => setHover(null)} />
        </svg>
      )}
      {h && hover !== null && (
        <div className="tip" style={{ left: Math.min(Math.max(x(hover), 70), W - 70), top: 4 }}>
          <div className="tip-t">{h.date} {weekday[new Date(h.date + 'T00:00:00').getDay()]}</div>
          <div><i style={{ background: 'var(--pri)' }} />下行 <b>{bytes(h.down)}</b></div>
          <div><i style={{ background: 'var(--warn)' }} />上行 <b>{bytes(h.up)}</b></div>
        </div>
      )}
    </div>
  )
}

function TopUsers({ list }: { list: Overview['topUsers'] }) {
  const max = Math.max(1, ...list.map((u) => u.up + u.down))
  if (!list.length) return <div className="empty" style={{ padding: '32px 0' }}>今天还没有用户流量</div>
  return (
    <ol className="toplist">
      {list.map((u, i) => (
        <li key={u.userId}>
          <span className={'rank r' + (i + 1)}>{i + 1}</span>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div className="row" style={{ justifyContent: 'space-between', gap: 8 }}>
              <Link to={'/users?q=' + encodeURIComponent(u.username || u.userId)} className="ellipsis">{u.username || u.userId}</Link>
              <span className="mono">{bytes(u.up + u.down)}</span>
            </div>
            <div className="bar" style={{ marginTop: 6 }}><div style={{ width: `${((u.up + u.down) / max) * 100}%` }} /></div>
          </div>
        </li>
      ))}
    </ol>
  )
}

function Attention({ d }: { d: Overview }) {
  const a = d.attention
  const items: { n: number; text: string; to: string; cls: string }[] = [
    { n: d.nodes.timeout, text: '个节点离线', to: '/nodes', cls: 'bad' },
    { n: d.nodes.error ?? 0, text: '个节点下发失败', to: '/nodes', cls: 'bad' },
    { n: a.limited, text: '个用户已超额', to: '/users?filter=limited', cls: 'bad' },
    { n: a.expired, text: '个用户已过期', to: '/users?filter=expired', cls: 'warn' },
    { n: a.expiring, text: `个用户 ${a.warnDays} 天内到期`, to: '/users?filter=expiring', cls: 'warn' },
    { n: a.near, text: '个用户流量用了 80% 以上', to: '/users?filter=near', cls: 'warn' },
  ].filter((x) => x.n > 0)
  return (
    <div className="attn" aria-label="需要关注">
      {items.length === 0
        ? <span className="attn-item ok">✓ 一切正常：节点在线，没有超额或到期的用户</span>
        : items.map((x) => <Link key={x.text} to={x.to} className={'attn-item ' + x.cls}><b>{x.n}</b>{x.text} ›</Link>)}
    </div>
  )
}

export function NodeRow({ n, t, onClick }: { n: Node; t?: NodeTraffic; onClick?: () => void }) {
  const st = nodeStatus[n.status] ?? nodeStatus.pending
  const syncing = n.appliedRev < n.desiredRev
  return (
    <tr className={onClick ? 'clickable' : ''} onClick={onClick}>
      <td className="rt-head" style={{ fontWeight: 500 }}>{n.name}</td>
      <td data-label="地区">{n.region || '—'}</td>
      <td data-label="内核">{n.cores.length ? n.cores.map((c) => <span key={c.id} className={coreTagClass(c.type)}>{coreShort[c.type]}</span>) : '—'}</td>
      <td data-label="CPU / 内存" className="mono">{n.connected ? `${n.system.cpu}% / ${n.system.mem}%` : '—'}</td>
      <td data-label="实时速率" className="mono">{n.connected ? `↓ ${bps(n.system.rxBps)} ↑ ${bps(n.system.txBps)}` : '—'}</td>
      <td data-label="今日流量" className="mono">{t ? `↑ ${bytes(t.todayUp)} ↓ ${bytes(t.todayDown)}` : '—'}</td>
      <td data-label="配置版本" className="mono" style={syncing ? { color: 'var(--warn)' } : undefined}>
        {syncing ? `rev ${n.appliedRev} → ${n.desiredRev}` : `rev ${n.appliedRev}${n.appliedRev ? ' ✓' : ''}`}
      </td>
      <td data-label="状态"><span><span className="dot" style={{ background: st.color }} />{st.label}{n.status === 'offline' && n.lastSeen ? ` · ${ago(n.lastSeen)}` : ''}</span></td>
    </tr>
  )
}

export default function Dashboard() {
  const { nodes } = useNodes()
  const nav = useNavigate()
  const [days, setDays] = useState(7)
  const overview = useLoad(() => get<Overview>('/stats/overview?days=' + days), [nodes.length, days])
  const data = overview.data
  const traffic = useLoad(() => get<Record<string, NodeTraffic>>('/stats/nodes').catch(() => ({} as Record<string, NodeTraffic>)), [nodes.length])
  usePoll(() => { overview.reload(); traffic.reload() }, 10000)
  const live = nodes.filter((n) => n.connected)
  const rx = live.reduce((s, n) => s + n.system.rxBps, 0), tx = live.reduce((s, n) => s + n.system.txBps, 0)
  const cores = Object.entries(data?.cores ?? {}).filter(([, n]) => n > 0)
  return (
    <>
      <div className="top">
        <div><h1>仪表盘</h1><div className="sub">全部节点 · 实时数据随心跳刷新</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => nav('/nodes?new=1')}>添加节点</button>
          <button className="btn pri" type="button" onClick={() => nav('/users?new=1')}>新建用户</button>
        </div>
      </div>
      {data && <Attention d={data} />}
      <section className="grid g4" aria-label="概览">
        <Link to="/nodes" className="card kpi"><span className="kpi-ico ok"><Icon.Server /></span><div className="label">在线节点</div><div className="val">{data?.nodes.online ?? '—'}<span className="of"> / {data?.nodes.total ?? '—'}</span></div><div className="foot">{data?.nodes.timeout ? `${data.nodes.timeout} 个离线` : '全部在线'}{data?.nodes.error ? ` · ${data.nodes.error} 个下发失败` : ''}</div></Link>
        <Link to="/users?filter=online" className="card kpi"><span className="kpi-ico"><Icon.Users /></span><div className="label">在线用户</div><div className="val">{data?.users.online ?? '—'}</div><div className="foot">有效 {data?.users.active ?? 0} · 共 {data?.users.total ?? 0} 个用户</div></Link>
        <div className="card kpi"><span className="kpi-ico violet"><Icon.Download /></span><div className="label">今日流量</div><div className="val">{bytes((data?.today.Up ?? 0) + (data?.today.Down ?? 0))}</div><div className="foot">↑ {bytes(data?.today.Up)} · ↓ {bytes(data?.today.Down)} · 本月 {bytes((data?.month.Up ?? 0) + (data?.month.Down ?? 0))}</div></div>
        <div className="card kpi"><span className="kpi-ico warn"><Icon.Cpu /></span><div className="label">实时速率</div><div className="val">{bps(rx + tx)}</div><div className="foot">↓ {bps(rx)} · ↑ {bps(tx)}{data?.rate.peakNode ? ` · 峰值 ${data.rate.peakNode}` : ''}</div></div>
      </section>
      <section className="grid g3">
        <div className="card" style={{ gridColumn: 'span 2', minWidth: 0 }}>
          <div className="ctitle">
            <span>流量趋势</span>
            <span className="row" style={{ gap: 12 }}>
              <span className="sub legend" style={{ margin: 0 }}><i style={{ background: 'var(--pri)' }} />下行<i className="dash" style={{ borderColor: 'var(--warn)' }} />上行</span>
              <span className="seg-mini">{[7, 30].map((d) => <button key={d} type="button" className={days === d ? 'on' : ''} onClick={() => setDays(d)}>{d} 天</button>)}</span>
            </span>
          </div>
          {data && <Trend data={data.trend} />}
        </div>
        <div className="card">
          <div className="ctitle"><span>今日用户排行</span><Link to="/users" style={{ fontSize: 13, fontWeight: 500 }}>全部用户</Link></div>
          {data && <TopUsers list={data.topUsers} />}
        </div>
      </section>
      <section className="card">
        <div className="ctitle">
          <span>节点状态</span>
          <span className="row" style={{ gap: 6 }}>
            {cores.map(([c, n]) => <span key={c} className={coreTagClass(c)} title={coreLabel[c]}>{coreShort[c as keyof typeof coreShort] ?? c} × {n}</span>)}
            <Link to="/nodes" style={{ fontSize: 13, fontWeight: 500, marginLeft: 6 }}>全部节点</Link>
          </span>
        </div>
        <div className="scroll" style={{ marginTop: 8 }}>
          {nodes.length === 0 ? <div className="empty">还没有节点，<Link to="/nodes?new=1">添加第一个节点</Link></div> : (
            <table className="rtable">
              <thead><tr><th>节点</th><th>地区</th><th>内核</th><th>CPU / 内存</th><th>实时速率</th><th>今日流量</th><th>配置版本</th><th>状态</th></tr></thead>
              <tbody>{nodes.map((n) => <NodeRow key={n.id} n={n} t={traffic.data?.[n.id]} onClick={() => nav('/nodes?id=' + n.id)} />)}</tbody>
            </table>
          )}
        </div>
      </section>
      <TagTraffic />
    </>
  )
}
