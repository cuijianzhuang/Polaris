import SnellSetup from '../components/SnellSetup'
import OutboundIPField from '../components/OutboundIPField'
import RelayManager, { RelayAccessFields } from '../components/RelayManager'
import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Icon } from '../components/Icons'
import { CloneModal, ImportInboundModal } from '../components/InboundTools'
import { downloadJSON } from '../components/UserTools'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import InboundFields, { normalizeDraft, type InboundDraft } from '../components/InboundFields'
import { Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, useToast, Section, useUndo } from '../components/ui'
import { bytes, coreLabel, coreShort, coreTagClass, inboundSummary, portLabel } from '../format'
import type { TagTrafficRow } from '../components/TagTraffic'
import type { CapSet, CDNCheck, CDNProfile, Certificate, Chain, CoreType, Endpoint, Fallback, Group, Inbound, InboundHealth, Node, Outbound } from '../types'
import { t, tr } from '../i18n'

const emptyDraft = (): InboundDraft => ({
  core: 'xray',
  protocol: { type: 'vless', flow: 'xtls-rprx-vision' },
  transport: { type: 'tcp' },
  security: { type: 'reality', realityDest: 'www.microsoft.com:443' },
  sniffing: { enabled: true },
})

function Endpoints({ inbound, onChanged }: { inbound: Inbound; onChanged: () => void }) {
  const eps = useLoad(() => get<Endpoint[]>(`/inbounds/${inbound.id}/endpoints`), [inbound.id])
  const cdn = useLoad(() => get<CDNProfile[]>('/cdn-profiles'))
  const chains = useLoad(() => get<Chain[]>('/chains'))
  const [kind, setKind] = useState<'direct' | 'cdn' | 'relay'>(inbound.cdnCapable ? 'cdn' : 'direct')
  const [form, setForm] = useState({ cdnProfileId: '', relayChainId: '', address: '', port: '', sni: '', host: '', path: '', remark: '' })
  const { busy, run } = useAction()
  // The form below adds another entry; existing ones are listed above it.
  const [adding, setAdding] = useState(false)
  useEffect(() => { if (eps.data && eps.data.length === 0) setAdding(true) }, [eps.data])
  const firstProfile = cdn.data?.[0]?.id ?? ''
  useEffect(() => { if (!form.cdnProfileId && firstProfile) setForm((f) => ({ ...f, cdnProfileId: firstProfile })) }, [firstProfile, form.cdnProfileId])
  const add = () => run(async () => {
    await post(`/inbounds/${inbound.id}/endpoints`, { kind, ...form, port: Number(form.port) || 0 })
    setForm({ cdnProfileId: firstProfile, relayChainId: '', address: '', port: '', sni: '', host: '', path: '', remark: '' })
    setAdding(false)
    eps.reload()
    onChanged()
  }, t('入口已添加'))
  const [checks, setChecks] = useState<Record<string, CDNCheck>>({})
  const [checking, setChecking] = useState('')
  const check = async (e: Endpoint) => {
    setChecking(e.id)
    try {
      const r = await post<CDNCheck>(`/inbounds/${inbound.id}/endpoints/${e.id}/check`)
      setChecks((c) => ({ ...c, [e.id]: r }))
    } catch (err) {
      setChecks((c) => ({ ...c, [e.id]: { ok: false, message: String((err as Error)?.message ?? err), steps: [] } }))
    } finally { setChecking('') }
  }
  const label = (e: Endpoint) => e.kind === 'direct' ? t('直连') : e.kind === 'cdn' ? 'CDN · ' + (cdn.data?.find((c) => c.id === e.cdnProfileId)?.name ?? e.cdnProfileId) : t('中转') + ' · ' + (chains.data?.find((c) => c.id === e.relayChainId)?.name ?? e.relayChainId)
  return (
    <section className="field">
      <span className="lbl">{t('入口（订阅里每个入口生成一个节点）')}</span>
      <div className="card flush">
        {(eps.data ?? []).map((e, i) => (
          <div key={e.id} style={{ padding: '12px 14px', borderTop: i ? '1px solid var(--line-2)' : undefined, display: 'grid', gap: 6, minWidth: 0 }}>
            <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
              <b style={{ flex: '1 1 auto', minWidth: 0, overflowWrap: 'anywhere' }}>{label(e)}</b>
              <span className="row" style={{ gap: 6, flex: 'none' }}>
                {e.kind === 'cdn' && <button className="btn sm" type="button" disabled={checking === e.id} onClick={() => check(e)}>{checking === e.id ? t('检测中…') : t('检测')}</button>}
                <button className="btn sm danger" type="button" onClick={() => run(async () => { await del(`/inbounds/${inbound.id}/endpoints/${e.id}`); eps.reload() })}>{t('删除')}</button>
              </span>
            </div>
            <div className="mono small mute" style={{ overflowWrap: 'anywhere' }} title={e.target ? t('客户端经 CDN 连接的地址；SNI / Host / 路径') : undefined}>{e.target
              ? <>{e.target.server}:{e.target.port} · {e.target.sni === e.target.host ? e.target.sni : `${e.target.sni} / ${e.target.host}`} · {e.target.path}</>
              : [e.address, e.port || '', e.host].filter(Boolean).join(' · ') || t('默认')}</div>
            {(e.warnings ?? []).map((w, j) => <div key={j} className="hint" style={{ color: 'var(--warn)', overflowWrap: 'anywhere' }}>{tr(w)}</div>)}
            {checks[e.id] && <CheckResult r={checks[e.id]} />}
          </div>
        ))}
        {eps.data?.length === 0 && <div className="mute" style={{ padding: '12px 14px' }}>{t('无入口：订阅按直连生成')}</div>}
      </div>
      {!adding && <div><button className="btn sm" type="button" onClick={() => setAdding(true)}>{t('+ 添加入口')}</button></div>}
      {adding && <div className="card" style={{ display: 'grid', gap: 10, background: 'var(--soft)' }}>
      <div className="row" style={{ justifyContent: 'space-between' }}><b>{t('添加入口')}</b>
        {(eps.data?.length ?? 0) > 0 && <button className="link" type="button" onClick={() => setAdding(false)}>{t('收起')}</button>}</div>
      <div className="seg c3">
        {(['direct', 'cdn', 'relay'] as const).map((k) => (
          <button key={k} type="button" className={'opt' + (kind === k ? ' on' : '')} disabled={k === 'cdn' && !inbound.cdnCapable} onClick={() => setKind(k)}>
            {k === 'direct' ? t('直连') : k === 'cdn' ? 'CDN' : t('中转')}{k === 'cdn' && !inbound.cdnCapable && <small>{t('{combo} 不能走 CDN', { combo: cdnBlocker(inbound) })}</small>}
          </button>
        ))}
      </div>
      {!inbound.cdnCapable && <CDNSibling inbound={inbound} profiles={cdn.data ?? []} onDone={onChanged} />}
      <div className="form-grid">
        {kind === 'cdn' && (
          <Field label={t('CDN 配置档')} full>
            <select className="input" value={form.cdnProfileId} onChange={(e) => setForm({ ...form, cdnProfileId: e.target.value })}>
              <option value="">{t('选择配置档')}</option>
              {(cdn.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name} · {c.originDomain}</option>)}
            </select>
          </Field>
        )}
        {kind === 'relay' && (
          <Field label={t('中转链路')} full>
            <select className="input" value={form.relayChainId} onChange={(e) => setForm({ ...form, relayChainId: e.target.value })}>
              <option value="">{t('选择链路')}</option>
              {(chains.data ?? []).filter((c) => c.mode === 'l4').map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
            </select>
          </Field>
        )}
        <Field label={t('地址（可选）')}><input className="input mono" value={form.address} onChange={(e) => setForm({ ...form, address: e.target.value })} placeholder={t('覆盖默认地址')} /></Field>
        <Field label={t('端口（可选）')}><input className="input mono" value={form.port} onChange={(e) => setForm({ ...form, port: e.target.value })} /></Field>
        <Field label={t('Host（可选）')}><input className="input mono" value={form.host} onChange={(e) => setForm({ ...form, host: e.target.value })} /></Field>
        <Field label={t('备注')}><input className="input" value={form.remark} onChange={(e) => setForm({ ...form, remark: e.target.value })} /></Field>
      </div>
      <div><button className="btn sm pri" type="button" disabled={busy || (kind === 'cdn' && !form.cdnProfileId) || (kind === 'relay' && !form.relayChainId)} onClick={add}>{t('添加')}</button></div>
      </div>}
    </section>
  )
}

