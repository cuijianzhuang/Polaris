import { useEffect, useState } from 'react'
import { get, post } from '../api'
import { t } from '../i18n'
import { Field, useAction, useConfirm, useLoad } from './ui'

export type LicenseState = {
  plan: string; source: string; limits: { nodes: number; users: number }; features: string[]
  expiresAt?: string; updatesUntil?: string; trialUntil?: string; warnings: string[]
}
type LicenseView = {
  state: LicenseState; instance: string; usage: { nodes: number; users: number }; server: boolean; keys: boolean
  license?: { id: string; customer: string; plan: string; offline: boolean; issuedAt: string; expiresAt?: string; updatesUntil?: string }
  invalid?: boolean; checkedAt?: string; warning?: string
}

const planLabel = (p: string) => ({ community: t('社区版'), trial: t('试用'), pro: t('专业版'), enterprise: t('企业版'), open: t('未启用授权') } as Record<string, string>)[p] ?? p
const featureLabel = (f: string) => ({ speed_limit: t('客户独立限速'), tg_menu: t('Telegram 管理菜单'), backup_push: t('备份推送') } as Record<string, string>)[f] ?? f
const allFeatures = ['speed_limit', 'tg_menu', 'backup_push']

function Usage({ label, used, max }: { label: string; used: number; max: number }) {
  const pct = max > 0 ? Math.min(100, (used / max) * 100) : 0
  return (
    <div>
      <div className="row small" style={{ justifyContent: 'space-between' }}><span>{label}</span><span className="mono">{used} / {max > 0 ? max : t('不限')}</span></div>
      {max > 0 && <div className="bar" style={{ marginTop: 4 }}><div style={{ width: pct + '%', background: pct >= 100 ? 'var(--bad)' : undefined }} /></div>}
    </div>
  )
}

// License: the plan in effect, usage against its limits, and installing,
// activating or releasing a license.
export default function License() {
  const v = useLoad(() => get<LicenseView>('/license'))
  cached = null // a change here refreshes the locks elsewhere
  const [key, setKey] = useState('')
  const [text, setText] = useState('')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const d = v.data
  if (!d) return <section className="card">{t('加载中…')}</section>
  const st = d.state
  const days = st.trialUntil ? Math.max(0, Math.ceil((new Date(st.trialUntil).getTime() - Date.now()) / 864e5)) : 0
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('许可证')}</h2>
        <span className={'tag ' + (st.source === 'license' || st.plan === 'open' ? 'ok' : st.plan === 'trial' ? 'warn' : '')}>{planLabel(st.plan)}</span>
      </div>
      {st.plan === 'open' && <div className="note">{t('当前版本未启用授权校验，所有功能可用。')}</div>}
      {st.plan === 'trial' && <div className="note warn">{t('高级功能试用中，还剩 {n} 天；到期后已生效的配置保持不变，只是不能再修改。', { n: days })}</div>}
      {d.invalid && <div className="note bad">{t('已安装的许可证对本面板无效（签名、实例或版本不符），请重新激活。')}</div>}
      {st.warnings.map((w) => <div key={w} className="note warn">{w}</div>)}
      {d.warning && <div className="note warn">{d.warning}</div>}

      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        {d.license && <><dt>{t('客户')}</dt><dd>{d.license.customer || '—'} <span className="small mute mono">{d.license.id}</span></dd></>}
        <dt>{t('到期')}</dt><dd className="mono">{st.expiresAt || (d.license ? t('永久') : '—')}</dd>
        {st.updatesUntil && <><dt>{t('更新截止')}</dt><dd className="mono">{st.updatesUntil}</dd></>}
        <dt>{t('实例 ID')}</dt><dd className="mono" style={{ wordBreak: 'break-all' }}>{d.instance}</dd>
        {d.checkedAt && d.license && !d.license.offline && <><dt>{t('联网校验')}</dt><dd className="small">{new Date(d.checkedAt).toLocaleString()}</dd></>}
        <dt>{t('高级功能')}</dt>
        <dd>{allFeatures.map((f) => <span key={f} className={'tag ' + (st.features.includes(f) ? 'ok' : '')} style={{ marginRight: 6 }}>{st.features.includes(f) ? '✓ ' : '🔒 '}{featureLabel(f)}</span>)}</dd>
      </dl>
      <div className="form-grid">
        <Usage label={t('节点')} used={d.usage.nodes} max={st.limits.nodes} />
        <Usage label={t('用户')} used={d.usage.users} max={st.limits.users} />
      </div>

      {d.keys && <>
        <Field label={t('激活码')} hint={d.server ? t('联网激活，许可证会绑定到本面板（实例 ID）') : t('未配置授权服务器，请改用下方粘贴许可证')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={key} onChange={(e) => setKey(e.target.value)} placeholder="XXXX-XXXX-XXXX" disabled={!d.server} />
            <button className="btn pri" type="button" disabled={busy || !key.trim() || !d.server} onClick={() => run(async () => { await post('/license/activate', { key }); setKey(''); v.reload() }, t('已激活'))}>{t('激活')}</button>
          </div>
        </Field>
        <Field label={t('粘贴许可证')} hint={t('离线许可证或授权后台下载的许可证文本；也可以直接导入 .lic 文件')}>
          <textarea className="input mono" rows={3} value={text} onChange={(e) => setText(e.target.value)} />
        </Field>
        <div className="row">
          <button className="btn" type="button" disabled={busy || !text.trim()} onClick={() => run(async () => { await post('/license', { text }); setText(''); v.reload() }, t('许可证已安装'))}>{t('安装许可证')}</button>
          <label className={'btn' + (busy ? ' disabled' : '')} style={{ cursor: 'pointer' }}>
            {t('导入许可证文件')}
            <input type="file" accept=".lic,.txt,text/plain" hidden disabled={busy} onChange={(e) => {
              const file = e.target.files?.[0]
              e.target.value = '' // the same file can be chosen again
              if (file) run(async () => { await post('/license', { text: (await file.text()).trim() }); v.reload() }, t('许可证已安装'))
            }} />
          </label>
          {d.license && <button className="btn danger" type="button" disabled={busy} onClick={async () => {
            if (await confirm(t('停用后本面板恢复为社区版，许可证可以在另一台面板上激活。确定停用？'), { danger: true }))
              run(async () => { const r = await post<LicenseView>('/license/deactivate'); v.reload(); if (r?.warning) throw new Error(r.warning) }, t('已停用'))
          }}>{t('停用（迁移）')}</button>}
        </div>
      </>}
    </section>
  )
}

// useFeature: whether a paid feature is licensed (fetched once per page load;
// while loading, features count as available so nothing flickers locked).
let cached: Promise<LicenseState> | null = null
export function useFeature(f: string): boolean {
  const [ok, setOk] = useState(true)
  useEffect(() => {
    if (!cached) cached = get<LicenseView>('/license').then((v) => v.state).catch(() => ({ features: allFeatures } as LicenseState))
    let live = true
    cached.then((st) => { if (live) setOk(st.features.includes(f)) })
    return () => { live = false }
  }, [f])
  return ok
}

// ProTag marks a locked paid feature.
export function ProTag({ on }: { on: boolean }) {
  return on ? null : <span className="tag warn" style={{ marginLeft: 6 }} title={t('需要专业版许可证（系统 → 许可证）')}>🔒 {t('专业版')}</span>
}
