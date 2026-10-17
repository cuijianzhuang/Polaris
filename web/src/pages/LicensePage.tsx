import License from '../components/License'
import { t } from '../i18n'

// LicensePage: the license on its own page (plan, activation, usage).
export default function LicensePage() {
  return (
    <>
      <div className="top"><div><h1>{t('许可证')}</h1><div className="sub">{t('套餐、激活与用量')}</div></div></div>
      <License />
    </>
  )
}
