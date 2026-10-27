import { useMemo, useState } from 'react'
import { api, del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Drawer, Empty, Field, Modal, Switch, fromLines, toLines, useAction, useConfirm, useLoad } from '../components/ui'
import { ago, coreShort, inboundSummary, protoLabel } from '../format'
import { t } from '../i18n'
import GeoPicker from '../components/GeoPicker'
import WarpDialog from '../components/WarpDialog'
import type { CapSet, CDNProfile, Chain, ChainHop, Inbound, Node, Outbound, RouteMatch, RoutingRule, RulePreset, RuleSet, SourceNode } from '../types'
import { DNSSettings, RuleSets } from './DNSRules'

type Tab = 'chains' | 'cdn' | 'routing' | 'rulesets' | 'dns'

function hopInbound(ins: Inbound[], id?: string) { return ins.find((i) => i.id === id) }

function ChainCard({ c, nodes, inbounds, onEdit, onChanged }: { c: Chain; nodes: Node[]; inbounds: Inbound[]; onEdit: () => void; onChanged: () => void }) {
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id
  const landing = c.hops[c.hops.length - 1]
  const landIn = hopInbound(inbounds, landing?.inboundId)
  const firstIn = c.mode === 'proxy' ? hopInbound(inbounds, c.hops[0]?.inboundId) : landIn
  const probe = (i: number) => c.probe?.find((p) => p.nodeId === c.hops[i]?.nodeId)
  const allOk = c.probe?.length && c.probe.every((p) => p.ok)
  const anyBad = c.probe?.some((p) => !p.ok)
  return (
    <article className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <div>
          <h2>{c.mode === 'l4' ? t('{from} ⇢ {to}（L4 转发）', { from: name(c.hops[0].nodeId), to: name(landing.nodeId) }) : c.hops.map((h) => name(h.nodeId)).join(' → ')}</h2>
          <div className="sub">{c.mode === 'l4' ? t('端口转发 · {from}:{port} → {to}:{toPort} · 不解密', { from: name(c.hops[0].nodeId), port: c.hops[0].port, to: name(landing.nodeId), toPort: landIn?.port ?? '?' }) : t('协议中转 · 服务账号 svc-{id} · 订阅名「{from} → {to}」', { id: c.id.slice(0, 8), from: name(c.hops[0].nodeId), to: name(landing.nodeId) })}{c.remark ? ' · ' + c.remark : ''}</div>
        </div>
        <span className={'st ' + (allOk ? 'ok' : anyBad ? 'warn' : 'off')}>{allOk ? t('● 全链路正常') : anyBad ? t('● 部分跳异常') : t('未探测')}</span>
      </div>
      <div className="path">
        <div className="hop"><b>{t('客户端')}</b><span>{firstIn ? `${protoLabel[firstIn.protocol.type]}-${firstIn.transport.type}` : '—'}</span></div>
        {c.hops.map((h, i) => {
          const ib = hopInbound(inbounds, h.inboundId)
          const last = i === c.hops.length - 1
          const p = i === 0 ? undefined : probe(i - 1)
          return (
            <span key={i} style={{ display: 'contents' }}>
              <div className="arrow" aria-hidden="true"><span>{p ? (p.ok ? `${p.latencyMs} ms` : t('超时')) : '—'}</span>→</div>
              <div className={'hop' + (last ? ' land' : '')}>
                <b>{name(h.nodeId)}</b>
                <span>{c.mode === 'l4' && !last ? t('转发器 · realm') : ib ? `${ib.tag} · ${coreShort[ib.core]}` : '—'}</span>
              </div>
            </span>
          )
        })}
        <div className="arrow" aria-hidden="true"><span>{probe(c.hops.length - 1)?.ok ? `${probe(c.hops.length - 1)!.latencyMs} ms` : '—'}</span>→</div>
        <div className="hop"><b>{t('出口')}</b><span>{t('见落地节点路由')}</span></div>
      </div>
      <div className="row">
        <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await post(`/chains/${c.id}/probe`); onChanged() }, t('探测完成'))}>{t('立即探测')}</button>
        <button className="btn sm" type="button" onClick={onEdit}>{t('编辑')}</button>
        <button className="btn sm danger" type="button" onClick={async () => {
          run(async () => {
            const preview = await post<{ etag: string; blocked: boolean; references: { kind: string; resourceId: string }[]; warnings: string[]; nodes: { name: string }[]; endpointIds: string[]; outboundIds: string[]; routingRuleIds: string[]; serviceUserIds: string[] }>(`/chains/${c.id}/change-preview`, { action: 'delete' })
            if (preview.blocked) throw new Error(t('仍有外部引用，解除引用后才能执行。') + '\n' + preview.references.map((r) => `${r.kind}: ${r.resourceId}`).join('\n'))
            const detail = [t('删除链路 {name}？相关出站、路由和服务账号会一并回收。', { name: c.name }), preview.nodes.map((n) => n.name).join(' → '), `${t('涉及发布入口')} ${preview.endpointIds.length} · ${t('出站')} ${preview.outboundIds.length} · ${t('路由规则')} ${preview.routingRuleIds.length} · ${t('服务身份')} ${preview.serviceUserIds.length}`, ...preview.warnings].join('\n')
            if (await confirm(detail, { danger: true })) { await api('DELETE', '/chains/' + c.id, undefined, { 'If-Match': preview.etag }); onChanged() }
          })
        }}>{t('删除')}</button>
        {c.probedAt && <span className="small mute">{t('上次探测 {ago}', { ago: ago(c.probedAt) })}</span>}
      </div>
    </article>
  )
}

