import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { CopyBox, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { datetime } from '../format'
import type { TGAdmin } from '../types'

type Status = { running: boolean; username: string; error: string }
type BindReq = { role: 'owner' | 'operator'; name: string; nodeIds: string[] }

const roleLabel: Record<string, string> = { owner: '超级管理员', operator: '节点管理员' }

export default function Telegram() {
  const { nodes } = useNodes()
  const st = useLoad(() => get<Status>('/telegram'))
  const admins = useLoad(() => get<{ admins: TGAdmin[]; boundUsers: number }>('/telegram/admins'))
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState({ tg_enabled: 'false', tg_token: '', tg_api_base: '', tg_notify_users: 'true', remind_usage_pct: '80', remind_expire_days: '3', tg_daily_report: 'true', tg_daily_report_hour: '9' })
  const [bind, setBind] = useState<BindReq | null>(null)
  const [code, setCode] = useState<{ command: string; bot: string } | null>(null)
  const [edit, setEdit] = useState<TGAdmin | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  useEffect(() => {
    const d = settings.data
    if (d) setF({ tg_enabled: d.tg_enabled ?? 'false', tg_token: '', tg_api_base: d.tg_api_base ?? '', tg_notify_users: d.tg_notify_users ?? 'true',
      remind_usage_pct: d.remind_usage_pct || '80', remind_expire_days: d.remind_expire_days || '3', tg_daily_report: d.tg_daily_report || 'true', tg_daily_report_hour: d.tg_daily_report_hour || '9' })
  }, [settings.data])
  const nodeName = (id: string) => nodes.find((n) => n.id === id)?.name ?? id

  return (
    <>
      <div className="top">
        <div><h1>Telegram 机器人</h1><div className="sub">管理员远程查看节点、重启内核、管理用户；用户绑定订阅后自助查询流量与到期、获取订阅链接</div></div>
        <button className="btn pri" type="button" disabled={!st.data?.running} onClick={() => setBind({ role: 'operator', name: '', nodeIds: [] })}>+ 绑定管理员</button>
      </div>
      <div className="grid g2">
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="row" style={{ justifyContent: 'space-between' }}>
            <h2>机器人设置</h2>
            <span className={'st ' + (st.data?.running ? 'ok' : 'off')}>{st.data?.running ? `● 运行中 @${st.data.username}` : '未运行'}</span>
          </div>
          {st.data?.error && <div className="note bad">{st.data.error}</div>}
          <div className="toggle"><span>启用机器人</span><Switch label="启用" on={f.tg_enabled === 'true'} onChange={(v) => setF({ ...f, tg_enabled: String(v) })} /></div>
          <Field label="Bot Token" hint={settings.data?.tg_token_set ? '已保存（留空不修改）' : '在 @BotFather 创建机器人获取；加密存储'}>
            <input className="input mono" type="password" autoComplete="off" value={f.tg_token} onChange={(e) => setF({ ...f, tg_token: e.target.value })} placeholder="123456:ABC-DEF…" />
          </Field>
          <Field label="API 地址（可选）" hint="主控无法直连 api.telegram.org 时填反代地址">
            <input className="input mono" value={f.tg_api_base} onChange={(e) => setF({ ...f, tg_api_base: e.target.value })} placeholder="https://api.telegram.org" />
          </Field>
          <div className="toggle"><span>向绑定用户推送提醒与状态变化（流量将满、即将到期、已暂停 / 已恢复）</span><Switch label="用户提醒" on={f.tg_notify_users === 'true'} onChange={(v) => setF({ ...f, tg_notify_users: String(v) })} /></div>
          <div className="form-grid">
            <Field label="流量提醒阈值（%）" hint="用到这个比例时提醒一次；0 关闭"><input className="input mono" inputMode="numeric" value={f.remind_usage_pct} onChange={(e) => setF({ ...f, remind_usage_pct: e.target.value })} /></Field>
            <Field label="到期提前提醒（天）" hint="到期前这几天每天提醒一次；0 关闭"><input className="input mono" inputMode="numeric" value={f.remind_expire_days} onChange={(e) => setF({ ...f, remind_expire_days: e.target.value })} /></Field>
          </div>
          <div className="toggle"><span>每日报告：每天向超级管理员发送节点、流量与需要关注的用户（超额 / 到期 / 将满）</span><Switch label="每日报告" on={f.tg_daily_report === 'true'} onChange={(v) => setF({ ...f, tg_daily_report: String(v) })} /></div>
          {f.tg_daily_report === 'true' && <Field label="每日报告时间（点）" hint="服务器本地时间 0–23"><input className="input mono" inputMode="numeric" value={f.tg_daily_report_hour} onChange={(e) => setF({ ...f, tg_daily_report_hour: e.target.value })} /></Field>}
          <div className="row">
            <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
              await put('/settings', f); settings.reload(); setTimeout(st.reload, 3000)
            }, '已保存，机器人将在几秒内重连')}>保存</button>
            <button className="btn" type="button" disabled={busy || !st.data?.running} onClick={() => run(async () => { const r = await post<{ sent: number }>('/telegram/test'); return r }, '测试消息已发送给所有管理员')}>发送测试消息</button>
            <button className="btn" type="button" onClick={st.reload}>刷新状态</button>
          </div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <h2>角色与命令</h2>
          <dl className="kv" style={{ margin: 0, gridTemplateColumns: '110px minmax(0,1fr)' }}>
            <dt>网页登录</dt><dd>机器人运行时，登录页会出现「使用 Telegram 登录」：页面显示一个两位数，管理员在机器人消息里点同一个数字即可登录（3 分钟有效，点错或点「拒绝」即作废）；也可以在机器人里发 /login 获取一次性链接</dd>
            <dt>所有人</dt><dd>/menu 或 /start 打开按钮式主菜单（流量、订阅、节点、登录、告警开关一键直达，消息原地刷新）· /help 帮助</dd>
            <dt>超级管理员</dt><dd>/status 总览 · /nodes 节点列表（重启内核、强制同步）· /node 名称 · /users 关键词 搜索用户（重置流量、续期、启停）· /traffic 流量排行 · /login 获取网页面板登录链接（完整权限）；接收节点离线、下发失败、IP 变更等告警（/notify off 关闭）</dd>
            <dt>节点管理员</dt><dd>/status、/nodes、/node 只能看到并操作已绑定的节点，只接收这些节点的告警；/login 登录网页面板后也只能看到这些节点（查看、强制同步、回滚、重启内核）</dd>
            <dt>用户</dt><dd>/bind 订阅链接 绑定账号 · /me 流量与到期 · /sub 订阅链接与二维码 · /nodes 自己可用的节点 · /unbind 解绑；不能登录网页面板，也看不到其他用户和节点</dd>
          </dl>
          <div className="note">管理员绑定：点击「绑定管理员」生成一次性绑定码（10 分钟有效），在 Telegram 中向机器人发送 <code>/bind 绑定码</code>。已绑定订阅的用户：<b>{admins.data?.boundUsers ?? 0}</b> 人。</div>
        </section>
      </div>

      <section className="card flush scroll">
        <div style={{ padding: '14px 16px 0' }}><h2>管理员</h2></div>
        {admins.data?.admins.length === 0 ? <Empty>还没有绑定管理员。</Empty> : (
          <table>
            <thead><tr><th>名称</th><th>Chat ID</th><th>角色</th><th>可管理节点</th><th>告警</th><th>绑定时间</th><th></th></tr></thead>
            <tbody>
              {(admins.data?.admins ?? []).map((a) => (
                <tr key={a.id}>
                  <td style={{ fontWeight: 500 }}>{a.name || '—'}</td>
                  <td className="mono small">{a.chatId}</td>
                  <td><span className={'tag' + (a.role === 'owner' ? ' warn' : '')}>{roleLabel[a.role] ?? a.role}</span></td>
                  <td className="small">{a.role === 'owner' ? '全部' : (a.nodeIds ?? []).map(nodeName).join('、') || '无'}</td>
                  <td><Switch label="告警" on={a.notify} onChange={(v) => run(async () => { await put('/telegram/admins/' + a.id, { notify: v }); admins.reload() })} /></td>
                  <td className="small">{datetime(a.createdAt)}</td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="link" type="button" onClick={() => setEdit(a)}>编辑</button>
                    <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                      if (await confirm(`移除管理员 ${a.name || a.chatId}？`, { danger: true })) run(async () => { await del('/telegram/admins/' + a.id); admins.reload() })
                    }}>移除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <ReminderTemplates />

      {bind && !code && (
        <Modal title="绑定 Telegram 管理员" onClose={() => setBind(null)} footer={<>
          <button className="btn" type="button" onClick={() => setBind(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || (bind.role === 'operator' && !bind.nodeIds.length)} onClick={() => run(async () => {
            setCode(await post('/telegram/bind-code', bind))
          })}>生成绑定码</button>
        </>}>
          <RoleFields v={bind} onChange={setBind} nodes={nodes} />
        </Modal>
      )}
      {code && (
        <Modal title="绑定码" onClose={() => { setCode(null); setBind(null); admins.reload() }}>
          <div className="sub" style={{ margin: 0 }}>10 分钟内在 Telegram 中向机器人{code.bot && <> <a href={code.bot} target="_blank" rel="noreferrer">{code.bot.replace('https://t.me/', '@')}</a></>} 发送：</div>
          <CopyBox text={code.command} />
          <div className="note">绑定码只能使用一次。绑定成功后关闭此窗口即可在列表中看到。</div>
        </Modal>
      )}
      {edit && (
        <Modal title={`编辑管理员 · ${edit.name || edit.chatId}`} onClose={() => setEdit(null)} footer={<>
          <button className="btn" type="button" onClick={() => setEdit(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
            await put('/telegram/admins/' + edit.id, { name: edit.name, role: edit.role, nodeIds: edit.nodeIds ?? [] }); setEdit(null); admins.reload()
          }, '已保存')}>保存</button>
        </>}>
          <RoleFields v={{ role: edit.role, name: edit.name, nodeIds: edit.nodeIds ?? [] }} onChange={(v) => setEdit({ ...edit, ...v })} nodes={nodes} />
        </Modal>
      )}
    </>
  )
}

const tplKinds = [
  { k: 'traffic_warn', label: '流量提醒', ph: '{user}，本期流量已用 {percent}%（{used} / {total}），剩余 {left}。{reset}' },
  { k: 'expire_warn', label: '到期提醒', ph: '{user}，你的账号将于 {expire} 到期（还剩 {days} 天），续费请联系 {support}' },
  { k: 'user_limited', label: '流量用尽', ph: '{user}，本期流量已用完，服务已暂停。{reset}' },
  { k: 'user_expired', label: '已到期', ph: '{user}，你的账号已于 {expire} 到期，服务已暂停。' },
  { k: 'user_active', label: '已恢复', ph: '{user}，你的服务已恢复，祝使用愉快！' },
] as const
const tplVars = ['user', 'used', 'total', 'left', 'percent', 'expire', 'days', 'reset', 'site', 'sub_url', 'support']

// ReminderTemplates: custom wording for what users receive on Telegram.
function ReminderTemplates() {
  const settings = useLoad(() => get<Record<string, string>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [kind, setKind] = useState<string>('traffic_warn')
  const [preview, setPreview] = useState<{ text: string; user: string } | null>(null)
  const { busy, run } = useAction()
  useEffect(() => { if (settings.data) setF(Object.fromEntries(tplKinds.map((t) => [t.k, settings.data?.['tpl_' + t.k] ?? '']))) }, [settings.data])
  const cur = tplKinds.find((t) => t.k === kind)!
  const text = f[kind] ?? ''
  useEffect(() => {
    if (!text.trim()) { setPreview(null); return }
    const t = setTimeout(() => post<{ text: string; user: string }>('/telegram/template-preview', { text }).then(setPreview).catch(() => {}), 300)
    return () => clearTimeout(t)
  }, [text])
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <h2>提醒文案</h2>
      <div className="sub" style={{ margin: 0 }}>自定义发给用户的提醒与状态通知；留空使用内置文案。变量会按每个用户替换：{tplVars.map((v) => <code key={v} style={{ marginRight: 6 }}>{'{' + v + '}'}</code>)}</div>
      <div className="seg" style={{ gridTemplateColumns: `repeat(${tplKinds.length}, minmax(0, 1fr))` }}>
        {tplKinds.map((t) => <button key={t.k} type="button" className={'opt' + (kind === t.k ? ' on' : '')} onClick={() => setKind(t.k)}>{t.label}{f[t.k]?.trim() ? <small>已自定义</small> : <small>内置</small>}</button>)}
      </div>
      <textarea className="input" style={{ minHeight: 90 }} value={text} placeholder={cur.ph} onChange={(e) => setF({ ...f, [kind]: e.target.value })} />
      {preview && <div className="note" style={{ whiteSpace: 'pre-wrap' }}><span className="small mute">预览（{preview.user}）：</span>{'\n'}{preview.text}</div>}
      <div className="row">
        <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', Object.fromEntries(tplKinds.map((t) => ['tpl_' + t.k, f[t.k] ?? '']))); settings.reload() }, '提醒文案已保存')}>保存</button>
        {!text && <button className="btn" type="button" onClick={() => setF({ ...f, [kind]: cur.ph })}>以示例为起点</button>}
      </div>
    </section>
  )
}

function RoleFields({ v, onChange, nodes }: { v: BindReq; onChange: (v: BindReq) => void; nodes: { id: string; name: string }[] }) {
  return <>
    <Field label="名称"><input className="input" value={v.name} onChange={(e) => onChange({ ...v, name: e.target.value })} placeholder="运维小王" /></Field>
    <Field label="角色">
      <div className="seg">
        {(['owner', 'operator'] as const).map((r) => (
          <button key={r} type="button" className={'opt' + (v.role === r ? ' on' : '')} onClick={() => onChange({ ...v, role: r })}>
            {roleLabel[r]}<small>{r === 'owner' ? '全部节点与用户' : '只管理指定节点'}</small>
          </button>
        ))}
      </div>
    </Field>
    {v.role === 'operator' && (
      <Field label="可管理节点">
        <div className="row">
          {nodes.map((n) => {
            const on = v.nodeIds.includes(n.id)
            return <button key={n.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
              onClick={() => onChange({ ...v, nodeIds: on ? v.nodeIds.filter((x) => x !== n.id) : [...v.nodeIds, n.id] })}>{n.name}</button>
          })}
        </div>
      </Field>
    )}
  </>
}
