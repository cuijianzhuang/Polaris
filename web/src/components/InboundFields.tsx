// Capability-driven editor for protocol × transport × security. Options a core
// cannot serve are greyed out; the matrix comes from GET /capabilities.
import type { CapSet, Certificate, CoreType, Protocol, ProtocolSpec, Security, SecuritySpec, SniffSpec, Transport, TransportSpec } from '../types'
import { coreLabel, protoLabel, secLabel, transLabel } from '../format'
import { Field, Switch, fromLines, toLines } from './ui'

export interface InboundDraft {
  core: CoreType
  protocol: ProtocolSpec
  transport: TransportSpec
  security: SecuritySpec
  sniffing: SniffSpec
}

const PROTOCOLS: Protocol[] = ['vless', 'vmess', 'trojan', 'shadowsocks', 'hysteria2', 'tuic', 'anytls', 'shadowtls', 'naive', 'socks', 'http']
const TRANSPORTS: Transport[] = ['tcp', 'ws', 'grpc', 'httpupgrade', 'xhttp', 'kcp', 'quic']
const SECURITIES: Security[] = ['reality', 'tls', 'none']
const SS_METHODS = ['2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305']

function supports(caps: CapSet[], p?: Protocol, t?: Transport, s?: Security) {
  return caps.some((c) => c.combos.some((x) => (!p || x.protocol === p) && (!t || x.transport === t) && (!s || x.security === s)))
}

function onlyOn(all: CapSet[], p: Protocol): string {
  const who = all.filter((c) => c.combos.some((x) => x.protocol === p)).map((c) => coreLabel[c.core])
  return who.length ? '仅 ' + who.join(' / ') : '无内核支持'
}

// normalize picks valid transport/security after a change.
export function normalizeDraft(d: InboundDraft, caps: CapSet[]): InboundDraft {
  const p = d.protocol.type
  let t = d.transport.type
  let s = d.security.type
  if (!supports(caps, p, t)) t = TRANSPORTS.find((x) => supports(caps, p, x)) ?? t
  if (!supports(caps, p, t, s)) s = SECURITIES.find((x) => supports(caps, p, t, x)) ?? s
  const protocol = { ...d.protocol }
  if (p !== 'vless' || t !== 'tcp' || s === 'none') delete protocol.flow
  if (p === 'shadowsocks' && !protocol.method) protocol.method = SS_METHODS[0]
  return { ...d, protocol, transport: { ...d.transport, type: t }, security: { ...d.security, type: s } }
}

