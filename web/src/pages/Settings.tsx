import { useEffect, useState } from 'react'
import { del, download, get, post, put, setToken, takeOAuthResult, upload } from '../api'
import { LoginProtection, RecoveryCodesModal, Sessions } from '../components/SecurityCards'
import { Icon } from '../components/Icons'
import { accents, useTheme, type ThemeMode } from '../theme'
import { CopyBox, Empty, Field, QR, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { bytes, datetime } from '../format'

// DefaultVersion picks the version new nodes install: "latest" follows the
// newest stable release, or pin one of the five most recent ones.
function DefaultVersion({ label, type, value, onChange }: { label: string; type: string; value: string; onChange: (v: string) => void }) {
  const [vers, setVers] = useState<string[] | null>(null)
  useEffect(() => {
    let live = true
    get<{ versions: string[] }>('/core-versions?type=' + type).then((r) => { if (live) setVers(r.versions ?? []) }).catch(() => { if (live) setVers([]) })
    return () => { live = false }
  }, [type])
  const cur = value.replace(/^v/, '') || 'latest'
  const recent = (vers ?? []).slice(0, 5)
  const opts = cur !== 'latest' && !recent.includes(cur) ? [...recent, cur] : recent
  return (
    <Field label={label} hint={vers === null ? '正在获取版本列表…' : vers.length === 0 ? '无法获取版本列表（可配置 GitHub 镜像）' : '「自动」时新节点安装最新稳定版'}>
      <select className="input mono" value={cur} onChange={(e) => onChange(e.target.value)}>
        <option value="latest">自动（最新{recent[0] ? `：v${recent[0]}` : ''}）</option>
        {opts.map((v, i) => <option key={v} value={v}>v{v}{i === 0 && recent[0] === v ? ' · 最新' : ''}{!recent.includes(v) ? ' · 自定义' : ''}</option>)}
      </select>
    </Field>
  )
}

type BackupInfo = { supported: boolean; dbSize: number; dir: string; files: { name: string; size: number; created: string }[] }

// Backups: on-demand and scheduled archives (database + internal CA), download,
// upload-and-restore. A restore restarts the panel.
function Backups() {
  const info = useLoad(() => get<BackupInfo>('/backup'))
  const s = useLoad(() => get<Record<string, string>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  useEffect(() => {
    if (s.data) setF({ backup_schedule: s.data.backup_schedule || 'off', backup_hour: s.data.backup_hour || '4', backup_keep: s.data.backup_keep || '7', backup_tg: s.data.backup_tg || 'false' })
  }, [s.data])
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const [restoring, setRestoring] = useState(false)
  const afterRestore = () => {
    setRestoring(true)
    // The panel re-executes itself; wait for it to answer again.
    const t0 = Date.now()
    const ping = () => get('/backup').then(() => location.reload()).catch(() => { if (Date.now() - t0 < 60000) setTimeout(ping, 1500); else { setRestoring(false); toast('面板未在 60 秒内恢复，请检查服务状态', true) } })
    setTimeout(ping, 2500)
  }
  const restore = async (label: string, fn: () => Promise<unknown>) => {
    if (!await confirm(`用「${label}」覆盖当前全部数据并重启面板？\n当前数据会先自动备份一份；恢复后需要重新登录。`, { danger: true })) return
    run(async () => { await fn(); afterRestore() }, '备份校验通过，面板正在重启…')
  }
  if (info.data && !info.data.supported) return (
    <section className="card"><h2>备份与恢复</h2><div className="note">当前使用 PostgreSQL，请用 pg_dump 备份数据库。</div></section>
  )
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>备份与恢复</h2>
        <span className="sub" style={{ margin: 0 }}>数据库 {bytes(info.data?.dbSize)} · 保存在 <code>{info.data?.dir}</code></span>
      </div>
      {restoring && <div className="note warn"><span className="spinner" /> 正在应用备份并重启面板，完成后自动刷新…</div>}
      <div className="form-grid">
        <Field label="自动备份">
          <select className="input" value={f.backup_schedule ?? 'off'} onChange={(e) => setF({ ...f, backup_schedule: e.target.value })}>
            <option value="off">关闭</option><option value="daily">每天</option><option value="weekly">每周一</option>
          </select>
        </Field>
        <Field label="备份时间（点）" hint="服务器本地时间 0–23"><input className="input mono" inputMode="numeric" value={f.backup_hour ?? ''} onChange={(e) => setF({ ...f, backup_hour: e.target.value })} /></Field>
        <Field label="保留份数" hint="超出后删除最旧的"><input className="input mono" inputMode="numeric" value={f.backup_keep ?? ''} onChange={(e) => setF({ ...f, backup_keep: e.target.value })} /></Field>
        <div className="toggle" style={{ alignSelf: 'end' }}><span>自动备份发送到 Telegram（超级管理员）</span><Switch label="发送到 Telegram" on={f.backup_tg === 'true'} onChange={(v) => setF({ ...f, backup_tg: String(v) })} /></div>
      </div>
      <div className="row">
        <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', f); s.reload() }, '备份设置已保存')}>保存设置</button>
        <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await post('/backup'); info.reload() }, '已创建备份')}>立即备份</button>
        <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await post('/backup?telegram=1'); info.reload() }, '已创建并发送到 Telegram')}>备份并发送到 Telegram</button>
        <label className="btn" style={{ cursor: 'pointer' }}>
          上传备份并恢复
          <input type="file" accept=".gz,.tar.gz,.db" style={{ display: 'none' }} onChange={(e) => {
            const file = e.target.files?.[0]; e.target.value = ''
            if (file) restore(file.name, () => upload('/backup/restore', file))
          }} />
        </label>
      </div>
      <div className="card flush scroll">
        {info.data?.files.length === 0 ? <Empty>还没有备份</Empty> : (
          <table className="rtable">
            <thead><tr><th>文件</th><th>大小</th><th>时间</th><th></th></tr></thead>
            <tbody>{(info.data?.files ?? []).map((b) => (
              <tr key={b.name}>
                <td className="rt-head mono">{b.name}{b.name.includes('-auto') && <span className="tag" style={{ marginLeft: 6 }}>自动</span>}{b.name.includes('-prerestore') && <span className="tag warn" style={{ marginLeft: 6 }}>恢复前</span>}</td>
                <td data-label="大小" className="mono">{bytes(b.size)}</td>
                <td data-label="时间">{datetime(b.created)}</td>
                <td className="rt-actions"><div className="row" style={{ justifyContent: 'flex-end', gap: 6 }}>
                  <button className="btn sm" type="button" onClick={() => run(() => download('/backup/files/' + b.name, b.name))}>下载</button>
                  <button className="btn sm" type="button" disabled={busy} onClick={() => restore(b.name, () => post(`/backup/files/${b.name}/restore`))}>恢复</button>
                  <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(`删除备份 ${b.name}？`, { danger: true })) run(async () => { await del('/backup/files/' + b.name); info.reload() }) }}>删除</button>
                </div></td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </div>
      <div className="hint">备份包含数据库与内部 CA 证书（迁移到新服务器后已接入的节点无需重装）。数据用 secret_key 加密，恢复到新服务器时需配置相同的 POLARIS_SECRET_KEY。</div>
    </section>
  )
}

