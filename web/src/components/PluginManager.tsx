import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, del, post, put } from '../api'
import { ago } from '../format'
import { t } from '../i18n'
import { refreshPlugins, usePlugins, type PluginInfo, type PluginManifest } from '../plugins'
import { scopeLabel } from './SecurityCards'
import { copyText, Field, Modal, useAction, useConfirm } from './ui'

export const PLUGIN_DOCS = 'https://github.com/cuijianzhuang/polaris-plugins'

type Preview = { baseUrl: string; manifest: PluginManifest; installed: boolean; limit: number; used: number }
type Credentials = { apiBase: string; panelUrl: string; token: string; pluginSecret: string; webhookSecret: string }
type InstallResult = { plugin: PluginInfo; handshake: string; credentials?: Credentials; detail?: string }

const statusTag = (p: PluginInfo) => {
  if (!p.enabled) return <span className="tag">{t('已停用')}</span>
  switch (p.status) {
    case 'online': return <span className="tag ok"><span className="dot" />{t('运行中')}</span>
    case 'offline': return <span className="tag bad"><span className="dot" />{t('无法访问')}</span>
    default: return <span className="tag warn"><span className="dot" />{t('等待插件连接')}</span>
  }
}

// PluginManager lists installed plugins and installs, configures, updates and
// removes them.
export default function PluginManager() {
  const list = usePlugins()
  const [installing, setInstalling] = useState(false)
  const [settings, setSettings] = useState<PluginInfo | null>(null)
  const [creds, setCreds] = useState<InstallResult | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const plugins = list?.plugins ?? []
  const limit = list?.limit ?? 0
  const full = limit > 0 && plugins.length >= limit
  const labels = scopeLabel()

  const refresh = (p: PluginInfo, approve = false) => run(async () => {
    try {
      await post(`/plugins/${p.id}/refresh`, { approve })
    } catch (e) {
      if (e instanceof ApiError && e.data?.needApproval) {
        const more = [...(e.data.addScopes ?? []).map((s: string) => labels[s] ?? s), ...(e.data.addEvents ?? []), ...(e.data.addFormats ?? []).map((f: string) => t('订阅格式 {f}（会收到用户节点和连接凭据）', { f }))]
        if (await confirm(t('新版本需要更多权限：{list}。允许并更新？', { list: more.join('、') }), { ok: t('允许并更新') })) { await post(`/plugins/${p.id}/refresh`, { approve: true }) } else return
      } else throw e
    }
    await refreshPlugins()
  }, t('已读取最新的插件信息'))

  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle">
        <span>{t('插件')} <span className="tag" style={{ marginLeft: 6 }}>{plugins.length} / {limit > 0 ? limit : t('不限')}</span></span>
        <button className="btn sm pri" type="button" disabled={full} title={full ? t('已达到数量上限') : undefined} onClick={() => setInstalling(true)}>{t('+ 安装插件')}</button>
      </div>
      <div className="sub" style={{ margin: 0 }}>
        {t('插件是运行在面板之外的独立服务，通过 API 令牌和 Webhook 与面板对接：支付与发卡、通知渠道、用户同步等。开发方法见')}{' '}
        <a href={PLUGIN_DOCS} target="_blank" rel="noreferrer">{t('插件开发文档')}</a>{t('。只安装你信任的插件，它能做的事由下面列出的权限决定。')}
      </div>
      {full && <div className="note warn">{t('社区版最多安装 {n} 个插件。需要更多请在许可证页面激活专业版。', { n: limit })} <Link to="/license">{t('查看许可证')}</Link></div>}
      {plugins.length === 0 && <div className="hint">{t('还没有安装插件。')}</div>}
      {plugins.map((p) => {
        const m = p.manifest
        return (
          <div key={p.id} style={{ borderTop: '1px solid var(--line)', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}>
            <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
              <b>{p.name}</b> <span className="tag mono">{p.version}</span> {statusTag(p)}
              {m.author && <span className="small mute">{m.author}</span>}
              <span className="spacer" />
              {m.settings?.length ? <button className="btn sm" type="button" onClick={() => setSettings(p)}>{t('设置')}</button> : null}
              {(m.pages ?? []).map((pg) => <Link key={pg.id} className="btn sm" to={`/plugins/${p.id}/${pg.id}`}>{pg.title}</Link>)}
              <button className="btn sm" type="button" disabled={busy} onClick={() => refresh(p)}>{t('检查更新')}</button>
              <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await put(`/plugins/${p.id}`, { enabled: !p.enabled }); await refreshPlugins() })}>{p.enabled ? t('停用') : t('启用')}</button>
              <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
                if (await confirm(t('卸载插件 {name}？它的 API 令牌和 Webhook 会一并删除，插件里的数据不受影响。', { name: p.name }), { danger: true, ok: t('卸载') })) run(async () => { await del('/plugins/' + p.id); await refreshPlugins() }, t('已卸载'))
              }}>{t('卸载')}</button>
            </div>
            {m.description && <div className="small mute">{m.description}</div>}
            <div className="hint mono" style={{ wordBreak: 'break-all' }}>{p.baseUrl}</div>
            <div className="hint">
              {(m.scopes ?? []).length ? (m.scopes ?? []).map((s) => labels[s] ?? s).join('、') : t('不调用面板接口')}
              {' · '}{(m.events ?? []).length ? t('订阅 {n} 类事件', { n: m.events!.length }) : t('不接收事件')}
              {(m.subscription?.formats ?? []).length > 0 && ' · ' + t('订阅格式 {list}', { list: m.subscription!.formats!.map((f) => f.name).join('、') })}
              {p.lastSeenAt ? ' · ' + t('最近应答 {when}', { when: ago(p.lastSeenAt) }) : ''}
            </div>
            {p.lastError && p.status !== 'online' && <div className="hint" style={{ color: 'var(--bad)', wordBreak: 'break-all' }}>{p.lastError}</div>}
          </div>
        )
      })}
      {installing && <InstallModal onClose={() => setInstalling(false)} onDone={(r) => { setInstalling(false); if (r.credentials) setCreds(r) }} />}
      {settings && <SettingsModal p={plugins.find((x) => x.id === settings.id) ?? settings} onClose={() => setSettings(null)} />}
      {creds && <CredentialsModal r={creds} onClose={() => setCreds(null)} />}
    </section>
  )
}

