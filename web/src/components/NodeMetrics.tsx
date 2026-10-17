import { useState } from 'react'
import { get } from '../api'
import { bps } from '../format'
import { t } from '../i18n'
import { Lines } from './Charts'
import { useLoad, usePoll } from './ui'

type Point = { t: string; cpu: number; mem: number; rx: number; tx: number; load: number }
const ranges = [{ h: 6, label: t('6 小时') }, { h: 24, label: t('24 小时') }, { h: 168, label: t('7 天') }, { h: 720, label: t('30 天') }]
const pad = (n: number) => String(n).padStart(2, '0')

// NodeMetrics: CPU / memory and bandwidth history (5-minute averages, 30 days).
export default function NodeMetrics({ nodeId }: { nodeId: string }) {
  const [hours, setHours] = useState(24)
  const data = useLoad(() => get<{ points: Point[] }>(`/nodes/${nodeId}/metrics?hours=${hours}`), [nodeId, hours])
  usePoll(data.reload, 60000)
  const pts = data.data?.points ?? []
  const labels = pts.map((p) => { const d = new Date(p.t); return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}` })
  return (
    <div className="tchart">
      <div className="ctitle">
        <span>{t('资源历史')}</span>
        <span className="seg-mini">{ranges.map((r) => <button key={r.h} type="button" className={hours === r.h ? 'on' : ''} onClick={() => setHours(r.h)}>{r.label}</button>)}</span>
      </div>
      {pts.length < 2 ? <div className="sub" style={{ marginTop: 10 }}>{t('数据还不够：节点在线后每 5 分钟记录一个点')}</div> : <>
        <div className="small mute" style={{ marginTop: 10 }}>
          <span className="legend"><i style={{ background: 'var(--pri)' }} />CPU</span>
          <span className="legend" style={{ marginLeft: 12 }}><i style={{ background: 'var(--warn)' }} />{t('内存')}</span>
        </div>
        <Lines labels={labels} max={100} fmt={(v) => `${v.toFixed(0)}%`} height={140}
          series={[{ name: 'CPU', color: 'var(--pri)', values: pts.map((p) => p.cpu) }, { name: t('内存'), color: 'var(--warn)', values: pts.map((p) => p.mem) }]} />
        <div className="small mute" style={{ marginTop: 6 }}>
          <span className="legend"><i style={{ background: 'var(--pri)' }} />{t('下行')}</span>
          <span className="legend" style={{ marginLeft: 12 }}><i style={{ background: 'var(--ok)' }} />{t('上行')}</span>
        </div>
        <Lines labels={labels} fmt={bps} height={140}
          series={[{ name: t('下行'), color: 'var(--pri)', values: pts.map((p) => p.rx) }, { name: t('上行'), color: 'var(--ok)', values: pts.map((p) => p.tx) }]} />
      </>}
    </div>
  )
}