const settingTabs = [
  { id: 'general', label: '常规', hint: '访问地址、标题、默认内核', icon: <Icon.Gear /> },
  { id: 'security', label: '账号安全', hint: '密码、二次验证、登录设备、登录防护', icon: <Icon.Lock /> },
  { id: 'update', label: '版本与更新', hint: '在线升级、自动更新', icon: <Icon.Refresh /> },
  { id: 'backup', label: '备份与恢复', hint: '自动备份、恢复', icon: <Icon.Download /> },
  { id: 'appearance', label: '外观', hint: '主题与配色', icon: <Icon.Sun /> },
]

// Appearance is per browser: mode plus an accent palette.
function Appearance() {
  const [t, setT] = useTheme()
  const modes: { id: ThemeMode; label: string }[] = [{ id: 'light', label: '浅色' }, { id: 'dark', label: '深色' }, { id: 'system', label: '跟随系统' }]
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>外观</h2>
      <Field label="主题模式" hint="仅保存在当前浏览器">
        <div className="seg c3 fixed">
          {modes.map((m) => <button key={m.id} type="button" className={'opt' + (t.mode === m.id ? ' on' : '')} aria-pressed={t.mode === m.id} onClick={() => setT({ ...t, mode: m.id })}>{m.label}</button>)}
        </div>
      </Field>
      <Field label="主题色">
        <div className="swatches">
          {accents.map((a) => <button key={a.id} type="button" className={'accent-swatch' + (t.accent === a.id ? ' on' : '')} style={{ background: a.color }}
            title={a.label} aria-label={'主题色：' + a.label} aria-pressed={t.accent === a.id} onClick={() => setT({ ...t, accent: a.id })} />)}
        </div>
      </Field>
    </section>
  )
}

