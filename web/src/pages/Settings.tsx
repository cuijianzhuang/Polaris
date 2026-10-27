import AdminAccounts from '../components/AdminAccounts'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../components/SettingsDrafts'
import { useLocation, useNavigate } from 'react-router-dom'
import Passkeys from '../components/Passkeys'
import { NotificationSettings, NetworkDatabaseSettings } from '../components/NetworkSettings'
import { ProTag, useFeature } from '../components/License'
import { useEffect, useRef, useState } from 'react'
import { ApiError, del, download, get, post, put, setToken, takeOAuthResult, upload } from '../api'
import { APITokens, LoginProtection, RecoveryCodesModal, Sessions, SSOProviders } from '../components/SecurityCards'
import { Icon } from '../components/Icons'
import { accents, styles, useTheme, type ThemeMode } from '../theme'
import { logoURL, refreshBrand, useBrand } from '../brand'
import { CopyBox, Empty, Field, Modal, QR, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { bytes, datetime } from '../format'
import { t } from '../i18n'
import ThemePreview from '../components/ThemePreview'

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

type RestoreDrill = { needPassphrase?: boolean; id: string; source: string; status: string; startedAt: string; backupAt?: string; durationMs: number; checks: { name: string; state: string; detail?: string }[]; error?: string }
const drillCheckNames = (): Record<string, string> => ({ archive: t('备份读取'), source_secrets: t('原密钥与备份数据'), restore: t('隔离数据库恢复'), data_and_secrets: t('数据与密钥迁移'), ca_identity: t('CA 身份'), database_restart: t('数据库重新打开'), live_acceptance: t('线上验收') })

type BackupInfo = { supported: boolean; db: string; dbSize: number; dir: string; store?: string; storeError?: string; encrypted: boolean; targets: string[] | null
  files: { name: string; size: number; created: string; encrypted: boolean }[] }

const backupKeys = ['backup_schedule', 'backup_hour', 'backup_keep', 'backup_tg', 'backup_webdav_url', 'backup_webdav_user',
  'backup_s3_endpoint', 'backup_s3_region', 'backup_s3_bucket', 'backup_s3_prefix', 'backup_s3_access_key', 'backup_store', 'backup_s3_path_style'] as const
const secretKeys = ['backup_passphrase', 'backup_webdav_pass', 'backup_s3_secret_key'] as const
const targetLabel: Record<string, string> = { telegram: 'Telegram', webdav: 'WebDAV', s3: 'S3' }

// Backups: on-demand and scheduled archives (all data + internal CA),
// optionally encrypted and pushed off-site; download, upload-and-restore.
// A restore restarts the panel.
function Backups() {
  const pushOK = useFeature('backup_push')
  const info = useLoad(() => get<BackupInfo>('/backup'))
  const drills = useLoad(() => get<RestoreDrill[]>('/backup/drills'))
  const [drillAsk, setDrillAsk] = useState<string | null>(null)
  const drillLock = useRef(false)
  const [drillPass, setDrillPass] = useState('')
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [baseline, setBaseline] = useState('')
  const saver = useSettingsSave()
  const [sec, setSec] = useState<Record<string, string>>({})
  const [ask, setAsk] = useState<{ label: string; go: (pass: string) => Promise<unknown> } | null>(null)
  const [pass, setPass] = useState('')
  useEffect(() => {
    if (s.data) { const next = Object.fromEntries(backupKeys.map((k) => [k, String(s.data?.[k] ?? '')])); setF(next); setBaseline(JSON.stringify(next)) }
  }, [s.data])
  const { busy: actionBusy, run } = useAction()
  const busy = actionBusy || saver.busy
  const dirty = !!baseline && (JSON.stringify(f) !== baseline || Object.values(sec).some(Boolean))
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
  const drill = async (name: string, passphrase = '') => {
    if (drillLock.current) return
    drillLock.current = true
    try {
      await run(async () => {
        const result = await post<RestoreDrill>(`/backup/files/${encodeURIComponent(name)}/drill`, { passphrase })
        drills.reload()
        if (result.status !== 'passed') {
          if (result.needPassphrase) { setDrillAsk(name); setDrillPass('') }
          throw new Error(result.error || t('恢复演练失败'))
        }
        setDrillAsk(null)
        setDrillPass('')
        toast(t('隔离恢复检查通过，请查看演练记录中的验证范围'))
      })
    } finally { drillLock.current = false }
  }
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const save = async () => {
    if (!dirty) return true
    if (busy || s.loading || !s.data) return false
    return saver.run(async () => {
      const body: Record<string, string> = { ...f }
      for (const k of secretKeys) if (sec[k]) body[k] = sec[k]
      await put('/settings', body); setSec({}); setBaseline(JSON.stringify(f)); s.reload(); info.reload()
    }, t('备份设置已保存'))
  }
  useSettingsDraft({ label: t('备份与恢复'), dirty, busy, save, discard: () => { setF(JSON.parse(baseline || '{}')); setSec({}) } })
  const isSet = (k: string) => !!s.data?.[k + '_set']
  const d = info.data
  const inS3 = f.backup_store === 's3'
  const testS3 = () => run(async () => {
    const r = await post<{ ok: boolean; error?: string }>('/backup/store/test', {
      endpoint: f.backup_s3_endpoint, region: f.backup_s3_region, bucket: f.backup_s3_bucket, prefix: f.backup_s3_prefix,
      accessKey: f.backup_s3_access_key, secretKey: sec.backup_s3_secret_key ?? '', pathStyle: f.backup_s3_path_style !== 'false',
    })
    if (!r.ok) throw new Error(t('连接失败：{err}', { err: r.error ?? '' }))
  }, t('S3 连接正常：写入、读取、列出、删除都成功'))
  const s3Fields = (
    <div style={{ display: 'grid', gap: 12 }}>
      <div className="form-grid">
        <Field label={t('S3 端点')} hint={t('AWS 留空；Cloudflare R2：https://<账户 ID>.r2.cloudflarestorage.com；MinIO / B2 填其地址')}><input className="input mono" value={f.backup_s3_endpoint || ''} placeholder="https://s3.amazonaws.com" onChange={(e) => set('backup_s3_endpoint', e.target.value.trim())} /></Field>
        <Field label={t('区域')} hint={t('R2 填 auto')}><input className="input mono" value={f.backup_s3_region || ''} placeholder="us-east-1" onChange={(e) => set('backup_s3_region', e.target.value.trim())} /></Field>
        <Field label={t('存储桶')}><input className="input mono" value={f.backup_s3_bucket || ''} onChange={(e) => set('backup_s3_bucket', e.target.value.trim())} /></Field>
        <Field label={t('路径前缀')} hint={t('如 polaris/')}><input className="input mono" value={f.backup_s3_prefix || ''} onChange={(e) => set('backup_s3_prefix', e.target.value.trim())} /></Field>
        <Field label="Access Key ID"><input className="input mono" value={f.backup_s3_access_key || ''} onChange={(e) => set('backup_s3_access_key', e.target.value.trim())} /></Field>
        <Field label="Secret Access Key" hint={isSet('backup_s3_secret_key') ? t('已保存（留空不修改）') : undefined}><input className="input mono" type="password" autoComplete="off" value={sec.backup_s3_secret_key ?? ''} onChange={(e) => setSec({ ...sec, backup_s3_secret_key: e.target.value })} /></Field>
      </div>
      <div className="toggle"><span>{t('路径风格地址（端点/存储桶/文件，MinIO 等需要；关闭则用「存储桶.端点」）')}</span><Switch label={t('路径风格地址')} on={f.backup_s3_path_style !== 'false'} onChange={(v) => set('backup_s3_path_style', String(v))} /></div>
      <div className="row"><button className="btn sm" type="button" disabled={busy} onClick={testS3}>{t('测试连接')}</button></div>
    </div>
  )
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <fieldset className="settings-fields" disabled={busy || s.loading || !baseline}>
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
      <div style={{ display: 'grid', gap: 12 }}>
        <Field label={t('备份存储位置')} hint={inS3 ? t('备份直接存到 S3 对象存储，每个主控实例都能列出、下载和恢复') : t('备份保存在本机数据目录')}>
          <select className="input" value={inS3 ? 's3' : 'local'} onChange={(e) => set('backup_store', e.target.value)}>
            <option value="local">{t('本机')}</option><option value="s3">{t('S3 对象存储')}</option>
          </select>
        </Field>
        {!inS3 && d?.db === 'postgres' && <div className="note warn">{t('运行多个主控实例时，请把备份存到 S3：存在本机时，每台只能看到自己机器上的备份。')}</div>}
        {d?.storeError && <div className="note warn">{t('备份存储不可用：{err}', { err: d.storeError })}</div>}
        {inS3 && s3Fields}
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
          {inS3 ? <div className="hint">{t('S3 已用作备份存储，不再重复推送到 S3。')}</div> : s3Fields}
        </div>
      </details>
      <SettingsSaveBar loading={s.loading} dirty={dirty} busy={saver.busy} error={saver.error || s.error} disabled={s.loading || !s.data} onSave={save} label={t('保存设置')} />
      <div className="row">
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
                  <button className="btn sm" type="button" disabled={busy} onClick={() => drill(b.name)}>{t('恢复演练')}</button>
                  <button className="btn sm" type="button" disabled={busy} onClick={() => restore(b.name, (p) => post(`/backup/files/${b.name}/restore`, p ? { passphrase: p } : {}))}>{t('恢复')}</button>
                  <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除备份 {name}？', { name: b.name }), { danger: true })) run(async () => { await del('/backup/files/' + b.name); info.reload() }) }}>{t('删除')}</button>
                </div></td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </div>
      <div className="hint">{t('备份包含全部数据与内部 CA 证书（迁移后已接入的节点无需重装），SQLite 与 PostgreSQL 之间可互相恢复。未加密的备份只能恢复到同一 secret_key 的面板；加密备份附带密钥，新服务器执行')} <code>{t('polaris install --restore 文件')}</code> {t('或安装后')} <code>{t('polaris restore 文件')}</code> {t('即可。')}</div>
      </fieldset>
      <section className="card" aria-label={t('恢复演练记录')}>
        <h3>{t('恢复演练记录')}</h3>
        <p className="hint">{t('在临时数据库验证恢复、密钥迁移和重新打开，完成后清理。不会覆盖当前数据或连接节点；线上登录、付款、通知与授权联网检查需要另外验收。')}</p>
        {drills.error && <div className="alert err" role="alert">{String(drills.error)} <button className="btn sm" type="button" onClick={drills.reload}>{t('重试')}</button></div>}
        {!drills.loading && !drills.error && !drills.data?.length && <div className="hint">{t('暂无演练记录，可在备份文件旁点击恢复演练')}</div>}
        {(drills.data ?? []).map((r) => <details key={r.id} style={{ padding: '10px 0', borderBottom: '1px solid var(--border)' }}>
          <summary><span className={'tag ' + (r.status === 'passed' ? 'ok' : 'bad')}>{r.status === 'passed' ? t('检查通过') : t('失败')}</span> {datetime(r.startedAt)} · {(r.durationMs / 1000).toFixed(1)} s</summary>
          <div className="mono" style={{ overflowWrap: 'anywhere', marginTop: 8 }}>{r.source}</div>
          {r.backupAt && <div className="hint">{t('恢复点')}：{datetime(r.backupAt)}</div>}
          {r.error && <div className="alert err">{r.error}</div>}
          <ul>{r.checks.map((c) => <li key={c.name}>{drillCheckNames()[c.name] || c.name} · {c.state === 'passed' ? t('通过') : c.state === 'skipped' ? t('未验证') : t('失败')}{c.detail && <div className="hint">{c.detail}</div>}</li>)}</ul>
        </details>)}
      </section>
      {drillAsk && <Modal title={t('输入备份口令')} onClose={() => { if (!busy) { setDrillAsk(null); setDrillPass('') } }} footer={<button className="btn pri" type="submit" form="drill-pass" disabled={busy || !drillPass}>{t('恢复演练')}</button>}>
        <form id="drill-pass" onSubmit={(e) => { e.preventDefault(); if (!busy) drill(drillAsk, drillPass) }}>
          <p className="hint">{t('仅在隔离数据库中检查备份，当前数据不受影响。')}</p>
          <input className="input" type="password" autoFocus aria-label={t('备份口令')} value={drillPass} onChange={(e) => setDrillPass(e.target.value)} disabled={busy} />
        </form>
      </Modal>}
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
  { id: 'network', label: t('网络检测'), hint: t('ASN、国家省市归属'), icon: <Icon.Globe /> },
  { id: 'notifications', label: t('通知策略'), hint: t('事件渠道、维护静默、告警合并'), icon: <Icon.Mail /> },
  { id: 'mail', label: t('邮件'), hint: t('SMTP、邮件提醒'), icon: <Icon.Mail /> },
  { id: 'appearance', label: t('外观'), hint: t('主题与配色'), icon: <Icon.Sun /> },
]

const mailKeys = ['smtp_host', 'smtp_port', 'smtp_security', 'smtp_user', 'smtp_from', 'smtp_from_name', 'mail_notify_users', 'mail_notify_admin', 'mail_admin_to'] as const

// MailSettings: the SMTP account, a test message, and who gets mail.
function MailSettings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [baseline, setBaseline] = useState('')
  const saver = useSettingsSave()
  const [pass, setPass] = useState('')
  const [to, setTo] = useState('')
  useEffect(() => {
    if (s.data) { const next = Object.fromEntries(mailKeys.map((k) => [k, String(s.data?.[k] ?? '')])); setF(next); setBaseline(JSON.stringify(next)) }
  }, [s.data])
  const { busy: actionBusy, run } = useAction()
  const busy = actionBusy || saver.busy
  const dirty = !!baseline && (JSON.stringify(f) !== baseline || !!pass)
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const security = f.smtp_security || 'starttls'
  const invalidPort = !!f.smtp_port && (Number(f.smtp_port) < 1 || Number(f.smtp_port) > 65535)
  const save = async () => {
    if (!dirty) return true
    if (busy || s.loading || !s.data || invalidPort) return false
    return saver.run(async () => {
      const body: Record<string, string> = { ...f, smtp_security: security, mail_notify_users: f.mail_notify_users === 'false' ? 'false' : 'true', mail_notify_admin: f.mail_notify_admin === 'false' ? 'false' : 'true' }
      if (pass) body.smtp_password = pass
      await put('/settings', body); setPass(''); setBaseline(JSON.stringify(f)); s.reload()
    }, t('邮件设置已保存'))
  }
  useSettingsDraft({ label: t('邮件'), dirty, busy, invalid: invalidPort, save, discard: () => { setF(JSON.parse(baseline || '{}')); setPass('') } })
  const test = () => run(async () => {
    const r = await post<{ ok: boolean; error?: string }>('/mail/test', { to, host: f.smtp_host, port: f.smtp_port, security, user: f.smtp_user, password: pass, from: f.smtp_from, fromName: f.smtp_from_name })
    if (!r.ok) throw new Error(t('发送失败：{err}', { err: r.error ?? '' }))
  }, t('测试邮件已发出，请查收'))
  const passSet = !!s.data?.smtp_password_set
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('邮件（SMTP）')}</h2>
      <fieldset className="settings-fields" disabled={busy || s.loading || !baseline}>
      <div className="hint">{t('配置后，填了邮箱的用户会收到和 Telegram 相同的提醒（流量、到期、停用、恢复），以及订单开通通知。提醒阈值在「Telegram」页设置，两边共用。')}</div>
      <div className="form-grid">
        <Field label={t('SMTP 服务器')}><input className="input mono" value={f.smtp_host || ''} placeholder="smtp.example.com" onChange={(e) => set('smtp_host', e.target.value.trim())} /></Field>
        <Field label={t('加密方式')}>
          <select className="input" value={security} onChange={(e) => set('smtp_security', e.target.value)}>
            <option value="starttls">STARTTLS（587）</option><option value="tls">SSL/TLS（465）</option><option value="none">{t('不加密（25，仅限内网中继）')}</option>
          </select>
        </Field>
        <Field label={t('端口')} hint={invalidPort ? <span role="alert" className="field-error">{t('端口须为 1–65535，或留空使用默认值。')}</span> : t('留空按加密方式使用默认端口')}><input className="input mono" inputMode="numeric" aria-invalid={invalidPort} value={f.smtp_port || ''} placeholder={security === 'tls' ? '465' : security === 'none' ? '25' : '587'} onChange={(e) => set('smtp_port', e.target.value.replace(/[^\d]/g, ''))} /></Field>
        <Field label={t('用户名')} hint={t('通常是完整邮箱地址；中继不需要时留空')}><input className="input mono" autoComplete="off" value={f.smtp_user || ''} onChange={(e) => set('smtp_user', e.target.value.trim())} /></Field>
        <Field label={t('密码 / 授权码')} hint={passSet ? t('已保存（留空不修改）') : t('QQ / 163 邮箱填授权码，Gmail 填应用专用密码')}><input className="input mono" type="password" autoComplete="new-password" value={pass} placeholder={passSet ? '••••••••' : ''} onChange={(e) => setPass(e.target.value)} /></Field>
        <Field label={t('发件地址')}><input className="input mono" value={f.smtp_from || ''} placeholder="noreply@example.com" onChange={(e) => set('smtp_from', e.target.value.trim())} /></Field>
        <Field label={t('发件人名称')} hint={t('可留空')}><input className="input" value={f.smtp_from_name || ''} onChange={(e) => set('smtp_from_name', e.target.value)} /></Field>
        <Field label={t('管理员收件地址')} hint={t('可留空；开启管理员邮件通知后，新订单、备份失败、升级结果、节点告警等通知发送到这里')}><input className="input mono" value={f.mail_admin_to || ''} placeholder="ops@example.com" onChange={(e) => set('mail_admin_to', e.target.value.trim())} /></Field>
      </div>
      <div className="toggle"><span>{t('给用户发提醒邮件')}</span><Switch label={t('给用户发提醒邮件')} on={f.mail_notify_users !== 'false'} onChange={(v) => set('mail_notify_users', String(v))} /></div>
      <div className="toggle"><span>{t('给管理员发通知邮件')}</span><Switch label={t('给管理员发通知邮件')} on={f.mail_notify_admin !== 'false'} onChange={(v) => set('mail_notify_admin', String(v))} /></div>
      <SettingsSaveBar loading={s.loading} dirty={dirty} busy={saver.busy} error={saver.error || s.error} disabled={s.loading || !s.data || invalidPort} onSave={save} />
      <div className="row">
        <input className="input mono" style={{ width: 'auto', minWidth: 200 }} type="email" value={to} placeholder={t('测试收件地址')} onChange={(e) => setTo(e.target.value)} aria-label={t('测试收件地址')} />
        <button className="btn" type="button" disabled={busy || !to || !f.smtp_host} onClick={test}>{t('发送测试邮件')}</button>
      </div>
      </fieldset>
    </section>
  )
}

