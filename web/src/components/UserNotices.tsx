import { get } from '../api'
import { datetime } from '../format'
import { t } from '../i18n'
import { useLoad, usePoll } from './ui'

type Notice = { id: string; username: string; kind: string; channel: string; tier: number; status: string; attempts: number; lastError?: string; updatedAt: string }

export default function UserNotices() {
  const data = useLoad(() => get<{ items: Notice[] }>('/user-notices'))
  usePoll(data.reload, 15000)
  const status: Record<string, string> = { pending: t('等待发送或重试'), sent: t('已发送'), canceled: t('已失效') }
  return <div className="ops-stack">
    <div className="row"><b>{t('用户提醒投递记录')}</b><button className="btn sm" onClick={data.reload}>{t('刷新')}</button></div>
    <div className="hint">{t('按渠道分别重试；第三方响应丢失时可能重复发送。续费、重置或关闭渠道后，过期提醒会取消。')}</div>
    {data.error && <div className="note warn">{data.error}</div>}
    <div className="scroll"><table><thead><tr><th>{t('用户')}</th><th>{t('提醒')}</th><th>{t('渠道')}</th><th>{t('状态')}</th><th>{t('尝试次数')}</th><th>{t('时间')}</th></tr></thead><tbody>
      {(data.data?.items ?? []).map((n) => <tr key={n.id}><td>{n.username || '—'}</td><td>{n.kind === 'expire_warn' ? t('到期提醒') : t('流量提醒')} · {n.kind === 'expire_warn' ? `${n.tier} ${t('天')}` : `${n.tier}%`}</td><td>{n.channel === 'email' ? t('邮件') : 'Telegram'}</td><td title={n.lastError}>{status[n.status] ?? n.status}{n.lastError && <div className="small mute">{n.lastError}</div>}</td><td>{n.attempts}</td><td>{datetime(n.updatedAt)}</td></tr>)}
    </tbody></table></div>
  </div>
}
