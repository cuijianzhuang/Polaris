// Newer Xray inbound options: VLESS Encryption, Vision testseed, REALITY
// limits / ML-DSA-65 signatures, and the REALITY target scanner.
import { useState } from 'react'
import { post } from '../api'
import type { CapSet, LimitFallback, ProtocolSpec, RealityScanResult, SecuritySpec, SniffSpec } from '../types'
import { Field, Modal, Switch, fromLines, toLines, useAction } from './ui'

export const hasFeature = (caps: CapSet[], f: string) => caps.some((c) => c.features?.includes(f))

const numOr = (v: string) => (v.trim() === '' ? undefined : Math.max(0, Number(v) || 0))

// VLESS Encryption: the decryption key is write-only (never returned by
// the API); the client key shows whether it is on.
export function VlessEncryption({ p, setP, available }: { p: ProtocolSpec; setP: (x: Partial<ProtocolSpec>) => void; available: boolean }) {
  const { busy, run } = useAction()
  const on = !!p.encryption
  const gen = (auth: string) => run(async () => {
    const r = await post<{ decryption: string; encryption: string }>('/tools/vlessenc', { auth })
    setP({ decryption: r.decryption, encryption: r.encryption })
  }, '已生成，保存后生效')
  return (
    <Field label="VLESS Encryption" full hint={available
      ? 'ML-KEM-768 + X25519 混合密钥交换，抗量子；客户端需 Xray 25.9+ 或 mihomo 1.19.14+，sing-box、Surge 等不支持的客户端会跳过该节点。服务端密钥加密存储、不回显。'
      : '需要 Xray 25.9.11 及以上的内核'}>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <button className="btn sm" type="button" disabled={busy || !available} onClick={() => gen('x25519')}>生成（X25519 认证）</button>
        <button className="btn sm" type="button" disabled={busy || !available} onClick={() => gen('mlkem768')}>生成（ML-KEM-768 认证）</button>
        {on && <button className="btn sm danger" type="button" onClick={() => setP({ decryption: undefined, encryption: undefined })}>关闭</button>}
      </div>
      {on
        ? <textarea className="input mono" readOnly style={{ minHeight: 56, marginTop: 8, fontSize: 12 }} value={p.encryption} aria-label="客户端 encryption" />
        : <div className="hint">未启用（decryption / encryption 为 none）</div>}
    </Field>
  )
}

export function TestSeed({ p, setP }: { p: ProtocolSpec; setP: (x: Partial<ProtocolSpec>) => void }) {
  const seed = p.testSeed?.length === 4 ? p.testSeed : undefined
  const vals = seed ?? [900, 500, 900, 256]
  return (
    <Field label="Vision testseed" hint="调整 Vision 填充；留默认即可，仅对 xtls-rprx-vision 客户端生效">
      <div className="row" style={{ gap: 6, alignItems: 'center' }}>
        <Switch label="自定义 testseed" on={!!seed} onChange={(v) => setP({ testSeed: v ? [900, 500, 900, 256] : undefined })} />
        {vals.map((v, i) => (
          <input key={i} className="input mono" style={{ width: 72 }} disabled={!seed} inputMode="numeric" value={v} aria-label={`testseed ${i + 1}`}
            onChange={(e) => { const n = [...vals]; n[i] = Number(e.target.value) || 0; setP({ testSeed: n }) }} />
        ))}
      </div>
    </Field>
  )
}

function LimitRow({ label, v, set }: { label: string; v?: LimitFallback; set: (v?: LimitFallback) => void }) {
  const cur = v ?? {}
  const upd = (k: keyof LimitFallback, x: string) => {
    const n = { ...cur, [k]: numOr(x) }
    set(n.afterBytes || n.bytesPerSec || n.burstBytesPerSec ? n : undefined)
  }
  return (
    <div className="form-grid">
      <Field label={`${label} · 起始字节数`}><input className="input mono" inputMode="numeric" value={cur.afterBytes ?? ''} onChange={(e) => upd('afterBytes', e.target.value)} /></Field>
      <Field label="每秒字节数"><input className="input mono" inputMode="numeric" value={cur.bytesPerSec ?? ''} onChange={(e) => upd('bytesPerSec', e.target.value)} /></Field>
      <Field label="突发每秒字节数"><input className="input mono" inputMode="numeric" value={cur.burstBytesPerSec ?? ''} onChange={(e) => upd('burstBytesPerSec', e.target.value)} /></Field>
    </div>
  )
}

