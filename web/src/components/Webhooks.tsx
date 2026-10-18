import { useState } from 'react'
import { del, get, post, put } from '../api'
import { ago, datetime } from '../format'
import { t } from '../i18n'
import { copyText, Field, Modal, useAction, useConfirm, useLoad } from './ui'

type Hook = { id: string; name: string; url: string; format: string; events: string[]; enabled: boolean; pluginId?: string; lastStatus: string; lastAt?: string; hasSecret: boolean }
type Delivery = { id: string; type: string; status: string; attempts: number; lastCode: number; lastError: string; createdAt: string; nextAt: string; payload: string }
type Form = { id?: string; name: string; url: string; format: string; events: string[]; enabled: boolean; secret?: string; rotateSecret?: boolean }

const formatLabel = (): Record<string, string> => ({
  json: t('JSON（插件 / 自建服务，带签名）'), discord: 'Discord', slack: 'Slack', wecom: t('企业微信机器人'), dingtalk: t('钉钉机器人'), feishu: t('飞书机器人'),
})
const statusTag: Record<string, string> = { success: 'ok', failed: 'bad', pending: 'warn', sending: 'warn' }
const statusLabel = (): Record<string, string> => ({ success: t('成功'), failed: t('失败'), pending: t('等待重试'), sending: t('发送中') })
const groupLabel = (): Record<string, string> => ({ user: t('用户'), node: t('节点'), inbound: t('入站'), routing: t('路由'), admin: t('管理员'), system: t('系统') })

// Webhooks: push events to plugins, your own services or chat robots.
export default function Webhooks() {
  const list = useLoad(() => get<{ webhooks: Hook[]; events: string[]; formats: string[] }>('/webhooks'))
  const [form, setForm] = useState<Form | null>(null)
  const [secret, setSecret] = useState('')
  const [logs, setLogs] = useState<Hook | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const hooks = list.data?.webhooks ?? []
  const events = list.data?.events ?? []
  const groups = [...new Set(events.map((e) => e.split('.')[0]))]
  const fl = formatLabel()
  const save = () => run(async () => {
    const r = form!.id ? await put<{ secret: string }>('/webhooks/' + form!.id, form) : await post<{ secret: string }>('/webhooks', form)
    setForm(null); list.reload()
    if (r.secret) setSecret(r.secret)
  }, t('已保存'))
  const toggle = (ev: string) => setForm((f) => f && ({ ...f, events: f.events.includes(ev) ? f.events.filter((x) => x !== ev) : [...f.events, ev] }))
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>Webhook</span><button className="btn sm" type="button" onClick={() => setForm({ name: '', url: '', format: 'json', events: [], enabled: true })}>{t('+ 添加 Webhook')}</button></div>
      <div className="sub" style={{ margin: 0 }}>{t('把用户、节点和系统事件推送到外部：插件和自建服务用 JSON（带 HMAC 签名），也可以直接接 Discord、Slack、企业微信、钉钉、飞书机器人。发送失败会自动重试（1 分钟、5 分钟、30 分钟、2 小时、12 小时）。')}</div>
      {hooks.length === 0 && <div className="hint">{t('还没有 Webhook。')}</div>}
      {hooks.map((h) => (
        <div key={h.id} className="row" style={{ flexWrap: 'wrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
          <div style={{ flex: 1, minWidth: 200 }}>
            <div style={{ fontWeight: 500 }}>{h.name} <span className="tag">{fl[h.format] ?? h.format}</span>
              {!h.enabled && <span className="tag warn" style={{ marginLeft: 4 }}>{t('已停用')}</span>}
              {h.pluginId && <span className="tag" style={{ marginLeft: 4 }}>{t('插件')}</span>}
              {h.lastStatus && <span className={'tag ' + (statusTag[h.lastStatus] ?? '')} style={{ marginLeft: 4 }}>{statusLabel()[h.lastStatus] ?? h.lastStatus}{h.lastAt ? ' · ' + ago(h.lastAt) : ''}</span>}</div>
            <div className="hint mono" style={{ wordBreak: 'break-all' }}>{h.url}</div>
            <div className="hint">{h.events.length ? h.events.join(', ') : t('全部事件')}</div>
          </div>
          <div className="row" style={{ gap: 6 }}>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => {
              const d = await post<Delivery>(`/webhooks/${h.id}/test`)
              list.reload()
              if (d.status !== 'success') throw new Error(t('发送失败：{err}', { err: d.lastError || d.lastCode }))
            }, t('测试消息已送达'))}>{t('测试')}</button>
            <button className="btn sm" type="button" onClick={() => setLogs(h)}>{t('记录')}</button>
            <button className="btn sm" type="button" onClick={() => setForm({ id: h.id, name: h.name, url: h.url, format: h.format, events: h.events, enabled: h.enabled })}>{t('编辑')}</button>
            <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除 Webhook {name}？', { name: h.name }), { danger: true })) run(async () => { await del('/webhooks/' + h.id); list.reload() }) }}>{t('删除')}</button>
          </div>
        </div>
      ))}
      {form && (
        <Modal title={form.id ? t('编辑 Webhook') : t('添加 Webhook')} wide onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" onClick={() => setForm(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !form.url.trim()} onClick={save}>{t('保存')}</button>
        </>}>
          <div style={{ display: 'grid', gap: 12 }}>
            <div className="form-grid">
              <Field label={t('名称')}><input className="input" value={form.name} maxLength={64} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder={t('留空用域名')} /></Field>
              <Field label={t('格式')}>
                <select className="input" value={form.format} onChange={(e) => setForm({ ...form, format: e.target.value })}>
                  {(list.data?.formats ?? Object.keys(fl)).map((f) => <option key={f} value={f}>{fl[f] ?? f}</option>)}
                </select>
              </Field>
            </div>
            <Field label={t('地址')} hint={form.format === 'json' ? t('收到 POST 请求，请求头 X-Polaris-Signature 为 t=时间戳,v1=HMAC-SHA256(密钥, "时间戳.请求体")') : t('机器人的 Webhook 地址')}>
              <input className="input mono" value={form.url} onChange={(e) => setForm({ ...form, url: e.target.value.trim() })} placeholder="https://" />
            </Field>
            {(form.format === 'dingtalk' || form.format === 'feishu') && (
              <Field label={t('加签密钥（可选）')} hint={form.id ? t('留空不修改') : t('机器人安全设置里开启「加签」时填写')}>
                <input className="input mono" type="password" autoComplete="off" value={form.secret ?? ''} onChange={(e) => setForm({ ...form, secret: e.target.value })} />
              </Field>
            )}
            {form.id && form.format === 'json' && <label className="chk"><input type="checkbox" checked={!!form.rotateSecret} onChange={(e) => setForm({ ...form, rotateSecret: e.target.checked })} />{t('重新生成签名密钥（旧密钥立即失效）')}</label>}
            <Field label={t('事件')} hint={t('都不勾选表示接收全部事件，包括以后新增的')}>
              <div style={{ display: 'grid', gap: 10 }}>
                {groups.map((g) => (
                  <div key={g}>
                    <label className="chk" style={{ fontWeight: 500 }}><input type="checkbox" checked={form.events.includes(g + '.*')} onChange={() => toggle(g + '.*')} />{groupLabel()[g] ?? g}<span className="small mute">{t('（全部）')}</span></label>
                    {!form.events.includes(g + '.*') && <div className="chips" style={{ marginTop: 4 }}>
                      {events.filter((e) => e.startsWith(g + '.')).map((e) => <button key={e} type="button" className={'chip sm' + (form.events.includes(e) ? ' on' : '')} onClick={() => toggle(e)}>{e.slice(g.length + 1)}</button>)}
                    </div>}
                  </div>
                ))}
              </div>
            </Field>
            {form.id && <label className="chk"><input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />{t('启用')}</label>}
          </div>
        </Modal>
      )}
      {secret && (
        <Modal title={t('签名密钥')} onClose={() => setSecret('')} footer={<button className="btn pri" type="button" onClick={() => setSecret('')}>{t('我已保存')}</button>}>
          <div className="sub" style={{ margin: '0 0 10px' }}>{t('用它校验请求头 X-Polaris-Signature。密钥')}<b>{t('只显示这一次')}</b>{t('，丢失后可在编辑时重新生成。')}</div>
          <div className="row" style={{ flexWrap: 'nowrap' }}><code style={{ flex: 1, wordBreak: 'break-all' }}>{secret}</code><button className="btn sm" type="button" onClick={() => copyText(secret)}>{t('复制')}</button></div>
        </Modal>
      )}
      {logs && <DeliveryLog hook={logs} onClose={() => { setLogs(null); list.reload() }} />}
    </section>
  )
}

