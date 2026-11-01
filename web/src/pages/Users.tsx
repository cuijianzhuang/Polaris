import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { get, post } from '../api'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { CopyBox, copyText, Empty, Modal, QR, useAction, useConfirm, useLoad, usePoll, useRefresh, RefreshPicker, useToast, SkeletonRows, useUndo, useToastApi, runBatch } from '../components/ui'
import { bytes, date, daysLeft, isOnline, userStatus } from '../format'
import type { Group, User } from '../types'
import { AdjustModal, BulkCreateModal, downloadJSON, ImportModal } from '../components/UserTools'
import { t, tr } from '../i18n'
import { UserPlanModal } from '../components/Plans'
import { BindNodesModal } from './users/BindNodesModal'
import { GroupsModal } from './users/GroupsModal'
import { UserDrawer } from './users/UserDrawer'
import { hasOnlineIPs, UserActivity, UserIPCount, UserOnlineDetails } from '../components/UserOnline'

type Filter = '' | 'active' | 'online' | 'expiring' | 'near' | 'limited' | 'expired' | 'disabled'
const filters: { k: Filter; label: string; sum: string }[] = [
  { k: '', label: t('全部'), sum: 'all' }, { k: 'active', label: t('正常'), sum: 'active' }, { k: 'online', label: t('在线 / 活跃'), sum: 'online' },
  { k: 'expiring', label: t('即将到期'), sum: 'expiring' }, { k: 'near', label: t('流量将满'), sum: 'near' }, { k: 'limited', label: t('已超额'), sum: 'limited' }, { k: 'expired', label: t('已过期'), sum: 'expired' },
  { k: 'disabled', label: t('已禁用'), sum: 'disabled' },
]
const PAGE = 100

// avatarColor gives each username a stable hue.
function avatarColor(name: string) {
  let h = 0
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) % 360
  return `linear-gradient(135deg, hsl(${h} 65% 52%), hsl(${(h + 40) % 360} 70% 45%))`
}

let subBase = location.origin
function subURL(u: User) { return `${subBase}/sub/${u.subToken}` }

function statusBadge(u: User) {
  const d = daysLeft(u.expireAt)
  if (u.status === 'active' && hasOnlineIPs(u)) return <span className="st ok">● {t('在线')}</span>
  if (u.status === 'active' && isOnline(u.onlineAt)) return <span className="st warn">{t('近期活跃')}</span>
  if (u.status === 'active' && d !== null && d <= 7) return <span className="st warn">{t('即将到期')}</span>
  if (u.bannedUntil && new Date(u.bannedUntil) > new Date()) return <span className="st bad">{t('IP 超限封禁')}</span>
  const s = userStatus[u.status] ?? { label: u.status, cls: 'off' }
  return <span className={'st ' + s.cls}>{s.label}</span>
}

