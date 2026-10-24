import type { OutboundIPStrategy } from '../types'
import { t } from '../i18n'
import { Field } from './ui'

export default function OutboundIPField({ value, onChange }: { value?: OutboundIPStrategy; onChange: (v: OutboundIPStrategy) => void }) {
  return <Field label={t('出站 IP 策略')} hint={t('仅影响节点发起的业务连接，不修改监听或分享地址。非默认策略需要新 Agent、Xray 26.5.9+ 或 sing-box 1.12+；不兼容组合会在保存时提示。')}>
    <select className="input" aria-label={t('出站 IP 策略')} value={value || 'auto'} onChange={(e) => onChange(e.target.value as OutboundIPStrategy)}>
      <option value="auto">{t('自动（IPv4 / IPv6）')}</option>
      <option value="ipv4_only">{t('仅 IPv4 出站')}</option><option value="ipv6_only">{t('仅 IPv6 出站')}</option>
      <option value="prefer_ipv4">{t('优先 IPv4（失败回退 IPv6）')}</option><option value="prefer_ipv6">{t('优先 IPv6（失败回退 IPv4）')}</option>
    </select>
    <div className="hint">{t('优先模式的连接回退仅适用于 TCP；UDP 不会因超时自动重发到另一地址族。')}</div>
  </Field>
}
