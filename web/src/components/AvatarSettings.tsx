import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { t } from '../i18n'
import { Field, useLoad } from './ui'
import { useSettingsDraft, useSettingsSave } from './SettingsDrafts'
import Avatar from './Avatar'

type Profile = { username: string; avatarEmail?: string; avatarURL?: string }

export default function AvatarSettings({ compact = false, onSaved }: { compact?: boolean; onSaved?: () => void }) {
  const data = useLoad(() => get<Profile>('/auth/me'))
  const [email, setEmail] = useState('')
  const [baseline, setBaseline] = useState('')
  const saver = useSettingsSave()
  useEffect(() => { if (data.data) { const value = data.data.avatarEmail || ''; setEmail(value); setBaseline(value) } }, [data.data])
  const dirty = email !== baseline
  const invalid = !!email.trim() && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim())
  const save = () => saver.run(async () => {
    const profile = await put<{ avatarEmail: string; avatarURL: string }>('/auth/avatar', { email: email.trim() })
    setEmail(profile.avatarEmail); setBaseline(profile.avatarEmail)
    window.dispatchEvent(new Event('polaris-avatar-updated'))
    data.reload(); onSaved?.()
  })
  useSettingsDraft({ label: t('账号头像'), dirty: !compact && dirty, busy: saver.busy, invalid, save })
  return <section className={compact ? 'avatar-settings' : 'card avatar-settings'} aria-busy={saver.busy}>
    {!compact && <h2>{t('账号头像')}</h2>}
    <div className="avatar-settings-preview"><Avatar src={data.data?.avatarURL} name={data.data?.username || '?'} size={48} /><p className="hint">{t('使用头像邮箱对应的 Gravatar；未设置时显示默认头像。')}</p></div>
    <Field label={t('头像邮箱')} htmlFor="profile-avatar-email" hint={t('此邮箱仅用于头像显示，不作为登录或找回密码的地址。')}>
      <input id="profile-avatar-email" className="input" type="email" autoComplete="off" maxLength={254} placeholder="name@example.com" value={email} onChange={(event) => setEmail(event.target.value)} disabled={!data.data || saver.busy} />
    </Field>
    {data.error && <p className="note bad" role="alert">{data.error}</p>}
    {saver.error && <p className="note bad" role="alert">{saver.error}</p>}
    <div><button className="btn pri" type="button" disabled={!data.data || !dirty || invalid || saver.busy} onClick={save}>{saver.busy ? t('保存中…') : t('保存头像设置')}</button></div>
  </section>
}