function InstallModal({ onClose, onDone }: { onClose: () => void; onDone: (r: InstallResult) => void }) {
  const [url, setUrl] = useState('')
  const [key, setKey] = useState('')
  const [pv, setPv] = useState<Preview | null>(null)
  const { busy, run } = useAction()
  const labels = scopeLabel()
  const m = pv?.manifest
  return (
    <Modal title={t('安装插件')} wide onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      {!pv
        ? <button className="btn pri" type="button" disabled={busy || !url.trim()} onClick={() => run(async () => { setPv(await post<Preview>('/plugins/preview', { baseUrl: url.trim() })) })}>{t('读取插件信息')}</button>
        : <button className="btn pri" type="button" disabled={busy || pv.installed} onClick={() => run(async () => {
          const r = await post<InstallResult>('/plugins', { baseUrl: pv.baseUrl, installKey: key.trim() })
          await refreshPlugins(); onDone(r)
        }, t('已安装'))}>{t('授权并安装')}</button>}
    </>}>
      <div style={{ display: 'grid', gap: 12 }}>
        <Field label={t('插件地址')} hint={t('插件服务的网址，面板会读取它的 /polaris-plugin.json。和面板在同一台服务器上时可以填 http://127.0.0.1:端口')}>
          <input className="input mono" value={url} placeholder="https://" disabled={!!pv} onChange={(e) => setUrl(e.target.value)} />
        </Field>
        <Field label={t('安装密钥（可选）')} hint={t('插件要求时填写，会随安装请求发给插件')}>
          <input className="input mono" type="password" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)} />
        </Field>
        {m && (
          <div className="card" style={{ background: 'var(--soft)', display: 'grid', gap: 8 }}>
            <div><b>{m.name}</b> <span className="tag mono">{m.version}</span>{m.author && <span className="small mute"> · {m.author}</span>}</div>
            {m.description && <div className="small">{m.description}</div>}
            {pv.installed && <div className="note warn">{t('这个插件已经安装。')}</div>}
            <div className="small"><b>{t('它将获得的权限')}</b>：{(m.scopes ?? []).length ? (m.scopes ?? []).map((s) => labels[s] ?? s).join('、') : t('无（不调用面板接口）')}</div>
            <div className="small"><b>{t('它会收到的事件')}</b>：{(m.events ?? []).length ? m.events!.join('、') : t('无')}</div>
            {(m.pages ?? []).length > 0 && <div className="small"><b>{t('后台页面')}</b>：{m.pages!.map((p) => p.title).join('、')}</div>}
            {(m.portal ?? []).length > 0 && <div className="small"><b>{t('用户中心入口')}</b>：{m.portal!.map((p) => p.title).join('、')}</div>}
            {(m.subscription?.formats ?? []).length > 0 && <div className="note warn small"><b>{t('订阅格式')}</b>：{m.subscription!.formats!.map((f) => `${f.title}（?format=${f.name}）`).join('、')}<br />{t('用户使用这些格式时，面板会把该用户的节点（包括 UUID、密码等连接凭据）发给插件生成配置。请只安装可信的插件。')}</div>}
            <div className="hint">{t('安装后面板会为它创建专用的 API 令牌和事件 Webhook，并通过 /polaris/install 把凭据交给它。')}</div>
          </div>
        )}
      </div>
    </Modal>
  )
}

