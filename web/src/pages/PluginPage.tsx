import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { get } from '../api'
import { errMsg } from '../format'
import { getLang, t } from '../i18n'
import { usePlugins } from '../plugins'

// PluginPage embeds an admin page served by a plugin. The address carries a
// short-lived signed identity the plugin verifies; the frame is sandboxed.
export default function PluginPage() {
  const { id = '', page = '' } = useParams()
  const list = usePlugins()
  const [target, setTarget] = useState<{ url: string; title: string } | null>(null)
  const [err, setErr] = useState('')
  const [n, setN] = useState(0)
  const plugin = list?.plugins.find((p) => p.id === id)
  useEffect(() => {
    setTarget(null); setErr('')
    get<{ url: string; title: string }>(`/plugins/${encodeURIComponent(id)}/pages/${encodeURIComponent(page)}?lang=${getLang()}`).then(setTarget).catch((e) => setErr(errMsg(e)))
  }, [id, page, n])
  return (
    <>
      <div className="top">
        <div><h1>{target?.title ?? t('插件页面')}</h1><div className="sub">{plugin?.name ?? id}</div></div>
        <div className="row" style={{ gap: 8 }}>
          <button className="btn" type="button" onClick={() => setN((x) => x + 1)}>{t('刷新')}</button>
          {target && <a className="btn" href={target.url} target="_blank" rel="noreferrer">{t('在新窗口打开')}</a>}
        </div>
      </div>
      {err && <div className="note bad">{err}</div>}
      {target && <iframe key={target.url} className="plugin-frame" src={target.url} title={target.title} referrerPolicy="no-referrer"
        sandbox="allow-scripts allow-forms allow-same-origin allow-popups allow-downloads" />}
      {target && <div className="hint" style={{ marginTop: 8 }}>{t('页面由插件提供。如果这里显示空白，可能是插件禁止被嵌入，请用「在新窗口打开」。')}</div>}
    </>
  )
}
