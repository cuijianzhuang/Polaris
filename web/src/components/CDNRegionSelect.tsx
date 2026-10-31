import { get } from '../api'
import { getLang } from '../i18n'
import { useLoad } from './ui'

type Region = { code: string; name: string; en: string }
export default function CDNRegionSelect({ value, onChange, label, emptyLabel, disabled }: { value: string; onChange: (v: string) => void; label: string; emptyLabel: string; disabled?: boolean }) {
  const data = useLoad(() => get<Region[]>('/cdn-regions'))
  const regions = Array.isArray(data.data) ? data.data : []
  return <select className="input" aria-label={label} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
    <option value="">{emptyLabel}</option>
    {value && !regions.some((r) => r.code === value) && <option value={value}>{value}</option>}
    {regions.map((r) => <option key={r.code} value={r.code}>{getLang() === 'en' ? r.en : r.name}</option>)}
  </select>
}
