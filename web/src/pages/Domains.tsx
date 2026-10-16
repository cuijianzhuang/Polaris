import { useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Empty, ErrorNote, Field, Modal, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, datetime } from '../format'
import type { DNSProvider, DomainBinding } from '../types'

type ProvForm = { id?: string; name: string; kind: 'cloudflare' | 'webhook'; token: string; endpoint: string; hasToken?: boolean }
type BindForm = { id?: string; nodeId: string; providerId: string; domain: string; family: string; proxied: boolean; ttl: number; setNodeDomain: boolean }

const famLabel: Record<string, string> = { ipv4: 'IPv4 (A)', ipv6: 'IPv6 (AAAA)', dual: '双栈' }
const statusTag: Record<string, [string, string]> = { synced: ['已同步', 'tag ok'], pending: ['待同步', 'tag warn'], error: ['失败', 'tag bad'] }

export default function Domains() {
  const { nodes, refresh } = useNodes()
  const provs = useLoad(() => get<DNSProvider[]>('/dns-providers'))
  const binds = useLoad(() => get<DomainBinding[]>('/domains'))
  const [pf, setPf] = useState<ProvForm | null>(null)
  const [bf, setBf] = useState<BindForm | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const [auto, setAuto] = useState<{ provider: string; zone: string; family: string; address: boolean } | null>(null)
  const a = auto ?? {
    provider: settings.data?.ddns_auto_provider ?? '', zone: settings.data?.ddns_auto_zone ?? '',
    family: settings.data?.ddns_auto_family || 'ipv4', address: (settings.data?.ddns_auto_address ?? 'true') !== 'false',
  }
  const saveAuto = () => run(async () => {
    await put('/settings', { ddns_auto_provider: a.provider, ddns_auto_zone: a.zone.trim(), ddns_auto_family: a.family, ddns_auto_address: String(a.address) })
    setAuto(null); settings.reload()
  }, a.provider && a.zone.trim() ? '已开启：新节点接入时自动绑定域名' : '已关闭自动绑定')
  const unbound = nodes.filter((n) => !binds.data?.some((b) => b.nodeId === n.id))
  const bindAll = () => run(async () => {
    const r = await post<{ nodeId: string; domain: string; error?: string }[]>('/domains/auto-bind', {})
    binds.reload(); refresh()
    const failed = r.filter((x) => x.error)
    if (failed.length) toast(`已绑定 ${r.length - failed.length} 个，${failed.length} 个写入 DNS 失败：${failed[0].error}`, true)
    else toast(r.length ? `已为 ${r.length} 个节点绑定域名` : '所有节点都已有域名')
  })
  const [syncing, setSyncing] = useState(false)
  const syncAll = () => run(async () => {
    setSyncing(true)
    const r = await post<{ domain: string; status: string; error?: string }[]>('/domains/sync-all', {})
    binds.reload()
    const failed = r.filter((x) => x.status === 'error')
    const pending = r.filter((x) => x.status === 'pending')
    if (failed.length) toast(`${r.length - failed.length - pending.length} 条已同步，${failed.length} 条失败：${failed[0].domain} — ${failed[0].error}`, true)
    else if (pending.length) toast(`${r.length - pending.length} 条已同步，${pending.length} 条待同步（节点暂无公网 IP）`)
    else toast(`已同步全部 ${r.length} 条记录`)
  }).finally(() => setSyncing(false))
  const nodeName = (id: string) => nodes.find((n) => n.id === id)?.name ?? id
  const provName = (id: string) => provs.data?.find((p) => p.id === id)?.name ?? '—'

  const saveBinding = (f: BindForm) => run(async () => {
    const r = await (f.id ? put('/domains/' + f.id, f) : post('/domains', f))
    setBf(null); binds.reload(); refresh()
    if (r?.warning) toast('已保存，但 DNS 写入失败：' + r.warning, true)
    else toast('已绑定并写入 DNS')
  })

  return (
    <>
      <div className="top">
        <div><h1>域名与 DDNS</h1><div className="sub">把域名解析到节点的公网 IP；Agent 上报的出口 IP 变化后自动更新记录（每 10 分钟兜底同步一次）</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => setPf({ name: '', kind: 'cloudflare', token: '', endpoint: '' })}>+ DNS 服务商</button>
          <button className="btn pri" type="button" disabled={!provs.data?.length} onClick={() => setBf({ nodeId: nodes[0]?.id ?? '', providerId: provs.data?.[0]?.id ?? '', domain: '', family: 'ipv4', proxied: false, ttl: 60, setNodeDomain: false })}>+ 域名绑定</button>
        </div>
      </div>
      <ErrorNote error={provs.error || binds.error} />

      <section className="card flush scroll">
        <div style={{ padding: '14px 16px 0' }}><h2>DNS 服务商</h2></div>
        {provs.data?.length === 0 ? <Empty>先添加一个 DNS 服务商：Cloudflare（API Token，Zone.DNS 编辑权限）或自定义 Webhook。</Empty> : (
          <table>
            <thead><tr><th>名称</th><th>类型</th><th>凭据 / 地址</th><th></th></tr></thead>
            <tbody>
              {(provs.data ?? []).map((p) => (
                <tr key={p.id}>
                  <td style={{ fontWeight: 500 }}>{p.name}</td>
                  <td><span className="tag">{p.kind === 'cloudflare' ? 'Cloudflare' : 'Webhook'}</span></td>
                  <td className="mono small">{p.kind === 'webhook' ? p.endpoint : p.hasToken ? 'Token 已保存' : '未设置 Token'}</td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="link" type="button" onClick={() => setPf({ id: p.id, name: p.name, kind: p.kind, token: '', endpoint: p.endpoint, hasToken: p.hasToken })}>编辑</button>
                    <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                      if (await confirm(`删除 DNS 服务商 ${p.name}？`, { danger: true })) run(async () => { await del('/dns-providers/' + p.id); provs.reload() })
                    }}>删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h2>自动绑定</h2>
        <div className="sub" style={{ marginTop: 0 }}>新节点接入时自动绑定「节点名.根域名」（如 bwg-hk.node.example.com），IP 变化后自动更新。节点名里的中文会被去掉，重名自动加序号。</div>
        <div className="grid g2" style={{ marginTop: 12 }}>
          <Field label="DNS 服务商">
            <select className="input" value={a.provider} onChange={(e) => setAuto({ ...a, provider: e.target.value })}>
              <option value="">不自动绑定</option>
              {(provs.data ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </Field>
          <Field label="根域名" hint="需在该服务商下（Cloudflare 里有这个域名的 Zone）">
            <input className="input mono" value={a.zone} placeholder="node.example.com" onChange={(e) => setAuto({ ...a, zone: e.target.value })} />
          </Field>
          <Field label="记录类型">
            <select className="input" value={a.family} onChange={(e) => setAuto({ ...a, family: e.target.value })}>
              {Object.entries(famLabel).map(([k, l]) => <option key={k} value={k}>{l}</option>)}
            </select>
          </Field>
          <label className="row small" style={{ alignSelf: 'end', paddingBottom: 10 }}>
            <Switch label="设为节点域名" on={a.address} onChange={(v) => setAuto({ ...a, address: v })} />设为节点域名：订阅优先用域名（换 IP 后客户端无需更新订阅）
          </label>
        </div>
        <div className="row" style={{ marginTop: 8 }}>
          <button className="btn pri" type="button" disabled={busy || !auto} onClick={saveAuto}>保存</button>
          <button className="btn" type="button" disabled={busy || !!auto || !settings.data?.ddns_auto_provider || !settings.data?.ddns_auto_zone || unbound.length === 0} onClick={bindAll}
            title={auto ? '先保存设置' : undefined}>为未绑定的节点补绑{unbound.length ? `（${unbound.length}）` : ''}</button>
        </div>
      </section>

      <section className="card flush scroll">
        <div className="row" style={{ padding: '14px 16px 0', justifyContent: 'space-between' }}>
          <h2 style={{ margin: 0 }}>域名绑定</h2>
          {(binds.data?.length ?? 0) > 0 && <button className="btn sm" type="button" disabled={busy} onClick={syncAll}>{syncing ? <><span className="spinner" />同步中…</> : '全部立即同步'}</button>}
        </div>
        {binds.data?.length === 0 ? <Empty>还没有域名绑定。绑定后节点的公网 IP 会自动写入 DNS 记录。</Empty> : (
          <table>
            <thead><tr><th>域名</th><th>节点</th><th>服务商</th><th>记录</th><th>当前值</th><th>状态</th><th>同步时间</th><th></th></tr></thead>
            <tbody>
              {(binds.data ?? []).map((b) => {
                const n = nodes.find((x) => x.id === b.nodeId)
                const [sl, sc] = statusTag[b.status] ?? [b.status, 'tag']
                return (
                  <tr key={b.id}>
                    <td className="mono" style={{ fontWeight: 500 }}>{b.domain}{b.proxied && <span className="tag warn" style={{ marginLeft: 6 }}>CDN 代理</span>}</td>
                    <td>{nodeName(b.nodeId)}{n && !n.ddns && /^[\d.]+$|:/.test(n.address) && <div className="small mute" title="节点地址是固定 IP，记录会一直写这个 IP；开启 DDNS 或把地址改成域名后才跟随出口 IP">节点地址为固定 IP</div>}</td>
                    <td>{provName(b.providerId)}</td>
                    <td className="small">{famLabel[b.family] ?? b.family} · TTL {b.ttl || 'auto'}</td>
                    <td className="mono small">{b.current || '—'}</td>
                    <td><span className={sc} title={b.lastError}>{sl}</span>{b.lastError && <div className="small" style={{ color: 'var(--bad)', maxWidth: 220 }}>{b.lastError}</div>}</td>
                    <td className="small" title={datetime(b.syncedAt)}>{ago(b.syncedAt)}</td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/domains/${b.id}/sync`); binds.reload() }, '已同步')}>立即同步</button>
                      <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setBf({ id: b.id, nodeId: b.nodeId, providerId: b.providerId, domain: b.domain, family: b.family, proxied: b.proxied, ttl: b.ttl, setNodeDomain: false })}>编辑</button>
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (!(await confirm(`解除绑定 ${b.domain}？`, { danger: true, ok: '解除' }))) return
                        const purge = await confirm(`同时删除 DNS 服务商上 ${b.domain} 的 A / AAAA 记录？`, { ok: '删除记录' })
                        run(async () => { await del(`/domains/${b.id}${purge ? '?purge=1' : ''}`); binds.reload() }, '已解除')
                      }}>解除</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h2>节点公网 IP</h2>
        <div className="sub" style={{ marginTop: 4 }}>由 Agent 定期探测出口地址并随心跳上报；开启 DDNS 的节点 IP 变化后立即更新其绑定域名并通知 Telegram 管理员。</div>
        <table style={{ marginTop: 10 }}>
          <thead><tr><th>节点</th><th>DDNS</th><th>IPv4</th><th>IPv6</th><th>最近变化</th></tr></thead>
          <tbody>
            {nodes.map((n) => (
              <tr key={n.id}>
                <td style={{ fontWeight: 500 }}>{n.name}</td>
                <td><Switch label={`${n.name} DDNS`} on={n.ddns} onChange={(v) => run(async () => { await put(`/nodes/${n.id}`, { name: n.name, address: n.address, domain: n.domain, region: n.region, flag: n.flag, labels: n.labels, defaultCore: n.defaultCore, trafficRate: n.trafficRate, sortOrder: n.sortOrder, noFirewall: n.noFirewall, ddns: v }); refresh() })} /></td>
                <td className="mono small">{n.publicIpv4 || '—'}</td>
                <td className="mono small">{n.publicIpv6 || '—'}</td>
                <td className="small">{n.ipChangedAt ? ago(n.ipChangedAt) : '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      {pf && (
        <Modal title={pf.id ? '编辑 DNS 服务商' : '添加 DNS 服务商'} onClose={() => setPf(null)} footer={<>
          <button className="btn" type="button" onClick={() => setPf(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !pf.name} onClick={() => run(async () => {
            await (pf.id ? put('/dns-providers/' + pf.id, pf) : post('/dns-providers', pf)); setPf(null); provs.reload()
          }, '已保存')}>保存</button>
        </>}>
          <Field label="名称"><input className="input" value={pf.name} onChange={(e) => setPf({ ...pf, name: e.target.value })} placeholder="cf-main" /></Field>
          <Field label="类型">
            <div className="seg">
              {(['cloudflare', 'webhook'] as const).map((k) => (
                <button key={k} type="button" className={'opt' + (pf.kind === k ? ' on' : '')} onClick={() => setPf({ ...pf, kind: k })}>
                  {k === 'cloudflare' ? 'Cloudflare' : 'Webhook'}<small>{k === 'cloudflare' ? 'API Token 直接写 A/AAAA 记录' : 'POST JSON 到自定义地址（适配其他 DNS）'}</small>
                </button>
              ))}
            </div>
          </Field>
          {pf.kind === 'cloudflare' ? (
            <Field label="API Token" hint={pf.hasToken ? '已保存（留空不修改）' : '需要 Zone.Zone 读取 + Zone.DNS 编辑权限；加密存储'}>
              <input className="input mono" type="password" autoComplete="off" value={pf.token} onChange={(e) => setPf({ ...pf, token: e.target.value })} />
            </Field>
          ) : <>
            <Field label="Webhook 地址" hint={'POST JSON：{"action":"set|delete","domain","type":"A|AAAA","content","ttl","proxied"}'}>
              <input className="input mono" value={pf.endpoint} onChange={(e) => setPf({ ...pf, endpoint: e.target.value })} placeholder="https://dns.example.com/hook" />
            </Field>
            <Field label="Bearer Token（可选）" hint={pf.hasToken ? '已保存（留空不修改）' : '作为 Authorization: Bearer 头发送'}>
              <input className="input mono" type="password" autoComplete="off" value={pf.token} onChange={(e) => setPf({ ...pf, token: e.target.value })} />
            </Field>
          </>}
        </Modal>
      )}

      {bf && (
        <Modal title={bf.id ? '编辑域名绑定' : '域名绑定节点 IP'} onClose={() => setBf(null)} footer={<>
          <button className="btn" type="button" onClick={() => setBf(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !bf.domain || !bf.nodeId || !bf.providerId} onClick={() => saveBinding(bf)}>{busy ? '写入中…' : '保存并同步'}</button>
        </>}>
          <Field label="域名"><input className="input mono" value={bf.domain} onChange={(e) => setBf({ ...bf, domain: e.target.value })} placeholder="jp1.example.com" /></Field>
          <div className="form-grid">
            <Field label="节点">
              <select className="input" value={bf.nodeId} onChange={(e) => setBf({ ...bf, nodeId: e.target.value })}>
                {nodes.map((n) => <option key={n.id} value={n.id}>{n.name} {n.publicIpv4 ? `· ${n.publicIpv4}` : ''}</option>)}
              </select>
            </Field>
            <Field label="DNS 服务商">
              <select className="input" value={bf.providerId} onChange={(e) => setBf({ ...bf, providerId: e.target.value })}>
                {(provs.data ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
              </select>
            </Field>
            <Field label="记录类型">
              <select className="input" value={bf.family} onChange={(e) => setBf({ ...bf, family: e.target.value })}>
                {Object.entries(famLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
              </select>
            </Field>
            <Field label="TTL（秒）" hint="1 或 0 为自动"><input className="input mono" inputMode="numeric" value={bf.ttl} onChange={(e) => setBf({ ...bf, ttl: Number(e.target.value) || 0 })} /></Field>
          </div>
          <label className="row small"><Switch label="CDN 代理" on={bf.proxied} onChange={(v) => setBf({ ...bf, proxied: v })} />开启 Cloudflare 代理（橙色云，直连协议请勿开启）</label>
          {!bf.id && <label className="row small"><Switch label="设为节点域名" on={bf.setNodeDomain} onChange={(v) => setBf({ ...bf, setNodeDomain: v })} />同时设为该节点的域名（用于证书与 {'{node.domain}'} 入口）</label>}
        </Modal>
      )}
    </>
  )
}
