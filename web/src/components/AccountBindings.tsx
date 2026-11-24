import { Fragment, useEffect, useRef, useState, type FormEvent } from 'react'
import { get, post, setToken, takeOAuthResult } from '../api'
import { createPasskey, passkeysSupported } from '../passkeys'
import { Field, useAction, useLoad } from './ui'
import { t } from '../i18n'

type PasskeyItem = { id: string; name: string; rpId: string; createdAt: string; lastUsedAt?: string }

type SSOItem = { providerId: string; providerName: string; login: string }

type Methods = {
  email: boolean; emailAddress: string; password: boolean; telegram: boolean
  github: boolean; githubLogin: string; githubAvailable: boolean; count: number
  passkey: boolean; passkeyCount: number; passkeys: PasskeyItem[]
  sso: SSOItem[]; ssoAvailable: boolean; ssoProviders: { id: string; name: string }[]
}

// AccountBindings is the member panel's sign-in methods card (identity
// batch 2): the same four methods and the same rules as the user
// center's bindings card. Telegram and e-mail binding flows live in
// the user center (bot confirmation / mail code); here they are
// status-only. Removing a method ends every session on both faces,
// so a successful removal drops straight back to the login page.
export default function AccountBindings() {
  const data = useLoad((signal) => get<Methods>('/auth/bindings', signal))
  const { busy, run } = useAction()
  const [note, setNote] = useState('')
  const [err, setErr] = useState('')
  const [askPw, setAskPw] = useState<'github' | 'password' | 'sso' | null>(null)
  const [ssoTarget, setSsoTarget] = useState('')
  const [pw, setPw] = useState('')
  const [regOpen, setRegOpen] = useState(false)
  const [regName, setRegName] = useState('')
  const [regPw, setRegPw] = useState('')
  const [delKey, setDelKey] = useState<PasskeyItem | null>(null)
  const [delPw, setDelPw] = useState('')
  const [renameKey, setRenameKey] = useState<PasskeyItem | null>(null)
  const [renameName, setRenameName] = useState('')
  const redeemed = useRef(false)

  // The GitHub / SSO bind round trips return with a one-use ticket
  // (or the result) in the URL fragment; redeem it under this session.
  useEffect(() => {
    const r = takeOAuthResult()
    if (r.bindTicket && !redeemed.current) {
      redeemed.current = true
      post('/auth/github/user/bind-complete', { ticket: r.bindTicket })
        .then(() => { setNote(t('GitHub 已绑定')); data.reload() })
        .catch((e) => setErr(e instanceof Error ? e.message : String(e)))
    } else if (r.ssoBindTicket && !redeemed.current) {
      redeemed.current = true
      post('/auth/bindings/sso/complete', { ticket: r.ssoBindTicket })
        .then(() => { setNote(t('单点登录已绑定')); data.reload() })
        .catch((e) => setErr(e instanceof Error ? e.message : String(e)))
    } else if (r.bound) {
      setNote(t('GitHub 已绑定'))
    } else if (r.ssoBound) {
      setNote(t('单点登录已绑定'))
    } else if (r.error) {
      setErr(r.error)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const m = data.data
  const relogin = () => { setToken(null); location.reload() }
  const bindGithub = () => run(async () => { location.href = (await post<{ url: string }>('/auth/github/user/bind-url')).url })
  const unbindGithub = () => {
    if (m?.password) { setAskPw('github'); return }
    run(async () => { await post('/auth/bindings/github/unbind', {}); relogin() })
  }
  const bindSSO = (providerId: string) => run(async () => { location.href = (await post<{ url: string }>('/auth/sso/user/bind-url', { providerId })).url })
  const unbindSSO = (providerId: string) => {
    setSsoTarget(providerId)
    if (m?.password) { setAskPw('sso'); return }
    run(async () => { await post('/auth/bindings/sso/unbind', { providerId }); relogin() })
  }
  const submitRemoval = (e: FormEvent) => {
    e.preventDefault()
    if (!askPw) return
    if (askPw === 'sso') {
      run(async () => { await post('/auth/bindings/sso/unbind', { providerId: ssoTarget, currentPassword: pw }); relogin() })
      return
    }
    const path = askPw === 'github' ? '/auth/bindings/github/unbind' : '/auth/bindings/password/clear'
    run(async () => { await post(path, { currentPassword: pw }); relogin() })
  }
  const registerPasskey = (e: FormEvent) => {
    e.preventDefault()
    run(async () => {
      const begin = await post<{ challengeId: string; options: any }>('/auth/passkeys/user/register/begin', { name: regName, currentPassword: regPw || undefined })
      const credential = await createPasskey(begin.options)
      await post('/auth/passkeys/user/register/finish', { challengeId: begin.challengeId, credential })
      setRegOpen(false); setRegName(''); setRegPw(''); setNote(t('通行密钥已注册')); data.reload()
    })
  }
  const submitDeleteKey = (e: FormEvent) => {
    e.preventDefault()
    if (!delKey) return
    run(async () => { await post('/auth/passkeys/user/delete', { id: delKey.id, currentPassword: delPw || undefined }); relogin() })
  }
  const submitRenameKey = (e: FormEvent) => {
    e.preventDefault()
    if (!renameKey) return
    run(async () => { await post('/auth/passkeys/user/rename', { id: renameKey.id, name: renameName }); setRenameKey(null); setNote(t('通行密钥已改名')); data.reload() })
  }
  const row = (name: string, state: string, action?: React.ReactNode) => (
    <div className="row" style={{ justifyContent: 'space-between' }}>
      <span>{name}</span>
      <span className="row"><span className="sub" style={{ margin: 0 }}>{state}</span>{action}</span>
    </div>
  )
  return (
    <section className="card"><h2>{t('登录方式')}</h2>
      {data.error && <div className="note bad" role="alert">{data.error}</div>}
      {m && <div style={{ display: 'grid', gap: 10 }}>
        {row(t('邮箱登录'), m.email ? t('已绑定') + (m.emailAddress ? ` · ${m.emailAddress}` : '') : t('未绑定'))}
        {row(t('登录密码'), m.password ? t('已设置') : t('未设置'),
          m.password ? <button className="btn sm" type="button" disabled={busy} onClick={() => setAskPw('password')}>{t('清除')}</button> : undefined)}
        {row('Telegram', m.telegram ? t('已绑定') : t('未绑定'))}
        {row('GitHub', m.github ? t('已绑定') + (m.githubLogin ? ` · ${m.githubLogin}` : '') : t('未绑定'),
          m.github
            ? <button className="btn sm" type="button" disabled={busy} onClick={unbindGithub}>{t('解绑')}</button>
            : m.githubAvailable ? <button className="btn sm" type="button" disabled={busy} onClick={bindGithub}>{t('绑定')}</button> : undefined)}
        {row(t('通行密钥'), m.passkey ? t('已注册 {n} 枚', { n: m.passkeyCount }) : t('未注册'),
          passkeysSupported() ? <button className="btn sm" type="button" disabled={busy} onClick={() => { setRegOpen(!regOpen); setErr('') }}>{t('注册新通行密钥')}</button> : undefined)}
        {m.passkeys.map((k) => (
          <div key={k.id} className="row" style={{ justifyContent: 'space-between' }}>
            <span style={{ paddingLeft: 14 }}>· {k.name}</span>
            <span className="row"><span className="sub" style={{ margin: 0 }}>{k.rpId}</span>
              <button className="btn sm" type="button" disabled={busy} onClick={() => { setRenameKey(k); setRenameName(k.name) }}>{t('改名')}</button>
              <button className="btn sm" type="button" disabled={busy} onClick={() => { setDelKey(k); setDelPw('') }}>{t('删除')}</button>
            </span>
          </div>
        ))}
        {m.passkey && <div className="hint">{t('通行密钥按注册时的访问域名区分，在其他域名登录时需另注册一枚。')}</div>}
        {(m.ssoProviders ?? []).map((p) => {
          const b = (m.sso ?? []).find((x) => x.providerId === p.id)
          return <Fragment key={p.id}>{row(t('单点登录 · {name}', { name: p.name }), b ? t('已绑定') + (b.login ? ` · ${b.login}` : '') : t('未绑定'),
            b
              ? <button className="btn sm" type="button" disabled={busy} onClick={() => unbindSSO(p.id)}>{t('解绑')}</button>
              : m.ssoAvailable ? <button className="btn sm" type="button" disabled={busy} onClick={() => bindSSO(p.id)}>{t('绑定')}</button> : undefined)}</Fragment>
        })}
        <div className="hint">{t('绑定或解绑 Telegram、更换邮箱请在用户中心完成；至少需要保留一种登录方式。')}</div>
      </div>}
      {regOpen && (
        <form style={{ display: 'grid', gap: 12, marginTop: 12 }} onSubmit={registerPasskey}>
          <Field label={t('通行密钥名称')}><input className="input" value={regName} onChange={(e) => setRegName(e.target.value)} placeholder={t('如：我的手机')} maxLength={64} required /></Field>
          {m?.password && <Field label={t('当前密码')}><input className="input" type="password" autoComplete="current-password" required value={regPw} onChange={(e) => setRegPw(e.target.value)} /></Field>}
          <div className="hint">{t('注册后可用这枚通行密钥直接登录面板，无需密码或验证码。')}</div>
          <div className="row">
            <button className="btn pri" type="submit" disabled={busy}>{t('开始注册')}</button>
            <button className="btn" type="button" disabled={busy} onClick={() => setRegOpen(false)}>{t('取消')}</button>
          </div>
        </form>
      )}
      {delKey && (
        <form style={{ display: 'grid', gap: 12, marginTop: 12 }} onSubmit={submitDeleteKey}>
          <div className="hint">{t('删除通行密钥「{name}」？删除后不能再用它登录，且所有设备的登录都会失效。', { name: delKey.name })}</div>
          {m?.password && <Field label={t('当前密码')}><input className="input" type="password" autoComplete="current-password" required value={delPw} onChange={(e) => setDelPw(e.target.value)} /></Field>}
          <div className="row">
            <button className="btn pri" type="submit" disabled={busy}>{t('确认删除')}</button>
            <button className="btn" type="button" disabled={busy} onClick={() => setDelKey(null)}>{t('取消')}</button>
          </div>
        </form>
      )}
      {renameKey && (
        <form style={{ display: 'grid', gap: 12, marginTop: 12 }} onSubmit={submitRenameKey}>
          <Field label={t('通行密钥名称')}><input className="input" value={renameName} onChange={(e) => setRenameName(e.target.value)} maxLength={64} required /></Field>
          <div className="row">
            <button className="btn pri" type="submit" disabled={busy}>{t('保存')}</button>
            <button className="btn" type="button" disabled={busy} onClick={() => setRenameKey(null)}>{t('取消')}</button>
          </div>
        </form>
      )}
      {askPw && (
        <form style={{ display: 'grid', gap: 12, marginTop: 12 }} onSubmit={submitRemoval}>
          <Field label={t('当前密码')}><input className="input" type="password" autoComplete="current-password" required value={pw} onChange={(e) => setPw(e.target.value)} /></Field>
          <div className="hint">{askPw === 'github' ? t('解绑后将不能再用 GitHub 登录本账号，且所有设备的登录都会失效。') : askPw === 'sso' ? t('解绑后将不能再用该单点登录方式登录本账号，且所有设备的登录都会失效。') : t('清除后将不能再用用户名和密码登录本账号，且所有设备的登录都会失效。')}</div>
          <div className="row">
            <button className="btn pri" type="submit" disabled={busy}>{t('确认')}</button>
            <button className="btn" type="button" disabled={busy} onClick={() => { setAskPw(null); setPw('') }}>{t('取消')}</button>
          </div>
        </form>
      )}
      {note && <div className="note" style={{ marginTop: 12 }}>{note}</div>}
      {err && <div className="note bad" role="alert" style={{ marginTop: 12 }}>{err}</div>}
    </section>
  )
}
