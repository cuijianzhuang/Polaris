import { APITokens } from '../components/SecurityCards'
import { t } from '../i18n'

export default function Extensions() {
  return (
    <>
      <div className="top">
        <div>
          <h1>{t('扩展')}</h1>
          <div className="sub">{t('管理 API 令牌，连接外部工具与服务')}</div>
        </div>
      </div>
      <APITokens />
    </>
  )
}
