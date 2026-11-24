import { useEffect, useRef, useState, type FormEvent } from 'react'
import { get, post, setToken, takeOAuthResult } from '../api'
import { Field, useAction, useLoad } from './ui'
import { t } from '../i18n'

type Methods = {
  email: boolean; emailAddress: string; password: boolean; telegram: boolean
  github: boolean; githubLogin: string; githubAvailable: boolean; count: number
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
  const [askPw, setAskPw] = useState<'github' | 'password' | null>(null)
  const [pw, setPw] = useState('')
  const redeemed = useRef(false)

  // The GitHub bind round trip returns with a one-use ticket (or the
  // result) in the URL fragment; redeem it under this session.
  useEffect(() => {
    const r = takeOAuthResult()
    if (r.bindTicket && !redeemed.current) {
      redeemed.current = true
      post('/auth/github/user/bind-complete', { ticket: r.bindTicket })
        .then(() => { setNote(t('GitHub 已绑定')); data.reload() })
        .catch((e) => setErr(e instanceof Error ? e.message : String(e)))
    } else if (r.bound) {
      setNote(t('GitHub 已绑定'))
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
  const submitRemoval = (e: FormEvent) => {
    e.preventDefault()
    if (!askPw) return
    const path = askPw === 'github' ? '/auth/bindings/github/unbind' : '/auth/bindings/password/clear'
    run(async () => { await post(path, { currentPassword: pw }); relogin() })
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
        <div className="hint">{t('绑定或解绑 Telegram、更换邮箱请在用户中心完成；至少需要保留一种登录方式。')}</div>
      </div>}
      {askPw && (
        <form style={{ display: 'grid', gap: 12, marginTop: 12 }} onSubmit={submitRemoval}>
          <Field label={t('当前密码')}><input className="input" type="password" autoComplete="current-password" required value={pw} onChange={(e) => setPw(e.target.value)} /></Field>
          <div className="hint">{askPw === 'github' ? t('解绑后将不能再用 GitHub 登录本账号，且所有设备的登录都会失效。') : t('清除后将不能再用用户名和密码登录本账号，且所有设备的登录都会失效。')}</div>
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
