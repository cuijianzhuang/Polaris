import { useState } from 'react'
import { get } from '../api'
import { bytes, datetime } from '../format'
import { t } from '../i18n'
import { useLoad } from './ui'

type Source = {
  hour: string; nodeId: string; nodeName: string; up: number; down: number
  rawUp: number | null; rawDown: number | null; accountUp: number | null; accountDown: number | null
  rateMin: number | null; rateMax: number | null; coverage: 'legacy' | 'partial' | 'complete'
}
type Result = {
  items: Source[]; nodes: { nodeId: string; nodeName: string }[]; total: number
  summary: { up: number; down: number; rawUp: number; rawDown: number; accountUp: number; accountDown: number; incompleteRows: number; recordedRows: number }
}
const PAGE = 20
const pair = (up: number | null, down: number | null) => up === null || down === null ? t('未记录') : `↑ ${bytes(up)} · ↓ ${bytes(down)}`

export default function UserTrafficSources({ userId }: { userId: string }) {
  const [hours, setHours] = useState(24)
  const [node, setNode] = useState('')
  const [offset, setOffset] = useState(0)
  const data = useLoad(() => get<Result>(`/users/${userId}/traffic-sources?hours=${hours}&node=${encodeURIComponent(node)}&limit=${PAGE}&offset=${offset}`), [userId, hours, node, offset])
  // useLoad retains the previous response while loading. Hide it so a newly
  // selected filter never displays the previous filter's rows or totals.
  const result = data.loading || data.error ? undefined : data.data
  const sum = result?.summary
  return (
    <section style={{ minWidth: 0 }} aria-label={t('流量来源')}>
      <div className="ctitle">{t('流量来源')}</div>
      <div className="row" style={{ marginTop: 8 }}>
        <select className="input" style={{ width: 'auto' }} aria-label={t('时间范围')} value={hours} onChange={(e) => { setHours(Number(e.target.value)); setNode(''); setOffset(0) }}>
          <option value={24}>{t('24 小时')}</option><option value={168}>{t('7 天')}</option><option value={720}>{t('30 天')}</option>
        </select>
        <select className="input" style={{ width: 'auto', maxWidth: '100%', flex: '1 1 120px', minWidth: 0 }} aria-label={t('节点')} value={node} onChange={(e) => { setNode(e.target.value); setOffset(0) }}>
          <option value="">{t('全部节点')}</option>
          {(data.data?.nodes ?? []).map((n) => <option key={n.nodeId} value={n.nodeId}>{n.nodeName || n.nodeId}</option>)}
        </select>
        <button type="button" className="btn sm" disabled={data.loading} onClick={data.reload}>{t('立即刷新')}</button>
      </div>
      <p className="hint">{t('按面板结算小时展示；离线补报可能晚于实际使用时间。箭头表示上行与下行。')}</p>
      {data.error && <p role="alert" className="error">{data.error}</p>}
      {data.loading && <p role="status" className="hint">{t('加载中…')}</p>}
      {sum && <>
        <div className="hint">{t('所选范围合计')}<br />
          {t('原始流量')}：{sum.incompleteRows > 0 && !sum.recordedRows ? t('未记录') : pair(sum.rawUp, sum.rawDown)}<br />
          {t('倍率后流量')}：{pair(sum.up, sum.down)}<br />
          {t('账户扣量')}：{sum.incompleteRows > 0 && !sum.recordedRows ? t('未记录') : pair(sum.accountUp, sum.accountDown)}
        </div>
        {sum.incompleteRows > 0 && <p className="hint">{t('包含旧记录：原始流量和账户扣量合计仅含已记录部分；部分记录的倍率也仅覆盖该部分。未记录不代表零流量。')}</p>}
        {result?.items.length === 0 ? <p className="hint">{t('所选范围暂无流量记录')}</p> :
          <div className="scroll" style={{ maxWidth: '100%', marginTop: 8 }} tabIndex={0} aria-label={t('流量来源明细')}>
            <table style={{ minWidth: 750 }}>
              <thead><tr>{[t('结算小时'), t('节点'), t('原始流量'), t('倍率'), t('倍率后流量'), t('账户扣量')].map((label) => <th key={label}>{label}</th>)}</tr></thead>
              <tbody>{result?.items.map((r) => <tr key={`${r.hour}/${r.nodeId}`}>
                <td style={{ whiteSpace: 'nowrap' }}>{datetime(r.hour).slice(0, -3)}</td>
                <td><div>{r.nodeName || t('已删除或未知节点')}</div><div className="hint mono" style={{ overflowWrap: 'anywhere', maxWidth: 170 }}>{r.nodeId}</div></td>
                <td>{pair(r.rawUp, r.rawDown)}{r.coverage === 'partial' && <div className="hint">{t('部分记录')}</div>}</td>
                <td>{r.rateMin === null ? t('未记录') : r.rateMin === r.rateMax ? `${r.rateMin}×` : `${r.rateMin}–${r.rateMax}×`}{r.coverage === 'partial' && <div className="hint">{t('部分记录')}</div>}</td>
                <td>{pair(r.up, r.down)}</td>
                <td>{pair(r.accountUp, r.accountDown)}{r.coverage === 'partial' && <div className="hint">{t('部分记录')}</div>}</td>
              </tr>)}</tbody>
            </table>
          </div>}
        <div className="row" style={{ marginTop: 8 }}>
          <button type="button" className="btn sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>{t('上一页')}</button>
          <span className="hint">{t('共 {n} 条', { n: result?.total ?? 0 })}</span>
          <button type="button" className="btn sm" disabled={offset + PAGE >= (result?.total ?? 0)} onClick={() => setOffset(offset + PAGE)}>{t('下一页')}</button>
        </div>
      </>}
      <p className="hint">{t('倍率记录结算时的实际值；同小时变更显示范围。节点流量包先于账户扣除。重置或手动调整会影响当前已用量，但不会改写历史来源。')}</p>
    </section>
  )
}
