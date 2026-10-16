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
import type { CapSet, CDNProfile, Certificate, Chain, CoreType, Endpoint, Fallback, Group, Inbound, InboundHealth, Node, Outbound } from '../types'

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
  const [kind, setKind] = useState<'direct' | 'cdn' | 'relay'>('cdn')
  const [form, setForm] = useState({ cdnProfileId: '', relayChainId: '', address: '', port: '', sni: '', host: '', path: '', remark: '' })
  const { busy, run } = useAction()
  const add = () => run(async () => {
    await post(`/inbounds/${inbound.id}/endpoints`, { kind, ...form, port: Number(form.port) || 0 })
    setForm({ cdnProfileId: '', relayChainId: '', address: '', port: '', sni: '', host: '', path: '', remark: '' })
    eps.reload()
    onChanged()
  }, '入口已添加')
  const label = (e: Endpoint) => e.kind === 'direct' ? '直连' : e.kind === 'cdn' ? 'CDN · ' + (cdn.data?.find((c) => c.id === e.cdnProfileId)?.name ?? e.cdnProfileId) : '中转 · ' + (chains.data?.find((c) => c.id === e.relayChainId)?.name ?? e.relayChainId)
  return (
    <section className="field">
      <span className="lbl">入口（订阅里每个入口生成一个节点）</span>
      <div className="card flush">
        <table>
          <tbody>
            {(eps.data ?? []).map((e) => (
              <tr key={e.id}>
                <td>{label(e)}</td>
                <td className="mono small">{[e.address, e.port || '', e.host].filter(Boolean).join(' · ') || '默认'}</td>
                <td style={{ textAlign: 'right' }}><button className="link" type="button" onClick={() => run(async () => { await del(`/inbounds/${inbound.id}/endpoints/${e.id}`); eps.reload() })}>删除</button></td>
              </tr>
            ))}
            {eps.data?.length === 0 && <tr><td className="mute">无入口：订阅按直连生成</td></tr>}
          </tbody>
        </table>
      </div>
      <div className="seg c3">
        {(['direct', 'cdn', 'relay'] as const).map((k) => (
          <button key={k} type="button" className={'opt' + (kind === k ? ' on' : '')} disabled={k === 'cdn' && !inbound.cdnCapable} onClick={() => setKind(k)}>
            {k === 'direct' ? '直连' : k === 'cdn' ? 'CDN' : '中转'}{k === 'cdn' && !inbound.cdnCapable && <small>此组合不能走 CDN</small>}
          </button>
        ))}
      </div>
      <div className="form-grid">
        {kind === 'cdn' && (
          <Field label="CDN 配置档" full>
            <select className="input" value={form.cdnProfileId} onChange={(e) => setForm({ ...form, cdnProfileId: e.target.value })}>
              <option value="">选择配置档</option>
              {(cdn.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name} · {c.originDomain}</option>)}
            </select>
          </Field>
        )}
        {kind === 'relay' && (
          <Field label="中转链路" full>
            <select className="input" value={form.relayChainId} onChange={(e) => setForm({ ...form, relayChainId: e.target.value })}>
              <option value="">选择链路</option>
              {(chains.data ?? []).filter((c) => c.mode === 'l4').map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
            </select>
          </Field>
        )}
        <Field label="地址（可选）"><input className="input mono" value={form.address} onChange={(e) => setForm({ ...form, address: e.target.value })} placeholder="覆盖默认地址" /></Field>
        <Field label="端口（可选）"><input className="input mono" value={form.port} onChange={(e) => setForm({ ...form, port: e.target.value })} /></Field>
        <Field label="Host（可选）"><input className="input mono" value={form.host} onChange={(e) => setForm({ ...form, host: e.target.value })} /></Field>
        <Field label="备注"><input className="input" value={form.remark} onChange={(e) => setForm({ ...form, remark: e.target.value })} /></Field>
      </div>
      <div><button className="btn sm" type="button" disabled={busy} onClick={add}>+ 添加入口</button></div>
    </section>
  )
}

