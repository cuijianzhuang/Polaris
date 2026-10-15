import { Link, useNavigate } from 'react-router-dom'
import { get } from '../api'
import { useNodes } from '../App'
import { useLoad } from '../components/ui'
import { bps, bytes, coreColor, coreLabel, coreShort, coreTagClass, nodeStatus, ago } from '../format'
import type { Node } from '../types'

interface Overview {
  nodes: { online: number; total: number; timeout: number }
  users: { online: number; active: number; total: number }
  today: { Up: number; Down: number }
  rate: { rxBps: number; txBps: number; peakNode: string }
  trend: { date: string; up: number; down: number }[]
  cores: Record<string, number>
}

const weekday = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

function Trend({ data }: { data: Overview['trend'] }) {
  const W = 700, H = 220, L = 52, R = 690, T = 20, B = 190
  const max = Math.max(1, ...data.map((d) => Math.max(d.up, d.down)))
  const x = (i: number) => L + (i * (R - L)) / Math.max(1, data.length - 1)
  const y = (v: number) => B - (v / max) * (B - T)
  const line = (k: 'up' | 'down') => data.map((d, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(d[k]).toFixed(1)}`).join(' ')
  const last = data[data.length - 1]
  return (
    <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 220, marginTop: 12 }} role="img" aria-label="近 7 天流量趋势">
      <g stroke="#EEF0EC">{[T, (T + B) / 2, B].map((v) => <line key={v} x1={L} y1={v} x2={R} y2={v} stroke={v === B ? '#D5D9D3' : undefined} />)}</g>
      <g fill="#5B6470" fontSize="11" fontFamily="IBM Plex Mono">
        <text x={L - 8} y={T + 4} textAnchor="end">{bytes(max)}</text>
        <text x={L - 8} y={B + 4} textAnchor="end">0</text>
        {data.map((d, i) => <text key={d.date} x={x(i)} y={210} textAnchor="middle">{weekday[new Date(d.date + 'T00:00:00').getDay()]}</text>)}
      </g>
      <path d={line('down')} fill="none" stroke="#0E7490" strokeWidth="2.5" />
      <path d={line('up')} fill="none" stroke="#B45309" strokeWidth="2" strokeDasharray="5 4" />
      {last && <g fontSize="12">
        <text x={R - 4} y={y(last.down) - 8} fill="#0E7490" textAnchor="end">下行</text>
        <text x={R - 4} y={y(last.up) + 16} fill="#B45309" textAnchor="end">上行</text>
      </g>}
    </svg>
  )
}

export function NodeRow({ n, onClick }: { n: Node; onClick?: () => void }) {
  const st = nodeStatus[n.status] ?? nodeStatus.pending
  const syncing = n.appliedRev < n.desiredRev
  return (
    <tr className={onClick ? 'clickable' : ''} onClick={onClick}>
      <td style={{ fontWeight: 500 }}>{n.name}</td>
      <td>{n.region || '—'}</td>
      <td>{n.cores.length ? n.cores.map((c) => <span key={c.id} className={coreTagClass(c.type)}>{coreShort[c.type]}</span>) : '—'}</td>
      <td className="mono">{n.connected ? `${n.system.cpu}% / ${n.system.mem}%` : '—'}</td>
      <td className="mono">{n.connected ? `↓ ${bps(n.system.rxBps)} ↑ ${bps(n.system.txBps)}` : '—'}</td>
      <td className="mono" style={syncing ? { color: 'var(--warn)' } : undefined}>
        {syncing ? `rev ${n.appliedRev} → ${n.desiredRev}` : `rev ${n.appliedRev}${n.appliedRev ? ' ✓' : ''}`}
      </td>
      <td><span className="dot" style={{ background: st.color }} />{st.label}{n.status === 'offline' && n.lastSeen ? ` · ${ago(n.lastSeen)}` : ''}</td>
    </tr>
  )
}

export default function Dashboard() {
  const { nodes } = useNodes()
  const nav = useNavigate()
  const { data } = useLoad(() => get<Overview>('/stats/overview'), [nodes.length])
  const allCores = ['xray', 'singbox', 'mihomo', 'hysteria']
  const totalCores = Math.max(1, nodes.length)
  return (
    <>
      <div className="top">
        <div><h1>仪表盘</h1><div className="sub">全部节点 · 实时数据随心跳刷新</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => nav('/nodes?new=1')}>添加节点</button>
          <button className="btn pri" type="button" onClick={() => nav('/users?new=1')}>新建用户</button>
        </div>
      </div>
      <section className="grid g4" aria-label="概览">
        <div className="card kpi"><div className="label">在线节点</div><div className="val">{data?.nodes.online ?? '—'} / {data?.nodes.total ?? '—'}</div><div className="foot">{data?.nodes.timeout ?? 0} 个节点心跳超时</div></div>
        <div className="card kpi"><div className="label">在线用户</div><div className="val">{data?.users.online ?? '—'}</div><div className="foot">共 {data?.users.active ?? 0} 个有效用户</div></div>
        <div className="card kpi"><div className="label">今日流量</div><div className="val">{bytes((data?.today.Up ?? 0) + (data?.today.Down ?? 0))}</div><div className="foot">上行 {bytes(data?.today.Up)} · 下行 {bytes(data?.today.Down)}</div></div>
        <div className="card kpi"><div className="label">实时速率</div><div className="val">{bps((data?.rate.rxBps ?? 0) + (data?.rate.txBps ?? 0))}</div><div className="foot">峰值节点：{data?.rate.peakNode || '—'}</div></div>
      </section>
      <section className="grid g3">
        <div className="card" style={{ gridColumn: 'span 2' }}>
          <div className="ctitle"><span>流量趋势 · 近 7 天</span><span className="sub" style={{ margin: 0 }}>上行 / 下行</span></div>
          {data && <Trend data={data.trend} />}
        </div>
        <div className="card">
          <div className="ctitle"><span>内核分布</span></div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14, marginTop: 16 }}>
            {allCores.map((c) => {
              const n = data?.cores[c] ?? 0
              return (
                <div key={c}>
                  <div className="row" style={{ justifyContent: 'space-between' }}>
                    <span className={coreTagClass(c)}>{coreLabel[c]}</span>
                    <span className="mono">{n ? `${n} 实例` : '未安装'}</span>
                  </div>
                  <div className="bar" style={{ marginTop: 8 }}><div style={{ width: `${(n / totalCores) * 100}%`, background: coreColor[c] }} /></div>
                </div>
              )
            })}
          </div>
        </div>
      </section>
      <section className="card">
        <div className="ctitle"><span>节点状态</span><Link to="/nodes" style={{ fontSize: 13, fontWeight: 500 }}>全部节点</Link></div>
        <div className="scroll" style={{ marginTop: 8 }}>
          {nodes.length === 0 ? <div className="empty">还没有节点，<Link to="/nodes?new=1">添加第一个节点</Link></div> : (
            <table>
              <thead><tr><th>节点</th><th>地区</th><th>内核</th><th>CPU / 内存</th><th>实时速率</th><th>配置版本</th><th>状态</th></tr></thead>
              <tbody>{nodes.map((n) => <NodeRow key={n.id} n={n} onClick={() => nav('/nodes?id=' + n.id)} />)}</tbody>
            </table>
          )}
        </div>
      </section>
    </>
  )
}
