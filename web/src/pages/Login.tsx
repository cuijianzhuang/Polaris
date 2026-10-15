import { useEffect, useState, type FormEvent } from 'react'
import { ApiError, get, post, setToken, takeOAuthResult } from '../api'
import { Icon } from '../components/Icons'

export default function Login({ onLogin }: { onLogin: (user: string) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [needTotp, setNeedTotp] = useState(false)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [github, setGithub] = useState(false)
  const [ticket, setTicket] = useState('') // GitHub verified, 2FA code still needed
  useEffect(() => {
    get<{ githubLogin?: boolean }>('/info').then((i) => setGithub(!!i.githubLogin)).catch(() => {})
    const r = takeOAuthResult()
    if (r.error) setErr(r.error)
    if (r.ticket) { setTicket(r.ticket); setErr('') }
  }, [])
  const githubLogin = async () => {
    setBusy(true); setErr('')
    try { location.href = (await post<{ url: string }>('/auth/github/login-url')).url } catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false) }
  }
  const submitTicket = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true); setErr('')
    try {
      const r = await post<{ token: string; username: string }>('/auth/github/totp', { ticket, totp })
      setToken(r.token)
      onLogin(r.username)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e)); setTicket('')
    } finally { setBusy(false) }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    try {
      const r = await post<{ token: string; username: string }>('/auth/login', { username, password, totp })
      setToken(r.token)
      onLogin(r.username)
    } catch (e) {
      if (e instanceof ApiError && e.data?.totpRequired) {
        setNeedTotp(true)
        setErr('请输入二次验证码')
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setBusy(false)
    }
  }

  if (ticket) {
    return (
      <div className="login">
        <form className="card" onSubmit={submitTicket}>
          <div className="row" style={{ gap: 12, fontWeight: 600, fontSize: 20 }}><Icon.LogoTile size={40} />Polaris</div>
          <div className="note">已通过 GitHub 验证，请输入二次验证码完成登录。</div>
          <div className="field">
            <label className="lbl" htmlFor="gt">二次验证码</label>
            <input className="input mono" id="gt" inputMode="numeric" autoComplete="one-time-code" value={totp} onChange={(e) => setTotp(e.target.value)} autoFocus />
          </div>
          {err && <div className="note bad">{err}</div>}
          <button className="btn pri" type="submit" disabled={busy || totp.length < 6}>{busy ? '登录中…' : '登录'}</button>
        </form>
      </div>
    )
  }

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <div className="row" style={{ gap: 12, fontWeight: 600, fontSize: 20 }}><Icon.LogoTile size={40} />Polaris</div>
        <div className="field">
          <label className="lbl" htmlFor="u">用户名</label>
          <input className="input" id="u" autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required />
        </div>
        <div className="field">
          <label className="lbl" htmlFor="p">密码</label>
          <input className="input" id="p" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
        </div>
        {needTotp && (
          <div className="field">
            <label className="lbl" htmlFor="t">二次验证码</label>
            <input className="input mono" id="t" inputMode="numeric" autoComplete="one-time-code" value={totp} onChange={(e) => setTotp(e.target.value)} autoFocus />
          </div>
        )}
        {err && <div className="note bad">{err}</div>}
        <button className="btn pri" type="submit" disabled={busy}>{busy ? '登录中…' : '登录'}</button>
        {github && <>
          <div className="or"><span>或</span></div>
          <button className="btn github" type="button" disabled={busy} onClick={githubLogin}><Icon.GitHub />使用 GitHub 登录</button>
        </>}
      </form>
    </div>
  )
}
