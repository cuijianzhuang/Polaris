// Xray geodata auto-update: Xray downloads the .dat files on a cron
// schedule and reloads them without a restart (Xray 26.4.25+).
import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { t } from '../i18n'
import { Field, Switch, useAction, useLoad } from './ui'

interface Asset { url: string; file: string }

export const STANDARD_GEO: Asset[] = [
  { url: 'https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat', file: 'geoip.dat' },
  { url: 'https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat', file: 'geosite.dat' },
]

export default function GeodataCard() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [on, setOn] = useState(false)
  const [cron, setCron] = useState('0 4 * * *')
  const [outbound, setOutbound] = useState('')
  const [assets, setAssets] = useState<Asset[]>(STANDARD_GEO)
  const { busy, run } = useAction()
  useEffect(() => {
    if (!s.data) return
    setOn(!!s.data.geodata_cron)
    if (s.data.geodata_cron) setCron(s.data.geodata_cron)
    setOutbound(s.data.geodata_outbound ?? '')
    try { const a = JSON.parse(s.data.geodata_assets || '[]'); if (Array.isArray(a) && a.length) setAssets(a) } catch { /* keep default */ }
  }, [s.data])
  const setA = (i: number, k: keyof Asset, v: string) => setAssets((xs) => xs.map((x, j) => (j === i ? { ...x, [k]: v } : x)))
  const save = () => run(async () => {
    const list = assets.filter((a) => a.url.trim() && a.file.trim())
    await put('/settings', { geodata_cron: on ? cron.trim() : '', geodata_outbound: outbound.trim(), geodata_assets: JSON.stringify(list) })
    s.reload()
  }, t('已保存，各节点将重新下发 Xray 配置'))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('Xray Geodata 自动更新')}</h2>
        <Switch label={t('Geodata 自动更新')} on={on} onChange={setOn} />
      </div>
      <div className="note">{t('Xray 按计划下载这些 .dat 文件并热重载，无需重启（需 Xray 26.4.25+）。地址必须为 HTTPS；缺失的文件由 Agent 先下载一次。路由规则可用')} <code>ext:geosite_IR.dat:ir</code> {t('引用自定义文件。在节点页 Xray 内核旁可立即更新。')}</div>
      <div className="form-grid">
        <Field label={t('更新计划（cron）')} hint={t('5 段，例如 0 4 * * * 为每天 4:00')}><input className="input mono" value={cron} disabled={!on} onChange={(e) => setCron(e.target.value)} /></Field>
        <Field label={t('通过出站下载（可选）')} hint={t('出站标签，如 warp；留空直连')}><input className="input mono" value={outbound} disabled={!on} onChange={(e) => setOutbound(e.target.value)} /></Field>
      </div>
      <div style={{ display: 'grid', gap: 8 }}>
        {assets.map((a, i) => (
          <div key={i} className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" style={{ flex: 3, minWidth: 0 }} value={a.url} placeholder="https://example.com/geosite_custom.dat" onChange={(e) => setA(i, 'url', e.target.value)} aria-label={t('下载地址')} />
            <input className="input mono" style={{ flex: 1, minWidth: 0 }} value={a.file} placeholder={t('文件名')} onChange={(e) => setA(i, 'file', e.target.value)} aria-label={t('文件名')} />
            <button className="btn sm danger" type="button" aria-label={t('删除')} onClick={() => setAssets((xs) => xs.filter((_, j) => j !== i))}>{t('删除')}</button>
          </div>
        ))}
      </div>
      <div className="row">
        <button className="btn" type="button" onClick={() => setAssets((xs) => [...xs, { url: '', file: '' }])}>{t('添加文件')}</button>
        <button className="btn" type="button" onClick={() => setAssets(STANDARD_GEO)}>{t('使用标准来源')}</button>
        <button className="btn pri" type="button" disabled={busy} onClick={save}>{t('保存')}</button>
      </div>
    </section>
  )
}
