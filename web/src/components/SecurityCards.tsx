import { useEffect, useState } from 'react'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from './SettingsDrafts'
import { del, get, post, put } from '../api'
import { ago } from '../format'
import { copyText, Field, Modal, useAction, useConfirm, useLoad } from './ui'
import { t } from '../i18n'

// RecoveryCodesModal shows freshly issued recovery codes once.
export function RecoveryCodesModal({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const text = codes.join('\n')
  const save = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([`${t('Polaris 二次验证恢复码（每个只能用一次）')}\n${location.host}\n\n${text}\n`], { type: 'text/plain' }))
    a.download = 'polaris-recovery-codes.txt'
    a.click()
  }
  return (
    <Modal title={t('保存恢复码')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={() => copyText(text)}>{t('复制')}</button>
      <button className="btn" type="button" onClick={save}>{t('下载')}</button>
      <button className="btn pri" type="button" onClick={onClose}>{t('我已保存')}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{t('手机丢失或验证器被删除时，可以用恢复码代替验证码登录。每个只能用一次，')}<b>{t('只显示这一次')}</b>{t('，请保存在密码管理器或离线处。')}</div>
      <div className="mono" style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 8, marginTop: 12, fontSize: 15 }}>
        {codes.map((c) => <div key={c} style={{ background: 'var(--bg-2, var(--bg))', borderRadius: 8, padding: '8px 10px', textAlign: 'center' }}>{c}</div>)}
      </div>
    </Modal>
  )
}

type Session = { id: string; username: string; via: string; ip: string; device: string; lastSeen: string; createdAt: string; current: boolean }
const viaLabel: Record<string, string> = { password: t('密码'), github: 'GitHub', telegram: 'Telegram' }

// Sessions lists signed-in browsers; any can be ended.
export function Sessions() {
  const list = useLoad(() => get<Session[]>('/auth/sessions'))
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ss = list.data ?? []
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>{t('登录中的设备')}</span>
        {ss.length > 1 && <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
          if (await confirm(t('结束除当前设备外的所有登录？'), { danger: true, ok: t('全部结束') })) run(async () => { await post('/auth/sessions/revoke-others'); list.reload() }, t('已结束其他登录'))
        }}>{t('结束其他所有登录')}</button>}
      </div>
      <div className="sub" style={{ margin: 0 }}>{t('每次登录都是一个会话；看到不认识的设备或地址，立即结束它并修改密码。')}</div>
      <div style={{ display: 'grid', gap: 8 }}>
        {ss.map((s) => (
          <div key={s.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ fontWeight: 500 }}>{s.device || t('未知设备')}{s.current && <span className="tag ok" style={{ marginLeft: 6 }}>{t('当前')}</span>}</div>
              <div className="hint mono">{s.username} · {viaLabel[s.via] ?? s.via} · {s.ip} · {t('活动于 {ago}', { ago: ago(s.lastSeen) })}</div>
            </div>
            {!s.current && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/auth/sessions/' + s.id); list.reload() }, t('已结束'))}>{t('结束')}</button>}
          </div>
        ))}
      </div>
    </section>
  )
}

type Ban = { ip: string; failures: number; bans: number; until?: string; reason: string; updatedAt: string }
type Info = { bans: Ban[]; allowlist: string; banFailures: number; trustedProxies: string[]; yourIp: string }

