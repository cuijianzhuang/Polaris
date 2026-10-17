import { ProTag, useFeature } from '../components/License'
import { useEffect, useState } from 'react'
import { ApiError, del, download, get, post, put, setToken, takeOAuthResult, upload } from '../api'
import { LoginProtection, RecoveryCodesModal, Sessions, SSOProviders } from '../components/SecurityCards'
import { Icon } from '../components/Icons'
import { accents, useTheme, type ThemeMode } from '../theme'
import { CopyBox, Empty, Field, Modal, QR, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { bytes, datetime } from '../format'
import { t } from '../i18n'

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
    <Field label={label} hint={vers === null ? t('正在获取版本列表…') : vers.length === 0 ? t('无法获取版本列表（可配置 GitHub 镜像）') : t('「自动」时新节点安装最新稳定版')}>
      <select className="input mono" value={cur} onChange={(e) => onChange(e.target.value)}>
        <option value="latest">{recent[0] ? t('自动（最新：v{v}）', { v: recent[0] }) : t('自动（最新）')}</option>
        {opts.map((v, i) => <option key={v} value={v}>v{v}{i === 0 && recent[0] === v ? ' · ' + t('最新') : ''}{!recent.includes(v) ? ' · ' + t('自定义') : ''}</option>)}
      </select>
    </Field>
  )
}

type BackupInfo = { supported: boolean; db: string; dbSize: number; dir: string; encrypted: boolean; targets: string[] | null
  files: { name: string; size: number; created: string; encrypted: boolean }[] }

const backupKeys = ['backup_schedule', 'backup_hour', 'backup_keep', 'backup_tg', 'backup_webdav_url', 'backup_webdav_user',
  'backup_s3_endpoint', 'backup_s3_region', 'backup_s3_bucket', 'backup_s3_prefix', 'backup_s3_access_key'] as const
const secretKeys = ['backup_passphrase', 'backup_webdav_pass', 'backup_s3_secret_key'] as const
const targetLabel: Record<string, string> = { telegram: 'Telegram', webdav: 'WebDAV', s3: 'S3' }

