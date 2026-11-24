import { passkeysSupported, usePasskey } from '../passkeys'
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ApiError, LOGIN_INFO, get, post, setToken, takeOAuthResult } from '../api'
import { Icon } from '../components/Icons'
import LangToggle from '../components/LangToggle'
import { t } from '../i18n'
import { useBrand } from '../brand'

// One card, one entry point — the shape of a mature sign-in page. The
// visitor is never asked "admin or registered user?": third-party
// buttons are unified (the server resolves admin first, then member),
// the single account box branches by shape (an address continues with
// an e-mail code, a name with a password), and a password attempt tries
// the admin table first and the member table second, so both
// populations share the one form. Registration is the same card
// flipped, not another page.
export default function Login({ onLogin }: { onLogin: (user: string) => void }) {
  const brand = useBrand()
  const [username, setUsername] = useState('') // the one account box: a username or an e-mail address
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [recovery, setRecovery] = useState(false) // a recovery code instead of the 6-digit code
  const [needTotp, setNeedTotp] = useState(false)
  const [adminTrack, setAdminTrack] = useState(false) // the password attempt proved an admin account (TOTP step)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [passkey, setPasskey] = useState(!!LOGIN_INFO?.passkeyLogin)
  const [accountRecovery,setAccountRecovery] = useState(false)
  const [accountCode,setAccountCode] = useState('')
  const [newPassword,setNewPassword] = useState('')
  const [newPasswordAgain,setNewPasswordAgain] = useState('')
  const [recovered,setRecovered] = useState(false)
  const [github, setGithub] = useState(!!LOGIN_INFO?.githubLogin)
  const [ticket, setTicket] = useState('') // GitHub verified, 2FA code still needed
  const [telegram, setTelegram] = useState(!!LOGIN_INFO?.telegramLogin)
  const [sso, setSso] = useState<{ id: string; name: string }[]>(LOGIN_INFO?.sso ?? [])
  const [tg, setTg] = useState<TGRequest | null>(null)
  // Member sign-in flags (registered users, their own nodes only). The
  // unified page shows a method when either population can use it; the
  // member flags still gate the member-only paths (password fallback,
  // reset, registration).
  const [mailLogin, setMailLogin] = useState(!!LOGIN_INFO?.memberMailLogin)
  const [pwLogin, setPwLogin] = useState(!!LOGIN_INFO?.memberPasswordLogin)
  const [pwRegister, setPwRegister] = useState(!!LOGIN_INFO?.memberPasswordRegister)
  const [mailRegister, setMailRegister] = useState(!!LOGIN_INFO?.memberMailRegister)
  const [memberGithub, setMemberGithub] = useState(!!LOGIN_INFO?.memberGithubLogin)
  const [memberPasskey, setMemberPasskey] = useState(!!LOGIN_INFO?.memberPasskeyLogin)
  const [view, setView] = useState<'login' | 'register'>('login')
  const [step, setStep] = useState<'account' | 'mail' | 'password' | 'reset'>('account')
  const [regMode, setRegMode] = useState<'mail' | 'pw'>('mail')
  const [regEmail, setRegEmail] = useState('')
  const [note, setNote] = useState('')
  const [accountHint, setAccountHint] = useState('')
  const [email, setEmail] = useState('')
  const [mailCode, setMailCode] = useState('')
  const [cooldown, setCooldown] = useState(0)
  const accountRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (!cooldown) return
    const id = setTimeout(() => setCooldown(cooldown - 1), 1000)
    return () => clearTimeout(id)
  }, [cooldown])
  useEffect(() => {
    // Production index.html inlines the flags; only fetch when they are
    // absent (vite dev server), otherwise the buttons would pop in late.
    if (!LOGIN_INFO) get<{ passkeyLogin?: boolean; githubLogin?: boolean; telegramLogin?: boolean; memberMailLogin?: boolean; memberPasswordLogin?: boolean; memberPasswordRegister?: boolean; memberMailRegister?: boolean; memberGithubLogin?: boolean; memberPasskeyLogin?: boolean; memberSSOLogin?: boolean; sso?: { id: string; name: string }[] }>('/info').then((i) => { setPasskey(!!i.passkeyLogin); setGithub(!!i.githubLogin); setTelegram(!!i.telegramLogin); setMailLogin(!!i.memberMailLogin); setPwLogin(!!i.memberPasswordLogin); setPwRegister(!!i.memberPasswordRegister); setMailRegister(!!i.memberMailRegister); setMemberGithub(!!i.memberGithubLogin); setMemberPasskey(!!i.memberPasskeyLogin); setSso(i.sso ?? []) }).catch(() => {})
    const r = takeOAuthResult()
    if (r.error) setErr(r.error)
    if (r.ticket) { setTicket(r.ticket); setErr('') }
  }, [])

  const account = username.trim()
  const isEmail = account.includes('@')
  const anyGithub = github || memberGithub
  const anyPasskey = passkey || memberPasskey
  const anyThirdParty = anyGithub || sso.length > 0 || anyPasskey || telegram
  const anyRegister = mailRegister || pwRegister

  // --- unified third-party entries (server resolves admin → member) ---
  const githubAnyLogin = async () => {
    setBusy(true); setErr('')
    try { location.href = (await post<{ url: string }>('/auth/github/any/login-url')).url } catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false) }
  }
  const ssoAnyLogin = async (id: string) => {
    setBusy(true); setErr('')
    try { location.href = (await post<{ url: string }>('/auth/sso/any/login-url', { providerId: id })).url } catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false) }
  }
  const passkeyAnyLogin = async () => {
    setBusy(true); setErr('')
    try {
      const begin = await post<{ challengeId: string; options: any }>('/auth/passkeys/any/login/begin')
      const credential = await usePasskey(begin.options)
      const r = await post<{ token: string; username: string }>('/auth/passkeys/any/login/finish', { challengeId: begin.challengeId, credential })
      setToken(r.token); onLogin(r.username)
    } catch (e) { setErr(e instanceof DOMException && e.name === 'NotAllowedError' ? t('设备验证已取消或超时，请重试。') : e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
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

  // --- e-mail code (sign-in and, with sign-up open, registration) ---
  const sendMailCode = async () => {
    setBusy(true); setErr('')
    try {
      const r = await post<{ cooldown?: number }>('/auth/mail/code', { email })
      setCooldown(r.cooldown ?? 60)
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }
  const submitMail = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true); setErr('')
    try {
      const r = await post<{ token: string; username: string }>('/auth/mail/verify', { email, code: mailCode })
      setToken(r.token)
      onLogin(r.username)
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }

  // --- password reset by e-mail code (member accounts) ---
  const sendResetCode = async () => {
    setBusy(true); setErr('')
    try {
      const r = await post<{ cooldown?: number }>('/auth/mail/code', { email, purpose: 'reset' })
      setCooldown(r.cooldown ?? 60)
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }
  const submitReset = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true); setErr('')
    try {
      await post('/auth/member/password/reset', { email, code: mailCode, newPassword })
      setStep('password'); setNote(t('密码已重置，请用新密码登录')); setMailCode(''); setNewPassword('')
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }

  // --- the account box and the password step ---
  const submitAccount = (e: FormEvent) => {
    e.preventDefault()
    if (!account) return
    setErr(''); setAccountHint(''); setNote('')
    if (isEmail) { setEmail(account); setStep('mail') } else setStep('password')
  }
  const backToAccount = () => {
    setStep('account'); setErr(''); setNeedTotp(false); setAdminTrack(false); setTotp('')
    setTimeout(() => accountRef.current?.focus(), 0)
  }
  const askEmailForCode = () => {
    backToAccount()
    setAccountHint(t('请输入邮箱以使用验证码登录'))
  }
  // An e-mail account already carries its address: jump straight to
  // the code step with it filled in. A username has no address yet,
  // so it goes back to the account box with a hint instead.
  const switchToMailCode = () => {
    if (isEmail) { setEmail(account); setStep('mail'); setErr('') }
    else askEmailForCode()
  }
  // Dual attempt: the admin endpoint answers first. Only a plain
  // credential failure (401 with neither a TOTP demand nor a lockout
  // marker) falls through to the member endpoint — a TOTP demand
  // proves the account is an admin's and stays on the admin track,
  // and a lockout or rate limit must not be spent twice. Both
  // endpoints feed the same server-side IP ban pool either way.
  const submitPassword = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true); setErr('')
    try {
      const r = await post<{ token: string; username: string }>('/auth/login', { username: account, password, totp })
      setToken(r.token)
      onLogin(r.username)
    } catch (e1) {
      if (e1 instanceof ApiError && e1.data?.totpRequired) {
        setNeedTotp(true); setAdminTrack(true); setErr(t('请输入二次验证码'))
      } else if (adminTrack) {
        setErr(e1 instanceof Error ? e1.message : String(e1))
      } else if (pwLogin && e1 instanceof ApiError && e1.status === 401 && !e1.data?.lockedUntil) {
        try {
          const r = await post<{ token: string; username: string }>('/auth/member/password', { username: account, password })
          setToken(r.token)
          onLogin(r.username)
        } catch (e2) { setErr(e2 instanceof Error ? e2.message : String(e2)) }
      } else {
        setErr(e1 instanceof Error ? e1.message : String(e1))
      }
    } finally { setBusy(false) }
  }

  // --- registration (password form; the mail form is submitMail) ---
  const submitRegisterPw = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true); setErr('')
    try {
      const r = await post<{ token: string; username: string }>('/auth/member/register', { username: account, password, email: regEmail || undefined })
      setToken(r.token)
      onLogin(r.username)
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }
  const goRegister = () => {
    setView('register'); setErr(''); setNote('')
    setRegMode(mailRegister ? 'mail' : 'pw')
    if (isEmail) setEmail(account)
  }
  const goLogin = () => { setView('login'); setStep('account'); setErr('') }

  const onSubmit = view === 'register'
    ? (regMode === 'mail' ? submitMail : submitRegisterPw)
    : step === 'account' ? submitAccount
    : step === 'mail' ? submitMail
    : step === 'reset' ? submitReset
    : submitPassword

  if (accountRecovery) return <div className="login"><form className="card" onSubmit={async(e)=>{
    e.preventDefault();setBusy(true);setErr('')
    try {await post('/auth/account-recovery',{username,code:accountCode,password:newPassword});setRecovered(true);setAccountCode('');setPassword('');setNewPassword('');setNewPasswordAgain('');setAccountRecovery(false)} catch(e){setErr(e instanceof Error?e.message:String(e))} finally {setBusy(false)}
  }}>
    <h1>{t('恢复管理员账号')}</h1>
    <div className="note warn">{t('使用离线账号恢复码重置密码。这将关闭 TOTP、撤销全部通行密钥和登录会话，整组旧恢复码随即失效。')}</div>
    <label className="lbl" htmlFor="recover-user">{t('用户名')}</label><input id="recover-user" className="input" value={username} onChange={(e)=>setUsername(e.target.value)} autoComplete="username" required />
    <label className="lbl" htmlFor="recover-code">{t('账号恢复码')}</label><input id="recover-code" className="input mono" value={accountCode} onChange={(e)=>setAccountCode(e.target.value)} autoComplete="off" autoCapitalize="none" spellCheck={false} required />
    <label className="lbl" htmlFor="recover-password">{t('新密码')}</label><input id="recover-password" className="input" type="password" value={newPassword} onChange={(e)=>setNewPassword(e.target.value)} autoComplete="new-password" minLength={8} required />
    <label className="lbl" htmlFor="recover-password-again">{t('确认新密码')}</label><input id="recover-password-again" className="input" type="password" value={newPasswordAgain} onChange={(e)=>setNewPasswordAgain(e.target.value)} autoComplete="new-password" minLength={8} required />
    {newPasswordAgain && newPasswordAgain!==newPassword && <div className="hint">{t('两次输入的新密码不一致')}</div>}
    {err&&<div className="note bad">{err}</div>}
    <button className="btn pri" disabled={busy || newPassword.length<8 || newPassword!==newPasswordAgain || !username || !accountCode} type="submit">{t('重置并撤销旧登录凭据')}</button>
    <button className="btn" disabled={busy} type="button" onClick={()=>{setAccountRecovery(false);setErr('');setAccountCode('');setNewPassword('');setNewPasswordAgain('')}}>{t('返回登录')}</button>
  </form></div>

  if (tg) {
    return <TelegramWait req={tg} onDone={(user, error) => { setTg(null); if (user) onLogin(user); else if (error) setErr(error) }} />
  }

  if (ticket) {
    return (
      <div className="login">
        <form className="card" onSubmit={submitTicket}>
          <div className="row" style={{ gap: 12, fontWeight: 600, fontSize: 20 }}><Icon.Brand size={40} />{brand.title}</div>
          <div className="note">{t('已通过 GitHub 验证，请输入二次验证码完成登录。')}</div>
          <div className="field">
            <label className="lbl" htmlFor="gt">{t('二次验证码')}</label>
            <input className="input mono" id="gt" inputMode={recovery ? 'text' : 'numeric'} autoCapitalize="none" autoComplete="one-time-code" placeholder={recovery ? 'xxxxx-xxxxx' : t('6 位数字')} value={totp} onChange={(e) => setTotp(e.target.value)} autoFocus />
            <button className="link small" type="button" style={{ justifySelf: 'start', marginTop: 4 }} onClick={() => { setRecovery(!recovery); setTotp('') }}>{recovery ? t('使用验证器的 6 位验证码') : t('验证器不在身边？使用恢复码')}</button>
          </div>
          {err && <div className="note bad">{err}</div>}
          <button className="btn pri" type="submit" disabled={busy || totp.length < 6}>{busy ? t('登录中…') : t('登录')}</button>
        </form>
      </div>
    )
  }

  const accountChip = (
    <div className="login-acct">
      <span className="mono">{account}</span>
      <button className="link small" type="button" onClick={backToAccount}>{t('换一个')}</button>
    </div>
  )
  const codeField = (id: string, send: () => void) => (
    <div className="field">
      <label className="lbl" htmlFor={id}>{t('邮箱验证码')}</label>
      <div className="row">
        <input className="input mono" id={id} inputMode="numeric" autoCapitalize="none" autoComplete="one-time-code" placeholder={t('6 位数字')} value={mailCode} onChange={(e) => setMailCode(e.target.value)} required />
        <button className="btn" type="button" disabled={busy || cooldown > 0 || !email} onClick={send}>{cooldown > 0 ? t('{n} 秒后重发', { n: cooldown }) : t('发送验证码')}</button>
      </div>
    </div>
  )

  return (
    <div className="login login-home">
      <aside className="login-visual">
        <div className="login-visual-brand">
          <span className="login-visual-logo"><Icon.Brand size={40} /></span>
          <span><strong>{brand.title}</strong><small>{t('节点管理控制台')}</small></span>
        </div>
        <div className="login-visual-copy">
          <span className="login-visual-kicker">{t('多内核代理管理面板')}</span>
          <h1>{t('统一管理多台节点')}</h1>
          <p>{t('在一个清晰的工作区中，掌握节点状态、流量与配置下发。')}</p>
          <ul className="login-features">
            <li><Icon.Server />{t('节点状态')}</li>
            <li><Icon.Pulse />{t('实时流量概览')}</li>
            <li><Icon.Upload />{t('配置下发')}</li>
          </ul>
        </div>
        <div className="login-visual-foot"><Icon.Lock /><span>{t('为日常运维打造的统一控制台')}</span></div>
      </aside>
      <section className="login-stage" aria-label={t('登录')}>
        <form className="card login-card" onSubmit={onSubmit}>
 {recovered && <div className="note">{t('账号已恢复，请用新密码登录，并重新设置通行密钥和二次验证。')}</div>}
          <div className="row login-card-brand"><LangToggle /></div>
          {view === 'register' ? (
            <>
              <div className="login-form-copy"><h2>{t('创建账号')}</h2><p>{t('注册用户只能管理自己的节点。')}</p></div>
              {mailRegister && pwRegister && (
                <div className="seg" role="tablist" aria-label={t('注册方式')}>
                  <button type="button" className={'opt' + (regMode === 'mail' ? ' on' : '')} aria-pressed={regMode === 'mail'} onClick={() => { setRegMode('mail'); setErr('') }}>{t('邮箱验证码')}</button>
                  <button type="button" className={'opt' + (regMode === 'pw' ? ' on' : '')} aria-pressed={regMode === 'pw'} onClick={() => { setRegMode('pw'); setErr('') }}>{t('账号密码')}</button>
                </div>
              )}
              {regMode === 'mail' ? (
                <>
                  <div className="field">
                    <label className="lbl" htmlFor="remail">{t('邮箱')}</label>
                    <input className="input" id="remail" type="email" autoComplete="email" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} required />
                  </div>
                  {codeField('rmc', sendMailCode)}
                  <div className="hint">{t('验证邮箱后即完成注册并登录；注册用户只能管理自己的节点。')}</div>
                </>
              ) : (
                <>
                  <div className="field">
                    <label className="lbl" htmlFor="ru">{t('用户名')}</label>
                    <input className="input" id="ru" autoComplete="username" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={username} onChange={(e) => setUsername(e.target.value)} required />
                  </div>
                  <div className="field">
                    <label className="lbl" htmlFor="rp">{t('密码')}</label>
                    <input className="input" id="rp" type="password" autoComplete="new-password" autoCapitalize="none" autoCorrect="off" spellCheck={false} minLength={8} value={password} onChange={(e) => setPassword(e.target.value)} required />
                  </div>
                  <div className="field">
                    <label className="lbl" htmlFor="rre">{t('邮箱（选填）')}</label>
                    <input className="input" id="rre" type="email" autoComplete="email" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={regEmail} onChange={(e) => setRegEmail(e.target.value)} />
                    <div className="hint">{t('选填，仅作联系信息；绑定邮箱需在用户中心用验证码确认。')}</div>
                  </div>
                </>
              )}
              {err && <div className="note bad">{err}</div>}
              <button className="btn pri" type="submit" disabled={busy}>{busy ? t('登录中…') : t('注册并登录')}</button>
              <div className="hint login-alt">{t('已有账号？')} <button className="link small" type="button" onClick={goLogin}>{t('去登录')}</button></div>
            </>
          ) : (
            <>
              <div className="login-form-copy"><h2>{t('欢迎回来')}</h2><p>{t('登录以继续管理节点。')}</p></div>
              {step === 'account' && (
                <>
                  {anyGithub && <button className="btn github" type="button" disabled={busy} onClick={githubAnyLogin}><Icon.GitHub />{t('使用 GitHub 登录')}</button>}
                  {sso.map((p) => <button key={p.id} className="btn" type="button" disabled={busy} onClick={() => ssoAnyLogin(p.id)}><Icon.Lock />{t('使用 {name} 登录', { name: p.name })}</button>)}
                  {anyPasskey && passkeysSupported() && <button className="btn" type="button" disabled={busy} onClick={passkeyAnyLogin}><Icon.Lock />{t('使用通行密钥登录')}</button>}
                  {passkey && <button className="link small" type="button" disabled={busy} onClick={() => { setAccountRecovery(true); setErr('') }}>{t('无法使用通行密钥？恢复账号')}</button>}
                  {telegram && <button className="btn telegram" type="button" disabled={busy} onClick={telegramLogin}><Icon.Telegram />{t('使用 Telegram 登录')}</button>}
                  {anyThirdParty && <div className="or"><span>{t('或')}</span></div>}
                  <div className="field">
                    <label className="lbl" htmlFor="acct">{t('账号')}</label>
                    <input className="input" id="acct" ref={accountRef} autoComplete="username" autoCapitalize="none" autoCorrect="off" spellCheck={false} placeholder={t('用户名或邮箱')} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus required />
                    {accountHint && <div className="hint">{accountHint}</div>}
                  </div>
                </>
              )}
              {step === 'mail' && (
                <>
                  {accountChip}
                  {codeField('mc', sendMailCode)}
                  <div className="hint">{mailRegister ? t('注册用户用邮箱验证码登录，只能管理自己的节点；没有账号且开放注册时将自动注册。') : t('注册用户用邮箱验证码登录，只能管理自己的节点。')}</div>
                  <div className="row">
                    <button className="link small" type="button" onClick={() => { setStep('password'); setErr('') }}>{t('使用密码登录')}</button>
                    <button className="link small" type="button" onClick={backToAccount}>{t('其他登录方式')}</button>
                  </div>
                </>
              )}
              {step === 'password' && (
                <>
                  {accountChip}
                  <div className="field">
                    <label className="lbl" htmlFor="p">{t('密码')}</label>
                    <input className="input" id="p" type="password" autoComplete="current-password" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={password} onChange={(e) => setPassword(e.target.value)} autoFocus required />
                  </div>
                  {needTotp && (
                    <div className="field">
                      <label className="lbl" htmlFor="t">{t('二次验证码')}</label>
                      <input className="input mono" id="t" inputMode={recovery ? 'text' : 'numeric'} autoCapitalize="none" autoComplete="one-time-code" placeholder={recovery ? 'xxxxx-xxxxx' : t('6 位数字')} value={totp} onChange={(e) => setTotp(e.target.value)} autoFocus />
                      <button className="link small" type="button" style={{ justifySelf: 'start', marginTop: 4 }} onClick={() => { setRecovery(!recovery); setTotp('') }}>{recovery ? t('使用验证器的 6 位验证码') : t('验证器不在身边？使用恢复码')}</button>
                    </div>
                  )}
                  {note && <div className="note">{note}</div>}
                  <div className="row">
                    {mailLogin && <button className="link small" type="button" onClick={switchToMailCode}>{t('使用邮箱验证码登录')}</button>}
                    {pwLogin && <button className="link small" type="button" onClick={() => { if (isEmail) setEmail(account); setStep('reset'); setErr('') }}>{t('忘记密码')}</button>}
                    <button className="link small" type="button" onClick={backToAccount}>{t('其他登录方式')}</button>
                  </div>
                </>
              )}
              {step === 'reset' && (
                <>
                  <div className="field">
                    <label className="lbl" htmlFor="rem">{t('邮箱')}</label>
                    <input className="input" id="rem" type="email" autoComplete="email" autoCapitalize="none" autoCorrect="off" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} required />
                  </div>
                  {codeField('rmc2', sendResetCode)}
                  <div className="field">
                    <label className="lbl" htmlFor="rnp">{t('新密码')}</label>
                    <input className="input" id="rnp" type="password" autoComplete="new-password" autoCapitalize="none" autoCorrect="off" spellCheck={false} minLength={8} value={newPassword} onChange={(e) => setNewPassword(e.target.value)} required />
                    <div className="hint">{t('用账号绑定的邮箱验证码重置登录密码；重置后所有设备的登录都会失效。')}</div>
                  </div>
                  <button className="link small" type="button" style={{ justifySelf: 'start' }} onClick={() => { setStep('password'); setErr('') }}>{t('返回登录')}</button>
                </>
              )}
              {err && <div className="note bad">{err}</div>}
              <button className="btn pri" type="submit" disabled={busy || (step === 'account' && !account)}>{busy ? t('登录中…') : step === 'account' ? t('下一步') : step === 'reset' ? t('重置密码') : t('登录')}</button>
              {step === 'account' && anyRegister && (
                <div className="hint login-alt">{t('没有账号？')} <button className="link small" type="button" onClick={goRegister}>{t('立即注册')}</button></div>
              )}
            </>
          )}
        </form>
      </section>
    </div>
  )
}