// LoginProtection: per-IP bans, the allowlist and proxy trust.
export function LoginProtection() {
  const info = useLoad(() => get<Info>('/security'))
  const [f, setF] = useState({ allow: '', fails: '' })
  const { busy, run } = useAction()
  const saver = useSettingsSave()
  const [baseline, setBaseline] = useState('')
  const d = info.data
  useEffect(() => { if (d) { const next = { allow: d.allowlist, fails: String(d.banFailures) }; setF(next); setBaseline(JSON.stringify(next)) } }, [d])
  const dirty = !!baseline && JSON.stringify(f) !== baseline
  const save = async () => {
    if (!dirty) return true
    if (busy || saver.busy || info.loading) return false
    return saver.run(async () => { await put('/settings', { login_allowlist: f.allow.trim(), login_ban_failures: f.fails || '10' }); setBaseline(JSON.stringify(f)); info.reload() })
  }
  useSettingsDraft({ label: t('登录防护'), dirty, busy: busy || saver.busy, save, discard: () => setF(JSON.parse(baseline || '{"allow":"","fails":""}')) })
  const banned = (d?.bans ?? []).filter((b) => b.until && new Date(b.until) > new Date())
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <h2>{t('登录防护')}</h2>
      <div className="sub" style={{ margin: 0 }}>{t('同一 IP 15 分钟内登录失败达到次数即封禁，首次 15 分钟，之后每次翻倍（最长 7 天），重启不会清除；账号本身失败 5 次锁定 15 分钟。白名单内的地址永不封禁。')}</div>
      <div className="form-grid">
        <Field label={t('封禁阈值（次）')}><input disabled={saver.busy || info.loading || !baseline} className="input mono" inputMode="numeric" value={f.fails} onChange={(e) => setF({ ...f, fails: e.target.value.replace(/\D/g, '') })} /></Field>
        <Field label={t('登录白名单')} hint={t('IP 或网段，逗号 / 换行分隔。你现在的地址：{ip}', { ip: d?.yourIp ?? '…' })}>
          <textarea disabled={saver.busy || info.loading || !baseline} className="input mono" style={{ minHeight: 60 }} value={f.allow} placeholder={d?.yourIp} onChange={(e) => setF({ ...f, allow: e.target.value })} />
        </Field>
      </div>
      <SettingsSaveBar loading={info.loading} dirty={dirty} busy={saver.busy} error={saver.error || info.error} disabled={busy || info.loading || !d} onSave={save} />
      <div className="hint">{t('真实访客 IP：只信任来自 {proxies} 的反向代理转发的 X-Forwarded-For；放在其他代理或 CDN 后面时，在 polaris.env 设置 POLARIS_TRUSTED_PROXIES。', { proxies: d?.trustedProxies.length ? d.trustedProxies.join(t('、')) : t('本机与内网地址') })}</div>
      {banned.length > 0 && <>
        <h2 style={{ fontSize: 14 }}>{t('已封禁（{n}）', { n: banned.length })}</h2>
        {banned.map((b) => (
          <div key={b.ip} className="row" style={{ flexWrap: 'nowrap', gap: 10 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="mono" style={{ fontWeight: 500 }}>{b.ip}</div>
              <div className="hint">{b.reason} · {t('第 {n} 次', { n: b.bans })} · {t('至 {time}', { time: new Date(b.until!).toLocaleString() })}</div>
            </div>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/security/bans/' + encodeURIComponent(b.ip)); info.reload() }, t('已解封'))}>{t('解封')}</button>
          </div>
        ))}
      </>}
    </section>
  )
}

type Provider = { id: string; name: string; kind: 'oidc' | 'oauth2'; issuer: string; authUrl: string; tokenUrl: string; userInfoUrl: string
  clientId: string; scopes: string; subjectField: string; loginField: string; enabled: boolean }
type ProviderForm = Partial<Provider> & { clientSecret?: string }

// Presets fill the usual endpoints; the tenant / realm parts are placeholders to edit.
const ssoPresets: { label: string; f: ProviderForm }[] = [
  { label: 'Google', f: { name: 'Google', kind: 'oidc', issuer: 'https://accounts.google.com' } },
  { label: 'Microsoft', f: { name: 'Microsoft', kind: 'oidc', issuer: `https://login.microsoftonline.com/${t('<租户 ID>')}/v2.0` } },
  { label: 'GitLab', f: { name: 'GitLab', kind: 'oidc', issuer: 'https://gitlab.com', scopes: 'openid profile email', loginField: 'nickname' } },
  { label: 'Keycloak', f: { name: 'Keycloak', kind: 'oidc', issuer: `https://${t('<keycloak 主机>')}/realms/<realm>` } },
  { label: 'Authentik', f: { name: 'Authentik', kind: 'oidc', issuer: `https://${t('<authentik 主机>')}/application/o/${t('<应用 slug>')}` } },
  { label: 'Okta', f: { name: 'Okta', kind: 'oidc', issuer: `https://${t('<你的域>')}.okta.com` } },
  { label: 'Auth0', f: { name: 'Auth0', kind: 'oidc', issuer: `https://${t('<你的租户>')}.auth0.com` } },
  { label: 'GitHub', f: { name: 'GitHub', kind: 'oauth2', authUrl: 'https://github.com/login/oauth/authorize', tokenUrl: 'https://github.com/login/oauth/access_token', userInfoUrl: 'https://api.github.com/user', scopes: 'read:user', subjectField: 'id', loginField: 'login' } },
  { label: t('其他 OIDC'), f: { name: '', kind: 'oidc' } },
  { label: t('其他 OAuth 2.0'), f: { name: '', kind: 'oauth2', subjectField: 'id', loginField: 'login' } },
]

// SSOProviders: single sign-on buttons for the login page, and this admin's
// bindings to them.
export function SSOProviders() {
  const list = useLoad(() => get<{ providers: Provider[]; callbackUrl: string }>('/sso/providers'))
  const mine = useLoad(() => get<{ providerId: string; login: string }[]>('/auth/sso/bindings'))
  const [edit, setEdit] = useState<ProviderForm | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  useSettingsDraft({ label: t('单点登录'), dirty: !!edit && (!!edit.clientSecret || JSON.stringify(edit) !== JSON.stringify(list.data?.providers?.find((p) => p.id === edit.id) ?? { kind: 'oidc', name: '' })), busy, discard: () => setEdit(null) })
  const ps = list.data?.providers ?? []
  const bound = (id: string) => (mine.data ?? []).find((b) => b.providerId === id)
  const set = (k: keyof ProviderForm, v: string | boolean) => setEdit((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    if (edit?.id) await put('/sso/providers/' + edit.id, edit); else await post('/sso/providers', edit)
    setEdit(null); list.reload()
  }, t('已保存'))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>{t('单点登录')}</span><button className="btn sm" type="button" onClick={() => setEdit({ kind: 'oidc', name: '' })}>{t('+ 添加登录方式')}</button></div>
      <div className="sub" style={{ margin: 0 }}>{t('为登录页添加「使用 … 登录」：任意 OpenID Connect 提供方（Google、Microsoft、GitLab、Keycloak、Authentik、Okta、Auth0…）或 OAuth 2.0 服务。添加后点「绑定」把你的外部账号关联到当前管理员；开启了二次验证时仍需输入验证码。')}</div>
      {ps.length === 0 && <div className="hint">{t('还没有添加单点登录。')}</div>}
      {ps.map((p) => {
        const b = bound(p.id)
        return (
          <div key={p.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ fontWeight: 500 }}>{p.name} <span className="tag">{p.kind === 'oidc' ? 'OIDC' : 'OAuth 2.0'}</span>{!p.enabled && <span className="tag warn" style={{ marginLeft: 4 }}>{t('已停用')}</span>}</div>
              <div className="hint">{b ? t('已绑定：{login}', { login: b.login }) : t('当前管理员未绑定')} · {p.issuer || p.authUrl}</div>
            </div>
            {b ? <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/auth/sso/bindings/' + p.id); mine.reload() }, t('已解除绑定'))}>{t('解绑')}</button>
              : <button className="btn sm" type="button" disabled={busy || !p.enabled} onClick={() => run(async () => { location.href = (await post<{ url: string }>(`/auth/sso/${p.id}/bind-url`)).url })}>{t('绑定')}</button>}
            <button className="btn sm" type="button" onClick={() => setEdit({ ...p })}>{t('编辑')}</button>
            <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除登录方式 {name}？已绑定的账号一并解除。', { name: p.name }), { danger: true })) run(async () => { await del('/sso/providers/' + p.id); list.reload(); mine.reload() }) }}>{t('删除')}</button>
          </div>
        )
      })}
      {edit && (
        <Modal title={edit.id ? t('编辑 {name}', { name: edit.name }) : t('添加登录方式')} wide onClose={() => setEdit(null)} footer={<>
          <button className="btn" type="button" onClick={() => setEdit(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !edit.name || !edit.clientId} onClick={save}>{t('保存')}</button>
        </>}>
          <div style={{ display: 'grid', gap: 12 }}>
            {!edit.id && <div className="chips">{ssoPresets.map((pr) => <button key={pr.label} type="button" className="chip sm" onClick={() => setEdit({ ...pr.f })}>{pr.label}</button>)}</div>}
            <div className="hint">{t('在提供方创建应用，回调地址（Redirect URI）填：')}</div>
            <CopyBoxInline text={list.data?.callbackUrl ?? ''} />
            <div className="form-grid">
              <Field label={t('按钮名称')}><input className="input" value={edit.name ?? ''} onChange={(e) => set('name', e.target.value)} placeholder={t('公司 SSO')} /></Field>
              <Field label={t('类型')}>
                <select className="input" value={edit.kind} onChange={(e) => set('kind', e.target.value)}><option value="oidc">{t('OpenID Connect（自动发现）')}</option><option value="oauth2">{t('OAuth 2.0（手动填写地址）')}</option></select>
              </Field>
              {edit.kind === 'oidc'
                ? <Field label="Issuer" hint={t('会自动读取 /.well-known/openid-configuration')}><input className="input mono" value={edit.issuer ?? ''} onChange={(e) => set('issuer', e.target.value.trim())} /></Field>
                : <>
                  <Field label={t('授权地址')}><input className="input mono" value={edit.authUrl ?? ''} onChange={(e) => set('authUrl', e.target.value.trim())} /></Field>
                  <Field label={t('令牌地址')}><input className="input mono" value={edit.tokenUrl ?? ''} onChange={(e) => set('tokenUrl', e.target.value.trim())} /></Field>
                  <Field label={t('用户信息地址')}><input className="input mono" value={edit.userInfoUrl ?? ''} onChange={(e) => set('userInfoUrl', e.target.value.trim())} /></Field>
                </>}
              <Field label="Client ID"><input className="input mono" value={edit.clientId ?? ''} onChange={(e) => set('clientId', e.target.value.trim())} /></Field>
              <Field label="Client Secret" hint={edit.id ? t('留空不修改') : undefined}><input className="input mono" type="password" autoComplete="off" value={edit.clientSecret ?? ''} onChange={(e) => set('clientSecret', e.target.value.trim())} /></Field>
              <Field label="Scope" hint={edit.kind === 'oidc' ? t('默认 openid profile email') : t('按提供方要求填写')}><input className="input mono" value={edit.scopes ?? ''} onChange={(e) => set('scopes', e.target.value)} /></Field>
              <Field label={t('账号标识字段')} hint={edit.kind === 'oidc' ? t('默认 sub') : t('用户信息里唯一不变的字段，如 id')}><input className="input mono" value={edit.subjectField ?? ''} onChange={(e) => set('subjectField', e.target.value.trim())} /></Field>
              <Field label={t('显示名字段')} hint={t('如 preferred_username / email / login')}><input className="input mono" value={edit.loginField ?? ''} onChange={(e) => set('loginField', e.target.value.trim())} /></Field>
            </div>
            {edit.id && <label className="chk"><input type="checkbox" checked={edit.enabled ?? true} onChange={(e) => set('enabled', e.target.checked)} />{t('在登录页显示')}</label>}
          </div>
        </Modal>
      )}
    </section>
  )
}

