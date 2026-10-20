import { ProTag, useFeature } from '../components/License'
import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import TrafficChart from '../components/TrafficChart'
import UserTrafficSources from '../components/UserTrafficSources'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { Copy, CopyBox, copyText, Drawer, Empty, Field, Modal, QR, useAction, useConfirm, useLoad, usePoll, useRefresh, RefreshPicker, useToast, SkeletonRows, Section, useUndo, useToastApi, runBatch } from '../components/ui'
import { ago, bytes, coreShort, date, datetime, daysLeft, GB, isOnline, userStatus } from '../format'
import type { Group, Inbound, Node, NodeQuota, User, UserDevice } from '../types'
import { AdjustModal, BulkCreateModal, downloadJSON, ImportModal, TrafficEdit } from '../components/UserTools'
import { t, tr } from '../i18n'
import { planSummary, UserPlanModal } from '../components/Plans'

type Filter = '' | 'active' | 'online' | 'expiring' | 'near' | 'limited' | 'expired' | 'disabled'
const filters: { k: Filter; label: string; sum: string }[] = [
  { k: '', label: t('全部'), sum: 'all' }, { k: 'active', label: t('正常'), sum: 'active' }, { k: 'online', label: t('在线'), sum: 'online' },
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
  if (u.status === 'active' && isOnline(u.onlineAt)) return <span className="st ok">● {t('在线')}</span>
  if (u.status === 'active' && d !== null && d <= 7) return <span className="st warn">{t('即将到期')}</span>
  if (u.bannedUntil && new Date(u.bannedUntil) > new Date()) return <span className="st bad">{t('IP 超限封禁')}</span>
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
        <b>{t('授权')}</b>
        <span className="small mute">{t('可用')} <b className="mono">{effective.length}</b> {t('个入站')} · <b className="mono">{effNodes.size}</b> {t('个节点')}</span>
      </div>
      <Field label={t('节点')} hint={t('绑定整台节点：包含其全部入站，以后新增的入站也自动生效')}>
        <div className="row">
          {nodes.map((n) => (
            <button key={n.id} type="button" className={'chip' + (nodeIds.includes(n.id) ? ' on' : '')} aria-pressed={nodeIds.includes(n.id)}
              onClick={() => onChange('nodeIds', toggle(nodeIds, n.id))}>
              <span className="dot" style={{ background: n.status === 'online' ? 'var(--ok)' : 'var(--input-strong)' }} />{n.name}
            </button>
          ))}
          {nodes.length === 0 && <span className="hint">{t('还没有节点')}</span>}
        </div>
      </Field>
      <Field label={t('分组')} hint={t('按分组授权：分组里的入站对组内所有用户生效')}>
        <div className="row">
          {groups.map((g) => (
            <button key={g.id} type="button" className={'chip' + (groupIds.includes(g.id) ? ' on' : '')} aria-pressed={groupIds.includes(g.id)}
              onClick={() => onChange('groupIds', toggle(groupIds, g.id))}>{tr(g.name)}<span className="mute small" style={{ marginLeft: 4 }}>{g.inboundIds.length}</span></button>
          ))}
          {groups.length === 0 && <span className="hint">{t('还没有分组')}</span>}
        </div>
      </Field>
      <div>
        <button type="button" className="link" onClick={() => setShowIns(!showIns)}>{showIns ? t('收起') : t('单独授权入站')}{inboundIds.length > 0 && t('（{n}）', { n: inboundIds.length })}</button>
        {showIns && (
          <div className="access-ins">
            {nodes.filter((n) => ins.some((i) => i.nodeId === n.id)).map((n) => (
              <div key={n.id} className="access-node">
                <div className="small" style={{ fontWeight: 600 }}>{n.name}{nodeIds.includes(n.id) && <span className="tag info-tag" style={{ marginLeft: 6 }}>{t('已整台绑定')}</span>}</div>
                <div className="row">
                  {ins.filter((i) => i.nodeId === n.id).map((i) => {
                    const covered = nodeIds.includes(n.id) || viaGroup.has(i.id)
                    const on = inboundIds.includes(i.id)
                    return (
                      <button key={i.id} type="button" disabled={covered && !on} title={covered ? t('已通过节点或分组授权') : ''}
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
    <Modal title={detach ? t('解绑节点 · {n} 个用户', { n: count }) : t('绑定节点 · {n} 个用户', { n: count })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className={'btn ' + (detach ? 'danger solid' : 'pri')} type="button" disabled={!ids.length} onClick={() => onSubmit(ids)}>{detach ? t('解绑') : t('绑定')}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{detach ? t('移除所选用户对这些节点的整台绑定（分组与单独入站授权不受影响）。') : t('所选用户获得这些节点上全部入站的使用权，节点以后新增的入站也自动生效。')}</div>
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
const viaText = (v: string) => (v === 'node' ? t('整台节点') : v === 'inbound' ? t('单独授权') : t('分组 {name}', { name: v.replace(/^group:/, '') }))

/** Explains what the user's subscription contains, and why something is missing. */
function SubCheck({ userId }: { userId: string }) {
  const [res, setRes] = useState<SubCheckResult | null>(null)
  const { busy, run } = useAction()
  const check = () => run(async () => setRes(await get<SubCheckResult>(`/users/${userId}/sub-check`)))
  const fmts: [string, string][] = [['links', t('通用')], ['clash', 'Clash'], ['singbox', 'sing-box'], ['surge', 'Surge'], ['loon', 'Loon'], ['quanx', 'QX']]
  return (
    <section className="field">
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <span className="lbl">{t('订阅诊断')}</span>
        <button className="btn sm" type="button" disabled={busy} onClick={check}>{res ? t('重新检查') : t('检查订阅')}</button>
      </div>
      {res && (
        <div className="subcheck">
          {res.problems.length === 0
            ? <div className="note ok-note">{t('订阅正常：共 {n} 个节点', { n: res.formats.links ?? 0 })}</div>
            : res.problems.map((p) => <div key={p} className="note bad">{p}</div>)}
          <div className="row small">{fmts.map(([k, l]) => <span key={k} className={'tag' + ((res.formats[k] ?? 0) === 0 ? ' warn' : '')}>{l} {res.formats[k] ?? 0}</span>)}</div>
          {res.inbounds.length > 0 && (
            <table>
              <thead><tr><th>{t('入站')}</th><th>{t('节点')}</th><th>{t('授权来源')}</th><th>{t('状态')}</th></tr></thead>
              <tbody>{res.inbounds.map((i) => (
                <tr key={i.id}>
                  <td className="mono">{i.tag}</td><td>{i.node || '—'}</td>
                  <td className="small">{i.via.map(viaText).join(t('、'))}</td>
                  <td className="small">{i.included ? (i.reason ? <span style={{ color: 'var(--warn-ink)' }}>⚠ {i.reason}</span> : '✓ ' + t('包含')) : <span style={{ color: 'var(--bad)' }}>✗ {i.reason}</span>}</td>
                </tr>
              ))}</tbody>
            </table>
          )}
          <div className="hint">{t('某个客户端格式为 0 表示该格式不支持这些协议组合（如 Surge 不支持 VLESS / REALITY）。')}</div>
        </div>
      )}
    </section>
  )
}

function UserDrawer({ user, groups: all, onClose, onSaved, onDelete }: { user?: User; groups: Group[]; onClose: () => void; onSaved: () => void; onDelete: (u: User) => void }) {
  const speedOK = useFeature('speed_limit')
  const groups = all.filter((g) => !g.plan)
  const plans = all.filter((g) => g.plan)
  const [planModal, setPlanModal] = useState(false)
  const sp = plans.length > 0 || user?.planId ? 1 : 0 // the plan section shifts the step numbers
  const initLimitGB = user?.trafficLimit ? String(+(user.trafficLimit / GB).toFixed(2)) : ''
  const [f, setF] = useState({
    username: user?.username ?? '', limitGB: initLimitGB,
    expire: user?.expireAt ? date(user.expireAt) : '', ipLimit: String(user?.ipLimit ?? 0), resetPolicy: user?.resetPolicy ?? 'none',
    resetDay: String(user?.resetDay ?? 1), remark: user?.remark ?? '', status: user?.status === 'disabled' ? 'disabled' : 'active',
    groupIds: user?.groupIds ?? groups.filter((g) => g.isDefault).map((g) => g.id), afterDays: String(user?.expireAfterDays || ''),
    nodeIds: user?.nodeIds ?? [], inboundIds: user?.inboundIds ?? [], telegramId: user?.telegramId ? String(user.telegramId) : '', email: user?.email ?? '',
    expireMode: !user?.expireAt && user?.expireAfterDays ? 'first' : 'date',
    renewDays: user?.renewDays ? String(user.renewDays) : '', renewMax: user?.renewMax ? String(user.renewMax) : '',
    deviceLimit: String(user?.deviceLimit ?? 0),
    speedUp: user?.speedUpMbps ? String(user.speedUpMbps) : '', speedDown: user?.speedDownMbps ? String(user.speedDownMbps) : '',
    planId: user?.planId ?? '',
  })
  // Picking a plan fills in its limits; they can still be edited.
  const pickPlan = (id: string) => {
    const p = plans.find((x) => x.id === id)
    setF((x) => {
      if (!p) return { ...x, planId: id }
      const exp = new Date()
      exp.setDate(exp.getDate() + (p.days ?? 0))
      return {
        ...x, planId: id, limitGB: p.trafficLimit ? String(+(p.trafficLimit / GB).toFixed(2)) : '', ipLimit: String(p.ipLimit ?? 0),
        deviceLimit: String(p.deviceLimit ?? 0), resetPolicy: p.resetPolicy || 'none', resetDay: String(p.resetDay || 1),
        expireMode: 'date', expire: p.days ? date(exp.toISOString()) : '', speedUp: '', speedDown: '',
      }
    })
  }
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  const save = () => run(async () => {
    const body = {
      username: f.username,
      // The field shows a rounded value: keep the exact quota unless it was edited.
      trafficLimit: user && f.limitGB === initLimitGB ? user.trafficLimit : Math.round(Number(f.limitGB || 0) * GB), ipLimit: Number(f.ipLimit) || 0,
      expireAt: f.expireMode === 'date' && f.expire ? new Date(f.expire + 'T23:59:59').toISOString() : null,
      expireAfterDays: f.expireMode === 'first' ? Number(f.afterDays) || 0 : 0, resetPolicy: f.resetPolicy, resetDay: Number(f.resetDay) || 0,
      remark: f.remark, status: f.status, groupIds: f.groupIds, nodeIds: f.nodeIds, inboundIds: f.inboundIds,
      telegramId: Number(f.telegramId.trim()) || 0, email: f.email.trim(),
      renewDays: Number(f.renewDays) || 0, renewMax: Number(f.renewMax) || 0, deviceLimit: Number(f.deviceLimit) || 0,
      speedUpMbps: Number(f.speedUp) || 0, speedDownMbps: Number(f.speedDown) || 0, planId: f.planId,
    }
    if (f.telegramId.trim() && !/^\d{4,15}$/.test(f.telegramId.trim())) throw new Error(t('Telegram User ID 应为纯数字'))
    if (user) await put('/users/' + user.id, body)
    else await post('/users', body)
    onSaved()
  }, user ? t('已保存') : t('用户已创建'))
  const extend = (days: number) => {
    const cur = f.expire ? new Date(f.expire + 'T00:00:00') : null // local midnight, not UTC
    const base = cur && cur > new Date() ? cur : new Date()
    base.setDate(base.getDate() + days)
    set('expire', date(base.toISOString()))
  }
  return (
    <Drawer title={user ? t('编辑用户 · {name}', { name: user.username }) : t('新建用户')} onClose={onClose} footer={<>
      {user && <button className="btn danger" type="button" style={{ marginRight: 'auto' }} onClick={() => onDelete(user)}>{t('删除')}</button>}
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.username} onClick={save}>{t('保存')}</button>
    </>}>
      <Section step={1} title={t('基本信息')} summary={[f.username || t('未命名'), f.status === 'disabled' ? t('已禁用') : t('启用'), f.remark].filter(Boolean).join(' · ')}>
        <div className="form-grid">
          <Field label={t('用户名')} htmlFor="u-name" full><input id="u-name" className="input" value={f.username} onChange={(e) => set('username', e.target.value)} /></Field>
          <Field label={t('备注')} htmlFor="u-remark" full><input id="u-remark" className="input" value={f.remark} onChange={(e) => set('remark', e.target.value)} /></Field>
        </div>
        <Field label={t('状态')}>
          <div className="seg">
            <button type="button" className={'opt' + (f.status === 'active' ? ' on' : '')} onClick={() => set('status', 'active')}>{t('启用')}</button>
            <button type="button" className={'opt' + (f.status === 'disabled' ? ' on' : '')} onClick={() => set('status', 'disabled')}>{t('禁用')}</button>
          </div>
        </Field>
      </Section>
      {(plans.length > 0 || user?.planId) && (
        <Section step={2} title={t('套餐')} summary={plans.find((p) => p.id === f.planId)?.name ?? t('无套餐')}>
          <div className="form-grid">
            <Field label={t('套餐')} htmlFor="u-plan" full hint={t('选择后自动填入套餐的流量、到期、设备数等，可再单独修改；套餐决定可用节点')}>
              <select id="u-plan" className="input" value={f.planId} onChange={(e) => pickPlan(e.target.value)}>
                <option value="">{t('无套餐')}</option>
                {plans.map((p) => <option key={p.id} value={p.id}>{p.name} · {planSummary(p)}</option>)}
              </select>
            </Field>
          </div>
          {user && <button className="btn sm" type="button" onClick={() => setPlanModal(true)}>{t('开通 / 续费套餐…')}</button>}
          {planModal && user && <UserPlanModal ids={[user.id]} current={user.planId} onClose={() => setPlanModal(false)} onDone={() => { setPlanModal(false); onSaved() }} />}
        </Section>
      )}
      <Section step={2 + sp} title={t('流量与限制')} summary={[Number(f.limitGB) ? f.limitGB + ' GB' : t('不限流量'), Number(f.ipLimit) ? t('{n} 个 IP', { n: f.ipLimit }) : '', Number(f.deviceLimit) ? t('{n} 台设备', { n: f.deviceLimit }) : '', Number(f.speedUp) || Number(f.speedDown) ? t('限速 ↑{up} ↓{down} Mbps', { up: Number(f.speedUp) || '∞', down: Number(f.speedDown) || '∞' }) : '', { none: '', daily: t('每日重置'), weekly: t('每周重置'), monthly: t('每月重置'), cycle: t('周期重置') }[f.resetPolicy as 'none']].filter(Boolean).join(' · ')}>
        <div className="form-grid">
          <Field label={t('流量配额 (GB)')} htmlFor="u-limit" hint={t('留空或 0 为不限')}><input id="u-limit" className="input mono" value={f.limitGB} onChange={(e) => set('limitGB', e.target.value)} inputMode="decimal" /></Field>
          <Field label={t('同时在线 IP 数')} htmlFor="u-ip" hint={t('0 为不限；受信任地址（设置中配置）不计入')}><input id="u-ip" className="input mono" value={f.ipLimit} onChange={(e) => set('ipLimit', e.target.value)} inputMode="numeric" /></Field>
          <Field label={<>{t('限速 上行 / 下行 (Mbps)')}<ProTag on={speedOK} /></>} hint={t('每个节点上该用户所有连接合计；留空跟随分组套餐，都没有则不限。Hysteria2 入站不支持')}>
            <div className="row" style={{ flexWrap: 'nowrap' }}>
              <input aria-label={t('上行 Mbps')} disabled={!speedOK} className="input mono" value={f.speedUp} placeholder={t('上行')} onChange={(e) => set('speedUp', e.target.value)} inputMode="numeric" />
              <input aria-label={t('下行 Mbps')} disabled={!speedOK} className="input mono" value={f.speedDown} placeholder={t('下行')} onChange={(e) => set('speedDown', e.target.value)} inputMode="numeric" />
            </div>
          </Field>
          <Field label={t('设备数（HWID）')} htmlFor="u-dev" hint={t('0 为不限；按 Happ、v2RayTun 等客户端拉订阅时上报的设备 ID 计数')}><input id="u-dev" className="input mono" value={f.deviceLimit} onChange={(e) => set('deviceLimit', e.target.value)} inputMode="numeric" /></Field>
          <Field label={t('流量重置')} htmlFor="u-reset">
            <select id="u-reset" className="input" value={f.resetPolicy} onChange={(e) => set('resetPolicy', e.target.value)}>
              <option value="none">{t('不重置')}</option><option value="daily">{t('每天')}</option><option value="weekly">{t('每周固定日')}</option>
              <option value="monthly">{t('每月固定日')}</option><option value="cycle">{t('按周期滚动')}</option>
            </select>
          </Field>
          {f.resetPolicy === 'weekly' && (
            <Field label={t('每周几')} htmlFor="u-rday">
              <select id="u-rday" className="input" value={f.resetDay} onChange={(e) => set('resetDay', e.target.value)}>
                {[t('周一'), t('周二'), t('周三'), t('周四'), t('周五'), t('周六'), t('周日')].map((d, i) => <option key={i} value={String(i + 1)}>{d}</option>)}
              </select>
            </Field>
          )}
          {(f.resetPolicy === 'monthly' || f.resetPolicy === 'cycle') && (
            <Field label={f.resetPolicy === 'monthly' ? t('每月几号') : t('周期（天）')} htmlFor="u-rday" hint={f.resetPolicy === 'monthly' ? t('29–31 在小月按月末') : undefined}>
              <input id="u-rday" className="input mono" value={f.resetDay} onChange={(e) => set('resetDay', e.target.value)} inputMode="numeric" />
            </Field>
          )}
        </div>
      </Section>
      <Section step={3 + sp} title={t('有效期与续订')} summary={f.expireMode === 'first' ? t('首次使用后 {n} 天', { n: f.afterDays || '?' }) : f.expire ? t('{date} 到期', { date: f.expire }) : t('永不到期')}>
        <div className="form-grid">
          <Field label={t('有效期')} full>
            <div className="seg">
              <button type="button" className={'opt' + (f.expireMode === 'date' ? ' on' : '')} onClick={() => set('expireMode', 'date')}>{t('固定到期日')}<small>{t('留空为永不到期')}</small></button>
              <button type="button" className={'opt' + (f.expireMode === 'first' ? ' on' : '')} onClick={() => set('expireMode', 'first')}>{t('首次使用后开始计时')}<small>{t('产生第一笔流量时才开始算天数')}</small></button>
            </div>
          </Field>
          {f.expireMode === 'date' ? <>
            <Field label={t('到期日期')} htmlFor="u-exp" hint={t('留空为永不到期')}><input id="u-exp" className="input mono" type="date" value={f.expire} onChange={(e) => set('expire', e.target.value)} /></Field>
            <Field label={t('快捷续期')}>
              <div className="row">{[30, 90, 365].map((d) => <button key={d} type="button" className="btn sm" onClick={() => extend(d)}>{t('+{n} 天', { n: d })}</button>)}</div>
            </Field>
          </> : (
            <Field label={t('有效天数')} htmlFor="u-after" hint={t('首次连接后生效，到期日自动写入')}><input id="u-after" className="input mono" inputMode="numeric" value={f.afterDays} onChange={(e) => set('afterDays', e.target.value)} placeholder="30" /></Field>
          )}
          <Field label={t('到期自动续订（天）')} htmlFor="u-renew" hint={t('到期时自动顺延这么多天并清零流量；留空不续订')}>
            <input id="u-renew" className="input mono" value={f.renewDays} onChange={(e) => set('renewDays', e.target.value)} inputMode="numeric" placeholder={t('例如 30')} />
          </Field>
          <Field label={t('最多续订次数')} htmlFor="u-rmax" hint={user?.renewDays ? t('已续订 {n} 次；留空不限', { n: user.renewCount }) : t('留空不限')}>
            <input id="u-rmax" className="input mono" value={f.renewMax} disabled={!f.renewDays} onChange={(e) => set('renewMax', e.target.value)} inputMode="numeric" />
          </Field>
        </div>
      </Section>
      <Section step={4 + sp} title={t('节点授权')}>
        <Access groups={groups} nodeIds={f.nodeIds} groupIds={f.groupIds} inboundIds={f.inboundIds}
          onChange={(k, v) => set(k, v)} />
      </Section>
      <Section step={5 + sp} title={t('Telegram 与邮箱')} defaultOpen={!!user?.telegramId || !!user?.email}
        summary={[f.telegramId ? t('已绑定 {id}', { id: f.telegramId }) : t('未绑定'), f.email].filter(Boolean).join(' · ')}>
          <Field label="Telegram User ID" htmlFor="u-tg" full hint={user?.telegramId ? t('已绑定 · 清空即解绑；用户可在机器人中查询流量、到期并接收提醒') : t('纯数字，用户在机器人里发 /whoami 可查看；也可让用户发「/bind 订阅链接」自行绑定')}>
            <input id="u-tg" className="input mono" inputMode="numeric" value={f.telegramId} onChange={(e) => set('telegramId', e.target.value.replace(/[^\d]/g, ''))} placeholder={t('例如 123456789')} />
          </Field>
          <Field label={t('邮箱')} htmlFor="u-mail" full hint={t('配置 SMTP 后，提醒和订单开通通知也会发到这个邮箱；留空表示不发邮件')}>
            <input id="u-mail" className="input mono" type="email" autoComplete="off" value={f.email} onChange={(e) => set('email', e.target.value)} placeholder="user@example.com" />
          </Field>
      </Section>
      {user && <UserLinks userId={user.id} />}
      {user && <NodeQuotas userId={user.id} />}
      {user && <Devices userId={user.id} limit={user.deviceLimit} />}
      {user && <SubCheck userId={user.id} />}
      {user && <div className="field"><TrafficChart query={'user=' + user.id} title={t('流量统计')} /></div>}
      {user && <UserTrafficSources key={user.id} userId={user.id} />}
      {user && (
        <div className="kv">
          <dt>UUID</dt><dd className="mono">{user.uuid}</dd>
          <dt>{t('密码')}</dt><dd className="mono">{user.password}</dd>
          {(user.extraTraffic ?? 0) > 0 && <><dt>{t('流量包')}</dt><dd className="mono">+{bytes(user.extraTraffic!)} <span className="mute small">{t('（本周期有效，已计入流量配额）')}</span></dd></>}
          <dt>{t('已用')}</dt><dd className="mono">↑ {bytes(user.usedUp)} · ↓ {bytes(user.usedDown)} <TrafficEdit user={user} onSaved={onSaved} /></dd>
          <dt>{t('在线设备')}</dt>
          <dd className="small">
            {(user.onlineDevices ?? []).length === 0
              ? <span className="mute">{isOnline(user.onlineAt) ? t('在线，但没有拿到来源 IP（刚连上，或走的是不上报 IP 的入站，如端口转发）') : '—'}</span>
              : (user.onlineDevices ?? []).map((d) => (
                <div key={d.ip}><span className="mono">{d.ip}</span> · {d.nodes.join(', ') || '—'} · {ago(d.seen)}{(d.model || d.os) && <> · {[d.model, d.os].filter(Boolean).join(' ')}</>}</div>
              ))}
          </dd>
          <dt>{t('最近拉取订阅')}</dt><dd className="small">{user.lastSubAt ? `${datetime(user.lastSubAt)} · ${user.lastSubUa || '—'}` : '—'}</dd>
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
      <summary>{t('节点链接与二维码（{n}）', { n: links.length })}</summary>
      <div style={{ display: 'grid', gap: 6, marginTop: 8 }}>
        {links.length === 0 && <div className="sub">{t('没有可用节点（见下方「订阅诊断」）')}</div>}
        {links.map((l) => (
          <div key={l.uri} className="row" style={{ flexWrap: 'nowrap' }}>
            <span className="small" style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={l.name}>{l.name}</span>
            <Copy text={l.uri} />
            <button className="btn sm" type="button" onClick={() => setQr(qr === l.uri ? null : l.uri)}>{t('二维码')}</button>
          </div>
        ))}
        {qr && <div style={{ justifySelf: 'center' }}><QR text={qr} size={200} /></div>}
      </div>
    </details>
  )
}

// NodeQuotas: per-node traffic packs (node-only, spent before the account
// quota, one-off) and per-node caps (each traffic period).
function NodeQuotas({ userId }: { userId: string }) {
  const data = useLoad(() => get<NodeQuota[]>(`/users/${userId}/node-quotas`), [userId])
  const { nodes } = useNodes()
  const { busy, run } = useAction()
  const [f, setF] = useState({ nodeId: '', add: '', limit: '' })
  const qs = data.data ?? []
  const gb = (v: string) => Math.round((Number(v) || 0) * GB)
  const save = () => run(async () => {
    const body: Record<string, number> = {}
    if (f.add) body.addExtra = gb(f.add)
    if (f.limit !== '') body.limit = gb(f.limit)
    await put(`/users/${userId}/node-quotas/${f.nodeId}`, body)
    setF({ nodeId: '', add: '', limit: '' }); data.reload()
  }, t('已保存'))
  return (
    <details className="more" open={qs.length > 0}>
      <summary>{t('节点流量（{n}）', { n: qs.length })}</summary>
      <div style={{ display: 'grid', gap: 8, marginTop: 8 }}>
        <div className="hint">{t('流量包：只能在该节点使用，先于总流量扣除，总流量用完后仍可用，用完为止（重置流量不补回）。节点上限：每个流量周期在该节点最多用多少，超出后只停这个节点。按节点倍率换算。')}</div>
        {qs.map((q) => (
          <div key={q.nodeId} className="row" style={{ flexWrap: 'nowrap' }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="small" style={{ fontWeight: 500 }}>{q.nodeName || q.nodeId}</div>
              <div className="hint">
                {q.extra > 0 && <>{t('流量包 剩余 {left} / {total}', { left: bytes(Math.max(q.extra - q.extraUsed, 0)), total: bytes(q.extra) })}</>}
                {q.extra > 0 && q.limit > 0 && ' · '}
                {q.limit > 0 && <span style={q.used >= q.limit ? { color: 'var(--bad)' } : undefined}>{t('本期 {used} / {limit}', { used: bytes(q.used), limit: bytes(q.limit) })}{q.used >= q.limit ? t('（已停用该节点）') : ''}</span>}
                {!q.extra && !q.limit && t('未设置')}
              </div>
            </div>
            {q.limit > 0 && q.used > 0 && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await put(`/users/${userId}/node-quotas/${q.nodeId}`, { resetUsed: true }); data.reload() }, t('已清零'))}>{t('清零本期')}</button>}
            <button className="btn sm" type="button" onClick={() => setF({ nodeId: q.nodeId, add: '', limit: q.limit ? String(+(q.limit / GB).toFixed(2)) : '' })}>{t('编辑')}</button>
            <button className="btn sm danger" type="button" disabled={busy} onClick={() => run(async () => { await del(`/users/${userId}/node-quotas/${q.nodeId}`); data.reload() }, t('已移除'))}>{t('移除')}</button>
          </div>
        ))}
        <div className="form-grid">
          <Field label={t('节点')}>
            <select className="input" value={f.nodeId} onChange={(e) => setF({ ...f, nodeId: e.target.value })}>
              <option value="">{t('选择节点')}</option>
              {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}
            </select>
          </Field>
          <Field label={t('增加流量包（GB）')}><input className="input mono" inputMode="decimal" placeholder={t('不增加')} value={f.add} onChange={(e) => setF({ ...f, add: e.target.value.replace(/[^0-9.]/g, '') })} /></Field>
          <Field label={t('节点上限（GB / 周期）')}><input className="input mono" inputMode="decimal" placeholder={t('不变；0 为不限')} value={f.limit} onChange={(e) => setF({ ...f, limit: e.target.value.replace(/[^0-9.]/g, '') })} /></Field>
        </div>
        <div><button className="btn sm primary" type="button" disabled={busy || !f.nodeId || (!f.add && f.limit === '')} onClick={save}>{t('保存')}</button></div>
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
      <summary>{t('设备（{n}）', { n: limit ? `${ds.length} / ${limit}` : ds.length })}</summary>
      <div style={{ display: 'grid', gap: 6, marginTop: 8 }}>
        {ds.length === 0 && <div className="sub">{t('还没有客户端上报设备 ID')}</div>}
        {ds.map((d) => (
          <div key={d.id} className="row" style={{ flexWrap: 'nowrap' }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="small">{[d.model, d.os].filter(Boolean).join(' · ') || d.userAgent || t('未知设备')}</div>
              <div className="hint mono">{d.hwid.slice(0, 16)}… · {d.ip} · {ago(d.lastSeen)}</div>
            </div>
            <button className="btn sm danger" type="button" onClick={() => run(async () => { await del(`/users/${userId}/devices/${d.id}`); data.reload() }, t('已移除'))}>{t('移除')}</button>
          </div>
        ))}
        {ds.length > 1 && <div><button className="btn sm" type="button" onClick={() => run(async () => { await del(`/users/${userId}/devices/all`); data.reload() }, t('已清空'))}>{t('清空全部设备')}</button></div>}
      </div>
    </details>
  )
}

function GroupsModal({ onClose, onChanged }: { onClose: () => void; onChanged: () => void }) {
  const speedOK = useFeature('speed_limit')
  const groups = useLoad(() => get<Group[]>('/groups'))
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const [edit, setEdit] = useState<{ id?: string; name: string; inboundIds: string[]; nodeIds: string[]; level: number; speedUpMbps: number; speedDownMbps: number } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const { nodes } = useNodes()
  const tiered = (lvl: number) => nodes.filter((n) => (n.level ?? 0) > 0 && (n.level ?? 0) <= lvl)
  const byNode = useMemo(() => {
    const m = new Map<string, Inbound[]>()
    for (const i of inbounds.data ?? []) m.set(i.nodeName, [...(m.get(i.nodeName) ?? []), i])
    return [...m.entries()]
  }, [inbounds.data])
  return (
    <Modal title={t('分组')} onClose={onClose} wide>
      {!edit ? (
        <>
          <div className="card flush">
            <table>
              <thead><tr><th>{t('分组')}</th><th>{t('等级')}</th><th>{t('限速')}</th><th>{t('用户')}</th><th>{t('节点')}</th><th>{t('入站')}</th><th></th></tr></thead>
              <tbody>
                {(groups.data ?? []).filter((g) => !g.plan).map((g) => (
                  <tr key={g.id}>
                    <td style={{ fontWeight: 500 }}>{g.name}{g.isDefault && <span className="tag info-tag" style={{ marginLeft: 6 }}>{t('默认')}</span>}</td><td>{g.level > 0 ? <span className="tag info-tag">Lv.{g.level}</span> : <span className="mute">—</span>}</td><td className="mono small">{g.speedUpMbps || g.speedDownMbps ? `↑${g.speedUpMbps || '∞'} ↓${g.speedDownMbps || '∞'}` : <span className="mute">—</span>}</td><td className="mono">{g.userCount}</td>
                    <td className="mono" title={[...new Set([...tiered(g.level), ...nodes.filter((n) => g.nodeIds?.includes(n.id))].map((n) => n.name))].join('、')}>{new Set([...tiered(g.level).map((n) => n.id), ...(g.nodeIds ?? [])]).size}</td><td className="mono">{g.inboundIds.length}</td>
                    <td style={{ textAlign: 'right' }}>
                      <button className="link" type="button" onClick={() => setEdit({ id: g.id, name: g.name, inboundIds: g.inboundIds, nodeIds: g.nodeIds ?? [], level: g.level ?? 0, speedUpMbps: g.speedUpMbps ?? 0, speedDownMbps: g.speedDownMbps ?? 0 })}>{t('编辑')}</button>{'  '}
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(t('删除分组 {name}？组内用户将失去对应入站。', { name: g.name }), { danger: true })) run(async () => { await del('/groups/' + g.id); groups.reload(); onChanged() })
                      }}>{t('删除')}</button>
                    </td>
                  </tr>
                ))}
                {groups.data?.filter((g) => !g.plan).length === 0 && <tr><td colSpan={7} className="mute">{t('还没有分组')}</td></tr>}
              </tbody>
            </table>
          </div>
          <div><button className="btn pri" type="button" onClick={() => setEdit({ name: '', inboundIds: [], nodeIds: [], level: 0, speedUpMbps: 0, speedDownMbps: 0 })}>{t('+ 新建分组')}</button></div>
        </>
      ) : (
        <>
          <div className="form-grid">
            <Field label={t('名称')} htmlFor="g-name"><input id="g-name" className="input" value={edit.name} onChange={(e) => setEdit({ ...edit, name: e.target.value })} /></Field>
            <Field label={t('等级')} htmlFor="g-level" hint={t('组内用户可用等级 1 到该等级的全部节点（在节点设置里给节点定等级）；0 表示不按等级')}>
              <input id="g-level" className="input mono" type="number" min={0} max={99} value={edit.level} onChange={(e) => setEdit({ ...edit, level: Math.max(0, Math.min(99, Number(e.target.value) || 0)) })} />
            </Field>
            <Field label={<>{t('限速套餐 上行 / 下行 (Mbps)')}<ProTag on={speedOK} /></>} hint={t('组内没有单独设置限速的用户按此限速（属于多个分组取最高）；0 为不限')}>
              <div className="row" style={{ flexWrap: 'nowrap' }}>
                <input aria-label={t('上行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedUpMbps || ''} placeholder={t('上行')} onChange={(e) => setEdit({ ...edit, speedUpMbps: Math.max(0, Number(e.target.value) || 0) })} />
                <input aria-label={t('下行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedDownMbps || ''} placeholder={t('下行')} onChange={(e) => setEdit({ ...edit, speedDownMbps: Math.max(0, Number(e.target.value) || 0) })} />
              </div>
            </Field>
          </div>
          {edit.level > 0 && <div className="note">{t('按等级可用：')}{tiered(edit.level).map((n) => `${n.name} (Lv.${n.level})`).join(', ') || t('还没有等级不高于 {n} 的节点', { n: edit.level })}</div>}
          <Field label={t('授权整个节点')} hint={t('不论等级，组内用户可用这些节点上的全部入站（包括以后新增的入站）')}>
            <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
              {nodes.map((n) => {
                const on = edit.nodeIds.includes(n.id)
                return <button key={n.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
                  onClick={() => setEdit({ ...edit, nodeIds: on ? edit.nodeIds.filter((x) => x !== n.id) : [...edit.nodeIds, n.id] })}>{n.name}{(n.level ?? 0) > 0 && <span className="mute small" style={{ marginLeft: 4 }}>Lv.{n.level}</span>}</button>
              })}
              {nodes.length === 0 && <span className="hint">{t('还没有节点')}</span>}
            </div>
          </Field>
          <Field label={t('授权入站')} hint={t('只授权单个入站时使用')}>
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
              {byNode.length === 0 && <span className="hint">{t('还没有入站')}</span>}
            </div>
          </Field>
          <div className="dfoot">
            <button className="btn" type="button" onClick={() => setEdit(null)}>{t('返回')}</button>
            <button className="btn pri" type="button" disabled={busy || !edit.name} onClick={() => run(async () => {
              if (edit.id) await put('/groups/' + edit.id, edit)
              else await post('/groups', edit)
              setEdit(null)
              groups.reload()
              onChanged()
            }, t('已保存'))}>{t('保存')}</button>
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
                  <tr key={u.id} className="clickable" onClick={() => setSelected(u)}>
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
                    <td data-label={t('最近在线')} className="small">{isOnline(u.onlineAt) ? <span style={{ color: 'var(--ok)' }}>{t('在线')}</span> : u.onlineAt ? ago(u.onlineAt) : <span className="mute">{t('从未')}</span>}</td>
                    <td data-label={t('在线 IP')} className="mono">{isOnline(u.onlineAt) && u.onlineIps.length === 0
                      ? <span className="mute" title={t('在线，但没有拿到来源 IP（刚连上，或走的是不上报 IP 的入站，如端口转发）')}>—</span>
                      : u.onlineIps.length} / {u.ipLimit || '∞'}</td>
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
