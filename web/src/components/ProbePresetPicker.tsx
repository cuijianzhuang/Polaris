import { useState } from 'react'
import { t } from '../i18n'
import { probeCatalogUpdatedAt, probePresets, type ProbeCarrier, type ProbeFamily, type ProbeLevel, type ProbePreset } from '../probeTargets'
import { Field } from './ui'

type Filters = Pick<ProbePreset, 'level' | 'province' | 'city' | 'carrier' | 'family'>
const distinct = (values: string[]) => [...new Set(values)].sort((a, b) => a.localeCompare(b, 'zh-CN'))
const matching = (f: Filters) => probePresets.filter((p) => p.level === f.level && p.province === f.province && p.family === f.family && p.city === f.city)

export default function ProbePresetPicker({ initial, onChange }: { initial?: ProbePreset; onChange: (preset?: ProbePreset) => void }) {
  const [f, setF] = useState<Filters>(initial ?? { level: 'province', province: '', city: '', carrier: 'ct', family: 'ipv4' })
  const change = (patch: Partial<Filters>) => {
    const next = { ...f, ...patch }
    if (patch.level !== undefined) { next.province = ''; next.city = ''; if (next.level === 'city') next.family = 'ipv4' }
    if (patch.province !== undefined) next.city = ''
    const candidates = matching(next)
    if (!candidates.some((p) => p.carrier === next.carrier) && candidates.length) next.carrier = candidates[0].carrier
    setF(next)
    onChange(candidates.find((p) => p.carrier === next.carrier))
  }
  const provinces = distinct(probePresets.filter((p) => p.level === f.level).map((p) => p.province))
  const cities = distinct(probePresets.filter((p) => p.level === 'city' && p.province === f.province).map((p) => p.city))
  const candidates = matching(f)
  const carriers: { v: ProbeCarrier; label: string }[] = [{ v: 'ct', label: t('电信') }, { v: 'cu', label: t('联通') }, { v: 'cm', label: t('移动') }]
  return <>
    <div className="small mute"><a className="link" href="https://www.zstaticcdn.com/" target="_blank" rel="noopener noreferrer">Zstatic</a> · {t('目录更新于 {date}', { date: probeCatalogUpdatedAt })}</div>
    <div className="form-grid">
      <Field label={t('节点级别')} htmlFor="pp-level"><select id="pp-level" className="input" value={f.level} onChange={(e) => change({ level: e.target.value as ProbeLevel })}>
        <option value="province">{t('省级')}</option><option value="city">{t('市级')}</option>
      </select></Field>
      <Field label={t('省份')} htmlFor="pp-province"><select id="pp-province" className="input" value={f.province} onChange={(e) => change({ province: e.target.value })}>
        <option value="">{t('请选择省份')}</option>{provinces.map((p) => <option key={p} value={p}>{p}</option>)}
      </select></Field>
      {f.level === 'city' && <Field label={t('城市')} htmlFor="pp-city"><select id="pp-city" className="input" value={f.city} disabled={!f.province} onChange={(e) => change({ city: e.target.value })}>
        <option value="">{t('请选择城市')}</option>{cities.map((city) => <option key={city} value={city}>{city}</option>)}
      </select></Field>}
      <Field label={t('运营商')} htmlFor="pp-carrier"><select id="pp-carrier" className="input" value={candidates.length ? f.carrier : ''} disabled={!candidates.length} onChange={(e) => change({ carrier: e.target.value as ProbeCarrier })}>
        {!candidates.length && <option value="">{t('请先选择地区')}</option>}
        {carriers.filter((c) => candidates.some((p) => p.carrier === c.v)).map((c) => <option key={c.v} value={c.v}>{c.label}</option>)}
      </select></Field>
      <Field label={t('IP 协议')} htmlFor="pp-family" full={f.level === 'city'} hint={f.level === 'city' ? t('市级目录目前仅提供 IPv4 节点') : undefined}>
        <select id="pp-family" className="input" value={f.family} disabled={f.level === 'city'} onChange={(e) => change({ family: e.target.value as ProbeFamily })}>
          <option value="ipv4">IPv4</option>{f.level === 'province' && <><option value="ipv6">IPv6</option><option value="dual">{t('双栈')}</option></>}
        </select>
      </Field>
    </div>
  </>
}
