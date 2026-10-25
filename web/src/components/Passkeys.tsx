import { useState } from 'react'
import { del, get, post } from '../api'
import { createPasskey, passkeysSupported } from '../passkeys'
import { ago } from '../format'
import { t } from '../i18n'
import { CopyBox, ErrorNote, Field, Modal, useAction, useConfirm, useLoad } from './ui'

type Key = { id: string; name: string; rpId: string; createdAt: string; lastUsedAt?: string }
type State = { items: Key[]; available: boolean; reason?: string; origin: string; recoveryCodesLeft: number }
export default function Passkeys() {
  const state = useLoad(() => get<State>('/auth/passkeys'))
  const [name, setName] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const supported = passkeysSupported()
  return <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
    <h2>{t('通行密钥')}</h2>
    <div className="hint">{t('使用设备解锁、指纹或面容登录。每次挑战仅可使用一次；密钥绑定当前公开域名，私钥由设备保管。')}</div>
    <ErrorNote error={state.error} />
 {state.data?.origin && <div className="hint mono">{state.data.origin}</div>}
    {state.data && !state.data.available && <div className="note warn">{state.data.reason}</div>}
    {!supported && <div className="note warn">{t('当前浏览器或连接不支持通行密钥，请使用 HTTPS 和支持 WebAuthn 的浏览器。')}</div>}
    <Field label={t('通行密钥名称')} htmlFor="passkey-name"><input id="passkey-name" className="input" value={name} maxLength={64} onChange={(e) => setName(e.target.value)} placeholder={t('例如：我的手机或安全密钥')} /></Field>
    <div><button className="btn pri" type="button" disabled={busy || !name.trim() || !supported || !state.data?.available} onClick={() => run(async () => {
      const begin = await post<{ challengeId: string; options: any }>('/auth/passkeys/register/begin', { name })
      const credential = await createPasskey(begin.options)
      const result = await post<{ recoveryCodes: string[] }>('/auth/passkeys/register/finish', { challengeId: begin.challengeId, credential })
      setName(''); state.reload(); if (result.recoveryCodes?.length) setCodes(result.recoveryCodes)
    }, t('通行密钥已注册'))}>{busy ? t('等待设备验证…') : t('注册通行密钥')}</button></div>
    <div className="scroll"><table><thead><tr><th>{t('名称')}</th><th>{t('绑定域名')}</th><th>{t('最近使用')}</th><th>{t('操作')}</th></tr></thead><tbody>
      {(state.data?.items ?? []).map((key) => <tr key={key.id}><td>{key.name}</td><td className="mono small">{key.rpId}</td><td>{key.lastUsedAt ? ago(key.lastUsedAt) : t('从未使用')}</td><td><button className="link" type="button" disabled={busy} onClick={() => run(async () => {
        if (await confirm(t('撤销通行密钥 {name}？使用该密钥登录的会话也将失效。', { name: key.name }), { danger: true })) { await del('/auth/passkeys/' + key.id); state.reload() }
      })}>{t('撤销')}</button></td></tr>)}
      {!state.data?.items.length && <tr><td colSpan={4} className="mute">{t('尚未注册通行密钥')}</td></tr>}
    </tbody></table></div>
    <h3>{t('账号恢复')}</h3>
    <div className="hint">{t('恢复码与 TOTP 恢复码不同。恢复账号会重置密码、关闭 TOTP、撤销全部通行密钥和登录会话，并使整组旧恢复码失效。')}</div>
    <div>{t('账号恢复码剩余 {n} 个', { n: state.data?.recoveryCodesLeft ?? 0 })}</div>
    <div><button className="btn" type="button" disabled={busy || !state.data?.available} onClick={() => run(async () => {
      if (await confirm(t('重新生成账号恢复码？此前保存的整组账号恢复码将立即失效。'), { danger: true })) {
        const r = await post<{ recoveryCodes: string[] }>('/auth/account-recovery-codes'); setCodes(r.recoveryCodes); state.reload()
      }
    })}>{t('重新生成账号恢复码')}</button></div>
    {codes && <Modal title={t('保存账号恢复码')} onClose={() => setCodes(null)}>
      <div className="note warn">{t('请离线安全保存，这些恢复码仅展示本次。每个恢复码都能重置整个账号，请勿与他人分享。')}</div>
      <CopyBox text={codes.join('\n')} />
      <button className="btn pri" type="button" onClick={() => setCodes(null)}>{t('我已保存')}</button>
    </Modal>}
  </section>
}
