import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { CopyBox, Drawer, Empty, Field, Modal, QR, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { bytes, coreShort, date, daysLeft, GB, isOnline, userStatus } from '../format'
import type { Group, Inbound, Node, User } from '../types'

type Filter = '' | 'online' | 'expiring' | 'limited' | 'disabled'
const filters: { k: Filter; label: string }[] = [
  { k: '', label: '全部' }, { k: 'online', label: '在线' }, { k: 'expiring', label: '即将到期' }, { k: 'limited', label: '已超额' }, { k: 'disabled', label: '已禁用' },
]

let subBase = location.origin
function subURL(u: User) { return `${subBase}/sub/${u.subToken}` }

function statusBadge(u: User) {
  const d = daysLeft(u.expireAt)
  if (u.status === 'active' && isOnline(u.onlineAt)) return <span className="st ok">● 在线</span>
  if (u.status === 'active' && d !== null && d <= 7) return <span className="st warn">即将到期</span>
  if (u.bannedUntil && new Date(u.bannedUntil) > new Date()) return <span className="st bad">IP 超限封禁</span>
  const s = userStatus[u.status] ?? { label: u.status, cls: 'off' }
  return <span className={'st ' + s.cls}>{s.label}</span>
}

const toggle = (list: string[], id: string) => (list.includes(id) ? list.filter((x) => x !== id) : [...list, id])

/** Authorisation: whole nodes, groups and single inbounds; shows what that adds up to. */
function Access({ groups, nodeIds, groupIds, inboundIds, onChange }: {
  groups: Group[]; nodeIds: string[]; groupIds: string[]; inboundIds: string[]
  onChange: (k: 'nodeIds' | 'groupIds' | 'inboundIds', v: string[]) => void
}) {
  const { nodes } = useNodes()
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const ins = (inbounds.data ?? []).filter((i) => i.enabled)
  const viaGroup = new Set(groups.filter((g) => groupIds.includes(g.id)).flatMap((g) => g.inboundIds))
  const effective = ins.filter((i) => nodeIds.includes(i.nodeId) || viaGroup.has(i.id) || inboundIds.includes(i.id))
  const effNodes = new Set(effective.map((i) => i.nodeId))
  const [showIns, setShowIns] = useState(inboundIds.length > 0)
  return (
    <section className="access">
      <div className="access-head">
        <b>授权</b>
        <span className="small mute">可用 <b className="mono">{effective.length}</b> 个入站 · <b className="mono">{effNodes.size}</b> 个节点</span>
      </div>
      <Field label="节点" hint="绑定整台节点：包含其全部入站，以后新增的入站也自动生效">
        <div className="row">
          {nodes.map((n) => (
            <button key={n.id} type="button" className={'chip' + (nodeIds.includes(n.id) ? ' on' : '')} aria-pressed={nodeIds.includes(n.id)}
              onClick={() => onChange('nodeIds', toggle(nodeIds, n.id))}>
              <span className="dot" style={{ background: n.status === 'online' ? 'var(--ok)' : '#C9CED4' }} />{n.name}
            </button>
          ))}
          {nodes.length === 0 && <span className="hint">还没有节点</span>}
        </div>
      </Field>
      <Field label="分组" hint="按分组授权：分组里的入站对组内所有用户生效">
        <div className="row">
          {groups.map((g) => (
            <button key={g.id} type="button" className={'chip' + (groupIds.includes(g.id) ? ' on' : '')} aria-pressed={groupIds.includes(g.id)}
              onClick={() => onChange('groupIds', toggle(groupIds, g.id))}>{g.name}<span className="mute small" style={{ marginLeft: 4 }}>{g.inboundIds.length}</span></button>
          ))}
          {groups.length === 0 && <span className="hint">还没有分组</span>}
        </div>
      </Field>
      <div>
        <button type="button" className="link" onClick={() => setShowIns(!showIns)}>{showIns ? '收起' : '单独授权入站'}{inboundIds.length > 0 && `（${inboundIds.length}）`}</button>
        {showIns && (
          <div className="access-ins">
            {nodes.filter((n) => ins.some((i) => i.nodeId === n.id)).map((n) => (
              <div key={n.id} className="access-node">
                <div className="small" style={{ fontWeight: 600 }}>{n.name}{nodeIds.includes(n.id) && <span className="tag info-tag" style={{ marginLeft: 6 }}>已整台绑定</span>}</div>
                <div className="row">
                  {ins.filter((i) => i.nodeId === n.id).map((i) => {
                    const covered = nodeIds.includes(n.id) || viaGroup.has(i.id)
                    const on = inboundIds.includes(i.id)
                    return (
                      <button key={i.id} type="button" disabled={covered && !on} title={covered ? '已通过节点或分组授权' : ''}
                        className={'chip sm' + (on || covered ? ' on' : '')} onClick={() => onChange('inboundIds', toggle(inboundIds, i.id))}>
                        {i.tag}<span className="mono small" style={{ marginLeft: 4, opacity: .7 }}>:{i.port}</span>
                      </button>
                    )
                  })}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </section>
  )
}

/** Bulk bind / unbind whole nodes for the selected users. */
function BindNodesModal({ count, detach, onClose, onSubmit }: { count: number; detach?: boolean; onClose: () => void; onSubmit: (ids: string[]) => void }) {
  const { nodes } = useNodes()
  const [ids, setIds] = useState<string[]>([])
  return (
    <Modal title={`${detach ? '解绑' : '绑定'}节点 · ${count} 个用户`} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className={'btn ' + (detach ? 'danger solid' : 'pri')} type="button" disabled={!ids.length} onClick={() => onSubmit(ids)}>{detach ? '解绑' : '绑定'}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{detach ? '移除所选用户对这些节点的整台绑定（分组与单独入站授权不受影响）。' : '所选用户获得这些节点上全部入站的使用权，节点以后新增的入站也自动生效。'}</div>
      <div className="row">
        {nodes.map((n: Node) => <button key={n.id} type="button" className={'chip' + (ids.includes(n.id) ? ' on' : '')} onClick={() => setIds(toggle(ids, n.id))}>{n.name}</button>)}
      </div>
    </Modal>
  )
}

type SubCheckResult = {
  status: string; problems: string[]; formats: Record<string, number>; url: string
  inbounds: { id: string; tag: string; node: string; via: string[]; included: boolean; reason?: string }[]
}
const viaText = (v: string) => (v === 'node' ? '整台节点' : v === 'inbound' ? '单独授权' : '分组 ' + v.replace(/^group:/, ''))

/** Explains what the user's subscription contains, and why something is missing. */
function SubCheck({ userId }: { userId: string }) {
  const [res, setRes] = useState<SubCheckResult | null>(null)
  const { busy, run } = useAction()
  const check = () => run(async () => setRes(await get<SubCheckResult>(`/users/${userId}/sub-check`)))
  const fmts: [string, string][] = [['links', '通用'], ['clash', 'Clash'], ['singbox', 'sing-box'], ['surge', 'Surge'], ['loon', 'Loon'], ['quanx', 'QX']]
  return (
    <section className="field">
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <span className="lbl">订阅诊断</span>
        <button className="btn sm" type="button" disabled={busy} onClick={check}>{res ? '重新检查' : '检查订阅'}</button>
      </div>
      {res && (
        <div className="subcheck">
          {res.problems.length === 0
            ? <div className="note ok-note">订阅正常：共 {res.formats.links ?? 0} 个节点</div>
            : res.problems.map((p) => <div key={p} className="note bad">{p}</div>)}
          <div className="row small">{fmts.map(([k, l]) => <span key={k} className={'tag' + ((res.formats[k] ?? 0) === 0 ? ' warn' : '')}>{l} {res.formats[k] ?? 0}</span>)}</div>
          {res.inbounds.length > 0 && (
            <table>
              <thead><tr><th>入站</th><th>节点</th><th>授权来源</th><th>状态</th></tr></thead>
              <tbody>{res.inbounds.map((i) => (
                <tr key={i.id}>
                  <td className="mono">{i.tag}</td><td>{i.node || '—'}</td>
                  <td className="small">{i.via.map(viaText).join('、')}</td>
                  <td className="small">{i.included ? (i.reason ? <span style={{ color: 'var(--warn-ink)' }}>⚠ {i.reason}</span> : '✓ 包含') : <span style={{ color: 'var(--bad)' }}>✗ {i.reason}</span>}</td>
                </tr>
              ))}</tbody>
            </table>
          )}
          <div className="hint">某个客户端格式为 0 表示该格式不支持这些协议组合（如 Surge 不支持 VLESS / REALITY）。</div>
        </div>
      )}
    </section>
  )
}

function UserDrawer({ user, groups, onClose, onSaved }: { user?: User; groups: Group[]; onClose: () => void; onSaved: () => void }) {
  const initLimitGB = user?.trafficLimit ? String(+(user.trafficLimit / GB).toFixed(2)) : ''
  const [f, setF] = useState({
    username: user?.username ?? '', limitGB: initLimitGB,
    expire: user?.expireAt ? date(user.expireAt) : '', ipLimit: String(user?.ipLimit ?? 0), resetPolicy: user?.resetPolicy ?? 'none',
    resetDay: String(user?.resetDay ?? 1), remark: user?.remark ?? '', status: user?.status === 'disabled' ? 'disabled' : 'active',
    groupIds: user?.groupIds ?? groups.filter((g) => g.isDefault).map((g) => g.id), afterDays: String(user?.expireAfterDays || ''),
    nodeIds: user?.nodeIds ?? [], inboundIds: user?.inboundIds ?? [], telegramId: user?.telegramId ? String(user.telegramId) : '',
    expireMode: !user?.expireAt && user?.expireAfterDays ? 'first' : 'date',
  })
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const save = () => run(async () => {
    const body = {
      username: f.username,
      // The field shows a rounded value: keep the exact quota unless it was edited.
      trafficLimit: user && f.limitGB === initLimitGB ? user.trafficLimit : Math.round(Number(f.limitGB || 0) * GB), ipLimit: Number(f.ipLimit) || 0,
      expireAt: f.expireMode === 'date' && f.expire ? new Date(f.expire + 'T23:59:59').toISOString() : null,
      expireAfterDays: f.expireMode === 'first' ? Number(f.afterDays) || 0 : 0, resetPolicy: f.resetPolicy, resetDay: Number(f.resetDay) || 0,
      remark: f.remark, status: f.status, groupIds: f.groupIds, nodeIds: f.nodeIds, inboundIds: f.inboundIds,
      telegramId: Number(f.telegramId.trim()) || 0,
    }
    if (f.telegramId.trim() && !/^\d{4,15}$/.test(f.telegramId.trim())) throw new Error('Telegram User ID 应为纯数字')
    if (user) await put('/users/' + user.id, body)
    else await post('/users', body)
    onSaved()
  }, user ? '已保存' : '用户已创建')
  const extend = (days: number) => {
    const cur = f.expire ? new Date(f.expire + 'T00:00:00') : null // local midnight, not UTC
    const base = cur && cur > new Date() ? cur : new Date()
    base.setDate(base.getDate() + days)
    set('expire', date(base.toISOString()))
  }
  return (
    <Drawer title={user ? '编辑用户 · ' + user.username : '新建用户'} onClose={onClose} footer={<>
      {user && <button className="btn danger" type="button" style={{ marginRight: 'auto' }} onClick={async () => {
        if (await confirm(`删除用户 ${user.username}？`, { danger: true, ok: '删除' })) run(async () => { await del('/users/' + user.id); onSaved() }, '已删除')
      }}>删除</button>}
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !f.username} onClick={save}>保存</button>
    </>}>
      <div className="form-grid">
        <Field label="用户名" htmlFor="u-name" full><input id="u-name" className="input" value={f.username} onChange={(e) => set('username', e.target.value)} /></Field>
        <Field label="流量配额 (GB)" htmlFor="u-limit" hint="留空或 0 为不限"><input id="u-limit" className="input mono" value={f.limitGB} onChange={(e) => set('limitGB', e.target.value)} inputMode="decimal" /></Field>
        <Field label="设备 / IP 数限制" htmlFor="u-ip" hint="0 为不限"><input id="u-ip" className="input mono" value={f.ipLimit} onChange={(e) => set('ipLimit', e.target.value)} inputMode="numeric" /></Field>
        <Field label="有效期" full>
          <div className="seg">
            <button type="button" className={'opt' + (f.expireMode === 'date' ? ' on' : '')} onClick={() => set('expireMode', 'date')}>固定到期日<small>留空为永不到期</small></button>
            <button type="button" className={'opt' + (f.expireMode === 'first' ? ' on' : '')} onClick={() => set('expireMode', 'first')}>首次使用后开始计时<small>产生第一笔流量时才开始算天数</small></button>
          </div>
        </Field>
        {f.expireMode === 'date' ? <>
          <Field label="到期日期" htmlFor="u-exp" hint="留空为永不到期"><input id="u-exp" className="input mono" type="date" value={f.expire} onChange={(e) => set('expire', e.target.value)} /></Field>
          <Field label="快捷续期">
            <div className="row">{[30, 90, 365].map((d) => <button key={d} type="button" className="btn sm" onClick={() => extend(d)}>+{d} 天</button>)}</div>
          </Field>
        </> : (
          <Field label="有效天数" htmlFor="u-after" hint="首次连接后生效，到期日自动写入"><input id="u-after" className="input mono" inputMode="numeric" value={f.afterDays} onChange={(e) => set('afterDays', e.target.value)} placeholder="30" /></Field>
        )}
        <Field label="流量重置" htmlFor="u-reset">
          <select id="u-reset" className="input" value={f.resetPolicy} onChange={(e) => set('resetPolicy', e.target.value)}>
            <option value="none">不重置</option><option value="monthly">每月固定日</option><option value="cycle">按周期滚动</option>
          </select>
        </Field>
        {f.resetPolicy !== 'none' && (
          <Field label={f.resetPolicy === 'monthly' ? '每月几号' : '周期（天）'} htmlFor="u-rday">
            <input id="u-rday" className="input mono" value={f.resetDay} onChange={(e) => set('resetDay', e.target.value)} inputMode="numeric" />
          </Field>
        )}
        <Field label="Telegram User ID" htmlFor="u-tg" full hint={user?.telegramId ? '已绑定 · 清空即解绑；用户可在机器人中查询流量、到期并接收提醒' : '纯数字，用户在机器人里发 /whoami 可查看；也可让用户发「/bind 订阅链接」自行绑定'}>
          <input id="u-tg" className="input mono" inputMode="numeric" value={f.telegramId} onChange={(e) => set('telegramId', e.target.value.replace(/[^\d]/g, ''))} placeholder="例如 123456789" />
        </Field>
        <Field label="备注" htmlFor="u-remark" full><input id="u-remark" className="input" value={f.remark} onChange={(e) => set('remark', e.target.value)} /></Field>
      </div>
      <Access groups={groups} nodeIds={f.nodeIds} groupIds={f.groupIds} inboundIds={f.inboundIds}
        onChange={(k, v) => set(k, v)} />
      <Field label="状态">
        <div className="seg">
          <button type="button" className={'opt' + (f.status === 'active' ? ' on' : '')} onClick={() => set('status', 'active')}>启用</button>
          <button type="button" className={'opt' + (f.status === 'disabled' ? ' on' : '')} onClick={() => set('status', 'disabled')}>禁用</button>
        </div>
      </Field>
      {user && <SubCheck userId={user.id} />}
      {user && <UserTraffic userId={user.id} />}
      {user && (
        <div className="kv">
          <dt>UUID</dt><dd className="mono">{user.uuid}</dd>
          <dt>密码</dt><dd className="mono">{user.password}</dd>
          <dt>已用</dt><dd className="mono">↑ {bytes(user.usedUp)} · ↓ {bytes(user.usedDown)}</dd>
          <dt>在线 IP</dt><dd className="mono">{user.onlineIps?.join(', ') || '—'}</dd>
        </div>
      )}
    </Drawer>
  )
}

function UserTraffic({ userId }: { userId: string }) {
  const from = useMemo(() => { const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - 29); return d.toISOString() }, [])
  const data = useLoad(() => get<{ series: { t: string; up: number; down: number }[] }>(`/stats/traffic?user=${userId}&bucket=day&from=${encodeURIComponent(from)}`), [userId])
  const days = useMemo(() => {
    const m = new Map((data.data?.series ?? []).map((p) => [date(p.t), p]))
    return Array.from({ length: 30 }, (_, i) => {
      const d = new Date(from); d.setDate(d.getDate() + i)
      const p = m.get(date(d.toISOString()))
      return { day: date(d.toISOString()), up: p?.up ?? 0, down: p?.down ?? 0 }
    })
  }, [data.data, from])
  const max = Math.max(1, ...days.map((d) => d.up + d.down))
  const total = days.reduce((a, d) => a + d.up + d.down, 0)
  const W = 300, H = 80, bw = W / days.length
  return (
    <div className="field">
      <span className="lbl">近 30 天流量 · 合计 {bytes(total)}</span>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" style={{ width: '100%', height: 90 }} role="img" aria-label="近 30 天每日流量">
        {days.map((d, i) => {
          const th = (d.up + d.down) / max * H, uh = d.up / max * H
          return <g key={d.day}><title>{d.day} ↑ {bytes(d.up)} ↓ {bytes(d.down)}</title>
            <rect x={i * bw + bw * 0.15} y={H - th} width={bw * 0.7} height={Math.max(0, th - uh)} rx={1.5} style={{ fill: "var(--pri)" }} />
            <rect x={i * bw + bw * 0.15} y={H - uh} width={bw * 0.7} height={uh} rx={1.5} style={{ fill: "#7DD3E3" }} /></g>
        })}
      </svg>
      <div className="hint">峰值 {bytes(max)} / 天 · 深色为下载、浅色为上传</div>
    </div>
  )
}

function GroupsModal({ onClose, onChanged }: { onClose: () => void; onChanged: () => void }) {
  const groups = useLoad(() => get<Group[]>('/groups'))
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const [edit, setEdit] = useState<{ id?: string; name: string; inboundIds: string[] } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const byNode = useMemo(() => {
    const m = new Map<string, Inbound[]>()
    for (const i of inbounds.data ?? []) m.set(i.nodeName, [...(m.get(i.nodeName) ?? []), i])
    return [...m.entries()]
  }, [inbounds.data])
  return (
    <Modal title="分组" onClose={onClose} wide>
      {!edit ? (
        <>
          <div className="card flush">
            <table>
              <thead><tr><th>分组</th><th>用户</th><th>入站</th><th></th></tr></thead>
              <tbody>
                {(groups.data ?? []).map((g) => (
                  <tr key={g.id}>
                    <td style={{ fontWeight: 500 }}>{g.name}{g.isDefault && <span className="tag info-tag" style={{ marginLeft: 6 }}>默认</span>}</td><td className="mono">{g.userCount}</td><td className="mono">{g.inboundIds.length}</td>
                    <td style={{ textAlign: 'right' }}>
                      <button className="link" type="button" onClick={() => setEdit({ id: g.id, name: g.name, inboundIds: g.inboundIds })}>编辑</button>{'  '}
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(`删除分组 ${g.name}？组内用户将失去对应入站。`, { danger: true })) run(async () => { await del('/groups/' + g.id); groups.reload(); onChanged() })
                      }}>删除</button>
                    </td>
                  </tr>
                ))}
                {groups.data?.length === 0 && <tr><td colSpan={4} className="mute">还没有分组</td></tr>}
              </tbody>
            </table>
          </div>
          <div><button className="btn pri" type="button" onClick={() => setEdit({ name: '', inboundIds: [] })}>+ 新建分组</button></div>
        </>
      ) : (
        <>
          <Field label="名称" htmlFor="g-name"><input id="g-name" className="input" value={edit.name} onChange={(e) => setEdit({ ...edit, name: e.target.value })} /></Field>
          <Field label="授权入站">
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8, maxHeight: 360, overflowY: 'auto' }}>
              {byNode.map(([node, ins]) => (
                <div key={node}>
                  <div className="lbl" style={{ marginBottom: 4 }}>{node}</div>
                  {ins.map((i) => (
                    <label key={i.id} className="chk">
                      <input type="checkbox" checked={edit.inboundIds.includes(i.id)} onChange={(e) => setEdit({ ...edit, inboundIds: e.target.checked ? [...edit.inboundIds, i.id] : edit.inboundIds.filter((x) => x !== i.id) })} />
                      <span className="mono">{i.tag}</span><span className="mute small">{coreShort[i.core]} · {i.port}</span>
                    </label>
                  ))}
                </div>
              ))}
              {byNode.length === 0 && <span className="hint">还没有入站</span>}
            </div>
          </Field>
          <div className="dfoot">
            <button className="btn" type="button" onClick={() => setEdit(null)}>返回</button>
            <button className="btn pri" type="button" disabled={busy || !edit.name} onClick={() => run(async () => {
              if (edit.id) await put('/groups/' + edit.id, edit)
              else await post('/groups', edit)
              setEdit(null)
              groups.reload()
              onChanged()
            }, '已保存')}>保存</button>
          </div>
        </>
      )}
    </Modal>
  )
}

