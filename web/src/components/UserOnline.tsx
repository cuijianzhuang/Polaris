import { get } from '../api'
import { ago, datetime, isOnline } from '../format'
import { t } from '../i18n'
import type { User } from '../types'
import { Copy, useLoad, usePoll } from './ui'

const state = (u: User) => u.onlineIpState ?? ((u.onlineIps ?? []).length ? 'available' : isOnline(u.onlineAt) ? 'unavailable' : 'none')
export const hasOnlineIPs = (u: User) => state(u) === 'available' || state(u) === 'partial'
export function UserActivity({ user }: { user: User }) {
  return hasOnlineIPs(user) ? <span style={{ color: 'var(--ok)' }}>{t('在线')}</span> : isOnline(user.onlineAt) ? <span style={{ color: 'var(--warn-ink)' }}>{t('近期活跃')}</span> : user.onlineAt ? <>{ago(user.onlineAt)}</> : <span className="mute">{t('从未')}</span>
}
function explanation(u: User) {
  switch (state(u)) {
    case 'available': return t('已收到有效来源 IP。计数已去重，受信任地址及 CDN 边缘地址不计入限制。')
    case 'partial': return t('部分连接仅上报 CDN 边缘地址，显示的是已识别且计入限制的 IP 数量下限。')
    case 'expired': return t('有近期活动，但来源 IP 报告已过期；旧地址不作为当前在线 IP。')
    case 'unavailable': return (u.onlineDevices ?? []).some((d) => d.excluded === 'cdn') ? t('仅收到 CDN 边缘地址，尚未识别客户端真实 IP。') : t('有近期活动，但未收到有效来源 IP。长连接或不支持来源上报的内核可能出现此情况。')
    default: return t('当前没有有效来源 IP 报告，也没有近两分钟的活动记录。')
  }
}
export function UserIPCount({ user, onClick }: { user: User; onClick: () => void }) {
  const s = state(user), count = user.onlineIpCount ?? (user.onlineIps ?? []).length
  const label = s === 'available' ? count : s === 'partial' ? `≥${count}` : s === 'expired' ? t('已过期') : s === 'unavailable' ? t('未识别') : 0
  const exempt = (user.onlineDevices ?? []).filter((d) => d.excluded === 'trusted').length
  return <div><button className="link mono online-ip-count" type="button" title={explanation(user)} aria-label={t('查看 {name} 的在线 IP', { name: user.username })} onClick={onClick} data-ip-state={s}>{label} / {user.ipLimit || '∞'}</button>{exempt > 0 && <div className="small mute">{t('{n} 个免计', { n: exempt })}</div>}</div>
}

// Poll only the read-only online view. Never replace an open editor's form.
export function UserOnlineDetails({ user }: { user: User }) {
  const data = useLoad(() => get<User>(`/users/${user.id}`), [user.id])
  usePoll(data.reload, 10000)
  const current = data.data ?? user
  const devices = current.onlineDevices ?? []
  return <div className="online-user-details">
    <div className="note small" role="status">{explanation(current)}</div>
    {data.error && <div className="note warn small" role="alert">{t('在线 IP 刷新失败，显示最近一次结果')} · {data.error}<button className="link" type="button" onClick={data.reload}>{t('重试')}</button></div>}
    {current.onlineIpSeenAt && <div className="small mute">{t('最近 IP 上报')}：{datetime(current.onlineIpSeenAt)}</div>}
    {devices.map((d) => <div className="online-device-row" key={d.ip}>
      <div className="row"><span className="mono online-device-ip">{d.ip}</span><Copy text={d.ip} />{d.excluded && <span className="tag">{d.excluded === 'cdn' ? t('CDN 边缘，不计入') : t('受信任地址，不计入')}</span>}</div>
      <div className="small mute">{d.nodes?.join(' · ') || t('节点未知')} · {ago(d.seen)}{(d.model || d.os) && ' · ' + [d.model, d.os].filter(Boolean).join(' ')}</div>
    </div>)}
    <div className="hint">{t('IP 数量不等于设备数量；同一 IP 跨节点只计一次。')}</div>
  </div>
}
