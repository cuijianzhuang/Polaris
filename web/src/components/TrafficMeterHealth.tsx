import { datetime } from '../format'
import { t } from '../i18n'
import type { Core } from '../types'

export default function TrafficMeterHealth({ core, connected }: { core: Core; connected: boolean }) {
  const meter = core.trafficMeter
  const sampled = meter?.mode === 'connection_sampled' || (core.type === 'mihomo' && meter?.mode !== 'kernel_counter')
  const at = meter?.lastSuccess ? Date.parse(meter.lastSuccess) : NaN
  const stale = Number.isFinite(at) && Date.now() - at > 180000
  const failed = (meter?.consecutiveFailures ?? 0) > 0
  const unavailable = core.noUserTraffic || meter?.mode === 'unavailable'
  const state = unavailable ? t('不支持用户计量') : !connected || core.status !== 'running' ? t('计量已暂停') : failed ? t('最近采集失败') : stale ? t('采集结果较旧') : !Number.isFinite(at) ? t('采集状态未上报') : t('采集正常')
  return <div className="meter-health small" data-meter-mode={meter?.mode || 'unknown'}>
    <div><strong>{sampled ? t('活动连接采样') : unavailable ? t('不支持用户计量') : t('内核用户计数')}</strong><span className={failed || stale || core.noUserTraffic || meter?.mode === 'unavailable' || meter?.mode === 'unavailable' ? 'warn' : 'mute'}> · {state}</span></div>
    {sampled && <div className="hint">{t('活动连接快照可能漏掉短连接及关闭前的尾部流量，不等同于精确账单。')}</div>}
    <div className="hint">{t('最近成功采集')}：{Number.isFinite(at) ? datetime(meter!.lastSuccess) : t('未上报')}{meter?.intervalSeconds ? ' · ' + t('采样间隔 {n} 秒', { n: meter.intervalSeconds }) : ''}</div>
  </div>
}
