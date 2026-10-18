import { useSearchParams } from 'react-router-dom'
import PluginManager from '../components/PluginManager'
import { APITokens } from '../components/SecurityCards'
import Webhooks from '../components/Webhooks'
import { t } from '../i18n'

const tabs = () => [
  { id: 'plugins', label: t('插件') },
  { id: 'tokens', label: t('API 令牌') },
  { id: 'webhooks', label: 'Webhook' },
]

// Extensions: everything that connects the panel to outside tools.
export default function Extensions() {
  const [q, setQ] = useSearchParams()
  const all = tabs()
  const tab = all.some((x) => x.id === q.get('tab')) ? q.get('tab')! : 'plugins'
  return (
    <>
      <div className="top">
        <div>
          <h1>{t('扩展')}</h1>
          <div className="sub">{t('安装插件，管理 API 令牌和 Webhook，连接外部工具与服务')}</div>
        </div>
      </div>
      <div className="tabs" role="tablist" style={{ marginBottom: 14 }}>
        {all.map((x) => <button key={x.id} type="button" role="tab" aria-selected={tab === x.id} className={'tab' + (tab === x.id ? ' on' : '')} onClick={() => setQ({ tab: x.id }, { replace: true })}>{x.label}</button>)}
      </div>
      {tab === 'plugins' && <PluginManager />}
      {tab === 'tokens' && <APITokens />}
      {tab === 'webhooks' && <Webhooks />}
    </>
  )
}