function ChainDrawer({ chain, nodes, inbounds, onClose, onSaved }: { chain?: Chain; nodes: Node[]; inbounds: Inbound[]; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(chain?.name ?? '')
  const [mode, setMode] = useState<'l4' | 'proxy'>(chain?.mode ?? 'l4')
  const [hops, setHops] = useState<ChainHop[]>(chain?.hops ?? [{ nodeId: '', port: 30000 }, { nodeId: '' }])
  const [remark, setRemark] = useState(chain?.remark ?? '')
  const { busy, run } = useAction()
  const setHop = (i: number, h: Partial<ChainHop>) => setHops((x) => x.map((y, j) => (j === i ? { ...y, ...h } : y)))
  const nodeIns = (id: string) => inbounds.filter((i) => i.nodeId === id)
  return (
    <Drawer title={chain ? t('编辑链路') : t('新建链路')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !name} onClick={() => run(async () => {
        const body = { name, mode, remark, hops: hops.map((h, i) => ({ nodeId: h.nodeId, inboundId: i === hops.length - 1 || mode === 'proxy' ? h.inboundId : undefined, port: mode === 'l4' && i < hops.length - 1 ? Number(h.port) : undefined })) }
        if (chain) await put('/chains/' + chain.id, body)
        else await post('/chains', body)
        onSaved()
      }, t('已保存并下发'))}>{t('保存')}</button>
    </>}>
      <Field label={t('名称')} htmlFor="ch-name"><input id="ch-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="hk-relay → jp" /></Field>
      <Field label={t('模式')}>
        <div className="seg">
          <button type="button" className={'opt' + (mode === 'l4' ? ' on' : '')} onClick={() => setMode('l4')}>{t('L4 端口转发')}<small>{t('不解密，REALITY / Hy2 原样透传；在落地统计')}</small></button>
          <button type="button" className={'opt' + (mode === 'proxy' ? ' on' : '')} onClick={() => setMode('proxy')}>{t('协议中转')}<small>{t('中转跑入站解密再转发；可在中转分流')}</small></button>
        </div>
      </Field>
      {hops.map((h, i) => {
        const last = i === hops.length - 1
        return (
          <div key={i} className="card" style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 10 }}>
            <div className="row" style={{ justifyContent: 'space-between' }}>
              <b>{last ? t('落地') : t('中转 {n}', { n: i + 1 })}</b>
              {!last && hops.length > 2 && <button className="link" type="button" onClick={() => setHops((x) => x.filter((_, j) => j !== i))}>{t('移除')}</button>}
            </div>
            <div className="form-grid">
              <Field label={t('节点')}>
                <select className="input" value={h.nodeId} onChange={(e) => setHop(i, { nodeId: e.target.value, inboundId: undefined })}>
                  <option value="">{t('选择节点')}</option>
                  {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}
                </select>
              </Field>
              {mode === 'l4' && !last ? (
                <Field label={t('转发监听端口')}><input className="input mono" value={h.port ?? ''} onChange={(e) => setHop(i, { port: Number(e.target.value) || undefined })} /></Field>
              ) : (
                <Field label={last ? t('落地入站') : t('中转入站')}>
                  <select className="input" value={h.inboundId ?? ''} onChange={(e) => setHop(i, { inboundId: e.target.value })}>
                    <option value="">{t('选择入站')}</option>
                    {nodeIns(h.nodeId).map((ib) => <option key={ib.id} value={ib.id}>{ib.tag} · {inboundSummary(ib)}</option>)}
                  </select>
                </Field>
              )}
            </div>
          </div>
        )
      })}
      {hops.length < 4 && <div><button className="btn sm" type="button" onClick={() => setHops((x) => [...x.slice(0, -1), { nodeId: '', port: 30000 }, x[x.length - 1]])}>{t('+ 增加中转跳（最多 3 跳）')}</button></div>}
      <Field label={t('备注')} htmlFor="ch-remark"><input id="ch-remark" className="input" value={remark} onChange={(e) => setRemark(e.target.value)} /></Field>
      <div className="note">{mode === 'l4' ? t('保存后自动在落地入站上添加「中转」入口，订阅里生成「[中转名] → [落地名]」节点。') : t('主控自动创建服务账号（不计配额、不出现在用户列表）、中转节点上的出站与路由规则；落地入站变更时上游自动重新下发。')}</div>
    </Drawer>
  )
}

