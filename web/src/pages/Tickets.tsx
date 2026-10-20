import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { Empty, Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { datetime } from '../format'
import { t } from '../i18n'

// Tickets: support requests from users (user center /u/tickets or the bot's
// /ticket). Admins read and answer them here; users are told on Telegram
// and by mail.

type Ticket = { id: string; no: number; userId: string; username?: string; subject: string; status: string; userUnread: boolean; adminUnread: boolean
  createdAt: string; updatedAt: string; closedAt?: string | null }
type Message = { id: string; admin: boolean; author: string; body: string; createdAt: string }

const statusLabel: Record<string, [string, string]> = {
  open: [t('待回复'), 'warn'], answered: [t('已回复'), 'ok'], closed: [t('已关闭'), 'off'],
}

function TicketSettings({ onSaved }: { onSaved: () => void }) {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  useEffect(() => {
    if (s.data) setF({ tickets_enabled: String(s.data.tickets_enabled ?? ''), ticket_max_open: String(s.data.ticket_max_open ?? ''), ticket_autoclose_days: String(s.data.ticket_autoclose_days ?? '') })
  }, [s.data])
  const { busy, run } = useAction()
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  return (
    <section className="card" style={{ display: 'grid', gap: 12 }}>
      <div className="toggle"><span>{t('开启工单：用户可在用户中心「🎫 工单」或机器人 /ticket 提交问题')}</span><Switch label={t('开启工单')} on={f.tickets_enabled === 'true'} onChange={(v) => set('tickets_enabled', String(v))} /></div>
      <div className="form-grid">
        <Field label={t('每人同时未关闭的工单')} hint={t('0 表示不限，默认 3')}><input className="input mono" inputMode="numeric" value={f.ticket_max_open ?? ''} placeholder="3" onChange={(e) => set('ticket_max_open', e.target.value.replace(/[^\d]/g, ''))} /></Field>
        <Field label={t('已回复后自动关闭（天）')} hint={t('回复后用户多少天没再回复就自动关闭；0 表示不自动关闭，默认 7')}><input className="input mono" inputMode="numeric" value={f.ticket_autoclose_days ?? ''} placeholder="7" onChange={(e) => set('ticket_autoclose_days', e.target.value.replace(/[^\d]/g, ''))} /></Field>
      </div>
      <div><button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', f); s.reload(); onSaved() }, t('已保存'))}>{t('保存')}</button></div>
    </section>
  )
}

function Thread({ id, onClose, onChanged }: { id: string; onClose: () => void; onChanged: () => void }) {
  const d = useLoad(() => get<{ ticket: Ticket; messages: Message[] }>('/tickets/' + id), [id])
  const [body, setBody] = useState('')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const tk = d.data?.ticket
  useEffect(() => { if (d.data) onChanged() }, [d.data])
  const reply = (close: boolean) => run(async () => {
    await post(`/tickets/${id}/reply`, { body, close }); setBody(''); d.reload(); onChanged()
  }, close ? t('已回复并关闭') : t('已回复'))
  return (
    <Modal title={tk ? `#${tk.no} ${tk.subject}` : '…'} onClose={onClose} footer={<>
      {tk && tk.status !== 'closed' && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await post(`/tickets/${id}/close`); d.reload(); onChanged() }, t('已关闭'))}>{t('关闭工单')}</button>}
      {tk && tk.status === 'closed' && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await post(`/tickets/${id}/reopen`); d.reload(); onChanged() }, t('已重新打开'))}>{t('重新打开')}</button>}
      <button className="btn danger" type="button" disabled={busy} onClick={async () => { if (await confirm(t('删除这个工单和全部消息？'), { danger: true, ok: t('删除') })) run(async () => { await del('/tickets/' + id); onChanged(); onClose() }, t('已删除')) }}>{t('删除')}</button>
    </>}>
      {tk && <div className="sub" style={{ margin: '0 0 10px' }}>{tk.username} · <span className={'st ' + statusLabel[tk.status]?.[1]}>{statusLabel[tk.status]?.[0]}</span> · {datetime(tk.createdAt)}</div>}
      <div style={{ display: 'grid', gap: 10, maxHeight: '50vh', overflowY: 'auto' }}>
        {(d.data?.messages ?? []).map((m) => (
          <div key={m.id} className="card" style={{ padding: 10, background: m.admin ? 'var(--accent-soft, var(--bg))' : undefined }}>
            <div className="small mute">{m.admin ? t('客服 {name}', { name: m.author }) : m.author} · {datetime(m.createdAt)}</div>
            <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{m.body}</div>
          </div>
        ))}
      </div>
      <div style={{ display: 'grid', gap: 8, marginTop: 12 }}>
        <textarea className="input" rows={4} value={body} placeholder={t('回复内容，用户会在 Telegram 和邮件里收到')} onChange={(e) => setBody(e.target.value)} />
        <div className="row">
          <button className="btn pri" type="button" disabled={busy || !body.trim()} onClick={() => reply(false)}>{t('回复')}</button>
          <button className="btn" type="button" disabled={busy || !body.trim()} onClick={() => reply(true)}>{t('回复并关闭')}</button>
        </div>
      </div>
    </Modal>
  )
}