function BulkModal({ groups, onClose, onDone }: { groups: Group[]; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({ prefix: 'user', count: '10', limitGB: '100', days: '30', groupIds: groups.filter((g) => g.isDefault).map((g) => g.id) })
  const { busy, run } = useAction()
  return (
    <Modal title="批量创建" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        const exp = new Date(); exp.setDate(exp.getDate() + Number(f.days || 0))
        await post('/users/bulk', { action: 'create', prefix: f.prefix, count: Number(f.count), template: {
          username: 'x', trafficLimit: Math.round(Number(f.limitGB || 0) * GB), expireAt: Number(f.days) ? exp.toISOString() : null, groupIds: f.groupIds } })
        onDone()
      }, '已创建')}>创建</button>
    </>}>
      <div className="form-grid">
        <Field label="用户名前缀"><input className="input mono" value={f.prefix} onChange={(e) => setF({ ...f, prefix: e.target.value })} /></Field>
        <Field label="数量" hint="生成 前缀001…"><input className="input mono" value={f.count} onChange={(e) => setF({ ...f, count: e.target.value })} /></Field>
        <Field label="流量 (GB)"><input className="input mono" value={f.limitGB} onChange={(e) => setF({ ...f, limitGB: e.target.value })} /></Field>
        <Field label="有效天数" hint="0 为永不到期"><input className="input mono" value={f.days} onChange={(e) => setF({ ...f, days: e.target.value })} /></Field>
      </div>
      <Field label="分组">
        <div className="row">{groups.map((g) => (
          <button key={g.id} type="button" className={'chip' + (f.groupIds.includes(g.id) ? ' on' : '')}
            onClick={() => setF({ ...f, groupIds: f.groupIds.includes(g.id) ? f.groupIds.filter((x) => x !== g.id) : [...f.groupIds, g.id] })}>{g.name}</button>
        ))}</div>
      </Field>
    </Modal>
  )
}

