import { useState } from 'react'
import { del, get, post, put } from '../api'
import { Field, Modal, useAction, useConfirm, useLoad } from './ui'
import { t } from '../i18n'

export const roleLabels = (): Record<string, string> => ({ owner: t('超级管理员'), operator: t('节点运维'), viewer: t('只读运维'), support: t('客服'), finance: t('财务只读') })
export const roleDetails = (): Record<string, string> => ({
  owner: t('全部功能，包含密钥、支付设置和后台账号管理。'),
  operator: t('仅限分配节点：查看状态和日志、强制同步、配置回滚、重启内核。日志可能包含业务信息。'),
  viewer: t('仅查看分配节点的资源与在线摘要，不读取配置、日志或用户凭据。'),
  support: t('查看全部用户的状态与配额摘要，读取、回复及关闭工单；不能修改权益或读取代理凭据。'),
  finance: t('只读查看全部订单金额、支付状态与服务器成本；不能退款、交付、导出凭据或修改支付设置。'),
})
type Admin = { id: string; username: string; role: string; nodeIds: string[] | null; totpEnabled: boolean }
type Form = { id?: string; username: string; password: string; role: string; nodeIds: string[] }

export default function AdminAccounts() {
  const list = useLoad(() => get<Admin[]>('/admins'))
  const nodes = useLoad(() => get<{ id: string; name: string }[]>('/nodes'))
  const me = useLoad(() => get<{ id: string }>('/auth/me'))
  const [form, setForm] = useState<Form | null>(null)
  const [reset, setReset] = useState<Admin | null>(null)
  const [password, setPassword] = useState('')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const nodeRole = form?.role === 'viewer' || form?.role === 'operator'
  return <section className="card" style={{ display: 'grid', gap: 12 }}>
    <div className="ctitle"><h2>{t('后台账号与角色')}</h2><button className="btn" type="button" onClick={() => setForm({ username: '', password: '', role: 'support', nodeIds: [] })}>{t('添加后台账号')}</button></div>
    <p className="hint">{t('角色或节点范围变化会撤销该账号的全部登录。至少保留一个超级管理员；其他角色不能管理账号或创建 API 令牌。')}</p>
    {list.error && <div className="note bad" role="alert">{list.error}</div>}
    <div className="scroll"><table><thead><tr><th>{t('用户名')}</th><th>{t('角色')}</th><th>{t('节点范围')}</th><th></th></tr></thead><tbody>
      {(list.data ?? []).map((a) => <tr key={a.id}><td>{a.username}{a.id === me.data?.id ? ' · ' + t('当前') : ''}</td><td>{roleLabels()[a.role]}</td><td>{(a.nodeIds ?? []).map((id) => nodes.data?.find((n) => n.id === id)?.name || id).join('、') || '—'}</td><td><div className="row">
        <button className="btn sm" type="button" disabled={busy} onClick={() => setForm({ id: a.id, username: a.username, password: '', role: a.role, nodeIds: a.nodeIds ?? [] })}>{t('修改权限')}</button>
        {a.id !== me.data?.id && <><button className="btn sm" type="button" disabled={busy} onClick={() => { setPassword(''); setReset(a) }}>{t('重置密码')}</button><button className="btn sm danger" type="button" disabled={busy} onClick={async () => { if (await confirm(t('删除后台账号 {name}？其所有登录将失效。', { name: a.username }), { danger: true })) run(async () => { await del('/admins/' + a.id); list.reload() }) }}>{t('删除')}</button></>}
      </div></td></tr>)}
    </tbody></table></div>
    {form && <Modal title={form.id ? t('修改权限') : t('添加后台账号')} onClose={() => { if (!busy) setForm(null) }} footer={<button className="btn pri" type="submit" form="admin-access" disabled={busy || (nodeRole && !form.nodeIds.length)}>{t('保存')}</button>}>
      <form id="admin-access" style={{ display: 'grid', gap: 12 }} onSubmit={(e) => { e.preventDefault(); if (busy) return; run(async () => {
        if (form.id) await put(`/admins/${form.id}/access`, { role: form.role, nodeIds: form.nodeIds })
        else await post('/admins', form)
        setForm(null); list.reload()
      }) }}>
        <Field label={t('用户名')}><input className="input" required pattern="[A-Za-z0-9_.-]{3,32}" disabled={!!form.id} value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></Field>
        {!form.id && <Field label={t('初始密码（至少 12 字节）')}><input className="input" type="password" required minLength={12} maxLength={72} autoComplete="new-password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} /></Field>}
        <Field label={t('角色')}><select className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value, nodeIds: [] })}>{Object.entries(roleLabels()).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></Field>
        <p className="hint">{roleDetails()[form.role]}</p>
        {nodeRole && <Field label={t('分配节点')}>
          <div style={{ display: 'grid', gap: 8, maxHeight: 240, overflowY: 'auto' }}>{(nodes.data ?? []).map((n) => <label className="chk" key={n.id}><input type="checkbox" checked={form.nodeIds.includes(n.id)} onChange={(e) => setForm({ ...form, nodeIds: e.target.checked ? [...form.nodeIds, n.id] : form.nodeIds.filter((id) => id !== n.id) })} />{n.name}</label>)}</div>
          {nodes.error && <div role="alert">{nodes.error}</div>}
        </Field>}
      </form>
    </Modal>}
    {reset && <Modal title={t('重置密码')} onClose={() => { if (!busy) { setReset(null); setPassword('') } }} footer={<button className="btn pri" type="submit" form="staff-reset" disabled={busy}>{t('确认')}</button>}>
      <form id="staff-reset" onSubmit={(e) => { e.preventDefault(); if (!busy) run(async () => { await post(`/admins/${reset.id}/password`, { password }); setReset(null); setPassword('') }) }}><Field label={t('初始密码（至少 12 字节）')}><input className="input" type="password" autoComplete="new-password" minLength={12} maxLength={72} required value={password} onChange={(e) => setPassword(e.target.value)} /></Field></form>
    </Modal>}
  </section>
}