export default function InboundFields({ draft, onChange, caps, allCaps, coreChoices, certs, showFlowHint = true }: {
  draft: InboundDraft
  onChange: (d: InboundDraft) => void
  caps: CapSet[] // capabilities of the selected core (or all cores for "auto")
  allCaps: CapSet[] // every core on the node, for "only on X" hints
  coreChoices?: { core: CoreType; version?: string; hint?: string }[]
  certs?: Certificate[]
  showFlowHint?: boolean
}) {
  const set = (patch: Partial<InboundDraft>) => onChange(normalizeDraft({ ...draft, ...patch }, caps))
  const p = draft.protocol
  const t = draft.transport
  const s = draft.security
  const setP = (x: Partial<ProtocolSpec>) => onChange({ ...draft, protocol: { ...p, ...x } })
  const setT = (x: Partial<TransportSpec>) => onChange({ ...draft, transport: { ...t, ...x } })
  const setS = (x: Partial<SecuritySpec>) => onChange({ ...draft, security: { ...s, ...x } })
  const flowable = p.type === 'vless' && t.type === 'tcp' && s.type !== 'none'

  return (
    <>
      {coreChoices && (
        <Field label="内核">
          <div className="seg">
            {coreChoices.map((c) => (
              <button key={c.core} type="button" className={'opt' + (draft.core === c.core ? ' on' : '')} aria-pressed={draft.core === c.core}
                onClick={() => onChange({ ...draft, core: c.core })}>
                {coreLabel[c.core]}<small>{c.hint ?? (c.version ? 'v' + c.version : '')}</small>
              </button>
            ))}
          </div>
        </Field>
      )}
      <Field label="协议" hint="不受当前内核支持的协议置灰；切换内核即可使用。">
        <div className="seg c3">
          {PROTOCOLS.map((x) => {
            const ok = supports(caps, x)
            return (
              <button key={x} type="button" disabled={!ok} className={'opt' + (p.type === x ? ' on' : '')} aria-pressed={p.type === x}
                onClick={() => set({ protocol: { type: x, flow: x === 'vless' ? 'xtls-rprx-vision' : undefined } })}>
                {protoLabel[x]}{!ok && <small>{onlyOn(allCaps, x)}</small>}
              </button>
            )
          })}
        </div>
      </Field>
      <div className="form-grid">
        <Field label="传输">
          <div className="seg">
            {TRANSPORTS.filter((x) => supports(allCaps, p.type, x) || x === t.type).map((x) => (
              <button key={x} type="button" disabled={!supports(caps, p.type, x)} className={'opt' + (t.type === x ? ' on' : '')}
                onClick={() => set({ transport: { ...t, type: x } })}>{transLabel[x]}</button>
            ))}
          </div>
        </Field>
        <Field label="安全">
          <div className="seg" style={{ gridTemplateColumns: 'minmax(0,1fr)' }}>
            {SECURITIES.filter((x) => supports(allCaps, p.type, t.type, x) || x === s.type).map((x) => (
              <button key={x} type="button" disabled={!supports(caps, p.type, t.type, x)} className={'opt' + (s.type === x ? ' on' : '')}
                onClick={() => set({ security: { ...s, type: x } })}>{secLabel[x]}</button>
            ))}
          </div>
        </Field>
      </div>

      {/* protocol specific */}
      {flowable && (
        <Field label="流控" htmlFor="f-flow" hint={showFlowHint ? 'Vision 仅用于 TCP + TLS/REALITY' : undefined}>
          <select id="f-flow" className="input mono" value={p.flow ?? ''} onChange={(e) => setP({ flow: e.target.value || undefined })}>
            <option value="xtls-rprx-vision">xtls-rprx-vision</option>
            <option value="">无</option>
          </select>
        </Field>
      )}
      {p.type === 'shadowsocks' && (
        <Field label="加密方式" htmlFor="f-ss" hint="多用户需 2022 系列；每个用户自动生成对应长度的 PSK">
          <select id="f-ss" className="input mono" value={p.method} onChange={(e) => setP({ method: e.target.value })}>
            {SS_METHODS.map((m) => <option key={m}>{m}</option>)}
          </select>
        </Field>
      )}
      {p.type === 'hysteria2' && (
        <div className="form-grid">
          <Field label="上行带宽 (Mbps)" htmlFor="f-up"><input id="f-up" className="input mono" type="number" value={p.upMbps ?? ''} onChange={(e) => setP({ upMbps: Number(e.target.value) || undefined })} /></Field>
          <Field label="下行带宽 (Mbps)" htmlFor="f-down"><input id="f-down" className="input mono" type="number" value={p.downMbps ?? ''} onChange={(e) => setP({ downMbps: Number(e.target.value) || undefined })} /></Field>
          <label className="chk full"><input type="checkbox" checked={p.obfs === 'salamander'} onChange={(e) => setP({ obfs: e.target.checked ? 'salamander' : undefined })} />Salamander 混淆（密码自动生成）</label>
        </div>
      )}
      {p.type === 'tuic' && (
        <Field label="拥塞控制" htmlFor="f-cc">
          <select id="f-cc" className="input mono" value={p.congestionControl ?? 'bbr'} onChange={(e) => setP({ congestionControl: e.target.value })}>
            <option>bbr</option><option>cubic</option><option>new_reno</option>
          </select>
        </Field>
      )}
      {p.type === 'shadowtls' && (
        <div className="form-grid">
          <Field label="握手服务器" htmlFor="f-hs"><input id="f-hs" className="input mono" value={p.handshakeServer ?? ''} onChange={(e) => setP({ handshakeServer: e.target.value })} placeholder="www.example.com:443" /></Field>
          <Field label="内层入站标签" htmlFor="f-det" hint="常与 SS 2022 串联"><input id="f-det" className="input mono" value={p.detour ?? ''} onChange={(e) => setP({ detour: e.target.value })} /></Field>
        </div>
      )}
      {p.type === 'socks' && <label className="chk"><input type="checkbox" checked={!!p.udp} onChange={(e) => setP({ udp: e.target.checked })} />允许 UDP</label>}

      {/* transport specific */}
      {(t.type === 'ws' || t.type === 'httpupgrade' || t.type === 'xhttp') && (
        <div className="form-grid">
          <Field label="路径" htmlFor="f-path" hint="留空则随机生成"><input id="f-path" className="input mono" value={t.path ?? ''} onChange={(e) => setT({ path: e.target.value })} placeholder="/随机" /></Field>
          <Field label="Host" htmlFor="f-host"><input id="f-host" className="input mono" value={t.host ?? ''} onChange={(e) => setT({ host: e.target.value })} /></Field>
          {t.type === 'xhttp' && (
            <Field label="XHTTP 模式" htmlFor="f-mode">
              <select id="f-mode" className="input mono" value={t.mode ?? 'auto'} onChange={(e) => setT({ mode: e.target.value })}>
                <option>auto</option><option>packet-up</option><option>stream-up</option><option>stream-one</option>
              </select>
            </Field>
          )}
        </div>
      )}
      {t.type === 'grpc' && (
        <Field label="Service Name" htmlFor="f-svc" hint="留空则随机生成"><input id="f-svc" className="input mono" value={t.serviceName ?? ''} onChange={(e) => setT({ serviceName: e.target.value })} /></Field>
      )}

      {/* security specific */}
      {s.type === 'tls' && (
        <div className="form-grid">
          <Field label="SNI / 证书域名" htmlFor="f-sni"><input id="f-sni" className="input mono" value={s.serverName ?? ''} onChange={(e) => setS({ serverName: e.target.value })} placeholder="默认节点域名" /></Field>
          <Field label="ALPN" htmlFor="f-alpn"><input id="f-alpn" className="input mono" value={(s.alpn ?? []).join(',')} onChange={(e) => setS({ alpn: fromLines(e.target.value) })} placeholder="h2,http/1.1" /></Field>
          {certs && (
            <Field label="证书" htmlFor="f-cert" full hint="不选则为该域名自动签发自签证书（客户端将跳过校验）">
              <select id="f-cert" className="input" value={s.certificateId ?? ''} onChange={(e) => setS({ certificateId: e.target.value || undefined })}>
                <option value="">自动（自签）</option>
                {certs.map((c) => <option key={c.id} value={c.id}>{c.domain} · {c.provider}</option>)}
              </select>
            </Field>
          )}
        </div>
      )}
      {s.type === 'reality' && (
        <>
          <Field label="REALITY 目标站点" htmlFor="f-dest" hint="私钥 / 公钥 / shortId 一键生成，私钥加密存储">
            <input id="f-dest" className="input mono" value={s.realityDest ?? ''} onChange={(e) => setS({ realityDest: e.target.value })} placeholder="www.microsoft.com:443" />
          </Field>
          <Field label="Server Names" htmlFor="f-sn" hint="每行一个，默认取目标站点域名">
            <textarea id="f-sn" className="input" style={{ minHeight: 64 }} value={toLines(s.realityServerNames)} onChange={(e) => setS({ realityServerNames: fromLines(e.target.value) })} />
          </Field>
          {s.realityPublicKey && <div className="hint mono">公钥：{s.realityPublicKey} · shortId：{(s.realityShortIds ?? []).join(', ')}</div>}
        </>
      )}
      <div className="toggle"><span>流量嗅探（http / tls / quic）</span><Switch label="嗅探" on={draft.sniffing.enabled} onChange={(v) => onChange({ ...draft, sniffing: { ...draft.sniffing, enabled: v } })} /></div>
    </>
  )
}
