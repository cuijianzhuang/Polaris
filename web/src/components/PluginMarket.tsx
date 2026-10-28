import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { get, put } from '../api'
import { ago } from '../format'
import { t } from '../i18n'
import { PLUGIN_DOCS } from './PluginManager'
import { scopeLabel } from './SecurityCards'
import { copyText, Field, Modal, useAction, useLoad, useToast } from './ui'

type Entry = {
  id: string; name: string; description: string; author: string; repo: string; homepage?: string; language?: string
  type: 'plugin' | 'tool'; kind: 'official' | 'example' | 'community'; category: string
  scopes: string[]; events: string[]; unknownScopes?: string[]; minPanel?: string
  deploy?: { command?: string; docker?: string; port?: number }
  installed: boolean; compatible: boolean
}
type Registry = { url: string; custom: boolean; defaultUrl: string; fetchedAt: string; plugins: Entry[]; skipped: number; version: string; error?: string }

const categories = (): Record<string, string> => ({
  notify: t('通知'), billing: t('支付与计费'), subscription: t('订阅'), monitor: t('监控'), cdn: 'CDN', users: t('用户'), other: t('其他'),
})
const kindTag = (k: Entry['kind']) => k === 'official'
  ? <span className="tag ok">{t('官方')}</span>
  : k === 'example' ? <span className="tag">{t('示例')}</span> : <span className="tag warn">{t('社区')}</span>

// PluginMarket browses the plugin registry. It is a catalogue only: nothing
// is downloaded or run; installing reads the deployed plugin's own manifest.
export default function PluginMarket() {
  const [q, setQ] = useState('')
  const [cat, setCat] = useState('')
  const [deploy, setDeploy] = useState<Entry | null>(null)
  const [source, setSource] = useState(false)
  const [refresh, setRefresh] = useState(0)
  const reg = useLoad(() => get<Registry>('/plugin-registry' + (refresh ? '?refresh=1' : '')), [refresh])
  const [, setParams] = useSearchParams()
  const labels = scopeLabel()
  const cats = categories()
  const r = reg.data
  const shown = useMemo(() => {
    const words = q.trim().toLowerCase()
    return (r?.plugins ?? []).filter((p) => (!cat || p.category === cat) &&
      (!words || [p.name, p.description, p.author, p.id, p.language ?? ''].some((s) => s.toLowerCase().includes(words))))
  }, [r, q, cat])
  const present = useMemo(() => new Set((r?.plugins ?? []).map((p) => p.category)), [r])

  const install = (p: Entry) => setParams({ tab: 'plugins', install: p.deploy?.port ? `http://127.0.0.1:${p.deploy.port}` : 'https://' }, { replace: true })

  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle">
        <span>{t('插件市场')} {r && <span className="tag" style={{ marginLeft: 6 }}>{r.plugins.length}</span>}</span>
        <span className="row" style={{ gap: 6 }}>
          <button className="btn sm" type="button" disabled={reg.loading} onClick={() => setRefresh((n) => n + 1)}>{t('刷新')}</button>
          <button className="btn sm" type="button" onClick={() => setSource(true)}>{t('目录地址')}</button>
        </span>
      </div>
      <div className="sub" style={{ margin: 0 }}>
        {t('来自在线插件目录。插件是独立部署的服务：先按「部署」里的说明在服务器上运行它，再点「安装」填入地址。面板不会下载或运行目录里的任何程序；安装时以插件自己声明的权限为准，需要你再次确认。')}{' '}
        <a href={PLUGIN_DOCS + '/tree/main/registry'} target="_blank" rel="noreferrer">{t('提交你的插件')}</a>
      </div>
      {r?.custom && <div className="note warn small">{t('正在使用自定义目录：{url}。目录里的插件不会标记为「官方」。', { url: r.url })}</div>}
      {r?.error && <div className="note warn small" style={{ wordBreak: 'break-all' }}>{r.error}{r.plugins.length > 0 ? ' ' + t('（下面是上次读取的结果）') : ''}</div>}
      {reg.error && <div className="note warn small">{reg.error}</div>}
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <input className="input" style={{ flex: '1 1 180px', minWidth: 0 }} value={q} placeholder={t('搜索名称、作者、说明')} aria-label={t('搜索插件')} onChange={(e) => setQ(e.target.value)} />
        <select className="input" style={{ flex: '0 0 auto', width: 'auto' }} value={cat} aria-label={t('分类')} onChange={(e) => setCat(e.target.value)}>
          <option value="">{t('全部分类')}</option>
          {Object.entries(cats).filter(([k]) => present.has(k)).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
        </select>
      </div>
      {reg.loading && !r && <div className="hint">{t('正在读取插件目录…')}</div>}
      {r && shown.length === 0 && <div className="hint">{r.plugins.length ? t('没有符合条件的插件。') : t('目录里还没有插件。')}</div>}
      <div style={{ display: 'grid', gap: 10, gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 300px), 1fr))' }}>
        {shown.map((p) => (
          <div key={p.id} className="card" style={{ background: 'var(--soft)', display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0 }}>
            <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>
              <b style={{ overflowWrap: 'anywhere' }}>{p.name}</b>
              {kindTag(p.kind)}
              <span className="tag">{p.type === 'tool' ? t('独立程序') : t('插件')}</span>
              {p.installed && <span className="tag ok"><span className="dot" />{t('已安装')}</span>}
            </div>
            <div className="small mute">{[p.author, p.language, cats[p.category]].filter(Boolean).join(' · ')}</div>
            {p.description && <div className="small" style={{ overflowWrap: 'anywhere' }}>{p.description}</div>}
            <div className="hint" style={{ overflowWrap: 'anywhere' }}>
              <b>{p.type === 'tool' ? t('需要的令牌权限') : t('申请的权限')}</b>：{p.scopes.length ? p.scopes.map((s) => labels[s] ?? s).join('、') : t('无')}
              {p.events.length > 0 && ' · ' + t('订阅 {n} 类事件', { n: p.events.length })}
            </div>
            {(p.unknownScopes ?? []).length > 0 && <div className="hint" style={{ color: 'var(--warn)' }}>{t('本面板不认识的权限：{list}', { list: p.unknownScopes!.join('、') })}</div>}
            {!p.compatible && <div className="hint" style={{ color: 'var(--bad)' }}>{t('需要面板 {need} 或更高版本（当前 {have}）', { need: p.minPanel ?? '', have: r!.version })}</div>}
            <div className="row" style={{ gap: 6, flexWrap: 'wrap', marginTop: 'auto' }}>
              <a className="btn sm" href={p.repo} target="_blank" rel="noreferrer">{t('源码')}</a>
              {p.homepage && <a className="btn sm" href={p.homepage} target="_blank" rel="noreferrer">{t('主页')}</a>}
              {p.deploy && <button className="btn sm" type="button" onClick={() => setDeploy(p)}>{t('部署')}</button>}
              <span className="spacer" />
              {p.type === 'tool'
                ? <button className="btn sm pri" type="button" onClick={() => setParams({ tab: 'tokens' }, { replace: true })}>{t('创建令牌')}</button>
                : <button className="btn sm pri" type="button" disabled={p.installed || !p.compatible} onClick={() => install(p)}>{t('安装')}</button>}
            </div>
          </div>
        ))}
      </div>
      {r && <div className="hint" style={{ wordBreak: 'break-all' }}>{t('目录：{url}', { url: r.url })} · {t('读取于 {when}', { when: ago(r.fetchedAt) })}{r.skipped > 0 && ' · ' + t('{n} 项格式不对，已忽略', { n: r.skipped })}</div>}
      {deploy && <DeployModal p={deploy} onClose={() => setDeploy(null)} onInstall={() => { setDeploy(null); if (deploy.type === 'tool') setParams({ tab: 'tokens' }, { replace: true }); else install(deploy) }} />}
      {source && r && <SourceModal r={r} onClose={() => setSource(false)} onSaved={() => { setSource(false); setRefresh((n) => n + 1) }} />}
    </section>
  )
}

