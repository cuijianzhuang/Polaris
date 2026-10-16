import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { Copy, CopyBox, copyText, Drawer, Empty, Field, Modal, QR, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, bytes, coreShort, date, datetime, daysLeft, GB, isOnline, userStatus } from '../format'
import type { Group, Inbound, Node, User, UserDevice } from '../types'
import { AdjustModal, BulkCreateModal, downloadJSON, ImportModal, TrafficEdit } from '../components/UserTools'

type Filter = '' | 'active' | 'online' | 'expiring' | 'near' | 'limited' | 'expired' | 'disabled'
const filters: { k: Filter; label: string; sum: string }[] = [
  { k: '', label: '全部', sum: 'all' }, { k: 'active', label: '正常', sum: 'active' }, { k: 'online', label: '在线', sum: 'online' },
  { k: 'expiring', label: '即将到期', sum: 'expiring' }, { k: 'near', label: '流量将满', sum: 'near' }, { k: 'limited', label: '已超额', sum: 'limited' }, { k: 'expired', label: '已过期', sum: 'expired' },
  { k: 'disabled', label: '已禁用', sum: 'disabled' },
]
const PAGE = 100

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
              <span className="dot" style={{ background: n.status === 'online' ? 'var(--ok)' : 'var(--input-strong)' }} />{n.name}
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
    renewDays: user?.renewDays ? String(user.renewDays) : '', renewMax: user?.renewMax ? String(user.renewMax) : '',
    deviceLimit: String(user?.deviceLimit ?? 0),
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
      renewDays: Number(f.renewDays) || 0, renewMax: Number(f.renewMax) || 0, deviceLimit: Number(f.deviceLimit) || 0,
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
        <Field label="同时在线 IP 数" htmlFor="u-ip" hint="0 为不限；受信任地址（设置中配置）不计入"><input id="u-ip" className="input mono" value={f.ipLimit} onChange={(e) => set('ipLimit', e.target.value)} inputMode="numeric" /></Field>
        <Field label="设备数（HWID）" htmlFor="u-dev" hint="0 为不限；按 Happ、v2RayTun 等客户端拉订阅时上报的设备 ID 计数"><input id="u-dev" className="input mono" value={f.deviceLimit} onChange={(e) => set('deviceLimit', e.target.value)} inputMode="numeric" /></Field>
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
            <option value="none">不重置</option><option value="daily">每天</option><option value="weekly">每周固定日</option>
            <option value="monthly">每月固定日</option><option value="cycle">按周期滚动</option>
          </select>
        </Field>
        {f.resetPolicy === 'weekly' && (
          <Field label="每周几" htmlFor="u-rday">
            <select id="u-rday" className="input" value={f.resetDay} onChange={(e) => set('resetDay', e.target.value)}>
              {['一', '二', '三', '四', '五', '六', '日'].map((d, i) => <option key={d} value={String(i + 1)}>周{d}</option>)}
            </select>
          </Field>
        )}
        {(f.resetPolicy === 'monthly' || f.resetPolicy === 'cycle') && (
          <Field label={f.resetPolicy === 'monthly' ? '每月几号' : '周期（天）'} htmlFor="u-rday" hint={f.resetPolicy === 'monthly' ? '29–31 在小月按月末' : undefined}>
            <input id="u-rday" className="input mono" value={f.resetDay} onChange={(e) => set('resetDay', e.target.value)} inputMode="numeric" />
          </Field>
        )}
        <Field label="到期自动续订（天）" htmlFor="u-renew" hint="到期时自动顺延这么多天并清零流量；留空不续订">
          <input id="u-renew" className="input mono" value={f.renewDays} onChange={(e) => set('renewDays', e.target.value)} inputMode="numeric" placeholder="例如 30" />
        </Field>
        <Field label="最多续订次数" htmlFor="u-rmax" hint={user?.renewDays ? `已续订 ${user.renewCount} 次；留空不限` : '留空不限'}>
          <input id="u-rmax" className="input mono" value={f.renewMax} disabled={!f.renewDays} onChange={(e) => set('renewMax', e.target.value)} inputMode="numeric" />
        </Field>
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
      {user && <UserLinks userId={user.id} />}
      {user && <Devices userId={user.id} limit={user.deviceLimit} />}
      {user && <SubCheck userId={user.id} />}
      {user && <UserTraffic userId={user.id} />}
      {user && (
        <div className="kv">
          <dt>UUID</dt><dd className="mono">{user.uuid}</dd>
          <dt>密码</dt><dd className="mono">{user.password}</dd>
          <dt>已用</dt><dd className="mono">↑ {bytes(user.usedUp)} · ↓ {bytes(user.usedDown)} <TrafficEdit user={user} onSaved={onSaved} /></dd>
          <dt>在线 IP</dt><dd className="mono">{user.onlineIps?.join(', ') || '—'}</dd>
          <dt>最近拉取订阅</dt><dd className="small">{user.lastSubAt ? `${datetime(user.lastSubAt)} · ${user.lastSubUa || '—'}` : '—'}</dd>
        </div>
      )}
    </Drawer>
  )
}

