import { createPortal } from 'react-dom'
import { useEffect, useState } from 'react'
import { api, get, post, put } from '../api'
import { datetime, errMsg } from '../format'
import { t, tr } from '../i18n'
import type { Inbound, Node } from '../types'
import { CopyBox, Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, usePoll } from './ui'

type Hop = { nodeId: string; port?: number; tunnelPort?: number }
type Relay = { businessDiagnostic?: { supported: boolean; reason?: string }; id: string; name: string; remark: string; mode: string; tunnelType?: string; disabled: boolean; batchId?: string; address: string; port: number; shareAddress?: string; sharePort?: number; shareName?: string; state: string; hops: Hop[]; nodes: { nodeId: string; name: string; state: string; desiredRev: number; appliedRev: number; error?: string }[] }
type Port = { nodeId: string; port: number; network: string; public: boolean; role?: string; engine?: string }
type Plan = { planToken: string; batchId: string; expiresAt: string; warnings: string[]; entries: { chain: { id: string; name: string; hops: Hop[] }; address: string; port: number; ports: Port[] }[] }
type Impact = { action: string; etag: string; blocked: boolean; chainIds: string[]; changedPaths: number; remainingRelayPaths: number; remainingEnabledRelayPaths: number; nodes: { id: string; name: string; status: string; otherPaths: number }[]; ports: Port[]; references: { kind: string; resourceId: string; nodeId?: string; targetTag?: string }[]; warnings: string[]; endpointIds: string[]; outboundIds: string[]; routingRuleIds: string[]; serviceUserIds: string[] }
type Diagnostic = { kind?: 'network' | 'business'; state: string; businessVerified: boolean; cleanupVerified?: boolean; message?: string; checkedAt?: string; checks?: { kind: string; state: string; nodeId?: string; target?: string; message?: string; latencyMs?: number }[] }
const relayState = (s: string) => ({ online: t('在线'), error: t('错误'), applied: t('已应用'), pending: t('待下发'), syncing: t('同步中'), offline: t('离线'), failed: t('失败'), blocked: t('入站已停用'), disabled: t('已停用'), mixed: t('部分生效') }[s] ?? s)
const checkState = (s: string) => ({ passed: t('通过'), failed: t('失败'), skipped: t('跳过'), not_tested: t('未测试'), not_ready: t('尚未就绪'), unsupported: t('暂不支持'), partial: t('部分结果') }[s] ?? s)

const checkKind = (kind: string) => ({ deployment: t('配置应用'), entry_tcp: t('入口 TCP 连接'), entry_tls_handshake: t('入口 TLS 握手'), hop_tcp: t('逐跳 TCP 连接'), landing_egress_http: t('落地节点 HTTP 出口'), proxy_business: t('代理业务验证'), proxy_tcp_business: t('代理 TCP 业务'), proxy_udp_business: t('代理 UDP 业务'), credential_cleanup: t('临时授权清理'), execution: t('诊断执行') }[kind] ?? kind)

function diagnosticSummary(d: Diagnostic) {
  if (d.kind === 'network') {
    if (d.state === 'not_ready') return { text: t('网络诊断尚未就绪'), tone: '' }
    if (d.state === 'failed' || d.checks?.some((c) => c.state === 'failed')) return { text: t('网络诊断发现异常'), tone: ' warn' }
    if (d.checks?.some((c) => c.kind !== 'proxy_business' && c.state === 'passed')) return { text: t('已执行的网络检查通过'), tone: ' ok-note' }
    return { text: t('网络诊断未完成'), tone: '' }
  }
  if (d.state === 'unsupported') return { text: t('当前入站暂不支持真实业务诊断'), tone: '' }
  if (d.state === 'not_ready') return { text: t('真实业务诊断尚未就绪'), tone: '' }
  if (d.businessVerified) return { text: t('真实业务已验证'), tone: ' ok-note' }
  return { text: t('真实业务诊断未通过'), tone: ' warn' }
}

const hostPort = (host: string, port: number) => `${host.includes(':') && !host.startsWith('[') ? '[' + host + ']' : host}:${port}`

