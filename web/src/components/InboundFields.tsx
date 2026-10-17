// Capability-driven editor for protocol × transport × security. Options a core
// cannot serve are greyed out; the matrix comes from GET /capabilities.
import type { CapSet, Certificate, CoreType, Protocol, ProtocolSpec, Security, SecuritySpec, SniffSpec, Transport, TransportSpec } from '../types'
import { coreLabel, protoLabel, secLabel, transLabel } from '../format'
import { Field, Switch, TagInput, fromLines } from './ui'
import { useState } from 'react'
import { RealityAdvanced, RealityScanner, SniffExtras, TestSeed, VlessEncryption, hasFeature } from './XrayExtras'
import { t } from '../i18n'

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
  return caps.some((c) => c.combos?.some((x) => (!p || x.protocol === p) && (!t || x.transport === t) && (!s || x.security === s)))
}

function onlyOn(all: CapSet[], p: Protocol): string {
  const who = all.filter((c) => c.combos?.some((x) => x.protocol === p)).map((c) => coreLabel[c.core])
  return who.length ? t('仅 {cores}', { cores: who.join(' / ') }) : t('无内核支持')
}

// normalize picks valid transport/security after a change.
export function normalizeDraft(d: InboundDraft, caps: CapSet[]): InboundDraft {
  const p = d.protocol.type
  let t = d.transport.type
  let s = d.security.type
  if (!supports(caps, p, t)) t = TRANSPORTS.find((x) => supports(caps, p, x)) ?? t
  if (!supports(caps, p, t, s)) s = SECURITIES.find((x) => supports(caps, p, t, x)) ?? s
  const protocol = { ...d.protocol }
  if (p !== 'vless' || t !== 'tcp' || s === 'none') { delete protocol.flow; delete protocol.testSeed }
  if (p !== 'vless') { delete protocol.decryption; delete protocol.encryption }
  if (p === 'shadowsocks' && !protocol.method) protocol.method = SS_METHODS[0]
  return { ...d, protocol, transport: { ...d.transport, type: t }, security: { ...d.security, type: s } }
}

