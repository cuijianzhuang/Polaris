import { useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, del, get, post } from '../api'
import { useNodes } from '../App'
import { Empty, Field, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { t } from '../i18n'
import { Icon } from '../components/Icons'
import './Websites.css'
import type { Certificate } from '../types'

type Site = {
  id: string; nodeId: string; domain: string; port: number; certificateId: string; enabled: boolean
  path: string; mode: 'static' | 'proxy'; upstream: string; panelUpstream: string
  adminPath: string; subPath: string; portalPath: string; version: number
  files?: Record<string, string>; nodePaths?: Record<string, string>
}
type SiteView = { site: Site; fileCount: number; size: number; digest: string }
type Route = { kind: string; path: string; upstream: string; inboundId?: string }
type Preview = { routes: Route[]; warnings: string[]; errors: string[]; portState: string; fileCount: number; size: number }
type Listing = { sites: SiteView[]; adminPath: string; subPath: string; portalPath: string }
const kinds: Record<string, string> = { 'panel-service': t('面板服务'), website: t('网站'), admin: t('管理后台'), subscription: t('订阅'), portal: t('用户中心'), node: t('节点') }
const sizeLabel = (size: number) => size < 1024 ? `${size} B` : size < 1024 * 1024 ? `${(size / 1024).toFixed(1)} KiB` : `${(size / (1024 * 1024)).toFixed(1)} MiB`

export default function Websites() {
  const list = useLoad(() => get<Listing>('/websites'))
  const certs = useLoad(() => get<Certificate[]>('/certificates'))
  const { nodes, refresh } = useNodes()
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const [form, setForm] = useState<Site | null>(null)
  const [preview, setPreview] = useState<Preview | null>(null)
  const [routes, setRoutes] = useState<Route[]>([])
  const uploadInput = useRef<HTMLInputElement>(null)
  const [uploadName, setUploadName] = useState('')
  const [uploadError, setUploadError] = useState('')
  const [uploading, setUploading] = useState(false)
  const [dragOver, setDragOver] = useState(false)
  const [bundle, setBundle] = useState<{ fileCount: number; size: number } | null>(null)
  const set = (patch: Partial<Site>) => {
    setForm((f) => f ? { ...f, ...patch } : null); setPreview(null)
    if (patch.nodeId !== undefined || patch.domain !== undefined) { setRoutes([]); setForm((f) => f ? { ...f, nodePaths: {} } : null) }
  }
  const edit = (view?: SiteView) => {
    setPreview(null); setRoutes([]); setUploadName(''); setUploadError(''); setBundle(view ? { fileCount: view.fileCount, size: view.size } : null)
    setForm(view ? { ...view.site, files: undefined, nodePaths: {} } : {
      id: '', nodeId: '', domain: '', port: 443, certificateId: '', enabled: true, path: '/', mode: 'static', upstream: '',
      panelUpstream: '', adminPath: '', subPath: '', portalPath: '', version: 0, nodePaths: {},
    })
  }
  const upload = (file: File) => {
    if (busy) return
    setUploadError(''); setDragOver(false); setPreview(null)
    if (!file.name.toLowerCase().endsWith('.zip') || file.size > 4 * 1024 * 1024) { setUploadError(t('请选择不超过 4 MiB 的 ZIP 文件。')); return }
    setUploading(true)
    run(async () => {
      try {
        const result = await api<{ files: Record<string, string>; fileCount: number; size: number }>('POST', '/websites/archive', file)
        set({ files: result.files }); setBundle({ fileCount: result.fileCount, size: result.size }); setUploadName(file.name)
      } catch (e) { setUploadError(e instanceof Error ? e.message : t('上传失败，请重试。')); throw e }
      finally { setUploading(false) }
    })
  }
  const close = async () => { if (!form || await confirm(t('关闭编辑？未保存的网站配置将丢失。'))) setForm(null) }
  const loadPreview = () => run(async () => {
    const p = await post<Preview>('/websites/preview', form)
    setPreview(p); setRoutes(p.routes.filter((r) => r.kind === 'node'))
  })
  return <>
    <div className="top"><div><h1>{t('网站与 443 入口')}</h1><div className="sub">{t('同一节点上的网站、后台、订阅和 CDN 节点按路径共享 HTTPS 443。')}</div></div><button className="btn pri" type="button" disabled={busy} onClick={() => edit()}>{t('+ 网站入口')}</button></div>
    <div className="note">{t('使用 Agent 内置 Caddy。已有 Nginx、宝塔或独立 Caddy 占用 443 时，请先处理端口冲突。节点间的 Agent mTLS 接入方式保持独立。')}</div>
    {list.error && <p role="alert" className="note bad">{list.error}</p>}
    {list.data?.sites.length === 0 && !form && <Empty>{t('创建入口，上传静态网站或连接本机博客服务，再预览全部分流规则。')}</Empty>}
    <div className="cards">{list.data?.sites.map((view) => {
      const w = view.site, n = nodes.find((n) => n.id === w.nodeId)
      return <section className="card" key={w.id}><div className="row"><strong className="mono" style={{ overflowWrap: 'anywhere' }}>{w.domain}:443</strong><span className="sp" /><span className="tag">{w.enabled ? t('已启用') : t('已停用')}</span></div>
        <p className="sub">{n?.name ?? w.nodeId} · {w.path} · {w.mode === 'static' ? t('静态网站') : t('本机服务反代')}{w.mode === 'static' ? ` · ${view.fileCount} ${t('个文件')} · ${sizeLabel(view.size)}` : ` · ${w.upstream}`}</p>
        <p className="small mute">{!w.enabled ? t('网站入口停用；已有 CDN 节点按原配置运行。') : !n || n.status === 'offline' ? t('节点离线，等待应用') : n.lastError ? n.lastError : n.desiredRev > 0 && n.appliedRev === n.desiredRev ? t('节点已应用当前配置') : t('等待节点应用配置')}</p>
        <div className="row"><button className="btn sm" type="button" disabled={busy} onClick={() => edit(view)}>{t('编辑')}</button><Link className="btn sm" to={`/nodes?id=${encodeURIComponent(w.nodeId)}`}>{t('查看节点')}</Link><button className="btn sm" type="button" disabled={busy} onClick={async () => {
          if (await confirm(t('删除网站入口 {domain}？网站、后台和订阅的此入口将停止服务；已有 CDN 节点保留。', { domain: w.domain }), { danger: true })) run(async () => { await del('/websites/' + w.id); if (form?.id === w.id) setForm(null); list.reload(); refresh() }, t('网站入口已删除'))
        }}>{t('删除')}</button></div>
      </section>
    })}</div>
    {form && <section className="card" style={{ display: 'grid', gap: 16 }}>
      <div className="row"><h2 style={{ margin: 0 }}>{form.id ? t('编辑网站入口') : t('创建网站入口')}</h2><span className="sp" /><button type="button" className="btn sm" disabled={busy} onClick={close}>{t('关闭编辑')}</button></div>
      <div className="form-grid">
        <Field label={t('目标节点')} htmlFor="website-node"><select id="website-node" className="input" value={form.nodeId} disabled={busy || !!form.id} onChange={(e) => set({ nodeId: e.target.value })}><option value="">{t('选择节点')}</option>{nodes.filter((n) => !n.revoked).map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select></Field>
        <Field label={t('网站域名')} htmlFor="website-domain"><input id="website-domain" className="input mono" value={form.domain} disabled={busy} placeholder="site.example.com" onChange={(e) => set({ domain: e.target.value })} /></Field>
        <Field label={t('入口证书')} htmlFor="website-cert" hint={t('证书须覆盖网站域名；同域名的节点入口使用这张证书。')}><select id="website-cert" className="input" value={form.certificateId} disabled={busy} onChange={(e) => set({ certificateId: e.target.value })}><option value="">{t('选择证书')}</option>{certs.data?.map((c) => <option value={c.id} key={c.id}>{c.domain} · {c.provider}</option>)}</select></Field>
        <Field label={t('网站根路径')} htmlFor="website-path" hint={t('默认 /；子路径例如 /blog。博客程序需支持所选路径。')}><input id="website-path" className="input mono" value={form.path} disabled={busy} onChange={(e) => set({ path: e.target.value })} /></Field>
      </div>
      <div className="toggle"><span>{t('启用网站入口')}</span><Switch label={t('启用网站入口')} on={form.enabled} disabled={busy} onChange={(enabled) => set({ enabled })} /></div>
      <Field label={t('网站托管方式')} htmlFor="website-mode"><select id="website-mode" className="input" value={form.mode} disabled={busy} onChange={(e) => set({ mode: e.target.value as Site['mode'] })}><option value="static">{t('上传静态网站')}</option><option value="proxy">{t('反代本机博客服务')}</option></select></Field>
      {form.mode === 'static' ? <div className="website-bundle">
        <span className="lbl">{t('静态网站包')}</span>
        <div className={'website-upload' + (dragOver ? ' dragging' : '') + (uploadError ? ' invalid' : '')} aria-busy={uploading}
          onDragOver={(e) => { e.preventDefault(); if (!busy) setDragOver(true) }}
          onDragLeave={(e) => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragOver(false) }}
          onDrop={(e) => { e.preventDefault(); const file = e.dataTransfer.files[0]; if (file) upload(file) }}>
          <div className="website-upload-main">
            <span className="website-upload-icon" aria-hidden="true">{bundle ? <Icon.Package /> : <Icon.Upload />}</span>
            <div className="website-upload-copy">
              <strong>{uploading ? t('正在上传并检查…') : uploadName || (bundle ? t('已保存的网站包') : t('上传你的静态网站'))}</strong>
              <span>{bundle ? t('网站文件：{count} 个，共 {size}', { count: bundle.fileCount, size: sizeLabel(bundle.size) }) : t('拖放 ZIP 到这里，或点击选择文件')}</span>
            </div>
            <button className="btn sm" type="button" disabled={busy} onClick={() => uploadInput.current?.click()}>{uploading ? t('上传中…') : bundle ? t('更换 ZIP') : t('选择 ZIP')}</button>
          </div>
          <input ref={uploadInput} id="website-archive" type="file" accept=".zip,application/zip" hidden aria-label={t('选择静态网站 ZIP')} disabled={busy} onChange={(e) => { const file = e.target.files?.[0]; e.target.value = ''; if (file) upload(file) }} />
          <div className="website-upload-footer" role="status">
            <span className={'website-upload-state' + (bundle && !uploadError && !uploading ? ' ready' : '')}>{uploadError ? t('上传未完成') : uploading ? t('正在验证文件') : bundle ? t('网站包已就绪') : t('仅支持 ZIP')}</span>
            <span>{bundle ? t('保存并发布后生效') : t('上传上限 4 MiB')}</span>
          </div>
        </div>
        {uploadError && <p className="website-upload-error" role="alert">{uploadError}</p>}
        <div className="website-upload-hint"><span>{t('根目录包含 index.html，解压后不超过 8 MiB。')}</span><details className="website-upload-help"><summary>{t('打包要求')}</summary><p>{t('最多 2048 个文件；请打包网站目录内的文件。不支持隐藏文件、符号链接或服务端脚本执行。')}</p></details></div>
      </div> : <Field label={t('本机博客地址')} htmlFor="website-upstream" hint={t('填写目标节点上的 HTTP 回环地址，例如 http://127.0.0.1:3000。')}><input id="website-upstream" className="input mono" value={form.upstream} disabled={busy} onChange={(e) => set({ upstream: e.target.value })} placeholder="http://127.0.0.1:3000" /></Field>}
      <h2 style={{ marginBottom: 0 }}>{t('面板与订阅分流')}</h2>
      <Field label={t('本机面板地址')} htmlFor="website-panel" hint={t('可选。填写目标节点实际能访问的面板 HTTP 回环地址；面板不在此节点时请留空。')}><input id="website-panel" className="input mono" value={form.panelUpstream} disabled={busy} onChange={(e) => set({ panelUpstream: e.target.value, ...(e.target.value ? {} : { adminPath: '', subPath: '', portalPath: '' }) })} placeholder="http://127.0.0.1:8080" /></Field>
      {form.panelUpstream && <div className="form-grid">
        <Field label={t('后台路径')} htmlFor="website-admin" hint={t('与系统「面板访问」的私密路径一致。根路径后台请先改为私密路径。')}><select id="website-admin" className="input" disabled={busy} value={form.adminPath} onChange={(e) => set({ adminPath: e.target.value })}><option value="">{t('不转发')}</option>{list.data?.adminPath && <option value={list.data.adminPath}>{list.data.adminPath}</option>}</select></Field>
        <Field label={t('订阅路径')} htmlFor="website-sub"><select id="website-sub" className="input" disabled={busy} value={form.subPath} onChange={(e) => set({ subPath: e.target.value })}><option value="">{t('不转发')}</option><option value="/sub">/sub</option></select></Field>
        <Field label={t('用户中心路径')} htmlFor="website-portal"><select id="website-portal" className="input" disabled={busy} value={form.portalPath} onChange={(e) => set({ portalPath: e.target.value })}><option value="">{t('不转发')}</option><option value="/u">/u</option></select></Field>
      </div>}
      <h2 style={{ marginBottom: 0 }}>{t('节点路径')}</h2>
      <p className="sub" style={{ margin: 0 }}>{t('预览会读取该节点同域名、443 的 Caddy 统一回源入站。可调整其路径；保存时同步更新入站及入口，客户端需要更新订阅。')} <Link to="/inbounds">{t('管理入站')}</Link></p>
      {routes.map((r) => <Field key={r.inboundId} label={r.upstream} htmlFor={`website-route-${r.inboundId}`}><input id={`website-route-${r.inboundId}`} className="input mono" value={form.nodePaths?.[r.inboundId!] ?? r.path} disabled={busy} onChange={(e) => set({ nodePaths: { ...form.nodePaths, [r.inboundId!]: e.target.value } })} /></Field>)}
      <div className="row"><button className="btn" type="button" disabled={busy || !form.nodeId || !form.domain || !form.certificateId} onClick={loadPreview}>{t('检查冲突并预览')}</button><span className="small mute">{t('修改配置后需重新预览，节点应用时还会校验端口。')}</span></div>
      {preview && <div aria-live="polite" style={{ minWidth: 0 }}>
        {preview.errors.map((e) => <p className="note bad" role="alert" key={e}>{e}</p>)}
        {preview.warnings.map((e) => <p className="note warn" key={e}>{e}</p>)}
        <p>{t('443 端口状态：')}{({ free: t('当前空闲'), managed: t('Agent Caddy 已管理'), occupied: t('其他程序占用'), unknown: t('待节点确认') })[preview.portState]}</p>
        <div className="scroll"><table><thead><tr><th>{t('用途')}</th><th>{t('请求路径')}</th><th>{t('转发目标')}</th></tr></thead><tbody>{preview.routes.map((r) => <tr key={r.kind + r.path}><td>{kinds[r.kind]}</td><td className="mono">{r.path === '/' ? '/*' : r.path + ' / ' + r.path + '/*'}</td><td className="mono" style={{ overflowWrap: 'anywhere' }}>{r.upstream}</td></tr>)}</tbody></table></div>
      </div>}
      <div className="row"><button className="btn pri" type="button" disabled={busy || !preview || !!preview.errors.length} onClick={() => run(async () => { await post('/websites', form); setForm(null); list.reload(); refresh() }, t('网站配置已保存，等待节点校验并应用'))}>{t('保存并发布')}</button><button className="btn" type="button" disabled={busy} onClick={close}>{t('取消')}</button></div>
    </section>}
  </>
}