// Appearance: mode, accent and surface style are per browser; the logo is
// the panel's (paid features "themes" and "branding").
function Appearance() {
  const [th, setT] = useTheme()
  const themesOK = useFeature('themes')
  const modes: { id: ThemeMode; label: string }[] = [{ id: 'light', label: t('浅色') }, { id: 'dark', label: t('深色') }, { id: 'system', label: t('跟随系统') }]
  const special = th.style === 'blackgold' && themesOK
  const activeStyle = th.style === 'blackgold' && !themesOK ? 'default' : th.style
  const shownMode = special ? 'dark' : th.mode
  return (
    <>
      <section className="card appearance">
        <div className="appearance-heading"><div><h2>{t('外观')}</h2><p>{t('让界面适合你的工作习惯。')}</p></div><span className="appearance-local">{t('仅保存在当前浏览器')}</span></div>
        <Field label={t('主题风格')} hint={t('三套基础主题均支持明暗模式；配色可跟随风格或自选。黑金需要专业版。')}>
          <div className="style-cards">
            {styles.map((x) => {
              const locked = x.premium && !themesOK
              const on = activeStyle === x.id
              return (
                <button key={x.id} type="button" className={'style-card style-' + x.id + (on ? ' on' : '')} aria-pressed={on} disabled={locked}
                  title={locked ? t('需要专业版许可证（许可证页面）') : x.label} onClick={() => setT({ ...th, style: x.id })}>
                  <span className="style-preview" aria-hidden="true"><i /><i /><i /></span>
                  <span className="style-label"><b>{x.label}{locked ? ' 🔒' : ''}</b><small>{x.description}</small></span>
                </button>
              )
            })}
          </div>
        </Field>
        <div className="appearance-layout">
          <div className="appearance-controls">
            <Field label={t('主题模式')} hint={special ? t('当前专属主题固定使用深色及专属配色，切换到基础主题后可调整。') : t('跟随系统会自动匹配设备的明暗模式。')}>
              <div className="seg c3 fixed">
                {modes.map((m) => <button key={m.id} type="button" disabled={special} className={'opt' + (shownMode === m.id ? ' on' : '')} aria-pressed={shownMode === m.id} onClick={() => setT({ ...th, mode: m.id })}>{m.label}</button>)}
              </div>
            </Field>
            <Field label={t('主题色')}>
              <div className="swatches">
                {accents.map((a) => <button key={a.id} type="button" disabled={special} className={'accent-swatch' + (!special && th.accent === a.id ? ' on' : '')}
                  title={a.label} aria-label={t('主题色：{name}', { name: a.label })} aria-pressed={!special && th.accent === a.id} onClick={() => setT({ ...th, accent: a.id })}>
                  <i style={{ background: a.color }} aria-hidden="true" /><span>{a.label}</span><span className="accent-selected" aria-hidden="true">{!special && th.accent === a.id ? '✓' : ''}</span>
                </button>)}
              </div>
            </Field>
          </div>
          <ThemePreview />
        </div>
      </section>
      <BrandLogoCard />
    </>
  )
}