export function RealityAdvanced({ s, setS, mldsa }: { s: SecuritySpec; setS: (x: Partial<SecuritySpec>) => void; mldsa: boolean }) {
  const { busy, run } = useAction()
  return (
    <details className="more">
      <summary>REALITY 高级选项</summary>
      <div style={{ display: 'grid', gap: 12, marginTop: 10 }}>
      <div className="form-grid">
        <Field label="最大时间差 (ms)" hint="0 = 不限制"><input className="input mono" inputMode="numeric" value={s.realityMaxTimeDiff ?? ''} onChange={(e) => setS({ realityMaxTimeDiff: numOr(e.target.value) })} placeholder="0" /></Field>
        <Field label="SpiderX" hint="客户端爬虫初始路径"><input className="input mono" value={s.realitySpiderX ?? ''} onChange={(e) => setS({ realitySpiderX: e.target.value || undefined })} placeholder="/" /></Field>
        <Field label="最小客户端版本" hint="Xray 26.7.x 默认只接受 26.3.27 起的客户端；填 1.0.0 可兼容旧客户端">
          <input className="input mono" value={s.realityMinClientVer ?? ''} onChange={(e) => setS({ realityMinClientVer: e.target.value || undefined })} placeholder="x.y.z" />
        </Field>
        <Field label="最大客户端版本"><input className="input mono" value={s.realityMaxClientVer ?? ''} onChange={(e) => setS({ realityMaxClientVer: e.target.value || undefined })} placeholder="x.y.z" /></Field>
      </div>
      <Field label="ML-DSA-65 签名" hint={mldsa
        ? '服务端用后量子签名证明身份；客户端填入校验公钥（订阅中的 pqv）后才会校验。目标站证书链需至少约 3.5 KB（扫描结果中标「ML-DSA 可用」），否则连接会失败。种子加密存储、不回显'
        : '需要 Xray 25.9.11 及以上的内核'}>
        <div className="row" style={{ gap: 8 }}>
          <button className="btn sm" type="button" disabled={busy || !mldsa} onClick={() => run(async () => {
            const r = await post<{ seed: string; verify: string }>('/tools/mldsa65', {})
            setS({ realityMldsa65Seed: r.seed, realityMldsa65Verify: r.verify })
          }, '已生成，保存后生效')}>{s.realityMldsa65Verify ? '重新生成' : '生成'}</button>
          {s.realityMldsa65Verify && <button className="btn sm danger" type="button" onClick={() => setS({ realityMldsa65Seed: undefined, realityMldsa65Verify: undefined })}>清除</button>}
        </div>
        {s.realityMldsa65Verify && <textarea className="input mono" readOnly style={{ minHeight: 56, marginTop: 8, fontSize: 12 }} value={s.realityMldsa65Verify} aria-label="mldsa65 verify" />}
      </Field>
      <div className="hint" style={{ margin: '4px 0 6px' }}>限制回落：对未通过认证、被转发到目标站点的流量限速（探测者看到的是限速后的目标站点）</div>
      <LimitRow label="上传" v={s.realityLimitUpload} set={(v) => setS({ realityLimitUpload: v })} />
      <LimitRow label="下载" v={s.realityLimitDownload} set={(v) => setS({ realityLimitDownload: v })} />
      </div>
    </details>
  )
}

