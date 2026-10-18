import { useEffect, useState } from 'react'
import { get, post } from '../api'
import { t } from '../i18n'
import { Icon } from './Icons'
import { CopyBox, Field, useAction, useConfirm, useLoad } from './ui'

export type LicenseState = {
  plan: string; source: string; limits: { nodes: number; users: number }; features: string[]
  expiresAt?: string; updatesUntil?: string; trialUntil?: string; warnings: string[]
}
type LicenseView = {
  state: LicenseState; instance: string; usage: { nodes: number; users: number }; server: boolean; keys: boolean; edition?: 'pro' | 'community'
  serverStatus?: 'unconfigured' | 'unchecked' | 'available' | 'unavailable'; serverCheckedAt?: string; allFeatures?: string[]
  license?: { id: string; customer: string; plan: string; offline: boolean; issuedAt: string; expiresAt?: string; updatesUntil?: string; features?: string[]; allGranted?: boolean }
  invalid?: boolean; checkedAt?: string; warning?: string
}

const planLabel = (p: string) => ({ community: t('社区版'), trial: t('试用'), pro: t('专业版'), enterprise: t('企业版'), open: t('未启用授权') } as Record<string, string>)[p] ?? p
const featureInfo = (): Record<string, [string, string]> => ({
  speed_limit: [t('客户独立限速'), t('按用户 / 分组限制上下行速率')],
  tg_menu: [t('Telegram 管理菜单'), t('在 Telegram 里管理用户和节点')],
  backup_push: [t('备份推送'), t('定时推送备份到 S3 / WebDAV / Telegram')],
  sub_convert: [t('订阅转换'), t('订阅源、订阅组与规则模板')],
  ddns: [t('DDNS 与域名绑定'), t('节点 IP 变化时自动更新解析')],
  branding: [t('自定义 Logo'), t('侧边栏、登录页和浏览器图标换成你自己的 Logo')],
  themes: [t('专属主题'), t('黑金等专属界面风格')],
})
const knownFeatures = ['speed_limit', 'tg_menu', 'backup_push', 'sub_convert', 'ddns', 'branding', 'themes']

const daysUntil = (d?: string) => d ? Math.ceil((new Date(d + 'T23:59:59').getTime() - Date.now()) / 864e5) : null