function Ports({ ports, nodes }: { ports: Port[]; nodes: Node[] }) {
  return <div className="scroll"><table><thead><tr><th>{t('节点')}</th><th>{t('端口')}</th><th>{t('网络')}</th><th>{t('监听范围')}</th></tr></thead><tbody>{ports.map((p, i) => <tr key={i}><td>{nodes.find((n) => n.id === p.nodeId)?.name ?? p.nodeId}</td><td className="mono">{p.port}</td><td>{p.network} · {p.engine}</td><td>{p.public ? t('公网监听') : t('仅本机')}</td></tr>)}</tbody></table></div>
}

function RelayCreate({ inbound, nodes, onClose, onDone }: { inbound: Inbound; nodes: Node[]; onClose: () => void; onDone: () => void }) {
  const [entryIds, setEntries] = useState<string[]>([])
  const [tail, setTail] = useState<Hop[]>([])
  const [name, setName] = useState('')
  const [remark, setRemark] = useState('')
  const [mode, setMode] = useState('l4')
  const [transport, setTransport] = useState('tcp')
  const [plan, setPlan] = useState<Plan>()
  const [previewError, setPreviewError] = useState('')
  const { busy, run } = useAction()
  const usable = nodes.filter((n) => n.id !== inbound.nodeId && !n.revoked)
  const touch = () => { setPlan(undefined); setPreviewError('') }
  const [now, setNow] = useState(Date.now)
  useEffect(() => { if (!plan) return; const timer = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(timer) }, [plan])
  const expired = !!plan && now > Date.parse(plan.expiresAt)
  const preview = () => run(async () => {
    setPreviewError('')
    try {
      setPlan(await post<Plan>(`/inbounds/${inbound.id}/relays/batch/preview`, { name, remark, mode, ...(mode === 'tunnel' ? { transport } : {}), entries: entryIds.map((nodeId) => ({ nodeId })), relays: tail }))
    } catch (e) { setPlan(undefined); setPreviewError(errMsg(e)); throw e }
  })
  return <Modal title={t('一键配置中转')} onClose={onClose} wide footer={<><button className="btn" disabled={busy} onClick={onClose}>{t('取消')}</button><button className="btn" disabled={busy || entryIds.length === 0 || tail.some((h) => !h.nodeId)} onClick={preview}>{t('预览配置')}</button>{plan && <button className="btn pri" disabled={busy || expired} onClick={() => run(async () => { await post(`/inbounds/${inbound.id}/relays/batch`, { planToken: plan.planToken }); onDone() }, t('中转已创建，等待节点应用'))}>{t('确认创建并下发')}</button>}</>}>
    <fieldset className="ops-stack" disabled={busy} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}><div className="note">{t('并行入口各自生成一条线路；后续中转按列表顺序串联。端口由服务端分配，先预览再创建。')}</div>
      <Field label={t('线路名称')}><input className="input" aria-label={t('线路名称')} maxLength={64} value={name} onChange={(e) => { setName(e.target.value); touch() }} /></Field>
      <Field label={t('中转方式')}><select className="input" aria-label={t('中转方式')} value={mode} onChange={(e) => { setMode(e.target.value); touch() }}><option value="l4">{t('原样端口转发')}</option><option value="tunnel">{t('GOST 认证隧道')}</option></select></Field>
      {mode === 'tunnel' && <Field label={t('隧道传输')}><select className="input" aria-label={t('隧道传输')} value={transport} onChange={(e) => { setTransport(e.target.value); touch() }}><option value="tcp">TCP</option><option value="ws">WebSocket</option></select></Field>}
      <Field label={t('并行入口节点（最多 16 个）')}><div className="ops-choices">{usable.filter((n) => !tail.some((h) => h.nodeId === n.id)).map((n) => <label className="chk" key={n.id}><input type="checkbox" checked={entryIds.includes(n.id)} disabled={!entryIds.includes(n.id) && entryIds.length >= 16} onChange={(e) => { setEntries(e.target.checked ? [...entryIds, n.id] : entryIds.filter((id) => id !== n.id)); touch() }} />{n.name} · {relayState(n.status)}</label>)}</div></Field>
      <Field label={t('后续中转（按顺序，可留空）')}><div className="ops-stack">{tail.map((hop, i) => <div className="row" key={i}><span>{i + 1}.</span><select className="input" style={{ flex: 1 }} aria-label={t('后续中转节点')} value={hop.nodeId} onChange={(e) => { setTail(tail.map((x, j) => i === j ? { nodeId: e.target.value } : x)); touch() }}><option value="">{t('请选择节点')}</option>{usable.filter((n) => !entryIds.includes(n.id) && !tail.some((x, j) => j !== i && x.nodeId === n.id)).map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select><button className="btn sm" onClick={() => { setTail(tail.filter((_, j) => i !== j)); touch() }}>{t('删除')}</button></div>)}<button className="btn sm" disabled={tail.length >= 2} onClick={() => { setTail([...tail, { nodeId: '' }]); touch() }}>{t('添加后续中转')}</button></div></Field>
      <div className="small">{t('落地入站')}：{inbound.nodeName} · {inbound.tag}</div><Field label={t('备注')}><input className="input" aria-label={t('备注')} value={remark} onChange={(e) => { setRemark(e.target.value); touch() }} /></Field>
      {previewError && <div className="note warn" role="alert">{previewError}</div>}
      {plan && <div className="ops-stack"><h3>{t('创建预览')}</h3><div className="small mute">{t('有效期至')} {datetime(plan.expiresAt)}</div>{plan.warnings?.map((w) => <div className="note warn" key={w}>{w}</div>)}{plan.entries.map((e) => <div key={e.chain.id} className="ops-summary ops-stack"><b>{e.chain.name}</b><div className="mono">{e.address} : {e.port}</div><Ports ports={e.ports} nodes={nodes} /></div>)}{expired && <div className="note warn">{t('预览已过期，请重新预览')}</div>}</div>}
    </fieldset>
  </Modal>
}

