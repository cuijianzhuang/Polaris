import { getLang, t } from './i18n'

export type GeoLocation = { ip: string; state: string; countryCode?: string; country?: string; countryEn?: string; province?: string; provinceEn?: string; city?: string; cityEn?: string; network?: string }
export function geoLocationText(g: GeoLocation): string {
  const parts = getLang() === 'en' ? [g.countryEn || g.country || g.countryCode, g.provinceEn || g.province, g.cityEn || g.city] : [g.country || g.countryEn || g.countryCode, g.province || g.provinceEn, g.city || g.cityEn]
  return parts.filter((part): part is string => !!part).filter((part, i, values) => i === 0 || part !== values[i - 1]).join(' · ')
}
export function geoStateText(state: string): string {
  return ({ private: t('私网或特殊地址'), not_found: t('数据库未收录地理归属'), unavailable: t('地理归属查询不可用'), invalid: t('IP 地址无效') }[state] ?? t('暂无地理归属'))
}