function CDNDrawer({ profile, onClose, onSaved }: { profile?: CDNProfile; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState({
    name: profile?.name ?? '', provider: profile?.provider ?? 'cloudflare', originDomain: profile?.originDomain ?? '', tlsMode: profile?.tlsMode ?? 'full_strict',
    ports: (profile?.ports ?? [443, 2053, 2083, 2087, 2096, 8443]).join(', '), preferredIps: toLines(profile?.preferredIps), ipSourceUrl: profile?.ipSourceUrl ?? '',
    ipTopN: String(profile?.ipTopN ?? 0), expandPerIp: profile?.expandPerIp ?? false, autoDns: profile?.autoDns ?? false, caddyFronting: profile?.caddyFronting ?? false, apiToken: '',
    nodeCheck: profile?.nodeCheck ?? false, staleHours: String(profile?.staleHours || 24),
  })
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  return (
    <Drawer title={profile ? t('CDN 配置档 · {name}', { name: profile.name }) : t('新建 CDN 配置档')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.name || !f.originDomain} onClick={() => run(async () => {
        const body = { ...f, ports: f.ports.split(/[,\s]+/).map(Number).filter(Boolean), preferredIps: fromLines(f.preferredIps), ipTopN: Number(f.ipTopN) || 0, staleHours: Number(f.staleHours) || 0 }
        if (profile) await put('/cdn-profiles/' + profile.id, body)
        else await post('/cdn-profiles', body)
        onSaved()
      }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('名称')}><input className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="cf-main" /></Field>
        <Field label={t('服务商')}>
          <select className="input" value={f.provider} onChange={(e) => set('provider', e.target.value)}><option value="cloudflare">Cloudflare</option><option value="other">{t('其他')}</option></select>
        </Field>
        <Field label={t('回源域名')} full><input className="input mono" value={f.originDomain} onChange={(e) => set('originDomain', e.target.value)} placeholder="sub.example.com" /></Field>
        <Field label={t('TLS 模式')}>
          <select className="input" value={f.tlsMode} onChange={(e) => set('tlsMode', e.target.value)}><option value="full_strict">Full (strict)</option><option value="full">Full</option><option value="flexible">Flexible</option></select>
        </Field>
        <Field label={t('可用端口')}><input className="input mono" value={f.ports} onChange={(e) => set('ports', e.target.value)} /></Field>
        <Field label="Cloudflare API Token" full hint={profile?.hasToken ? t('已保存（留空不修改）· 加密存储') : t('可选：填入后可自动同步 DNS、设置 SSL、签发 Origin 证书')}>
          <input className="input mono" type="password" value={f.apiToken} onChange={(e) => set('apiToken', e.target.value)} autoComplete="off" />
        </Field>
        <Field label={t('优选 IP 来源 URL')} full hint={t('每 6 小时拉取；每行一个 IP，支持 ip:port / CSV')}><input className="input mono" value={f.ipSourceUrl} onChange={(e) => set('ipSourceUrl', e.target.value)} /></Field>
        <Field label={t('优选 IP（手填）')} full><textarea className="input" style={{ minHeight: 80 }} value={f.preferredIps} onChange={(e) => set('preferredIps', e.target.value)} /></Field>
        <Field label={t('订阅取前 N 个')} hint={t('0 为全部')}><input className="input mono" value={f.ipTopN} onChange={(e) => set('ipTopN', e.target.value)} /></Field>
        <Field label={t('测速点多久没上报就告警（小时）')} hint={t('只在有测速点上报过时生效；告警后继续用原有 IP')}><input className="input mono" value={f.staleHours} onChange={(e) => set('staleHours', e.target.value)} /></Field>
      </div>
      <div className="toggle"><span>{t('节点检测：每 30 分钟由在线节点连接各优选 IP，所有节点都连不上的不放进订阅')}</span><Switch label={t('节点检测')} on={f.nodeCheck} onChange={(v) => set('nodeCheck', v)} /></div>
      <div className="toggle"><span>{t('订阅按优选 IP 展开（一个 IP 一个节点）')}</span><Switch label={t('展开')} on={f.expandPerIp} onChange={(v) => set('expandPerIp', v)} /></div>
      <div className="toggle"><span>{t('自动同步 DNS 记录（开启代理）')}</span><Switch label="DNS" on={f.autoDns} onChange={(v) => set('autoDns', v)} /></div>
      <div className="toggle"><span>{t('Agent 内置 Caddy 统一回源（多个 WS/gRPC/HTTPUpgrade 入站共享 443，按路径分流）')}</span><Switch label="Caddy" on={f.caddyFronting} onChange={(v) => set('caddyFronting', v)} /></div>
    </Drawer>
  )
}

function CDNAside({ p, onEdit, onChanged }: { p: CDNProfile; onEdit: () => void; onChanged: () => void }) {
  const { busy, run } = useAction()
  return (
    <>
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('CDN 配置档 · {name}', { name: p.name })}</h2><span className="tag" style={{ margin: 0 }}>{p.provider === 'cloudflare' ? 'Cloudflare' : t('其他')}</span></div>
        <dl className="kv" style={{ margin: '16px 0 0' }}>
          <dt>{t('回源域名')}</dt><dd className="mono">{p.originDomain}</dd>
          <dt>{t('TLS 模式')}</dt><dd>{p.tlsMode === 'full_strict' ? 'Full (strict)' : p.tlsMode}</dd>
          <dt>{t('可用端口')}</dt><dd>{p.ports.map((x) => <span key={x} className="tag">{x}</span>)}</dd>
          <dt>{t('使用入站')}</dt><dd>{t('{n} 个', { n: p.inbounds })}</dd>
          <dt>API Token</dt><dd>{p.hasToken ? t('已配置') : t('未配置（仅生成订阅）')}</dd>
        </dl>
        {p.lastError && <div className="note bad" style={{ marginTop: 12 }}>{p.lastError}</div>}
        <div className="row" style={{ marginTop: 16 }}>
          <button className="btn sm" type="button" onClick={onEdit}>{t('编辑')}</button>
          <button className="btn sm" type="button" disabled={busy || !p.hasToken} onClick={() => run(async () => { await post(`/cdn-profiles/${p.id}/sync-dns`); onChanged() }, t('DNS 已同步'))}>{t('同步 DNS')}</button>
        </div>
      </section>
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('优选 IP')}</h2>
          <button className="btn sm" type="button" disabled={busy || !p.ipSourceUrl} onClick={() => run(async () => { await post(`/cdn-profiles/${p.id}/refresh-ips`); onChanged() }, t('已刷新'))}>{t('刷新')}</button></div>
        <div className="sub" style={{ marginBottom: 10 }}>{p.reports.length ? t('由 {n} 个测速点维护', { n: p.reports.length }) : p.ipSourceUrl ? t('来源：{url}', { url: p.ipSourceUrl }) : t('手动维护')} · {p.expandPerIp ? t('订阅取前 {n} 个', { n: p.ipTopN || t('全部') }) : t('订阅只用回源域名')}{p.lastRefreshAt ? ' · ' + ago(p.lastRefreshAt) : ''}</div>
        <div className="mono small" style={{ background: 'var(--soft)', borderRadius: 8, padding: '10px 12px', lineHeight: 1.8, maxHeight: 200, overflowY: 'auto' }}>
          {p.preferredIps.length ? p.preferredIps.map((ip) => {
            const bad = p.nodeCheck && (p.badIps ?? []).includes(ip)
            return <div key={ip} style={bad ? { color: 'var(--bad)', textDecoration: 'line-through' } : undefined} title={bad ? t('节点都连不上，订阅暂不使用') : undefined}>{ip}</div>
          }) : t('暂无')}
        </div>
        {p.nodeCheck && <div className="sub" style={{ marginTop: 8 }}>{p.checkedAt ? t('节点检测：{t}，{n} 个连不上', { t: ago(p.checkedAt), n: (p.badIps ?? []).length }) : t('节点检测：等待第一次检测')}</div>}
      </section>
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('测速点')}</h2><span className="sub">{t('超过 {h} 小时没上报会告警', { h: p.staleHours || 24 })}</span></div>
        {p.reports.length === 0 ? <div className="sub">{t('还没有测速点上报。用带「上报 CDN 优选 IP」权限的 API 令牌，从用户所在地区的机器调用 POST /cdn-profiles/{id}/preferred-ips。', { id: p.id })}</div> : (
          <div style={{ marginTop: 10 }}>{p.reports.map((r) => {
            const stale = Date.now() - new Date(r.at).getTime() > (p.staleHours || 24) * 3600e3
            const best = r.ips[0]
            return (
              <div key={r.source} className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start', gap: 8, padding: '8px 0', borderTop: '1px solid var(--line)' }}>
                <div style={{ minWidth: 0 }}>
                  <div><b>{r.source}</b> <span className="sub" style={stale ? { color: 'var(--bad)' } : undefined}>· {ago(r.at)}</span></div>
                  <div className="mono small" style={{ overflowWrap: 'anywhere' }}>{best ? `${best.ip} · ${Math.round(best.latencyMs)} ms${best.loss > 0 ? ` · ${t('丢包')} ${Math.round(best.loss * 100)}%` : ''}` : '—'} <span className="sub">{t('共 {n} 个', { n: r.ips.length })}</span></div>
                </div>
                <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del(`/cdn-profiles/${p.id}/ip-reports/${encodeURIComponent(r.source)}`); onChanged() }, t('已移除'))}>{t('移除')}</button>
              </div>
            )
          })}</div>
        )}
      </section>
    </>
  )
}

const RESERVED = ['direct', 'block']

const lbStrategies: [string, string, string][] = [
  ['leastPing', t('延迟最低'), t('持续探测，选最快')], ['leastLoad', t('最稳定'), t('延迟 + 抖动综合（Xray）')], ['fallback', t('故障转移'), t('按顺序选第一个可用')],
  ['roundRobin', t('轮询'), t('逐连接轮换（仅 Xray）')], ['random', t('随机'), t('逐连接随机（仅 Xray）')],
]

function dependsOn(tag: string, target: string, outbounds: Outbound[], seen = new Set<string>()): boolean {
  if (tag === target) return true
  if (seen.has(tag)) return false
  seen.add(tag)
  const o = outbounds.find((o) => o.tag === tag)
  if (!o) return false
  const group = ['balancer', 'urltest', 'selector'].includes(o.type)
  return [o.detour, ...(group ? [...(o.settings?.members ?? []), o.settings?.fallback] : [])]
    .some((next) => next && dependsOn(next, target, outbounds, seen))
}

