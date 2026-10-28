import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../../components/SettingsDrafts'
import { useEffect, useState } from 'react'
import { get, post, put } from '../../api'
import { Field, Switch, useAction, useLoad } from '../../components/ui'
import { t } from '../../i18n'

export const mailKeys = ['smtp_host', 'smtp_port', 'smtp_security', 'smtp_user', 'smtp_from', 'smtp_from_name', 'mail_notify_users', 'mail_notify_admin', 'mail_admin_to'] as const

// MailSettings: the SMTP account, a test message, and who gets mail.
export function MailSettings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [baseline, setBaseline] = useState('')
  const saver = useSettingsSave()
  const [pass, setPass] = useState('')
  const [to, setTo] = useState('')
  useEffect(() => {
    if (s.data) { const next = Object.fromEntries(mailKeys.map((k) => [k, String(s.data?.[k] ?? '')])); setF(next); setBaseline(JSON.stringify(next)) }
  }, [s.data])
  const { busy: actionBusy, run } = useAction()
  const busy = actionBusy || saver.busy
  const dirty = !!baseline && (JSON.stringify(f) !== baseline || !!pass)
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const security = f.smtp_security || 'starttls'
  const invalidPort = !!f.smtp_port && (Number(f.smtp_port) < 1 || Number(f.smtp_port) > 65535)
  const save = async () => {
    if (!dirty) return true
    if (busy || s.loading || !s.data || invalidPort) return false
    return saver.run(async () => {
      const body: Record<string, string> = { ...f, smtp_security: security, mail_notify_users: f.mail_notify_users === 'false' ? 'false' : 'true', mail_notify_admin: f.mail_notify_admin === 'false' ? 'false' : 'true' }
      if (pass) body.smtp_password = pass
      await put('/settings', body); setPass(''); setBaseline(JSON.stringify(f)); s.reload()
    }, t('邮件设置已保存'))
  }
  useSettingsDraft({ label: t('邮件'), dirty, busy, invalid: invalidPort, save, discard: () => { setF(JSON.parse(baseline || '{}')); setPass('') } })
  const test = () => run(async () => {
    const r = await post<{ ok: boolean; error?: string }>('/mail/test', { to, host: f.smtp_host, port: f.smtp_port, security, user: f.smtp_user, password: pass, from: f.smtp_from, fromName: f.smtp_from_name })
    if (!r.ok) throw new Error(t('发送失败：{err}', { err: r.error ?? '' }))
  }, t('测试邮件已发出，请查收'))
  const passSet = !!s.data?.smtp_password_set
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('邮件（SMTP）')}</h2>
      <fieldset className="settings-fields" disabled={busy || s.loading || !baseline}>
      <div className="hint">{t('配置后，填了邮箱的用户会收到和 Telegram 相同的提醒（流量、到期、停用、恢复），以及订单开通通知。提醒阈值在「Telegram」页设置，两边共用。')}</div>
      <div className="form-grid">
        <Field label={t('SMTP 服务器')}><input className="input mono" value={f.smtp_host || ''} placeholder="smtp.example.com" onChange={(e) => set('smtp_host', e.target.value.trim())} /></Field>
        <Field label={t('加密方式')}>
          <select className="input" value={security} onChange={(e) => set('smtp_security', e.target.value)}>
            <option value="starttls">STARTTLS（587）</option><option value="tls">SSL/TLS（465）</option><option value="none">{t('不加密（25，仅限内网中继）')}</option>
          </select>
        </Field>
        <Field label={t('端口')} hint={invalidPort ? <span role="alert" className="field-error">{t('端口须为 1–65535，或留空使用默认值。')}</span> : t('留空按加密方式使用默认端口')}><input className="input mono" inputMode="numeric" aria-invalid={invalidPort} value={f.smtp_port || ''} placeholder={security === 'tls' ? '465' : security === 'none' ? '25' : '587'} onChange={(e) => set('smtp_port', e.target.value.replace(/[^\d]/g, ''))} /></Field>
        <Field label={t('用户名')} hint={t('通常是完整邮箱地址；中继不需要时留空')}><input className="input mono" autoComplete="off" value={f.smtp_user || ''} onChange={(e) => set('smtp_user', e.target.value.trim())} /></Field>
        <Field label={t('密码 / 授权码')} hint={passSet ? t('已保存（留空不修改）') : t('QQ / 163 邮箱填授权码，Gmail 填应用专用密码')}><input className="input mono" type="password" autoComplete="new-password" value={pass} placeholder={passSet ? '••••••••' : ''} onChange={(e) => setPass(e.target.value)} /></Field>
        <Field label={t('发件地址')}><input className="input mono" value={f.smtp_from || ''} placeholder="noreply@example.com" onChange={(e) => set('smtp_from', e.target.value.trim())} /></Field>
        <Field label={t('发件人名称')} hint={t('可留空')}><input className="input" value={f.smtp_from_name || ''} onChange={(e) => set('smtp_from_name', e.target.value)} /></Field>
        <Field label={t('管理员收件地址')} hint={t('可留空；开启管理员邮件通知后，新订单、备份失败、升级结果、节点告警等通知发送到这里')}><input className="input mono" value={f.mail_admin_to || ''} placeholder="ops@example.com" onChange={(e) => set('mail_admin_to', e.target.value.trim())} /></Field>
      </div>
      <div className="toggle"><span>{t('给用户发提醒邮件')}</span><Switch label={t('给用户发提醒邮件')} on={f.mail_notify_users !== 'false'} onChange={(v) => set('mail_notify_users', String(v))} /></div>
      <div className="toggle"><span>{t('给管理员发通知邮件')}</span><Switch label={t('给管理员发通知邮件')} on={f.mail_notify_admin !== 'false'} onChange={(v) => set('mail_notify_admin', String(v))} /></div>
      <SettingsSaveBar loading={s.loading} dirty={dirty} busy={saver.busy} error={saver.error || s.error} disabled={s.loading || !s.data || invalidPort} onSave={save} />
      <div className="row">
        <input className="input mono" style={{ width: 'auto', minWidth: 200 }} type="email" value={to} placeholder={t('测试收件地址')} onChange={(e) => setTo(e.target.value)} aria-label={t('测试收件地址')} />
        <button className="btn" type="button" disabled={busy || !to || !f.smtp_host} onClick={test}>{t('发送测试邮件')}</button>
      </div>
      </fieldset>
    </section>
  )
}