// BrandLogoCard uploads the panel's own logo (sidebar, login page, favicon).
function BrandLogoCard() {
  const ok = useFeature('branding')
  const { logo } = useBrand()
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const pick = (file?: File) => {
    if (!file) return
    if (file.size > 256 << 10) { run(async () => { throw new Error(t('图片不能超过 256 KB')) }); return }
    const r = new FileReader()
    r.onload = () => run(async () => { await put('/brand/logo', { data: String(r.result) }); refreshBrand() }, t('Logo 已更新'))
    r.readAsDataURL(file)
  }
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('自定义 Logo')}<ProTag on={ok} /></h2>
      <div className="row" style={{ gap: 16, flexWrap: 'wrap' }}>
        <div className="logo-preview">{logo ? <img src={logoURL(logo)} alt="" /> : <Icon.LogoTile size={56} />}</div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8, flex: 1, minWidth: 200 }}>
          <div className="small mute">{t('显示在侧边栏、登录页和浏览器标签页图标。支持 PNG、JPG、WebP、GIF、SVG，不超过 256 KB，建议正方形、至少 128×128。')}</div>
          <div className="row" style={{ gap: 8 }}>
            <label className={'btn' + (ok && !busy ? '' : ' disabled')} aria-disabled={!ok || busy}>
              {logo ? t('更换') : t('上传')}
              <input type="file" accept="image/png,image/jpeg,image/webp,image/gif,image/svg+xml" hidden disabled={!ok || busy}
                onChange={(e) => { pick(e.target.files?.[0]); e.target.value = '' }} />
            </label>
            {logo && <button type="button" className="btn" disabled={busy} onClick={async () => {
              if (!await confirm(t('恢复为默认的 Polaris Logo？'), { ok: t('恢复默认') })) return
              run(async () => { await del('/brand/logo'); refreshBrand() }, t('已恢复默认 Logo'))
            }}>{t('恢复默认')}</button>}
          </div>
        </div>
      </div>
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
  const { busy: actionBusy, run } = useAction()
  const saver = useSettingsSave()
  const busy = actionBusy || saver.busy
  const [baseline, setBaseline] = useState('')
  useEffect(() => { if (s.data) { setBaseline(JSON.stringify([s.data.auto_update || 'notify', s.data.auto_update_hour || '4'])) } }, [s.data])
  const confirm = useConfirm()
  const m = mode ?? (s.data?.auto_update || 'notify')
  const h = hour ?? (s.data?.auto_update_hour || '4')
  const dirty = !!baseline && JSON.stringify([m,h]) !== baseline
  const save = async () => {
    if (!dirty) return true
    if (busy || s.loading) return false
    return saver.run(async () => { await put('/settings', { auto_update: m, auto_update_hour: h }); setBaseline(JSON.stringify([m,h])); s.reload() })
  }
  useSettingsDraft({ label: t('版本与更新'), dirty, busy, save, discard: () => { setMode(null); setHour(null) } })
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
          <select className="input" disabled={busy || s.loading || !baseline} value={m} onChange={(e) => setMode(e.target.value)}>
            <option value="off">{t('关闭')}</option>
            <option value="notify">{t('只通知新版本')}</option>
            <option value="panel">{t('自动升级面板')}</option>
            <option value="all">{t('自动升级面板和全部节点')}</option>
          </select>
        </Field>
        <Field label={t('自动升级时间')} hint={t('服务器本地时间；某个版本自动升级失败后不再自动重试，可手动升级')}>
          <select className="input mono" value={h} disabled={busy || s.loading || !baseline || (m !== 'panel' && m !== 'all')} onChange={(e) => setHour(e.target.value)}>
            {Array.from({ length: 24 }, (_, i) => <option key={i} value={String(i)}>{String(i).padStart(2, '0')}:00</option>)}
          </select>
        </Field>
      </div>
      <SettingsSaveBar loading={s.loading} dirty={dirty} busy={saver.busy} error={saver.error || s.error} disabled={busy || s.loading || !s.data} onSave={save} />
    </section>
  )
}

