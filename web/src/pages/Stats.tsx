import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { download, get, put } from '../api'
import { StackedBars } from '../components/Charts'
import { Field, useAction, useLoad } from '../components/ui'
import { bytes } from '../format'
import { t } from '../i18n'

type Usage = { up: number; down: number }
type Ranked = Usage & { id: string; name: string }
interface Report {
  from: string; to: string; total: Usage; activeUsers: number
  prev: { from: string; to: string; total: Usage; activeUsers: number }
  filter: { node: string; nodeName: string; user: string; userName: string }
  trend: (Usage & { date: string })[]
  nodes: Ranked[]; users: Ranked[]
  inbounds: (Usage & { nodeId: string; nodeName: string; tag: string })[]
}

const pad = (n: number) => String(n).padStart(2, '0')
const ymd = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
const presets = [
  { k: 'today', label: t('今天') },
  { k: '7d', label: t('7 天') },
  { k: '30d', label: t('30 天') },
  { k: 'month', label: t('本月') },
  { k: 'last', label: t('上月') },
  { k: '90d', label: t('90 天') },
  { k: 'year', label: t('今年') },
  { k: 'custom', label: t('自定义') },
] as const
type Preset = typeof presets[number]['k']

function presetRange(k: Preset): [string, string] {
  const now = new Date()
  const back = (n: number) => { const d = new Date(now); d.setDate(d.getDate() - n); return ymd(d) }
  switch (k) {
    case 'today': return [ymd(now), ymd(now)]
    case '30d': return [back(29), ymd(now)]
    case '90d': return [back(89), ymd(now)]
    case 'month': return [ymd(new Date(now.getFullYear(), now.getMonth(), 1)), ymd(now)]
    case 'last': return [ymd(new Date(now.getFullYear(), now.getMonth() - 1, 1)), ymd(new Date(now.getFullYear(), now.getMonth(), 0))]
    case 'year': return [ymd(new Date(now.getFullYear(), 0, 1)), ymd(now)]
    default: return [back(6), ymd(now)]
  }
}

// Delta shows the change against the previous period of the same length.
function Delta({ now, prev, label }: { now: number; prev: number; label: string }) {
  if (!prev) return <span className="mute">{now ? t('上期无数据') : '—'}</span>
  const pct = ((now - prev) / prev) * 100
  const up = pct >= 0
  return <span title={label} style={{ color: Math.abs(pct) < 0.05 ? 'var(--mute)' : up ? 'var(--ok)' : 'var(--bad)', fontWeight: 600 }}>{up ? '↑' : '↓'} {Math.abs(pct).toFixed(1)}%<span className="mute" style={{ fontWeight: 400 }}> {t('较上期')}</span></span>
}

type Row = { key: string; name: string; sub?: string; v: number; pick?: () => void }

function Share({ rows, total }: { rows: Row[]; total: number }) {
  if (!rows.length) return <div className="sub" style={{ marginTop: 10 }}>{t('这段时间没有流量')}</div>
  const max = Math.max(1, ...rows.map((r) => r.v))
  return (
    <div style={{ display: 'grid', gap: 10, marginTop: 12 }}>
      {rows.map((r, i) => (
        <div key={r.key}>
          <div className={'row' + (r.pick ? ' clickable' : '')} style={{ flexWrap: 'nowrap', gap: 8, cursor: r.pick ? 'pointer' : undefined }}
            onClick={r.pick} title={r.pick ? t('点击按此筛选') : undefined}>
            <span className="mono small mute" style={{ width: 22 }}>{i + 1}</span>
            <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontWeight: 500 }}>
              {r.name}{r.sub && <span className="small mute"> · {r.sub}</span>}
            </span>
            <span className="mono small">{bytes(r.v)}</span>
            <span className="mono small mute" style={{ width: 52, textAlign: 'right' }}>{total ? ((r.v / total) * 100).toFixed(1) : '0.0'}%</span>
          </div>
          <div className="bar" style={{ marginTop: 5, marginLeft: 30 }}><div style={{ width: `${(r.v / max) * 100}%` }} /></div>
        </div>
      ))}
    </div>
  )
}