type UpdateInfo = { current: string; latest?: string; newer?: boolean; updater: boolean; pending?: boolean; error?: string }

// Updates: the running and latest versions, update now (panel, or panel and
// then every agent), and auto update. The panel restarts while updating;
// the outcome arrives as a Telegram message and in the audit log.
function Updates() {
  const info = useLoad(() => get<UpdateInfo>('/system/update?fresh=1'))
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [mode, setMode] = useState<string | null>(null)
  const [hour, setHour] = useState<string | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const m = mode ?? (s.data?.auto_update || 'notify')
  const h = hour ?? (s.data?.auto_update_hour || '4')
  const d = info.data
  const start = async (agents: boolean) => {
    if (!await confirm(`升级面板到 ${d?.latest}？${agents ? '\n完成后会接着升级所有在线节点的 Agent。' : ''}\n升级约需 1 分钟，期间面板会重启、网页短暂断开；结果会通过 Telegram 通知并记入审计日志。`)) return
    run(async () => { await post('/system/update', { agents }); info.reload() }, '已开始升级，稍后刷新页面')
  }
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>版本与更新</h2>
      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        <dt>当前版本</dt><dd className="mono">{d?.current ?? '…'}</dd>
        <dt>最新版本</dt><dd className="mono">{d?.latest ? <>{d.latest}{d.newer ? <span className="tag warn" style={{ marginLeft: 8 }}>可升级</span> : <span className="tag ok" style={{ marginLeft: 8 }}>已是最新</span>}</> : d?.error ? <span style={{ color: 'var(--bad)' }}>获取失败：{d.error}</span> : '…'}</dd>
      </dl>
      {d && !d.updater && <div className="note warn">这台服务器还没有安装在线升级组件：在服务器上执行 <code>polaris update --force</code> 安装（只需一次，之后的版本都能在这里或 Telegram 里一键升级）。Docker 部署请更新镜像。</div>}
      {d?.pending && <div className="note">升级进行中…完成后刷新页面。</div>}
      <div className="row">
        <button className="btn pri" type="button" disabled={busy || !d?.newer || !d?.updater || !!d?.pending} onClick={() => start(false)}>升级面板</button>
        <button className="btn" type="button" disabled={busy || !d?.newer || !d?.updater || !!d?.pending} onClick={() => start(true)}>升级面板和全部节点</button>
        <button className="btn" type="button" disabled={busy} onClick={() => info.reload()}>检查更新</button>
      </div>
      <div className="form-grid">
        <Field label="自动更新" hint="新版本发布后：通知会发给 Telegram 超级管理员（每个版本一次）">
          <select className="input" value={m} onChange={(e) => setMode(e.target.value)}>
            <option value="off">关闭</option>
            <option value="notify">只通知新版本</option>
            <option value="panel">自动升级面板</option>
            <option value="all">自动升级面板和全部节点</option>
          </select>
        </Field>
        <Field label="自动升级时间" hint="服务器本地时间；某个版本自动升级失败后不再自动重试，可手动升级">
          <select className="input mono" value={h} disabled={m !== 'panel' && m !== 'all'} onChange={(e) => setHour(e.target.value)}>
            {Array.from({ length: 24 }, (_, i) => <option key={i} value={String(i)}>{String(i).padStart(2, '0')}:41</option>)}
          </select>
        </Field>
      </div>
      <div><button className="btn pri" type="button" disabled={busy || (mode === null && hour === null)} onClick={() => run(async () => {
        await put('/settings', { auto_update: m, auto_update_hour: h }); setMode(null); setHour(null); s.reload()
      }, '已保存')}>保存</button></div>
    </section>
  )
}