export default function Users() {
  const [onlineUser, setOnlineUser] = useState<User | null>(null)
  const [params, setParams] = useSearchParams()
  const [q, setQ] = useState(() => params.get('q') ?? '')
  const [filter, setFilter] = useState<Filter>(() => (filters.some((f) => f.k === params.get('filter')) ? params.get('filter') as Filter : ''))
  const [query, setQuery] = useState('')
  useEffect(() => { const t = setTimeout(() => setQuery(q), 300); return () => clearTimeout(t) }, [q])
  useEffect(() => { const v = params.get('q'); if (v !== null) setQ(v) }, [params])
  const [group, setGroup] = useState('')
  const [sort, setSort] = useState<{ k: string; desc: boolean }>({ k: '', desc: true })
  const [page, setPage] = useState(0)
  useEffect(() => setPage(0), [query, filter, group, sort])
  const list = useLoad(() => get<{ items: User[]; total: number; summary?: Record<string, number> }>(
    `/users?limit=${PAGE}&offset=${page * PAGE}&q=${encodeURIComponent(query)}&filter=${filter}&group=${group}${sort.k ? `&sort=${sort.k}&order=${sort.desc ? 'desc' : 'asc'}` : ''}`),
  [query, filter, group, sort, page])
  const [refreshMs, setRefreshMs] = useRefresh(15000)
  usePoll(list.reload, refreshMs)
  const sum = list.data?.summary ?? {}
  const total = list.data?.total ?? 0
  const sortBy = (k: string) => setSort((s) => (s.k === k ? { k, desc: !s.desc } : { k, desc: k !== 'username' }))
  const th = (k: string, label: string) => (
    <th className="sortable" aria-sort={sort.k === k ? (sort.desc ? 'descending' : 'ascending') : 'none'} onClick={() => sortBy(k)}>
      {label}<span className="sort-ind">{sort.k === k ? (sort.desc ? '↓' : '↑') : '↕'}</span>
    </th>
  )
  const confirm = useConfirm()
  const groups = useLoad(() => get<Group[]>('/groups'))
  const settings = useLoad(() => get<{ public_url?: string; sub_base_url?: string }>('/settings'))
  const base = settings.data?.sub_base_url || settings.data?.public_url
  if (base) subBase = base.replace(/\/$/, '')
  const [drawer, setDrawer] = useState<User | 'new' | null>(params.get('new') ? 'new' : null)
  const [selected, setSelected] = useState<User | null>(null)
  const [qr, setQr] = useState<User | null>(null)
  const [modal, setModal] = useState<'groups' | 'plan' | 'bulk' | 'adjust' | 'import' | null>(null)
  const [checked, setChecked] = useState<string[]>([])
  const toast = useToast()
  const { busy, run } = useAction()
  const gname = (id: string) => { const n = groups.data?.find((g) => g.id === id)?.name; return n ? tr(n) : id }
  const { nodes } = useNodes()
  const nname = (id: string) => nodes.find((n) => n.id === id)?.name ?? t('已删除节点')
  const [bind, setBind] = useState<'attach' | 'detach' | null>(null)
  const [hidden, setHidden] = useState<string[]>([])
  const items = (list.data?.items ?? []).filter((u) => !hidden.includes(u.id))
  const undo = useUndo()
  const toastApi = useToastApi()
  const sel = selected ? items.find((u) => u.id === selected.id) ?? selected : items[0]
  useEffect(() => { if (params.get('new')) setParams({}, { replace: true }) }, [params, setParams])

  const copySub = async (u: User) => {
    try { await copyText(subURL(u)); toast(t('订阅链接已复制')) } catch { window.prompt(t('复制失败，请手动复制：'), subURL(u)) }
  }
  // Large selections go in chunks so the progress toast can show real progress.
  const bulk = (action: string, extra?: object) => run(async () => {
    const ids = checked
    if (ids.length <= 25) await post('/users/bulk', { action, ids, ...extra })
    else {
      const chunks: string[][] = []
      for (let i = 0; i < ids.length; i += 25) chunks.push(ids.slice(i, i + 25))
      await runBatch(toastApi, t('正在处理用户'), chunks, (c) => post('/users/bulk', { action, ids: c, ...extra }), 2)
    }
    setChecked([])
    list.reload()
  }, checked.length <= 25 ? t('已完成') : undefined)
  // Deleting hides the rows at once; the request goes out after 5 s unless undone.
  const removeUsers = (us: User[]) => {
    const ids = us.map((u) => u.id)
    setHidden((h) => [...h, ...ids])
    setChecked((c) => c.filter((x) => !ids.includes(x)))
    setDrawer(null)
    undo(us.length === 1 ? t('已删除用户 {name}', { name: us[0].username }) : t('已删除 {n} 个用户', { n: us.length }), async () => {
      for (let i = 0; i < ids.length; i += 100) await post('/users/bulk', { action: 'delete', ids: ids.slice(i, i + 100) })
      list.reload()
    }, () => setHidden((h) => h.filter((x) => !ids.includes(x))))
  }

  return (
    <>
      <div className="top">
        <div><h1>{t('用户')}</h1><div className="sub">{t('一个用户跨所有内核、所有协议通用 · 可整台绑定节点、按分组或单独授权入站')}</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => setModal('groups')}>{t('分组')}</button>
          <details className="menu">
            <summary className="btn">{t('更多')}</summary>
            <div className="menu-list" onClick={(e) => { (e.currentTarget.parentElement as HTMLDetailsElement).open = false }}>
              <button type="button" onClick={() => setModal('import')}>{t('导入用户（JSON / 3x-ui）')}</button>
              <button type="button" onClick={() => run(async () => downloadJSON(`polaris-users-${new Date().toISOString().slice(0, 10)}.json`, await get('/users/export')), t('已导出'))}>{t('导出全部用户')}</button>
              <button type="button" disabled={!sum.limited && !sum.expired} onClick={async () => {
                if (await confirm(t('删除全部 {n} 个已超额 / 已过期的用户？', { n: (sum.limited ?? 0) + (sum.expired ?? 0) }), { danger: true, ok: t('删除') }))
                  run(async () => { await post('/users/bulk', { action: 'delete-depleted' }); list.reload() }, t('已删除'))
              }}>{t('删除已超额 / 已过期用户')}</button>
              <button type="button" onClick={async () => {
                if (await confirm(t('把全部用户的已用流量清零？'), { danger: true, ok: t('全部重置') }))
                  run(async () => { await post('/users/bulk', { action: 'reset-traffic', all: true }); list.reload() }, t('已重置'))
              }}>{t('重置全部用户流量')}</button>
            </div>
          </details>
          <RefreshPicker ms={refreshMs} onChange={setRefreshMs} onNow={list.reload} />
          <button className="btn" type="button" onClick={() => setModal('bulk')}>{t('批量创建')}</button>
          <button className="btn pri" type="button" onClick={() => setDrawer('new')}>+ {t('新建用户')}</button>
        </div>
      </div>
      <div className="row">
        <label htmlFor="q" className="sr">{t('搜索用户')}</label>
        <input className="input search" id="q" placeholder={t('搜索用户名 / UUID / 备注')} value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 'auto' }} />
        <div className="chips">
          {filters.map((f) => <button key={f.k} className={'chip' + (filter === f.k ? ' on' : '')} type="button" onClick={() => setFilter(f.k)}>{f.label}{sum[f.sum] != null && <span className="chip-n">{sum[f.sum]}</span>}</button>)}
          {(groups.data?.length ?? 0) > 0 && (
            <select className="input" style={{ width: 'auto', minHeight: 34 }} value={group} onChange={(e) => setGroup(e.target.value)} aria-label={t('按分组筛选')}>
              <option value="">{t('全部分组')}</option>
              {groups.data!.map((g) => <option key={g.id} value={g.id}>{g.plan ? t('套餐 · {name}', { name: g.name }) : tr(g.name)}</option>)}
            </select>
          )}
        </div>
        <span className="spacer" />
        {checked.length > 0 && (
          <>
            <span className="small mute">{t('已选 {n}', { n: checked.length })}</span>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('enable')}>{t('启用')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('disable')}>{t('禁用')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('reset-traffic')}>{t('重置流量')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setModal('plan')}>{t('开通套餐')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setModal('adjust')}>{t('调整到期 / 流量 / 限制')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => downloadJSON('polaris-users.json', await get('/users/export?ids=' + checked.join(','))), t('已导出'))}>{t('导出')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('attach')}>{t('绑定节点')}</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('detach')}>{t('解绑节点')}</button>
            <button className="btn sm danger" type="button" disabled={busy} onClick={() => removeUsers(items.filter((u) => checked.includes(u.id)))}>{t('删除')}</button>
          </>
        )}
      </div>
      <div className="card flush scroll">
        {items.length === 0 ? (list.loading && !list.data ? <SkeletonRows rows={6} cols={5} /> : <Empty icon={<Icon.Users />}>{t('没有匹配的用户')}</Empty>) : (
          <table className="rtable">
            <thead><tr>
              <th style={{ width: 32 }}><input type="checkbox" aria-label={t('全选')} checked={checked.length === items.length} onChange={(e) => setChecked(e.target.checked ? items.map((u) => u.id) : [])} /></th>
              {th('username', t('用户'))}<th>{t('状态')}</th><th>{t('授权')}</th>{th('used', t('流量（已用 / 总量）'))}{th('expire', t('到期'))}{th('online', t('最近在线'))}<th>{t('在线 IP')}</th><th>{t('操作')}</th>
            </tr></thead>
            <tbody>
              {items.map((u) => {
                const used = u.usedUp + u.usedDown
                const pct = u.trafficLimit ? Math.min(100, (used / u.trafficLimit) * 100) : 0
                const d = daysLeft(u.expireAt)
                return (
                  <tr key={u.id} className="clickable" data-user-id={u.id} onClick={() => setSelected(u)}>
                    <td className="rt-check" onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={t('选择 {name}', { name: u.username })} checked={checked.includes(u.id)} onChange={(e) => setChecked((x) => e.target.checked ? [...x, u.id] : x.filter((y) => y !== u.id))} /></td>
                    <td className="rt-head"><div className="user-cell"><span className="avatar sm" style={{ background: avatarColor(u.username) }}>{(u.username[0] || '?').toUpperCase()}</span><div><div style={{ fontWeight: 550 }}>{u.username}</div><div className="mono small mute">{u.uuid.slice(0, 4)}…{u.uuid.slice(-4)}</div></div></div></td>
                    <td data-label={t('状态')}>{statusBadge(u)}</td>
                    <td data-label={t('授权')}>
                      {(u.nodeIds ?? []).map((n) => <span key={n} className="tag xray" title={t('整台节点')}>{nname(n)}</span>)}
                      {u.planId && <span className="tag info-tag" title={t('套餐')}>{gname(u.planId)}</span>}
                      {u.groupIds.map((g) => <span key={g} className="tag">{gname(g)}</span>)}
                      {(u.inboundIds ?? []).length > 0 && <span className="tag" title={t('单独授权的入站')}>{t('+{n} 入站', { n: u.inboundIds.length })}</span>}
                      {!u.planId && !u.groupIds.length && !(u.nodeIds ?? []).length && !(u.inboundIds ?? []).length && <span className="st warn">{t('未授权')}</span>}
                    </td>
                    <td data-label={t('流量')} className="rt-wrap" style={{ minWidth: 180 }}>
                      <span className="mono">{bytes(used)} / {u.trafficLimit ? bytes(u.trafficLimit) : t('不限')}</span>
                      {(u.speedUpMbps || u.speedDownMbps) ? <span className="tag info-tag" style={{ marginLeft: 6 }} title={t('单独限速 (Mbps)')}>↑{u.speedUpMbps || '∞'} ↓{u.speedDownMbps || '∞'}</span> : null}
                      <div className="bar" style={{ marginTop: 6 }}><div style={{ width: pct + '%', background: pct >= 100 ? 'var(--bad)' : 'var(--pri)' }} /></div>
                    </td>
                    <td data-label={t('到期')} className="mono" style={d !== null && d <= 7 && d >= 0 ? { color: 'var(--warn-ink)' } : undefined}>{u.expireAt ? (d !== null && d >= 0 && d <= 7 ? t('{n} 天后', { n: d }) : date(u.expireAt)) : t('永不')}</td>
                    <td data-label={t('最近在线')} className="small"><UserActivity user={u} /></td>
                    <td data-label={t('在线 IP')} className="mono" onClick={(e) => e.stopPropagation()}><UserIPCount user={u} onClick={() => setOnlineUser(u)} /></td>
                    <td className="rt-actions" onClick={(e) => e.stopPropagation()}>
                      {u.status === 'limited' ? <button className="btn sm" type="button" onClick={() => run(async () => { await post(`/users/${u.id}/reset-traffic`); list.reload() }, t('流量已重置'))}>{t('重置流量')}</button>
                        : u.status === 'disabled' ? <button className="btn sm" type="button" onClick={() => run(async () => { await post('/users/bulk', { action: 'enable', ids: [u.id] }); list.reload() }, t('已启用'))}>{t('启用')}</button>
                        : <>
                          <button className="icon-btn" type="button" aria-label={t('复制订阅链接')} onClick={() => copySub(u)}><Icon.Copy /></button>
                          <button className="icon-btn" type="button" aria-label={t('显示二维码')} onClick={() => setQr(u)}><Icon.QR /></button>
                        </>}
                      <button className="icon-btn" type="button" aria-label={t('编辑')} onClick={() => setDrawer(u)}><Icon.Edit /></button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
      {total > PAGE && (
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <span className="small mute">{t('第 {from}–{to} 个，共 {total} 个', { from: page * PAGE + 1, to: Math.min(total, (page + 1) * PAGE), total })}</span>
          <button className="btn sm" type="button" disabled={page === 0} onClick={() => setPage(page - 1)}>{t('上一页')}</button>
          <button className="btn sm" type="button" disabled={(page + 1) * PAGE >= total} onClick={() => setPage(page + 1)}>{t('下一页')}</button>
        </div>
      )}
      {sel && (
        <div className="card row" style={{ justifyContent: 'space-between', gap: 16 }}>
          <div style={{ minWidth: 0 }}>
            <div style={{ fontWeight: 600 }}>{t('订阅链接 · {name}', { name: sel.username })}</div>
            <div className="mono" style={{ fontSize: 13, color: 'var(--ink-2)', marginTop: 4, overflowWrap: 'anywhere' }}>{subURL(sel)}</div>
            <div className="sub">{t('按客户端自动输出：通用链接 · Clash/mihomo · sing-box · Surge；浏览器打开显示订阅信息页')}</div>
          </div>
          <div className="row">
            <button className="btn" type="button" onClick={async () => {
              if (!(await confirm(t('重置后旧订阅链接和已导入的节点立即失效（同时更换 UUID / 密码），用户需要重新导入订阅。确定重置？'), { danger: true, ok: t('重置') }))) return
              run(async () => { await post(`/users/${sel.id}/reset-token`); list.reload() }, t('已重置：旧链接和旧节点立即失效'))
            }}>{t('重置 token')}</button>
            <button className="btn" type="button" onClick={() => setQr(sel)}>{t('二维码')}</button>
            <button className="btn" type="button" onClick={() => copySub(sel)}>{t('复制')}</button>
          </div>
        </div>
      )}
      {drawer && groups.data && <UserDrawer user={drawer === 'new' ? undefined : drawer} groups={groups.data} onClose={() => setDrawer(null)} onSaved={() => { setDrawer(null); list.reload() }} onDelete={(u) => removeUsers([u])} />}
      {onlineUser && <Modal title={t('在线 IP · {name}', { name: onlineUser.username })} onClose={() => setOnlineUser(null)}><UserOnlineDetails key={onlineUser.id} user={onlineUser} /></Modal>}
      {qr && (
        <Modal title={t('订阅 · {name}', { name: qr.username })} onClose={() => setQr(null)}>
          <QR text={subURL(qr)} size={220} />
          <CopyBox text={subURL(qr)} />
        </Modal>
      )}
      {modal === 'plan' && <UserPlanModal ids={checked} onClose={() => setModal(null)} onDone={() => { setModal(null); setChecked([]); list.reload() }} />}
      {modal === 'groups' && <GroupsModal onClose={() => setModal(null)} onChanged={() => { groups.reload(); list.reload() }} />}
      {bind && <BindNodesModal count={checked.length} detach={bind === 'detach'} onClose={() => setBind(null)} onSubmit={(ids) => {
        bulk(bind === 'detach' ? 'detach-nodes' : 'attach-nodes', { nodeIds: ids }); setBind(null)
      }} />}
      {modal === 'bulk' && groups.data && <BulkCreateModal groups={groups.data.filter((g) => !g.plan)} onClose={() => setModal(null)} onDone={() => { setModal(null); list.reload() }} />}
      {modal === 'adjust' && <AdjustModal ids={checked} onClose={() => setModal(null)} onDone={() => { setModal(null); setChecked([]); list.reload() }} />}
      {modal === 'import' && <ImportModal onClose={() => setModal(null)} onDone={() => { setModal(null); list.reload() }} />}
    </>
  )
}