export default function Users() {
  const [params, setParams] = useSearchParams()
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState<Filter>('')
  const [query, setQuery] = useState('')
  useEffect(() => { const t = setTimeout(() => setQuery(q), 300); return () => clearTimeout(t) }, [q])
  const list = useLoad(() => get<{ items: User[]; total: number }>(`/users?limit=500&q=${encodeURIComponent(query)}&filter=${filter}`), [query, filter])
  const groups = useLoad(() => get<Group[]>('/groups'))
  const settings = useLoad(() => get<{ public_url?: string; sub_base_url?: string }>('/settings'))
  const base = settings.data?.sub_base_url || settings.data?.public_url
  if (base) subBase = base.replace(/\/$/, '')
  const [drawer, setDrawer] = useState<User | 'new' | null>(params.get('new') ? 'new' : null)
  const [selected, setSelected] = useState<User | null>(null)
  const [qr, setQr] = useState<User | null>(null)
  const [modal, setModal] = useState<'groups' | 'bulk' | null>(null)
  const [checked, setChecked] = useState<string[]>([])
  const toast = useToast()
  const { busy, run } = useAction()
  const gname = (id: string) => groups.data?.find((g) => g.id === id)?.name ?? id
  const { nodes } = useNodes()
  const nname = (id: string) => nodes.find((n) => n.id === id)?.name ?? '已删除节点'
  const [bind, setBind] = useState<'attach' | 'detach' | null>(null)
  const items = list.data?.items ?? []
  const sel = selected ? items.find((u) => u.id === selected.id) ?? selected : items[0]
  useEffect(() => { if (params.get('new')) setParams({}, { replace: true }) }, [params, setParams])

  const copySub = async (u: User) => {
    try { await navigator.clipboard.writeText(subURL(u)); toast('订阅链接已复制') } catch { toast('复制失败', true) }
  }
  const bulk = (action: string, extra?: object) => run(async () => {
    await post('/users/bulk', { action, ids: checked, ...extra })
    setChecked([])
    list.reload()
  }, '已完成')

  return (
    <>
      <div className="top">
        <div><h1>用户</h1><div className="sub">一个用户跨所有内核、所有协议通用 · 可整台绑定节点、按分组或单独授权入站</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => setModal('groups')}>分组</button>
          <button className="btn" type="button" onClick={() => setModal('bulk')}>批量创建</button>
          <button className="btn pri" type="button" onClick={() => setDrawer('new')}>+ 新建用户</button>
        </div>
      </div>
      <div className="row">
        <label htmlFor="q" className="sr">搜索用户</label>
        <input className="input search" id="q" placeholder="搜索用户名 / UUID / 备注" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 'auto' }} />
        {filters.map((f) => <button key={f.k} className={'chip' + (filter === f.k ? ' on' : '')} type="button" onClick={() => setFilter(f.k)}>{f.label}</button>)}
        <span className="spacer" />
        {checked.length > 0 && (
          <>
            <span className="small mute">已选 {checked.length}</span>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('enable')}>启用</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('disable')}>禁用</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('reset-traffic')}>重置流量</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('extend', { days: 30 })}>续期 30 天</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('attach')}>绑定节点</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('detach')}>解绑节点</button>
            <button className="btn sm danger" type="button" disabled={busy} onClick={() => bulk('delete')}>删除</button>
          </>
        )}
      </div>
      <div className="card flush scroll">
        {items.length === 0 ? <Empty>{list.loading ? '加载中…' : '没有匹配的用户'}</Empty> : (
          <table>
            <thead><tr>
              <th style={{ width: 32 }}><input type="checkbox" aria-label="全选" checked={checked.length === items.length} onChange={(e) => setChecked(e.target.checked ? items.map((u) => u.id) : [])} /></th>
              <th>用户</th><th>状态</th><th>授权</th><th>流量（已用 / 总量）</th><th>到期</th><th>在线 IP</th><th>操作</th>
            </tr></thead>
            <tbody>
              {items.map((u) => {
                const used = u.usedUp + u.usedDown
                const pct = u.trafficLimit ? Math.min(100, (used / u.trafficLimit) * 100) : 0
                const d = daysLeft(u.expireAt)
                return (
                  <tr key={u.id} className="clickable" onClick={() => setSelected(u)}>
                    <td onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={'选择 ' + u.username} checked={checked.includes(u.id)} onChange={(e) => setChecked((x) => e.target.checked ? [...x, u.id] : x.filter((y) => y !== u.id))} /></td>
                    <td><div style={{ fontWeight: 500 }}>{u.username}</div><div className="mono small mute">{u.uuid.slice(0, 4)}…{u.uuid.slice(-4)}</div></td>
                    <td>{statusBadge(u)}</td>
                    <td>
                      {(u.nodeIds ?? []).map((n) => <span key={n} className="tag xray" title="整台节点">{nname(n)}</span>)}
                      {u.groupIds.map((g) => <span key={g} className="tag">{gname(g)}</span>)}
                      {(u.inboundIds ?? []).length > 0 && <span className="tag" title="单独授权的入站">+{u.inboundIds.length} 入站</span>}
                      {!u.groupIds.length && !(u.nodeIds ?? []).length && !(u.inboundIds ?? []).length && <span className="st warn">未授权</span>}
                    </td>
                    <td style={{ minWidth: 180 }}>
                      <span className="mono">{bytes(used)} / {u.trafficLimit ? bytes(u.trafficLimit) : '不限'}</span>
                      <div className="bar" style={{ marginTop: 6 }}><div style={{ width: pct + '%', background: pct >= 100 ? 'var(--bad)' : 'var(--pri)' }} /></div>
                    </td>
                    <td className="mono" style={d !== null && d <= 7 && d >= 0 ? { color: 'var(--warn-ink)' } : undefined}>{u.expireAt ? (d !== null && d >= 0 && d <= 7 ? `${d} 天后` : date(u.expireAt)) : '永不'}</td>
                    <td className="mono">{isOnline(u.onlineAt) ? u.onlineIps.length : 0} / {u.ipLimit || '∞'}</td>
                    <td onClick={(e) => e.stopPropagation()}>
                      {u.status === 'limited' ? <button className="btn sm" type="button" onClick={() => run(async () => { await post(`/users/${u.id}/reset-traffic`); list.reload() }, '流量已重置')}>重置流量</button>
                        : u.status === 'disabled' ? <button className="btn sm" type="button" onClick={() => run(async () => { await post('/users/bulk', { action: 'enable', ids: [u.id] }); list.reload() }, '已启用')}>启用</button>
                        : <>
                          <button className="icon-btn" type="button" aria-label="复制订阅链接" onClick={() => copySub(u)}><Icon.Copy /></button>
                          <button className="icon-btn" type="button" aria-label="显示二维码" onClick={() => setQr(u)}><Icon.QR /></button>
                        </>}
                      <button className="icon-btn" type="button" aria-label="编辑" onClick={() => setDrawer(u)}><Icon.Edit /></button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
      {sel && (
        <div className="card row" style={{ justifyContent: 'space-between', gap: 16 }}>
          <div style={{ minWidth: 0 }}>
            <div style={{ fontWeight: 600 }}>订阅链接 · {sel.username}</div>
            <div className="mono" style={{ fontSize: 13, color: 'var(--ink-2)', marginTop: 4, overflowWrap: 'anywhere' }}>{subURL(sel)}</div>
            <div className="sub">按客户端自动输出：通用链接 · Clash/mihomo · sing-box · Surge；浏览器打开显示订阅信息页</div>
          </div>
          <div className="row">
            <button className="btn" type="button" onClick={() => run(async () => { await post(`/users/${sel.id}/reset-token`); list.reload() }, 'token 已重置，旧链接立即失效')}>重置 token</button>
            <button className="btn" type="button" onClick={() => setQr(sel)}>二维码</button>
            <button className="btn" type="button" onClick={() => copySub(sel)}>复制</button>
          </div>
        </div>
      )}
      {drawer && groups.data && <UserDrawer user={drawer === 'new' ? undefined : drawer} groups={groups.data} onClose={() => setDrawer(null)} onSaved={() => { setDrawer(null); list.reload() }} />}
      {qr && (
        <Modal title={'订阅 · ' + qr.username} onClose={() => setQr(null)}>
          <QR text={subURL(qr)} size={220} />
          <CopyBox text={subURL(qr)} />
        </Modal>
      )}
      {modal === 'groups' && <GroupsModal onClose={() => setModal(null)} onChanged={() => { groups.reload(); list.reload() }} />}
      {bind && <BindNodesModal count={checked.length} detach={bind === 'detach'} onClose={() => setBind(null)} onSubmit={(ids) => {
        bulk(bind === 'detach' ? 'detach-nodes' : 'attach-nodes', { nodeIds: ids }); setBind(null)
      }} />}
      {modal === 'bulk' && groups.data && <BulkModal groups={groups.data} onClose={() => setModal(null)} onDone={() => { setModal(null); list.reload() }} />}
    </>
  )
}