// Backups: on-demand and scheduled archives (all data + internal CA),
// optionally encrypted and pushed off-site; download, upload-and-restore.
// A restore restarts the panel.
function Backups() {
  const pushOK = useFeature('backup_push')
  const info = useLoad(() => get<BackupInfo>('/backup'))
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [sec, setSec] = useState<Record<string, string>>({})
  const [ask, setAsk] = useState<{ label: string; go: (pass: string) => Promise<unknown> } | null>(null)
  const [pass, setPass] = useState('')
  useEffect(() => {
    if (s.data) setF(Object.fromEntries(backupKeys.map((k) => [k, String(s.data?.[k] ?? '')])))
  }, [s.data])
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const [restoring, setRestoring] = useState(false)
  const afterRestore = () => {
    setRestoring(true)
    // The panel re-executes itself; wait for it to answer again.
    const t0 = Date.now()
    const ping = () => get('/info').then(() => location.reload()).catch(() => { if (Date.now() - t0 < 90000) setTimeout(ping, 1500); else { setRestoring(false); toast(t('面板未在 90 秒内恢复，请检查服务状态'), true) } })
    setTimeout(ping, 2500)
  }
  // restore runs fn(passphrase); an encrypted archive without one asks for it.
  const restore = async (label: string, fn: (pass: string) => Promise<unknown>) => {
    if (!await confirm(t('用「{label}」覆盖当前全部数据并重启面板？', { label }) + '\n' + t('当前数据会先自动保存一份快照；恢复后需要重新登录。'), { danger: true })) return
    const attempt = async (p: string) => {
      try { await fn(p); setAsk(null); toast(t('备份校验通过，面板正在重启…')); afterRestore() } catch (e) {
        if (e instanceof ApiError && e.data?.needPassphrase) { setPass(''); setAsk({ label, go: fn }); if (p) toast(e.message, true); return }
        toast(e instanceof Error ? e.message : String(e), true)
      }
    }
    attempt('')
  }
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    const body: Record<string, string> = { ...f }
    for (const k of secretKeys) if (sec[k]) body[k] = sec[k]
    await put('/settings', body); setSec({}); s.reload(); info.reload()
  }, t('备份设置已保存'))
  const isSet = (k: string) => !!s.data?.[k + '_set']
  const d = info.data
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('备份与恢复')}</h2>
        <span className="sub" style={{ margin: 0 }}>{d?.db === 'postgres' ? 'PostgreSQL' : `SQLite ${bytes(d?.dbSize)}`} · {t('保存在')} <code>{d?.dir}</code></span>
      </div>
      {restoring && <div className="note warn"><span className="spinner" /> {t('正在应用备份并重启面板，完成后自动刷新…')}</div>}
      <div className="form-grid">
        <Field label={t('自动备份')}>
          <select className="input" value={f.backup_schedule || 'off'} onChange={(e) => set('backup_schedule', e.target.value)}>
            <option value="off">{t('关闭')}</option><option value="daily">{t('每天')}</option><option value="weekly">{t('每周一')}</option>
          </select>
        </Field>
        <Field label={t('备份时间（点）')} hint={t('服务器本地时间 0–23')}><input className="input mono" inputMode="numeric" value={f.backup_hour || ''} placeholder="4" onChange={(e) => set('backup_hour', e.target.value)} /></Field>
        <Field label={t('保留份数')} hint={t('超出后删除最旧的；恢复前快照不会被删除')}><input className="input mono" inputMode="numeric" value={f.backup_keep || ''} placeholder="7" onChange={(e) => set('backup_keep', e.target.value)} /></Field>
        <Field label={t('备份口令')} hint={isSet('backup_passphrase') ? t('已设置：备份已加密（留空不修改）') : t('设置后备份用口令加密（scrypt + AES-GCM）并附带密钥，可恢复到任何服务器；口令丢失无法找回')}>
          <input className="input mono" type="password" autoComplete="new-password" value={sec.backup_passphrase ?? ''} placeholder={isSet('backup_passphrase') ? '••••••••' : t('不加密')} onChange={(e) => setSec({ ...sec, backup_passphrase: e.target.value })} />
        </Field>
      </div>
      <details className="more" open={!!(d?.targets?.length)}>
        <summary>{t('推送到其他位置')}<ProTag on={pushOK} />{d?.targets?.length ? `（${d.targets.map((x) => targetLabel[x] ?? x).join(t('、'))}）` : ''}</summary>
        <div style={{ display: 'grid', gap: 12, marginTop: 10 }}>
          <div className="hint">{t('自动备份完成后推送到下列已配置的位置；也可以点「立即备份并推送」。服务器坏了也能从这里取回。')}</div>
          {!pushOK && <div className="note warn">{t('备份推送需要专业版许可证；未授权时只保存本地备份（许可证页面）。')}</div>}
          <div className="toggle"><span>{t('发送到 Telegram（超级管理员，单个文件 50 MB 以内）')}</span><Switch label="Telegram" on={f.backup_tg === 'true'} onChange={(v) => set('backup_tg', String(v))} /></div>
          <div className="form-grid">
            <Field label={t('WebDAV 文件夹地址')} hint={t('如坚果云 https://dav.jianguoyun.com/dav/polaris/（文件夹需已存在）')}><input className="input mono" value={f.backup_webdav_url || ''} placeholder="https://dav.example.com/backups/" onChange={(e) => set('backup_webdav_url', e.target.value.trim())} /></Field>
            <Field label={t('WebDAV 用户名')}><input className="input mono" value={f.backup_webdav_user || ''} onChange={(e) => set('backup_webdav_user', e.target.value.trim())} /></Field>
            <Field label={t('WebDAV 密码')} hint={isSet('backup_webdav_pass') ? t('已保存（留空不修改）') : t('应用密码')}><input className="input mono" type="password" autoComplete="off" value={sec.backup_webdav_pass ?? ''} onChange={(e) => setSec({ ...sec, backup_webdav_pass: e.target.value })} /></Field>
          </div>
          <div className="form-grid">
            <Field label={t('S3 端点')} hint={t('AWS 留空；Cloudflare R2：https://<账户 ID>.r2.cloudflarestorage.com；MinIO / B2 填其地址')}><input className="input mono" value={f.backup_s3_endpoint || ''} placeholder="https://s3.amazonaws.com" onChange={(e) => set('backup_s3_endpoint', e.target.value.trim())} /></Field>
            <Field label={t('区域')} hint={t('R2 填 auto')}><input className="input mono" value={f.backup_s3_region || ''} placeholder="us-east-1" onChange={(e) => set('backup_s3_region', e.target.value.trim())} /></Field>
            <Field label={t('存储桶')}><input className="input mono" value={f.backup_s3_bucket || ''} onChange={(e) => set('backup_s3_bucket', e.target.value.trim())} /></Field>
            <Field label={t('路径前缀')} hint={t('如 polaris/')}><input className="input mono" value={f.backup_s3_prefix || ''} onChange={(e) => set('backup_s3_prefix', e.target.value.trim())} /></Field>
            <Field label="Access Key ID"><input className="input mono" value={f.backup_s3_access_key || ''} onChange={(e) => set('backup_s3_access_key', e.target.value.trim())} /></Field>
            <Field label="Secret Access Key" hint={isSet('backup_s3_secret_key') ? t('已保存（留空不修改）') : undefined}><input className="input mono" type="password" autoComplete="off" value={sec.backup_s3_secret_key ?? ''} onChange={(e) => setSec({ ...sec, backup_s3_secret_key: e.target.value })} /></Field>
          </div>
        </div>
      </details>
      <div className="row">
        <button className="btn pri" type="button" disabled={busy} onClick={save}>{t('保存设置')}</button>
        <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await post('/backup'); info.reload() }, t('已创建备份'))}>{t('立即备份')}</button>
        <button className="btn" type="button" disabled={busy} onClick={() => run(async () => {
          const r = await post<{ push?: Record<string, string> }>('/backup?push=1'); info.reload()
          const bad = Object.entries(r.push ?? {}).filter(([, v]) => v !== 'ok')
          if (bad.length) toast(bad.map(([k, v]) => `${targetLabel[k] ?? k}：${v}`).join('；'), true)
          else toast(Object.keys(r.push ?? {}).length ? t('已备份并推送到 {targets}', { targets: Object.keys(r.push ?? {}).map((k) => targetLabel[k] ?? k).join(t('、')) }) : t('已备份（没有配置推送位置）'))
        })}>{t('立即备份并推送')}</button>
        <label className="btn" style={{ cursor: 'pointer' }}>
          {t('上传备份并恢复')}
          <input type="file" accept=".gz,.enc,.db" style={{ display: 'none' }} onChange={(e) => {
            const file = e.target.files?.[0]; e.target.value = ''
            if (file) restore(file.name, (p) => upload('/backup/restore', file, p ? { passphrase: p } : {}))
          }} />
        </label>
      </div>
      <div className="card flush scroll">
        {d?.files.length === 0 ? <Empty>{t('还没有备份')}</Empty> : (
          <table className="rtable">
            <thead><tr><th>{t('文件')}</th><th>{t('大小')}</th><th>{t('时间')}</th><th></th></tr></thead>
            <tbody>{(d?.files ?? []).map((b) => (
              <tr key={b.name}>
                <td className="rt-head mono" style={{ wordBreak: 'break-all', minWidth: 200 }}>{b.name}{b.name.includes('-auto') && <span className="tag" style={{ marginLeft: 6 }}>{t('自动')}</span>}{b.name.includes('-prerestore') && <span className="tag warn" style={{ marginLeft: 6 }}>{t('恢复前快照')}</span>}{b.encrypted && <span className="tag ok" style={{ marginLeft: 6 }}>{t('已加密')}</span>}</td>
                <td data-label={t('大小')} className="mono">{bytes(b.size)}</td>
                <td data-label={t('时间')}>{datetime(b.created)}</td>
                <td className="rt-actions"><div className="row" style={{ justifyContent: 'flex-end', gap: 6 }}>
                  <button className="btn sm" type="button" onClick={() => run(() => download('/backup/files/' + b.name, b.name))}>{t('下载')}</button>
                  <button className="btn sm" type="button" disabled={busy} onClick={() => restore(b.name, (p) => post(`/backup/files/${b.name}/restore`, p ? { passphrase: p } : {}))}>{t('恢复')}</button>
                  <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除备份 {name}？', { name: b.name }), { danger: true })) run(async () => { await del('/backup/files/' + b.name); info.reload() }) }}>{t('删除')}</button>
                </div></td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </div>
      <div className="hint">{t('备份包含全部数据与内部 CA 证书（迁移后已接入的节点无需重装），SQLite 与 PostgreSQL 之间可互相恢复。未加密的备份只能恢复到同一 secret_key 的面板；加密备份附带密钥，新服务器执行')} <code>{t('polaris install --restore 文件')}</code> {t('或安装后')} <code>{t('polaris restore 文件')}</code> {t('即可。')}</div>
      {ask && (
        <Modal title={t('输入备份口令')} onClose={() => setAsk(null)} footer={<>
          <button className="btn" type="button" onClick={() => setAsk(null)}>{t('取消')}</button>
          <button className="btn pri" type="submit" form="bk-pass" disabled={!pass || busy}>{t('恢复')}</button>
        </>}>
          <form id="bk-pass" style={{ display: 'grid', gap: 10 }} onSubmit={(e) => { e.preventDefault(); const go = ask.go; run(async () => { try { await go(pass); setAsk(null); afterRestore() } catch (err) { if (err instanceof ApiError && err.data?.needPassphrase) throw err; setAsk(null); throw err } }, t('备份校验通过，面板正在重启…')) }}>
            <div className="sub" style={{ margin: 0 }}>{t('「{label}」是加密备份，请输入创建它时的备份口令。', { label: ask.label })}</div>
            <input className="input mono" type="password" autoFocus value={pass} onChange={(e) => setPass(e.target.value)} />
          </form>
        </Modal>
      )}
    </section>
  )
}

const settingTabs = [
  { id: 'general', label: t('常规'), hint: t('访问地址、标题、默认内核'), icon: <Icon.Gear /> },
  { id: 'security', label: t('账号安全'), hint: t('密码、二次验证、登录设备、登录防护'), icon: <Icon.Lock /> },
  { id: 'update', label: t('版本与更新'), hint: t('在线升级、自动更新'), icon: <Icon.Refresh /> },
  { id: 'backup', label: t('备份与恢复'), hint: t('自动备份、恢复'), icon: <Icon.Download /> },
  { id: 'appearance', label: t('外观'), hint: t('主题与配色'), icon: <Icon.Sun /> },
]

// Appearance is per browser: mode plus an accent palette.
function Appearance() {
  const [th, setT] = useTheme()
  const modes: { id: ThemeMode; label: string }[] = [{ id: 'light', label: t('浅色') }, { id: 'dark', label: t('深色') }, { id: 'system', label: t('跟随系统') }]
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('外观')}</h2>
      <Field label={t('主题模式')} hint={t('仅保存在当前浏览器')}>
        <div className="seg c3 fixed">
          {modes.map((m) => <button key={m.id} type="button" className={'opt' + (th.mode === m.id ? ' on' : '')} aria-pressed={th.mode === m.id} onClick={() => setT({ ...th, mode: m.id })}>{m.label}</button>)}
        </div>
      </Field>
      <Field label={t('主题色')}>
        <div className="swatches">
          {accents.map((a) => <button key={a.id} type="button" className={'accent-swatch' + (th.accent === a.id ? ' on' : '')} style={{ background: a.color }}
            title={a.label} aria-label={t('主题色：{name}', { name: a.label })} aria-pressed={th.accent === a.id} onClick={() => setT({ ...th, accent: a.id })} />)}
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
    if (!await confirm(t('升级面板到 {v}？', { v: d?.latest }) + (agents ? '\n' + t('完成后会接着升级所有在线节点的 Agent。') : '') + '\n' + t('升级约需 1 分钟，期间面板会重启、网页短暂断开；结果会通过 Telegram 通知并记入审计日志。'))) return
    run(async () => { await post('/system/update', { agents }); info.reload() }, t('已开始升级，稍后刷新页面'))
  }
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('版本与更新')}</h2>
      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        <dt>{t('当前版本')}</dt><dd className="mono">{d?.current ?? '…'}</dd>
        <dt>{t('最新版本')}</dt><dd className="mono">{d?.latest ? <>{d.latest}{d.newer ? <span className="tag warn" style={{ marginLeft: 8 }}>{t('可升级')}</span> : <span className="tag ok" style={{ marginLeft: 8 }}>{t('已是最新')}</span>}</> : d?.error ? <span style={{ color: 'var(--bad)' }}>{t('获取失败：{error}', { error: d.error })}</span> : '…'}</dd>
      </dl>
      {d && !d.updater && <div className="note warn">{t('这台服务器还没有安装在线升级组件：在服务器上执行')} <code>polaris update --force</code> {t('安装（只需一次，之后的版本都能在这里或 Telegram 里一键升级）。Docker 部署请更新镜像。')}</div>}
      {d?.pending && <div className="note">{t('升级进行中…完成后刷新页面。')}</div>}
      <div className="row">
        <button className="btn pri" type="button" disabled={busy || !d?.newer || !d?.updater || !!d?.pending} onClick={() => start(false)}>{t('升级面板')}</button>
        <button className="btn" type="button" disabled={busy || !d?.newer || !d?.updater || !!d?.pending} onClick={() => start(true)}>{t('升级面板和全部节点')}</button>
        <button className="btn" type="button" disabled={busy} onClick={() => info.reload()}>{t('检查更新')}</button>
      </div>
      <div className="form-grid">
        <Field label={t('自动更新')} hint={t('新版本发布后：通知会发给 Telegram 超级管理员（每个版本一次）')}>
          <select className="input" value={m} onChange={(e) => setMode(e.target.value)}>
            <option value="off">{t('关闭')}</option>
            <option value="notify">{t('只通知新版本')}</option>
            <option value="panel">{t('自动升级面板')}</option>
            <option value="all">{t('自动升级面板和全部节点')}</option>
          </select>
        </Field>
        <Field label={t('自动升级时间')} hint={t('服务器本地时间；某个版本自动升级失败后不再自动重试，可手动升级')}>
          <select className="input mono" value={h} disabled={m !== 'panel' && m !== 'all'} onChange={(e) => setHour(e.target.value)}>
            {Array.from({ length: 24 }, (_, i) => <option key={i} value={String(i)}>{String(i).padStart(2, '0')}:41</option>)}
          </select>
        </Field>
      </div>
      <div><button className="btn pri" type="button" disabled={busy || (mode === null && hour === null)} onClick={() => run(async () => {
        await put('/settings', { auto_update: m, auto_update_hour: h }); setMode(null); setHour(null); s.reload()
      }, t('已保存'))}>{t('保存')}</button></div>
    </section>
  )
}