function SpikeSettings() {
  const s = useLoad(() => get<Record<string, string>>('/settings'))
  const [f, setF] = useState({ factor: '', min: '' })
  const { busy, run } = useAction()
  useEffect(() => { if (s.data) setF({ factor: s.data.traffic_spike_factor ?? '5', min: s.data.traffic_spike_min_gb ?? '5' }) }, [s.data])
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <h2>{t('流量异常提醒')}</h2>
      <div className="sub" style={{ margin: 0 }}>{t('每小时检查一次：用户近 24 小时的流量超过过去 7 天日均的若干倍（且不少于下限）时，通过 Telegram 通知管理员，每个用户每天最多一次。常用于发现共享或泄露的账号。')}</div>
      <div className="form-grid">
        <Field label={t('倍数')} hint={t('0 为关闭')}><input className="input mono" inputMode="decimal" value={f.factor} onChange={(e) => setF({ ...f, factor: e.target.value.replace(/[^0-9.]/g, '') })} /></Field>
        <Field label={t('最少流量（GB）')} hint={t('低于这个量不提醒')}><input className="input mono" inputMode="decimal" value={f.min} onChange={(e) => setF({ ...f, min: e.target.value.replace(/[^0-9.]/g, '') })} /></Field>
      </div>
      <div><button className="btn" type="button" disabled={busy} onClick={() => run(() => put('/settings', { traffic_spike_factor: f.factor || '0', traffic_spike_min_gb: f.min || '0' }), t('已保存'))}>{t('保存')}</button></div>
    </section>
  )
}

