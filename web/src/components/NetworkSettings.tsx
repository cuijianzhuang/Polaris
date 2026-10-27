import { useEffect, useState } from 'react'
import { SettingsSaveBar, useSettingsDraft, useSettingsLeave, useSettingsSave } from './SettingsDrafts'
import { get, put } from '../api'
import { useNodes } from '../App'
import { t } from '../i18n'
import { Field, Switch, useConfirm, useLoad } from './ui'
import { applyNotificationProfile, notificationGroupMode, notificationGroups, notificationProfile, selectedChannels, setNotificationChannel, setNotificationGroup } from '../notificationPolicy'
import type { EventCatalog, GroupMode, MaintenanceWindow, NotificationPolicy } from '../notificationPolicy'

import { RouteASNSettings } from './RouteASNSettings'
import RouteGeoSettings from './RouteGeoSettings'
import UserNotices from './UserNotices'

export function NetworkDatabaseSettings() {
  const [tab, setTab] = useState<'asn' | 'geo'>('asn')
  const leave = useSettingsLeave()
  const go = async (next: 'asn' | 'geo') => { if (next !== tab && await leave()) setTab(next) }
  return <div className="ops-stack">
    <nav className="tabs" aria-label={t('网络归属数据库')}>
      <button className={'tab' + (tab === 'asn' ? ' on' : '')} type="button" aria-pressed={tab === 'asn'} onClick={() => go('asn')}>{t('ASN 归属')}</button>
      <button className={'tab' + (tab === 'geo' ? ' on' : '')} type="button" aria-pressed={tab === 'geo'} onClick={() => go('geo')}>{t('地理归属')}</button>
    </nav>
    {tab === 'asn' ? <RouteASNSettings /> : <RouteGeoSettings />}
  </div>
}

type PolicyInfo = { policy: NotificationPolicy; channels: string[]; events: EventCatalog }
const channelName = (c: string) => ({ telegram: 'Telegram', email: t('邮件'), webhook: 'Webhook' }[c] ?? c)
const eventName = (e: string) => ({ node_offline: t('节点离线'), node_online: t('节点恢复'), inbound_down: t('入站不可达'), inbound_up: t('入站恢复'), plugin_offline: t('插件离线'), plugin_online: t('插件恢复'), health: t('均衡落地健康变化'), apply_failed: t('配置下发失败'), backup_failed: t('备份失败'), cert_failed: t('证书续期失败'), node_traffic: t('服务器流量告警'), operational_alert: t('运行健康异常'), route_changed: t('回程路径变化') }[e] ?? e)
const groupName = (id: string, members: string[]) => ({ node_availability: t('节点状态'), inbound_availability: t('入站状态'), plugin_availability: t('插件状态') }[id] ?? eventName(members[0]))
const localDate = (v: string) => { const d = new Date(v); return Number.isNaN(d.getTime()) ? '' : new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16) }
const absoluteDate = (v: string) => { const d = new Date(v); return v && Number.isFinite(d.getTime()) ? d.toISOString() : '' }
const toggleItem = (xs: string[], id: string) => xs.includes(id) ? xs.filter((x) => x !== id) : [...xs, id]
const normalizePolicy = (p: NotificationPolicy): NotificationPolicy => ({ ...p, channels: Object.fromEntries(Object.entries(p.channels ?? {}).map(([channel, rules]) => [channel, { ...rules }])), maintenance: (p.maintenance ?? []).map((w) => ({ ...w, nodeIds: w.nodeIds ?? [], channels: w.channels ?? [] })) })

