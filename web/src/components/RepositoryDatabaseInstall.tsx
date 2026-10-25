import { useState } from 'react'
import { t } from '../i18n'
import { Field } from './ui'

export default function RepositoryDatabaseInstall({ kind, disabled, busy, onFetch }: { kind: 'asn' | 'geo'; disabled: boolean; busy: boolean; onFetch: (source: string) => void }) {
  const [source, setSource] = useState(kind === 'asn' ? 'loyalsoldier' : 'wp-statistics')
  return <div className="ops-stack">
    <Field htmlFor={`${kind}-repository`} label={t('仓库来源')} hint={t('下载并安装到当前实例；首次安装后仍需手动开启注释。')}>
      <select id={`${kind}-repository`} className="input" value={source} disabled={disabled} onChange={(e) => setSource(e.target.value)}>
        {kind === 'asn' ? <><option value="loyalsoldier">Loyalsoldier / geoip · GitHub</option><option value="loyalsoldier-cdn">Loyalsoldier / geoip · CDN</option></> : <><option value="wp-statistics">wp-statistics / GeoLite2-City · GitHub</option><option value="wp-statistics-cdn">wp-statistics / GeoLite2-City · CDN</option></>}
        <option value="free-geoip">lwpk110 / free-geoip-databases · GitHub Releases</option>
      </select>
    </Field>
    <button className="btn pri" type="button" disabled={disabled} onClick={() => onFetch(source)}>{busy ? t('正在处理…') : t('从仓库获取并安装')}</button>
    <div className="hint">{t('自动选择对应 ASN 或 City 文件，校验通过后替换；下载失败或配置冲突会保留原数据库。')}</div>
  </div>
}