function fallbackPath(tag: string, fallback: string, outbounds: Outbound[]): string {
  const path = [tag || '?']
  const seen = new Set(path)
  let next = fallback
  while (next && !seen.has(next)) {
    path.push(next)
    seen.add(next)
    next = outbounds.find((o) => o.tag === next)?.settings?.fallback ?? ''
  }
  return path.join(' → ')
}

function OutboundDrawer({ node, outbound, inbounds, outbounds, onClose, onSaved }: { node: Node; outbound?: Outbound; inbounds: Inbound[]; outbounds: Outbound[]; onClose: () => void; onSaved: () => void }) {
  const s = outbound?.settings ?? {}
  const [f, setF] = useState({
    tag: outbound?.tag ?? '', type: outbound?.type ?? 'socks', sourceNodeId: outbound?.sourceNodeId ?? '', nodeRefInboundId: outbound?.nodeRefInboundId ?? '', detour: outbound?.detour ?? '', remark: outbound?.remark ?? '',
    server: s.server ?? '', port: String(s.port ?? ''), username: s.username ?? '', password: s.password ?? '', uuid: s.uuid ?? '', method: s.method ?? '2022-blake3-aes-128-gcm',
    privateKey: s.privateKey ?? '', peerPublicKey: s.peerPublicKey ?? '', localAddress: (s.localAddress ?? []).join(', '), reserved: (s.reserved ?? []).join(','),
    members: (s.members ?? []) as string[], sni: s.security?.serverName ?? '', tls: !!s.security,
    strategy: s.strategy ?? 'leastPing', testUrl: s.testUrl ?? '', interval: s.interval ?? '', tolerance: String(s.tolerance ?? ''), fallback: s.fallback ?? '',
  })
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  const settings = () => {
    switch (f.type) {
      case 'socks': case 'http': return { server: f.server, port: Number(f.port), username: f.username || undefined, password: f.password || undefined }
      case 'shadowsocks': return { server: f.server, port: Number(f.port), method: f.method, password: f.password }
      case 'trojan': case 'hysteria2': return { server: f.server, port: Number(f.port), password: f.password, security: { type: 'tls', serverName: f.sni || undefined } }
      case 'vless': case 'vmess': return { server: f.server, port: Number(f.port), uuid: f.uuid, security: f.tls ? { type: 'tls', serverName: f.sni || undefined } : undefined }
      case 'wireguard': return { server: f.server, port: Number(f.port), privateKey: f.privateKey, peerPublicKey: f.peerPublicKey, localAddress: fromLines(f.localAddress), reserved: f.reserved ? f.reserved.split(',').map(Number) : undefined }
      case 'selector': return { members: f.members }
      case 'urltest': return { members: f.members, fallback: f.fallback || undefined, testUrl: f.testUrl || undefined, interval: f.interval || undefined, tolerance: Number(f.tolerance) || undefined }
      case 'balancer': return { members: f.members, strategy: f.strategy, testUrl: f.testUrl || undefined, interval: f.interval || undefined, tolerance: Number(f.tolerance) || undefined, fallback: f.fallback || undefined }
      default: return {}
    }
  }
  const remote = inbounds.filter((i) => i.nodeId !== node.id)
  const sourceNodes = useLoad(() => get<SourceNode[]>('/sub-source-nodes'))
  const caps = useLoad(() => get<CapSet[]>('/capabilities?node=' + node.id), [node.id]).data ?? []
  const hasSingbox = node.cores.some((c) => c.type === 'singbox')
  const hasMihomo = node.cores.some((c) => c.type === 'mihomo')
  const isAutoGroup = f.type === 'balancer' || f.type === 'urltest'
  const fallbackUnsupported = isAutoGroup && !!f.fallback && (hasSingbox || (hasMihomo && f.fallback === 'block'))
  const fallbackChoices = outbounds.filter((o) => o.tag !== f.tag && !f.members.includes(o.tag) && !dependsOn(o.tag, outbound?.tag ?? f.tag, outbounds))

  return (
    <Drawer title={outbound ? t('编辑出站 · {tag}', { tag: outbound.tag }) : t('新建出站')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.tag || fallbackUnsupported} onClick={() => run(async () => {
        const body = { tag: f.tag, type: f.type, sourceNodeId: f.type === 'source_ref' ? f.sourceNodeId : '', nodeRefInboundId: f.type === 'node_ref' ? f.nodeRefInboundId : '', detour: f.detour, remark: f.remark, settings: f.type === 'node_ref' || f.type === 'source_ref' ? undefined : settings() }
        if (outbound) await put(`/nodes/${node.id}/outbounds/${outbound.id}`, body)
        else await post(`/nodes/${node.id}/outbounds`, body)
        onSaved()
      }, t('已保存并下发'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('标签')}><input className="input mono" value={f.tag} onChange={(e) => set('tag', e.target.value)} placeholder="warp" /></Field>
        <Field label={t('类型')}>
          <select className="input" value={f.type} onChange={(e) => set('type', e.target.value)}>
            <option value="socks">{t('SOCKS5（住宅代理等）')}</option><option value="http">HTTP</option><option value="wireguard">WireGuard（WARP）</option>
            <option value="shadowsocks">Shadowsocks</option><option value="vless">VLESS</option><option value="vmess">VMess</option><option value="trojan">Trojan</option>
            <option value="hysteria2">{t('Hysteria2（仅 sing-box）')}</option><option value="node_ref">{t('面板内另一节点')}</option><option value="source_ref">{t('外部订阅节点')}</option>
            <option value="balancer">{t('负载均衡组')}</option><option value="urltest">{t('自动选择组（延迟最低）')}</option><option value="selector">{t('手动选择组')}</option><option value="direct">{t('直连')}</option>
          </select>
        </Field>
      </div>
      {f.type === 'source_ref' ? <Field label={t('来源节点')} hint={t('凭据跟随来源刷新，不复制到出站；已移除或停用时阻断连接。')}><select className="input" value={f.sourceNodeId} onChange={(e) => set('sourceNodeId', e.target.value)}><option value="">{t('选择来源节点')}</option>{(sourceNodes.data ?? []).filter((n) => (n.available && n.supported) || n.id === f.sourceNodeId).map((n) => <option key={n.id} value={n.id}>{n.source} · {n.name} · {n.protocol}{!n.available ? ' · ' + t('不可用') : ''}</option>)}</select>{sourceNodes.error && <div className="hint">{sourceNodes.error}</div>}</Field> : f.type === 'node_ref' ? (
        <Field label={t('目标入站')} hint={t('地址与服务账号由主控自动填写')}>
          <select className="input" value={f.nodeRefInboundId} onChange={(e) => set('nodeRefInboundId', e.target.value)}>
            <option value="">{t('选择其他节点的入站')}</option>
            {remote.map((i) => <option key={i.id} value={i.id}>{i.nodeName} · {i.tag} · {inboundSummary(i)}</option>)}
          </select>
        </Field>
      ) : f.type === 'urltest' || f.type === 'selector' || f.type === 'balancer' ? (<>
        <Field label={t('成员出站')} hint={t('点击选择，按选择顺序排列（故障转移按此顺序）；健康检查失败的成员自动移出、恢复后加回')}>
          <div className="row">
            {outbounds.filter((o) => o.tag !== f.tag && o.type !== 'balancer' && o.type !== 'urltest' && o.type !== 'selector').map((o) => {
              const i = f.members.indexOf(o.tag)
              return <button key={o.id} type="button" className={'chip' + (i >= 0 ? ' on' : '')} aria-pressed={i >= 0}
                onClick={() => set('members', i >= 0 ? f.members.filter((m) => m !== o.tag) : [...f.members, o.tag])}>{i >= 0 ? `${i + 1}. ` : ''}{o.tag}</button>
            })}
            {['direct'].map((t) => {
              const i = f.members.indexOf(t)
              return <button key={t} type="button" className={'chip' + (i >= 0 ? ' on' : '')} onClick={() => set('members', i >= 0 ? f.members.filter((m) => m !== t) : [...f.members, t])}>{i >= 0 ? `${i + 1}. ` : ''}{t}</button>
            })}
          </div>
        </Field>
        {f.type === 'balancer' && (
          <Field label={t('均衡策略')} hint={caps.find((c) => c.core === 'singbox') && !caps.find((c) => c.core === 'xray') ? t('sing-box 仅支持延迟最低 / 最稳定 / 故障转移') : t('Xray 支持全部策略；sing-box 节点上随机 / 轮询不可用')}>
            <div className="seg c3">
              {lbStrategies.map(([k, l, h]) => (
                <button key={k} type="button" className={'opt' + (f.strategy === k ? ' on' : '')} onClick={() => set('strategy', k)}>{l}<small>{h}</small></button>
              ))}
            </div>
          </Field>
        )}
        {f.type !== 'selector' && (
          <div className="form-grid">
            <Field label={t('探测地址')} hint={t('默认 gstatic generate_204')}><input className="input mono" value={f.testUrl} onChange={(e) => set('testUrl', e.target.value)} placeholder="https://www.gstatic.com/generate_204" /></Field>
            <Field label={t('探测间隔')}><input className="input mono" value={f.interval} onChange={(e) => set('interval', e.target.value)} placeholder="1m" /></Field>
            <Field label={t('容差（ms）')} hint={t('延迟差小于此值不切换')}><input className="input mono" value={f.tolerance} onChange={(e) => set('tolerance', e.target.value)} placeholder="50" /></Field>
            <Field label={t('兜底出站 / 均衡组')} hint={t('主组全部不可用时切换备用组；恢复后新连接回到主组。支持多级回退，禁止成环。')}>
              <select className="input" aria-label={t('兜底出站 / 均衡组')} value={f.fallback} onChange={(e) => set('fallback', e.target.value)}>
                <option value="">{t('无')}</option><option value="direct" disabled={hasSingbox}>direct</option><option value="block" disabled={hasSingbox || hasMihomo}>block</option>
                <optgroup label={t('备用均衡组')}>
                  {fallbackChoices.filter((o) => ['balancer', 'urltest', 'selector'].includes(o.type)).map((o) => <option key={o.id} value={o.tag} disabled={hasSingbox}>{o.tag}</option>)}
                </optgroup>
                <optgroup label={t('备用出站')}>
                  {fallbackChoices.filter((o) => !['balancer', 'urltest', 'selector'].includes(o.type)).map((o) => <option key={o.id} value={o.tag} disabled={hasSingbox}>{o.tag}</option>)}
                </optgroup>
                {f.fallback && !RESERVED.includes(f.fallback) && !fallbackChoices.some((o) => o.tag === f.fallback) && <option value={f.fallback} disabled>{f.fallback} · {t('不可用，请重新选择')}</option>}
              </select>
              {f.fallback && <div className="hint mono">{t('回退路径：{path}', { path: fallbackPath(f.tag, f.fallback, outbounds) })}</div>}
              {hasSingbox && <div className="hint">{t('sing-box 暂不支持组间兜底；请清除兜底或使用 Xray / mihomo。')}</div>}
              {hasMihomo && <div className="hint">{t('mihomo 支持备用组或出站，不支持 block 兜底。')}</div>}
            </Field>
          </div>
        )}
      </>) : f.type !== 'direct' && (
        <div className="form-grid">
          <Field label={t('服务器')}><input className="input mono" value={f.server} onChange={(e) => set('server', e.target.value)} /></Field>
          <Field label={t('端口')}><input className="input mono" value={f.port} onChange={(e) => set('port', e.target.value)} /></Field>
          {(f.type === 'socks' || f.type === 'http') && <Field label={t('用户名')}><input className="input mono" value={f.username} onChange={(e) => set('username', e.target.value)} /></Field>}
          {['socks', 'http', 'shadowsocks', 'trojan', 'hysteria2'].includes(f.type) && <Field label={t('密码')}><input className="input mono" value={f.password} onChange={(e) => set('password', e.target.value)} /></Field>}
          {f.type === 'shadowsocks' && <Field label={t('加密方式')}><input className="input mono" value={f.method} onChange={(e) => set('method', e.target.value)} /></Field>}
          {(f.type === 'vless' || f.type === 'vmess') && <Field label="UUID" full><input className="input mono" value={f.uuid} onChange={(e) => set('uuid', e.target.value)} /></Field>}
          {['vless', 'vmess', 'trojan', 'hysteria2'].includes(f.type) && <Field label="SNI"><input className="input mono" value={f.sni} onChange={(e) => set('sni', e.target.value)} /></Field>}
          {(f.type === 'vless' || f.type === 'vmess') && <label className="chk"><input type="checkbox" checked={f.tls} onChange={(e) => set('tls', e.target.checked)} />TLS</label>}
          {f.type === 'wireguard' && <>
            <Field label={t('私钥')} full><input className="input mono" value={f.privateKey} onChange={(e) => set('privateKey', e.target.value)} /></Field>
            <Field label={t('对端公钥')} full><input className="input mono" value={f.peerPublicKey} onChange={(e) => set('peerPublicKey', e.target.value)} /></Field>
            <Field label={t('本地地址')}><input className="input mono" value={f.localAddress} onChange={(e) => set('localAddress', e.target.value)} placeholder="172.16.0.2/32" /></Field>
            <Field label="Reserved"><input className="input mono" value={f.reserved} onChange={(e) => set('reserved', e.target.value)} placeholder="0,0,0" /></Field>
          </>}
        </div>
      )}
      <Field label={t('下一跳（detour）')} hint={t('经由另一个出站发出，串起来就是出口链；保存时检查成环与跳数')}>
        <select className="input" value={f.detour} onChange={(e) => set('detour', e.target.value)}>
          <option value="">{t('无（直接发出）')}</option>
          {outbounds.filter((o) => o.tag !== f.tag).map((o) => <option key={o.id} value={o.tag}>{o.tag}</option>)}
        </select>
      </Field>
      <Field label={t('备注')}><input className="input" value={f.remark} onChange={(e) => set('remark', e.target.value)} /></Field>
    </Drawer>
  )
}

const matchKeys: { k: keyof RouteMatch; label: string }[] = [
  { k: 'geosite', label: 'geosite' }, { k: 'geoip', label: 'geoip' }, { k: 'domain', label: 'domain' }, { k: 'domainSuffix', label: 'domain_suffix' },
  { k: 'domainKeyword', label: 'keyword' }, { k: 'ipCidr', label: 'ip_cidr' }, { k: 'ruleSets', label: t('规则集') }, { k: 'port', label: 'port' }, { k: 'inbound', label: 'inbound' }, { k: 'userGroups', label: t('用户分组') },
]

function matchText(m: RouteMatch, groupName: (id: string) => string) {
  const parts: string[] = []
  for (const { k, label } of matchKeys) {
    const v = m[k]
    if (Array.isArray(v) && v.length) parts.push(`${label}: ${k === 'userGroups' ? v.map(groupName).join(',') : v.join(',')}`)
  }
  if (m.network) parts.push('network: ' + m.network)
  return parts.length ? parts : [t('默认')]
}

function Routing({ nodes }: { nodes: Node[] }) {
  const [nodeId, setNodeId] = useState(nodes[0]?.id ?? '')
  const global = nodeId === '*'
  const node = nodes.find((n) => n.id === nodeId)
  const outs = useLoad(() => (global
    ? Promise.all(nodes.map((n) => get<Outbound[]>(`/nodes/${n.id}/outbounds`))).then((ls) => {
      const seen = new Set<string>()
      return ls.flat().filter((o) => !o.chainId && !seen.has(o.tag) && !!seen.add(o.tag))
    })
    : nodeId ? get<Outbound[]>(`/nodes/${nodeId}/outbounds`) : Promise.resolve([])), [nodeId, nodes.length])
  const presets = useLoad(() => get<RulePreset[]>('/routing/presets'))
  const [preset, setPreset] = useState<{ key: string; outbound: string; nodeIds: string[] } | null>(null)
  const rules = useLoad(() => (nodeId ? get<RoutingRule[]>(`/nodes/${nodeId}/routing`) : Promise.resolve([])), [nodeId])
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const groups = useLoad(() => get<{ id: string; name: string }[]>('/groups'))
  const ruleSets = useLoad(() => get<RuleSet[]>('/rulesets'))
  const [drawer, setDrawer] = useState<Outbound | 'new' | null>(null)
  const [geoPick, setGeoPick] = useState<'geosite' | 'geoip' | null>(null)
  const [warp, setWarp] = useState(false)
  const [draft, setDraft] = useState<{ priority: number; outboundTag: string; key: keyof RouteMatch; value: string } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const gname = (id: string) => groups.data?.find((g) => g.id === id)?.name ?? id
  const manual = (rules.data ?? []).filter((r) => !r.chainId)
  const saveRules = (list: { match: RouteMatch; outboundTag: string; priority: number; remark?: string }[]) =>
    run(async () => { await put(`/nodes/${nodeId}/routing`, list); rules.reload() }, t('路由已保存并下发'))
  const tags = [...RESERVED, ...(outs.data ?? []).map((o) => o.tag)]
  const nodeHasHysteria = (inbounds.data ?? []).some((i) => i.core === 'hysteria' && (global || i.nodeId === nodeId))
  return (
    <>
      <div className="row">
        <label className="lbl" htmlFor="rt-node">{t('节点')}</label>
        <select id="rt-node" className="input" style={{ width: 'auto', minWidth: 200 }} value={nodeId} onChange={(e) => setNodeId(e.target.value)}>
          <option value="*">{t('全局规则（所有节点）')}</option>
          {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}
        </select>
        <span className="spacer" />
        <button className="btn" type="button" onClick={() => setPreset({ key: presets.data?.[0]?.key ?? '', outbound: '', nodeIds: [nodeId] })}>{t('应用规则预设')}</button>
        <button className="btn" type="button" disabled={!node} onClick={() => setWarp(true)} title={t('一键注册 Cloudflare WARP 并添加为出站')}>{t('+ WARP')}</button>
        <button className="btn pri" type="button" disabled={!node} onClick={() => setDrawer('new')}>{t('+ 出站')}</button>
      </div>
      {warp && node && <WarpDialog nodeId={node.id} nodeName={node.name} taken={(outs.data ?? []).map((o) => o.tag)} onClose={() => setWarp(false)}
        onDone={() => { setWarp(false); outs.reload(); rules.reload() }} />}
      {global && <div className="note">{t('全局规则下发到每个节点，排在节点自身规则之后（同优先级时节点规则优先）；引用的出站在某节点不存在时，该节点自动跳过这条规则。')}</div>}
      {nodeHasHysteria && <div className="note">{t('这个节点有 Hysteria2 入站：Hysteria2 只能走 直连 / 拦截 / SOCKS5 / HTTP 出站，按用户、关键词的规则和指向其他出站的规则对它不生效；用到 geosite / geoip 时由 Hysteria2 自行下载规则文件。')}</div>}
      {!global && <section className="card flush">
        <div className="ctitle" style={{ padding: '12px 16px 4px' }}><span>{t('出站 · {name}', { name: node?.name })}</span><span className="sub" style={{ margin: 0 }}>{t('detour 串成出口链（Xray: dialerProxy · sing-box: detour）')}</span></div>
        <table>
          <thead><tr><th>{t('标签')}</th><th>{t('类型')}</th><th>{t('目标')}</th><th>{t('下一跳')}</th><th></th></tr></thead>
          <tbody>
            <tr><td className="mono">direct</td><td>{t('直连')}</td><td className="mute">{t('落地本机 IP')}</td><td>—</td><td /></tr>
            <tr><td className="mono">block</td><td>{t('拦截')}</td><td className="mute">—</td><td>—</td><td /></tr>
            {(outs.data ?? []).map((o) => (
              <tr key={o.id}>
                <td className="mono">{o.tag}{o.chainId && <span className="tag" style={{ marginLeft: 6 }}>{t('链路')}</span>}</td>
                <td>{o.type}</td>
                <td className="mono small">{o.type === 'source_ref' ? t('来源节点 · {id}', { id: o.sourceNodeId }) : o.type === 'node_ref' ? (inbounds.data?.find((i) => i.id === o.nodeRefInboundId)?.nodeName ?? '') + ' · ' + (inbounds.data?.find((i) => i.id === o.nodeRefInboundId)?.tag ?? '') : o.settings?.server ? `${o.settings.server}:${o.settings.port}` : o.settings?.members?.join(', ') ?? '—'}{o.settings?.fallback && <div className="hint">{t('兜底：{tag}', { tag: o.settings.fallback })}</div>}</td>
                <td className="mono">{o.detour || '—'}</td>
                <td style={{ textAlign: 'right' }}>{!o.chainId && <>
                  <button className="link" type="button" onClick={() => setDrawer(o)}>{t('编辑')}</button>
                  <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                    if (await confirm(t('删除出站 {tag}？', { tag: o.tag }), { danger: true })) run(async () => { await del(`/nodes/${nodeId}/outbounds/${o.id}`); outs.reload() })
                  }}>{t('删除')}</button></>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>}
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('路由规则 · {name}', { name: global ? t('全局') : node?.name })}</h2>
          <button className="btn sm" type="button" onClick={() => setDraft({ priority: (manual[manual.length - 1]?.priority ?? 0) + 10, outboundTag: tags[2] ?? 'direct', key: 'geosite', value: '' })}>{t('+ 规则')}</button></div>
        <div className="scroll" style={{ marginTop: 8 }}>
          <table>
            <thead><tr><th>{t('优先级')}</th><th>{t('匹配条件')}</th><th>{t('出口')}</th><th></th></tr></thead>
            <tbody>
              {(rules.data ?? []).map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.priority}</td>
                  <td>{matchText(r.match, gname).map((t) => <span key={t} className="tag">{t}</span>)}{r.chainId && <span className="tag xray">{t('链路自动')}</span>}</td>
                  <td>{r.outboundTag === 'block' ? t('拦截') : r.outboundTag === 'direct' ? t('直连') : r.outboundTag}{r.remark && <div className="small mute">{r.remark}</div>}</td>
                  <td style={{ textAlign: 'right' }}>{!r.chainId && <button className="link" type="button" style={{ color: 'var(--bad)' }} onClick={() =>
                    saveRules(manual.filter((x) => x.id !== r.id).map((x) => ({ match: x.match, outboundTag: x.outboundTag, priority: x.priority, remark: x.remark })))}>{t('删除')}</button>}</td>
                </tr>
              ))}
              {rules.data?.length === 0 && <tr><td colSpan={4} className="mute">{t('无规则：全部直连')}</td></tr>}
            </tbody>
          </table>
        </div>
        {draft && (
          <div className="form-grid rule-draft">
            <Field label={t('优先级')}><input className="input mono" value={draft.priority} onChange={(e) => setDraft({ ...draft, priority: Number(e.target.value) })} /></Field>
            <Field label={t('条件')}>
              <select className="input" value={draft.key} onChange={(e) => setDraft({ ...draft, key: e.target.value as keyof RouteMatch, value: '' })}>
                {matchKeys.map((m) => <option key={m.k} value={m.k}>{m.label}</option>)}<option value="network">network</option><option value="users">{t('（默认规则）')}</option>
              </select>
            </Field>
            <Field label={t('值（逗号分隔）')}>
              {draft.key === 'userGroups' ? (
                <select className="input" value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })}>
                  <option value="">{t('选择分组')}</option>{(groups.data ?? []).map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
                </select>
              ) : draft.key === 'ruleSets' ? (
                <select className="input" value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })}>
                  <option value="">{ruleSets.data?.length ? t('选择规则集') : t('先在「规则集」页新建')}</option>{(ruleSets.data ?? []).map((r) => <option key={r.id} value={r.tag}>{r.tag}{r.name ? ` · ${r.name}` : ''}（{r.count}）</option>)}
                </select>
              ) : draft.key === 'geosite' || draft.key === 'geoip' ? (
                <div className="joined">
                  <input className="input mono" value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })} placeholder={draft.key === 'geosite' ? 'netflix, openai' : 'cn, private'} />
                  <button className="btn" type="button" onClick={() => setGeoPick(draft.key as 'geosite' | 'geoip')}>{t('浏览')}</button>
                </div>
              ) : <input className="input mono" disabled={draft.key === 'users'} value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })} />}
              {geoPick && <GeoPicker kind={geoPick} value={fromLines(draft.value)} onChange={(v) => setDraft({ ...draft, value: v.join(', ') })} onClose={() => setGeoPick(null)} />}
            </Field>
            <Field label={t('出口')}>
              <select className="input" value={draft.outboundTag} onChange={(e) => setDraft({ ...draft, outboundTag: e.target.value })}>{tags.map((t) => <option key={t}>{t}</option>)}</select>
            </Field>
            <div className="row">
              <button className="btn sm pri" type="button" disabled={busy} onClick={() => {
                const match: RouteMatch = {}
                if (draft.key === 'network') match.network = draft.value
                else if (draft.key !== 'users') (match as any)[draft.key] = fromLines(draft.value)
                saveRules([...manual.map((x) => ({ match: x.match, outboundTag: x.outboundTag, priority: x.priority, remark: x.remark })), { match, outboundTag: draft.outboundTag, priority: draft.priority }])
                setDraft(null)
              }}>{t('添加')}</button>
              <button className="btn sm" type="button" onClick={() => setDraft(null)}>{t('取消')}</button>
            </div>
          </div>
        )}
      </section>
      {preset && (() => {
        const p = presets.data?.find((x) => x.key === preset.key)
        return (
          <Modal title={t('应用规则预设')} onClose={() => setPreset(null)} footer={<>
            <button className="btn" type="button" onClick={() => setPreset(null)}>{t('取消')}</button>
            <button className="btn pri" type="button" disabled={busy || !p || (!p.outbound && !preset.outbound) || !preset.nodeIds.length} onClick={() => run(async () => {
              await post('/routing/presets/apply', preset); setPreset(null); rules.reload()
            }, t('规则已追加并下发'))}>{t('应用')}</button>
          </>}>
            <Field label={t('预设')}>
              <div className="seg">
                {(presets.data ?? []).map((x) => (
                  <button key={x.key} type="button" className={'opt' + (preset.key === x.key ? ' on' : '')} onClick={() => setPreset({ ...preset, key: x.key, outbound: '' })}>{x.name}<small>{x.description}</small></button>
                ))}
              </div>
            </Field>
            <Field label={t('出口')} hint={p?.outbound ? t('默认 {tag}，可覆盖', { tag: p.outbound }) : t('该预设需要指定出口，例如解锁流媒体的落地或负载均衡组')}>
              <select className="input" value={preset.outbound} onChange={(e) => setPreset({ ...preset, outbound: e.target.value })}>
                <option value="">{p?.outbound ? t('默认（{tag}）', { tag: p.outbound }) : t('选择出口')}</option>
                {tags.map((t) => <option key={t} value={t}>{t}</option>)}
              </select>
            </Field>
            <Field label={t('应用到')} hint={t('选择「全局」则写入全局规则')}>
              <div className="row">
                {[{ id: '*', name: t('全局') }, ...nodes].map((n) => {
                  const on = preset.nodeIds.includes(n.id)
                  return <button key={n.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
                    onClick={() => setPreset({ ...preset, nodeIds: n.id === '*' ? (on ? [] : ['*']) : on ? preset.nodeIds.filter((x) => x !== n.id) : [...preset.nodeIds.filter((x) => x !== '*'), n.id] })}>{n.name}</button>
                })}
              </div>
            </Field>
          </Modal>
        )
      })()}
      {drawer && node && <OutboundDrawer node={node} outbound={drawer === 'new' ? undefined : drawer} inbounds={inbounds.data ?? []} outbounds={outs.data ?? []}
        onClose={() => setDrawer(null)} onSaved={() => { setDrawer(null); outs.reload() }} />}
    </>
  )
}

