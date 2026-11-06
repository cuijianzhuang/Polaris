import { Fragment } from 'react'
import { flag } from '../format'
import taiwanFlag from '../../../internal/flags/taiwan.svg?inline'

// A bundled image avoids OS/locale-dependent suppression of the Taiwan emoji.
function TaiwanFlag() {
  return <img className="country-flag" src={taiwanFlag} alt="TW" draggable={false} style={{ display: 'inline-block', width: '1.5em', height: '1em', verticalAlign: '-.125em', flexShrink: 0 }} />
}

export function FlagText({ children }: { children: string }) {
  return <>{children.split('🇹🇼').map((part, i) => <Fragment key={i}>{i > 0 && <TaiwanFlag />}{part}</Fragment>)}</>
}

export default function CountryFlag({ code, fallback = '' }: { code?: string; fallback?: string }) {
  const value = code?.trim() ?? ''
  const symbol = /^[\u{1F1E6}-\u{1F1FF}]{2}$/u.test(value) ? value : flag(value)
  return <FlagText>{symbol || fallback}</FlagText>
}