export default function Tickets() {
  const [status, setStatus] = useState('open')
  const [q, setQ] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const [settings, setSettings] = useState(false)
  const list = useLoad(() => get<{ items: Ticket[]; counts: Record<string, number>; enabled: boolean }>(`/tickets?status=${status}&q=${encodeURIComponent(q)}`), [status, q])
  const items = list.data?.items ?? []
  const counts = list.data?.counts ?? {}
  return (
    <>
      <div className="top">
        <div><h1>{t('工单')}</h1><div className="sub">{t('用户在用户中心或机器人里提交的问题')}</div></div>
        <div className="row"><button className="btn" type="button" onClick={() => setSettings((v) => !v)}>{t('工单设置')}</button></div>
      </div>
      {list.data && !list.data.enabled && <div className="note warn" style={{ marginBottom: 12 }}>{t('工单未开启：在「工单设置」里打开后，用户中心和机器人才会出现工单入口。')}</div>}
      {settings && <div style={{ marginBottom: 12 }}><TicketSettings onSaved={list.reload} /></div>}
      <div className="row" style={{ marginBottom: 12 }}>
        <div className="chips">
          {([['open', t('待回复')], ['answered', t('已回复')], ['closed', t('已关闭')], ['', t('全部')]] as const).map(([k, l]) =>
            <button key={k} type="button" className={'chip' + (status === k ? ' on' : '')} onClick={() => setStatus(k)}>{l}{k && counts[k] != null && <span className="chip-n">{counts[k]}</span>}</button>)}
        </div>
        <input className="input search" style={{ width: 'auto' }} placeholder={t('搜索标题、用户名或 #编号')} value={q} onChange={(e) => setQ(e.target.value)} aria-label={t('搜索工单')} />
      </div>
      <div className="card flush scroll">
        {items.length === 0 ? <Empty>{t('没有工单')}</Empty> : (
          <table className="rtable">
            <thead><tr><th>{t('工单')}</th><th>{t('用户')}</th><th>{t('状态')}</th><th>{t('更新时间')}</th></tr></thead>
            <tbody>{items.map((x) => (
              <tr key={x.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(x.id)}>
                <td className="rt-head">{x.adminUnread && <span className="tag warn" style={{ marginRight: 6 }}>{t('新')}</span>}<span className="mono">#{x.no}</span> {x.subject}</td>
                <td data-label={t('用户')}>{x.username}</td>
                <td data-label={t('状态')}><span className={'st ' + statusLabel[x.status]?.[1]}>{statusLabel[x.status]?.[0]}</span></td>
                <td data-label={t('更新时间')}>{datetime(x.updatedAt)}</td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </div>
      {open && <Thread id={open} onClose={() => setOpen(null)} onChanged={list.reload} />}
    </>
  )
}