const loopback = (l?: string) => l === 'localhost' || /^127\./.test(l ?? '') || l === '::1'

/** Fallbacks: other protocols on this inbound's port, routed by path / ALPN. */
function FallbackEditor({ value, onChange, candidates, selfId }: { value: Fallback[]; onChange: (v: Fallback[]) => void; candidates: Inbound[]; selfId?: string }) {
  const usable = candidates.filter((i) => i.id !== selfId && i.security.type === 'none' && loopback(i.listen))
  const set = (i: number, p: Partial<Fallback>) => onChange(value.map((f, j) => (j === i ? { ...f, ...p } : f)))
  return (
    <section className="field">
      <span className="lbl">回落（同一端口承载多个协议）</span>
      <div className="hint">不是本入站协议的连接，按路径 / ALPN 转给子入站。子入站需在同一节点、监听 127.0.0.1、不开 TLS；客户端链接自动使用本入站的端口与 TLS。</div>
      {value.map((f, i) => (
        <div key={i} className="fb-row">
          <select className="input" value={f.inboundId} onChange={(e) => set(i, { inboundId: e.target.value })} aria-label="子入站">
            <option value="">选择子入站</option>
            {usable.map((c) => <option key={c.id} value={c.id}>{c.tag} · {inboundSummary(c)} · :{c.port}</option>)}
          </select>
          <input className="input mono" value={f.path ?? ''} placeholder="路径，如 /ws（空 = 不限）" onChange={(e) => set(i, { path: e.target.value || undefined })} aria-label="路径" />
          <select className="input" value={f.alpn ?? ''} onChange={(e) => set(i, { alpn: e.target.value || undefined })} aria-label="ALPN">
            <option value="">ALPN 不限</option><option value="h2">h2</option><option value="http/1.1">http/1.1</option>
          </select>
          <button className="icon-btn" type="button" aria-label="删除回落" onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </div>
      ))}
      <div className="row">
        <button className="btn sm" type="button" disabled={!usable.length} onClick={() => onChange([...value, { inboundId: usable[0]?.id ?? '' }])}>+ 添加回落</button>
        {!usable.length && <span className="hint">该节点还没有可作回落目标的入站：新建一个监听 127.0.0.1、不开 TLS 的入站（如 VLESS-WS、Trojan）</span>}
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
    <Modal title={kind === 'port' ? `修改端口（${ids.length} 个入站）` : `设置限速（${ids.length} 个入站）`} onClose={onClose} footer={res
      ? <button className="btn pri" type="button" onClick={onClose}>完成</button>
      : <><button className="btn" type="button" onClick={onClose}>取消</button><button className="btn pri" type="button" disabled={busy} onClick={go}>应用并下发</button></>}>
      {res ? (
        <div style={{ display: 'grid', gap: 6 }}>
          <div className={'note' + (failed.length ? ' warn' : '')}>{res.length - failed.length} 个已修改{failed.length ? `，${failed.length} 个未改（见下）` : ''}；节点会自动重新下发，防火墙端口随之更新。</div>
          {res.map((r) => <div key={r.id} className="row small" style={{ flexWrap: 'nowrap' }}>
            <span style={{ flex: 1, minWidth: 0 }}>{r.node} · {r.tag}</span>
            {r.error ? <span style={{ color: 'var(--bad)' }}>{r.error}</span> : r.port ? <span className="mono">→ {r.port}</span> : <span className="mute">✓</span>}
          </div>)}
        </div>
      ) : kind === 'speed' ? (
        <Field label="限速（Mbps）" hint="每个入站端口各自的上下行上限，所有用户共享；0 或留空为不限速">
          <input className="input mono" inputMode="numeric" autoFocus placeholder="不限速" value={v.mbps} onChange={(e) => setV({ ...v, mbps: e.target.value.replace(/\D/g, '') })} />
        </Field>
      ) : (
        <div style={{ display: 'grid', gap: 12 }}>
          <div className="seg c3">
            {([['random', '随机', '在范围内随机换一个空闲端口，常用于端口被封'], ['sequential', '顺序编号', '从起始端口开始依次分配'], ['offset', '整体偏移', '每个端口加上同一个数']] as const).map(([k, l, h]) => (
              <button key={k} type="button" className={'opt' + (mode === k ? ' on' : '')} onClick={() => setMode(k)}>{l}<small>{h}</small></button>
            ))}
          </div>
          {mode === 'random' && <div className="form-grid">
            <Field label="最小端口"><input className="input mono" inputMode="numeric" value={v.min} onChange={(e) => setV({ ...v, min: e.target.value })} /></Field>
            <Field label="最大端口"><input className="input mono" inputMode="numeric" value={v.max} onChange={(e) => setV({ ...v, max: e.target.value })} /></Field>
          </div>}
          {mode === 'sequential' && <Field label="起始端口" hint="按列表顺序依次 +1"><input className="input mono" inputMode="numeric" value={v.start} onChange={(e) => setV({ ...v, start: e.target.value })} /></Field>}
          {mode === 'offset' && <Field label="偏移量" hint="可为负数，如 -1000"><input className="input mono" value={v.offset} onChange={(e) => setV({ ...v, offset: e.target.value })} /></Field>}
          <div className="hint">与同节点其他入站冲突的会跳过并列出原因。改端口后用户需要更新订阅（客户端自动更新时无需操作）。</div>
        </div>
      )}
    </Modal>
  )
}