function DeployModal({ p, onClose, onInstall }: { p: Entry; onClose: () => void; onInstall: () => void }) {
  const toast = useToast()
  const d = p.deploy ?? {}
  const copy = async (s: string) => { await copyText(s); toast(t('已复制')) }
  return (
    <Modal title={t('部署 {name}', { name: p.name })} wide onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('关闭')}</button>
      <button className="btn pri" type="button" disabled={p.installed || !p.compatible} onClick={onInstall}>{p.type === 'tool' ? t('去创建令牌') : t('已部署，去安装')}</button>
    </>}>
      <div style={{ display: 'grid', gap: 12 }}>
        <div className="note small">{t('这些命令来自插件目录，运行前请先看一遍源码和命令内容。<…> 是需要你替换的部分。')}</div>
        {d.command && <Field label={t('部署命令')}>
          <pre className="mono small" style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', margin: 0, padding: 10, background: 'var(--soft)', borderRadius: 8 }}>{d.command}</pre>
          <div><button className="btn sm" type="button" onClick={() => copy(d.command!)}>{t('复制命令')}</button></div>
        </Field>}
        {d.docker && <Field label={t('Docker 镜像')}>
          <div className="row" style={{ gap: 6 }}><code className="mono small" style={{ overflowWrap: 'anywhere' }}>{d.docker}</code><button className="btn sm" type="button" onClick={() => copy(d.docker!)}>{t('复制')}</button></div>
        </Field>}
        {p.type === 'plugin'
          ? <div className="hint">{d.port ? t('默认监听 {port} 端口。和面板在同一台服务器上时，安装地址填 http://127.0.0.1:{port}；在其他服务器上请填它的公网地址。', { port: d.port }) : t('运行后，在「安装」里填入插件服务的网址。')}</div>
          : <div className="hint">{t('这是独立程序，不在面板里安装：在「API 令牌」里创建一个只带上面这些权限的令牌，部署时填进去即可。')}</div>}
        <div className="hint">{t('完整说明：')}<a href={p.homepage || p.repo} target="_blank" rel="noreferrer">{p.homepage || p.repo}</a></div>
      </div>
    </Modal>
  )
}

function SourceModal({ r, onClose, onSaved }: { r: Registry; onClose: () => void; onSaved: () => void }) {
  const [url, setUrl] = useState(r.custom ? r.url : '')
  const { busy, run } = useAction()
  return (
    <Modal title={t('插件目录地址')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      {r.custom && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await put('/plugin-registry', { url: '' }); onSaved() }, t('已恢复默认目录'))}>{t('恢复默认')}</button>}
      <button className="btn pri" type="button" disabled={busy || !url.trim()} onClick={() => run(async () => { await put('/plugin-registry', { url: url.trim() }); onSaved() }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <Field label={t('目录地址')} hint={t('https:// 开头的 JSON 文件，格式与官方目录相同。留空使用默认目录：{url}', { url: r.defaultUrl })}>
        <input className="input mono" value={url} placeholder={r.defaultUrl} onChange={(e) => setUrl(e.target.value)} />
      </Field>
    </Modal>
  )
}