export default function InboundFields({ draft, onChange, caps, allCaps, coreChoices, certs, showFlowHint = true, nodeId }: {
  draft: InboundDraft
  onChange: (d: InboundDraft) => void
  caps: CapSet[] // capabilities of the selected core (or all cores for "auto")
  allCaps: CapSet[] // every core on the node, for "only on X" hints
  coreChoices?: { core: CoreType; version?: string; hint?: string }[]
  certs?: Certificate[]
  showFlowHint?: boolean
  nodeId?: string // for the REALITY scanner (probes from the node)
}) {
  const [scanning, setScanning] = useState(false)
  const isXray = caps.some((c) => c.core === 'xray') // newer options are Xray-only
  const set = (patch: Partial<InboundDraft>) => onChange(normalizeDraft({ ...draft, ...patch }, caps))
  const p = draft.protocol
  const tp = draft.transport
  const s = draft.security
  const setP = (x: Partial<ProtocolSpec>) => onChange({ ...draft, protocol: { ...p, ...x } })
  const setT = (x: Partial<TransportSpec>) => onChange({ ...draft, transport: { ...tp, ...x } })
  const setS = (x: Partial<SecuritySpec>) => onChange({ ...draft, security: { ...s, ...x } })
  // REALITY: the SNI must be a name on the target's certificate.
  const destHost = hostOf(s.realityDest)
  const first = s.realityServerNames?.[0] ?? ''
  const snMismatch = s.type === 'reality' && !!first && isDomain(destHost) && baseDomain(first) !== baseDomain(destHost)
  const setDest = (dest: string) => {
    const prev = hostOf(s.realityDest), next = hostOf(dest)
    const names = s.realityServerNames ?? []
    // Names that came from the previous target follow the new one.
    const derived = names.length === 0 || (!!prev && names.every((n) => baseDomain(n) === baseDomain(prev)))
    setS(derived && isDomain(next) && baseDomain(next) !== baseDomain(prev) ? { realityDest: dest, realityServerNames: [next] } : { realityDest: dest })
  }
  const flowable = p.type === 'vless' && tp.type === 'tcp' && s.type !== 'none'

  return (
    <>
      {coreChoices && (
        <Field label={t('内核')}>
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
      <Field label={t('协议')} hint={t('不受当前内核支持的协议置灰；切换内核即可使用。')}>
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
        <Field label={t('传输')}>
          <div className="seg">
            {TRANSPORTS.filter((x) => supports(allCaps, p.type, x) || x === tp.type).map((x) => (
              <button key={x} type="button" disabled={!supports(caps, p.type, x)} className={'opt' + (tp.type === x ? ' on' : '')}
                onClick={() => set({ transport: { ...tp, type: x } })}>{transLabel[x]}</button>
            ))}
          </div>
        </Field>
        <Field label={t('安全')}>
          <div className="seg" style={{ gridTemplateColumns: 'minmax(0,1fr)' }}>
            {SECURITIES.filter((x) => supports(allCaps, p.type, tp.type, x) || x === s.type).map((x) => (
              <button key={x} type="button" disabled={!supports(caps, p.type, tp.type, x)} className={'opt' + (s.type === x ? ' on' : '')}
                onClick={() => set({ security: { ...s, type: x } })}>{secLabel[x]}</button>
            ))}
          </div>
        </Field>
      </div>

      {/* protocol specific */}
      {flowable && (
        <Field label={t('流控')} htmlFor="f-flow" hint={showFlowHint ? t('Vision 仅用于 TCP + TLS/REALITY') : undefined}>
          <select id="f-flow" className="input mono" value={p.flow ?? ''} onChange={(e) => setP({ flow: e.target.value || undefined })}>
            <option value="xtls-rprx-vision">xtls-rprx-vision</option>
            <option value="">{t('无')}</option>
          </select>
        </Field>
      )}
      {flowable && p.flow && hasFeature(caps, 'vless-encryption') && <TestSeed p={p} setP={setP} />}
      {p.type === 'vless' && (hasFeature(allCaps, 'vless-encryption') || !!p.encryption) && (
        <VlessEncryption p={p} setP={setP} available={hasFeature(caps, 'vless-encryption')} />
      )}
      {p.type === 'shadowsocks' && (
        <Field label={t('加密方式')} htmlFor="f-ss" hint={t('多用户需 2022 系列；每个用户自动生成对应长度的 PSK')}>
          <select id="f-ss" className="input mono" value={p.method} onChange={(e) => setP({ method: e.target.value })}>
            {SS_METHODS.map((m) => <option key={m}>{m}</option>)}
          </select>
        </Field>
      )}
      {p.type === 'hysteria2' && (
        <div className="form-grid">
          <Field label={t('上行带宽 (Mbps)')} htmlFor="f-up"><input id="f-up" className="input mono" type="number" value={p.upMbps ?? ''} onChange={(e) => setP({ upMbps: Number(e.target.value) || undefined })} /></Field>
          <Field label={t('下行带宽 (Mbps)')} htmlFor="f-down"><input id="f-down" className="input mono" type="number" value={p.downMbps ?? ''} onChange={(e) => setP({ downMbps: Number(e.target.value) || undefined })} /></Field>
          <label className="chk full"><input type="checkbox" checked={p.obfs === 'salamander'} onChange={(e) => setP({ obfs: e.target.checked ? 'salamander' : undefined })} />{t('Salamander 混淆（密码自动生成）')}</label>
        </div>
      )}
      {p.type === 'tuic' && (
        <Field label={t('拥塞控制')} htmlFor="f-cc">
          <select id="f-cc" className="input mono" value={p.congestionControl ?? 'bbr'} onChange={(e) => setP({ congestionControl: e.target.value })}>
            <option>bbr</option><option>cubic</option><option>new_reno</option>
          </select>
        </Field>
      )}
      {p.type === 'shadowtls' && (
        <div className="form-grid">
          <Field label={t('握手服务器')} htmlFor="f-hs"><input id="f-hs" className="input mono" value={p.handshakeServer ?? ''} onChange={(e) => setP({ handshakeServer: e.target.value })} placeholder="www.example.com:443" /></Field>
          <Field label={t('内层入站标签')} htmlFor="f-det" hint={t('常与 SS 2022 串联')}><input id="f-det" className="input mono" value={p.detour ?? ''} onChange={(e) => setP({ detour: e.target.value })} /></Field>
        </div>
      )}
      {p.type === 'socks' && <label className="chk"><input type="checkbox" checked={!!p.udp} onChange={(e) => setP({ udp: e.target.checked })} />{t('允许 UDP')}</label>}

      {/* transport specific */}
      {(tp.type === 'ws' || tp.type === 'httpupgrade' || tp.type === 'xhttp') && (
        <div className="form-grid">
          <Field label={t('路径')} htmlFor="f-path" hint={t('留空则随机生成')}><input id="f-path" className="input mono" value={tp.path ?? ''} onChange={(e) => setT({ path: e.target.value })} placeholder={t('/随机')} /></Field>
          <Field label="Host" htmlFor="f-host"><input id="f-host" className="input mono" value={tp.host ?? ''} onChange={(e) => setT({ host: e.target.value })} /></Field>
          {tp.type === 'xhttp' && (
            <Field label={t('XHTTP 模式')} htmlFor="f-mode">
              <select id="f-mode" className="input mono" value={tp.mode ?? 'auto'} onChange={(e) => setT({ mode: e.target.value })}>
                <option>auto</option><option>packet-up</option><option>stream-up</option><option>stream-one</option>
              </select>
            </Field>
          )}
        </div>
      )}
      {(tp.type === 'tcp' || tp.type === 'ws' || tp.type === 'httpupgrade') && hasFeature(caps, 'proxy-protocol') && (
        <label className="chk"><input type="checkbox" checked={!!tp.acceptProxyProtocol} onChange={(e) => setT({ acceptProxyProtocol: e.target.checked || undefined })} />
          {t('接收 PROXY protocol（前面有 HAProxy / Nginx stream / 中转并发送 PROXY 头时开启，否则直连客户端会失败）')}</label>
      )}
      {tp.type === 'grpc' && (
        <Field label="Service Name" htmlFor="f-svc" hint={t('留空则随机生成')}><input id="f-svc" className="input mono" value={tp.serviceName ?? ''} onChange={(e) => setT({ serviceName: e.target.value })} /></Field>
      )}

      {/* security specific */}
      {s.type === 'tls' && (
        <div className="form-grid">
          <Field label={t('SNI / 证书域名')} htmlFor="f-sni"><input id="f-sni" className="input mono" value={s.serverName ?? ''} onChange={(e) => setS({ serverName: e.target.value })} placeholder={t('默认节点域名')} /></Field>
          <Field label="ALPN" htmlFor="f-alpn"><input id="f-alpn" className="input mono" value={(s.alpn ?? []).join(',')} onChange={(e) => setS({ alpn: fromLines(e.target.value) })} placeholder="h2,http/1.1" /></Field>
          {certs && (
            <Field label={t('证书')} htmlFor="f-cert" full hint={t('不选则为该域名自动签发自签证书（客户端将跳过校验）')}>
              <select id="f-cert" className="input" value={s.certificateId ?? ''} onChange={(e) => setS({ certificateId: e.target.value || undefined })}>
                <option value="">{t('自动（自签）')}</option>
                {certs.map((c) => <option key={c.id} value={c.id}>{c.domain} · {c.provider}</option>)}
              </select>
            </Field>
          )}
        </div>
      )}
      {s.type === 'reality' && (
        <>
          <Field label={t('REALITY 目标站点')} htmlFor="f-dest" hint={t('私钥 / 公钥 / shortId 自动生成，私钥加密存储。目标需支持 TLS 1.3 + h2，最好支持 X25519MLKEM768')}>
            <div className="row" style={{ flexWrap: 'nowrap' }}>
              <input id="f-dest" className="input mono" style={{ flex: 1 }} value={s.realityDest ?? ''} onChange={(e) => setDest(e.target.value)} placeholder="www.microsoft.com:443" />
              <button className="btn" type="button" onClick={() => setScanning(true)}>{t('扫描')}</button>
            </div>
          </Field>
          {scanning && <RealityScanner nodeId={nodeId} onClose={() => setScanning(false)}
            onPick={(target, names) => setS({ realityDest: target, realityServerNames: names.length ? names : undefined })} />}
          <Field label="SNI（Server Names）" htmlFor="f-sn" hint={t('必须是目标站点证书里的域名；回车或逗号添加。扫描结果点「使用」会自动填入证书中的全部域名，订阅给客户端用第一个')}>
            <TagInput id="f-sn" value={s.realityServerNames ?? []} onChange={(v) => setS({ realityServerNames: v.length ? v : undefined })} placeholder={destHost || 'www.microsoft.com'} />
          </Field>
          {snMismatch && (
            <div className="note warn">{t('SNI 与目标站点 {host} 不一致：客户端用 {sni} 握手时，目标站点会拒绝或返回别的证书，节点将连不上。', { host: destHost, sni: first })}
              <button className="btn sm" type="button" style={{ marginLeft: 8 }} onClick={() => setS({ realityServerNames: [destHost] })}>{t('改为 {host}', { host: destHost })}</button>
              <button className="btn sm" type="button" style={{ marginLeft: 6 }} onClick={() => setScanning(true)}>{t('扫描证书域名')}</button>
            </div>
          )}
          {s.realityPublicKey && <div className="hint mono">{t('公钥：{key} · shortId：{ids}', { key: s.realityPublicKey, ids: (s.realityShortIds ?? []).join(', ') })}</div>}
          {isXray && hasFeature(caps, 'reality-pq-only') && (
            <div className="note warn">{t('当前 Xray 版本（26.9.8 起）的 REALITY 只接受带 X25519MLKEM768 密钥交换的客户端：sing-box、Clash / mihomo 客户端无法连接，其订阅会自动跳过此节点。需要兼容这些客户端时，请把该入站放到 sing-box 内核，或把节点 Xray 切换到 26.7.28 及更早版本。')}</div>
          )}
          {isXray &&
            <RealityAdvanced s={s} setS={setS} mldsa={hasFeature(caps, 'reality-mldsa65')} />}
        </>
      )}
      <div className="toggle"><span>{t('流量嗅探（http / tls / quic）')}</span><Switch label={t('嗅探')} on={draft.sniffing.enabled} onChange={(v) => onChange({ ...draft, sniffing: { ...draft.sniffing, enabled: v } })} /></div>
      {draft.sniffing.enabled && isXray && (
        <details className="more"><summary>{t('嗅探选项')}</summary><div><SniffExtras sn={draft.sniffing} set={(sn) => onChange({ ...draft, sniffing: sn })} /></div></details>
      )}
    </>
  )
}

// hostOf strips the port from a REALITY target ("host:443", "[v6]:443").
function hostOf(dest?: string): string {
  const d = (dest ?? '').trim()
  if (d.startsWith('[')) return d.slice(1, d.indexOf(']') > 0 ? d.indexOf(']') : undefined)
  const i = d.lastIndexOf(':')
  return (i > 0 && d.indexOf(':') === i ? d.slice(0, i) : d).toLowerCase()
}

const isDomain = (h: string) => /[a-z]/i.test(h) && !h.includes(':') && h.includes('.')

// baseDomain is the last two labels (three for co.uk-style suffixes).
function baseDomain(h: string): string {
  const p = h.toLowerCase().replace(/\.$/, '').split('.')
  const n = p.length >= 3 && /^(co|com|net|org|gov|edu|ac)$/.test(p[p.length - 2]) && p[p.length - 1].length === 2 ? 3 : 2
  return p.slice(-n).join('.')
}
