import { useState } from 'react'
import { del, get, post } from '../api'
import { useNodes } from '../App'
import { Empty, Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { date, daysLeft } from '../format'
import { t } from '../i18n'
import type { Certificate } from '../types'

const providers: Record<string, string> = { acme: 'ACME', selfsigned: t('自签'), manual: t('手动上传'), 'cf-origin': 'Cloudflare Origin' }

export default function Certificates() {
  const list = useLoad(() => get<Certificate[]>('/certificates'))
  const settings = useLoad(() => get<{ acme_ready: boolean; acme_http_ready: boolean }>('/settings'))
  const { nodes } = useNodes()
  const [form, setForm] = useState<{ domain: string; provider: string; cert: string; key: string; challenge: string; nodeId: string } | null>(null)
  const dnsReady = !!settings.data?.acme_ready
  const acmeReady = dnsReady || !!settings.data?.acme_http_ready
  const isIP = (d: string) => /^\d+\.\d+\.\d+\.\d+$/.test(d) || d.includes(':')
  const autoNode = (d: string) => nodes.find((n) => [n.domain, n.address, n.publicIpv4, n.publicIpv6].some((v) => v && v.toLowerCase() === d.toLowerCase()))
  const nodeName = (id?: string) => nodes.find((n) => n.id === id)?.name
  const { busy, run } = useAction()
  const confirm = useConfirm()
  return (
    <>
      <div className="top">
        <div><h1>{t('证书')}</h1><div className="sub">{t('ACME 证书由主控签发（HTTP 验证由节点应答，或 Cloudflare DNS 验证）并下发给节点；到期前自动续期')}</div></div>
        <button className="btn pri" type="button" onClick={() => setForm({ domain: '', provider: acmeReady ? 'acme' : 'selfsigned', cert: '', key: '', challenge: '', nodeId: '' })}>{t('+ 证书')}</button>
      </div>
      {!acmeReady && <div className="note warn">{t('尚未配置 ACME：在「设置」填写邮箱即可通过 HTTP 验证签发（域名需解析到节点、节点 80 端口空闲）；再填 Cloudflare Token 可签泛域名。未配置时 TLS 入站使用自签证书（客户端跳过校验）。')}</div>}
      <div className="card flush scroll">
        {list.data?.length === 0 ? <Empty>{t('还没有证书。创建 TLS 入站时会自动为 SNI 生成自签证书。')}</Empty> : (
          <table>
            <thead><tr><th>{t('域名')}</th><th>{t('来源')}</th><th>{t('到期')}</th><th>{t('自动续期')}</th><th>{t('使用节点')}</th><th></th></tr></thead>
            <tbody>
              {(list.data ?? []).map((c) => {
                const d = daysLeft(c.expireAt)
                return (
                  <tr key={c.id}>
                    <td className="mono" style={{ fontWeight: 500 }}>{c.domain}</td>
                    <td><span className="tag">{providers[c.provider] ?? c.provider}</span>
                      {c.provider === 'acme' && c.challenge && <span className="small mute">{c.challenge === 'http' ? `HTTP · ${nodeName(c.nodeId) ?? t('节点')}` : 'DNS'}</span>}</td>
                    <td className="mono" style={d !== null && d < 30 ? { color: 'var(--warn-ink)' } : undefined}>{date(c.expireAt)}{d !== null && ' · ' + t('{n} 天', { n: d })}</td>
                    <td>{c.autoRenew ? t('是') : t('否')}</td>
                    <td className="mono">{c.usedBy}</td>
                    <td style={{ textAlign: 'right' }}>
                      {c.lastError && <span className="small" style={{ color: 'var(--bad)', marginRight: 12 }} title={c.lastError}>{t('续期失败')}</span>}
                      {(c.provider === 'acme' || c.provider === 'selfsigned') && <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/certificates/${c.id}/renew`); list.reload() }, t('已续期并下发'))}>{t('续期')}</button>}
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(t('删除证书 {domain}？', { domain: c.domain }), { danger: true })) run(async () => { await del('/certificates/' + c.id); list.reload() })
                      }}>{t('删除')}</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
      {form && (
        <Modal title={t('添加证书')} onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" onClick={() => setForm(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !form.domain} onClick={() => run(async () => { await post('/certificates', form); setForm(null); list.reload() }, t('证书已添加'))}>{busy && form.provider === 'acme' ? t('签发中…') : t('确定')}</button>
        </>}>
          <Field label={t('来源')}>
            <div className="seg c3">
              {(['acme', 'selfsigned', 'manual'] as const).map((p) => (
                <button key={p} type="button" className={'opt' + (form.provider === p ? ' on' : '')} disabled={p === 'acme' && !acmeReady} onClick={() => setForm({ ...form, provider: p })}>{providers[p]}</button>
              ))}
            </div>
          </Field>
          <Field label={t('域名')} htmlFor="ct-domain"><input id="ct-domain" className="input mono" value={form.domain} onChange={(e) => setForm({ ...form, domain: e.target.value })} placeholder={t('jp1.example.com、*.example.com 或节点 IP')} /></Field>
          {form.provider === 'acme' && (() => {
            const wildcard = form.domain.startsWith('*.')
            const ip = isIP(form.domain)
            const method = form.challenge || (dnsReady && !ip ? 'dns' : 'http')
            const auto = autoNode(form.domain)
            return <>
              <Field label={t('验证方式')}>
                <div className="seg c3">
                  <button type="button" className={'opt' + (form.challenge === '' ? ' on' : '')} onClick={() => setForm({ ...form, challenge: '' })}>{t('自动')}<small>{dnsReady ? t('有 Token 用 DNS，否则 HTTP') : 'HTTP'}</small></button>
                  <button type="button" className={'opt' + (form.challenge === 'http' ? ' on' : '')} disabled={wildcard} onClick={() => setForm({ ...form, challenge: 'http' })}>HTTP<small>{t('节点 80 端口应答')}</small></button>
                  <button type="button" className={'opt' + (form.challenge === 'dns' ? ' on' : '')} disabled={!dnsReady || ip} onClick={() => setForm({ ...form, challenge: 'dns' })}>DNS<small>{dnsReady ? t('Cloudflare，可签泛域名') : t('需填 Cloudflare Token')}</small></button>
                </div>
              </Field>
              {method === 'http' && (
                <Field label={t('应答节点')} hint={auto ? t('按域名自动匹配到「{name}」', { name: auto.name }) : t('域名需解析到所选节点，且该节点 80 端口空闲（签发时临时监听几秒）')}>
                  <select className="input" value={form.nodeId} onChange={(e) => setForm({ ...form, nodeId: e.target.value })}>
                    <option value="">{auto ? t('自动（{name}）', { name: auto.name }) : t('选择节点')}</option>
                    {nodes.map((n) => <option key={n.id} value={n.id} disabled={n.status !== 'online'}>{n.name}{n.status !== 'online' ? t('（离线）') : ''}</option>)}
                  </select>
                </Field>
              )}
              {wildcard && method !== 'dns' && <div className="note warn">{t('泛域名证书只能用 DNS 验证，请在「设置」填写 Cloudflare Token。')}</div>}
              {ip && <div className="note">{t('IP 证书由 Let\'s Encrypt 按短期证书签发（约 6 天有效），面板会自动续期。')}</div>}
            </>
          })()}
          {form.provider === 'manual' && <>
            <Field label={t('证书 PEM（含中间证书）')}><textarea className="input" value={form.cert} onChange={(e) => setForm({ ...form, cert: e.target.value })} /></Field>
            <Field label={t('私钥 PEM')} hint={t('加密存储')}><textarea className="input" value={form.key} onChange={(e) => setForm({ ...form, key: e.target.value })} /></Field>
          </>}
        </Modal>
      )}
    </>
  )
}
