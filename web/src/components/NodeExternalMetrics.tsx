import { get } from '../api'
import { ago } from '../format'
import { t } from '../i18n'
import { useLoad } from './ui'

type Metric = { source: string; sourceLabel?: string; name: string; label?: string; unit: string; value: number; at: string }

const show = (m: Metric) => m.unit === 'bool' ? (m.value ? t('是') : t('否')) : `${Number(m.value.toFixed(2))} ${m.unit === 'count' ? '' : m.unit}`.trim()

// NodeExternalMetrics: readings outside services (monitoring plugins) report
// for the node. Shown as reported, with their source; nothing here comes
// from the node's own agent.
export default function NodeExternalMetrics({ nodeId }: { nodeId: string }) {
  const data = useLoad(() => get<{ metrics: Metric[] }>(`/nodes/${nodeId}/metrics/external`), [nodeId])
  const ms = data.data?.metrics ?? []
  if (!ms.length) return null
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div className="ctitle"><span>{t('外部指标')}</span><span className="small mute">{t('由插件或 API 令牌上报，仅供参考，不影响节点状态')}</span></div>
      <div className="tbl"><table><thead><tr><th>{t('指标')}</th><th>{t('数值')}</th><th>{t('来源')}</th><th>{t('时间')}</th></tr></thead><tbody>
        {ms.map((m) => <tr key={m.source + '/' + m.name}><td>{m.label || <span className="mono">{m.name}</span>}</td><td className="mono">{show(m)}</td><td className="small">{m.sourceLabel || m.source}</td><td className="small mute">{ago(m.at)}</td></tr>)}
      </tbody></table></div>
    </section>
  )
}
