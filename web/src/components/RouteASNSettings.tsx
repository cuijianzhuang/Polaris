import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, get, post, put } from '../api'
import { bytes, datetime } from '../format'
import { t } from '../i18n'
import RepositoryDatabaseInstall from './RepositoryDatabaseInstall'
import { Field, Switch, useAction, useLoad } from './ui'

type ASNInfo = {
  policy: { enabled: boolean; sha256: string; revision: number }
  databaseState: string
  database?: { databaseType: string; buildEpoch: number; ipVersion: number }
  effectiveEnabled: boolean
}
type Lookup = {
  databaseSHA256: string
  match: { ip: string; state: string; asn?: number; organization?: string; network?: string }
}
const maxSize = 64 * 1024 * 1024

export function RouteASNSettings() {
  const info = useLoad(() => get<ASNInfo>('/tools/route-asn'))
  const [file, setFile] = useState<File>()
  const [fileVersion, setFileVersion] = useState(0)
  const [ip, setIP] = useState('')
  const [lookup, setLookup] = useState<Lookup>()
  const { busy, run } = useAction()
  const d = info.data
  const locked = busy || info.loading || !!info.error || !d
  const ready = d?.databaseState === 'ready'
  const fileError = !file ? '' : file.size === 0 ? t('文件不能为空') : file.size > maxSize ? t('文件超过 64 MiB') : !/\.mmdb$/i.test(file.name) ? t('请选择 .mmdb 格式的 ASN 数据库') : ''
  const state = ({ ready: t('数据库就绪'), not_configured: t('未安装数据库'), local_database_missing: t('当前实例缺少文件'), local_database_unreadable: t('数据库无法读取'), hash_mismatch: t('数据库校验不一致'), invalid_database: t('数据库无效') }[d?.databaseState ?? ''] ?? t('数据库不可用'))
  const status = d?.effectiveEnabled ? t('已生效') : d?.policy.enabled ? t('已开启 · 当前实例未生效') : t('未启用')
  const guidance = d?.databaseState === 'not_configured' ? t('先上传 ASN 数据库，再开启注释。')
    : !ready ? t('请向当前实例重新上传策略对应的数据库，校验通过后才能生效。')
    : !d?.policy.enabled ? t('数据库已就绪，开启后将为新的回程检测添加 ASN 归属。')
    : t('新的回程检测将使用当前数据库，历史结果保持原样。')
  useEffect(() => { setLookup(undefined) }, [d?.policy.sha256])
  const upload = () => run(async () => {
    if (!file || !d || fileError) return
    try {
      await api('PUT', `/tools/route-asn/database?expectedRevision=${d.policy.revision}`, file)
      setFile(undefined); setFileVersion((v) => v + 1); setLookup(undefined)
    } finally { info.reload() }
  }, t('ASN 数据库已上传'))
  const match = !info.loading && !info.error && lookup?.databaseSHA256 === d?.policy.sha256 ? lookup?.match : undefined

  return <div className="asn-settings ops-stack" aria-busy={busy || info.loading}>
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('回程 ASN 归属')}</h2><div className="row"><Link className="btn sm" to="/nodes?network=route">{t('查看回程检测')}</Link><button className="btn sm" disabled={busy || info.loading} onClick={info.reload}>{t('刷新')}</button></div></div>
      <p className="hint asn-description">{t('为回程检测的每跳 IP 标注 ASN 编号和所属组织，便于识别网络归属。')}</p>
      <div className="hint">{t('显示位置：节点 → 回程与定时检测 → 回程历史详情 → ASN 列。')}</div>
      {info.error && <div className="note warn" role="alert">{info.error}</div>}
      {info.loading && !d && <div className="hint" role="status">{t('正在读取数据库状态…')}</div>}
      {d && <>
        <div className="asn-enable">
          <div><div className="asn-enable-title">{t('启用 ASN 注释')}<span className={'asn-status' + (d.effectiveEnabled ? ' active' : d.policy.enabled ? ' warning' : '')}>{status}</span></div><div className="hint">{guidance}</div></div>
          <Switch label={t('启用 ASN 注释')} on={d.policy.enabled} disabled={locked || (!d.policy.enabled && !ready)} onChange={(enabled) => run(async () => {
            try { await put('/tools/route-asn', { enabled, expectedRevision: d.policy.revision }) } finally { info.reload() }
          }, t('已保存'))} />
        </div>
        <dl className="asn-metadata">
          <div><dt>{t('当前实例数据库')}</dt><dd>{state}</dd></div>
          <div><dt>{t('数据库类型')}</dt><dd>{d.database?.databaseType || '—'}</dd></div>
          <div><dt>{t('数据库构建时间')}</dt><dd>{d.database?.buildEpoch ? datetime(new Date(d.database.buildEpoch * 1000).toISOString()) : '—'}</dd></div>
          <div><dt>{t('地址支持')}</dt><dd>{d.database ? (d.database.ipVersion === 6 ? 'IPv4 / IPv6' : 'IPv4') : '—'}</dd></div>
          {d.policy.sha256 && <div className="asn-hash"><dt>SHA-256</dt><dd className="mono">{d.policy.sha256}</dd></div>}
        </dl>
      </>}
    </section>

    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('安装与更新数据库')}</h2></div>
      <div className="hint">{t('支持 GeoLite2 ASN、GeoIP2 ASN 等兼容 MaxMind ASN 字段的 MMDB 文件；国家库、城市库不能用于 ASN 注释。')}</div>
      <RepositoryDatabaseInstall kind="asn" disabled={locked} busy={busy} onFetch={(source) => run(async () => { if (!d) return; try { await post('/tools/route-asn/database/fetch', { source, expectedRevision: d.policy.revision }); setFile(undefined); setFileVersion((v) => v + 1); setLookup(undefined) } finally { info.reload() } }, t('ASN 数据库已安装'))} />
      <div className="asn-source-links"><span>{t('获取数据库')}</span><a href="https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb" target="_blank" rel="noopener noreferrer">GeoLite2-ASN.mmdb · GitHub ↗</a><a href="https://cdn.jsdelivr.net/gh/Loyalsoldier/geoip@release/GeoLite2-ASN.mmdb" target="_blank" rel="noopener noreferrer">{t('CDN 镜像')} ↗</a><a href="https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb.sha256sum" target="_blank" rel="noopener noreferrer">{t('SHA-256 校验文件')} ↗</a></div>
      <div className="hint">{t('由 Loyalsoldier/geoip 提供 GeoLite2 ASN 下载。请选择 GeoLite2-ASN.mmdb；Country.mmdb、Country-asn.mmdb 是规则分类库，不提供 ASN 编号。')}</div>
      <div className="asn-source-links"><span>{t('备用 ASN 数据源')}</span><a href="https://github.com/lwpk110/free-geoip-databases/releases?q=geolite2&amp;expanded=true" target="_blank" rel="noopener noreferrer">free-geoip-databases ↗</a></div>
      <div className="hint">{t('备用来源请选择 GeoLite2-ASN-日期.mmdb；不要选择 City 或 Country 文件。')}</div>
      <details className="asn-details"><summary>{t('数据来源与格式说明')}</summary><div className="asn-source-links"><a href="https://github.com/Loyalsoldier/geoip#maxmind-mmdb-%E4%B8%8B%E8%BD%BD%E5%9C%B0%E5%9D%80" target="_blank" rel="noopener noreferrer">Loyalsoldier / geoip ↗</a><a href="https://dev.maxmind.com/geoip/geolite2-free-geolocation-data/" target="_blank" rel="noopener noreferrer">{t('GeoLite2 ASN（需免费账号）')} ↗</a><a href="https://dev.maxmind.com/geoip/docs/databases/asn/" target="_blank" rel="noopener noreferrer">{t('ASN 格式说明')} ↗</a></div></details>
      <div className="hint asn-details">{t('也可以手动上传数据库文件。')}</div>
      <Field htmlFor="asn-database-file" label={t('上传 ASN MMDB')} hint={t('选择解压后的 .mmdb 文件，最大 64 MiB。上传后会校验格式、完整性和 SHA-256。')}>
        <input id="asn-database-file" key={fileVersion} className="input asn-file" type="file" accept=".mmdb" disabled={locked} onChange={(e) => setFile(e.target.files?.[0])} />
      </Field>
      {file && <div className="asn-selected"><span className="ops-break">{file.name} · {bytes(file.size)}</span><button type="button" className="link" disabled={busy} onClick={() => { setFile(undefined); setFileVersion((v) => v + 1) }}>{t('移除')}</button></div>}
      {fileError && <div className="note warn" role="alert">{fileError}</div>}
      <button className="btn pri" disabled={locked || !file || !!fileError} onClick={upload}>{busy ? t('正在处理…') : t('上传数据库')}</button>
      <div className="hint">{t('首次上传后需手动开启注释；替换数据库保留原开关状态。')}</div>
      <details className="asn-details"><summary>{t('多实例部署与备份说明')}</summary><p className="hint">{t('数据库文件仅安装在当前实例。多实例须分别安装同一文件；配置备份不包含 MMDB，迁移或恢复后需重新上传。')}</p></details>
    </section>

    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('验证 IP 归属')}</h2></div>
      <div className="hint">{t('使用当前实例数据库进行本地查询，无需开启注释，不会修改设置或检测历史。')}</div>
      <form className="asn-lookup-form" onSubmit={(e) => { e.preventDefault(); setLookup(undefined); run(async () => setLookup(await post<Lookup>('/tools/route-asn/lookup', { ip: ip.trim() }))) }}>
        <Field htmlFor="asn-lookup-ip" label={t('IPv4 / IPv6 地址')}><input id="asn-lookup-ip" className="input mono" value={ip} maxLength={64} placeholder="1.1.1.1 / 2606:4700:4700::1111" autoCapitalize="none" autoComplete="off" spellCheck={false} disabled={locked || !ready} onChange={(e) => { setIP(e.target.value); setLookup(undefined) }} /></Field>
        <button className="btn" type="submit" disabled={locked || !ready || !ip.trim()}>{t('查询归属')}</button>
      </form>
      {!ready && <div className="hint">{t('安装有效数据库后即可查询。')}</div>}
      {match && <div className="asn-lookup-result" role="status">
        <div className="mono ops-break">{match.ip}</div>
        {match.state === 'matched' ? <><strong>AS{match.asn} · {match.organization}</strong><div className="hint mono ops-break">{match.network}</div></> : <div>{({ private: t('私网或特殊地址，不查询 ASN 归属。'), not_found: t('当前数据库未收录此地址。'), unavailable: t('当前数据库查询不可用。'), invalid: t('请输入有效的 IPv4 或 IPv6 地址') }[match.state] ?? t('当前数据库查询不可用。'))}</div>}
      </div>}
      <div className="hint">{t('ASN 归属不代表真实 BGP 路径、线路等级或业务可用性。面板仅在管理员操作时下载数据库，不会向外部服务查询 IP。')}</div>
    </section>
  </div>
}
