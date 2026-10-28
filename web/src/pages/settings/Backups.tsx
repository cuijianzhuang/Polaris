import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../../components/SettingsDrafts'
import { ProTag, useFeature } from '../../components/License'
import { useEffect, useRef, useState } from 'react'
import { ApiError, del, download, get, post, put, upload } from '../../api'
import { Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, useToast } from '../../components/ui'
import { bytes, datetime } from '../../format'
import { t } from '../../i18n'

export type RestoreDrill = { needPassphrase?: boolean; id: string; source: string; status: string; startedAt: string; backupAt?: string; durationMs: number; checks: { name: string; state: string; detail?: string }[]; error?: string }

export const drillCheckNames = (): Record<string, string> => ({ archive: t('备份读取'), source_secrets: t('原密钥与备份数据'), restore: t('隔离数据库恢复'), data_and_secrets: t('数据与密钥迁移'), ca_identity: t('CA 身份'), database_restart: t('数据库重新打开'), live_acceptance: t('线上验收') })

export type BackupInfo = { supported: boolean; db: string; dbSize: number; dir: string; store?: string; storeError?: string; encrypted: boolean; targets: string[] | null
  files: { name: string; size: number; created: string; encrypted: boolean }[] }

export const backupKeys = ['backup_schedule', 'backup_hour', 'backup_keep', 'backup_tg', 'backup_webdav_url', 'backup_webdav_user',
  'backup_s3_endpoint', 'backup_s3_region', 'backup_s3_bucket', 'backup_s3_prefix', 'backup_s3_access_key', 'backup_store', 'backup_s3_path_style'] as const

export const secretKeys = ['backup_passphrase', 'backup_webdav_pass', 'backup_s3_secret_key'] as const

export const targetLabel: Record<string, string> = { telegram: 'Telegram', webdav: 'WebDAV', s3: 'S3' }

// Backups: on-demand and scheduled archives (all data + internal CA),
// optionally encrypted and pushed off-site; download, upload-and-restore.
// A restore restarts the panel.
export function Backups() {
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
        <span className="sub" style={{ margin: 0 }}>{d?.db === 'postgres' ? 'PostgreSQL' : `SQLite ${bytes(d?.dbSize)}`} · {t('保存在')} <code style={{ overflowWrap: 'anywhere' }}>{d?.dir}</code></span>
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