export default function Chains() {
  const { nodes } = useNodes()
  const [tab, setTab] = useState<Tab>('chains')
  const chains = useLoad(() => get<Chain[]>('/chains'))
  const cdn = useLoad(() => get<CDNProfile[]>('/cdn-profiles'))
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const [chainDrawer, setChainDrawer] = useState<Chain | 'new' | null>(null)
  const [cdnDrawer, setCdnDrawer] = useState<CDNProfile | 'new' | null>(null)
  const [cdnSel, setCdnSel] = useState<string | null>(null)
  const confirm = useConfirm()
  const { run } = useAction()
  const selCDN = useMemo(() => cdn.data?.find((c) => c.id === cdnSel) ?? cdn.data?.[0], [cdn.data, cdnSel])

  return (
    <>
      <div className="top">
        <div><h1>{t('链路与 CDN')}</h1><div className="sub">{t('入口（直连 / CDN / 中转）→ 落地 → 出口，每条链路都会生成独立的订阅节点')}</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => setCdnDrawer('new')}>{t('+ CDN 配置档')}</button>
          <button className="btn pri" type="button" disabled={nodes.length < 2} title={nodes.length < 2 ? t('至少需要两个节点') : undefined} onClick={() => setChainDrawer('new')}>{t('+ 新建链路')}</button>
        </div>
      </div>
      <div className="tabs" role="tablist" aria-label={t('视图')}>
        {([['chains', t('链路')], ['cdn', t('CDN 配置档')], ['routing', t('出站与路由')], ['rulesets', t('规则集')], ['dns', 'DNS']] as [Tab, string][]).map(([k, l]) => (
          <button key={k} className={'tab' + (tab === k ? ' on' : '')} type="button" role="tab" aria-selected={tab === k} onClick={() => setTab(k)}>{l}</button>
        ))}
      </div>
      {tab === 'chains' && (
        <div className="split">
          <div className="grow">
            {(chains.data ?? []).map((c) => <ChainCard key={c.id} c={c} nodes={nodes} inbounds={inbounds.data ?? []} onEdit={() => setChainDrawer(c)} onChanged={chains.reload} />)}
            {chains.data?.length === 0 && <div className="card"><Empty>{t('还没有链路。L4 转发适合线路优化；协议中转可在中转节点分流或换协议。')}</Empty></div>}
          </div>
          <aside className="aside">
            {selCDN ? <CDNAside p={selCDN} onEdit={() => setCdnDrawer(selCDN)} onChanged={cdn.reload} /> : <div className="card"><Empty>{t('还没有 CDN 配置档')}</Empty></div>}
            <section className="note">{t('REALITY、Hysteria2、TUIC 不能走 CDN；添加 CDN 入口时只列出 WS / gRPC / HTTPUpgrade / XHTTP 入站。')}</section>
          </aside>
        </div>
      )}
      {tab === 'cdn' && (
        <div className="split">
          <div className="grow">
            <div className="card flush scroll">
              {cdn.data?.length === 0 ? <Empty>{t('还没有 CDN 配置档')}</Empty> : (
                <table>
                  <thead><tr><th>{t('名称')}</th><th>{t('服务商')}</th><th>{t('回源域名')}</th><th>{t('优选 IP')}</th><th>{t('使用入站')}</th><th></th></tr></thead>
                  <tbody>
                    {(cdn.data ?? []).map((p) => (
                      <tr key={p.id} className="clickable" onClick={() => setCdnSel(p.id)}>
                        <td style={{ fontWeight: 500 }}>{p.name}</td><td>{p.provider}</td><td className="mono">{p.originDomain}</td>
                        <td className="mono">{p.preferredIps.length}</td><td className="mono">{p.inbounds}</td>
                        <td style={{ textAlign: 'right' }} onClick={(e) => e.stopPropagation()}>
                          <button className="link" type="button" onClick={() => setCdnDrawer(p)}>{t('编辑')}</button>
                          <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                            if (await confirm(t('删除配置档 {name}？', { name: p.name }), { danger: true })) run(async () => { await del('/cdn-profiles/' + p.id); cdn.reload() })
                          }}>{t('删除')}</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
            <div className="note">{t('给入站添加 CDN 入口：在「入站」页打开入站 → 入口 → CDN。Cloudflare 的 gRPC 需在控制台开启。经 CDN 后内核读取 CF-Connecting-IP；不支持时该入站跳过 IP 数限制。')}</div>
          </div>
          <aside className="aside">{selCDN && <CDNAside p={selCDN} onEdit={() => setCdnDrawer(selCDN)} onChanged={cdn.reload} />}</aside>
        </div>
      )}
      {tab === 'routing' && (nodes.length ? <Routing nodes={nodes} /> : <div className="card"><Empty>{t('还没有节点')}</Empty></div>)}
      {tab === 'rulesets' && <RuleSets />}
      {tab === 'dns' && <DNSSettings nodes={nodes} />}
      {chainDrawer && <ChainDrawer chain={chainDrawer === 'new' ? undefined : chainDrawer} nodes={nodes} inbounds={inbounds.data ?? []} onClose={() => setChainDrawer(null)} onSaved={() => { setChainDrawer(null); chains.reload() }} />}
      {cdnDrawer && <CDNDrawer profile={cdnDrawer === 'new' ? undefined : cdnDrawer} onClose={() => setCdnDrawer(null)} onSaved={() => { setCdnDrawer(null); cdn.reload() }} />}
    </>
  )
}
