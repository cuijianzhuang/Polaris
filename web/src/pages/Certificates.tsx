import { useState } from 'react'
import { del, get, post } from '../api'
import { Empty, Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { date, daysLeft } from '../format'
import type { Certificate } from '../types'

const providers: Record<string, string> = { acme: 'ACME', selfsigned: '自签', manual: '手动上传', 'cf-origin': 'Cloudflare Origin' }

export default function Certificates() {
  const list = useLoad(() => get<Certificate[]>('/certificates'))
  const settings = useLoad(() => get<{ acme_ready: boolean }>('/settings'))
  const [form, setForm] = useState<{ domain: string; provider: string; cert: string; key: string } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  return (
    <>
      <div className="top">
        <div><h1>证书</h1><div className="sub">ACME 证书由主控签发（DNS-01）并下发给节点；到期前 30 天自动续期</div></div>
        <button className="btn pri" type="button" onClick={() => setForm({ domain: '', provider: settings.data?.acme_ready ? 'acme' : 'selfsigned', cert: '', key: '' })}>+ 证书</button>
      </div>
      {!settings.data?.acme_ready && <div className="note warn">尚未配置 ACME：在「设置」填写邮箱与 Cloudflare DNS Token 后可自动签发；否则 TLS 入站使用自签证书（客户端跳过校验）。</div>}
      <div className="card flush scroll">
        {list.data?.length === 0 ? <Empty>还没有证书。创建 TLS 入站时会自动为 SNI 生成自签证书。</Empty> : (
          <table>
            <thead><tr><th>域名</th><th>来源</th><th>到期</th><th>自动续期</th><th>使用节点</th><th></th></tr></thead>
            <tbody>
              {(list.data ?? []).map((c) => {
                const d = daysLeft(c.expireAt)
                return (
                  <tr key={c.id}>
                    <td className="mono" style={{ fontWeight: 500 }}>{c.domain}</td>
                    <td><span className="tag">{providers[c.provider] ?? c.provider}</span></td>
                    <td className="mono" style={d !== null && d < 30 ? { color: 'var(--warn-ink)' } : undefined}>{date(c.expireAt)}{d !== null && ` · ${d} 天`}</td>
                    <td>{c.autoRenew ? '是' : '否'}</td>
                    <td className="mono">{c.usedBy}</td>
                    <td style={{ textAlign: 'right' }}>
                      {c.lastError && <span className="small" style={{ color: 'var(--bad)', marginRight: 12 }} title={c.lastError}>续期失败</span>}
                      {(c.provider === 'acme' || c.provider === 'selfsigned') && <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/certificates/${c.id}/renew`); list.reload() }, '已续期并下发')}>续期</button>}
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(`删除证书 ${c.domain}？`, { danger: true })) run(async () => { await del('/certificates/' + c.id); list.reload() })
                      }}>删除</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
      {form && (
        <Modal title="添加证书" onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" onClick={() => setForm(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !form.domain} onClick={() => run(async () => { await post('/certificates', form); setForm(null); list.reload() }, '证书已添加')}>{busy && form.provider === 'acme' ? '签发中…' : '确定'}</button>
        </>}>
          <Field label="来源">
            <div className="seg c3">
              {(['acme', 'selfsigned', 'manual'] as const).map((p) => (
                <button key={p} type="button" className={'opt' + (form.provider === p ? ' on' : '')} disabled={p === 'acme' && !settings.data?.acme_ready} onClick={() => setForm({ ...form, provider: p })}>{providers[p]}</button>
              ))}
            </div>
          </Field>
          <Field label="域名" htmlFor="ct-domain"><input id="ct-domain" className="input mono" value={form.domain} onChange={(e) => setForm({ ...form, domain: e.target.value })} placeholder="jp1.example.com 或 *.example.com" /></Field>
          {form.provider === 'manual' && <>
            <Field label="证书 PEM（含中间证书）"><textarea className="input" value={form.cert} onChange={(e) => setForm({ ...form, cert: e.target.value })} /></Field>
            <Field label="私钥 PEM" hint="加密存储"><textarea className="input" value={form.key} onChange={(e) => setForm({ ...form, key: e.target.value })} /></Field>
          </>}
        </Modal>
      )}
    </>
  )
}
