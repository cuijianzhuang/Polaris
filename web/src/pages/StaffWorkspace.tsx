import { lazy, Suspense, useState } from 'react'
import { get, post, setToken } from '../api'
import { CopyBox, Field, QR, useAction, useLoad, usePoll } from '../components/ui'
import { Sessions, RecoveryCodesModal } from '../components/SecurityCards'
import Passkeys from '../components/Passkeys'
import { roleDetails, roleLabels } from '../components/AdminAccounts'
const Tickets = lazy(() => import('./Tickets'))
import { bytes, datetime } from '../format'
import { t } from '../i18n'

export function StaffAccount() {
  const me = useLoad(() => get<{ totpEnabled: boolean }>('/auth/me'))
  const [pw, setPw] = useState({ old: '', next: '', again: '' })
  const [totp, setTotp] = useState<{ secret: string; url: string } | null>(null)
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const { busy, run } = useAction()
  return <div style={{ display: 'grid', gap: 16 }}>
    <section className="card"><h2>{t('修改密码')}</h2><form style={{ display: 'grid', gap: 12 }} onSubmit={(e) => { e.preventDefault(); if (!busy && pw.next === pw.again) run(async () => { const r = await post<{ token: string }>('/auth/password', { old: pw.old, new: pw.next }); setToken(r.token); setPw({ old: '', next: '', again: '' }) }, t('密码已修改')) }}>
      <Field label={t('原密码')}><input className="input" type="password" autoComplete="current-password" required value={pw.old} onChange={(e) => setPw({ ...pw, old: e.target.value })} /></Field>
      <Field label={t('新密码')}><input className="input" type="password" autoComplete="new-password" required minLength={12} maxLength={72} value={pw.next} onChange={(e) => setPw({ ...pw, next: e.target.value })} /></Field>
      <Field label={t('确认新密码')}><input className="input" type="password" autoComplete="new-password" required value={pw.again} onChange={(e) => setPw({ ...pw, again: e.target.value })} /></Field>
      <button className="btn pri" disabled={busy || pw.next !== pw.again}>{t('修改密码')}</button>
    </form></section>
    <section className="card"><h2>{t('二次验证（TOTP）')}</h2>
      <div className="hint">{me.data?.totpEnabled ? t('已开启') : t('未开启')}</div>
      <div className="row"><button className="btn" disabled={busy} onClick={() => run(async () => setTotp(await post('/auth/totp/setup')))}>{t('开始绑定')}</button>
      {me.data?.totpEnabled && <button className="btn" disabled={busy} onClick={() => run(async () => setCodes((await post<{ recoveryCodes: string[] }>('/auth/totp/recovery-codes')).recoveryCodes))}>{t('重新生成恢复码')}</button>}</div>
      {totp && <div style={{ display: 'grid', gap: 10, marginTop: 12 }}><QR text={totp.url} size={180} /><CopyBox text={totp.secret} /><Field label={t('验证码')}><input className="input" inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} /></Field><button className="btn" disabled={busy || code.length < 6} onClick={() => run(async () => { const r = await post<{ recoveryCodes: string[] }>('/auth/totp/enable', { enable: true, code }); setTotp(null); setCode(''); setCodes(r.recoveryCodes); me.reload() })}>{t('开启')}</button></div>}
      {codes && <RecoveryCodesModal codes={codes} onClose={() => setCodes(null)} />}
    </section>
    <Passkeys /><Sessions />
  </div>
}

type StaffUser = { id: string; username: string; status: string; trafficLimit: number; usedUp: number; usedDown: number; expireAt?: string }
function CustomerSummary() {
  const [q, setQ] = useState(''), [search, setSearch] = useState(''), [offset, setOffset] = useState(0)
  const data = useLoad(() => get<StaffUser[]>(`/staff/users?q=${encodeURIComponent(search)}&offset=${offset}`), [search, offset])
  return <section className="card"><h2>{t('用户摘要')}</h2>
    <form className="row" onSubmit={(e) => { e.preventDefault(); setSearch(q); setOffset(0) }}><input className="input" aria-label={t('搜索用户名')} value={q} onChange={(e) => setQ(e.target.value)} /><button className="btn">{t('搜索')}</button></form>
    {data.error && <div className="note bad" role="alert">{data.error}</div>}
    <div className="scroll"><table><thead><tr><th>{t('用户名')}</th><th>{t('状态')}</th><th>{t('已用流量')}</th><th>{t('流量配额')}</th><th>{t('到期时间')}</th></tr></thead><tbody>{(data.data ?? []).map((u) => <tr key={u.id}><td>{u.username}</td><td>{u.status}</td><td>{bytes(u.usedUp + u.usedDown)}</td><td>{u.trafficLimit ? bytes(u.trafficLimit) : '∞'}</td><td>{datetime(u.expireAt)}</td></tr>)}</tbody></table></div>
    <div className="row"><button className="btn" disabled={!offset} onClick={() => setOffset(offset - 100)}>{t('上一页')}</button><button className="btn" disabled={data.loading || (data.data?.length ?? 0) < 100} onClick={() => setOffset(offset + 100)}>{t('下一页')}</button></div>
  </section>
}

