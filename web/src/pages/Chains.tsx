import { useMemo, useState } from 'react'
import { api, API, ApiError, del, get, post, put } from '../api'
import { useNodes } from '../App'
import { copyText, Drawer, Empty, Field, Modal, Switch, fromLines, toLines, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, coreShort, inboundSummary, protoLabel } from '../format'
import { t, tr } from '../i18n'
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
    nodeCheck: profile?.nodeCheck ?? false, staleHours: String(profile?.staleHours || 24), carrierSplit: profile?.carrierSplit ?? false,
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
        <Field label={t('回源域名')} full hint={t('一个 Cloudflare 域名建一个配置档就够了：多台节点共用时，第二台起的入口自动用「节点名-域名」（如 oracle-sg-cdn.example.com），在下方「服务节点」里能看到每台节点用哪个域名')}><input className="input mono" value={f.originDomain} onChange={(e) => set('originDomain', e.target.value)} placeholder="sub.example.com" /></Field>
        <Field label={t('TLS 模式')}>
          <select className="input" value={f.tlsMode} onChange={(e) => set('tlsMode', e.target.value)}><option value="full_strict">Full (strict)</option><option value="full">Full</option><option value="flexible">Flexible</option></select>
        </Field>
        {f.provider === 'cloudflare' && <div className="hint" style={{ gridColumn: '1 / -1' }}>{
          f.tlsMode === 'full_strict' ? t('Full (strict)：Cloudflare 校验节点证书，证书必须包含回源域名（或用「同步 DNS」签发的 Origin 证书），否则报 526。')
            : f.tlsMode === 'full' ? t('Full：Cloudflare 用 HTTPS 回源但不校验证书，节点上任意证书都行，入站必须开 TLS。')
              : t('Flexible：Cloudflare 用明文 HTTP 回源，入站不能开 TLS，也不能用 Caddy 统一回源；一般不推荐。')}
          {' '}{t('这里的设置要和 Cloudflare 后台「SSL/TLS」一致：点「同步 DNS」会替你改（对整个域名生效）。')}</div>}
        <Field label={t('可用端口')} hint={f.provider === 'cloudflare' ? t('只能用 Cloudflare 的 HTTPS 端口：443、2053、2083、2087、2096、8443；第一个用于不在这些端口上的入站和 Caddy 统一回源') : undefined}><input className="input mono" value={f.ports} onChange={(e) => set('ports', e.target.value)} /></Field>
        {profile?.tokenSource === 'profile' && <Field label="Cloudflare API Token" full hint={t('这个配置档有单独保存的 Token（旧版本填写），留空保持不变。新配置档统一用「CDN 配置档」页的 CDN Token')}>
          <input className="input mono" type="password" value={f.apiToken} onChange={(e) => set('apiToken', e.target.value)} autoComplete="off" />
        </Field>}
        <Field label={t('优选 IP 来源 URL')} full hint={t('每 6 小时拉取；每行一个 IP 或域名，支持 ip:port / CSV')}><input className="input mono" value={f.ipSourceUrl} onChange={(e) => set('ipSourceUrl', e.target.value)} /></Field>
        <Field label={t('优选 IP（手填）')} full hint={t('一行一个 IP 或优选域名（如 visa.cn），不带端口。手填的域名会一直保留：测速点和来源 URL 只更新 IP，订阅里域名排在前 N 个 IP 之后')}><textarea className="input" style={{ minHeight: 80 }} value={f.preferredIps} onChange={(e) => set('preferredIps', e.target.value)} /></Field>
        <Field label={t('订阅取前 N 个')} hint={t('0 为全部')}><input className="input mono" value={f.ipTopN} onChange={(e) => set('ipTopN', e.target.value)} /></Field>
        <Field label={t('测速点多久没上报就告警（小时）')} hint={t('只在有测速点上报过时生效；告警后继续用原有 IP')}><input className="input mono" value={f.staleHours} onChange={(e) => set('staleHours', e.target.value)} /></Field>
      </div>
      <div className="toggle"><span>{t('节点检测：每 30 分钟由在线节点连接各优选 IP，所有节点都连不上的不放进订阅')}</span><Switch label={t('节点检测')} on={f.nodeCheck} onChange={(v) => set('nodeCheck', v)} /></div>
      <div className="toggle"><span>{t('按运营商分配：电信、联通、移动用户各自拿到同运营商测速点上报的 IP；识别不出运营商时用合并后的列表')}</span><Switch label={t('按运营商分配')} on={f.carrierSplit} onChange={(v) => set('carrierSplit', v)} /></div>
      <div className="toggle"><span>{t('订阅按优选 IP 展开（一个 IP 一个节点）')}</span><Switch label={t('展开')} on={f.expandPerIp} onChange={(v) => set('expandPerIp', v)} /></div>
      <div className="toggle"><span>{t('自动同步 DNS 记录（开启代理）：需要 API Token；新加 CDN 入口时和每 6 小时把记录指向节点 IP，不改 SSL 模式和证书')}</span><Switch label="DNS" on={f.autoDns} onChange={(v) => set('autoDns', v)} /></div>
      <div className="toggle"><span>{t('Agent 内置 Caddy 统一回源（多个 WS/gRPC/HTTPUpgrade 入站共享第一个端口，按路径分流；入站需开 TLS；该端口被其他入站占用时不生效）')}</span><Switch label="Caddy" on={f.caddyFronting} onChange={(v) => set('caddyFronting', v)} /></div>
    </Drawer>
  )
}