function CopyBoxInline({ text }: { text: string }) {
  return <div className="row" style={{ flexWrap: 'nowrap' }}><code style={{ flex: 1, wordBreak: 'break-all' }}>{text}</code><button className="btn sm" type="button" onClick={() => copyText(text)}>{t('复制')}</button></div>
}

type APIToken = { id: string; name: string; pluginId?: string; prefix: string; scopes: string[]; createdBy: string; createdAt: string; lastUsedAt?: string; lastIp?: string; expiresAt?: string }
export const scopeLabel = (): Record<string, string> => ({
  'users:read': t('查看用户（含代理凭据）'), 'users:write': t('管理用户（创建、修改、续费、加流量）'), 'nodes:read': t('查看节点和入站'),
  'users:renew': t('仅续期用户'), 'users:traffic:add': t('仅增加用户流量'), 'subs:read': t('读取订阅链接与诊断'),
  'traffic:read': t('查看流量统计'), 'settings:read': t('查看设置'), 'nodes:metrics:write': t('上报节点外部指标（不能修改节点）'), 'cdn:ips:write': t('上报 CDN 优选 IP（测速点使用，不能修改配置档）'), 'metrics:read': t('读取监控指标（Prometheus 抓取 /metrics）'),
})

// APITokens: tokens for scripts and plugins to call the API without logging in.
export function APITokens() {
  const list = useLoad(() => get<{ tokens: APIToken[]; scopes: string[] }>('/tokens'))
  const [form, setForm] = useState<{ name: string; scopes: string[]; days: number } | null>(null)
  const [secret, setSecret] = useState('')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ts = list.data?.tokens ?? []
  const labels = scopeLabel()
  const toggle = (sc: string) => setForm((f) => f && ({ ...f, scopes: f.scopes.includes(sc) ? f.scopes.filter((x) => x !== sc) : [...f.scopes, sc] }))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>{t('API 令牌')}</span><button className="btn sm" type="button" onClick={() => setForm({ name: '', scopes: ['users:read'], days: 0 })}>{t('+ 新建令牌')}</button></div>
      <div className="sub" style={{ margin: 0 }}>{t('给脚本和插件调用接口用，请求头带 Authorization: Bearer plt_…。令牌只能访问勾选的权限范围，不能修改设置或管理员。')}</div>
      {ts.length === 0 && <div className="hint">{t('还没有 API 令牌。')}</div>}
      {ts.map((tk) => (
        <div key={tk.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ fontWeight: 500 }}>{tk.name} <span className="mono small mute">{tk.prefix}…</span>{tk.pluginId && <span className="tag" style={{ marginLeft: 4 }}>{t('插件')}</span>}
              {tk.expiresAt && new Date(tk.expiresAt) < new Date() && <span className="tag bad" style={{ marginLeft: 4 }}>{t('已过期')}</span>}</div>
            <div className="hint">{tk.scopes.length ? tk.scopes.map((s) => labels[s] ?? s).join('、') : t('仅能读取令牌自身信息')}</div>
            <div className="hint">{tk.lastUsedAt ? t('最近使用 {when}', { when: ago(tk.lastUsedAt) }) + (tk.lastIp ? ' · ' + tk.lastIp : '') : t('从未使用')}{' · '}{tk.expiresAt ? t('{date} 到期', { date: tk.expiresAt.slice(0, 10) }) : t('永久有效')}</div>
          </div>
          <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
            if (await confirm(t('吊销令牌 {name}？使用它的脚本或插件会立即失去访问权限。', { name: tk.name }), { danger: true, ok: t('吊销') })) run(async () => { await del('/tokens/' + tk.id); list.reload() }, t('已吊销'))
          }}>{t('吊销')}</button>
        </div>
      ))}
      {form && (
        <Modal title={t('新建 API 令牌')} onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" onClick={() => setForm(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !form.name.trim()} onClick={() => run(async () => {
            const r = await post<{ secret: string }>('/tokens', { ...form, name: form.name.trim() })
            setForm(null); setSecret(r.secret); list.reload()
          })}>{t('创建')}</button>
        </>}>
          <div style={{ display: 'grid', gap: 12 }}>
            <Field label={t('名称')}><input className="input" value={form.name} maxLength={64} placeholder={t('例如：发卡网对接')} onChange={(e) => setForm({ ...form, name: e.target.value })} /></Field>
            <Field label={t('权限范围')}>
              <div style={{ display: 'grid', gap: 6 }}>
                {(list.data?.scopes ?? Object.keys(labels)).map((sc) => <label key={sc} className="chk"><input type="checkbox" checked={form.scopes.includes(sc)} onChange={() => toggle(sc)} />{labels[sc] ?? sc} <span className="mono small mute">{sc}</span></label>)}
              </div>
            </Field>
            <Field label={t('有效期')}>
              <select className="input" value={form.days} onChange={(e) => setForm({ ...form, days: Number(e.target.value) })}>
                <option value={0}>{t('永久')}</option><option value={30}>{t('30 天')}</option><option value={90}>{t('90 天')}</option><option value={365}>{t('1 年')}</option>
              </select>
            </Field>
          </div>
        </Modal>
      )}
      {secret && (
        <Modal title={t('令牌已创建')} onClose={() => setSecret('')} footer={<button className="btn pri" type="button" onClick={() => setSecret('')}>{t('我已保存')}</button>}>
          <div className="sub" style={{ margin: '0 0 10px' }}>{t('令牌')}<b>{t('只显示这一次')}</b>{t('，请立即复制保存。丢失后只能吊销重建。')}</div>
          <CopyBoxInline text={secret} />
        </Modal>
      )}
    </section>
  )
}
