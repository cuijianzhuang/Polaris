import { Lines } from './Charts'
import { colorOf, lossColor, pingColor } from './ProbeMonitor'
import { useProbeFeed } from '../useProbeFeed'
import { datetime } from '../format'
import { t } from '../i18n'

// Three-carrier backhaul section of the probe node drawer: the panel's
// per-carrier aggregation of the TCP probe (GET /nodes/:id/carriers).
// The failure figure is a TCP connection failure rate, never a packet
// loss rate (design D1); missing slots stay gaps in the mini trends.

type CarrierCurrent = { ms: number; avg: number | null; loss: number | null; at: string }
type CarrierTrendPoint = { t: string; avg: number | null; loss: number | null }
type CarrierTarget = { id: string; name: string; state: string; current: CarrierCurrent | null; hourAvg: number | null; loss1h: number | null }
type CarrierView = {
  state: string; current: CarrierCurrent | null; hourAvg: number | null; loss1h: number | null
  line: string | null; confidence: string | null; lineAt: string | null
  trend: CarrierTrendPoint[]; targets: CarrierTarget[]
}

// Heuristic line label (design D5/D9): a suspected GIA is worded as a
// suspicion, an ASN-only match says so; the label is never stated as a
// carrier fact.
function lineText(v: CarrierView): string {
  if (!v.line) return ''
  const base: Record<string, string> = { cn2: 'CN2', '163': '163', '9929': '9929', '4837': '4837', cmin2: 'CMIN2', cmi: 'CMI', cmnet: 'CMNET', unknown: t('无法识别') }
  let s = base[v.line] ?? v.line
  if (v.line === 'cn2' && v.confidence === 'suspect') s += t('（疑似 GIA）')
  else if (v.confidence === 'asn') s += t('（按 ASN 识别）')
  return s
}
type CarriersResp = { probeEnabled: boolean; carriers: Record<string, CarrierView> }

const carrierNames: Record<string, string> = { ct: t('电信'), cu: t('联通'), cm: t('移动') }
const pad = (x: number) => String(x).padStart(2, '0')
const msText = (ms: number) => ms > 0 ? ms.toFixed(ms < 1 ? 1 : 0) + ' ms' : t('超时')
const pctText = (v: number | null) => v == null ? '—' : v.toFixed(1) + '%'

function CarrierColumn({ carrier, v }: { carrier: string; v: CarrierView }) {
  const color = colorOf(carrier)
  const hasTrend = v.trend.some((p) => p.avg != null || p.loss != null)
  const labels = v.trend.map((p) => { const d = new Date(p.t); return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}` })
  return (
    <div className="pv-carrier">
      <div className="pv-carrier-head"><span className="legend"><i style={{ background: color }} />{carrierNames[carrier]}</span></div>
      {v.state === 'no_target' && <div className="sub">{t('未配置该运营商目标')}</div>}
      {v.state === 'disabled' && <div className="sub">{t('探测已停用')}</div>}
      {v.state === 'no_data' && <div className="sub">{t('暂无数据')}</div>}
      {(v.state === 'ok' || v.state === 'stale') && <>
        {v.state === 'stale' && <div className="sub" style={{ color: 'var(--pv-yellow)' }}>{t('数据滞后，以下为最近记录')}</div>}
        {v.current && <div className="pv-carrier-now" style={{ color: pingColor(v.current.ms) }}>{msText(v.current.ms)}</div>}
        <div className="pv-carrier-rows">
          {v.current?.avg != null && <div><span>{t('最新 5 分钟平均')}</span><b>{v.current.avg.toFixed(1)} ms</b></div>}
          <div><span>{t('连接失败率')}</span><b style={{ color: v.current?.loss != null ? lossColor(v.current.loss) : undefined }}>{pctText(v.current?.loss ?? null)}</b></div>
          <div><span>{t('近 1 小时失败率')}</span><b style={{ color: v.loss1h != null ? lossColor(v.loss1h) : undefined }}>{pctText(v.loss1h)}</b></div>
          {v.current && <div><span>{t('最近探测')}</span><b>{datetime(v.current.at)}</b></div>}
          {v.line && <div><span>{t('识别线路')}</span><b>{lineText(v)}</b></div>}
          {v.line && v.lineAt && <div><span>{t('最近分类')}</span><b>{datetime(v.lineAt)}</b></div>}
        </div>
        {v.targets.length > 1 && <div className="pv-carrier-targets">
          {v.targets.map((tg) => (
            <div key={tg.id}><span>{tg.name}</span><b>{tg.current ? msText(tg.current.ms) : tg.state === 'stale' ? t('数据滞后') : t('暂无数据')}{tg.loss1h != null ? ` · ${pctText(tg.loss1h)}` : ''}</b></div>
          ))}
        </div>}
        {hasTrend && <>
          <div className="small mute" style={{ marginTop: 8 }}>{t('近 24 小时延迟')}</div>
          <Lines labels={labels} fmt={(x) => `${x.toFixed(0)} ms`} height={64} series={[{ name: carrierNames[carrier], color, values: v.trend.map((p) => p.avg) }]} />
          <div className="small mute" style={{ marginTop: 4 }}>{t('连接失败率')}</div>
          <Lines labels={labels} max={100} fmt={(x) => `${x.toFixed(0)}%`} height={64} series={[{ name: t('连接失败率'), color, values: v.trend.map((p) => p.loss) }]} />
        </>}
      </>}
      {v.line && v.state !== 'ok' && v.state !== 'stale' && <div className="pv-carrier-rows">
        <div><span>{t('识别线路')}</span><b>{lineText(v)}</b></div>
        {v.lineAt && <div><span>{t('最近分类')}</span><b>{datetime(v.lineAt)}</b></div>}
      </div>}
    </div>
  )
}

export default function NodeCarriers({ nodeId }: { nodeId: string }) {
  const data = useProbeFeed<CarriersResp>(`/nodes/${nodeId}/carriers`, 10000)
  const d = data.data
  return (
    <div className="tchart">
      <div className="ctitle"><span>{t('三网回程')}</span></div>
      {!d ? <div className="sub" style={{ marginTop: 10 }}>{data.error ?? t('加载中…')}</div> : (
        <div className="pv-carriers">
          {['ct', 'cu', 'cm'].map((ck) => <CarrierColumn key={ck} carrier={ck} v={d.carriers[ck] ?? { state: 'no_target', current: null, hourAvg: null, loss1h: null, line: null, confidence: null, lineAt: null, trend: [], targets: [] }} />)}
        </div>
      )}
    </div>
  )
}
