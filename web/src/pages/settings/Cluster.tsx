import { useEffect } from 'react'
import { get } from '../../api'
import { useLoad } from '../../components/ui'
import { datetime } from '../../format'
import { t } from '../../i18n'

export type ClusterInfo = { operations: {id: string; kind: string; target: string; createdAt: string}[]; deliveries: {operationId: string; memberId: string; status: string; lastError: string}[]; shared: boolean; members: { id: string; host: string; version: string; edition: string; startedAt: string; seenAt: string; online: boolean; leader: boolean; self: boolean }[] }

// Cluster: the panel instances sharing this database (shown only when there
// is more than one). The leader runs the scheduled jobs; "update" updates
// every running instance.
export function Cluster() {
  const info = useLoad(() => get<ClusterInfo>('/cluster'))
  useEffect(() => {
    const id = setInterval(() => info.reload(), 15000)
    return () => clearInterval(id)
  }, [])
  const ms = info.data?.members ?? []
  const ops = info.data?.operations ?? []
  const deliveries = info.data?.deliveries ?? []
  if (ms.length < 2 && ops.length === 0) return null
  const running = ms.filter((m) => m.online)
  const versions = new Set(running.map((m) => m.version))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('集群')}</h2>
        <span className="sub" style={{ margin: 0 }}>{t('{n} 个主控实例运行中', { n: running.length })}</span>
      </div>
      {versions.size > 1 && <div className="note warn">{t('各实例版本不一致，请等待升级完成或检查升级失败的实例。')}</div>}
      <div className="card flush scroll">
        <table className="rtable">
          <thead><tr><th>{t('主机')}</th><th>{t('版本')}</th><th>{t('启动时间')}</th><th>{t('最后在线')}</th></tr></thead>
          <tbody>{ms.map((m) => (
            <tr key={m.id}>
              <td className="rt-head mono">{m.host || m.id.slice(0, 8)}
                {m.leader && <span className="tag ok" style={{ marginLeft: 6 }}>{t('主实例')}</span>}
                {m.self && <span className="tag" style={{ marginLeft: 6 }}>{t('当前')}</span>}
                {!m.online && <span className="tag warn" style={{ marginLeft: 6 }}>{t('离线')}</span>}</td>
              <td data-label={t('版本')} className="mono">{m.version}</td>
              <td data-label={t('启动时间')}>{datetime(m.startedAt)}</td>
              <td data-label={t('最后在线')}>{datetime(m.seenAt)}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
      {ops.slice(0, 5).map((op) => {
        const ds = deliveries.filter((d) => d.operationId === op.id)
        return <div key={op.id} className="hint">{op.kind === 'update' ? t('升级面板') : t('恢复后重启')} {op.target} · {t('已受理 {n}/{m}', { n: ds.filter((d) => d.status === 'accepted').length, m: ds.length })}
          {ds.filter((d) => d.lastError).map((d) => <div key={d.memberId} className="note warn">{d.lastError}</div>)}</div>
      })}
      <div className="hint">{t('受理表示请求已交给本机处理，实际升级结果以版本和升级通知为准。')}</div>
      <div className="hint">{t('主实例负责定时任务和 Telegram 机器人，停止后其他实例 15 秒内接管。「升级面板」会升级全部运行中的实例；恢复备份后全部实例自动重启。离线超过 1 天的实例自动移除。')}</div>
    </section>
  )
}

export function OperationalHealth() {
  const info = useLoad(() => get<{ issues: {key: string; detail: string}[]; states: {key: string; lastSuccess?: string}[] }>('/operational-health'))
  useEffect(() => { const id = setInterval(() => info.reload(), 15000); return () => clearInterval(id) }, [])
  const backup = info.data?.states?.find((s) => s.key === 'backup')
  return <section className="card">
    <h2>{t('运行状态')}</h2><p className="hint">{t('付款交付、退款、流量队列、USDT 核验与备份状态')}</p>
    {info.error && <div className="note warn">{t('运行状态暂不可用')}</div>}
    {info.data && (info.data.issues.length ? info.data.issues.map((i) => <div key={i.key} className="note warn">{i.detail}</div>) : <div className="note ok">{t('未检测到运行告警')}</div>)}
    {backup?.lastSuccess && <div className="hint">{t('最近成功备份')}：{datetime(backup.lastSuccess)}</div>}
  </section>
}