export default function Stats() {
  // Range, filters and tab live in the URL: shareable, survive reloads.
  const [sp, setSp] = useSearchParams()
  const preset = (presets.some((p) => p.k === sp.get('range')) ? sp.get('range') : (sp.get('from') ? 'custom' : '7d')) as Preset
  const [from, to] = preset === 'custom' ? [sp.get('from') ?? presetRange('30d')[0], sp.get('to') ?? presetRange('30d')[1]] : presetRange(preset)
  const node = sp.get('node') ?? '', user = sp.get('user') ?? ''
  const tab = (sp.get('tab') ?? 'users') as 'users' | 'nodes' | 'inbounds'
  const patch = (kv: Record<string, string | null>) => {
    const n = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(kv)) { if (v) n.set(k, v); else n.delete(k) }
    setSp(n, { replace: true })
  }
  const setRange = (f: string, t: string) => patch({ range: 'custom', from: f, to: t })
  const filterQ = (node ? `&node=${node}` : '') + (user ? `&user=${user}` : '')
  const rep = useLoad(() => get<Report>(`/stats/report?from=${from}&to=${to}&limit=50${filterQ}`), [from, to, filterQ])
  const { busy, run } = useAction()
  const d = rep.data
  const total = d ? d.total.up + d.total.down : 0
  const prevTotal = d ? d.prev.total.up + d.prev.total.down : 0
  const days = d?.trend.length || 1
  const pts = useMemo(() => (d?.trend ?? []).map((p) => {
    const [, m, dd] = p.date.split('-')
    return { label: `${+m}/${+dd}`, tip: p.date, up: p.up, down: p.down }
  }), [d])
  const peak = d?.trend.reduce((a, p) => (p.up + p.down > a.v ? { v: p.up + p.down, date: p.date } : a), { v: 0, date: '' })
  const ranking: Row[] = !d ? [] : tab === 'users' ? d.users.map((u) => ({ key: u.id, name: u.name, v: u.up + u.down, pick: user ? undefined : () => patch({ user: u.id }) }))
    : tab === 'nodes' ? d.nodes.map((n) => ({ key: n.id, name: n.name, v: n.up + n.down, pick: node ? undefined : () => patch({ node: n.id }) }))
      : d.inbounds.map((i) => ({ key: i.nodeId + i.tag, name: i.tag, sub: i.nodeName, v: i.up + i.down, pick: node ? undefined : () => patch({ node: i.nodeId }) }))
  const inTotal = d ? d.inbounds.reduce((a, i) => a + i.up + i.down, 0) : 0
  const prevLabel = d ? t('上期 {from} ~ {to}', { from: d.prev.from, to: d.prev.to }) : ''

  return (
    <>
      <div className="top">
        <div><h1>{t('统计')}</h1><div className="sub">{t('任意时间段的流量趋势、节点占比与排行；在图上拖选可放大到那几天，点排行可按用户 / 节点筛选；按天的数据长期保留')}</div></div>
        <button className="btn" type="button" disabled={busy} onClick={() => run(() => download(`/stats/export?from=${from}&to=${to}${filterQ}`, `traffic-${from}_${to}.csv`))}>{t('导出 CSV')}</button>
      </div>
      <section className="card" style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
        <span className="seg-mini" style={{ flexWrap: 'wrap' }}>
          {presets.map((p) => <button key={p.k} type="button" className={preset === p.k ? 'on' : ''}
            onClick={() => p.k === 'custom' ? setRange(from, to) : patch({ range: p.k, from: null, to: null })}>{p.label}</button>)}
        </span>
        {preset === 'custom' ? (
          <span className="row" style={{ gap: 6 }}>
            <input type="date" className="input" style={{ width: 'auto' }} value={from} max={to} onChange={(e) => e.target.value && setRange(e.target.value, to)} />
            <span className="mute">{t('至')}</span>
            <input type="date" className="input" style={{ width: 'auto' }} value={to} min={from} onChange={(e) => e.target.value && setRange(from, e.target.value)} />
          </span>
        ) : <span className="mono small mute">{from} ~ {to}</span>}
        {(node || user) && <span className="row" style={{ gap: 6, marginLeft: 'auto' }}>
          <span className="small mute">{t('筛选')}</span>
          {node && <button type="button" className="chip on" onClick={() => patch({ node: null })} title={t('清除')}>{t('节点：{name}', { name: d?.filter.nodeName || node })} ✕</button>}
          {user && <button type="button" className="chip on" onClick={() => patch({ user: null })} title={t('清除')}>{t('用户：{name}', { name: d?.filter.userName || user })} ✕</button>}
        </span>}
      </section>
      {rep.error && <div className="note bad">{rep.error}</div>}
      <section className="grid g4" aria-label={t('合计')}>
        <div className="card kpi"><div className="label">{t('总流量')}</div><div className="val">{d ? bytes(total) : '—'}</div><div className="foot">{d && <Delta now={total} prev={prevTotal} label={prevLabel} />}</div></div>
        <div className="card kpi"><div className="label">{t('日均')}</div><div className="val">{d ? bytes(total / days) : '—'}</div><div className="foot">{peak?.date ? t('峰值 {v} · {date}', { v: bytes(peak.v), date: peak.date.slice(5) }) : t('{n} 天', { n: days })}</div></div>
        <div className="card kpi"><div className="label">{t('下行 / 上行')}</div><div className="val" style={{ fontSize: 'clamp(17px, 4.6vw, 22px)', whiteSpace: 'nowrap' }}>{d ? `${bytes(d.total.down)} / ${bytes(d.total.up)}` : '—'}</div><div className="foot">{t('下行占 {pct}% · 按节点倍率计', { pct: total ? ((d!.total.down / total) * 100).toFixed(1) : 0 })}</div></div>
        <div className="card kpi"><div className="label">{t('有流量的用户')}</div><div className="val">{d ? d.activeUsers : '—'}</div><div className="foot">{d && <Delta now={d.activeUsers} prev={d.prev.activeUsers} label={prevLabel} />}</div></div>
      </section>
      <section className="card">
        <div className="ctitle"><span>{t('每日流量')}</span>
          <span className="legend small mute"><i style={{ background: 'var(--pri)' }} />{t('下行')} <i style={{ background: 'var(--warn)', marginLeft: 10 }} />{t('上行')}</span>
        </div>
        {pts.length > 0 && <StackedBars pts={pts} fmt={bytes} height={200} onSelect={(a, b) => setRange(d!.trend[a].date, d!.trend[b].date)} />}
      </section>
      <section className="grid g2">
        <section className="card">
          <div className="ctitle"><span>{t('节点占比')}</span></div>
          <Share rows={(d?.nodes ?? []).map((n) => ({ key: n.id, name: n.name, v: n.up + n.down, pick: node ? undefined : () => patch({ node: n.id }) }))} total={total} />
        </section>
        <section className="card">
          <div className="ctitle"><span>{t('排行')}</span>
            <span className="seg-mini">
              {([['users', t('用户')], ['nodes', t('节点')], ['inbounds', t('入站')]] as const).map(([k, l]) => <button key={k} type="button" className={tab === k ? 'on' : ''} onClick={() => patch({ tab: k === 'users' ? null : k })}>{l}</button>)}
            </span>
          </div>
          {tab === 'inbounds' && <div className="hint" style={{ marginTop: 6 }}>{t('入站为实际字节数（不按倍率、不随用户筛选），从本版本开始记录')}</div>}
          <Share rows={ranking} total={tab === 'inbounds' ? inTotal : total} />
        </section>
      </section>
      <SpikeSettings />
    </>
  )
}
