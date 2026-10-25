import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { useNodes } from '../App'
import { t } from '../i18n'
import { Field, Switch, useAction, useLoad } from './ui'

export { RouteASNSettings } from './RouteASNSettings'

type Window = { id: string; nodeIds: string[]; channels: string[]; startsAt: string; endsAt: string }
type Policy = { repeatWindowSeconds: number; channels: Record<string, Record<string, boolean>>; maintenance: Window[] }
type PolicyInfo = { policy: Policy; channels: string[]; events: Record<string, { paired: boolean; recovery: boolean; defaultOff?: boolean }> }
const channelName = (c: string) => ({ telegram: 'Telegram', email: t('邮件'), webhook: 'Webhook' }[c] ?? c)
const eventName = (e: string) => ({ node_offline: t('节点离线'), node_online: t('节点恢复'), inbound_down: t('入站不可达'), inbound_up: t('入站恢复'), plugin_offline: t('插件离线'), plugin_online: t('插件恢复'), health: t('均衡落地健康变化'), apply_failed: t('配置下发失败'), backup_failed: t('备份失败'), cert_failed: t('证书续期失败'), node_traffic: t('服务器流量告警'), operational_alert: t('运行健康异常'), route_changed: t('回程路径变化') }[e] ?? e)
const localDate = (v: string) => { const d = new Date(v); return Number.isNaN(d.getTime()) ? '' : new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16) }
const toggleItem = (xs: string[], id: string) => xs.includes(id) ? xs.filter((x) => x !== id) : [...xs, id]

export function NotificationSettings() {
  const info = useLoad(() => get<PolicyInfo>('/notifications/policy'))
  const { nodes } = useNodes()
  const [f, setF] = useState<Policy>()
  const { busy, run } = useAction()
  useEffect(() => { if (info.data) setF({ ...info.data.policy, channels: info.data.policy.channels ?? {}, maintenance: (info.data.policy.maintenance ?? []).map((w) => ({ ...w, nodeIds: w.nodeIds ?? [], channels: w.channels ?? [] })) }) }, [info.data])
  const changeWindow = (i: number, patch: Partial<Window>) => setF((old) => old && ({ ...old, maintenance: old.maintenance.map((x, j) => i === j ? { ...x, ...patch } : x) }))
  return <section className="card ops-stack">
    <div className="ctitle"><h2>{t('运维通知策略')}</h2><button className="btn sm" disabled={busy} onClick={info.reload}>{t('重新加载')}</button></div>
    {info.error && <div className="note warn" role="alert">{info.error}</div>}
    <div className="note">{t('事件开关仍受渠道总开关约束。此处不静默登录安全、客户和交易通知；回程变化默认关闭。')}</div>
    {f && info.data && <>
      <Field label={t('重复告警合并窗口（秒）')} hint={t('0–86400；设为 0 关闭时间窗口合并，故障与恢复配对仍生效。')}><input className="input mono" aria-label={t('重复告警合并窗口（秒）')} type="number" min={0} max={86400} value={f.repeatWindowSeconds} onChange={(e) => setF({ ...f, repeatWindowSeconds: Number(e.target.value) })} /></Field>
      <div className="scroll"><table><thead><tr><th>{t('事件')}</th>{info.data.channels.map((c) => <th key={c}>{channelName(c)}</th>)}</tr></thead><tbody>{Object.keys(info.data.events).map((event) => <tr key={event}><td>{eventName(event)}{info.data!.events[event].defaultOff && <div className="hint">{t('默认关闭')}</div>}</td>{info.data!.channels.map((channel) => <td key={channel}><Switch label={`${eventName(event)} · ${channelName(channel)}`} on={f.channels[channel]?.[event] ?? !info.data!.events[event].defaultOff} onChange={(v) => setF({ ...f, channels: { ...f.channels, [channel]: { ...f.channels[channel], [event]: v } } })} /></td>)}</tr>)}</tbody></table></div>
      <div className="ctitle"><h3>{t('维护静默窗口')}</h3><button className="btn sm" disabled={f.maintenance.length >= 20} onClick={() => { const start = new Date(), end = new Date(start.getTime() + 3600000); setF({ ...f, maintenance: [...f.maintenance, { id: crypto.randomUUID(), nodeIds: [], channels: [], startsAt: start.toISOString(), endsAt: end.toISOString() }] }) }}>{t('添加维护窗口')}</button></div>
      <div className="hint">{t('时间按当前浏览器时区填写，保存为绝对时间；每个窗口最多 7 天。节点或渠道未选择表示全部。静默消息不会在结束后补发。')}</div>
      {f.maintenance.map((w, i) => <div className="ops-summary ops-stack" key={w.id}>
        <div className="row"><b>{t('维护窗口 {n}', { n: i + 1 })}</b><button className="link" onClick={() => setF({ ...f, maintenance: f.maintenance.filter((_, j) => i !== j) })}>{t('删除')}</button></div>
        <div className="form-grid"><Field label={t('开始时间')}><input className="input" type="datetime-local" aria-label={t('开始时间')} value={localDate(w.startsAt)} onChange={(e) => changeWindow(i, { startsAt: e.target.value ? new Date(e.target.value).toISOString() : '' })} /></Field><Field label={t('结束时间')}><input className="input" type="datetime-local" aria-label={t('结束时间')} value={localDate(w.endsAt)} onChange={(e) => changeWindow(i, { endsAt: e.target.value ? new Date(e.target.value).toISOString() : '' })} /></Field></div>
        <Field label={t('静默渠道')}><div className="row">{info.data!.channels.map((c) => <label className="chk" key={c}><input type="checkbox" checked={w.channels.includes(c)} onChange={() => changeWindow(i, { channels: toggleItem(w.channels, c) })} />{channelName(c)}</label>)}</div></Field>
        <Field label={t('静默节点')}><div className="ops-choices">{nodes.map((n) => <label className="chk" key={n.id}><input type="checkbox" checked={w.nodeIds.includes(n.id)} onChange={() => changeWindow(i, { nodeIds: toggleItem(w.nodeIds, n.id) })} />{n.name}</label>)}</div>{w.nodeIds.filter((id) => !nodes.some((n) => n.id === id)).map((id) => <button className="link" key={id} onClick={() => changeWindow(i, { nodeIds: w.nodeIds.filter((x) => x !== id) })}>{t('移除已不存在节点')} · {id}</button>)}</Field>
      </div>)}
      <button className="btn pri" disabled={busy || f.repeatWindowSeconds < 0 || f.repeatWindowSeconds > 86400} onClick={() => run(async () => { for (const w of f.maintenance) { const from = Date.parse(w.startsAt), to = Date.parse(w.endsAt); if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > 7 * 86400000) throw new Error(t('维护窗口结束时间须晚于开始时间，且不能超过 7 天')) } await put('/notifications/policy', f); info.reload() }, t('已保存'))}>{t('保存通知策略')}</button>
    </>}
  </section>
}
