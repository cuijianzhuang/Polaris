import { useEffect, useRef, useState } from 'react'
import { get, post, setSudoHandler } from '../api'
import { Modal } from './ui'
import { t } from '../i18n'

// SudoPrompt: sensitive actions (backups, access settings, 2FA changes…)
// need a re-verification in the last 10 minutes. The API layer calls the
// handler registered here, which asks for the password, or the TOTP /
// recovery code when 2FA is on, then the original request is retried.
export default function SudoPrompt() {
  const [open, setOpen] = useState(false)
  const [totp, setTotp] = useState(false)
  const [value, setValue] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const waiting = useRef<((ok: boolean) => void)[]>([])

  useEffect(() => {
    setSudoHandler(() => new Promise<boolean>((resolve) => {
      waiting.current.push(resolve)
      if (waiting.current.length > 1) return // already asking: answer all at once
      setValue(''); setErr(''); setOpen(true)
      get<{ totpEnabled?: boolean }>('/auth/me').then((m) => setTotp(!!m.totpEnabled)).catch(() => {})
    }))
  }, [])

  const finish = (ok: boolean) => {
    const ws = waiting.current
    waiting.current = []
    setOpen(false)
    ws.forEach((r) => r(ok))
  }
  const submit = async () => {
    setBusy(true); setErr('')
    try {
      await post('/auth/sudo', totp ? { code: value.trim() } : { password: value })
      finish(true)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }
  if (!open) return null
  return (
    <Modal title={t('确认是你本人')} onClose={() => finish(false)}
      footer={<>
        <button className="btn" type="button" onClick={() => finish(false)}>{t('取消')}</button>
        <button className="btn pri" type="submit" form="sudo-form" disabled={busy || !value}>{busy ? t('验证中…') : t('确认')}</button>
      </>}>
      <form id="sudo-form" style={{ display: 'grid', gap: 10 }} onSubmit={(e) => { e.preventDefault(); if (value) submit() }}>
        <div className="sub" style={{ margin: 0 }}>{totp ? t('这是敏感操作，请输入二次验证码（或一个恢复码）。') : t('这是敏感操作，请输入登录密码。')}{t('验证后 10 分钟内不再询问。')}</div>
        {totp
          ? <input className="input mono" autoFocus autoComplete="one-time-code" placeholder={t('6 位验证码 / 恢复码')} value={value} onChange={(e) => setValue(e.target.value)} />
          : <input className="input" type="password" autoFocus autoComplete="current-password" placeholder={t('登录密码')} value={value} onChange={(e) => setValue(e.target.value)} />}
        {err && <div className="note bad">{err}</div>}
      </form>
    </Modal>
  )
}