function SettingsModal({ p, onClose }: { p: PluginInfo; onClose: () => void }) {
  const fields = p.manifest.settings ?? []
  const [vals, setVals] = useState<Record<string, string>>(() => ({ ...p.values }))
  const { busy, run } = useAction()
  const set = (k: string, v: string) => setVals((x) => ({ ...x, [k]: v }))
  return (
    <Modal title={t('{name} 设置', { name: p.name })} wide onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        const r = await put<{ pushed: boolean; pushError: string }>(`/plugins/${p.id}/settings`, { values: vals })
        await refreshPlugins()
        if (!r.pushed) throw new Error(t('已保存，但没能通知插件：{err}。插件重启后会自行读取。', { err: r.pushError }))
        onClose()
      }, t('已保存并通知插件'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        {fields.map((f) => (
          <Field key={f.key} label={f.label + (f.required ? ' *' : '')} hint={f.type === 'secret' && p.hasSecret[f.key] ? t('已设置，留空不修改') : f.hint} full={f.type === 'text'}>
            {f.type === 'bool'
              ? <label className="chk"><input type="checkbox" checked={vals[f.key] === 'true'} onChange={(e) => set(f.key, e.target.checked ? 'true' : 'false')} />{t('启用')}</label>
              : f.type === 'select'
                ? <select className="input" value={vals[f.key] ?? ''} onChange={(e) => set(f.key, e.target.value)}><option value="">—</option>{(f.options ?? []).map((o) => <option key={o} value={o}>{o}</option>)}</select>
                : f.type === 'text'
                  ? <textarea className="input" rows={4} value={vals[f.key] ?? ''} onChange={(e) => set(f.key, e.target.value)} />
                  : <input className={'input' + (f.type === 'secret' || f.type === 'url' ? ' mono' : '')} type={f.type === 'secret' ? 'password' : f.type === 'number' ? 'number' : 'text'} autoComplete="off"
                    value={vals[f.key] ?? ''} onChange={(e) => set(f.key, e.target.value)} />}
          </Field>
        ))}
      </div>
    </Modal>
  )
}

function CredentialsModal({ r, onClose }: { r: InstallResult; onClose: () => void }) {
  const c = r.credentials!
  const rows: [string, string][] = [[t('面板 API 地址'), c.apiBase], [t('API 令牌'), c.token], [t('插件密钥'), c.pluginSecret]]
  if (c.webhookSecret) rows.push([t('Webhook 签名密钥'), c.webhookSecret])
  return (
    <Modal title={t('把凭据填到插件里')} wide onClose={onClose} footer={<button className="btn pri" type="button" onClick={onClose}>{t('我已保存')}</button>}>
      <div className="note warn" style={{ marginBottom: 10 }}>{t('插件已安装，但它没有自动接收凭据：{detail}', { detail: r.detail ?? '' })}</div>
      <div className="sub" style={{ margin: '0 0 10px' }}>{t('请把下面的内容配置到插件里。这些凭据')}<b>{t('只显示这一次')}</b>{t('。插件上线后状态会自动变为「运行中」。')}</div>
      <div style={{ display: 'grid', gap: 8 }}>
        {rows.map(([k, v]) => (
          <div key={k}>
            <div className="small mute">{k}</div>
            <div className="row" style={{ flexWrap: 'nowrap' }}><code style={{ flex: 1, wordBreak: 'break-all' }}>{v}</code><button className="btn sm" type="button" onClick={() => copyText(v)}>{t('复制')}</button></div>
          </div>
        ))}
      </div>
    </Modal>
  )
}