export function SniffExtras({ sn, set }: { sn: SniffSpec; set: (x: SniffSpec) => void }) {
  return (
    <div className="form-grid">
      <label className="chk"><input type="checkbox" checked={!!sn.metadataOnly} onChange={(e) => set({ ...sn, metadataOnly: e.target.checked || undefined })} />仅元数据（不读取流量内容）</label>
      <label className="chk"><input type="checkbox" checked={!!sn.routeOnly} onChange={(e) => set({ ...sn, routeOnly: e.target.checked || undefined })} />仅用于路由（不改写目标地址）</label>
      <Field label="排除的 IP" hint="每行一个：IP / CIDR / geoip:xx（Xray）">
        <textarea className="input mono" style={{ minHeight: 52 }} value={toLines(sn.ipsExcluded)} onChange={(e) => set({ ...sn, ipsExcluded: fromLines(e.target.value) })} />
      </Field>
      <Field label="排除的域名" hint="每行一个：domain:xx / geosite:xx（Xray）">
        <textarea className="input mono" style={{ minHeight: 52 }} value={toLines(sn.domainsExcluded)} onChange={(e) => set({ ...sn, domainsExcluded: fromLines(e.target.value) })} />
      </Field>
    </div>
  )
}

// RealityScanner probes candidates (or a domain / IP / CIDR) for a REALITY
// target: TLS 1.3, h2, X25519MLKEM768 and a valid certificate.
export function RealityScanner({ nodeId, initial, onPick, onClose }: {
  nodeId?: string; initial?: string; onPick: (target: string, names: string[]) => void; onClose: () => void
}) {
  const [q, setQ] = useState(initial ?? '')
  const [res, setRes] = useState<{ results: RealityScanResult[]; from: string } | null>(null)
  const { busy, run } = useAction()
  const scan = () => run(async () => { setRes(await post('/tools/reality-scan', { target: q.trim(), nodeId })) })
  return (
    <Modal wide title="REALITY 目标扫描" onClose={onClose} footer={<button className="btn" type="button" onClick={onClose}>关闭</button>}>
      <div className="hint" style={{ marginBottom: 8 }}>验证域名，或扫描 IP / CIDR（最多 256 个地址）从证书中发现目标；留空则探测常用候选。节点在线时由节点发起，延迟以节点为准。</div>
      <div className="row" style={{ gap: 8 }}>
        <input className="input mono" style={{ flex: 1 }} value={q} onChange={(e) => setQ(e.target.value)} placeholder="IP、CIDR 或域名 — 留空使用常用候选"
          onKeyDown={(e) => { if (e.key === 'Enter') scan() }} />
        <button className="btn pri" type="button" disabled={busy} onClick={scan}>{busy ? '扫描中…' : '扫描'}</button>
      </div>
      {res && (
        <div className="scroll" style={{ marginTop: 12 }}>
          <div className="hint">来源：{res.from === 'node' ? '节点' : res.from}</div>
          <table>
            <thead><tr><th>目标</th><th>状态</th><th>TLS</th><th>ALPN</th><th>密钥交换</th><th>证书</th><th>证书链</th><th>ML-DSA</th><th>延迟</th><th /></tr></thead>
            <tbody>
              {res.results.length === 0 && <tr><td colSpan={10} className="sub">没有响应的目标</td></tr>}
              {res.results.map((r) => (
                <tr key={r.target}>
                  <td className="mono small">{r.target}</td>
                  <td><span className={'tag ' + (r.ok ? 'ok' : 'bad')}>{r.ok ? '可用' : '不可用'}</span>{r.problem && <div className="small sub">{r.problem}</div>}</td>
                  <td className="mono small">{r.tls ?? '—'}</td>
                  <td className="mono small">{r.alpn || '—'}</td>
                  <td className="mono small">{r.keyShare ?? '—'}</td>
                  <td className="small">{r.certName ?? '—'}</td>
                  <td className="mono small">{r.chainSize ? `${r.chainSize} B` : '—'}</td>
                  <td className="small">{r.tls ? (r.mldsa ? '可用' : '链太短') : '—'}</td>
                  <td className="mono small">{r.latencyMs != null ? `${r.latencyMs} ms` : '—'}</td>
                  <td>{r.ok && <button className="btn sm" type="button" onClick={() => { onPick(r.target, r.names ?? []); onClose() }}>使用</button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Modal>
  )
}
