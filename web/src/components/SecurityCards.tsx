import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { ago } from '../format'
import { copyText, Field, Modal, useAction, useConfirm, useLoad } from './ui'

// RecoveryCodesModal shows freshly issued recovery codes once.
export function RecoveryCodesModal({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const text = codes.join('\n')
  const save = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([`Polaris 二次验证恢复码（每个只能用一次）\n${location.host}\n\n${text}\n`], { type: 'text/plain' }))
    a.download = 'polaris-recovery-codes.txt'
    a.click()
  }
  return (
    <Modal title="保存恢复码" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={() => copyText(text)}>复制</button>
      <button className="btn" type="button" onClick={save}>下载</button>
      <button className="btn pri" type="button" onClick={onClose}>我已保存</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>手机丢失或验证器被删除时，可以用恢复码代替验证码登录。每个只能用一次，<b>只显示这一次</b>，请保存在密码管理器或离线处。</div>
      <div className="mono" style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 8, marginTop: 12, fontSize: 15 }}>
        {codes.map((c) => <div key={c} style={{ background: 'var(--bg-2, var(--bg))', borderRadius: 8, padding: '8px 10px', textAlign: 'center' }}>{c}</div>)}
      </div>
    </Modal>
  )
}

type Session = { id: string; username: string; via: string; ip: string; device: string; lastSeen: string; createdAt: string; current: boolean }
const viaLabel: Record<string, string> = { password: '密码', github: 'GitHub', telegram: 'Telegram' }

