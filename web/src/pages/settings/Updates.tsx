import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../../components/SettingsDrafts'
import { useEffect, useState } from 'react'
import { get, post, put } from '../../api'
import { Field, useAction, useConfirm, useLoad } from '../../components/ui'
import { t } from '../../i18n'

// DefaultVersion picks the version new nodes install: "latest" follows the
// newest stable release, or pin one of the five most recent ones.
export function DefaultVersion({ label, type, value, onChange }: { label: string; type: string; value: string; onChange: (v: string) => void }) {
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

export type UpdateInfo = { current: string; latest?: string; newer?: boolean; updater: boolean; pending?: boolean; error?: string }

// Updates: the running and latest versions, update now (panel, or panel and
// then every agent), and auto update. The panel restarts while updating;
// the outcome arrives as a Telegram message and in the audit log.
export function Updates() {
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
