// Per-inbound / per-outbound traffic since the last reset (Xray; sing-box
// Clash API), with reset controls.
import { useState } from 'react'
import { get, post } from '../api'
import { bytes, ago } from '../format'
import { useAction, useConfirm, useLoad, usePoll } from './ui'
import { t } from '../i18n'

export interface TagTrafficRow { nodeId: string; nodeName: string; kind: 'inbound' | 'outbound'; tag: string; up: number; down: number; updatedAt: string }

export default function TagTraffic() {
  const [kind, setKind] = useState<'inbound' | 'outbound'>('inbound')
  const data = useLoad(() => get<TagTrafficRow[]>('/stats/tags?kind=' + kind), [kind])
  usePoll(data.reload, 15000)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const rows = [...(data.data ?? [])].sort((a, b) => b.up + b.down - (a.up + a.down))
  const total = rows.reduce((s, r) => s + r.up + r.down, 0)
  const reset = async (r?: TagTrafficRow) => {
    const what = r ? `${r.nodeName} · ${r.tag}` : (kind === 'inbound' ? t('全部入站') : t('全部出站'))
    if (!(await confirm(t('重置 {what} 的流量统计？', { what }), { ok: t('重置') }))) return
    run(async () => { await post('/stats/tags/reset', r ? { kind, nodeId: r.nodeId, tag: r.tag } : { kind }); data.reload() }, t('已重置'))
  }
  return (
    <section className="card">
      <div className="ctitle">
        <span>{t('流量统计 · 合计 {v}', { v: bytes(total) })}</span>
        <div className="row">
          <div className="seg" style={{ display: 'inline-grid', gridTemplateColumns: 'repeat(2, auto)' }}>
            <button type="button" className={'opt' + (kind === 'inbound' ? ' on' : '')} onClick={() => setKind('inbound')}>{t('按入站')}</button>
            <button type="button" className={'opt' + (kind === 'outbound' ? ' on' : '')} onClick={() => setKind('outbound')}>{t('按出站')}</button>
          </div>
          <button className="btn sm" type="button" disabled={busy || rows.length === 0} onClick={() => reset()}>{t('全部重置')}</button>
        </div>
      </div>
      <div className="scroll" style={{ marginTop: 8 }}>
        {rows.length === 0 ? <div className="empty">{t('暂无数据（节点有流量经过后显示；节点 Agent 需为最新版）')}</div> : (
          <table>
            <thead><tr><th>{t('节点')}</th><th>{kind === 'inbound' ? t('入站') : t('出站')}</th><th>{t('上传')}</th><th>{t('下载')}</th><th>{t('合计')}</th><th>{t('更新')}</th><th /></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.nodeId + r.tag}>
                  <td>{r.nodeName || r.nodeId}</td>
                  <td className="mono small">{r.tag}</td>
                  <td className="mono small">{bytes(r.up)}</td>
                  <td className="mono small">{bytes(r.down)}</td>
                  <td className="mono small">{bytes(r.up + r.down)}</td>
                  <td className="small">{ago(r.updatedAt)}</td>
                  <td><button className="btn sm" type="button" disabled={busy} onClick={() => reset(r)}>{t('重置')}</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  )
}