type AccessInfo = { adminPath: string; publicUrl: string; listen: string; domain?: string; updater: boolean; pending?: boolean
  last?: { action: string; value: string; ok: boolean; log: string; at: string } }

const accessLabel: Record<string, string> = { 'set-path': '后台路径', 'set-domain': '面板域名', 'set-port': '面板端口', 'set-url': '公开地址' }

// Access: how the panel is reached (admin path, HTTPS domain, port, public
// address). These live in /etc/polaris/polaris.env; the change is carried
// out by the root side of the server (`polaris set-path` …), which restarts
// the panel, so the page then moves to the new address.
function Access() {
  const a = useLoad(() => get<AccessInfo>('/system/access'))
  const [f, setF] = useState<{ path?: string; domain?: string; port?: string; url?: string }>({})
  const [moving, setMoving] = useState<{ to: string; note: string } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const d = a.data
  const port = d?.listen.split(':').pop() ?? ''
  const base = (d?.publicUrl || location.origin).replace(/\/$/, '')
  const adminPath = d?.adminPath === '/' ? '' : d?.adminPath ?? ''
  const here = base + adminPath + '/'

  // After the restart: same-origin targets are polled, others opened after a pause.
  useEffect(() => {
    if (!moving) return
    let live = true
    const sameOrigin = new URL(moving.to).origin === location.origin
    const t0 = Date.now()
    const tick = async () => {
      if (!live) return
      if (sameOrigin) {
        try {
          const r = await fetch(moving.to + 'api/v1/info', { cache: 'no-store' })
          if (r.ok && Date.now() - t0 > 3000) { location.href = moving.to; return }
        } catch { /* restarting */ }
      } else if (Date.now() - t0 > 15000) { location.href = moving.to; return }
      setTimeout(tick, 1500)
    }
    setTimeout(tick, 2000)
    return () => { live = false }
  }, [moving])

  const apply = async (action: string, value: string, to: string, note: string, warn: string) => {
    if (!await confirm(`${warn}\n\n应用后面板会重启，页面将跳转到：\n${to}`, { ok: '应用' })) return
    const ok = await run(async () => { await post('/system/access', { action, value }); return true })
    if (ok) setMoving({ to, note })
  }

  if (moving) return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <h2><span className="spinner" style={{ marginRight: 8 }} />正在应用…</h2>
      <div className="sub" style={{ margin: 0 }}>{moving.note}</div>
      <div>新地址：<a className="mono" href={moving.to}>{moving.to}</a></div>
      <div className="hint">页面会自动跳转；如果一直打不开，等一会儿手动打开上面的地址，或在服务器上执行 <code>polaris info</code> 查看。</div>
    </section>
  )
  const newPath = (f.path ?? d?.adminPath ?? '').trim()
  const normPath = '/' + newPath.replace(/^\/+|\/+$/g, '')
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>面板访问</h2>
      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        <dt>后台地址</dt><dd className="mono" style={{ wordBreak: 'break-all' }}>{d ? here : '…'}</dd>
        <dt>监听</dt><dd className="mono">{d?.listen ?? '…'}{d?.domain ? <span className="small mute">（由 Caddy 提供 HTTPS）</span> : null}</dd>
      </dl>
      {d && !d.updater && <div className="note warn">这台服务器还没有安装在线配置组件：在服务器上执行 <code>polaris update --force</code> 安装（只需一次）后就能在这里修改。Docker 部署请改环境变量。</div>}
      {d?.pending && <div className="note">上一项更改正在执行…</div>}
      {d?.last && <div className={'note ' + (d.last.ok ? '' : 'bad')}>{d.last.ok ? '✓' : '✗'} {accessLabel[d.last.action] ?? d.last.action}：{d.last.value}{d.last.ok ? ' 已生效' : ' 修改失败'}{!d.last.ok && d.last.log ? <div className="mono small" style={{ whiteSpace: 'pre-wrap', marginTop: 4 }}>{d.last.log}</div> : null}</div>}
      <div className="form-grid">
        <Field label="后台路径" hint="留空或填 / 不使用路径（打开地址就是后台）；用路径时扫描器只能看到 404 或伪装站点">
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.path ?? d?.adminPath ?? ''} placeholder="/" onChange={(e) => setF({ ...f, path: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || f.path === undefined || normPath === (d?.adminPath ?? '')}
              onClick={() => apply('set-path', normPath, base + (normPath === '/' ? '' : normPath) + '/', '后台路径修改后面板会重启。',
                normPath === '/' ? '关闭后台路径后，任何人打开这个地址都能看到登录页。请确认已使用强密码并开启二次验证。' : `后台路径改为 ${normPath}，旧的书签将失效。`)}>应用</button>
          </div>
        </Field>
        <Field label="面板域名（HTTPS）" hint="先把域名 A 记录解析到本服务器；会安装 Caddy 自动签发证书并放行 80 / 443。已有 Nginx / OpenResty / 宝塔等占用 443 时不要用这里：在它们里反代到本机面板端口，再改下方「公开地址」">
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.domain ?? d?.domain ?? ''} placeholder="panel.example.com" onChange={(e) => setF({ ...f, domain: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.domain?.trim() || f.domain.trim() === d?.domain}
              onClick={() => apply('set-domain', f.domain!.trim(), 'https://' + f.domain!.trim().toLowerCase() + adminPath + '/', '正在安装 Caddy 并签发证书，通常需要半分钟到一分钟。',
                `面板将改用 https://${f.domain!.trim()} 访问，订阅链接也会变成这个域名（用户需重新导入；不想变可在订阅设置里填订阅域名）。已接入的节点不受影响。`)}>启用 HTTPS</button>
          </div>
        </Field>
        <Field label="面板端口" hint={d?.domain ? '已用域名访问时，这是 Caddy 转发的本机端口' : '订阅链接里的端口会随之改变；云服务器需在安全组放行新端口'}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" inputMode="numeric" value={f.port ?? port} onChange={(e) => setF({ ...f, port: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.port || f.port === port}
              onClick={() => apply('set-port', f.port!, d?.domain ? here : `${location.protocol}//${location.hostname}:${f.port}${adminPath}/`, '端口修改后面板会重启。',
                d?.domain ? `Caddy 将转发到本机 ${f.port} 端口，访问地址不变。` : `面板改为 ${f.port} 端口，订阅链接里的端口也会改变，用户需要重新导入。`)}>应用</button>
          </div>
        </Field>
        <Field label="公开地址" hint="订阅和安装链接里使用的地址；自建反向代理时填写，一般不用改">
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.url ?? d?.publicUrl ?? ''} placeholder="https://panel.example.com" onChange={(e) => setF({ ...f, url: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.url?.trim() || f.url.trim().replace(/\/$/, '') === d?.publicUrl}
              onClick={() => apply('set-url', f.url!.trim(), here, '公开地址修改后面板会重启。', `订阅与安装链接将改用 ${f.url!.trim()}，用户需要重新导入订阅。`)}>应用</button>
          </div>
        </Field>
      </div>
    </section>
  )
}

export default function Settings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const me = useLoad(() => get<{ username: string; totpEnabled: boolean; githubLogin?: string; recoveryCodesLeft?: number }>('/auth/me'))
  const [codes, setCodes] = useState<string[] | null>(null)
  const gh = useLoad(() => get<{ enabled: boolean; clientId: string; callbackUrl: string }>('/auth/github'))
  const [ghForm, setGhForm] = useState({ id: '', secret: '' })
  const toast = useToast()
  const confirm = useConfirm()
  useEffect(() => {
    const r = takeOAuthResult()
    if (r.bound) toast('已绑定 GitHub 账号 ' + r.bound)
    if (r.error) toast('GitHub 绑定失败：' + r.error, true)
    r.bound = r.error = undefined // shown once, not on every visit
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  useEffect(() => { if (gh.data) setGhForm((x) => ({ ...x, id: gh.data!.clientId })) }, [gh.data])
  const [f, setF] = useState<Record<string, string>>({})
  const [pw, setPw] = useState({ old: '', new: '', again: '' })
  const [showPw, setShowPw] = useState(false)
  const [totp, setTotp] = useState<{ secret: string; url: string } | null>(null)
  const [code, setCode] = useState('')
  const { busy, run } = useAction()
  useEffect(() => {
    if (s.data) setF({
      site_title: s.data.site_title ?? '', sub_update_interval: s.data.sub_update_interval ?? '1', ip_ban_minutes: s.data.ip_ban_minutes ?? '0',
      acme_email: s.data.acme_email ?? '', acme_cf_token: '', default_xray: s.data.default_xray || 'latest', default_singbox: s.data.default_singbox || 'latest',
      ip_limit_trusted: s.data.ip_limit_trusted ?? '',
    })
  }, [s.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const [tab, setTab] = useState<string>(() => settingTabs.some((t) => '#' + t.id === location.hash) ? location.hash.slice(1) : 'general')
  const go = (id: string) => { setTab(id); history.replaceState(null, '', '#' + id) }

  return (
    <>
      <div className="top"><div><h1>设置</h1><div className="sub">面板、证书、账号安全、备份与外观</div></div></div>
      <div className="settings">
        <nav className="snav" aria-label="设置分区">
          {settingTabs.map((t) => <button key={t.id} type="button" className={'snav-item' + (tab === t.id ? ' on' : '')} aria-current={tab === t.id ? 'page' : undefined} onClick={() => go(t.id)}>{t.icon}<span><b>{t.label}</b><small>{t.hint}</small></span></button>)}
        </nav>
        <div className="spane" key={tab}>
        {tab === 'general' && <>
        <Access />
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>常规</h2>
          <div className="form-grid">
            <Field label="站点标题" full><input className="input" value={f.site_title ?? ''} onChange={(e) => set('site_title', e.target.value)} /></Field>
            <Field label="订阅更新间隔（小时）"><input className="input mono" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
            <Field label="IP 超限封禁（分钟）" hint="0 为只告警不封禁"><input className="input mono" value={f.ip_ban_minutes ?? ''} onChange={(e) => set('ip_ban_minutes', e.target.value)} /></Field>
            <Field label="IP 限制受信任地址" full hint="每行一个 IP 或 CIDR，不计入用户的同时在线 IP 数（如家庭 / 公司出口、自己的中转节点）">
              <textarea className="input mono" style={{ minHeight: 56 }} value={f.ip_limit_trusted ?? ''} onChange={(e) => set('ip_limit_trusted', e.target.value)} placeholder={'203.0.113.10\n198.51.100.0/24'} />
            </Field>
            <DefaultVersion label="默认 Xray 版本" type="xray" value={f.default_xray ?? ''} onChange={(v) => set('default_xray', v)} />
            <DefaultVersion label="默认 sing-box 版本" type="singbox" value={f.default_singbox ?? ''} onChange={(v) => set('default_singbox', v)} />
          </div>
          <h2 style={{ marginTop: 8 }}>ACME（Let's Encrypt）</h2>
          <div className="hint" style={{ marginTop: -6 }}>只填邮箱即可用 HTTP 验证签发（由域名指向的节点在 80 端口应答）；再填 Cloudflare Token 则改用 DNS 验证，支持泛域名、无需 80 端口。</div>
          <div className="form-grid">
            <Field label="邮箱"><input className="input" type="email" value={f.acme_email ?? ''} onChange={(e) => set('acme_email', e.target.value)} /></Field>
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? '已保存（留空不修改）' : '可选 · 需要 Zone.DNS 编辑权限'}>
              <input className="input mono" type="password" autoComplete="off" value={f.acme_cf_token ?? ''} onChange={(e) => set('acme_cf_token', e.target.value)} />
            </Field>
          </div>
          <div><button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', f); s.reload() }, '设置已保存')}>保存</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>部署信息</h2>
          <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
            <dt>版本</dt><dd className="mono">{s.data?.version}</dd>
            <dt>后台路径</dt><dd className="mono">{s.data?.admin_path}</dd>
            <dt>公开地址</dt><dd className="mono">{s.data?.public_url || '未设置（POLARIS_PUBLIC_URL）'}</dd>
            <dt>Agent 接入点</dt><dd className="mono">{s.data?.agent_url}</dd>
          </dl>
          <Field label="CA 指纹（SHA-256）" hint="安装命令携带此指纹，Agent 首次接入时校验主控身份"><CopyBox text={s.data?.ca_sha256 ?? ''} /></Field>
          <div className="note">后台路径、域名、端口和公开地址可在上方「面板访问」修改；数据库与加密密钥通过 /etc/polaris/polaris.env 设置（见 README）。使用后台路径时，其他路径一律返回 404 或伪装站点。</div>
        </section>
        </>}
        {tab === 'update' && <Updates />}
        {tab === 'appearance' && <Appearance />}
        {tab === 'backup' && <Backups />}
        {tab === 'security' && <>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>修改密码 · {me.data?.username}</h2>
          <Field label="原密码"><input className="input" type={showPw ? 'text' : 'password'} autoComplete="current-password" autoCapitalize="none" spellCheck={false} value={pw.old} onChange={(e) => setPw({ ...pw, old: e.target.value })} /></Field>
          <Field label="新密码" hint={pw.new && pw.new.trim() !== pw.new ? '首尾不能有空格' : '至少 8 位'}><input className="input" type={showPw ? 'text' : 'password'} autoComplete="new-password" autoCapitalize="none" spellCheck={false} value={pw.new} onChange={(e) => setPw({ ...pw, new: e.target.value })} /></Field>
          <Field label="确认新密码" hint={pw.again && pw.again !== pw.new ? '两次输入的新密码不一致' : undefined}><input className="input" type={showPw ? 'text' : 'password'} autoComplete="new-password" autoCapitalize="none" spellCheck={false} value={pw.again} onChange={(e) => setPw({ ...pw, again: e.target.value })} /></Field>
          <label className="chk"><input type="checkbox" checked={showPw} onChange={(e) => setShowPw(e.target.checked)} />显示密码</label>
          <div><button className="btn" type="button" disabled={busy || !pw.old || pw.new.length < 8 || pw.new !== pw.again || pw.new.trim() !== pw.new} onClick={() => run(async () => { const r = await post<{ token: string }>('/auth/password', { old: pw.old, new: pw.new }); setToken(r.token); setPw({ old: '', new: '', again: '' }) }, '密码已修改，下次请用新密码登录；其他设备上的登录已退出')}>修改密码</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>二次验证（TOTP）</h2>
          <div className="sub" style={{ margin: 0 }}>当前：{me.data?.totpEnabled ? '已开启' : '未开启'}{me.data?.totpEnabled ? ` · 恢复码剩余 ${me.data.recoveryCodesLeft ?? 0} 个` : ''}</div>
          {codes && <RecoveryCodesModal codes={codes} onClose={() => setCodes(null)} />}
          {me.data?.totpEnabled && !totp && (me.data.recoveryCodesLeft ?? 0) < 4 && <div className="note warn">恢复码快用完了，建议重新生成并保存。</div>}
          {!totp ? (
            <div className="row">
              <button className="btn" type="button" disabled={busy} onClick={() => run(async () => setTotp(await post('/auth/totp/setup')))}>{me.data?.totpEnabled ? '重新绑定' : '开始绑定'}</button>
              {me.data?.totpEnabled && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { setCodes((await post<{ recoveryCodes: string[] }>('/auth/totp/recovery-codes')).recoveryCodes); me.reload() })}>重新生成恢复码</button>}
            </div>
          ) : (
            <>
              <QR text={totp.url} size={180} />
              <CopyBox text={totp.secret} />
              <Field label="验证码"><input className="input mono" inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} /></Field>
              <div className="row">
                <button className="btn pri" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { const r = await post<{ recoveryCodes: string[] }>('/auth/totp/enable', { code, enable: true }); setTotp(null); setCode(''); me.reload(); setCodes(r.recoveryCodes) }, '二次验证已开启')}>开启</button>
              </div>
            </>
          )}
          <h2 style={{ marginTop: 8 }}><span className="row" style={{ gap: 8 }}><Icon.GitHub />GitHub 登录</span></h2>
          {me.data?.githubLogin ? (
            <div className="row">
              <span className="small">当前账号已绑定 <b>@{me.data.githubLogin}</b>，可在登录页用 GitHub 登录{me.data.totpEnabled ? '（仍需二次验证码）' : ''}</span>
              <span className="spacer" />
              <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
                if (await confirm('解除 GitHub 绑定？之后只能用密码登录。')) run(async () => { await post('/auth/github/unbind'); me.reload() }, '已解除绑定')
              }}>解除绑定</button>
            </div>
          ) : gh.data?.enabled ? (
            <div className="row">
              <span className="small mute">绑定后可在登录页一键用 GitHub 登录</span><span className="spacer" />
              <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { location.href = (await post<{ url: string }>('/auth/github/bind-url')).url })}><Icon.GitHub />绑定 GitHub</button>
            </div>
          ) : null}
          <details className="more" open={!gh.data?.enabled}>
            <summary>OAuth App 设置{gh.data?.enabled ? '（已配置）' : ''}</summary>
            <div style={{ display: 'grid', gap: 10, marginTop: 10 }}>
              <div className="hint">在 GitHub → Settings → Developer settings → OAuth Apps 新建应用，「Authorization callback URL」填写下方地址，然后填入 Client ID 与 Client Secret。</div>
              <CopyBox text={gh.data?.callbackUrl ?? ''} />
              <div className="form-grid">
                <Field label="Client ID"><input className="input mono" value={ghForm.id} onChange={(e) => setGhForm({ ...ghForm, id: e.target.value.trim() })} /></Field>
                <Field label="Client Secret" hint={gh.data?.enabled ? '已保存（留空不修改）' : undefined}><input className="input mono" type="password" autoComplete="off" value={ghForm.secret} onChange={(e) => setGhForm({ ...ghForm, secret: e.target.value.trim() })} /></Field>
              </div>
              <div><button className="btn sm pri" type="button" disabled={busy || !ghForm.id} onClick={() => run(async () => {
                await put('/settings', ghForm.secret ? { github_client_id: ghForm.id, github_client_secret: ghForm.secret } : { github_client_id: ghForm.id })
                setGhForm((x) => ({ ...x, secret: '' })); gh.reload()
              }, 'GitHub 登录配置已保存')}>保存</button></div>
            </div>
          </details>
          {me.data?.totpEnabled && !totp && (
            <div className="row" style={{ marginTop: 4 }}>
              <input className="input mono" style={{ width: 140 }} placeholder="验证码" value={code} onChange={(e) => setCode(e.target.value)} />
              <button className="btn danger" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { await post('/auth/totp/enable', { code, enable: false }); setCode(''); me.reload() }, '已关闭')}>关闭二次验证</button>
            </div>
          )}
        </section>
        <Sessions />
        <LoginProtection />
        </>}
        </div>
      </div>
    </>
  )
}
