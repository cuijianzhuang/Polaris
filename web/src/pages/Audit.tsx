import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { Empty, Switch, useAction, useLoad } from '../components/ui'
import { datetime } from '../format'
import type { AuditLog } from '../types'
import { t } from '../i18n'

const prefixes = [
  { k: '', label: t('全部') }, { k: 'node', label: t('节点') }, { k: 'inbound', label: t('入站') }, { k: 'user', label: t('用户') },
  { k: 'template', label: t('模板') }, { k: 'chain', label: t('链路') }, { k: 'admin', label: t('登录') }, { k: 'spec', label: t('下发') },
  { k: 'change', label: t('变更记录') }, { k: 'security', label: t('安全') },
]
const changeLabel: Record<string, string> = { 'change.create': t('新建'), 'change.update': t('修改'), 'change.delete': t('删除') }

// ChangeDetail renders a change record's fields as old → new.
function ChangeDetail({ detail, action }: { detail: string; action: string }) {
  let d: Record<string, [unknown, unknown]>
  try { d = JSON.parse(detail) } catch { return <>{detail}</> }
  const show = (v: unknown) => v === null || v === undefined || v === '' ? <span className="mute">{t('空')}</span> : <span className="mono">{typeof v === 'object' ? JSON.stringify(v) : String(v)}</span>
  return (
    <div style={{ display: 'grid', gap: 2 }}>
      {Object.entries(d).map(([k, [o, n]]) => (
        <div key={k}><b style={{ fontWeight: 500 }}>{k}</b>：{action === 'change.create' ? show(n) : action === 'change.delete' ? show(o) : <>{show(o)} → {show(n)}</>}</div>
      ))}
    </div>
  )
}

export default function Audit() {
  const [action, setAction] = useState('')
  const [q, setQ] = useState('')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  useEffect(() => { const tm = setTimeout(() => { setQuery(q); setPage(0) }, 300); return () => clearTimeout(tm) }, [q])
  const list = useLoad(() => get<{ items: AuditLog[]; total: number }>(`/audit-logs?limit=100&offset=${page * 100}&action=${action}&q=${encodeURIComponent(query)}`), [action, query, page])
  const total = list.data?.total ?? 0
  const settings = useLoad(() => get<Record<string, string>>('/settings'))
  const { busy, run } = useAction()
  const recording = settings.data?.audit_changes === 'true'
  return (
    <>
      <div className="top">
        <div><h1>{t('审计日志')}</h1><div className="sub">{t('全部写操作与登录记录 · 保留 180 天 · 仅超级管理员可见')}</div></div>
        <label className="row small" title={t('开启后，管理员每次操作创建、修改、删除了哪些记录、改了哪些字段（改前 → 改后）都会记下来；密码、密钥等只记「已隐藏」')}>
          {t('变更记录')}<Switch label={t('变更记录')} on={recording} onChange={(v) => !busy && run(async () => { await put('/settings', { audit_changes: String(v) }); settings.reload() }, v ? t('已开启变更记录') : t('已关闭变更记录'))} />
        </label>
      </div>
      <div className="row">
        <input className="input search" style={{ width: 'auto' }} placeholder={t('搜索对象 / 详情 / 管理员')} value={q} onChange={(e) => setQ(e.target.value)} aria-label={t('搜索')} />
        {prefixes.map((p) => <button key={p.k} type="button" className={'chip' + (action === p.k ? ' on' : '')} onClick={() => { setAction(p.k); setPage(0) }}>{p.label}</button>)}
      </div>
      <div className="card flush scroll">
        {list.data?.items.length === 0 ? <Empty>{t('没有记录')}</Empty> : (
          <table>
            <thead><tr><th>{t('时间')}</th><th>{t('操作者')}</th><th>{t('操作')}</th><th>{t('对象')}</th><th>{t('详情')}</th><th>IP</th></tr></thead>
            <tbody>
              {(list.data?.items ?? []).map((l) => (
                <tr key={l.id}>
                  <td className="mono">{datetime(l.createdAt)}</td>
                  <td>{l.admin}</td>
                  <td><span className={'tag' + (l.action.includes('fail') || l.action.includes('ip_limit') || l.action.includes('ban') || l.action === 'change.delete' ? ' bad' : l.action.startsWith('change.') ? ' ok' : '')}>{changeLabel[l.action] ?? l.action}</span></td>
                  <td style={{ whiteSpace: 'normal', maxWidth: 260, wordBreak: 'break-all' }}>{l.target}</td>
                  <td style={{ whiteSpace: 'normal', maxWidth: 460, wordBreak: 'break-all' }} className="small">{l.action.startsWith('change.') ? <ChangeDetail detail={l.detail} action={l.action} /> : l.detail}</td>
                  <td className="mono">{l.ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {total > 100 && (
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <span className="small mute">{t('共 {total} 条 · 第 {page} / {pages} 页', { total, page: page + 1, pages: Math.ceil(total / 100) })}</span>
          <button className="btn sm" type="button" disabled={page === 0} onClick={() => setPage(page - 1)}>{t('上一页')}</button>
          <button className="btn sm" type="button" disabled={(page + 1) * 100 >= total} onClick={() => setPage(page + 1)}>{t('下一页')}</button>
        </div>
      )}
    </>
  )
}