function CDNAside({ p, onEdit, onDelete, onChanged }: { p: CDNProfile; onEdit: () => void; onDelete: () => void; onChanged: () => void }) {
  const { busy, run } = useAction()
  const [synced, setSynced] = useState<{ records: string[]; sslMode?: string; cert?: string; split?: string[] } | null>(null)
  const confirm = useConfirm()
  const reported = new Set(p.reports.map((r) => r.source))
  const waiting = (p.probes ?? []).filter((w) => !reported.has(w.source))
  const paired = new Set((p.probes ?? []).map((w) => w.source))
  const remove = async (source: string) => {
    const msg = paired.has(source) ? t('移除测速点 {name}？它上报的 IP 会从列表中去掉，专用令牌同时作废，那台机器上的程序之后会上报失败。', { name: source }) : t('移除测速点 {name}？它上报的 IP 会从列表中去掉。', { name: source })
    if (await confirm(msg, { danger: true, ok: t('移除') })) run(async () => { await del(`/cdn-profiles/${p.id}/ip-reports/${encodeURIComponent(source)}`); onChanged() }, t('已移除'))
  }
  return (
    <>
      <section className="card">
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('CDN 配置档 · {name}', { name: p.name })}</h2><span className="tag" style={{ margin: 0 }}>{p.provider === 'cloudflare' ? 'Cloudflare' : t('其他')}</span></div>
        <dl className="kv" style={{ margin: '16px 0 0' }}>
          <dt>{t('回源域名')}</dt><dd className="mono">{p.originDomain}</dd>
          <dt>{t('TLS 模式')}</dt><dd>{p.tlsMode === 'full_strict' ? 'Full (strict)' : p.tlsMode}</dd>
          <dt>{t('可用端口')}</dt><dd>{p.ports.map((x) => <span key={x} className="tag">{x}</span>)}</dd>
          <dt>{t('服务节点')}</dt><dd>{(p.serves ?? []).length === 0 ? <span className="sub">{t('还没有入站使用：在「入站」里给入站添加这个配置档的 CDN 入口')}</span> : (p.serves ?? []).map((x) => (
            <div key={x.node + x.tag} className="small"><b>{x.node}</b> · {x.tag} → <span className="mono">{x.host}</span></div>))}</dd>
          <dt>API Token</dt><dd>{({ profile: t('配置档单独的 Token'), cdn: t('CDN Token'), dns: t('系统设置的 DNS Token（只能写 DNS 记录，同步 DNS 改 SSL、签证书需要 CDN Token）') } as Record<string, string>)[p.tokenSource] ?? t('未配置（仅生成订阅）')}</dd>
        </dl>
        {p.lastError && <div className="note bad" style={{ marginTop: 12 }}>{p.lastError}</div>}
        <div className="row" style={{ marginTop: 16 }}>
          <button className="btn sm" type="button" onClick={onEdit}>{t('编辑')}</button>
          <button className="btn sm" type="button" disabled={busy || !p.hasToken} onClick={() => run(async () => {
            setSynced(await post<{ records: string[]; sslMode?: string; cert?: string; split?: string[] }>(`/cdn-profiles/${p.id}/sync-dns`)); onChanged()
          }, t('DNS 已同步'))}>{t('同步 DNS')}</button>
          <button className="btn sm danger" type="button" style={{ marginLeft: 'auto' }} disabled={busy} onClick={onDelete}>{t('删除')}</button>
        </div>
        {synced && <div className="note small" style={{ marginTop: 12, overflowWrap: 'anywhere' }}>
          {t('DNS 记录：{list}', { list: synced.records.join('；') || t('无（还没有入站用这个配置档）') })}
          {(synced.split ?? []).length > 0 && <div>{t('多台节点共用了回源域名，已给这些入口换成各自的域名：{list}', { list: synced.split!.join('；') })}</div>}
          {synced.sslMode && <div>{t('Cloudflare SSL 模式已设为 {mode}（对整个域名生效）', { mode: synced.sslMode })}</div>}
          {synced.cert && <div>{tr(synced.cert)}</div>}
        </div>}
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
        {p.reports.length === 0 && waiting.length === 0 ? <div className="sub">{t('还没有测速点上报。在用户所在地区的机器上安装测速程序（见下方「添加测速点」），或用带「上报 CDN 优选 IP」权限的 API 令牌调用 POST /cdn-profiles/{id}/preferred-ips。', { id: p.id })}</div> : (
          <div style={{ marginTop: 10 }}>{p.reports.map((r) => {
            const stale = Date.now() - new Date(r.at).getTime() > (p.staleHours || 24) * 3600e3
            const best = r.ips[0]
            return (
              <div key={r.source} className="row" style={{ flexWrap: 'nowrap', justifyContent: 'space-between', alignItems: 'flex-start', gap: 8, padding: '8px 0', borderTop: '1px solid var(--line)' }}>
                <div style={{ minWidth: 0 }}>
                  <div><b>{r.source}</b>{r.carrier && <span className="tag" style={{ marginLeft: 6 }}>{carrierLabel(r.carrier)}</span>} <span className="sub" style={stale ? { color: 'var(--bad)' } : undefined}>· {ago(r.at)}</span></div>
                  <div className="mono small" style={{ overflowWrap: 'anywhere' }}>{best ? `${best.ip}${best.colo ? ` · ${best.colo}` : ''} · ${Math.round(best.latencyMs)} ms${best.speed ? ` · ${best.speed.toFixed(1)} MB/s` : ''}${best.loss > 0 ? ` · ${t('丢包')} ${Math.round(best.loss * 100)}%` : ''}` : '—'} <span className="sub">{t('共 {n} 个', { n: r.ips.length })}</span></div>
                </div>
                <button className="btn sm" type="button" disabled={busy} onClick={() => remove(r.source)}>{t('移除')}</button>
              </div>
            )
          })}
          {waiting.map((w) => (
            <div key={'w-' + w.source} className="row" style={{ flexWrap: 'nowrap', justifyContent: 'space-between', alignItems: 'flex-start', gap: 8, padding: '8px 0', borderTop: '1px solid var(--line)' }}>
              <div style={{ minWidth: 0 }}>
                <div><b>{w.source}</b> <span className="tag warn" style={{ marginLeft: 6 }}>{t('等待首次上报')}</span></div>
                <div className="sub">{t('{when}生成安装命令', { when: ago(w.createdAt) })}{w.lastUsedAt ? ' · ' + t('最近连接 {when}', { when: ago(w.lastUsedAt) }) : ''}</div>
              </div>
              <button className="btn sm" type="button" disabled={busy} onClick={() => remove(w.source)}>{t('移除')}</button>
            </div>
          ))}</div>
        )}
        <ProbeInstall profile={p} onPaired={onChanged} />
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
  const toast = useToast()
  const { run } = useAction()
  // removeProfile deletes a profile; one in use is confirmed again with
  // the inbounds it would change.
  const removeProfile = async (prof: CDNProfile) => {
    if (!await confirm(t('删除配置档 {name}？', { name: prof.name }), { danger: true })) return
    try {
      await del('/cdn-profiles/' + prof.id)
      cdn.reload()
    } catch (e) {
      const usage = (e instanceof ApiError && e.status === 409 ? e.data?.usage : null) as { tag: string; node: string; cdnOnly: boolean }[] | null
      if (!usage) { toast(String((e as Error).message), true); return }
      const only = usage.filter((u) => u.cdnOnly).map((u) => `${u.node} / ${u.tag}`)
      const msg = t('{n} 个入站在用这个配置档：{list}。继续删除会同时删掉它们的 CDN 入口。', { n: usage.length, list: usage.map((u) => `${u.node} / ${u.tag}`).join('、') })
        + (only.length ? ' ' + t('其中这些入站只有 CDN 入口，会被停用（以免订阅改为直连、暴露节点 IP）：{list}', { list: only.join('、') }) : '')
      if (await confirm(msg, { danger: true, ok: t('仍然删除') })) run(async () => {
        const r = await del<{ disabled: string[] }>('/cdn-profiles/' + prof.id + '?force=1')
        cdn.reload()
        if (r.disabled?.length) toast(t('已删除，停用了 {n} 个入站', { n: r.disabled.length }))
      }, t('已删除'))
    }
  }
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
            {selCDN ? <CDNAside p={selCDN} onEdit={() => setCdnDrawer(selCDN)} onDelete={() => removeProfile(selCDN)} onChanged={cdn.reload} /> : <div className="card"><Empty>{t('还没有 CDN 配置档')}</Empty></div>}
            <section className="note">{t('REALITY、Hysteria2、TUIC 不能走 CDN；添加 CDN 入口时只列出 WS / gRPC / HTTPUpgrade / XHTTP 入站。')}</section>
          </aside>
        </div>
      )}
      {tab === 'cdn' && (
        <div className="split">
          <div className="grow">
            <CFTokenCard onSaved={cdn.reload} />
            <div className="card flush scroll">
              {cdn.data?.length === 0 ? <Empty>{t('还没有 CDN 配置档')}</Empty> : (
                <table className="rtable">
                  <thead><tr><th>{t('名称')}</th><th>{t('服务商')}</th><th>{t('回源域名')}</th><th>{t('优选 IP')}</th><th>{t('服务节点')}</th><th></th></tr></thead>
                  <tbody>
                    {(cdn.data ?? []).map((p) => (
                      <tr key={p.id} className="clickable" onClick={() => setCdnSel(p.id)}>
                        <td className="rt-head" style={{ fontWeight: 500 }}>{p.name}</td><td data-label={t('服务商')}>{p.provider}</td><td data-label={t('回源域名')} className="mono rt-break">{p.originDomain}</td>
                        <td data-label={t('优选 IP')} className="mono">{p.preferredIps.length}</td><td data-label={t('服务节点')} className="small">{(p.serves ?? []).length === 0 ? <span className="sub">{t('未使用')}</span> : <div>{(p.serves ?? []).map((x) => <div key={x.node + x.tag}>{x.node} → <span className="mono">{x.host}</span></div>)}</div>}</td>
                        <td className="rt-actions" style={{ textAlign: 'right' }} onClick={(e) => e.stopPropagation()}>
                          <button className="link" type="button" onClick={() => setCdnDrawer(p)}>{t('编辑')}</button>
                          <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={() => removeProfile(p)}>{t('删除')}</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
            <div className="note">{t('给入站添加 CDN 入口：在「入站」页打开入站 → 入口 → CDN。Cloudflare 的 gRPC 需在控制台开启。经 CDN 后内核读取 CF-Connecting-IP；不支持时该入站跳过 IP 数限制。')}</div>
          </div>
          <aside className="aside">{selCDN && <CDNAside p={selCDN} onEdit={() => setCdnDrawer(selCDN)} onDelete={() => removeProfile(selCDN)} onChanged={cdn.reload} />}</aside>
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

// ProbeInstall pairs a cf-ip-probe speed-test point: the panel creates a
// token that may only report for this profile under this name, and shows the
// complete install command once.
function ProbeInstall({ profile: p, onPaired }: { profile: CDNProfile; onPaired: () => void }) {
  const [source, setSource] = useState('')
  const [carrier, setCarrier] = useState('')
  const [made, setMade] = useState<{ source: string; token: string; mirror: string } | null>(null)
  const { busy, run } = useAction()
  const toast = useToast()
  const apiUrl = new URL(API, window.location.href).href
  const script = 'https://github.com/cuijianzhuang/polaris-plugins/releases/latest/download/install.sh'
  const q = (v: string) => /^[A-Za-z0-9._:/@-]+$/.test(v) ? v : `'${v.replace(/'/g, `'\\''`)}'`
  const cmd = made && [
    `curl -fsSL ${made.mirror ? made.mirror + '/' : ''}${script} | sudo sh -s --`,
    `--api ${q(apiUrl)} --profile ${p.id} --host ${q(p.originDomain)}`,
    `--token ${made.token} --source ${q(made.source)}`,
    carrier ? `--carrier ${carrier}` : '',
    made.mirror ? `--mirror ${q(made.mirror + '/')}` : '',
  ].filter(Boolean).join(' ')
  const manual = `curl -fsSL ${script} | sudo sh -s -- --api ${apiUrl} --profile ${p.id} --host ${p.originDomain} --token <${t('令牌')}> --source <${t('测速点名称')}>`
  return (
    <details style={{ marginTop: 10 }} open={!!made}>
      <summary className="sub" style={{ cursor: 'pointer' }}>{t('添加测速点（一键安装 cf-ip-probe）')}</summary>
      <div className="sub" style={{ margin: '8px 0' }}>{t('填写测速点名称，面板会生成一个只能给这个配置档、以这个名称上报的专用令牌，并给出完整的安装命令。在用户所在地区的 Linux 机器（国内 VPS、树莓派、OpenWrt 路由器）上以 root 运行；OpenWrt 上去掉 sudo。')}</div>
      {!made ? (
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <input className="input" style={{ flex: '1 1 160px', minWidth: 0 }} value={source} maxLength={40} placeholder={t('测速点名称，例如 上海电信')} aria-label={t('测速点名称')} onChange={(e) => setSource(e.target.value)} />
          <select className="input" style={{ flex: '0 0 auto', width: 'auto' }} value={carrier} aria-label={t('运营商')} onChange={(e) => setCarrier(e.target.value)}>
            <option value="">{t('运营商：从名称识别')}</option>
            <option value="ct">{t('电信')}</option><option value="cu">{t('联通')}</option><option value="cm">{t('移动')}</option>
          </select>
          <button className="btn sm pri" type="button" disabled={busy || !source.trim()} onClick={() => run(async () => {
            const r = await post<{ source: string; token: string; mirror: string }>(`/cdn-profiles/${p.id}/probes`, { source: source.trim() })
            setMade(r); onPaired()
          })}>{t('生成安装命令')}</button>
        </div>
      ) : (
        <div style={{ display: 'grid', gap: 8 }}>
          <div className="note warn small">{t('命令里含有专用令牌，只显示这一次。再次生成同名测速点会让旧令牌失效。')}</div>
          <div className="row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}><code className="small" style={{ flex: 1, wordBreak: 'break-all' }}>{cmd}</code><button className="btn sm" type="button" onClick={async () => { await copyText(cmd!); toast(t('已复制')) }}>{t('复制')}</button></div>
          <div><button className="btn sm" type="button" onClick={() => { setMade(null); setSource(''); setCarrier('') }}>{t('再添加一个')}</button></div>
        </div>
      )}
      <details style={{ marginTop: 8 }}>
        <summary className="hint" style={{ cursor: 'pointer' }}>{t('使用已有的 API 令牌')}</summary>
        <div className="row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start', marginTop: 6 }}><code className="small" style={{ flex: 1, wordBreak: 'break-all' }}>{manual}</code><button className="btn sm" type="button" onClick={() => copyText(manual)}>{t('复制')}</button></div>
      </details>
    </details>
  )
}

const carrierLabel = (c: string) => ({ ct: t('电信'), cu: t('联通'), cm: t('移动') } as Record<string, string>)[c] ?? c

// CFTokenCard sets the CDN token (cdn_cf_token): the Cloudflare token every
// CDN profile uses. It needs more permissions than the DNS token in 系统设置.
function CFTokenCard({ onSaved }: { onSaved: () => void }) {
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const { busy, run } = useAction()
  const [editing, setEditing] = useState(false)
  const [token, setToken] = useState('')
  const set = !!settings.data?.cdn_cf_token_set
  const dnsOnly = !set && !!settings.data?.acme_cf_token_set
  const save = () => run(async () => {
    await put('/settings', { cdn_cf_token: token.trim() })
    setToken(''); setEditing(false); settings.reload(); onSaved()
  }, t('已保存'))
  return (
    <section className="card" style={{ marginBottom: 16 }}>
      <div className="row" style={{ justifyContent: 'space-between', flexWrap: 'wrap', gap: 8 }}>
        <div>
          <b>{t('Cloudflare CDN Token')}</b>
          <div className="sub small">{set ? t('已配置：所有 CDN 配置档共用') : dnsOnly ? t('未配置：暂用「系统设置」的 DNS Token，只能写 DNS 记录；同步 DNS 改 SSL 模式、签 Origin 证书需要在这里填 CDN Token') : t('未配置：填一次，所有 CDN 配置档共用')}</div>
        </div>
        {!editing && <button className="btn sm" type="button" onClick={() => setEditing(true)}>{set ? t('更换') : t('填写')}</button>}
      </div>
      {editing && (
        <div style={{ marginTop: 12 }}>
          <input className="input mono" type="password" autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} placeholder={t('粘贴 Cloudflare API Token')} aria-label="Cloudflare API Token" />
          <div className="sub small" style={{ marginTop: 6 }}>{t('权限：Zone.Zone 读取、Zone.DNS 编辑、Zone.Zone Settings 编辑、Zone.SSL and Certificates 编辑。用于同步 DNS 记录、设置 SSL 模式、签发 Origin 证书')}</div>
          <div className="row" style={{ marginTop: 10, gap: 8 }}>
            <button className="btn sm pri" type="button" disabled={busy || !token.trim()} onClick={save}>{t('保存')}</button>
            <button className="btn sm" type="button" onClick={() => { setEditing(false); setToken('') }}>{t('取消')}</button>
          </div>
        </div>
      )}
    </section>
  )
}