function ViewerSummary() {
  const data = useLoad(() => get<{ id: string; name: string; status: string; cpu: number; mem: number; lastSeen?: string }[]>('/staff/nodes'))
  usePoll(data.reload, 2000)
  return <section className="card"><h2>{t('我的节点')}</h2>{data.error && <div className="note bad" role="alert">{data.error}</div>}<div className="scroll"><table><thead><tr><th>{t('节点')}</th><th>{t('状态')}</th><th>CPU / RAM</th><th>{t('最后在线')}</th></tr></thead><tbody>{(data.data ?? []).map((n) => <tr key={n.id}><td>{n.name}</td><td>{n.status}</td><td>{n.cpu}% / {n.mem}%</td><td>{datetime(n.lastSeen)}</td></tr>)}</tbody></table></div></section>
}

function FinanceSummary() {
  const [offset, setOffset] = useState(0)
  const orders = useLoad(() => get<{ id: string; planName: string; amount: string; currency: string; status: string; method: string; createdAt: string }[]>(`/staff/orders?offset=${offset}`), [offset])
  const costs = useLoad(() => get<{ nodeId: string; name: string; amount: number; currency: string; cycle: string; expireAt?: string }[]>('/staff/costs'))
  return <>
    <section className="card"><h2>{t('订单摘要')}</h2>{orders.error && <div className="note bad" role="alert">{orders.error}</div>}<div className="scroll"><table><thead><tr><th>{t('订单')}</th><th>{t('套餐')}</th><th>{t('金额')}</th><th>{t('状态')}</th><th>{t('时间')}</th></tr></thead><tbody>{(orders.data ?? []).map((o) => <tr key={o.id}><td className="mono">{o.id}</td><td>{o.planName}</td><td>{o.amount} {o.currency}</td><td>{o.status} · {o.method}</td><td>{datetime(o.createdAt)}</td></tr>)}</tbody></table></div><div className="row"><button className="btn" disabled={!offset} onClick={() => setOffset(offset - 100)}>{t('上一页')}</button><button className="btn" disabled={orders.loading || (orders.data?.length ?? 0) < 100} onClick={() => setOffset(offset + 100)}>{t('下一页')}</button></div></section>
    <section className="card"><h2>{t('服务器成本')}</h2>{costs.error && <div className="note bad" role="alert">{costs.error}</div>}<div className="scroll"><table><thead><tr><th>{t('节点')}</th><th>{t('金额')}</th><th>{t('周期')}</th><th>{t('到期时间')}</th></tr></thead><tbody>{(costs.data ?? []).map((c) => <tr key={c.nodeId}><td>{c.name}</td><td>{c.amount} {c.currency}</td><td>{c.cycle}</td><td>{datetime(c.expireAt)}</td></tr>)}</tbody></table></div></section>
  </>
}

export default function StaffWorkspace({ role, username, onLogout }: { role: string; username: string; onLogout: () => void }) {
  const [account, setAccount] = useState(false)
  return <main style={{ maxWidth: 1200, margin: 'auto', padding: 20, display: 'grid', gap: 16 }}>
    <header className="row" style={{ justifyContent: 'space-between', flexWrap: 'wrap' }}><div><h1>{roleLabels()[role]}</h1><div className="hint">{username} · {roleDetails()[role]}</div></div><div className="row"><button className="btn" onClick={() => setAccount(!account)}>{account ? t('返回工作台') : t('我的账号')}</button><button className="btn" onClick={onLogout}>{t('退出登录')}</button></div></header>
    {account ? <StaffAccount /> : role === 'viewer' ? <ViewerSummary /> : role === 'support' ? <><CustomerSummary /><Suspense fallback={t('加载中…')}><Tickets staff /></Suspense></> : <FinanceSummary />}
  </main>
}
