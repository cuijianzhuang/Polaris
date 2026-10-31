import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { bps, coreLabel, coreShort, coreTagClass, netUsed, nodeStatus } from '../format'
import { t } from '../i18n'
import type { Node, NodeTraffic } from '../types'
import NodeCard, { NetUsage } from './NodeCard'
import { Icon } from './Icons'
import { Empty } from './ui'

const VIEW_KEY = 'polaris.nodes.view'
type View = 'cards' | 'list'
const needsAttention = (n: Node) => n.status !== 'online' || !!n.lastError || !!n.progress?.error || n.appliedRev < n.desiredRev || n.cores.some((c) => !!c.error)

function Resource({ value, label, connected }: { value: number; label: string; connected: boolean }) {
  const v = Math.max(0, Math.min(100, value || 0))
  return <div className="node-resource"><span>{label}</span><span className="node-resource-track" aria-hidden="true"><i style={{ width: connected ? v + '%' : 0, background: v >= 85 ? 'var(--bad)' : v >= 60 ? 'var(--warn)' : 'var(--pri)' }} /></span><span className="mono">{connected ? `${Math.round(v)}%` : '—'}</span></div>
}

export default function NodeBrowser({ nodes, selected, traffic, open, onOpen, onSelect, badge }: {
  nodes: Node[]; selected?: Node; traffic?: Record<string, NodeTraffic>; open: boolean
  onOpen: (open: boolean) => void; onSelect: (node: Node) => void; badge: (node: Node) => ReactNode
}) {
  const [query, setQuery] = useState(''), [status, setStatus] = useState(''), [region, setRegion] = useState(''), [core, setCore] = useState('')
  const [sort, setSort] = useState('default'), [page, setPage] = useState(1), [size, setSize] = useState(12)
  const [preference, setPreference] = useState<View | null>(() => {
    try { const v = localStorage.getItem(VIEW_KEY); return v === 'cards' || v === 'list' ? v : null } catch { return null }
  })
  const view = preference ?? (nodes.length > 6 ? 'list' : 'cards')
  const chooseView = (v: View) => { setPreference(v); try { localStorage.setItem(VIEW_KEY, v) } catch { /* storage unavailable */ } }
  const regions = useMemo(() => [...new Set(nodes.map((n) => n.region).filter(Boolean))].sort((a, b) => a.localeCompare(b)), [nodes])
  const cores = useMemo(() => [...new Set(nodes.flatMap((n) => n.cores.map((c) => c.type)))].sort(), [nodes])
  const online = nodes.filter((n) => n.status === 'online').length
  const attention = nodes.filter(needsAttention).length
  const filtered = useMemo(() => {
    const words = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean)
    const rows = nodes.filter((n) => (!status || (status === 'attention' ? needsAttention(n) : n.status === status)) && (!region || n.region === region) && (!core || n.cores.some((c) => c.type === core)) && words.every((word) => [n.name, n.id, n.region, n.address, n.domain, n.publicIpv4, n.publicIpv6, ...(n.labels ?? [])].filter(Boolean).join(' ').toLocaleLowerCase().includes(word)))
    const byName = (a: Node, b: Node) => a.name.localeCompare(b.name, undefined, { numeric: true }) || a.id.localeCompare(b.id)
    // Live values only reorder rows when the operator explicitly chooses it.
    if (sort === 'name') rows.sort(byName)
    else if (sort === 'attention') rows.sort((a, b) => Number(needsAttention(b)) - Number(needsAttention(a)) || byName(a, b))
    else if (sort === 'cpu' || sort === 'mem') rows.sort((a, b) => Number(b.connected) - Number(a.connected) || (b.system[sort] || 0) - (a.system[sort] || 0) || byName(a, b))
    else if (sort === 'traffic') rows.sort((a, b) => netUsed(b) - netUsed(a) || byName(a, b))
    return rows
  }, [nodes, query, status, region, core, sort])
  useEffect(() => setPage(1), [query, status, region, core, sort, size])
  const pages = Math.max(1, Math.ceil(filtered.length / size)), current = Math.min(page, pages)
  useEffect(() => setPage((p) => Math.min(p, pages)), [pages])
  const shown = filtered.slice((current - 1) * size, current * size)
  const active = !!(query || status || region || core)
  const clear = () => { setQuery(''); setStatus(''); setRegion(''); setCore('') }
  const selectedVisible = !selected || filtered.some((n) => n.id === selected.id)

  return <section className={'node-browser' + (open ? '' : ' collapsed')} aria-label={t('节点浏览')} id="node-browser">
    <div className="node-browser-heading">
      <div className="node-totals" aria-label={t('节点概况')}>
        <span>{t('节点')} <b>{nodes.length}</b></span><span><i className="dot" style={{ background: 'var(--ok)' }} />{t('在线')} <b>{online}</b></span>
        {attention > 0 && <button className={'link node-attention' + (status === 'attention' ? ' active' : '')} type="button" onClick={() => { setStatus(status === 'attention' ? '' : 'attention'); onOpen(true) }}>{t('需关注')} <b>{attention}</b></button>}
      </div>
      <button className="btn sm" type="button" aria-expanded={open} aria-controls="node-browser-content" onClick={() => onOpen(!open)}>{open ? t('收起节点列表') : t('切换节点')}</button>
    </div>
    <div id="node-browser-content" hidden={!open}>
      <div className="node-filters">
        <label className="node-search"><Icon.Search /><input className="input" type="search" aria-label={t('搜索节点')} placeholder={t('名称、地址、地区或标签')} value={query} onChange={(e) => setQuery(e.target.value)} /></label>
        <select className="input" aria-label={t('节点状态')} value={status} onChange={(e) => setStatus(e.target.value)}>
          <option value="">{t('全部状态')}</option><option value="attention">{t('需关注')}</option>
          {Object.entries(nodeStatus).map(([key, s]) => <option key={key} value={key}>{s.label}</option>)}
        </select>
        <select className="input" aria-label={t('节点地区')} value={region} onChange={(e) => setRegion(e.target.value)}><option value="">{t('全部地区')}</option>{regions.map((r) => <option key={r}>{r}</option>)}</select>
        <select className="input" aria-label={t('节点内核')} value={core} onChange={(e) => setCore(e.target.value)}><option value="">{t('全部内核')}</option>{cores.map((c) => <option key={c} value={c}>{coreLabel[c] ?? c}</option>)}</select>
        <select className="input" aria-label={t('节点排序')} value={sort} onChange={(e) => setSort(e.target.value)}>
          <option value="default">{t('默认顺序')}</option><option value="name">{t('名称排序')}</option><option value="attention">{t('需关注优先')}</option><option value="cpu">{t('CPU 从高到低')}</option><option value="mem">{t('内存从高到低')}</option><option value="traffic">{t('服务器流量从高到低')}</option>
        </select>
        <div className="seg-mini node-view" role="group" aria-label={t('节点展示方式')}><button type="button" className={view === 'cards' ? 'on' : ''} aria-pressed={view === 'cards'} onClick={() => chooseView('cards')}>{t('卡片')}</button><button type="button" className={view === 'list' ? 'on' : ''} aria-pressed={view === 'list'} onClick={() => chooseView('list')}>{t('列表')}</button></div>
      </div>
      <div className="node-browser-meta"><span role="status">{t('显示 {from}–{to} / {total} 个节点', { from: filtered.length ? (current - 1) * size + 1 : 0, to: Math.min(current * size, filtered.length), total: filtered.length })}</span>{active && <button className="link" type="button" onClick={clear}>{t('清除筛选')}</button>}{!selectedVisible && <span>{t('当前节点不在筛选结果中')}</span>}</div>
      {filtered.length === 0 ? <div className="card"><Empty>{t('没有匹配的节点')}<button className="btn sm" type="button" onClick={clear}>{t('清除筛选')}</button></Empty></div> : view === 'cards' ? <div className="nodes" role="group" aria-label={t('选择节点')}>
        {shown.map((n) => <NodeCard key={n.id} n={n} on={selected?.id === n.id} onClick={() => onSelect(n)} traffic={traffic?.[n.id]} badge={badge(n)} />)}
      </div> : <div className="card flush node-list"><div className="scroll">
        <table className="rtable" aria-label={t('节点列表')}>
          <thead><tr><th scope="col">{t('节点')}</th><th scope="col">{t('状态')}</th><th scope="col">{t('资源')}</th><th scope="col">{t('实时速率')}</th><th scope="col">{t('服务器流量')}</th><th scope="col">{t('内核')}</th></tr></thead>
          <tbody>{shown.map((n) => {
            const st = nodeStatus[n.status] ?? nodeStatus.pending
            const problem = n.progress?.error || n.lastError || n.cores.find((c) => c.error)?.error
            return <tr key={n.id} className={selected?.id === n.id ? 'sel' : ''} data-node-id={n.id}>
              <td className="rt-head"><button className="node-list-select" type="button" title={[n.name, n.domain || n.address || n.publicIpv4 || n.publicIpv6].filter(Boolean).join('\n')} aria-pressed={selected?.id === n.id} aria-label={t('查看节点 {name}', { name: n.name })} onClick={() => onSelect(n)}>
                <span className="node-list-name"><i className="dot" style={{ background: st.color }} />{n.flag} {n.name}{badge(n)}</span><span className="node-list-address">{[n.region, n.domain || n.address || n.publicIpv4 || n.publicIpv6].filter(Boolean).join(' · ') || t('无公网地址')}</span>
              </button></td>
              <td data-label={t('状态')}><div className="node-list-state"><span className={'st ' + st.cls}>{st.label}</span>{n.progress?.active ? <span className="small mute">{t('处理中…')}</span> : problem ? <span className="small node-problem" title={problem}>{t('有错误')}</span> : n.appliedRev < n.desiredRev ? <span className="small mute">{t('等待同步')}</span> : null}</div></td>
              <td data-label={t('资源')}><div className="node-resources"><Resource value={n.system.cpu} label="CPU" connected={n.connected} /><Resource value={n.system.mem} label={t('内存')} connected={n.connected} /></div></td>
              <td data-label={t('实时速率')}><div className="node-list-rates mono">{n.connected ? <><span>↓ {bps(n.system.rxBps)}</span><span>↑ {bps(n.system.txBps)}</span></> : '—'}</div></td>
              <td data-label={t('服务器流量')}>{n.netLimit || netUsed(n) ? <NetUsage n={n} /> : <span className="mute">—</span>}</td>
              <td data-label={t('内核')}><div className="node-list-cores" title={n.cores.map((c) => coreLabel[c.type]).join(' · ')}>{n.cores.slice(0, 3).map((c) => <span key={c.id} className={coreTagClass(c.type)}>{coreShort[c.type]}</span>)}{n.cores.length > 3 && <span className="tag">+{n.cores.length - 3}</span>}{n.cores.length === 0 && <span className="mute">—</span>}</div></td>
            </tr>
          })}</tbody>
        </table>
      </div></div>}
      <nav className="node-pagination" aria-label={t('节点分页')}>
        <label>{t('每页节点数')}<select className="input" value={size} onChange={(e) => setSize(Number(e.target.value))}>{[12, 24, 48].map((n) => <option key={n}>{n}</option>)}</select></label>
        <span className="mute">{t('第 {page} / {pages} 页', { page: current, pages })}</span>
        <button className="btn sm" type="button" disabled={current === 1} onClick={() => setPage(current - 1)}>{t('上一页')}</button><button className="btn sm" type="button" disabled={current >= pages} onClick={() => setPage(current + 1)}>{t('下一页')}</button>
      </nav>
    </div>
  </section>
}