// CheckResult shows what the panel saw connecting like a CDN client.
function CheckResult({ r }: { r: CDNCheck }) {
  return (
    <div className={'note small ' + (r.ok ? 'ok-note' : 'bad')} style={{ marginTop: 6, overflowWrap: 'anywhere' }}>
      <b>{r.ok ? t('检测通过') : t('检测未通过')}</b>：{tr(r.message)}
      {r.steps.length > 0 && <div className="mono" style={{ marginTop: 4, opacity: 0.8 }}>{r.steps.map((x, i) => <div key={i}>{tr(x)}</div>)}</div>}
    </div>
  )
}

// Cloudflare proxies these HTTPS ports and reaches the origin on the same
// one; any other port goes out on the profile's first port (see cdnNodePort).
const cloudflarePorts = [443, 2053, 2083, 2087, 2096, 8443]
const cdnClientPort = (p: CDNProfile, port: number) => (p.caddyFronting || !cloudflarePorts.includes(port)) ? (p.ports[0] ?? 443) : port

// NewCDNEntry publishes a new inbound through a CDN profile as it is created.
function NewCDNEntry({ value, onChange, grpc, tls, port }: { value: { profileId: string; only: boolean }; onChange: (v: { profileId: string; only: boolean }) => void; grpc: boolean; tls: boolean; port: number }) {
  const cdn = useLoad(() => get<CDNProfile[]>('/cdn-profiles'))
  const p = cdn.data?.find((c) => c.id === value.profileId)
  const clientPort = p ? cdnClientPort(p, port) : 0
  return (
    <Field label={t('CDN 入口')} hint={t('可选：创建时同时加一个 CDN 入口，订阅里多出一个经 CDN 的节点。之后也可以在编辑里添加。')}>
      <select className="input" value={value.profileId} onChange={(e) => onChange({ ...value, profileId: e.target.value })}>
        <option value="">{t('不走 CDN')}</option>
        {(cdn.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name} · {c.originDomain}</option>)}
      </select>
      {cdn.data?.length === 0 && <div className="hint">{t('还没有 CDN 配置档，先在「CDN」页添加。')}</div>}
      {p && <Switch on={value.only} onChange={(v) => onChange({ ...value, only: v })} label={t('只走 CDN（订阅里不出现直连节点，不暴露节点 IP）')} />}
      {p && <div className="hint">{t('客户端经 CDN 连 {domain}:{port}', { domain: p.originDomain, port: clientPort })}</div>}
      {p && p.provider === 'cloudflare' && p.tlsMode === 'flexible' && tls && <div className="note warn small">{t('配置档的 SSL 模式是 Flexible：Cloudflare 用明文 HTTP 回源，开了 TLS 的入站接不上。把配置档改为 Full。')}</div>}
      {p && p.provider === 'cloudflare' && p.tlsMode !== 'flexible' && !tls && <div className="note warn small">{t('配置档的 SSL 模式是 Full：Cloudflare 用 HTTPS 回源，这个入站需要开启 TLS。')}</div>}
      {p && p.provider === 'cloudflare' && grpc && clientPort !== 443 && <div className="note warn small">{t('Cloudflare 只在 443 端口代理 gRPC：把入站端口改为 443、开启配置档的「Caddy 统一回源」，或改用 WS。')}</div>}
      {p && p.provider === 'cloudflare' && (p.tlsMode || 'full_strict') === 'full_strict' && <div className="hint">{t('配置档的 SSL 模式是 Full (strict)：入站证书需要包含 {domain}，否则 Cloudflare 报 526。可在配置档点「同步 DNS」换成 Origin 证书，或改用 Full。', { domain: p.originDomain })}</div>}
    </Field>
  )
}

// cdnBlocker names what keeps an inbound off a CDN, e.g. "TCP + REALITY".
const cdnBlocker = (i: Inbound) => {
  if (i.security.type === 'reality') return `${(i.transport.type || 'tcp').toUpperCase()} + REALITY`
  if (i.network === 'udp') return inboundSummary(i).split(' · ')[0]
  return (i.transport.type || 'tcp').toUpperCase()
}

/** CDNSibling explains why this inbound cannot use a CDN and creates a
 * VLESS + WS + TLS inbound on the same node that can. */
