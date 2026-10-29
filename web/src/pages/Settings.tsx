import AdminAccounts from '../components/AdminAccounts'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../components/SettingsDrafts'
import { useLocation, useNavigate } from 'react-router-dom'
import Passkeys from '../components/Passkeys'
import { NotificationSettings, NetworkDatabaseSettings } from '../components/NetworkSettings'
import { useEffect, useRef, useState } from 'react'
import { get, post, put, setToken, takeOAuthResult } from '../api'
import { APITokens, LoginProtection, RecoveryCodesModal, Sessions, SSOProviders } from '../components/SecurityCards'
import { Icon } from '../components/Icons'
import { refreshBrand } from '../brand'
import { CopyBox, Field, QR, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { t } from '../i18n'
import { Access } from './settings/Access'
import { Appearance } from './settings/Appearance'
import { Backups } from './settings/Backups'
import { Cluster, OperationalHealth } from './settings/Cluster'
import { MailSettings } from './settings/MailSettings'
import { DefaultVersion, Updates } from './settings/Updates'

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
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? t('已保存（留空不修改）') : t('可选 · 用于 ACME 证书，也是 CDN 配置档的默认 Token。ACME 只需 Zone.DNS 编辑；CDN「同步 DNS」还需 Zone Settings 和 SSL and Certificates 编辑权限')}>
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