type ClusterInfo = { operations: {id: string; kind: string; target: string; createdAt: string}[]; deliveries: {operationId: string; memberId: string; status: string; lastError: string}[]; shared: boolean; members: { id: string; host: string; version: string; edition: string; startedAt: string; seenAt: string; online: boolean; leader: boolean; self: boolean }[] }

// Cluster: the panel instances sharing this database (shown only when there
// is more than one). The leader runs the scheduled jobs; "update" updates
// every running instance.
function Cluster() {
  const info = useLoad(() => get<ClusterInfo>('/cluster'))
  useEffect(() => {
    const id = setInterval(() => info.reload(), 15000)
    return () => clearInterval(id)
  }, [])
  const ms = info.data?.members ?? []
  const ops = info.data?.operations ?? []
  const deliveries = info.data?.deliveries ?? []
  if (ms.length < 2 && ops.length === 0) return null
  const running = ms.filter((m) => m.online)
  const versions = new Set(running.map((m) => m.version))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('集群')}</h2>
        <span className="sub" style={{ margin: 0 }}>{t('{n} 个主控实例运行中', { n: running.length })}</span>
      </div>
      {versions.size > 1 && <div className="note warn">{t('各实例版本不一致，请等待升级完成或检查升级失败的实例。')}</div>}
      <div className="card flush scroll">
        <table className="rtable">
          <thead><tr><th>{t('主机')}</th><th>{t('版本')}</th><th>{t('启动时间')}</th><th>{t('最后在线')}</th></tr></thead>
          <tbody>{ms.map((m) => (
            <tr key={m.id}>
              <td className="rt-head mono">{m.host || m.id.slice(0, 8)}
                {m.leader && <span className="tag ok" style={{ marginLeft: 6 }}>{t('主实例')}</span>}
                {m.self && <span className="tag" style={{ marginLeft: 6 }}>{t('当前')}</span>}
                {!m.online && <span className="tag warn" style={{ marginLeft: 6 }}>{t('离线')}</span>}</td>
              <td data-label={t('版本')} className="mono">{m.version}</td>
              <td data-label={t('启动时间')}>{datetime(m.startedAt)}</td>
              <td data-label={t('最后在线')}>{datetime(m.seenAt)}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
      {ops.slice(0, 5).map((op) => {
        const ds = deliveries.filter((d) => d.operationId === op.id)
        return <div key={op.id} className="hint">{op.kind === 'update' ? t('升级面板') : t('恢复后重启')} {op.target} · {t('已受理 {n}/{m}', { n: ds.filter((d) => d.status === 'accepted').length, m: ds.length })}
          {ds.filter((d) => d.lastError).map((d) => <div key={d.memberId} className="note warn">{d.lastError}</div>)}</div>
      })}
      <div className="hint">{t('受理表示请求已交给本机处理，实际升级结果以版本和升级通知为准。')}</div>
      <div className="hint">{t('主实例负责定时任务和 Telegram 机器人，停止后其他实例 15 秒内接管。「升级面板」会升级全部运行中的实例；恢复备份后全部实例自动重启。离线超过 1 天的实例自动移除。')}</div>
    </section>
  )
}

function OperationalHealth() {
  const info = useLoad(() => get<{ issues: {key: string; detail: string}[]; states: {key: string; lastSuccess?: string}[] }>('/operational-health'))
  useEffect(() => { const id = setInterval(() => info.reload(), 15000); return () => clearInterval(id) }, [])
  const backup = info.data?.states?.find((s) => s.key === 'backup')
  return <section className="card">
    <h2>{t('运行状态')}</h2><p className="hint">{t('付款交付、退款、流量队列、USDT 核验与备份状态')}</p>
    {info.error && <div className="note warn">{t('运行状态暂不可用')}</div>}
    {info.data && (info.data.issues.length ? info.data.issues.map((i) => <div key={i.key} className="note warn">{i.detail}</div>) : <div className="note ok">{t('未检测到运行告警')}</div>)}
    {backup?.lastSuccess && <div className="hint">{t('最近成功备份')}：{datetime(backup.lastSuccess)}</div>}
  </section>
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

  const accessOriginal: Record<string, string> = { path: d?.adminPath ?? '', domain: d?.domain ?? '', sub: d?.subDomain ?? '', port, url: d?.publicUrl ?? '' }
  const accessDirty = !moving && Object.entries(f).some(([key, value]) => value !== undefined && value !== accessOriginal[key])
  useSettingsDraft({ label: t('面板访问'), dirty: accessDirty, busy, discard: () => setF({}) })

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
        <Field label={t('面板域名（HTTPS）')} hint={<details className="field-help"><summary>{t('配置前请阅读')}</summary><div>{t('先把域名 A 记录解析到本服务器；会安装 Caddy 自动签发证书并放行 80 / 443。已有 Nginx / OpenResty / 宝塔等占用 443 时不要用这里：在它们里反代到本机面板端口，再改下方「公开地址」')}</div></details>}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.domain ?? d?.domain ?? ''} placeholder="panel.example.com" onChange={(e) => setF({ ...f, domain: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.domain?.trim() || f.domain.trim() === d?.domain}
              onClick={() => apply('set-domain', f.domain!.trim(), 'https://' + f.domain!.trim().toLowerCase() + adminPath + '/', t('正在安装 Caddy 并签发证书，通常需要半分钟到一分钟。'),
                t('面板将改用 https://{domain} 访问，订阅链接也会变成这个域名（用户需重新导入；不想变可在订阅设置里填订阅域名）。已接入的节点不受影响。', { domain: f.domain!.trim() }))}>{t('启用 HTTPS')}</button>
          </div>
        </Field>
        <Field label={t('订阅域名（HTTPS）')} hint={<details className="field-help"><summary>{t('配置前请阅读')}</summary><div>{t('给订阅链接单独一个域名（可走 CDN）：先把域名解析到本服务器，这里会用 Caddy 签发证书，完成后订阅链接自动改用它。该域名只开放订阅和用户中心，后台地址不变。已有 Nginx / OpenResty / 宝塔占用 80、443 时，先在它们里把域名反代到本机面板端口，再点「配置」（会检测反代是否生效，不安装 Caddy）')}</div></details>}>
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
  const [generalBaseline, setGeneralBaseline] = useState('')
  const generalSaver = useSettingsSave()
  const [pw, setPw] = useState({ old: '', new: '', again: '' })
  const [showPw, setShowPw] = useState(false)
  const [totp, setTotp] = useState<{ secret: string; url: string } | null>(null)
  const [code, setCode] = useState('')
  const { busy, run } = useAction()
  useEffect(() => {
    if (s.data) { const next = {
      site_title: s.data.site_title ?? '', sub_update_interval: s.data.sub_update_interval ?? '1', ip_ban_minutes: s.data.ip_ban_minutes ?? '0',
      acme_email: s.data.acme_email ?? '', acme_cf_token: '', default_xray: s.data.default_xray || 'latest', default_singbox: s.data.default_singbox || 'latest',
      ip_limit_trusted: s.data.ip_limit_trusted ?? '',
    }; setF(next); setGeneralBaseline(JSON.stringify(next)) }
  }, [s.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const settingsLocation = useLocation()
  const navigate = useNavigate()
  const tab = settingTabs.some((item) => '#' + item.id === settingsLocation.hash) ? settingsLocation.hash.slice(1) : 'general'
  const generalDirty = !!generalBaseline && JSON.stringify(f) !== generalBaseline
  const saveGeneral = async () => {
    if (!generalDirty) return true
    if (generalSaver.busy || s.loading || !s.data) return false
    return generalSaver.run(async () => { await put('/settings', f); setGeneralBaseline(JSON.stringify(f)); s.reload(); refreshBrand() })
  }
  useSettingsDraft({ label: t('常规'), dirty: tab === 'general' && generalDirty, busy: generalSaver.busy, save: saveGeneral, discard: () => setF(JSON.parse(generalBaseline || '{}')) })
  useSettingsDraft({ label: t('账号安全'), dirty: tab === 'security' && (Object.values(pw).some(Boolean) || !!code || !!ghForm.secret || (!!gh.data && ghForm.id !== gh.data.clientId)), busy: tab === 'security' && busy,
    discard: () => { setPw({ old: '', new: '', again: '' }); setCode(''); setGhForm({ id: gh.data?.clientId || '', secret: '' }) } })
  const go = (id: string) => { if (id !== tab) void navigate({ hash: '#' + id }, { replace: true }) }
  const settingsNav = useRef<HTMLElement>(null)
  const [moreTabs, setMoreTabs] = useState(false)
  const updateTabOverflow = () => { const nav = settingsNav.current; setMoreTabs(!!nav && nav.scrollLeft + nav.clientWidth < nav.scrollWidth - 2) }
  useEffect(() => {
    const nav = settingsNav.current
    if (!nav) return
    const observer = new ResizeObserver(updateTabOverflow)
    observer.observe(nav); updateTabOverflow()
    return () => observer.disconnect()
  }, [])
  useEffect(() => {
    const nav = settingsNav.current
    const active = nav?.querySelector<HTMLElement>('[aria-current="page"]')
    if (nav && active && nav.scrollWidth > nav.clientWidth) {
      nav.scrollTo({ left: nav.scrollLeft + active.getBoundingClientRect().left - nav.getBoundingClientRect().left - (nav.clientWidth - active.clientWidth) / 2 })
    }
    updateTabOverflow()
  }, [tab])


  return (
    <>
      <div className="top"><div><h1>{t('设置')}</h1><div className="sub">{t('面板、证书、账号安全、备份与外观')}</div></div></div>
      <div className="settings">
        <div className="settings-nav-shell" data-more={moreTabs}>
        <nav ref={settingsNav} onScroll={updateTabOverflow} className="snav" aria-label={t('设置分区')}>
          {settingTabs.map((t) => <button key={t.id} type="button" className={'snav-item' + (tab === t.id ? ' on' : '')} aria-current={tab === t.id ? 'page' : undefined} title={t.hint} onClick={() => go(t.id)}>{t.icon}<span><b>{t.label}</b><small>{t.hint}</small></span></button>)}
        </nav>
        </div>
        <div className="spane" key={tab}>
        {tab === 'network' && <NetworkDatabaseSettings />}
        {tab === 'notifications' && <NotificationSettings />}
        {tab === 'general' && <>
        <Access />
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('常规')}</h2>
          <fieldset className="settings-fields" disabled={generalSaver.busy || s.loading || !generalBaseline}>
          <div className="form-grid">
            <Field label={t('站点标题')} full><input className="input" value={f.site_title ?? ''} onChange={(e) => set('site_title', e.target.value)} /></Field>
            <Field label={t('订阅更新间隔（小时）')}><input className="input mono" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
            <DefaultVersion label={t('默认 Xray 版本')} type="xray" value={f.default_xray ?? ''} onChange={(v) => set('default_xray', v)} />
            <DefaultVersion label={t('默认 sing-box 版本')} type="singbox" value={f.default_singbox ?? ''} onChange={(v) => set('default_singbox', v)} />
          </div>
          <details className="settings-disclosure"><summary>{t('高级访问限制')}</summary><div className="form-grid">
            <Field label={t('IP 超限封禁（分钟）')} hint={t('0 为只告警不封禁')}><input className="input mono" value={f.ip_ban_minutes ?? ''} onChange={(e) => set('ip_ban_minutes', e.target.value)} /></Field>
            <Field label={t('IP 限制受信任地址')} full hint={t('每行一个 IP 或 CIDR，不计入用户的同时在线 IP 数（如家庭 / 公司出口、自己的中转节点）')}>
              <textarea className="input mono" style={{ minHeight: 56 }} value={f.ip_limit_trusted ?? ''} onChange={(e) => set('ip_limit_trusted', e.target.value)} placeholder={'203.0.113.10\n198.51.100.0/24'} />
            </Field>
          </div></details>
          <details className="settings-disclosure"><summary>{t('证书申请与验证')}</summary><div className="settings-fields">
          <b>ACME（Let's Encrypt）</b>
          <div className="hint" style={{ marginTop: -6 }}>{t('只填邮箱即可用 HTTP 验证签发（由域名指向的节点在 80 端口应答）；再填 Cloudflare Token 则改用 DNS 验证，支持泛域名、无需 80 端口。')}</div>
          <div className="form-grid">
            <Field label={t('邮箱')}><input className="input" type="email" value={f.acme_email ?? ''} onChange={(e) => set('acme_email', e.target.value)} /></Field>
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? t('已保存（留空不修改）') : t('可选 · 需要 Zone.DNS 编辑权限')}>
              <input className="input mono" type="password" autoComplete="off" value={f.acme_cf_token ?? ''} onChange={(e) => set('acme_cf_token', e.target.value)} />
            </Field>
          </div>
          </div></details>
          <SettingsSaveBar loading={s.loading} dirty={generalDirty} busy={generalSaver.busy} error={generalSaver.error || s.error} disabled={s.loading || !s.data} onSave={saveGeneral} />
          </fieldset>
        </section>
        <details className="card settings-disclosure"><summary>{t('部署信息')}</summary><div className="settings-fields">
          <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
            <dt>{t('版本')}</dt><dd className="mono">{s.data?.version}</dd>
            <dt>{t('后台路径')}</dt><dd className="mono">{s.data?.admin_path}</dd>
            <dt>{t('公开地址')}</dt><dd className="mono">{s.data?.public_url || t('未设置（POLARIS_PUBLIC_URL）')}</dd>
            <dt>{t('Agent 接入点')}</dt><dd className="mono">{s.data?.agent_url}</dd>
          </dl>
          <Field label={t('CA 指纹（SHA-256）')} hint={t('安装命令携带此指纹，Agent 首次接入时校验主控身份')}><CopyBox text={s.data?.ca_sha256 ?? ''} /></Field>
          <div className="note">{t('后台路径、域名、端口和公开地址可在上方「面板访问」修改；数据库与加密密钥通过 /etc/polaris/polaris.env 设置（见 README）。使用后台路径时，其他路径一律返回 404 或伪装站点。')}</div>
        </div></details>
        </>}
        {tab === 'update' && <><Updates /><Cluster /><OperationalHealth /></>}
        {tab === 'appearance' && <Appearance />}
        {tab === 'backup' && <Backups />}
        {tab === 'mail' && <MailSettings />}
        {tab === 'security' && <>
 <Passkeys />
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
        <AdminAccounts />
        <SSOProviders />
        <APITokens />
        <Sessions />
        <LoginProtection />
        </>}
        </div>
      </div>
    </>
  )
}