function CDNSibling({ inbound, profiles, onDone }: { inbound: Inbound; profiles: CDNProfile[]; onDone: () => void }) {
  const [profile, setProfile] = useState('')
  const [made, setMade] = useState<{ tag: string; port: number; profile: string; needSync: boolean; addedPort: boolean } | null>(null)
  const { busy, run } = useAction()
  const pid = profile || (profiles.length === 1 ? profiles[0].id : '')
  return (
    <div className="note" style={{ display: 'grid', gap: 8, margin: '10px 0' }}>
      <div>{t('这个入站是 {combo}，不能走 CDN：CDN 只转发 WS、gRPC、HTTPUpgrade、XHTTP 传输，而且不能用 REALITY。可以在同一节点新建一个 VLESS + WS + TLS 入站专门走 CDN：端口自动选一个没被占用、CDN 支持的端口，用户分组和本入站相同，订阅里只出现 CDN 节点；本入站不受影响。', { combo: cdnBlocker(inbound) })}</div>
      {made ? (
        <div className="small">
          {t('已新建入站 {tag}，端口 {port}，走 CDN 配置档 {profile}，正在下发。', { tag: made.tag, port: made.port, profile: made.profile })}
          {made.addedPort && ' ' + t('端口 {port} 已加入配置档的可用端口。', { port: made.port })}
          {made.needSync ? ' ' + t('下一步：在「链路与路由 → CDN 配置档」点「同步 DNS」，自动建 DNS 记录并换上 Cloudflare Origin 证书。') : ' ' + t('下一步：在 Cloudflare 把回源域名解析到这个节点并开启代理（小云朵）；SSL 模式用 Full，或者给配置档填 API Token 后用「同步 DNS」换上 Origin 证书以使用 Full (strict)。')}
        </div>
      ) : profiles.length === 0 ? (
        <div className="small">{t('还没有 CDN 配置档，先在「链路与路由 → CDN 配置档」新建一个。')}</div>
      ) : (
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <select className="input" style={{ flex: '1 1 200px', minWidth: 0 }} value={pid} aria-label={t('CDN 配置档')} onChange={(e) => setProfile(e.target.value)}>
            <option value="">{t('选择配置档')}</option>
            {profiles.map((c) => <option key={c.id} value={c.id}>{c.name} · {c.originDomain}</option>)}
          </select>
          <button className="btn sm pri" type="button" disabled={busy || !pid} onClick={() => run(async () => {
            const r = await post<{ inbound: Inbound; port: number; profile: string; needSync: boolean; addedPort: boolean }>(`/inbounds/${inbound.id}/cdn-sibling`, { cdnProfileId: pid })
            setMade({ tag: r.inbound.tag, port: r.port, profile: r.profile, needSync: r.needSync, addedPort: r.addedPort }); onDone()
          }, t('已新建 CDN 入站'))}>{t('新建 CDN 入站')}</button>
        </div>
      )}
    </div>
  )
}

const loopback = (l?: string) => l === 'localhost' || /^127\./.test(l ?? '') || l === '::1'

/** Fallbacks: other protocols on this inbound's port, routed by path / ALPN. */
function FallbackEditor({ value, onChange, candidates, selfId }: { value: Fallback[]; onChange: (v: Fallback[]) => void; candidates: Inbound[]; selfId?: string }) {
  const usable = candidates.filter((i) => i.id !== selfId && i.security.type === 'none' && loopback(i.listen))
  const set = (i: number, p: Partial<Fallback>) => onChange(value.map((f, j) => (j === i ? { ...f, ...p } : f)))
  return (
    <section className="field">
      <span className="lbl">{t('回落（同一端口承载多个协议）')}</span>
      <div className="hint">{t('不是本入站协议的连接，按路径 / ALPN 转给子入站。子入站需在同一节点、监听 127.0.0.1、不开 TLS；客户端链接自动使用本入站的端口与 TLS。')}</div>
      {value.map((f, i) => (
        <div key={i} className="fb-row">
          <select className="input" value={f.inboundId} onChange={(e) => set(i, { inboundId: e.target.value })} aria-label={t('子入站')}>
            <option value="">{t('选择子入站')}</option>
            {usable.map((c) => <option key={c.id} value={c.id}>{c.tag} · {inboundSummary(c)} · :{c.port}</option>)}
          </select>
          <input className="input mono" value={f.path ?? ''} placeholder={t('路径，如 /ws（空 = 不限）')} onChange={(e) => set(i, { path: e.target.value || undefined })} aria-label={t('路径')} />
          <select className="input" value={f.alpn ?? ''} onChange={(e) => set(i, { alpn: e.target.value || undefined })} aria-label="ALPN">
            <option value="">{t('ALPN 不限')}</option><option value="h2">h2</option><option value="http/1.1">http/1.1</option>
          </select>
          <button className="icon-btn" type="button" aria-label={t('删除回落')} onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </div>
      ))}
      <div className="row">
        <button className="btn sm" type="button" disabled={!usable.length} onClick={() => onChange([...value, { inboundId: usable[0]?.id ?? '' }])}>{t('+ 添加回落')}</button>
        {!usable.length && <span className="hint">{t('该节点还没有可作回落目标的入站：新建一个监听 127.0.0.1、不开 TLS 的入站（如 VLESS-WS、Trojan）')}</span>}
      </div>
    </section>
  )
}

type BulkResult = { id: string; tag: string; node: string; port?: number; error?: string }

