import { useEffect, useState } from 'react'
import { useSettingsDraft } from './SettingsDrafts'
import { Link } from 'react-router-dom'
import { api, get, post, put } from '../api'
import { bytes, datetime } from '../format'
import { geoLocationText, geoStateText, type GeoLocation } from '../geoip'
import { t } from '../i18n'
import RepositoryDatabaseInstall from './RepositoryDatabaseInstall'
import { Field, Switch, useAction, useLoad } from './ui'

type GeoInfo = { policy: { enabled: boolean; sha256: string; revision: number }; databaseState: string; database?: { databaseType: string; buildEpoch: number; ipVersion: number }; effectiveEnabled: boolean }
type GeoLookup = { databaseSHA256: string; match: GeoLocation }
export default function RouteGeoSettings() {
  const info = useLoad(() => get<GeoInfo>('/tools/route-geo'))
  const [file, setFile] = useState<File>()
  const [fileVersion, setFileVersion] = useState(0)
  const [ip, setIP] = useState('')
  const [lookup, setLookup] = useState<GeoLookup>()
  const { busy, run } = useAction()
  useSettingsDraft({ label: t('回程地理归属'), dirty: !!file, busy, discard: () => setFile(undefined) })
  const d = info.data, ready = d?.databaseState === 'ready', locked = busy || info.loading || !!info.error || !d
  const compressed = !!file && /\.mmdb\.gz$/i.test(file.name)
  const fileError = !file ? '' : file.size === 0 ? t('文件不能为空') : !/\.mmdb(\.gz)?$/i.test(file.name) ? t('请选择 .mmdb 或 .mmdb.gz 城市数据库') : file.size > (compressed ? 64 : 128) * 1024 * 1024 ? t('城市库限 128 MiB；压缩文件限 64 MiB，解压后仍不能超过 128 MiB。') : ''
  const state = ({ ready: t('数据库就绪'), not_configured: t('未安装数据库'), local_database_missing: t('当前实例缺少文件'), local_database_unreadable: t('数据库无法读取'), hash_mismatch: t('数据库校验不一致'), invalid_database: t('数据库无效'), invalid_policy: t('地理归属配置无效，请重新安装数据库') }[d?.databaseState ?? ''] ?? t('数据库不可用'))
  useEffect(() => { setLookup(undefined) }, [d?.policy.sha256])
  const match = !info.loading && !info.error && lookup?.databaseSHA256 === d?.policy.sha256 ? lookup?.match : undefined
  return <div className="asn-settings ops-stack" aria-busy={busy || info.loading}>
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('回程地理归属')}</h2><div className="row"><Link className="btn sm" to="/nodes?network=route">{t('查看回程检测')}</Link><button className="btn sm" disabled={busy || info.loading} onClick={info.reload}>{t('刷新')}</button></div></div>
      <p className="hint asn-description">{t('为新的回程记录标注国家、省份和城市，独立于 ASN 注释开关。')}</p>
      {info.error && <div className="note warn" role="alert">{info.error}</div>}
      {info.loading && !d && <div className="hint" role="status">{t('正在读取数据库状态…')}</div>}
      {d && <>
        <div className="asn-enable"><div><div className="asn-enable-title">{t('启用地理归属注释')}<span className={'asn-status' + (d.effectiveEnabled ? ' active' : d.policy.enabled ? ' warning' : '')}>{d.effectiveEnabled ? t('已生效') : d.policy.enabled ? t('已开启 · 当前实例未生效') : t('未启用')}</span></div><div className="hint">{ready ? t('开启后只为新回程记录添加归属，历史结果保持原样。') : t('先向当前实例上传有效的 City 类型数据库。')}</div></div><Switch label={t('启用地理归属注释')} on={d.policy.enabled} disabled={locked || (!d.policy.enabled && !ready)} onChange={(enabled) => run(async () => { try { await put('/tools/route-geo', { enabled, expectedRevision: d.policy.revision }) } finally { info.reload() } }, t('已保存'))} /></div>
        <dl className="asn-metadata">
          <div><dt>{t('当前实例数据库')}</dt><dd>{state}</dd></div><div><dt>{t('数据库类型')}</dt><dd>{d.database?.databaseType || '—'}</dd></div>
          <div><dt>{t('数据库构建时间')}</dt><dd>{d.database?.buildEpoch ? datetime(new Date(d.database.buildEpoch * 1000).toISOString()) : '—'}</dd></div><div><dt>{t('地址支持')}</dt><dd>{d.database ? d.database.ipVersion === 6 ? 'IPv4 / IPv6' : 'IPv4' : '—'}</dd></div>
          {d.policy.sha256 && <div className="asn-hash"><dt>SHA-256 · {t('解压后的数据库')}</dt><dd className="mono">{d.policy.sha256}</dd></div>}
        </dl>
      </>}
    </section>
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('安装与更新城市库')}</h2></div>
      <div className="hint">{t('支持 GeoLite2 City、GeoIP2 City 等兼容国家、省份、城市字段的 MMDB。ASN 库、国家库和规则分类库不能作为城市库。')}</div>
      <RepositoryDatabaseInstall kind="geo" disabled={locked} busy={busy} onFetch={(source) => run(async () => { if (!d) return; try { await post('/tools/route-geo/database/fetch', { source, expectedRevision: d.policy.revision }); setFile(undefined); setFileVersion((v) => v + 1); setLookup(undefined) } finally { info.reload() } }, t('城市数据库已安装'))} />
      <div className="asn-source-links"><span>{t('获取城市数据库')}</span><a href="https://raw.githubusercontent.com/wp-statistics/GeoLite2-City/master/GeoLite2-City.mmdb.gz" target="_blank" rel="noopener noreferrer">GeoLite2-City.mmdb.gz · GitHub ↗</a><a href="https://cdn.jsdelivr.net/npm/geolite2-city/GeoLite2-City.mmdb.gz" target="_blank" rel="noopener noreferrer">{t('CDN 镜像')} ↗</a><a href="https://github.com/lwpk110/free-geoip-databases/releases?q=geolite2&amp;expanded=true" target="_blank" rel="noopener noreferrer">{t('备用城市库来源')} ↗</a></div>
      <div className="hint">{t('主要下载来源为 wp-statistics/GeoLite2-City；备用来源请选择 GeoLite2-City-日期.mmdb。底层数据使用条款以原始提供者说明为准。')}</div>
      <div className="hint asn-details">{t('也可以手动上传数据库文件。')}</div>
      <Field htmlFor="geo-database-file" label={t('上传城市数据库')} hint={t('城市库限 128 MiB；压缩文件限 64 MiB，解压后仍不能超过 128 MiB。')}><input id="geo-database-file" key={fileVersion} className="input asn-file" type="file" accept=".mmdb,.gz" disabled={locked} onChange={(e) => setFile(e.target.files?.[0])} /></Field>
      {file && <div className="asn-selected"><span className="ops-break">{file.name} · {bytes(file.size)}</span><button className="link" type="button" disabled={busy} onClick={() => { setFile(undefined); setFileVersion((v) => v + 1) }}>{t('移除')}</button></div>}
      {fileError && <div className="note warn" role="alert">{fileError}</div>}
      <button className="btn pri" disabled={locked || !file || !!fileError} onClick={() => run(async () => { if (!file || !d || fileError) return; try { await api('PUT', `/tools/route-geo/database?expectedRevision=${d.policy.revision}`, file); setFile(undefined); setFileVersion((v) => v + 1); setLookup(undefined) } finally { info.reload() } }, t('城市数据库已上传'))}>{busy ? t('正在处理…') : t('上传城市库')}</button>
      <div className="hint">{t('首次上传后需手动开启注释；替换数据库保留原开关状态。')}</div>
      <details className="asn-details"><summary>{t('多实例部署与备份说明')}</summary><p className="hint">{t('数据库文件仅安装在当前实例。多实例须分别安装同一文件；配置备份不包含 MMDB，迁移或恢复后需重新上传。')}</p></details>
    </section>
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('验证地理归属')}</h2></div><div className="hint">{t('使用当前实例数据库进行本地查询，无需开启注释，不会修改设置或检测历史。')}</div>
      <form className="asn-lookup-form" onSubmit={(e) => { e.preventDefault(); setLookup(undefined); run(async () => setLookup(await post<GeoLookup>('/tools/route-geo/lookup', { ip: ip.trim() }))) }}>
        <Field htmlFor="geo-lookup-ip" label={t('IPv4 / IPv6 地址')}><input id="geo-lookup-ip" className="input mono" maxLength={64} value={ip} placeholder="8.8.8.8 / 2001:4860:4860::8888" autoCapitalize="none" autoComplete="off" spellCheck={false} disabled={locked || !ready} onChange={(e) => { setIP(e.target.value); setLookup(undefined) }} /></Field><button className="btn" disabled={locked || !ready || !ip.trim()} type="submit">{t('查询地理归属')}</button>
      </form>
      {!ready && <div className="hint">{t('安装有效数据库后即可查询。')}</div>}
      {match && <div className="asn-lookup-result" role="status"><div className="mono ops-break">{match.ip}</div><strong>{match.state === 'matched' ? geoLocationText(match) : geoStateText(match.state)}</strong>{match.network && <div className="hint mono ops-break">{match.network}</div>}</div>}
      <div className="hint">{t('地理归属是数据库估计，不代表精确物理位置。缺失的省份、城市不会推测补全；私网和特殊地址不查询。')}</div>
    </section>
  </div>
}
