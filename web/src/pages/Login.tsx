import { useState, type FormEvent } from 'react'
import { ApiError, post, setToken } from '../api'
import { Icon } from '../components/Icons'

export default function Login({ onLogin }: { onLogin: (user: string) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [needTotp, setNeedTotp] = useState(false)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

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
      </form>
    </div>
  )
}