type AccessInfo = { adminPath: string; publicUrl: string; listen: string; domain?: string; subDomain?: string; updater: boolean; pending?: boolean
  last?: { action: string; value: string; ok: boolean; log: string; at: string } }

const accessLabel: Record<string, string> = { 'set-path': t('后台路径'), 'set-domain': t('面板域名'), 'set-port': t('面板端口'), 'set-url': t('公开地址'), 'set-sub-domain': t('订阅域名') }

// The last access change is reported for a while: success for 10 minutes,
// failure for a day, so the note does not linger once it has been seen.
const recent = (l: { ok: boolean; at: string }) => {
  const at = Date.parse(l.at)
  return !Number.isFinite(at) || Date.now() - at < (l.ok ? 10 * 60e3 : 24 * 3600e3)
}

// Access: how the panel is reached (admin path, HTTPS domain, port, public
// address). These live in /etc/polaris/polaris.env; the change is carried
// out by the root side of the server (`polaris set-path` …), which restarts
// the panel, so the page then moves to the new address.
function Access() {
  const a = useLoad(() => get<AccessInfo>('/system/access'))
  const [f, setF] = useState<{ path?: string; domain?: string; port?: string; url?: string; sub?: string }>({})
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
    if (!await confirm(`${warn}\n\n${t('应用后面板会重启，页面将跳转到：')}\n${to}`, { ok: t('应用') })) return
    const ok = await run(async () => { await post('/system/access', { action, value }); return true })
    if (ok) setMoving({ to, note })
  }

  if (moving) return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <h2><span className="spinner" style={{ marginRight: 8 }} />{t('正在应用…')}</h2>
      <div className="sub" style={{ margin: 0 }}>{moving.note}</div>
      <div>{t('新地址：')}<a className="mono" href={moving.to}>{moving.to}</a></div>
      <div className="hint">{t('页面会自动跳转；如果一直打不开，等一会儿手动打开上面的地址，或在服务器上执行')} <code>polaris info</code> {t('查看。')}</div>
    </section>
  )
  const newPath = (f.path ?? d?.adminPath ?? '').trim()
  const normPath = '/' + newPath.replace(/^\/+|\/+$/g, '')
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('面板访问')}</h2>
      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        <dt>{t('后台地址')}</dt><dd className="mono" style={{ wordBreak: 'break-all' }}>{d ? here : '…'}</dd>
        <dt>{t('监听')}</dt><dd className="mono">{d?.listen ?? '…'}{d?.domain ? <span className="small mute">{t('（由 Caddy 提供 HTTPS）')}</span> : null}</dd>
      </dl>
      {d && !d.updater && <div className="note warn">{t('这台服务器还没有安装在线配置组件：在服务器上执行')} <code>polaris update --force</code> {t('安装（只需一次）后就能在这里修改。Docker 部署请改环境变量。')}</div>}
      {d?.pending && <div className="note">{t('上一项更改正在执行…')}</div>}
      {d?.last && recent(d.last) && <div className={'note ' + (d.last.ok ? '' : 'bad')}>{d.last.ok ? '✓' : '✗'} {accessLabel[d.last.action] ?? d.last.action}：{d.last.value}{' '}{d.last.ok ? t('已生效') : t('修改失败')}{!d.last.ok && d.last.log ? <div className="mono small" style={{ whiteSpace: 'pre-wrap', marginTop: 4 }}>{d.last.log}</div> : null}</div>}
      <div className="form-grid">
        <Field label={t('后台路径')} hint={t('留空或填 / 不使用路径（打开地址就是后台）；用路径时扫描器只能看到 404 或伪装站点')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.path ?? d?.adminPath ?? ''} placeholder="/" onChange={(e) => setF({ ...f, path: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || f.path === undefined || normPath === (d?.adminPath ?? '')}
              onClick={() => apply('set-path', normPath, base + (normPath === '/' ? '' : normPath) + '/', t('后台路径修改后面板会重启。'),
                normPath === '/' ? t('关闭后台路径后，任何人打开这个地址都能看到登录页。请确认已使用强密码并开启二次验证。') : t('后台路径改为 {path}，旧的书签将失效。', { path: normPath }))}>{t('应用')}</button>
          </div>
        </Field>
        <Field label={t('面板域名（HTTPS）')} hint={t('先把域名 A 记录解析到本服务器；会安装 Caddy 自动签发证书并放行 80 / 443。已有 Nginx / OpenResty / 宝塔等占用 443 时不要用这里：在它们里反代到本机面板端口，再改下方「公开地址」')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.domain ?? d?.domain ?? ''} placeholder="panel.example.com" onChange={(e) => setF({ ...f, domain: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.domain?.trim() || f.domain.trim() === d?.domain}
              onClick={() => apply('set-domain', f.domain!.trim(), 'https://' + f.domain!.trim().toLowerCase() + adminPath + '/', t('正在安装 Caddy 并签发证书，通常需要半分钟到一分钟。'),
                t('面板将改用 https://{domain} 访问，订阅链接也会变成这个域名（用户需重新导入；不想变可在订阅设置里填订阅域名）。已接入的节点不受影响。', { domain: f.domain!.trim() }))}>{t('启用 HTTPS')}</button>
          </div>
        </Field>
        <Field label={t('订阅域名（HTTPS）')} hint={t('给订阅链接单独一个域名（可走 CDN）：先把域名解析到本服务器，这里会用 Caddy 签发证书，完成后订阅链接自动改用它。该域名只开放订阅和用户中心，后台地址不变。已有 Nginx / OpenResty / 宝塔占用 80、443 时，先在它们里把域名反代到本机面板端口，再点「配置」（会检测反代是否生效，不安装 Caddy）')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.sub ?? d?.subDomain ?? ''} placeholder="sub.example.com" onChange={(e) => setF({ ...f, sub: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.sub?.trim() || f.sub.trim().toLowerCase() === d?.subDomain}
              onClick={async () => {
                const dom = f.sub!.trim().toLowerCase().replace(/^https?:\/\//, '').replace(/\/.*$/, '')
                if (!await confirm(t('为 {domain} 签发证书并作为订阅域名？订阅链接会改用 https://{domain}，用户需要重新导入订阅；面板不会重启。', { domain: dom }), { ok: t('应用') })) return
                run(async () => { await post('/system/access', { action: 'set-sub-domain', value: dom }); a.reload() }, t('已提交，证书签发通常需要半分钟'))
              }}>{t('配置')}</button>
          </div>
        </Field>
        <Field label={t('面板端口')} hint={d?.domain ? t('已用域名访问时，这是 Caddy 转发的本机端口') : t('订阅链接里的端口会随之改变；云服务器需在安全组放行新端口')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" inputMode="numeric" value={f.port ?? port} onChange={(e) => setF({ ...f, port: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.port || f.port === port}
              onClick={() => apply('set-port', f.port!, d?.domain ? here : `${location.protocol}//${location.hostname}:${f.port}${adminPath}/`, t('端口修改后面板会重启。'),
                d?.domain ? t('Caddy 将转发到本机 {port} 端口，访问地址不变。', { port: f.port }) : t('面板改为 {port} 端口，订阅链接里的端口也会改变，用户需要重新导入。', { port: f.port }))}>{t('应用')}</button>
          </div>
        </Field>
        <Field label={t('公开地址')} hint={t('订阅和安装链接里使用的地址；自建反向代理时填写，一般不用改')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.url ?? d?.publicUrl ?? ''} placeholder="https://panel.example.com" onChange={(e) => setF({ ...f, url: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.url?.trim() || f.url.trim().replace(/\/$/, '') === d?.publicUrl}
              onClick={() => apply('set-url', f.url!.trim(), here, t('公开地址修改后面板会重启。'), t('订阅与安装链接将改用 {url}，用户需要重新导入订阅。', { url: f.url!.trim() }))}>{t('应用')}</button>
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
    if (r.bound) toast(t('已绑定 GitHub 账号 {name}', { name: r.bound }))
    if (r.ssoBound) toast(t('已绑定 {name}', { name: r.ssoBound }))
    if (r.error) toast(t('绑定失败：{error}', { error: r.error }), true)
    r.bound = r.error = r.ssoBound = undefined // shown once, not on every visit
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
      <div className="top"><div><h1>{t('设置')}</h1><div className="sub">{t('面板、证书、账号安全、备份与外观')}</div></div></div>
      <div className="settings">
        <nav className="snav" aria-label={t('设置分区')}>
          {settingTabs.map((t) => <button key={t.id} type="button" className={'snav-item' + (tab === t.id ? ' on' : '')} aria-current={tab === t.id ? 'page' : undefined} onClick={() => go(t.id)}>{t.icon}<span><b>{t.label}</b><small>{t.hint}</small></span></button>)}
        </nav>
        <div className="spane" key={tab}>
        {tab === 'general' && <>
        <Access />
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('常规')}</h2>
          <div className="form-grid">
            <Field label={t('站点标题')} full><input className="input" value={f.site_title ?? ''} onChange={(e) => set('site_title', e.target.value)} /></Field>
            <Field label={t('订阅更新间隔（小时）')}><input className="input mono" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
            <Field label={t('IP 超限封禁（分钟）')} hint={t('0 为只告警不封禁')}><input className="input mono" value={f.ip_ban_minutes ?? ''} onChange={(e) => set('ip_ban_minutes', e.target.value)} /></Field>
            <Field label={t('IP 限制受信任地址')} full hint={t('每行一个 IP 或 CIDR，不计入用户的同时在线 IP 数（如家庭 / 公司出口、自己的中转节点）')}>
              <textarea className="input mono" style={{ minHeight: 56 }} value={f.ip_limit_trusted ?? ''} onChange={(e) => set('ip_limit_trusted', e.target.value)} placeholder={'203.0.113.10\n198.51.100.0/24'} />
            </Field>
            <DefaultVersion label={t('默认 Xray 版本')} type="xray" value={f.default_xray ?? ''} onChange={(v) => set('default_xray', v)} />
            <DefaultVersion label={t('默认 sing-box 版本')} type="singbox" value={f.default_singbox ?? ''} onChange={(v) => set('default_singbox', v)} />
          </div>
          <h2 style={{ marginTop: 8 }}>ACME（Let's Encrypt）</h2>
          <div className="hint" style={{ marginTop: -6 }}>{t('只填邮箱即可用 HTTP 验证签发（由域名指向的节点在 80 端口应答）；再填 Cloudflare Token 则改用 DNS 验证，支持泛域名、无需 80 端口。')}</div>
          <div className="form-grid">
            <Field label={t('邮箱')}><input className="input" type="email" value={f.acme_email ?? ''} onChange={(e) => set('acme_email', e.target.value)} /></Field>
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? t('已保存（留空不修改）') : t('可选 · 需要 Zone.DNS 编辑权限')}>
              <input className="input mono" type="password" autoComplete="off" value={f.acme_cf_token ?? ''} onChange={(e) => set('acme_cf_token', e.target.value)} />
            </Field>
          </div>
          <div><button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', f); s.reload() }, t('设置已保存'))}>{t('保存')}</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('部署信息')}</h2>
          <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
            <dt>{t('版本')}</dt><dd className="mono">{s.data?.version}</dd>
            <dt>{t('后台路径')}</dt><dd className="mono">{s.data?.admin_path}</dd>
            <dt>{t('公开地址')}</dt><dd className="mono">{s.data?.public_url || t('未设置（POLARIS_PUBLIC_URL）')}</dd>
            <dt>{t('Agent 接入点')}</dt><dd className="mono">{s.data?.agent_url}</dd>
          </dl>
          <Field label={t('CA 指纹（SHA-256）')} hint={t('安装命令携带此指纹，Agent 首次接入时校验主控身份')}><CopyBox text={s.data?.ca_sha256 ?? ''} /></Field>
          <div className="note">{t('后台路径、域名、端口和公开地址可在上方「面板访问」修改；数据库与加密密钥通过 /etc/polaris/polaris.env 设置（见 README）。使用后台路径时，其他路径一律返回 404 或伪装站点。')}</div>
        </section>
        </>}
        {tab === 'update' && <Updates />}
        {tab === 'appearance' && <Appearance />}
        {tab === 'backup' && <Backups />}
        {tab === 'security' && <>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('修改密码')} · {me.data?.username}</h2>
          <Field label={t('原密码')}><input className="input" type={showPw ? 'text' : 'password'} autoComplete="current-password" autoCapitalize="none" spellCheck={false} value={pw.old} onChange={(e) => setPw({ ...pw, old: e.target.value })} /></Field>
          <Field label={t('新密码')} hint={pw.new && pw.new.trim() !== pw.new ? t('首尾不能有空格') : t('至少 8 位')}><input className="input" type={showPw ? 'text' : 'password'} autoComplete="new-password" autoCapitalize="none" spellCheck={false} value={pw.new} onChange={(e) => setPw({ ...pw, new: e.target.value })} /></Field>
          <Field label={t('确认新密码')} hint={pw.again && pw.again !== pw.new ? t('两次输入的新密码不一致') : undefined}><input className="input" type={showPw ? 'text' : 'password'} autoComplete="new-password" autoCapitalize="none" spellCheck={false} value={pw.again} onChange={(e) => setPw({ ...pw, again: e.target.value })} /></Field>
          <label className="chk"><input type="checkbox" checked={showPw} onChange={(e) => setShowPw(e.target.checked)} />{t('显示密码')}</label>
          <div><button className="btn" type="button" disabled={busy || !pw.old || pw.new.length < 8 || pw.new !== pw.again || pw.new.trim() !== pw.new} onClick={() => run(async () => { const r = await post<{ token: string }>('/auth/password', { old: pw.old, new: pw.new }); setToken(r.token); setPw({ old: '', new: '', again: '' }) }, t('密码已修改，下次请用新密码登录；其他设备上的登录已退出'))}>{t('修改密码')}</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('二次验证（TOTP）')}</h2>
          <div className="sub" style={{ margin: 0 }}>{t('当前：')}{me.data?.totpEnabled ? t('已开启') : t('未开启')}{me.data?.totpEnabled ? ' · ' + t('恢复码剩余 {n} 个', { n: me.data.recoveryCodesLeft ?? 0 }) : ''}</div>
          {codes && <RecoveryCodesModal codes={codes} onClose={() => setCodes(null)} />}
          {me.data?.totpEnabled && !totp && (me.data.recoveryCodesLeft ?? 0) < 4 && <div className="note warn">{t('恢复码快用完了，建议重新生成并保存。')}</div>}
          {!totp ? (
            <div className="row">
              <button className="btn" type="button" disabled={busy} onClick={() => run(async () => setTotp(await post('/auth/totp/setup')))}>{me.data?.totpEnabled ? t('重新绑定') : t('开始绑定')}</button>
              {me.data?.totpEnabled && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { setCodes((await post<{ recoveryCodes: string[] }>('/auth/totp/recovery-codes')).recoveryCodes); me.reload() })}>{t('重新生成恢复码')}</button>}
            </div>
          ) : (
            <>
              <QR text={totp.url} size={180} />
              <CopyBox text={totp.secret} />
              <Field label={t('验证码')}><input className="input mono" inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} /></Field>
              <div className="row">
                <button className="btn pri" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { const r = await post<{ recoveryCodes: string[] }>('/auth/totp/enable', { code, enable: true }); setTotp(null); setCode(''); me.reload(); setCodes(r.recoveryCodes) }, t('二次验证已开启'))}>{t('开启')}</button>
              </div>
            </>
          )}
          <h2 style={{ marginTop: 8 }}><span className="row" style={{ gap: 8 }}><Icon.GitHub />{t('GitHub 登录')}</span></h2>
          {me.data?.githubLogin ? (
            <div className="row">
              <span className="small">{t('当前账号已绑定')} <b>@{me.data.githubLogin}</b>{t('，可在登录页用 GitHub 登录')}{me.data.totpEnabled ? t('（仍需二次验证码）') : ''}</span>
              <span className="spacer" />
              <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
                if (await confirm(t('解除 GitHub 绑定？之后只能用密码登录。'))) run(async () => { await post('/auth/github/unbind'); me.reload() }, t('已解除绑定'))
              }}>{t('解除绑定')}</button>
            </div>
          ) : gh.data?.enabled ? (
            <div className="row">
              <span className="small mute">{t('绑定后可在登录页一键用 GitHub 登录')}</span><span className="spacer" />
              <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { location.href = (await post<{ url: string }>('/auth/github/bind-url')).url })}><Icon.GitHub />{t('绑定 GitHub')}</button>
            </div>
          ) : null}
          <details className="more" open={!gh.data?.enabled}>
            <summary>{t('OAuth App 设置')}{gh.data?.enabled ? t('（已配置）') : ''}</summary>
            <div style={{ display: 'grid', gap: 10, marginTop: 10 }}>
              <div className="hint">{t('在 GitHub → Settings → Developer settings → OAuth Apps 新建应用，「Authorization callback URL」填写下方地址，然后填入 Client ID 与 Client Secret。')}</div>
              <CopyBox text={gh.data?.callbackUrl ?? ''} />
              <div className="form-grid">
                <Field label="Client ID"><input className="input mono" value={ghForm.id} onChange={(e) => setGhForm({ ...ghForm, id: e.target.value.trim() })} /></Field>
                <Field label="Client Secret" hint={gh.data?.enabled ? t('已保存（留空不修改）') : undefined}><input className="input mono" type="password" autoComplete="off" value={ghForm.secret} onChange={(e) => setGhForm({ ...ghForm, secret: e.target.value.trim() })} /></Field>
              </div>
              <div><button className="btn sm pri" type="button" disabled={busy || !ghForm.id} onClick={() => run(async () => {
                await put('/settings', ghForm.secret ? { github_client_id: ghForm.id, github_client_secret: ghForm.secret } : { github_client_id: ghForm.id })
                setGhForm((x) => ({ ...x, secret: '' })); gh.reload()
              }, t('GitHub 登录配置已保存'))}>{t('保存')}</button></div>
            </div>
          </details>
          {me.data?.totpEnabled && !totp && (
            <div className="row" style={{ marginTop: 4 }}>
              <input className="input mono" style={{ width: 140 }} placeholder={t('验证码')} value={code} onChange={(e) => setCode(e.target.value)} />
              <button className="btn danger" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { await post('/auth/totp/enable', { code, enable: false }); setCode(''); me.reload() }, t('已关闭'))}>{t('关闭二次验证')}</button>
            </div>
          )}
        </section>
        <SSOProviders />
        <Sessions />
        <LoginProtection />
        </>}
        </div>
      </div>
    </>
  )
}