// UserLinks lists one share link per node with copy and QR code.
function UserLinks({ userId }: { userId: string }) {
  const data = useLoad(() => get<{ links: { name: string; uri: string }[] | null }>(`/users/${userId}/links`), [userId])
  const [qr, setQr] = useState<string | null>(null)
  const links = data.data?.links ?? []
  return (
    <details className="more">
      <summary>节点链接与二维码（{links.length}）</summary>
      <div style={{ display: 'grid', gap: 6, marginTop: 8 }}>
        {links.length === 0 && <div className="sub">没有可用节点（见下方「订阅诊断」）</div>}
        {links.map((l) => (
          <div key={l.uri} className="row" style={{ flexWrap: 'nowrap' }}>
            <span className="small" style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={l.name}>{l.name}</span>
            <Copy text={l.uri} />
            <button className="btn sm" type="button" onClick={() => setQr(qr === l.uri ? null : l.uri)}>二维码</button>
          </div>
        ))}
        {qr && <div style={{ justifySelf: 'center' }}><QR text={qr} size={200} /></div>}
      </div>
    </details>
  )
}

// Devices lists the HWIDs that fetched the subscription.
function Devices({ userId, limit }: { userId: string; limit: number }) {
  const data = useLoad(() => get<UserDevice[]>(`/users/${userId}/devices`), [userId])
  const { run } = useAction()
  const ds = data.data ?? []
  if (!ds.length && !limit) return null
  return (
    <details className="more">
      <summary>设备（{ds.length}{limit ? ` / ${limit}` : ''}）</summary>
      <div style={{ display: 'grid', gap: 6, marginTop: 8 }}>
        {ds.length === 0 && <div className="sub">还没有客户端上报设备 ID</div>}
        {ds.map((d) => (
          <div key={d.id} className="row" style={{ flexWrap: 'nowrap' }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="small">{[d.model, d.os].filter(Boolean).join(' · ') || d.userAgent || '未知设备'}</div>
              <div className="hint mono">{d.hwid.slice(0, 16)}… · {d.ip} · {ago(d.lastSeen)}</div>
            </div>
            <button className="btn sm danger" type="button" onClick={() => run(async () => { await del(`/users/${userId}/devices/${d.id}`); data.reload() }, '已移除')}>移除</button>
          </div>
        ))}
        {ds.length > 1 && <div><button className="btn sm" type="button" onClick={() => run(async () => { await del(`/users/${userId}/devices/all`); data.reload() }, '已清空')}>清空全部设备</button></div>}
      </div>
    </details>
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
            <rect x={i * bw + bw * 0.15} y={H - uh} width={bw * 0.7} height={uh} rx={1.5} style={{ fill: 'color-mix(in srgb, var(--pri) 45%, transparent)' }} /></g>
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

export default function Users() {
  const [params, setParams] = useSearchParams()
  const [q, setQ] = useState(() => params.get('q') ?? '')
  const [filter, setFilter] = useState<Filter>(() => (filters.some((f) => f.k === params.get('filter')) ? params.get('filter') as Filter : ''))
  const [query, setQuery] = useState('')
  useEffect(() => { const t = setTimeout(() => setQuery(q), 300); return () => clearTimeout(t) }, [q])
  const [group, setGroup] = useState('')
  const [sort, setSort] = useState<{ k: string; desc: boolean }>({ k: '', desc: true })
  const [page, setPage] = useState(0)
  useEffect(() => setPage(0), [query, filter, group, sort])
  const list = useLoad(() => get<{ items: User[]; total: number; summary?: Record<string, number> }>(
    `/users?limit=${PAGE}&offset=${page * PAGE}&q=${encodeURIComponent(query)}&filter=${filter}&group=${group}${sort.k ? `&sort=${sort.k}&order=${sort.desc ? 'desc' : 'asc'}` : ''}`),
  [query, filter, group, sort, page])
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
  const [modal, setModal] = useState<'groups' | 'bulk' | 'adjust' | 'import' | null>(null)
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
    try { await copyText(subURL(u)); toast('订阅链接已复制') } catch { window.prompt('复制失败，请手动复制：', subURL(u)) }
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
          <details className="menu">
            <summary className="btn">更多</summary>
            <div className="menu-list" onClick={(e) => { (e.currentTarget.parentElement as HTMLDetailsElement).open = false }}>
              <button type="button" onClick={() => setModal('import')}>导入用户（JSON / 3x-ui）</button>
              <button type="button" onClick={() => run(async () => downloadJSON(`polaris-users-${new Date().toISOString().slice(0, 10)}.json`, await get('/users/export')), '已导出')}>导出全部用户</button>
              <button type="button" disabled={!sum.limited && !sum.expired} onClick={async () => {
                if (await confirm(`删除全部 ${(sum.limited ?? 0) + (sum.expired ?? 0)} 个已超额 / 已过期的用户？`, { danger: true, ok: '删除' }))
                  run(async () => { await post('/users/bulk', { action: 'delete-depleted' }); list.reload() }, '已删除')
              }}>删除已超额 / 已过期用户</button>
              <button type="button" onClick={async () => {
                if (await confirm('把全部用户的已用流量清零？', { danger: true, ok: '全部重置' }))
                  run(async () => { await post('/users/bulk', { action: 'reset-traffic', all: true }); list.reload() }, '已重置')
              }}>重置全部用户流量</button>
            </div>
          </details>
          <button className="btn" type="button" onClick={() => setModal('bulk')}>批量创建</button>
          <button className="btn pri" type="button" onClick={() => setDrawer('new')}>+ 新建用户</button>
        </div>
      </div>
      <div className="row">
        <label htmlFor="q" className="sr">搜索用户</label>
        <input className="input search" id="q" placeholder="搜索用户名 / UUID / 备注" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 'auto' }} />
        <div className="chips">
          {filters.map((f) => <button key={f.k} className={'chip' + (filter === f.k ? ' on' : '')} type="button" onClick={() => setFilter(f.k)}>{f.label}{sum[f.sum] != null && <span className="chip-n">{sum[f.sum]}</span>}</button>)}
          {(groups.data?.length ?? 0) > 0 && (
            <select className="input" style={{ width: 'auto', minHeight: 34 }} value={group} onChange={(e) => setGroup(e.target.value)} aria-label="按分组筛选">
              <option value="">全部分组</option>
              {groups.data!.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
            </select>
          )}
        </div>
        <span className="spacer" />
        {checked.length > 0 && (
          <>
            <span className="small mute">已选 {checked.length}</span>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('enable')}>启用</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('disable')}>禁用</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => bulk('reset-traffic')}>重置流量</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setModal('adjust')}>调整到期 / 流量 / 限制</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => downloadJSON('polaris-users.json', await get('/users/export?ids=' + checked.join(','))), '已导出')}>导出</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('attach')}>绑定节点</button>
            <button className="btn sm" type="button" disabled={busy} onClick={() => setBind('detach')}>解绑节点</button>
            <button className="btn sm danger" type="button" disabled={busy} onClick={() => bulk('delete')}>删除</button>
          </>
        )}
      </div>
      <div className="card flush scroll">
        {items.length === 0 ? <Empty>{list.loading ? '加载中…' : '没有匹配的用户'}</Empty> : (
          <table className="rtable">
            <thead><tr>
              <th style={{ width: 32 }}><input type="checkbox" aria-label="全选" checked={checked.length === items.length} onChange={(e) => setChecked(e.target.checked ? items.map((u) => u.id) : [])} /></th>
              {th('username', '用户')}<th>状态</th><th>授权</th>{th('used', '流量（已用 / 总量）')}{th('expire', '到期')}{th('online', '最近在线')}<th>在线 IP</th><th>操作</th>
            </tr></thead>
            <tbody>
              {items.map((u) => {
                const used = u.usedUp + u.usedDown
                const pct = u.trafficLimit ? Math.min(100, (used / u.trafficLimit) * 100) : 0
                const d = daysLeft(u.expireAt)
                return (
                  <tr key={u.id} className="clickable" onClick={() => setSelected(u)}>
                    <td className="rt-check" onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={'选择 ' + u.username} checked={checked.includes(u.id)} onChange={(e) => setChecked((x) => e.target.checked ? [...x, u.id] : x.filter((y) => y !== u.id))} /></td>
                    <td className="rt-head"><div style={{ fontWeight: 500 }}>{u.username}</div><div className="mono small mute">{u.uuid.slice(0, 4)}…{u.uuid.slice(-4)}</div></td>
                    <td data-label="状态">{statusBadge(u)}</td>
                    <td data-label="授权">
                      {(u.nodeIds ?? []).map((n) => <span key={n} className="tag xray" title="整台节点">{nname(n)}</span>)}
                      {u.groupIds.map((g) => <span key={g} className="tag">{gname(g)}</span>)}
                      {(u.inboundIds ?? []).length > 0 && <span className="tag" title="单独授权的入站">+{u.inboundIds.length} 入站</span>}
                      {!u.groupIds.length && !(u.nodeIds ?? []).length && !(u.inboundIds ?? []).length && <span className="st warn">未授权</span>}
                    </td>
                    <td data-label="流量" className="rt-wrap" style={{ minWidth: 180 }}>
                      <span className="mono">{bytes(used)} / {u.trafficLimit ? bytes(u.trafficLimit) : '不限'}</span>
                      <div className="bar" style={{ marginTop: 6 }}><div style={{ width: pct + '%', background: pct >= 100 ? 'var(--bad)' : 'var(--pri)' }} /></div>
                    </td>
                    <td data-label="到期" className="mono" style={d !== null && d <= 7 && d >= 0 ? { color: 'var(--warn-ink)' } : undefined}>{u.expireAt ? (d !== null && d >= 0 && d <= 7 ? `${d} 天后` : date(u.expireAt)) : '永不'}</td>
                    <td data-label="最近在线" className="small">{isOnline(u.onlineAt) ? <span style={{ color: 'var(--ok)' }}>在线</span> : u.onlineAt ? ago(u.onlineAt) : <span className="mute">从未</span>}</td>
                    <td data-label="在线 IP" className="mono">{isOnline(u.onlineAt) ? u.onlineIps.length : 0} / {u.ipLimit || '∞'}</td>
                    <td className="rt-actions" onClick={(e) => e.stopPropagation()}>
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
      {total > PAGE && (
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <span className="small mute">第 {page * PAGE + 1}–{Math.min(total, (page + 1) * PAGE)} 个，共 {total} 个</span>
          <button className="btn sm" type="button" disabled={page === 0} onClick={() => setPage(page - 1)}>上一页</button>
          <button className="btn sm" type="button" disabled={(page + 1) * PAGE >= total} onClick={() => setPage(page + 1)}>下一页</button>
        </div>
      )}
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
      {modal === 'bulk' && groups.data && <BulkCreateModal groups={groups.data} onClose={() => setModal(null)} onDone={() => { setModal(null); list.reload() }} />}
      {modal === 'adjust' && <AdjustModal ids={checked} onClose={() => setModal(null)} onDone={() => { setModal(null); setChecked([]); list.reload() }} />}
      {modal === 'import' && <ImportModal onClose={() => setModal(null)} onDone={() => { setModal(null); list.reload() }} />}
    </>
  )
}