// HealthDot: the panel's latest reachability probe of an inbound.
function HealthDot({ h }: { h?: InboundHealth }) {
  if (!h || !h.checkedAt) return null
  if (h.error && h.ok) return <span className="small mute" title={h.error} style={{ marginLeft: 6 }}>UDP</span>
  const tip = `${h.ok ? `可连接，延迟 ${h.latencyMs} ms` : `无法连接：${h.error}（连续 ${h.fails} 次）`} · 检查于 ${new Date(h.checkedAt).toLocaleTimeString()}`
  return <span title={tip} style={{ marginLeft: 6, whiteSpace: 'nowrap', fontSize: 12, color: h.ok ? 'var(--ok)' : 'var(--bad)' }}>● {h.ok ? `${h.latencyMs}ms` : '不通'}</span>
}

function InboundDrawer({ nodes, editing, onClose, onSaved, onDelete }: { nodes: Node[]; editing?: Inbound; onClose: () => void; onSaved: () => void; onDelete: (i: Inbound) => void }) {
  const [nodeId, setNodeId] = useState(editing?.nodeId ?? nodes[0]?.id ?? '')
  const node = nodes.find((n) => n.id === nodeId)
  const caps = useLoad(() => (nodeId ? get<CapSet[]>('/capabilities?node=' + nodeId) : Promise.resolve([])), [nodeId])
  const certs = useLoad(() => get<Certificate[]>('/certificates'))
  const groups = useLoad(() => get<Group[]>('/groups'))
  const nodeInbounds = useLoad(() => (nodeId ? get<Inbound[]>('/inbounds?node=' + nodeId) : Promise.resolve([])), [nodeId])
  const [draft, setDraft] = useState<InboundDraft>(() => editing ? {
    core: editing.core, protocol: editing.protocol, transport: editing.transport, security: editing.security, sniffing: editing.sniffing,
  } : emptyDraft())
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
  const [outboundTag, setOutboundTag] = useState(editing?.outboundTag ?? '')
  const [speedLimit, setSpeedLimit] = useState(editing?.speedLimitMbps ? String(editing.speedLimitMbps) : '')
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
      security: draft.security, sniffing: draft.sniffing, extra: ex, enabled, remark, groupIds, outboundTag, speedLimitMbps: Math.max(0, Math.round(Number(speedLimit) || 0)) }
  }
  const save = () => run(async () => {
    if (editing) await put('/inbounds/' + editing.id, body())
    else await post('/inbounds', body())
    onSaved()
  }, editing ? '已保存并下发' : '入站已创建，正在下发')

  return (
    <Drawer title={editing ? '编辑入站 · ' + editing.tag : '新建入站'} onClose={onClose} footer={<>
      {editing && <button className="btn danger" type="button" style={{ marginRight: 'auto' }} onClick={() => onDelete(editing)}>删除</button>}
      {editing && <button className="btn" type="button" disabled={busy} title="从面板连接这个入站（TCP，TLS / REALITY 再握手）" onClick={() => run(async () => {
        const h = await post<InboundHealth>(`/inbounds/${editing.id}/probe`)
        if (h.error && h.ok) toast(h.error)
        else if (h.ok) toast(`可以连接，延迟 ${h.latencyMs} ms`)
        else toast('无法连接：' + h.error, true)
      })}>探测</button>}
      <button className="btn" type="button" disabled={busy} onClick={() => run(async () => {
        const r = await post<{ config: string }>('/inbounds/preview' + (editing ? '?id=' + editing.id : ''), body())
        setPreview(r.config)
      })}>预览内核配置</button>
      <button className="btn pri" type="button" disabled={busy || !tag || !nodeId} onClick={save}>保存并下发</button>
    </>}>
      <Section step={1} title="基础" summary={[node?.name, tag, port && ':' + port, enabled ? '' : '已停用'].filter(Boolean).join(' · ')}>
        <Field label="节点" htmlFor="ib-node">
          <select id="ib-node" className="input" value={nodeId} disabled={!!editing} onChange={(e) => setNodeId(e.target.value)}>
            {nodes.map((n) => <option key={n.id} value={n.id}>{n.name} · 已装 {n.cores.map((c) => `${coreShort[c.type]} v${c.version}`).join(' / ') || '无内核'}</option>)}
          </select>
        </Field>
        <div className="form-grid">
          <Field label="标签" htmlFor="ib-tag"><input id="ib-tag" className="input mono" value={tag} onChange={(e) => setTag(e.target.value)} /></Field>
          <Field label="端口" htmlFor="ib-port" hint="点「随机」选一个节点上未被占用的端口（含其他程序占用的端口）；保存后自动在节点防火墙放行">
            <div className="joined">
              <input id="ib-port" className="input mono" value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" />
              <button className="btn" type="button" disabled={!nodeId} onClick={() => run(async () => {
                const r = await get<{ port: number }>(`/nodes/${nodeId}/free-port`)
                if (r.port) setPort(String(r.port))
              })}>随机</button>
            </div>
          </Field>
          <Field label="监听地址" htmlFor="ib-listen"><input id="ib-listen" className="input mono" value={listen} onChange={(e) => setListen(e.target.value)} placeholder=":: (全部)" /></Field>
          <Field label="备注" htmlFor="ib-remark"><input id="ib-remark" className="input" value={remark} onChange={(e) => setRemark(e.target.value)} /></Field>
        </div>
        <div className="toggle"><span>启用</span><Switch label="启用" on={enabled} onChange={setEnabled} /></div>
      </Section>
      <Section step={2} title="协议与传输" summary={[draft.protocol.type, draft.transport.type, draft.security.type].filter((x) => x && x !== 'none').join(' · ')}>
        {proxyCores.length === 0 ? <div className="note warn">该节点还没有可承载入站的内核，请先在「节点与内核」安装。</div> : (
          <InboundFields draft={draft} onChange={setDraft} caps={coreCaps} allCaps={proxyCores} certs={certs.data} nodeId={nodeId}
            coreChoices={proxyCores.map((c) => ({ core: c.core, version: c.version, hint: `v${c.version} · ${c.hotUsers ? '支持热增删用户' : '用户变更需重载'}` }))} />
        )}
        {draft.core === 'xray' && (draft.protocol.type === 'vless' || draft.protocol.type === 'trojan') && draft.transport.type === 'tcp' &&
          (draft.security.type === 'tls' || draft.security.type === 'reality') && (
          <FallbackEditor value={draft.protocol.fallbacks ?? []} candidates={nodeInbounds.data ?? []} selfId={editing?.id}
            onChange={(fallbacks) => setDraft((d) => ({ ...d, protocol: { ...d.protocol, fallbacks: fallbacks.length ? fallbacks : undefined } }))} />
        )}
      </Section>
      <Section step={3} title="授权与出口" summary={[groupIds.length ? `${groupIds.length} 个分组` : '未授权分组', outboundTag ? '出口 ' + outboundTag : '按路由规则', Number(speedLimit) > 0 ? `限速 ${speedLimit} Mbps` : ''].filter(Boolean).join(' · ')}>
        <Field label="授权分组" hint="分组内的用户获得此入站">
          <div className="row">
            {(groups.data ?? []).map((g) => (
              <button key={g.id} type="button" className={'chip' + (groupIds.includes(g.id) ? ' on' : '')} aria-pressed={groupIds.includes(g.id)}
                onClick={() => setGroupIds((x) => x.includes(g.id) ? x.filter((y) => y !== g.id) : [...x, g.id])}>{g.name}</button>
            ))}
            {groups.data?.length === 0 && <span className="hint">还没有分组，可在「用户」页创建</span>}
          </div>
        </Field>
        <Field label="出口" htmlFor="ib-out" hint="绑定后此入站的全部流量走指定出站（含负载均衡组），不再经过路由规则；默认由路由规则决定">
          <select id="ib-out" className="input" value={outboundTag} onChange={(e) => setOutboundTag(e.target.value)}>
            <option value="">按路由规则</option>
            <option value="direct">direct · 直连</option>
            <option value="block">block · 拦截</option>
            {(outbounds.data ?? []).map((o) => <option key={o.id} value={o.tag}>{o.tag} · {o.type === 'balancer' ? '负载均衡' : o.type}</option>)}
          </select>
        </Field>
        <Field label="限速（Mbps）" htmlFor="ib-speed" hint="节点用 tc 限制这个端口的上传、下载带宽，所有用户共享；留空或 0 不限速。需要节点有 tc（iproute2）">
          <input id="ib-speed" className="input mono" inputMode="numeric" placeholder="不限速" value={speedLimit} onChange={(e) => setSpeedLimit(e.target.value.replace(/[^0-9]/g, ''))} />
        </Field>
      </Section>
      <Section step={4} title="高级" defaultOpen={!!extra.trim()} summary={extra.trim() ? '已设置 Extra' : 'Extra（原样合并进内核配置）'}>
        <Field label="Extra" hint="原样合并进内核配置的 JSON">
          <textarea className="input" value={extra} onChange={(e) => setExtra(e.target.value)} placeholder='{"sniffing": {"routeOnly": true}}' />
        </Field>
      </Section>
      {conflict ? <div className="note bad">端口 {port} 与 {node?.name} 上的入站 {conflict.tag} 冲突</div> : (
        <div className="note">与 {node?.name ?? '节点'} 上已有入站无端口冲突 · 预计下发方式：{editing ? (draft.core === 'xray' ? '结构变更，重启 Xray' : draft.core === 'singbox' ? '结构变更，SIGHUP 重载' : '重载') : '新增入站需重载内核'}；之后的用户增删{coreHot ? '走热更新' : '会触发重载'}</div>
      )}
      {draft.core === 'singbox' && node?.cores.find((c) => c.type === 'singbox')?.noUserTraffic && (
        <div className="note warn">该节点运行的是官方版 sing-box（不含 v2ray_api），暂时只能统计入站总流量，用户的已用流量不会增加。Polaris 会自动构建带统计功能的同版本 sing-box，发布后 Agent 下次启动时自动替换；也可以在「节点」页把 sing-box 切换到已构建的版本。</div>
      )}
      {editing && <Endpoints inbound={editing} onChanged={onSaved} />}
      {preview && <Modal title={`${coreLabel[draft.core]} 原生配置预览`} onClose={() => setPreview('')} wide><div className="code">{preview}</div></Modal>}
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
    undo(`已删除入站 ${i.tag}`, async () => { await del('/inbounds/' + i.id); list.reload() }, () => setHidden((h) => h.filter((x) => x !== i.id)))
  }
  const tagStats = useLoad(() => get<TagTrafficRow[]>('/stats/tags?kind=inbound').catch(() => [] as TagTrafficRow[]))
  const trafficOf = useMemo(() => {
    const m = new Map<string, { up: number; down: number }>()
    for (const r of tagStats.data ?? []) m.set(r.nodeId + '/' + r.tag, { up: r.up, down: r.down })
    return m
  }, [tagStats.data])
  const [drawer, setDrawer] = useState<Inbound | 'new' | null>(null)
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
  const exportOne = (i: Inbound) => run(async () => downloadJSON(`inbound-${i.tag}.json`, await get(`/inbounds/${i.id}/export`)), '已导出')
  const resetTraffic = async (i?: Inbound) => {
    if (!(await confirm(i ? `清零入站 ${i.tag} 的流量统计？` : '清零全部入站的流量统计？', { danger: !i, ok: '清零' }))) return
    run(async () => { await post('/stats/tags/reset', i ? { kind: 'inbound', nodeId: i.nodeId, tag: i.tag } : { kind: 'inbound' }); tagStats.reload() }, '已清零')
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
        <div><h1>入站</h1><div className="sub">一个入站 = 节点 × 内核 × 协议 × 传输 × 安全</div></div>
        <div className="row">
          <details className="menu">
            <summary className="btn">更多</summary>
            <div className="menu-list" onClick={(e) => { (e.currentTarget.parentElement as HTMLDetailsElement).open = false }}>
              <button type="button" disabled={!nodes.length} onClick={() => setImporting(true)}>导入入站 JSON</button>
              <button type="button" disabled={!rows.length} onClick={() => run(async () => downloadJSON('polaris-inbounds.json', await Promise.all(rows.map((i) => get(`/inbounds/${i.id}/export`)))), '已导出')}>导出当前列表</button>
              <button type="button" onClick={() => resetTraffic()}>清零全部入站流量</button>
            </div>
          </details>
          <button className="btn pri" type="button" disabled={!nodes.length} onClick={() => setDrawer('new')}>+ 新建入站</button>
        </div>
      </div>
      <div className="row" role="group" aria-label="筛选">
        <input className="input search" placeholder="搜索标签 / 备注 / 端口 / 节点" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 'auto' }} aria-label="搜索入站" />
        <div className="chips">
          <button className={'chip' + (!filter.core && !filter.node ? ' on' : '')} type="button" onClick={() => setFilter({})}>全部</button>
          {usedCores.map((c) => <button key={c} className={'chip' + (filter.core === c ? ' on' : '')} type="button" onClick={() => setFilter({ core: c })}>{coreLabel[c]}</button>)}
          {nodes.map((n) => <button key={n.id} className={'chip' + (filter.node === n.id ? ' on' : '')} type="button" onClick={() => setFilter({ node: n.id })}>{n.name}</button>)}
        </div>
      </div>
      {sel.size > 0 && (
        <div className="card row" style={{ gap: 8, padding: '10px 14px', position: 'sticky', top: 8, zIndex: 3 }}>
          <b>已选 {sel.size} 个</b><span className="spacer" />
          <button className="btn sm" type="button" onClick={() => setBulk('port')}>修改端口</button>
          <button className="btn sm" type="button" onClick={() => setBulk('speed')}>设置限速</button>
          <button className="btn sm" type="button" onClick={() => run(async () => { await post('/inbounds/bulk', { ids: [...sel], action: 'enable' }); list.reload() }, '已启用')}>启用</button>
          <button className="btn sm" type="button" onClick={() => run(async () => { await post('/inbounds/bulk', { ids: [...sel], action: 'disable' }); list.reload() }, '已停用')}>停用</button>
          <button className="btn sm" type="button" onClick={() => setSel(new Set())}>取消选择</button>
        </div>
      )}
      <div className="card flush scroll">
        {rows.length === 0 ? <Empty>{list.loading ? '加载中…' : '还没有入站'}</Empty> : (
          <table className="rtable">
            <thead><tr><th style={{ width: 32 }} onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label="全选" checked={rows.length > 0 && rows.every((i) => sel.has(i.id))}
              onChange={(e) => setSel(e.target.checked ? new Set(rows.map((i) => i.id)) : new Set())} /></th>{th('tag', '标签')}{th('node', '节点 / 内核')}<th>协议</th><th>传输 · 安全</th>{th('port', '端口')}{th('users', '授权用户')}{th('traffic', `流量 ↑${bytes(totals.up)} ↓${bytes(totals.down)}`)}<th>入口</th><th>启用</th><th /></tr></thead>
            <tbody>
              {rows.map((i) => (
                <tr key={i.id} className="clickable" onClick={() => setDrawer(i)}>
                  <td className="rt-check" onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={'选择 ' + i.tag} checked={sel.has(i.id)}
                    onChange={(e) => setSel((x) => { const n = new Set(x); if (e.target.checked) n.add(i.id); else n.delete(i.id); return n })} /></td>
                  <td className="rt-head" style={{ fontWeight: 500 }}>{i.tag}{i.templateId && <span className="tag" style={{ marginLeft: 6 }}>模板</span>}
                    {(i.protocol.fallbacks?.length ?? 0) > 0 && <span className="tag xray" style={{ marginLeft: 6 }}>{i.protocol.fallbacks!.length} 个回落</span>}
                    {masterOf[i.id] && <span className="tag" style={{ marginLeft: 6 }}>经 {masterOf[i.id]} 回落</span>}
                    {i.speedLimitMbps > 0 && <span className="tag" style={{ marginLeft: 6 }}>限速 {i.speedLimitMbps}M</span>}</td>
                  <td data-label="节点">{i.nodeName} <span className={coreTagClass(i.core)}>{coreShort[i.core]}</span></td>
                  <td data-label="协议">{inboundSummary(i).split(' · ')[0]}</td>
                  <td data-label="传输 · 安全">{inboundSummary(i).split(' · ').slice(1).join(' · ')}</td>
                  <td data-label="端口" className="mono">{portLabel(i)}<HealthDot h={i.health} /></td>
                  <td data-label="授权用户" className="mono">{i.userCount}</td>
                  <td data-label="流量" className="mono small">{(() => { const t = trafficOf.get(i.nodeId + '/' + i.tag); return t ? <>↑ {bytes(t.up)}<br />↓ {bytes(t.down)}</> : '—' })()}</td>
                  <td data-label="入口" className="mono">{i.endpoints}</td>
                  <td data-label="启用" onClick={(e) => e.stopPropagation()}><Switch label="启用" on={i.enabled} onChange={() => toggle(i)} /></td>
                  <td className="rt-actions" onClick={(e) => e.stopPropagation()}>
                    <div className="row" style={{ flexWrap: 'nowrap', gap: 2 }}>
                      <button className="icon-btn" type="button" title="复制到…（同节点或其他节点）" aria-label="复制入站" onClick={() => setCloning(i)}><Icon.Copy /></button>
                      <button className="icon-btn" type="button" title="导出 JSON" aria-label="导出入站" onClick={() => exportOne(i)}><Icon.Download /></button>
                      <button className="icon-btn" type="button" title="清零流量统计" aria-label="清零流量" onClick={() => resetTraffic(i)}><Icon.Reset /></button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {bulk && <BulkModal kind={bulk} ids={[...sel]} onClose={() => setBulk(null)} onDone={() => list.reload()} />}
      {cloning && <CloneModal inbound={cloning} nodes={nodes} onClose={() => setCloning(null)} onDone={() => { setCloning(null); list.reload() }} />}
      {importing && <ImportInboundModal nodes={nodes} onClose={() => setImporting(false)} onDone={() => { setImporting(false); list.reload() }} />}
      {drawer && <InboundDrawer nodes={nodes} editing={drawer === 'new' ? undefined : drawer} onClose={() => setDrawer(null)} onSaved={() => { list.reload(); if (drawer === 'new') setDrawer(null) }} onDelete={removeInbound} />}
    </>
  )
}
