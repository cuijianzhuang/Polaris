import { get } from '../api'
import { bps } from '../format'
import { t } from '../i18n'
import { useLoad, usePoll } from './ui'

type Row = { userId: string; username: string; upKbps: number; downKbps: number; upBps: number; downBps: number; saturated: boolean }

// Share of the allowance in use, for the busier direction (0 when unlimited).
function share(r: Row): number {
  const up = r.upKbps ? r.upBps / (r.upKbps * 1000) : 0
  const down = r.downKbps ? r.downBps / (r.downKbps * 1000) : 0
  return Math.min(100, Math.round(Math.max(up, down) * 100))
}

const allowance = (kbps: number) => (kbps ? bps(kbps * 1000) : t('不限'))

// LimitedUsers: users under a per-user speed limit on the node, with their
// current speed (from the agent's heartbeat). Hidden when nobody is limited.
export default function LimitedUsers({ nodeId }: { nodeId: string }) {
  const data = useLoad(() => get<Row[]>(`/nodes/${nodeId}/limited`), [nodeId])
  usePoll(data.reload, 5000)
  const rows = data.data ?? []
  if (!rows.length) return null
  const busy = rows.filter((r) => r.saturated).length
  return (
    <section className="card flush">
      <div className="ctitle" style={{ padding: '12px 16px 4px' }}>
        <span>{t('限速用户')}</span>
        <span className="sub" style={{ margin: 0 }}>{t('{n} 个用户受限速', { n: rows.length })}{busy ? ' · ' + t('{n} 个已顶满', { n: busy }) : ''}</span>
      </div>
      <div className="scroll">
        <table>
          <thead><tr><th>{t('用户')}</th><th>{t('额度 ↑ / ↓')}</th><th>{t('当前 ↑ / ↓')}</th><th style={{ minWidth: 140 }}>{t('占用')}</th></tr></thead>
          <tbody>
            {rows.map((r) => {
              const pct = share(r)
              return (
                <tr key={r.userId}>
                  <td style={{ fontWeight: 500 }}>{r.username || r.userId}{r.saturated && <span className="tag warn" style={{ marginLeft: 6 }}>{t('已顶满')}</span>}</td>
                  <td className="mono small">{allowance(r.upKbps)} / {allowance(r.downKbps)}</td>
                  <td className="mono small">{bps(r.upBps)} / {bps(r.downBps)}</td>
                  <td><div className="bar"><div style={{ width: pct + '%', background: r.saturated ? 'var(--warn)' : undefined }} /></div><span className="small mute">{pct}%</span></td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </section>
  )
}