function DeliveryLog({ hook, onClose }: { hook: Hook; onClose: () => void }) {
  const ds = useLoad(() => get<Delivery[]>(`/webhooks/${hook.id}/deliveries`))
  const [open, setOpen] = useState('')
  const { busy, run } = useAction()
  const sl = statusLabel()
  return (
    <Modal title={t('发送记录：{name}', { name: hook.name })} wide onClose={onClose}>
      {(ds.data ?? []).length === 0 && <div className="hint">{t('最近 7 天没有发送记录。')}</div>}
      <div style={{ display: 'grid', gap: 6 }}>
        {(ds.data ?? []).map((d) => (
          <div key={d.id} style={{ borderTop: '1px solid var(--line)', paddingTop: 6 }}>
            <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
              <span className={'tag ' + (statusTag[d.status] ?? '')}>{sl[d.status] ?? d.status}</span>
              <span className="mono small">{d.type}</span>
              <span className="small mute">{datetime(d.createdAt)} · {t('第 {n} 次', { n: d.attempts })}{d.lastCode ? ' · HTTP ' + d.lastCode : ''}</span>
              <span className="spacer" />
              <button className="btn sm link" type="button" onClick={() => setOpen(open === d.id ? '' : d.id)}>{open === d.id ? t('收起') : t('内容')}</button>
              {d.status !== 'success' && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await post(`/webhook-deliveries/${d.id}/retry`); ds.reload() }, t('已重新发送'))}>{t('重发')}</button>}
            </div>
            {d.lastError && <div className="hint" style={{ color: 'var(--bad)', wordBreak: 'break-all' }}>{d.lastError}</div>}
            {open === d.id && <pre className="mono small" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', margin: '6px 0 0', maxHeight: 240, overflow: 'auto' }}>{(() => { try { return JSON.stringify(JSON.parse(d.payload), null, 2) } catch { return d.payload } })()}</pre>}
          </div>
        ))}
      </div>
    </Modal>
  )
}
