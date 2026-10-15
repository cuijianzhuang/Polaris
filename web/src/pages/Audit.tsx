import { useEffect, useState } from 'react'
import { get } from '../api'
import { Empty, useLoad } from '../components/ui'
import { datetime } from '../format'
import type { AuditLog } from '../types'

const prefixes = [
  { k: '', label: '全部' }, { k: 'node', label: '节点' }, { k: 'inbound', label: '入站' }, { k: 'user', label: '用户' },
  { k: 'template', label: '模板' }, { k: 'chain', label: '链路' }, { k: 'admin', label: '登录' }, { k: 'spec', label: '下发' },
]

export default function Audit() {
  const [action, setAction] = useState('')
  const [q, setQ] = useState('')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  useEffect(() => { const t = setTimeout(() => { setQuery(q); setPage(0) }, 300); return () => clearTimeout(t) }, [q])
  const list = useLoad(() => get<{ items: AuditLog[]; total: number }>(`/audit-logs?limit=100&offset=${page * 100}&action=${action}&q=${encodeURIComponent(query)}`), [action, query, page])
  const total = list.data?.total ?? 0
  return (
    <>
      <div className="top"><div><h1>审计日志</h1><div className="sub">全部写操作与登录记录 · 保留 180 天</div></div></div>
      <div className="row">
        <input className="input search" style={{ width: 'auto' }} placeholder="搜索对象 / 详情 / 管理员" value={q} onChange={(e) => setQ(e.target.value)} aria-label="搜索" />
        {prefixes.map((p) => <button key={p.k} type="button" className={'chip' + (action === p.k ? ' on' : '')} onClick={() => { setAction(p.k); setPage(0) }}>{p.label}</button>)}
      </div>
      <div className="card flush scroll">
        {list.data?.items.length === 0 ? <Empty>没有记录</Empty> : (
          <table>
            <thead><tr><th>时间</th><th>操作者</th><th>操作</th><th>对象</th><th>详情</th><th>IP</th></tr></thead>
            <tbody>
              {(list.data?.items ?? []).map((l) => (
                <tr key={l.id}>
                  <td className="mono">{datetime(l.createdAt)}</td>
                  <td>{l.admin}</td>
                  <td><span className={'tag' + (l.action.includes('fail') || l.action.includes('ip_limit') ? ' bad' : '')}>{l.action}</span></td>
                  <td style={{ whiteSpace: 'normal', maxWidth: 260 }}>{l.target}</td>
                  <td style={{ whiteSpace: 'normal', maxWidth: 420 }} className="small">{l.detail}</td>
                  <td className="mono">{l.ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {total > 100 && (
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <span className="small mute">共 {total} 条 · 第 {page + 1} / {Math.ceil(total / 100)} 页</span>
          <button className="btn sm" type="button" disabled={page === 0} onClick={() => setPage(page - 1)}>上一页</button>
          <button className="btn sm" type="button" disabled={(page + 1) * 100 >= total} onClick={() => setPage(page + 1)}>下一页</button>
        </div>
      )}
    </>
  )
}