export function NotificationSettings() {
	const [showNotices, setShowNotices] = useState(false)
  const info = useLoad(() => get<PolicyInfo>('/notifications/policy'))
  const { nodes } = useNodes()
  const [f, setF] = useState<NotificationPolicy>()
  const [baseline, setBaseline] = useState('')
  const [repeat, setRepeat] = useState('300')
  const [custom, setCustom] = useState(false)
  const saver = useSettingsSave()
  const { busy } = saver
  const confirm = useConfirm()
  const accept = (data: PolicyInfo) => {
    const policy = normalizePolicy(data.policy)
    setF(policy); setBaseline(JSON.stringify(policy)); setRepeat(String(policy.repeatWindowSeconds))
  }
  useEffect(() => { if (info.data) { accept(info.data); setCustom(false) } }, [info.data])
  const dirty = !!f && (JSON.stringify(f) !== baseline || repeat !== String(f.repeatWindowSeconds))
  const invalidRepeat = repeat.trim() === '' || !Number.isInteger(Number(repeat)) || Number(repeat) < 0 || Number(repeat) > 86400
  const changeWindow = (i: number, patch: Partial<MaintenanceWindow>) => setF((old) => old && ({ ...old, maintenance: old.maintenance.map((x, j) => i === j ? { ...x, ...patch } : x) }))
  const save = async () => {
    if (!dirty) return true
    if (!f || invalidRepeat || busy || info.loading) return false
    return saver.run(async () => {
      for (const w of f.maintenance) {
        const from = Date.parse(w.startsAt), to = Date.parse(w.endsAt)
        if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > 7 * 86400000) throw new Error(t('维护窗口结束时间须晚于开始时间，且不能超过 7 天'))
      }
      accept(await put<PolicyInfo>('/notifications/policy', f))
    }, t('已保存'))
  }
  useSettingsDraft({ label: t('通知策略'), dirty, busy, invalid: invalidRepeat, save, discard: () => { if (info.data) accept(info.data) } })
  const reload = async () => {
    if (dirty && !await confirm(t('重新加载会丢弃尚未保存的通知设置，继续吗？'))) return
    info.reload()
  }
  const channels = f && info.data ? selectedChannels(f, info.data.events, info.data.channels) : []
  const mode = f && info.data ? notificationProfile(f, info.data.events, info.data.channels) : 'custom'
  return <section className="card ops-stack">
    <div className="ctitle"><h2>{t('运维通知策略')}</h2><div className="row"><button type="button" className="btn sm" disabled={busy || info.loading} onClick={reload}>{t('重新加载')}</button><button type="button" className="btn pri sm" disabled={busy || info.loading || !dirty || invalidRepeat} onClick={save}>{t('保存通知策略')}</button></div></div>
    {info.error && <div className="note warn" role="alert">{info.error}</div>}
    <div className="hint">{t('先选择接收渠道和通知范围；只有需要单独调整事件时才展开自定义。登录安全、客户和交易通知仍按原有设置发送。')}</div>
    {f && info.data && <fieldset className="notification-form ops-stack" disabled={busy || info.loading}>
      <Field label={t('接收渠道')} hint={t('这里只控制运维通知；各渠道仍需完成配置并启用管理员通知。')}><div className="row">{info.data.channels.map((channel) => <label className="chk" key={channel}><input type="checkbox" checked={channels.includes(channel)} onChange={(e) => setF(setNotificationChannel(f, info.data!.events, info.data!.channels, channel, e.target.checked))} />{channelName(channel)}</label>)}</div></Field>
      {!channels.length && <div className="note warn">{t('未选择接收渠道，保存后将暂停全部运维通知。')}</div>}
      <Field label={t('通知范围')}>
        <div className="ops-choices">{([['essential', t('重要故障')], ['standard', t('常规运维')], ['custom', t('自定义')]] as const).map(([value, label]) => <label className="chk" key={value}><input type="radio" name="notification-profile" value={value} checked={(custom ? 'custom' : mode) === value} onClick={() => { if (value === 'custom') setCustom(true) }} onChange={() => { setCustom(value === 'custom'); if (value !== 'custom') setF(applyNotificationProfile(f, info.data!.events, info.data!.channels, value)) }} />{label}</label>)}</div>
        <div className="hint">{custom || mode === 'custom' ? t('保留现有细分规则；修改某一项不会重置其他事件。') : mode === 'essential' ? t('通知节点和入站故障、配置下发、备份、证书及运行健康异常。') : t('覆盖日常运维事件；回程路径变化保持默认关闭。选择预设会统一各渠道的细分规则。')}</div>
      </Field>
      <div className="toggle"><div><b>{t('允许故障恢复通知')}</b><div className="hint">{t('仅对已通知的故障发送恢复消息；关闭后仍跟踪恢复状态，避免漏掉下一次故障。')}</div></div><Switch label={t('允许故障恢复通知')} on={f.notifyRecovery !== false} onChange={(value) => setF({ ...f, notifyRecovery: value })} /></div>
      <details open={custom} onToggle={(e) => { if (e.currentTarget.open !== custom) setCustom(e.currentTarget.open) }}>
        <summary>{t('按事件自定义')}</summary>
        <div className="ops-stack notification-details"><div className="hint">{t('故障与恢复按组设置；需要不同渠道时展开该组。关闭全局恢复通知后，这里的恢复选项也不会发送。')}</div>
          {notificationGroups(info.data.events).map((group) => {
            const paired = group.events.some((event) => info.data!.events[event].recovery)
            const current = notificationGroupMode(f, info.data!.events, channels, group.events)
            const options = <>{current === 'custom' && <option value="custom">{t('自定义规则')}</option>}<option value="off">{t('关闭')}</option>{paired && <option value="fault">{t('仅故障')}</option>}<option value="all">{paired ? t('故障与恢复') : t('通知')}</option></>
            return <div className="ops-summary ops-stack" key={group.id}>
              <div className="row"><b className="notification-group-name">{groupName(group.id, group.events)}</b><select className="input notification-mode" aria-label={groupName(group.id, group.events)} disabled={!channels.length} value={current} onChange={(e) => { if (e.target.value !== 'custom') setF(setNotificationGroup(f, info.data!.events, channels, group.events, e.target.value as Exclude<GroupMode, 'custom'>)) }}>{options}</select>{group.events.every((event) => info.data!.events[event].defaultOff) && <span className="tag">{t('默认关闭')}</span>}</div>
              <details><summary>{t('细分渠道')}</summary><div className="ops-choices notification-details">{info.data!.channels.map((channel) => {
                const value = notificationGroupMode(f, info.data!.events, [channel], group.events)
                return <Field key={channel} label={channelName(channel)} hint={!channels.includes(channel) ? t('接收渠道未启用，规则暂不发送') : undefined}><select className="input" aria-label={`${groupName(group.id, group.events)} · ${channelName(channel)}`} value={value} onChange={(e) => { if (e.target.value !== 'custom') setF(setNotificationGroup(f, info.data!.events, [channel], group.events, e.target.value as Exclude<GroupMode, 'custom'>)) }}>{value === 'custom' && <option value="custom">{t('自定义规则')}</option>}<option value="off">{t('关闭')}</option>{paired && <option value="fault">{t('仅故障')}</option>}<option value="all">{paired ? t('故障与恢复') : t('通知')}</option></select></Field>
              })}</div></details>
            </div>
          })}
        </div>
      </details>
      <details><summary>{t('告警合并与维护静默')}</summary><div className="ops-stack notification-details">
        <Field label={t('重复告警合并窗口（秒）')} hint={t('0–86400；设为 0 关闭时间窗口合并，故障与恢复配对仍生效。')}><input className="input mono" aria-label={t('重复告警合并窗口（秒）')} type="number" min={0} max={86400} step={1} value={repeat} aria-invalid={invalidRepeat} onChange={(e) => { const value = e.target.value; setRepeat(value); if (value.trim() && Number.isInteger(Number(value))) setF({ ...f, repeatWindowSeconds: Number(value) }) }} /></Field>
        <div className="ctitle"><h3>{t('维护静默窗口')} · {f.maintenance.length}</h3><button type="button" className="btn sm" disabled={f.maintenance.length >= 20} onClick={() => { const start = new Date(), end = new Date(start.getTime() + 3600000); setF({ ...f, maintenance: [...f.maintenance, { id: crypto.randomUUID(), nodeIds: [], channels: [], startsAt: start.toISOString(), endsAt: end.toISOString() }] }) }}>{t('添加维护窗口')}</button></div>
        <div className="hint">{t('时间按当前浏览器时区填写，保存为绝对时间；每个窗口最多 7 天。节点或渠道未选择表示全部。静默消息不会在结束后补发。')}</div>
        {f.maintenance.map((w, i) => <div className="ops-summary ops-stack" key={w.id}>
          <div className="row"><b>{t('维护窗口 {n}', { n: i + 1 })}</b><button type="button" className="link" onClick={() => setF({ ...f, maintenance: f.maintenance.filter((_, j) => i !== j) })}>{t('删除')}</button></div>
          <div className="form-grid"><Field label={t('开始时间')}><input className="input" type="datetime-local" aria-label={t('开始时间')} value={localDate(w.startsAt)} onChange={(e) => changeWindow(i, { startsAt: absoluteDate(e.target.value) })} /></Field><Field label={t('结束时间')}><input className="input" type="datetime-local" aria-label={t('结束时间')} value={localDate(w.endsAt)} onChange={(e) => changeWindow(i, { endsAt: absoluteDate(e.target.value) })} /></Field></div>
          <Field label={t('静默渠道')}><div className="row">{info.data!.channels.map((c) => <label className="chk" key={c}><input type="checkbox" checked={w.channels.includes(c)} onChange={() => changeWindow(i, { channels: toggleItem(w.channels, c) })} />{channelName(c)}</label>)}</div></Field>
          <Field label={t('静默节点')}><div className="ops-choices">{nodes.map((n) => <label className="chk" key={n.id}><input type="checkbox" checked={w.nodeIds.includes(n.id)} onChange={() => changeWindow(i, { nodeIds: toggleItem(w.nodeIds, n.id) })} />{n.name}</label>)}</div>{w.nodeIds.filter((id) => !nodes.some((n) => n.id === id)).map((id) => <button type="button" className="link" key={id} onClick={() => changeWindow(i, { nodeIds: w.nodeIds.filter((x) => x !== id) })}>{t('移除已不存在节点')} · {id}</button>)}</Field>
        </div>)}
      </div></details>
      <SettingsSaveBar loading={info.loading} dirty={dirty} busy={busy} error={saver.error} disabled={invalidRepeat} onSave={save} label={t('保存通知策略')} />
    </fieldset>}
    <details onToggle={(e) => setShowNotices(e.currentTarget.open)}><summary>{t('用户提醒投递记录')}</summary>{showNotices && <UserNotices />}</details>
  </section>
}