function RelayMetadata({ relay, base, onClose, onDone }: { relay: Relay; base: string; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({ name: relay.name, remark: relay.remark, shareName: relay.shareName ?? '', shareAddress: relay.shareAddress ?? '', sharePort: String(relay.sharePort || '') })
  const { busy, run } = useAction()
  return <Modal title={t('编辑中转名称与分享地址')} onClose={onClose} footer={<><button className="btn" onClick={onClose}>{t('取消')}</button><button className="btn pri" disabled={busy || !f.name.trim()} onClick={() => run(async () => { await put(`${base}/relays/${relay.id}/metadata`, { ...f, sharePort: f.sharePort ? Number(f.sharePort) : 0 }); onDone() }, t('已保存'))}>{t('保存')}</button></>}>
    <div className="ops-stack"><div className="hint">{t('此处不重建监听或隧道。分享地址留空恢复节点地址，端口留空恢复实际入口端口。')}</div><Field label={t('线路名称')}><input aria-label={t('线路名称')} className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field><Field label={t('订阅显示名称')}><input aria-label={t('订阅显示名称')} className="input" value={f.shareName} onChange={(e) => setF({ ...f, shareName: e.target.value })} /></Field><Field label={t('分享地址')}><input aria-label={t('分享地址')} className="input mono" value={f.shareAddress} onChange={(e) => setF({ ...f, shareAddress: e.target.value.trim() })} /></Field><Field label={t('分享端口')}><input aria-label={t('分享端口')} className="input mono" type="number" min={1} max={65535} value={f.sharePort} onChange={(e) => setF({ ...f, sharePort: e.target.value })} /></Field><Field label={t('备注')}><input aria-label={t('备注')} className="input" value={f.remark} onChange={(e) => setF({ ...f, remark: e.target.value })} /></Field></div>
  </Modal>
}

export default function RelayManager({ inbound, nodes, onClose, onChanged }: { inbound: Inbound; nodes: Node[]; onClose: () => void; onChanged: () => void }) {
  const base = `/inbounds/${inbound.id}`
  const list = useLoad(() => get<Relay[]>(base + '/relays'), [inbound.id])
  usePoll(list.reload, 10000)
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Relay>()
  const [impact, setImpact] = useState<{ path: string; preview: Impact }>()
  const [diagnostic, setDiagnostic] = useState<Diagnostic>()
  const [diagnosticTargets, setDiagnosticTargets] = useState({ tcpUrl: '', udpAddress: '' })
  const [batch, setBatch] = useState<{ batchId: string; state: string; chains: Relay[] }>()
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const refresh = () => { list.reload(); onChanged() }
  const actionPreview = (path: string, action: string) => run(async () => setImpact({ path, preview: await post<Impact>(path + '/change-preview', { action }) }))
  const execute = () => run(async () => { if (!impact) return; const { path, preview: p } = impact; try { await api(p.action === 'delete' ? 'DELETE' : 'PUT', path + (p.action === 'delete' ? '' : '/state'), p.action === 'delete' ? undefined : { enabled: p.action === 'enable' }, { 'If-Match': p.etag }); setImpact(undefined); refresh() } catch (e) { if ((e as { status?: number }).status === 412) setImpact(undefined); throw e } }, t('操作已提交，等待节点应用'))
  const diagnose = async (r: Relay, business: boolean) => {
    if (business && !await confirm(t('运行真实业务诊断？正常情况下会回收临时身份；若身份清理失败，将重启落地 Xray，现有连接会中断。'))) return
    setDiagnostic(undefined)
    await run(async () => { setDiagnostic({ ...await post<Diagnostic>(`${base}/relays/${r.id}/diagnose${business ? '/business' : ''}`, business ? { tcpUrl: diagnosticTargets.tcpUrl.trim(), udpAddress: diagnosticTargets.udpAddress.trim() } : undefined), kind: business ? 'business' : 'network' }); refresh() })
  }
  const protocolSupported = inbound.core === 'xray' && inbound.protocol.type === 'vless'
  const protocolReason = t('真实业务诊断目前仅支持 Xray VLESS；此限制不影响当前协议的中转创建和转发')
  const summary = diagnostic ? diagnosticSummary(diagnostic) : undefined
  const groups = [...new Set((list.data ?? []).map((r) => r.batchId).filter((id): id is string => !!id))]
  return createPortal(<Drawer title={t('中转管理 · {tag}', { tag: inbound.tag })} onClose={onClose} footer={<button className="btn" onClick={onClose}>{t('关闭')}</button>}>
    <div className="ops-stack"><div className="row"><button className="btn pri" disabled={busy || !inbound.enabled} onClick={() => setCreating(true)}>+ {t('一键配置中转')}</button><button className="btn" disabled={busy} onClick={list.reload}>{t('刷新')}</button></div>
      {list.error && <div className="note warn" role="alert">{list.error}</div>}
      <details><summary>{t('诊断测试目标（可选）')}</summary><div className="ops-stack"><Field label={t('TCP 测试 URL')}><input aria-label={t('TCP 测试 URL')} className="input mono" placeholder="https://www.gstatic.com/generate_204" value={diagnosticTargets.tcpUrl} onChange={(e) => setDiagnosticTargets({ ...diagnosticTargets, tcpUrl: e.target.value })} /></Field><Field label={t('UDP DNS 测试地址')}><input aria-label={t('UDP DNS 测试地址')} className="input mono" placeholder="1.1.1.1:53" value={diagnosticTargets.udpAddress} onChange={(e) => setDiagnosticTargets({ ...diagnosticTargets, udpAddress: e.target.value })} /></Field><div className="hint">{t('留空使用标准目标；自定义目标的结果不覆盖概览中的标准业务检查记录。')}</div></div></details>
      {groups.map((id) => { const path = `${base}/relay-batches/${id}`; return <div className="ops-summary" key={id}><div className="row"><b>{t('多入口组')}</b><span className="mono small">{id.slice(0, 8)}</span><button className="link" disabled={busy} onClick={() => run(async () => setBatch(await get(path)))}>{t('组详情')}</button><button className="btn sm" disabled={busy} onClick={() => actionPreview(path, 'enable')}>{t('整组启用')}</button><button className="btn sm" disabled={busy} onClick={() => actionPreview(path, 'disable')}>{t('整组停用')}</button><button className="btn sm danger" disabled={busy} onClick={() => actionPreview(path, 'delete')}>{t('整组删除')}</button></div></div> })}
      {(list.data ?? []).map((r) => <div className="ops-summary ops-stack" key={r.id}>
        <div className="row"><b>{r.name}</b><span className="tag">{relayState(r.state)}</span>{r.tunnelType && <span className="tag">GOST · {r.tunnelType}</span>}</div>
        <CopyBox text={hostPort(r.shareAddress || r.address, r.sharePort || r.port)} />
        <div className="small">{r.hops.map((h) => nodes.find((n) => n.id === h.nodeId)?.name ?? h.nodeId).join(' → ')}</div>
        {r.nodes.map((n) => <div className="small" key={n.nodeId}>{n.name} · {relayState(n.state)} · rev {n.appliedRev}/{n.desiredRev}{n.error && <div className="note warn ops-break">{n.error}</div>}</div>)}
        <div className="row"><button className="btn sm" disabled={busy} onClick={() => setEditing(r)}>{t('编辑')}</button><button className="btn sm" disabled={busy} onClick={() => diagnose(r, false)}>{t('网络诊断')}</button><button className="btn sm" disabled={busy || r.disabled || r.state !== 'applied' || !(r.businessDiagnostic?.supported ?? protocolSupported)} title={r.businessDiagnostic?.reason ? tr(r.businessDiagnostic.reason) : (!protocolSupported ? protocolReason : r.state !== 'applied' ? t('等待线路在所有节点应用后再进行真实业务诊断') : undefined)} onClick={() => diagnose(r, true)}>{t('真实业务诊断')}</button><button className="btn sm" disabled={busy} onClick={() => actionPreview(`${base}/relays/${r.id}`, r.disabled ? 'enable' : 'disable')}>{r.disabled ? t('启用') : t('停用')}</button><button className="btn sm danger" disabled={busy} onClick={() => actionPreview(`${base}/relays/${r.id}`, 'delete')}>{t('删除')}</button></div>
        {(!(r.businessDiagnostic?.supported ?? protocolSupported) || r.state !== 'applied') && <div className="hint">{tr(r.businessDiagnostic?.reason || (!protocolSupported ? protocolReason : t('等待线路在所有节点应用后再进行真实业务诊断')))}</div>}
      </div>)}
      {!list.loading && !list.error && !list.data?.length && <Empty>{t('还没有受管中转线路')}</Empty>}
      <div className="hint">{t('一键中转按入站的 TCP/UDP 网络转发；真实业务诊断目前仅支持 Linux + Xray VLESS，此限制不影响其他协议创建中转。保存成功与节点应用成功分别展示。')}</div>
    </div>
    {creating && <RelayCreate inbound={inbound} nodes={nodes} onClose={() => setCreating(false)} onDone={() => { setCreating(false); refresh() }} />}
    {editing && <RelayMetadata relay={editing} base={base} onClose={() => setEditing(undefined)} onDone={() => { setEditing(undefined); refresh() }} />}
    {batch && <Modal title={t('多入口组详情')} onClose={() => setBatch(undefined)}><div className="ops-stack"><b>{relayState(batch.state)}</b>{batch.chains.map((r) => <div key={r.id}>{r.name} · {relayState(r.state)}<div className="mono">{hostPort(r.shareAddress || r.address, r.sharePort || r.port)}</div></div>)}</div></Modal>}
    {impact && <Modal title={t('中转操作影响预览')} onClose={() => setImpact(undefined)} wide footer={<><button className="btn" disabled={busy} onClick={() => setImpact(undefined)}>{t('取消')}</button><button className={'btn ' + (impact.preview.action === 'delete' ? 'danger' : 'pri')} disabled={busy || impact.preview.blocked} onClick={execute}>{t('确认执行')}</button></>}><div className="ops-stack">
      <div className="note">{({ enable: t('启用'), disable: t('停用'), delete: t('删除') }[impact.preview.action])} · {t('影响 {n} 条线路', { n: impact.preview.changedPaths })}</div>
      <div className="small">{t('剩余线路')} {impact.preview.remainingRelayPaths} · {t('剩余启用线路')} {impact.preview.remainingEnabledRelayPaths}</div><div className="small">{t('涉及发布入口')} {impact.preview.endpointIds.length} · {t('出站')} {impact.preview.outboundIds.length} · {t('路由规则')} {impact.preview.routingRuleIds.length} · {t('服务身份')} {impact.preview.serviceUserIds.length}</div>
      {impact.preview.nodes.map((n) => <div key={n.id}>{n.name} · {relayState(n.status)} · {t('其他线路引用 {n}', { n: n.otherPaths })}</div>)}
      {impact.preview.warnings.map((w) => <div className="note warn" key={w}>{w}</div>)}
      {impact.preview.blocked && <div className="note warn">{t('仍有外部引用，解除引用后才能执行。')}</div>}
      {impact.preview.references.map((r, i) => <div key={i} className="small mono ops-break">{r.kind} · {r.resourceId} · {r.targetTag}</div>)}
      <Ports ports={impact.preview.ports} nodes={nodes} /><div className="hint">{t('确认时会重新校验预览；配置已变化时需重新预览。')}</div>
    </div></Modal>}
    {diagnostic && <Modal title={t('线路诊断结果')} onClose={() => setDiagnostic(undefined)} wide><div className="ops-stack"><div className={'note' + (summary?.tone ?? '')}>{summary?.text}</div>{diagnostic.kind === 'network' && <div className="hint">{t('本次仅检查网络，代理业务未测试；未测试不代表业务失败。')}</div>}{diagnostic.message && <div>{tr(diagnostic.message)}</div>}{diagnostic.checkedAt && <div className="small mute">{datetime(diagnostic.checkedAt)}</div>}{diagnostic.checks?.map((c, i) => <div className="ops-summary" key={i}><b>{checkKind(c.kind)}</b> · {checkState(c.state)}{c.latencyMs !== undefined && ' · ' + c.latencyMs + ' ms'}<div className="small ops-break">{c.target} {c.message && tr(c.message)}</div></div>)}</div></Modal>}
  </Drawer>, document.body)
}

export function RelayAccessFields({ value, relayOnly, nodes, onChange }: { value: NonNullable<Inbound['relayAccess']>; relayOnly: boolean; nodes: Node[]; onChange: (only: boolean, policy: NonNullable<Inbound['relayAccess']>) => void }) {
  return <div className="ops-stack">
    <div className="toggle"><span>{t('仅通过中转提供')}</span><Switch label={t('仅通过中转提供')} on={relayOnly || value.enabled} disabled={value.enabled} onChange={(v) => onChange(v, value)} /></div><div className="hint">{t('订阅和分享中隐藏直连入口；这不是防火墙访问限制。')}</div>
    <div className="toggle"><span>{t('只允许中转机访问')}</span><Switch label={t('只允许中转机访问')} on={value.enabled} onChange={(v) => onChange(v || relayOnly, { ...value, enabled: v, proxyProtocol: v ? value.proxyProtocol : 0 })} /></div>
    {value.enabled && <><div className="note warn">{t('来源白名单需要新 Agent 和受管线路；没有有效中转来源时会拒绝外部访问。请先确认线路与出口地址。')}</div><Field label={t('真实客户端 IP（PROXY 协议）')} hint={t('当前仅支持原样 TCP 中转及兼容入站；启用后直连客户端无法使用。')}><select className="input" aria-label={t('真实客户端 IP（PROXY 协议）')} value={value.proxyProtocol || 0} onChange={(e) => onChange(true, { ...value, proxyProtocol: Number(e.target.value) })}><option value={0}>{t('关闭')}</option><option value={1}>PROXY v1</option><option value={2}>PROXY v2</option></select></Field>
      <details><summary>{t('自定义中转来源地址')}</summary><div className="ops-stack"><div className="hint">{t('默认从节点已上报公网 IP 获取；NAT 或专线可按节点覆盖实际来源 IP/CIDR，每行一个，留空恢复自动。')}</div>{nodes.map((n) => <Field key={n.id} label={n.name}><textarea className="input mono" aria-label={n.name + ' CIDR'} rows={2} value={value.sourceOverrides?.[n.id]?.join('\n') ?? ''} onChange={(e) => { const sources = e.target.value.split('\n'); const overrides = { ...value.sourceOverrides }; if (e.target.value.trim()) overrides[n.id] = sources; else delete overrides[n.id]; onChange(true, { ...value, sourceOverrides: overrides }) }} /></Field>)}</div></details>
    </>}
  </div>
}