// Sessions lists signed-in browsers; any can be ended.
export function Sessions() {
  const list = useLoad(() => get<Session[]>('/auth/sessions'))
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ss = list.data ?? []
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>登录中的设备</span>
        {ss.length > 1 && <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
          if (await confirm('结束除当前设备外的所有登录？', { danger: true, ok: '全部结束' })) run(async () => { await post('/auth/sessions/revoke-others'); list.reload() }, '已结束其他登录')
        }}>结束其他所有登录</button>}
      </div>
      <div className="sub" style={{ margin: 0 }}>每次登录都是一个会话；看到不认识的设备或地址，立即结束它并修改密码。</div>
      <div style={{ display: 'grid', gap: 8 }}>
        {ss.map((s) => (
          <div key={s.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ fontWeight: 500 }}>{s.device || '未知设备'}{s.current && <span className="tag ok" style={{ marginLeft: 6 }}>当前</span>}</div>
              <div className="hint mono">{s.username} · {viaLabel[s.via] ?? s.via} · {s.ip} · 活动于 {ago(s.lastSeen)}</div>
            </div>
            {!s.current && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/auth/sessions/' + s.id); list.reload() }, '已结束')}>结束</button>}
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
  const d = info.data
  useEffect(() => { if (d) setF({ allow: d.allowlist, fails: String(d.banFailures) }) }, [d])
  const banned = (d?.bans ?? []).filter((b) => b.until && new Date(b.until) > new Date())
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <h2>登录防护</h2>
      <div className="sub" style={{ margin: 0 }}>同一 IP 15 分钟内登录失败达到次数即封禁，首次 15 分钟，之后每次翻倍（最长 7 天），重启不会清除；账号本身失败 5 次锁定 15 分钟。白名单内的地址永不封禁。</div>
      <div className="form-grid">
        <Field label="封禁阈值（次）"><input className="input mono" inputMode="numeric" value={f.fails} onChange={(e) => setF({ ...f, fails: e.target.value.replace(/\D/g, '') })} /></Field>
        <Field label="登录白名单" hint={`IP 或网段，逗号 / 换行分隔。你现在的地址：${d?.yourIp ?? '…'}`}>
          <textarea className="input mono" style={{ minHeight: 60 }} value={f.allow} placeholder={d?.yourIp} onChange={(e) => setF({ ...f, allow: e.target.value })} />
        </Field>
      </div>
      <div><button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', { login_allowlist: f.allow.trim(), login_ban_failures: f.fails || '10' }); info.reload() }, '已保存')}>保存</button></div>
      <div className="hint">真实访客 IP：只信任来自 {d?.trustedProxies.length ? d.trustedProxies.join('、') : '本机与内网地址'} 的反向代理转发的 X-Forwarded-For；放在其他代理或 CDN 后面时，在 polaris.env 设置 POLARIS_TRUSTED_PROXIES。</div>
      {banned.length > 0 && <>
        <h2 style={{ fontSize: 14 }}>已封禁（{banned.length}）</h2>
        {banned.map((b) => (
          <div key={b.ip} className="row" style={{ flexWrap: 'nowrap', gap: 10 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="mono" style={{ fontWeight: 500 }}>{b.ip}</div>
              <div className="hint">{b.reason} · 第 {b.bans} 次 · 至 {new Date(b.until!).toLocaleString()}</div>
            </div>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/security/bans/' + encodeURIComponent(b.ip)); info.reload() }, '已解封')}>解封</button>
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
  { label: 'Microsoft', f: { name: 'Microsoft', kind: 'oidc', issuer: 'https://login.microsoftonline.com/<租户 ID>/v2.0' } },
  { label: 'GitLab', f: { name: 'GitLab', kind: 'oidc', issuer: 'https://gitlab.com', scopes: 'openid profile email', loginField: 'nickname' } },
  { label: 'Keycloak', f: { name: 'Keycloak', kind: 'oidc', issuer: 'https://<keycloak 主机>/realms/<realm>' } },
  { label: 'Authentik', f: { name: 'Authentik', kind: 'oidc', issuer: 'https://<authentik 主机>/application/o/<应用 slug>' } },
  { label: 'Okta', f: { name: 'Okta', kind: 'oidc', issuer: 'https://<你的域>.okta.com' } },
  { label: 'Auth0', f: { name: 'Auth0', kind: 'oidc', issuer: 'https://<你的租户>.auth0.com' } },
  { label: 'GitHub', f: { name: 'GitHub', kind: 'oauth2', authUrl: 'https://github.com/login/oauth/authorize', tokenUrl: 'https://github.com/login/oauth/access_token', userInfoUrl: 'https://api.github.com/user', scopes: 'read:user', subjectField: 'id', loginField: 'login' } },
  { label: '其他 OIDC', f: { name: '', kind: 'oidc' } },
  { label: '其他 OAuth 2.0', f: { name: '', kind: 'oauth2', subjectField: 'id', loginField: 'login' } },
]

// SSOProviders: single sign-on buttons for the login page, and this admin's
// bindings to them.
export function SSOProviders() {
  const list = useLoad(() => get<{ providers: Provider[]; callbackUrl: string }>('/sso/providers'))
  const mine = useLoad(() => get<{ providerId: string; login: string }[]>('/auth/sso/bindings'))
  const [edit, setEdit] = useState<ProviderForm | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ps = list.data?.providers ?? []
  const bound = (id: string) => (mine.data ?? []).find((b) => b.providerId === id)
  const set = (k: keyof ProviderForm, v: string | boolean) => setEdit((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    if (edit?.id) await put('/sso/providers/' + edit.id, edit); else await post('/sso/providers', edit)
    setEdit(null); list.reload()
  }, '已保存')
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>单点登录</span><button className="btn sm" type="button" onClick={() => setEdit({ kind: 'oidc', name: '' })}>+ 添加登录方式</button></div>
      <div className="sub" style={{ margin: 0 }}>为登录页添加「使用 … 登录」：任意 OpenID Connect 提供方（Google、Microsoft、GitLab、Keycloak、Authentik、Okta、Auth0…）或 OAuth 2.0 服务。添加后点「绑定」把你的外部账号关联到当前管理员；开启了二次验证时仍需输入验证码。</div>
      {ps.length === 0 && <div className="hint">还没有添加单点登录。</div>}
      {ps.map((p) => {
        const b = bound(p.id)
        return (
          <div key={p.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ fontWeight: 500 }}>{p.name} <span className="tag">{p.kind === 'oidc' ? 'OIDC' : 'OAuth 2.0'}</span>{!p.enabled && <span className="tag warn" style={{ marginLeft: 4 }}>已停用</span>}</div>
              <div className="hint">{b ? `已绑定：${b.login}` : '当前管理员未绑定'} · {p.issuer || p.authUrl}</div>
            </div>
            {b ? <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/auth/sso/bindings/' + p.id); mine.reload() }, '已解除绑定')}>解绑</button>
              : <button className="btn sm" type="button" disabled={busy || !p.enabled} onClick={() => run(async () => { location.href = (await post<{ url: string }>(`/auth/sso/${p.id}/bind-url`)).url })}>绑定</button>}
            <button className="btn sm" type="button" onClick={() => setEdit({ ...p })}>编辑</button>
            <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(`删除登录方式 ${p.name}？已绑定的账号一并解除。`, { danger: true })) run(async () => { await del('/sso/providers/' + p.id); list.reload(); mine.reload() }) }}>删除</button>
          </div>
        )
      })}
      {edit && (
        <Modal title={edit.id ? `编辑 ${edit.name}` : '添加登录方式'} wide onClose={() => setEdit(null)} footer={<>
          <button className="btn" type="button" onClick={() => setEdit(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !edit.name || !edit.clientId} onClick={save}>保存</button>
        </>}>
          <div style={{ display: 'grid', gap: 12 }}>
            {!edit.id && <div className="chips">{ssoPresets.map((pr) => <button key={pr.label} type="button" className="chip sm" onClick={() => setEdit({ ...pr.f })}>{pr.label}</button>)}</div>}
            <div className="hint">在提供方创建应用，回调地址（Redirect URI）填：</div>
            <CopyBoxInline text={list.data?.callbackUrl ?? ''} />
            <div className="form-grid">
              <Field label="按钮名称"><input className="input" value={edit.name ?? ''} onChange={(e) => set('name', e.target.value)} placeholder="公司 SSO" /></Field>
              <Field label="类型">
                <select className="input" value={edit.kind} onChange={(e) => set('kind', e.target.value)}><option value="oidc">OpenID Connect（自动发现）</option><option value="oauth2">OAuth 2.0（手动填写地址）</option></select>
              </Field>
              {edit.kind === 'oidc'
                ? <Field label="Issuer" hint="会自动读取 /.well-known/openid-configuration"><input className="input mono" value={edit.issuer ?? ''} onChange={(e) => set('issuer', e.target.value.trim())} /></Field>
                : <>
                  <Field label="授权地址"><input className="input mono" value={edit.authUrl ?? ''} onChange={(e) => set('authUrl', e.target.value.trim())} /></Field>
                  <Field label="令牌地址"><input className="input mono" value={edit.tokenUrl ?? ''} onChange={(e) => set('tokenUrl', e.target.value.trim())} /></Field>
                  <Field label="用户信息地址"><input className="input mono" value={edit.userInfoUrl ?? ''} onChange={(e) => set('userInfoUrl', e.target.value.trim())} /></Field>
                </>}
              <Field label="Client ID"><input className="input mono" value={edit.clientId ?? ''} onChange={(e) => set('clientId', e.target.value.trim())} /></Field>
              <Field label="Client Secret" hint={edit.id ? '留空不修改' : undefined}><input className="input mono" type="password" autoComplete="off" value={edit.clientSecret ?? ''} onChange={(e) => set('clientSecret', e.target.value.trim())} /></Field>
              <Field label="Scope" hint={edit.kind === 'oidc' ? '默认 openid profile email' : '按提供方要求填写'}><input className="input mono" value={edit.scopes ?? ''} onChange={(e) => set('scopes', e.target.value)} /></Field>
              <Field label="账号标识字段" hint={edit.kind === 'oidc' ? '默认 sub' : '用户信息里唯一不变的字段，如 id'}><input className="input mono" value={edit.subjectField ?? ''} onChange={(e) => set('subjectField', e.target.value.trim())} /></Field>
              <Field label="显示名字段" hint="如 preferred_username / email / login"><input className="input mono" value={edit.loginField ?? ''} onChange={(e) => set('loginField', e.target.value.trim())} /></Field>
            </div>
            {edit.id && <label className="chk"><input type="checkbox" checked={edit.enabled ?? true} onChange={(e) => set('enabled', e.target.checked)} />在登录页显示</label>}
          </div>
        </Modal>
      )}
    </section>
  )
}

function CopyBoxInline({ text }: { text: string }) {
  return <div className="row" style={{ flexWrap: 'nowrap' }}><code style={{ flex: 1, wordBreak: 'break-all' }}>{text}</code><button className="btn sm" type="button" onClick={() => copyText(text)}>复制</button></div>
}