// BulkModal: change ports or the speed limit of the selected inbounds.
function BulkModal({ kind, ids, onClose, onDone }: { kind: 'port' | 'speed'; ids: string[]; onClose: () => void; onDone: () => void }) {
  const [mode, setMode] = useState<'sequential' | 'offset' | 'random'>('random')
  const [v, setV] = useState({ start: '30000', offset: '1', min: '20000', max: '60000', mbps: '' })
  const [res, setRes] = useState<BulkResult[] | null>(null)
  const { busy, run } = useAction()
  const num = (x: string) => Number(x) || 0
  const go = () => run(async () => {
    const body = kind === 'speed' ? { ids, action: 'speed', mbps: num(v.mbps) }
      : { ids, action: 'port', mode, start: num(v.start), offset: num(v.offset), min: num(v.min), max: num(v.max) }
    const r = await post<BulkResult[]>('/inbounds/bulk', body)
    setRes(r); onDone()
  })
  const failed = res?.filter((r) => r.error) ?? []
  return (
    <Modal title={kind === 'port' ? t('修改端口（{n} 个入站）', { n: ids.length }) : t('设置限速（{n} 个入站）', { n: ids.length })} onClose={onClose} footer={res
      ? <button className="btn pri" type="button" onClick={onClose}>{t('完成')}</button>
      : <><button className="btn" type="button" onClick={onClose}>{t('取消')}</button><button className="btn pri" type="button" disabled={busy} onClick={go}>{t('应用并下发')}</button></>}>
      {res ? (
        <div style={{ display: 'grid', gap: 6 }}>
          <div className={'note' + (failed.length ? ' warn' : '')}>{t('{n} 个已修改', { n: res.length - failed.length })}{failed.length ? t('，{n} 个未改（见下）', { n: failed.length }) : ''}{t('；节点会自动重新下发，防火墙端口随之更新。')}</div>
          {res.map((r) => <div key={r.id} className="row small" style={{ flexWrap: 'nowrap' }}>
            <span style={{ flex: 1, minWidth: 0 }}>{r.node} · {r.tag}</span>
            {r.error ? <span style={{ color: 'var(--bad)' }}>{r.error}</span> : r.port ? <span className="mono">→ {r.port}</span> : <span className="mute">✓</span>}
          </div>)}
        </div>
      ) : kind === 'speed' ? (
        <Field label={t('限速（Mbps）')} hint={t('每个入站端口各自的上下行上限，所有用户共享；0 或留空为不限速')}>
          <input className="input mono" inputMode="numeric" autoFocus placeholder={t('不限速')} value={v.mbps} onChange={(e) => setV({ ...v, mbps: e.target.value.replace(/\D/g, '') })} />
        </Field>
      ) : (
        <div style={{ display: 'grid', gap: 12 }}>
          <div className="seg c3">
            {([['random', t('随机'), t('在范围内随机换一个空闲端口，常用于端口被封')], ['sequential', t('顺序编号'), t('从起始端口开始依次分配')], ['offset', t('整体偏移'), t('每个端口加上同一个数')]] as const).map(([k, l, h]) => (
              <button key={k} type="button" className={'opt' + (mode === k ? ' on' : '')} onClick={() => setMode(k)}>{l}<small>{h}</small></button>
            ))}
          </div>
          {mode === 'random' && <div className="form-grid">
            <Field label={t('最小端口')}><input className="input mono" inputMode="numeric" value={v.min} onChange={(e) => setV({ ...v, min: e.target.value })} /></Field>
            <Field label={t('最大端口')}><input className="input mono" inputMode="numeric" value={v.max} onChange={(e) => setV({ ...v, max: e.target.value })} /></Field>
          </div>}
          {mode === 'sequential' && <Field label={t('起始端口')} hint={t('按列表顺序依次 +1')}><input className="input mono" inputMode="numeric" value={v.start} onChange={(e) => setV({ ...v, start: e.target.value })} /></Field>}
          {mode === 'offset' && <Field label={t('偏移量')} hint={t('可为负数，如 -1000')}><input className="input mono" value={v.offset} onChange={(e) => setV({ ...v, offset: e.target.value })} /></Field>}
          <div className="hint">{t('与同节点其他入站冲突的会跳过并列出原因。改端口后用户需要更新订阅（客户端自动更新时无需操作）。')}</div>
        </div>
      )}
    </Modal>
  )
}

// HealthDot: the panel's latest reachability probe of an inbound.
function HealthDot({ h }: { h?: InboundHealth }) {
  if (!h || !h.checkedAt) return null
  if (h.error && h.ok) return <span className="small mute" title={h.error} style={{ marginLeft: 6 }}>UDP</span>
  const tip = `${h.ok ? t('可连接，延迟 {ms} ms', { ms: h.latencyMs }) : t('无法连接：{err}（连续 {n} 次）', { err: h.error, n: h.fails })} · ${t('检查于 {time}', { time: new Date(h.checkedAt).toLocaleTimeString() })}`
  return <span title={tip} style={{ marginLeft: 6, whiteSpace: 'nowrap', fontSize: 12, color: h.ok ? 'var(--ok)' : 'var(--bad)' }}>● {h.ok ? `${h.latencyMs}ms` : t('不通')}</span>
}

