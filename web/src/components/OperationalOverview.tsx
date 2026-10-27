import { Link } from 'react-router-dom'
import { datetime } from '../format'
import { t } from '../i18n'
import { Empty } from './ui'

export interface OperationalData {
  available: boolean; checkedAt: string; issueCount: number; truncated: boolean
  relayBusiness: { total: number; passed: number; failed: number; untested: number; stale: number; running: number; interrupted: number; unavailable: number; inactive: number }
  sampling: { healthy: number; stale: number; never: number; inactive: number; pending: number; legacy: number }
  delivery: { paymentPending: number; paymentReview: number; refundPending: number; clusterPending: number }
  issues: { kind: string; resourceId: string; nodeId?: string; label?: string; state: string; code: string; checkedAt?: string }[]
}

export default function OperationalOverview({ data }: { data?: OperationalData }) {
  const [r, s, d] = [data?.relayBusiness, data?.sampling, data?.delivery]
  return <section className="card ops-stack operational-overview" aria-label={t('运维汇总')}>
    <div className="ctitle"><h2>{t('运维汇总')}</h2><span className="small mute">{data?.checkedAt ? datetime(data.checkedAt) : ''}</span></div>
    {!data?.available ? <div className="note warn">{t('运维数据暂不可用，不能据此判断系统健康。')}</div> : <>
      <div className="grid g3">
        <div className="ops-summary"><b>{t('线路业务')}</b><div>{t('验证失败')} <strong>{r?.failed}</strong> · {t('未测试')} {r?.untested} · {t('需重测')} {r?.stale}</div><div className="small mute">{t('已通过')} {r?.passed} · {t('运行中')} {r?.running} · {t('已中断')} {r?.interrupted} · {t('不可用')} {r?.unavailable} · {t('已停用')} {r?.inactive}</div></div>
        <div className="ops-summary"><b>{t('流量采样')}</b><div>{t('过期')} <strong>{s?.stale}</strong> · {t('无样本')} {s?.never} · {t('正常')} {s?.healthy}</div><div className="small mute">{t('旧版采样')} {s?.legacy} · {t('待接入')} {s?.pending} · {t('离线')} {s?.inactive}</div></div>
        <div className="ops-summary"><b>{t('付款与交付')}</b><div>{t('待交付付款')} <strong>{d?.paymentPending}</strong> · {t('人工对账')} {d?.paymentReview}</div><div className="small mute">{t('退款待处理')} {d?.refundPending} · {t('集群待交付')} {d?.clusterPending}</div></div>
      </div>
      <div className="hint">{t('未测试不代表故障，采样缺失不代表流量为零；付款、退款、集群交付统计可能重叠。')}</div>
      <details><summary>{t('异常明细')} · {data.issueCount}{data.truncated ? ' · ' + t('仅展示前 200 条') : ''}</summary><div className="ops-stack" style={{ marginTop: 12 }}>{data.issues.length === 0 ? <Empty>{t('没有已记录的运维异常')}</Empty> : data.issues.map((x, i) => <div className="ops-summary" key={`${x.kind}/${x.resourceId}/${i}`}><div className="row"><b>{x.label || x.resourceId}</b><span className="tag">{x.state}</span><Link to={x.nodeId ? `/nodes?id=${encodeURIComponent(x.nodeId)}` : x.kind === 'delivery' && x.code !== 'cluster_pending' ? '/shop?tab=orders' : '/settings#general'}>{t('查看')}</Link></div><div className="small mono ops-break">{x.kind} · {x.code}</div><div className="small mute">{x.checkedAt ? datetime(x.checkedAt) : '—'}</div></div>)}</div></details>
    </>}
  </section>
}