interface TGRequest { id: string; secret: string; code: number; url: string; bot: string; expiresIn: number }

// Waits for the admin to confirm in Telegram by tapping the number shown here.
function TelegramWait({ req, onDone }: { req: TGRequest; onDone: (user?: string, error?: string) => void }) {
  const brand = useBrand()
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
          else done.current(undefined, r.status === 'denied' ? t('已在 Telegram 中拒绝本次登录') : t('登录请求已过期，请重试'))
          return
        } catch { /* transient: keep polling until expiry */ }
        if (Date.now() > end + 5000) { done.current(undefined, t('登录请求已过期，请重试')); return }
      }
    }
    poll()
    return () => { stop = true; clearInterval(tick) }
  }, [req])
  return (
    <div className="login">
      <div className="card tgwait">
        <div className="row" style={{ gap: 12, fontWeight: 600, fontSize: 20 }}><Icon.Brand size={40} />{brand.title}</div>
        <div className="tgwait-head"><span className="tgwait-icon"><Icon.Telegram /></span>{t('使用 Telegram 登录')}</div>
        <div className="tgwait-code mono" aria-label={t('验证数字')}>{req.code}</div>
        <ol className="tgwait-steps">
          <li>{t('点击下方按钮，在 Telegram 中打开')} <b>@{req.bot}</b>{opened && <span className="tag ok" style={{ marginLeft: 6 }}>{t('已打开')}</span>}</li>
          <li>{t('在机器人发来的消息中点击上面的数字')} <b>{req.code}</b></li>
        </ol>
        <a className="btn telegram" href={req.url} target="_blank" rel="noopener noreferrer"><Icon.Telegram />{t('打开 Telegram')}</a>
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="sub" style={{ margin: 0 }}><span className="spin" />{t('等待确认')} · {Math.floor(left / 60)}:{String(left % 60).padStart(2, '0')}</span>
          <button className="btn sm" type="button" onClick={() => done.current()}>{t('返回')}</button>
        </div>
        <div className="note">{t('仅已绑定的 Telegram 管理员可用；节点管理员登录后只能看到分配给自己的节点。')}</div>
      </div>
    </div>
  )
}
