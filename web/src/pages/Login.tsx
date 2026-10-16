import { useEffect, useRef, useState, type FormEvent } from 'react'
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
  const [telegram, setTelegram] = useState(false)
  const [tg, setTg] = useState<TGRequest | null>(null)
  useEffect(() => {
    get<{ githubLogin?: boolean; telegramLogin?: boolean }>('/info').then((i) => { setGithub(!!i.githubLogin); setTelegram(!!i.telegramLogin) }).catch(() => {})
    const r = takeOAuthResult()
    if (r.error) setErr(r.error)
    if (r.ticket) { setTicket(r.ticket); setErr('') }
  }, [])
  const githubLogin = async () => {
    setBusy(true); setErr('')
    try { location.href = (await post<{ url: string }>('/auth/github/login-url')).url } catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false) }
  }
  const telegramLogin = async () => {
    setBusy(true); setErr('')
    try {
      const r = await post<TGRequest>('/auth/tg/start')
      setTg(r)
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
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

  if (tg) {
    return <TelegramWait req={tg} onDone={(user, error) => { setTg(null); if (user) onLogin(user); else if (error) setErr(error) }} />
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
        {(github || telegram) && <div className="or"><span>或</span></div>}
        {github && <button className="btn github" type="button" disabled={busy} onClick={githubLogin}><Icon.GitHub />使用 GitHub 登录</button>}
        {telegram && <button className="btn telegram" type="button" disabled={busy} onClick={telegramLogin}><Icon.Telegram />使用 Telegram 登录</button>}
      </form>
    </div>
  )
}

interface TGRequest { id: string; secret: string; code: number; url: string; bot: string; expiresIn: number }

// Waits for the admin to confirm in Telegram by tapping the number shown here.
function TelegramWait({ req, onDone }: { req: TGRequest; onDone: (user?: string, error?: string) => void }) {
  const [left, setLeft] = useState(req.expiresIn)
  const [opened, setOpened] = useState(false)
  const done = useRef(onDone)
  done.current = onDone
  useEffect(() => {
    let stop = false
    const end = Date.now() + req.expiresIn * 1000
    const tick = setInterval(() => setLeft(Math.max(0, Math.round((end - Date.now()) / 1000))), 1000)
    const poll = async () => {
      while (!stop) {
        await new Promise((r) => setTimeout(r, 2000))
        if (stop) return
        try {
          const r = await post<{ status: string; opened?: boolean; token?: string; username?: string }>('/auth/tg/poll', { id: req.id, secret: req.secret })
          if (r.status === 'pending') { setOpened(!!r.opened); continue }
          if (r.status === 'ok' && r.token) { setToken(r.token); done.current(r.username) }
          else done.current(undefined, r.status === 'denied' ? '已在 Telegram 中拒绝本次登录' : '登录请求已过期，请重试')
          return
        } catch { /* transient: keep polling until expiry */ }
        if (Date.now() > end + 5000) { done.current(undefined, '登录请求已过期，请重试'); return }
      }
    }
    poll()
    return () => { stop = true; clearInterval(tick) }
  }, [req])
  return (
    <div className="login">
      <div className="card tgwait">
        <div className="row" style={{ gap: 12, fontWeight: 600, fontSize: 20 }}><Icon.LogoTile size={40} />Polaris</div>
        <div className="tgwait-head"><span className="tgwait-icon"><Icon.Telegram /></span>使用 Telegram 登录</div>
        <div className="tgwait-code mono" aria-label="验证数字">{req.code}</div>
        <ol className="tgwait-steps">
          <li>点击下方按钮，在 Telegram 中打开 <b>@{req.bot}</b>{opened && <span className="tag ok" style={{ marginLeft: 6 }}>已打开</span>}</li>
          <li>在机器人发来的消息中点击上面的数字 <b>{req.code}</b></li>
        </ol>
        <a className="btn telegram" href={req.url} target="_blank" rel="noopener noreferrer"><Icon.Telegram />打开 Telegram</a>
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="sub" style={{ margin: 0 }}><span className="spin" />等待确认 · {Math.floor(left / 60)}:{String(left % 60).padStart(2, '0')}</span>
          <button className="btn sm" type="button" onClick={() => done.current()}>返回</button>
        </div>
        <div className="note">仅已绑定的 Telegram 管理员可用；节点管理员登录后只能看到分配给自己的节点。</div>
      </div>
    </div>
  )
}