function Meter({ label, used, max, icon }: { label: string; used: number; max: number; icon: JSX.Element }) {
  const pct = max > 0 ? Math.min(100, (used / max) * 100) : 0
  return (
    <div className="card kpi">
      <span className="kpi-ico">{icon}</span>
      <div className="label">{label}</div>
      <div className="val">{used}<span className="of"> / {max > 0 ? max : t('不限')}</span></div>
      <div className="foot">{max > 0
        ? <div className="bar" style={{ marginTop: 4 }}><div style={{ width: pct + '%', background: pct >= 100 ? 'var(--bad)' : undefined }} /></div>
        : t('当前许可证不限数量')}</div>
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
  const [mode, setMode] = useState<'key' | 'text'>('key')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const d = v.data
  if (!d) return <section className="card">{t('加载中…')}</section>
  const st = d.state
  const lic = d.license
  const features = d.allFeatures?.length ? d.allFeatures : knownFeatures
  const trialDays = st.trialUntil ? Math.max(0, Math.ceil((new Date(st.trialUntil).getTime() - Date.now()) / 864e5)) : 0
  const expDays = daysUntil(st.expiresAt)
  const licensed = st.source === 'license' || st.plan === 'open'
  const serverStatus = d.serverStatus ?? (d.server ? 'unchecked' : 'unconfigured')
  const serverStatusLabel = {
    unconfigured: t('未配置'), unchecked: t('尚未校验'), available: t('连接正常'), unavailable: t('连接异常'),
  }[serverStatus]
  const apply = (r: LicenseView | undefined) => { if (r?.warning) throw new Error(r.warning) }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      {/* Plan */}
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div className="row" style={{ gap: 14, alignItems: 'center' }}>
            <span className="kpi-ico" style={{ position: 'static', width: 44, height: 44 }}><Icon.Lock /></span>
            <div>
              <div className="row" style={{ gap: 8 }}>
                <h2 style={{ margin: 0 }}>{planLabel(st.plan)}</h2>
                <span className={'tag ' + (licensed ? 'ok' : st.plan === 'trial' ? 'warn' : '')}>
                  {licensed ? t('已授权') : st.plan === 'trial' ? t('试用中') : t('未激活')}</span>
                {lic?.offline && <span className="tag">{t('离线许可证')}</span>}
              </div>
              <div className="sub">{lic ? <>{lic.customer || '—'} · <span className="mono">{lic.id}</span></> : t('输入激活码或导入许可证文件即可启用专业版功能')}</div>
            </div>
          </div>
          {lic && !lic.offline && d.server && <button className="btn" type="button" disabled={busy}
            onClick={() => run(async () => { const r = await post<LicenseView>('/license/check'); v.reload(); apply(r) }, t('已从授权服务器更新'))}>
            <Icon.Refresh /> {t('立即校验')}</button>}
        </div>
        {d.edition === 'community' && <div className="note">{t('当前是社区版构建（从开源代码编译），不包含高级功能。官方发布版包含专业版功能，安装许可证后即可启用。')}</div>}
        {st.plan === 'open' && <div className="note">{t('当前版本未启用授权校验，所有功能可用。')}</div>}
        {st.plan === 'trial' && <div className="note warn">{t('高级功能试用中，还剩 {n} 天；到期后已生效的配置保持不变，只是不能再修改。', { n: trialDays })}</div>}
        {d.invalid && <div className="note bad">{t('已安装的许可证对本面板无效（签名、实例或版本不符），请重新激活。')}</div>}
        {st.warnings.map((w) => <div key={w} className="note warn">{w}</div>)}
        {d.warning && <div className="note warn">{d.warning}</div>}
      </section>

      {/* Dates and usage */}
      <div className="grid g4">
        <div className="card kpi">
          <span className={'kpi-ico ' + (expDays !== null && expDays <= 30 ? 'warn' : 'ok')}><Icon.Wallet /></span>
          <div className="label">{t('到期')}</div>
          <div className="val" style={{ fontSize: 22 }}>{st.expiresAt || (lic ? t('永久') : '—')}</div>
          <div className="foot">{expDays !== null ? (expDays >= 0 ? t('还剩 {n} 天', { n: expDays }) : t('已过期')) : lic ? t('买断授权，不会到期') : '—'}</div>
        </div>
        <div className="card kpi">
          <span className="kpi-ico violet"><Icon.Download /></span>
          <div className="label">{t('更新截止')}</div>
          <div className="val" style={{ fontSize: 22 }}>{st.updatesUntil || (lic ? t('所有版本') : '—')}</div>
          <div className="foot">{st.updatesUntil ? t('此日期前发布的版本都可以使用') : lic ? t('可以升级到任何新版本') : '—'}</div>
        </div>
        <Meter label={t('节点')} used={d.usage.nodes} max={st.limits.nodes} icon={<Icon.Server />} />
        <Meter label={t('用户')} used={d.usage.users} max={st.limits.users} icon={<Icon.Users />} />
      </div>

      {/* Features */}
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between', marginBottom: 12 }}>
          <h3>{t('高级功能')}</h3>
          {lic?.allGranted && licensed && <span className="tag ok">{t('全部高级功能（含以后新增）')}</span>}
        </div>
        <div className="grid g3" style={{ gap: 10 }}>
          {features.map((f) => {
            const on = st.features.includes(f)
            const info = featureInfo()[f]
            return (
              <div key={f} className="row" style={{ flexWrap: 'nowrap', gap: 10, padding: '10px 12px', border: '1px solid var(--line)', borderRadius: 10, background: on ? 'var(--ok-bg)' : undefined, opacity: on ? 1 : .75 }}>
                <span style={{ fontSize: 16 }}>{on ? '✓' : '🔒'}</span>
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontWeight: 600, color: on ? 'var(--ok)' : undefined }}>{info ? info[0] : f}</div>
                  {info && <div className="small mute">{info[1]}</div>}
                </div>
              </div>
            )
          })}
        </div>
        {lic && licensed && !lic.allGranted && features.some((f) => !st.features.includes(f)) &&
          <div className="hint" style={{ marginTop: 10 }}>{t('授权后台调整了功能后，点上方「立即校验」即可更新；否则会在每日联网校验时自动更新。')}</div>}
      </section>

      {/* Activation */}
      {d.keys && <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <h3>{lic ? t('更换许可证') : t('激活')}</h3>
          <div className="seg-mini row" role="tablist">
            <button type="button" className={'btn sm' + (mode === 'key' ? ' pri' : '')} onClick={() => setMode('key')}>{t('激活码')}</button>
            <button type="button" className={'btn sm' + (mode === 'text' ? ' pri' : '')} onClick={() => setMode('text')}>{t('许可证文件')}</button>
          </div>
        </div>
        {mode === 'key' ? <>
          {!d.server && <div className="note warn">{t('授权服务未配置，请联系服务提供方，或改用许可证文件。')}</div>}
          <Field label={t('激活码')} hint={t('联网激活，许可证会绑定到本面板（实例 ID）')}>
            <div className="row" style={{ flexWrap: 'nowrap' }}>
              <input className="input mono" value={key} onChange={(e) => setKey(e.target.value)} placeholder="XXXXX-XXXXX-XXXXX-XXXXX" disabled={!d.server} style={{ letterSpacing: 1 }} />
              <button className="btn pri" type="button" disabled={busy || !key.trim() || !d.server} onClick={() => run(async () => { try { await post('/license/activate', { key }); setKey('') } finally { v.reload() } }, t('已激活'))}>{t('激活')}</button>
            </div>
          </Field>
        </> : <>
          <Field label={t('粘贴许可证')} hint={t('离线许可证或授权后台下载的许可证文本；也可以直接导入 .lic 文件')}>
            <textarea className="input mono" rows={3} value={text} onChange={(e) => setText(e.target.value)} />
          </Field>
          <div className="row">
            <button className="btn pri" type="button" disabled={busy || !text.trim()} onClick={() => run(async () => { await post('/license', { text }); setText(''); v.reload() }, t('许可证已安装'))}>{t('安装许可证')}</button>
            <label className={'btn' + (busy ? ' disabled' : '')} style={{ cursor: 'pointer' }}>
              {t('导入许可证文件')}
              <input type="file" accept=".lic,.txt,text/plain" hidden disabled={busy} onChange={(e) => {
                const file = e.target.files?.[0]
                e.target.value = '' // the same file can be chosen again
                if (file) run(async () => { await post('/license', { text: (await file.text()).trim() }); v.reload() }, t('许可证已安装'))
              }} />
            </label>
          </div>
        </>}
      </section>}

      {/* This panel and the license server */}
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <h3>{t('本机与授权服务器')}</h3>
        <Field label={t('实例 ID')} hint={t('授权后台生成许可证文件时需要填写')}><CopyBox text={d.instance} /></Field>
        {d.keys && <Field label={t('授权服务器状态')} hint={t('状态根据最近一次授权通信结果更新')}>
          <div className="row" role="status">
            <span className={'tag ' + (serverStatus === 'available' ? 'ok' : serverStatus === 'unavailable' ? 'bad' : 'warn')}>{serverStatusLabel}</span>
            {d.serverCheckedAt && <span className="small mute">{t('最近通信：{t}', { t: new Date(d.serverCheckedAt).toLocaleString() })}</span>}
          </div>
        </Field>}
        {d.checkedAt && lic && !lic.offline && <div className="small mute">{t('最近联网校验：{t}', { t: new Date(d.checkedAt).toLocaleString() })}</div>}
        {lic && <div className="row"><button className="btn danger" type="button" disabled={busy} onClick={async () => {
          if (await confirm(t('停用后本面板恢复为社区版，许可证可以在另一台面板上激活。确定停用？'), { danger: true }))
            run(async () => { const r = await post<LicenseView>('/license/deactivate'); v.reload(); apply(r) }, t('已停用'))
        }}>{t('停用（迁移）')}</button></div>}
      </section>
    </div>
  )
}

// useFeature: whether a paid feature is licensed (fetched once per page load;
// while loading, features count as available so nothing flickers locked).
let cached: Promise<LicenseState> | null = null
export function useFeature(f: string): boolean {
  const [ok, setOk] = useState(true)
  useEffect(() => {
    if (!cached) cached = get<LicenseView>('/license').then((v) => v.state).catch(() => ({ features: knownFeatures } as LicenseState))
    let live = true
    cached.then((st) => { if (live) setOk(st.features.includes(f)) })
    return () => { live = false }
  }, [f])
  return ok
}

// ProTag marks a locked paid feature.
export function ProTag({ on }: { on: boolean }) {
  return on ? null : <span className="tag warn" style={{ marginLeft: 6 }} title={t('需要专业版许可证（许可证页面）')}>🔒 {t('专业版')}</span>
}