function InboundDrawer({ nodes, editing, snellNode, onClose, onSaved, onDelete }: { nodes: Node[]; editing?: Inbound; snellNode?: string; onClose: () => void; onSaved: () => void; onDelete: (i: Inbound) => void }) {
  const [nodeId, setNodeId] = useState(editing?.nodeId ?? snellNode ?? nodes[0]?.id ?? '')
  const node = nodes.find((n) => n.id === nodeId)
  const caps = useLoad(() => (nodeId ? get<CapSet[]>('/capabilities?node=' + nodeId) : Promise.resolve([])), [nodeId])
  const certs = useLoad(() => get<Certificate[]>('/certificates'))
  const groups = useLoad(() => get<Group[]>('/groups'))
  const nodeInbounds = useLoad(() => (nodeId ? get<Inbound[]>('/inbounds?node=' + nodeId) : Promise.resolve([])), [nodeId])
  const [draft, setDraft] = useState<InboundDraft>(() => editing ? {
    core: editing.core, protocol: editing.protocol, transport: editing.transport, security: editing.security, sniffing: editing.sniffing,
  } : snellNode ? { ...emptyDraft(),core: 'singbox', protocol: {type:'snell',snellVersion:5,snellAuth:'psk'},transport:{type:'tcp'},security:{type:'none'} } : emptyDraft())
  const [tag, setTag] = useState(editing?.tag ?? '')
  const [port, setPort] = useState(String(editing?.port ?? 443))
  const [listen, setListen] = useState(editing?.listen && editing.listen !== '::' ? editing.listen : '')
  const [groupIds, setGroupIds] = useState<string[]>(editing?.groupIds ?? [])
  // New inbounds join the default group(s), so existing users can use them right away.
  useEffect(() => {
    if (!editing && groups.data) setGroupIds((x) => (x.length ? x : groups.data!.filter((g) => g.isDefault).map((g) => g.id)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [groups.data])
  const [extra, setExtra] = useState(editing?.extra ? JSON.stringify(editing.extra, null, 2) : '')
  const [enabled, setEnabled] = useState(editing?.enabled ?? true)
  const [remark, setRemark] = useState(editing?.remark ?? '')
  const [ipStrategy, setIPStrategy] = useState<import('../types').OutboundIPStrategy>(editing?.outboundIPStrategy || 'auto')
  const [relayOnly, setRelayOnly] = useState(!!editing?.relayOnly)
  const [relayAccess, setRelayAccess] = useState<NonNullable<Inbound['relayAccess']>>(editing?.relayAccess ?? { enabled: false, proxyProtocol: 0 })
  const [relaysOpen, setRelaysOpen] = useState(false)
  const [outboundTag, setOutboundTag] = useState(editing?.outboundTag ?? '')
  const [speedLimit, setSpeedLimit] = useState(editing?.speedLimitMbps ? String(editing.speedLimitMbps) : '')
  const [newCDN, setNewCDN] = useState({ profileId: '', only: false })
  const draftCDNCapable = draft.security.type !== 'reality' && ['ws', 'httpupgrade', 'grpc', 'xhttp'].includes(draft.transport.type)
  const outbounds = useLoad(() => (nodeId ? get<Outbound[]>(`/nodes/${nodeId}/outbounds`) : Promise.resolve([])), [nodeId])
  const [preview, setPreview] = useState('')
  const { busy, run } = useAction()
  const toast = useToast()

  const proxyCores = (caps.data ?? []).filter((c) => (c.combos?.length ?? 0) > 0)
  const coreCaps = proxyCores.filter((c) => c.core === draft.core)
  useEffect(() => {
    // Pick a valid core when the node changes.
    if (!caps.data || editing) return
    if (!proxyCores.find((c) => c.core === draft.core) && proxyCores[0]) {
      setDraft((d) => normalizeDraft({ ...d, core: proxyCores[0].core }, [proxyCores[0]]))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [caps.data])
  useEffect(() => {
    if (!editing && !tag) setTag(`${draft.protocol.type}-${draft.transport.type === 'tcp' ? draft.security.type : draft.transport.type}`)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft.protocol.type])

  const net = draft.protocol.type === 'hysteria2' || draft.protocol.type === 'tuic' || draft.transport.type === 'kcp' ? 'udp' : draft.protocol.type === 'shadowsocks' ? 'tcp,udp' : 'tcp'
  const conflict = (nodeInbounds.data ?? []).find((i) => i.id !== editing?.id && i.port === Number(port) && (i.network.split(',').some((n) => net.includes(n))))
  const coreHot = coreCaps[0]?.hotUsers
  const body = () => {
    let ex: any = undefined
    if (extra.trim()) ex = JSON.parse(extra)
    return { nodeId, core: draft.core, tag, port: Number(port), listen: listen || undefined, protocol: draft.protocol, transport: draft.transport,
      security: draft.security, sniffing: draft.sniffing, extra: ex, enabled, remark, groupIds, outboundTag, outboundIPStrategy: ipStrategy, relayOnly, relayAccess: { ...relayAccess, sourceOverrides: Object.fromEntries(Object.entries(relayAccess.sourceOverrides ?? {}).map(([id, sources]) => [id, sources.map((x) => x.trim()).filter(Boolean)])) }, speedLimitMbps: Math.max(0, Math.round(Number(speedLimit) || 0)),
      ...(!editing && draftCDNCapable && newCDN.profileId ? { cdnProfileId: newCDN.profileId, cdnOnly: newCDN.only } : {}) }
  }
  const save = () => run(async () => {
    if (editing) await put('/inbounds/' + editing.id, body())
    else await post('/inbounds', body())
    onSaved()
  }, editing ? t('已保存并下发') : t('入站已创建，正在下发'))

  return (
    <Drawer title={editing ? t('编辑入站 · {tag}', { tag: editing.tag }) : t('新建入站')} onClose={onClose} footer={<>
      {editing && <button className="btn danger" type="button" style={{ marginRight: 'auto' }} onClick={() => onDelete(editing)}>{t('删除')}</button>}
      {editing && <button className="btn" type="button" disabled={busy} title={t('从面板连接这个入站（TCP，TLS / REALITY 再握手）')} onClick={() => run(async () => {
        const h = await post<InboundHealth>(`/inbounds/${editing.id}/probe`)
        if (h.error && h.ok) toast(h.error)
        else if (h.ok) toast(t('可以连接，延迟 {ms} ms', { ms: h.latencyMs }))
        else toast(t('无法连接：{err}', { err: h.error }), true)
      })}>{t('探测')}</button>}
      <button className="btn" type="button" disabled={busy} onClick={() => run(async () => {
        const r = await post<{ config: string }>('/inbounds/preview' + (editing ? '?id=' + editing.id : ''), body())
        setPreview(r.config)
      })}>{t('预览内核配置')}</button>
      <button className="btn pri" type="button" disabled={busy || !tag || !nodeId || (draft.protocol.type === 'snell' && draft.protocol.snellAuth === 'psk' && !draft.protocol.snellUserId)} onClick={save}>{t('保存并下发')}</button>
    </>}>
      <Section step={1} title={t('基础')} summary={[node?.name, tag, port && ':' + port, enabled ? '' : t('已停用')].filter(Boolean).join(' · ')}>
        <Field label={t('节点')} htmlFor="ib-node">
          <select id="ib-node" className="input" value={nodeId} disabled={!!editing} onChange={(e) => setNodeId(e.target.value)}>
            {nodes.map((n) => <option key={n.id} value={n.id}>{n.name} · {t('已装 {cores}', { cores: n.cores.map((c) => `${coreShort[c.type]} v${c.version}`).join(' / ') || t('无内核') })}</option>)}
          </select>
        </Field>
        <div className="form-grid">
          <Field label={t('标签')} htmlFor="ib-tag"><input id="ib-tag" className="input mono" value={tag} onChange={(e) => setTag(e.target.value)} /></Field>
          <Field label={t('端口')} htmlFor="ib-port" hint={t('点「随机」选一个节点上未被占用的端口（含其他程序占用的端口）；保存后自动在节点防火墙放行')}>
            <div className="joined">
              <input id="ib-port" className="input mono" value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" />
              <button className="btn" type="button" disabled={!nodeId} onClick={() => run(async () => {
                const r = await get<{ port: number }>(`/nodes/${nodeId}/free-port`)
                if (r.port) setPort(String(r.port))
              })}>{t('随机')}</button>
            </div>
          </Field>
          <Field label={t('监听地址')} htmlFor="ib-listen"><input id="ib-listen" className="input mono" value={listen} onChange={(e) => setListen(e.target.value)} placeholder={t(':: (全部)')} /></Field>
          <Field label={t('备注')} htmlFor="ib-remark"><input id="ib-remark" className="input" value={remark} onChange={(e) => setRemark(e.target.value)} /></Field>
        </div>
        <div className="toggle"><span>{t('启用')}</span><Switch label={t('启用')} on={enabled} onChange={setEnabled} /></div>
      </Section>
      <Section step={2} title={t('协议与传输')} summary={[draft.protocol.type, draft.transport.type, draft.security.type].filter((x) => x && x !== 'none').join(' · ')}>
        {proxyCores.length === 0 ? <div className="note warn">{t('该节点还没有可承载入站的内核，请先在「节点与内核」安装。')}</div> : (
          <InboundFields draft={draft} onChange={setDraft} caps={coreCaps} allCaps={proxyCores} certs={certs.data} nodeId={nodeId}
            coreChoices={proxyCores.map((c) => ({ core: c.core, version: c.version, hint: `v${c.version} · ${c.hotUsers ? t('支持热增删用户') : t('用户变更需重载')}` }))} />
        )}
        {draft.core === 'xray' && (draft.protocol.type === 'vless' || draft.protocol.type === 'trojan') && draft.transport.type === 'tcp' &&
          (draft.security.type === 'tls' || draft.security.type === 'reality') && (
          <FallbackEditor value={draft.protocol.fallbacks ?? []} candidates={nodeInbounds.data ?? []} selfId={editing?.id}
            onChange={(fallbacks) => setDraft((d) => ({ ...d, protocol: { ...d.protocol, fallbacks: fallbacks.length ? fallbacks : undefined } }))} />
        )}
      </Section>
      <Section step={3} title={t('授权与出口')} summary={[groupIds.length ? t('{n} 个分组', { n: groupIds.length }) : t('未授权分组'), outboundTag ? t('出口 {tag}', { tag: outboundTag }) : t('按路由规则'), Number(speedLimit) > 0 ? t('限速 {n} Mbps', { n: speedLimit }) : ''].filter(Boolean).join(' · ')}>
        <Field label={t('授权分组')} hint={t('分组内的用户获得此入站')}>
          <div className="row">
            {(groups.data ?? []).map((g) => (
              <button key={g.id} type="button" className={'chip' + (groupIds.includes(g.id) ? ' on' : '')} aria-pressed={groupIds.includes(g.id)}
                onClick={() => setGroupIds((x) => x.includes(g.id) ? x.filter((y) => y !== g.id) : [...x, g.id])}>{g.name}</button>
            ))}
            {groups.data?.length === 0 && <span className="hint">{t('还没有分组，可在「用户」页创建')}</span>}
          </div>
        </Field>
        <Field label={t('出口')} htmlFor="ib-out" hint={t('绑定后此入站的全部流量走指定出站（含负载均衡组），不再经过路由规则；默认由路由规则决定')}>
          <select id="ib-out" className="input" value={outboundTag} onChange={(e) => setOutboundTag(e.target.value)}>
            <option value="">{t('按路由规则')}</option>
            <option value="direct">{t('direct · 直连')}</option>
            <option value="block">{t('block · 拦截')}</option>
            {(outbounds.data ?? []).map((o) => <option key={o.id} value={o.tag}>{o.tag} · {o.type === 'balancer' ? t('负载均衡') : o.type}</option>)}
          </select>
        </Field>
        <OutboundIPField value={ipStrategy} onChange={setIPStrategy} />
        <Field label={t('限速（Mbps）')} htmlFor="ib-speed" hint={t('节点用 tc 限制这个端口的上传、下载带宽，所有用户共享；留空或 0 不限速。需要节点有 tc（iproute2）')}>
          <input id="ib-speed" className="input mono" inputMode="numeric" placeholder={t('不限速')} value={speedLimit} onChange={(e) => setSpeedLimit(e.target.value.replace(/[^0-9]/g, ''))} />
        </Field>
      </Section>
      <section className="card ops-stack"><h3>{t('中转与访问控制')}</h3><RelayAccessFields value={relayAccess} relayOnly={relayOnly} nodes={nodes.filter((n) => n.id !== nodeId)} onChange={(only, policy) => { setRelayOnly(only); setRelayAccess(policy) }} />{editing && <button className="btn" onClick={() => setRelaysOpen(true)}>{t('中转管理')}</button>}</section>
      <Section step={4} title={t('高级')} defaultOpen={!!extra.trim()} summary={extra.trim() ? t('已设置 Extra') : t('Extra（原样合并进内核配置）')}>
        <Field label="Extra" hint={t('原样合并进内核配置的 JSON')}>
          <textarea className="input" value={extra} onChange={(e) => setExtra(e.target.value)} placeholder='{"sniffing": {"routeOnly": true}}' />
        </Field>
      </Section>
      {conflict ? <div className="note bad">{t('端口 {port} 与 {node} 上的入站 {tag} 冲突', { port, node: node?.name, tag: conflict.tag })}</div> : (
        <div className="note">{t('与 {node} 上已有入站无端口冲突 · 预计下发方式：', { node: node?.name ?? t('节点') })}{editing ? (draft.core === 'xray' ? t('结构变更，重启 Xray') : draft.core === 'singbox' ? t('结构变更，SIGHUP 重载') : t('重载')) : t('新增入站需重载内核')}{t('；之后的用户增删')}{coreHot ? t('走热更新') : t('会触发重载')}</div>
      )}
      {draft.core === 'singbox' && node?.cores.find((c) => c.type === 'singbox')?.noUserTraffic && (
        <div className="note warn">{t('该节点运行的是官方版 sing-box（不含 v2ray_api），暂时只能统计入站总流量，用户的已用流量不会增加。Polaris 会自动构建带统计功能的同版本 sing-box，发布后 Agent 下次启动时自动替换；也可以在「节点」页把 sing-box 切换到已构建的版本。')}</div>
      )}
      {editing && <Endpoints inbound={editing} onChanged={onSaved} />}
      {!editing && (draftCDNCapable
        ? <NewCDNEntry value={newCDN} onChange={setNewCDN} grpc={draft.transport.type === 'grpc'} tls={draft.security.type === 'tls'} port={Number(port) || 0} />
        : <Field label={t('CDN 入口')} hint={t('当前是 {combo}，不能走 CDN：CDN 只转发 WS、gRPC、HTTPUpgrade、XHTTP，而且不能用 REALITY。', { combo: `${draft.transport.type.toUpperCase()} + ${draft.security.type.toUpperCase()}` })}>
          <div><button className="btn sm" type="button" onClick={() => {
            setDraft((d) => normalizeDraft({ ...d, protocol: { ...d.protocol, type: 'vless' },
              transport: { ...d.transport, type: 'ws', path: d.transport.path || '/' + Math.random().toString(36).slice(2, 10) },
              security: { ...d.security, type: 'tls' } }, coreCaps))
            if (!cloudflarePorts.includes(Number(port))) setPort('2053')
          }}>{t('改为 VLESS + WS + TLS（可走 CDN）')}</button></div>
        </Field>)}
      {editing && relaysOpen && <RelayManager inbound={editing} nodes={nodes} onClose={() => setRelaysOpen(false)} onChanged={onSaved} />}
      {preview && <Modal title={t('{core} 原生配置预览', { core: coreLabel[draft.core] })} onClose={() => setPreview('')} wide><div className="code">{preview}</div></Modal>}
    </Drawer>
  )
}

export default function Inbounds() {
  const { nodes } = useNodes()
  const [params] = useSearchParams()
  const [filter, setFilter] = useState<{ core?: CoreType; node?: string }>(params.get('node') ? { node: params.get('node')! } : {})
  const list = useLoad(() => get<Inbound[]>('/inbounds'))
  const [hidden, setHidden] = useState<string[]>([])
  const undo = useUndo()
  const removeInbound = (i: Inbound) => {
    setHidden((h) => [...h, i.id])
    setDrawer(null)
    undo(t('已删除入站 {tag}', { tag: i.tag }), async () => { await del('/inbounds/' + i.id); list.reload() }, () => setHidden((h) => h.filter((x) => x !== i.id)))
  }
  const tagStats = useLoad(() => get<TagTrafficRow[]>('/stats/tags?kind=inbound').catch(() => [] as TagTrafficRow[]))
  const trafficOf = useMemo(() => {
    const m = new Map<string, { up: number; down: number }>()
    for (const r of tagStats.data ?? []) m.set(r.nodeId + '/' + r.tag, { up: r.up, down: r.down })
    return m
  }, [tagStats.data])
  const [drawer, setDrawer] = useState<Inbound | 'new' | null>(null)
  const [snellSetup,setSnellSetup] = useState(false)
  const [snellNode,setSnellNode] = useState<string>()
  const [relayInbound, setRelayInbound] = useState<Inbound>()
  const [cloning, setCloning] = useState<Inbound | null>(null)
  const [importing, setImporting] = useState(false)
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<{ k: string; desc: boolean }>({ k: '', desc: false })
  const [sel, setSel] = useState<Set<string>>(new Set())
  const [bulk, setBulk] = useState<'port' | 'speed' | null>(null)
  const toast = useToast()
  const confirm = useConfirm()
  const { run } = useAction()
  const traffic = (i: Inbound) => { const t = trafficOf.get(i.nodeId + '/' + i.tag); return t ? t.up + t.down : 0 }
  const rows = useMemo(() => {
    const s = q.trim().toLowerCase()
    const out = (list.data ?? []).filter((i) => !hidden.includes(i.id) && (!filter.core || i.core === filter.core) && (!filter.node || i.nodeId === filter.node) &&
      (!s || [i.tag, i.remark ?? '', String(i.port), i.nodeName, i.protocol.type, i.transport.type].some((x) => x.toLowerCase().includes(s))))
    const key: Record<string, (i: Inbound) => string | number> = { tag: (i) => i.tag, node: (i) => i.nodeName, port: (i) => i.port, users: (i) => i.userCount, traffic }
    if (sort.k && key[sort.k]) {
      const f = key[sort.k]
      out.sort((a, b) => { const x = f(a), y = f(b); const r = x < y ? -1 : x > y ? 1 : 0; return sort.desc ? -r : r })
    }
    return out
  }, [list.data, filter, q, sort, trafficOf, hidden]) // eslint-disable-line react-hooks/exhaustive-deps
  const th = (k: string, label: string) => (
    <th className="sortable" aria-sort={sort.k === k ? (sort.desc ? 'descending' : 'ascending') : 'none'}
      onClick={() => setSort((s) => (s.k === k ? { k, desc: !s.desc } : { k, desc: k === 'users' || k === 'traffic' }))}>
      {label}<span className="sort-ind">{sort.k === k ? (sort.desc ? '↓' : '↑') : '↕'}</span>
    </th>
  )
  const totals = rows.reduce((a, i) => { const t = trafficOf.get(i.nodeId + '/' + i.tag); return t ? { up: a.up + t.up, down: a.down + t.down } : a }, { up: 0, down: 0 })
  const exportOne = (i: Inbound) => run(async () => downloadJSON(`inbound-${i.tag}.json`, await get(`/inbounds/${i.id}/export`)), t('已导出'))
  const resetTraffic = async (i?: Inbound) => {
    if (!(await confirm(i ? t('清零入站 {tag} 的流量统计？', { tag: i.tag }) : t('清零全部入站的流量统计？'), { danger: !i, ok: t('清零') }))) return
    run(async () => { await post('/stats/tags/reset', i ? { kind: 'inbound', nodeId: i.nodeId, tag: i.tag } : { kind: 'inbound' }); tagStats.reload() }, t('已清零'))
  }
  const usedCores = [...new Set((list.data ?? []).map((i) => i.core))]
  const masterOf = useMemo(() => {
    const m: Record<string, string> = {}
    for (const i of list.data ?? []) for (const f of i.protocol.fallbacks ?? []) m[f.inboundId] = i.tag
    return m
  }, [list.data])

  const toggle = async (i: Inbound) => {
    try {
      await put('/inbounds/' + i.id, { ...i, enabled: !i.enabled, groupIds: undefined })
      list.reload()
    } catch (e) { toast(String(e), true) }
  }

  return (
    <>
      <div className="top">
        <div><h1>{t('入站')}</h1><div className="sub">{t('一个入站 = 节点 × 内核 × 协议 × 传输 × 安全')}</div></div>
        <div className="row">
          <details className="menu">
            <summary className="btn">{t('更多')}</summary>
            <div className="menu-list" onClick={(e) => { (e.currentTarget.parentElement as HTMLDetailsElement).open = false }}>
              <button type="button" disabled={!nodes.length} onClick={() => setImporting(true)}>{t('导入入站 JSON')}</button>
              <button type="button" disabled={!rows.length} onClick={() => run(async () => downloadJSON('polaris-inbounds.json', await Promise.all(rows.map((i) => get(`/inbounds/${i.id}/export`)))), t('已导出'))}>{t('导出当前列表')}</button>
              <button type="button" onClick={() => resetTraffic()}>{t('清零全部入站流量')}</button>
            </div>
          </details>
          <button className="btn" type="button" disabled={!nodes.length} onClick={() => setSnellSetup(true)}>{t('Snell 入站')}</button>
          <button className="btn pri" type="button" disabled={!nodes.length} onClick={() => {setSnellNode(undefined);setDrawer('new')}}>{t('+ 新建入站')}</button>
        </div>
      </div>
      <div className="row" role="group" aria-label={t('筛选')}>
        <input className="input search" placeholder={t('搜索标签 / 备注 / 端口 / 节点')} value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 'auto' }} aria-label={t('搜索入站')} />
        <div className="chips">
          <button className={'chip' + (!filter.core && !filter.node ? ' on' : '')} type="button" onClick={() => setFilter({})}>{t('全部')}</button>
          {usedCores.map((c) => <button key={c} className={'chip' + (filter.core === c ? ' on' : '')} type="button" onClick={() => setFilter({ core: c })}>{coreLabel[c]}</button>)}
          {nodes.map((n) => <button key={n.id} className={'chip' + (filter.node === n.id ? ' on' : '')} type="button" onClick={() => setFilter({ node: n.id })}>{n.name}</button>)}
        </div>
      </div>
      {sel.size > 0 && (
        <div className="card row" style={{ gap: 8, padding: '10px 14px', position: 'sticky', top: 8, zIndex: 3 }}>
          <b>{t('已选 {n} 个', { n: sel.size })}</b><span className="spacer" />
          <button className="btn sm" type="button" onClick={() => setBulk('port')}>{t('修改端口')}</button>
          <button className="btn sm" type="button" onClick={() => setBulk('speed')}>{t('设置限速')}</button>
          <button className="btn sm" type="button" onClick={() => run(async () => { await post('/inbounds/bulk', { ids: [...sel], action: 'enable' }); list.reload() }, t('已启用'))}>{t('启用')}</button>
          <button className="btn sm" type="button" onClick={() => run(async () => { await post('/inbounds/bulk', { ids: [...sel], action: 'disable' }); list.reload() }, t('已停用'))}>{t('停用')}</button>
          <button className="btn sm" type="button" onClick={() => setSel(new Set())}>{t('取消选择')}</button>
        </div>
      )}
      <div className="card flush scroll">
        {rows.length === 0 ? <Empty>{list.loading ? t('加载中…') : t('还没有入站')}</Empty> : (
          <table className="rtable">
            <thead><tr><th style={{ width: 32 }} onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={t('全选')} checked={rows.length > 0 && rows.every((i) => sel.has(i.id))}
              onChange={(e) => setSel(e.target.checked ? new Set(rows.map((i) => i.id)) : new Set())} /></th>{th('tag', t('标签'))}{th('node', t('节点 / 内核'))}<th>{t('协议')}</th><th>{t('传输 · 安全')}</th>{th('port', t('端口'))}{th('users', t('授权用户'))}{th('traffic', t('流量 ↑{up} ↓{down}', { up: bytes(totals.up), down: bytes(totals.down) }))}<th>{t('入口')}</th><th>{t('启用')}</th><th /></tr></thead>
            <tbody>
              {rows.map((i) => (
                <tr key={i.id} className="clickable" onClick={() => setDrawer(i)}>
                  <td className="rt-check" onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={t('选择 {tag}', { tag: i.tag })} checked={sel.has(i.id)}
                    onChange={(e) => setSel((x) => { const n = new Set(x); if (e.target.checked) n.add(i.id); else n.delete(i.id); return n })} /></td>
                  <td className="rt-head" style={{ fontWeight: 500 }}>{i.tag}{i.templateId && <span className="tag" style={{ marginLeft: 6 }}>{t('模板')}</span>}
                    {(i.protocol.fallbacks?.length ?? 0) > 0 && <span className="tag xray" style={{ marginLeft: 6 }}>{t('{n} 个回落', { n: i.protocol.fallbacks!.length })}</span>}
                    {masterOf[i.id] && <span className="tag" style={{ marginLeft: 6 }}>{t('经 {tag} 回落', { tag: masterOf[i.id] })}</span>}
                    {i.speedLimitMbps > 0 && <span className="tag" style={{ marginLeft: 6 }}>{t('限速 {n}M', { n: i.speedLimitMbps })}</span>}</td>
                  <td data-label={t('节点')}>{i.nodeName} <span className={coreTagClass(i.core)}>{coreShort[i.core]}</span></td>
                  <td data-label={t('协议')}>{inboundSummary(i).split(' · ')[0]}</td>
                  <td data-label={t('传输 · 安全')}>{inboundSummary(i).split(' · ').slice(1).join(' · ')}</td>
                  <td data-label={t('端口')} className="mono">{portLabel(i)}<HealthDot h={i.health} /></td>
                  <td data-label={t('授权用户')} className="mono">{i.userCount}</td>
                  <td data-label={t('流量')} className="mono small">{(() => { const t = trafficOf.get(i.nodeId + '/' + i.tag); return t ? <>↑ {bytes(t.up)}<br />↓ {bytes(t.down)}</> : '—' })()}</td>
                  <td data-label={t('入口')} className="mono">{i.endpoints}</td>
                  <td data-label={t('启用')} onClick={(e) => e.stopPropagation()}><Switch label={t('启用')} on={i.enabled} onChange={() => toggle(i)} /></td>
                  <td className="rt-actions" onClick={(e) => e.stopPropagation()}>
                    <div className="row" style={{ flexWrap: 'wrap', gap: 2 }}>
                      <button className="btn sm" type="button" onClick={() => setRelayInbound(i)}>{t('中转')}</button>
                      <button className="icon-btn" type="button" title={t('复制到…（同节点或其他节点）')} aria-label={t('复制入站')} onClick={() => setCloning(i)}><Icon.Copy /></button>
                      <button className="icon-btn" type="button" title={t('导出 JSON')} aria-label={t('导出入站')} onClick={() => exportOne(i)}><Icon.Download /></button>
                      <button className="icon-btn" type="button" title={t('清零流量统计')} aria-label={t('清零流量')} onClick={() => resetTraffic(i)}><Icon.Reset /></button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {relayInbound && <RelayManager inbound={relayInbound} nodes={nodes} onClose={() => setRelayInbound(undefined)} onChanged={list.reload} />}
      {bulk && <BulkModal kind={bulk} ids={[...sel]} onClose={() => setBulk(null)} onDone={() => list.reload()} />}
      {cloning && <CloneModal inbound={cloning} nodes={nodes} onClose={() => setCloning(null)} onDone={() => { setCloning(null); list.reload() }} />}
      {importing && <ImportInboundModal nodes={nodes} onClose={() => setImporting(false)} onDone={() => { setImporting(false); list.reload() }} />}
      {snellSetup && <SnellSetup nodes={nodes} initialNode={filter.node} onClose={() => setSnellSetup(false)} onCreate={(id) => {setSnellNode(id);setSnellSetup(false);setDrawer('new')}} />}
      {drawer && <InboundDrawer nodes={nodes} snellNode={drawer === 'new' ? snellNode : undefined} editing={drawer === 'new' ? undefined : drawer} onClose={() => setDrawer(null)} onSaved={() => { list.reload(); if (drawer === 'new') setDrawer(null) }} onDelete={removeInbound} />}
    </>
  )
}
