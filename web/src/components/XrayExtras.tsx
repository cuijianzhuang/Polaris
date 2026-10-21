// Newer Xray inbound options: VLESS Encryption, Vision testseed, REALITY
// limits / ML-DSA-65 signatures, and the REALITY target scanner.
import { useEffect, useState } from 'react'
import { get, post, put } from '../api'
import type { CapSet, LimitFallback, ProtocolSpec, RealityScanResult, SecuritySpec, SniffSpec } from '../types'
import { Field, Modal, Switch, fromLines, toLines, useAction } from './ui'
import { t } from '../i18n'

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
  }, t('已生成，保存后生效'))
  return (
    <Field label="VLESS Encryption" full hint={available
      ? t('ML-KEM-768 + X25519 混合密钥交换，抗量子；客户端需 Xray 25.9+ 或 mihomo 1.19.14+，sing-box、Surge 等不支持的客户端会跳过该节点。服务端密钥加密存储、不回显。')
      : t('需要 Xray 25.9.11 及以上的内核')}>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <button className="btn sm" type="button" disabled={busy || !available} onClick={() => gen('x25519')}>{t('生成（X25519 认证）')}</button>
        <button className="btn sm" type="button" disabled={busy || !available} onClick={() => gen('mlkem768')}>{t('生成（ML-KEM-768 认证）')}</button>
        {on && <button className="btn sm danger" type="button" onClick={() => setP({ decryption: undefined, encryption: undefined })}>{t('关闭@@off')}</button>}
      </div>
      {on
        ? <textarea className="input mono" readOnly style={{ minHeight: 56, marginTop: 8, fontSize: 12 }} value={p.encryption} aria-label={t('客户端 encryption')} />
        : <div className="hint">{t('未启用（decryption / encryption 为 none）')}</div>}
    </Field>
  )
}

export function TestSeed({ p, setP }: { p: ProtocolSpec; setP: (x: Partial<ProtocolSpec>) => void }) {
  const seed = p.testSeed?.length === 4 ? p.testSeed : undefined
  const vals = seed ?? [900, 500, 900, 256]
  return (
    <Field label="Vision testseed" hint={t('调整 Vision 填充；留默认即可，仅对 xtls-rprx-vision 客户端生效')}>
      <div className="row" style={{ gap: 6, alignItems: 'center' }}>
        <Switch label={t('自定义 testseed')} on={!!seed} onChange={(v) => setP({ testSeed: v ? [900, 500, 900, 256] : undefined })} />
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
      <Field label={t('{label} · 起始字节数', { label })}><input className="input mono" inputMode="numeric" value={cur.afterBytes ?? ''} onChange={(e) => upd('afterBytes', e.target.value)} /></Field>
      <Field label={t('每秒字节数')}><input className="input mono" inputMode="numeric" value={cur.bytesPerSec ?? ''} onChange={(e) => upd('bytesPerSec', e.target.value)} /></Field>
      <Field label={t('突发每秒字节数')}><input className="input mono" inputMode="numeric" value={cur.burstBytesPerSec ?? ''} onChange={(e) => upd('burstBytesPerSec', e.target.value)} /></Field>
    </div>
  )
}

export function RealityAdvanced({ s, setS, mldsa }: { s: SecuritySpec; setS: (x: Partial<SecuritySpec>) => void; mldsa: boolean }) {
  const { busy, run } = useAction()
  return (
    <details className="more">
      <summary>{t('REALITY 高级选项')}</summary>
      <div style={{ display: 'grid', gap: 12, marginTop: 10 }}>
      <div className="form-grid">
        <Field label={t('最大时间差 (ms)')} hint={t('0 = 不限制')}><input className="input mono" inputMode="numeric" value={s.realityMaxTimeDiff ?? ''} onChange={(e) => setS({ realityMaxTimeDiff: numOr(e.target.value) })} placeholder="0" /></Field>
        <Field label="SpiderX" hint={t('客户端爬虫初始路径')}><input className="input mono" value={s.realitySpiderX ?? ''} onChange={(e) => setS({ realitySpiderX: e.target.value || undefined })} placeholder="/" /></Field>
        <Field label={t('最小客户端版本')} hint={t('留空则不限制（Xray 26.7.x 自带的 26.3.27 下限会拒绝 sing-box / Clash 客户端，面板会自动放开）')}>
          <input className="input mono" value={s.realityMinClientVer ?? ''} onChange={(e) => setS({ realityMinClientVer: e.target.value || undefined })} placeholder="x.y.z" />
        </Field>
        <Field label={t('最大客户端版本')} hint={t('一般留空：设置后更高版本的客户端都会被拒绝')}><input className="input mono" value={s.realityMaxClientVer ?? ''} onChange={(e) => setS({ realityMaxClientVer: e.target.value || undefined })} placeholder="x.y.z" /></Field>
      </div>
      {s.realityMaxClientVer && verLess(s.realityMaxClientVer, '26.0.0') && (
        <div className="note bad">{t('最大客户端版本 {ver} 会拒绝所有新版 Xray 客户端（v2rayN、Xray 26.x 等），这些客户端将连不上。', { ver: s.realityMaxClientVer })}
          <button className="btn sm" type="button" style={{ marginLeft: 8 }} onClick={() => setS({ realityMaxClientVer: undefined })}>{t('清空')}</button>
        </div>
      )}
      <Field label={t('ML-DSA-65 签名')} hint={mldsa
        ? t('服务端用后量子签名证明身份；客户端填入校验公钥（订阅中的 pqv）后才会校验。目标站证书链需至少约 3.5 KB（扫描结果中标「ML-DSA 可用」），否则连接会失败。种子加密存储、不回显')
        : t('需要 Xray 25.9.11 及以上的内核')}>
        <div className="row" style={{ gap: 8 }}>
          <button className="btn sm" type="button" disabled={busy || !mldsa} onClick={() => run(async () => {
            const r = await post<{ seed: string; verify: string }>('/tools/mldsa65', {})
            setS({ realityMldsa65Seed: r.seed, realityMldsa65Verify: r.verify })
          }, t('已生成，保存后生效'))}>{s.realityMldsa65Verify ? t('重新生成') : t('生成')}</button>
          {s.realityMldsa65Verify && <button className="btn sm danger" type="button" onClick={() => setS({ realityMldsa65Seed: undefined, realityMldsa65Verify: undefined })}>{t('清除')}</button>}
        </div>
        {s.realityMldsa65Verify && <textarea className="input mono" readOnly style={{ minHeight: 56, marginTop: 8, fontSize: 12 }} value={s.realityMldsa65Verify} aria-label="mldsa65 verify" />}
      </Field>
      <div className="hint" style={{ margin: '4px 0 6px' }}>{t('限制回落：对未通过认证、被转发到目标站点的流量限速（探测者看到的是限速后的目标站点）')}</div>
      <LimitRow label={t('上传')} v={s.realityLimitUpload} set={(v) => setS({ realityLimitUpload: v })} />
      <LimitRow label={t('下载')} v={s.realityLimitDownload} set={(v) => setS({ realityLimitDownload: v })} />
      </div>
    </details>
  )
}

export function SniffExtras({ sn, set }: { sn: SniffSpec; set: (x: SniffSpec) => void }) {
  return (
    <div className="form-grid">
      <label className="chk"><input type="checkbox" checked={!!sn.metadataOnly} onChange={(e) => set({ ...sn, metadataOnly: e.target.checked || undefined })} />{t('仅元数据（不读取流量内容）')}</label>
      <label className="chk"><input type="checkbox" checked={!!sn.routeOnly} onChange={(e) => set({ ...sn, routeOnly: e.target.checked || undefined })} />{t('仅用于路由（不改写目标地址）')}</label>
      <Field label={t('排除的 IP')} hint={t('每行一个：IP / CIDR / geoip:xx（Xray）')}>
        <textarea className="input mono" style={{ minHeight: 52 }} value={toLines(sn.ipsExcluded)} onChange={(e) => set({ ...sn, ipsExcluded: fromLines(e.target.value) })} />
      </Field>
      <Field label={t('排除的域名')} hint={t('每行一个：domain:xx / geosite:xx（Xray）')}>
        <textarea className="input mono" style={{ minHeight: 52 }} value={toLines(sn.domainsExcluded)} onChange={(e) => set({ ...sn, domainsExcluded: fromLines(e.target.value) })} />
      </Field>
    </div>
  )
}

function scanSource(from: string) {
  const labels: Record<string, string> = { node: t('节点'), panel: t('面板'), 'panel-fallback-offline': t('面板（节点离线）'), 'panel-fallback-unsupported': t('面板（需升级 Agent）'), '': t('未执行验证') }
  return labels[from] ?? from
}

// RealityScanner probes candidates (or domains / IPs / CIDRs, several at
// once) for a REALITY target: preserve the dial address and verify the SNI
// on that endpoint before returning a usable target/serverNames pair.
export function RealityScanner({ nodeId, initial, onPick, onClose }: {
  nodeId?: string; initial?: string; onPick: (target: string, names: string[]) => void; onClose: () => void
}) {
  const [q, setQ] = useState(initial ?? '')
  const [res, setRes] = useState<{ results: RealityScanResult[]; from: string; ms: number; warnings?: string[]; shodan?: { total: number; candidates: number; query: string; pagesRead: number } } | null>(null)
  const [okOnly, setOkOnly] = useState(false)
  const [editing, setEditing] = useState<{ value: string; def: string } | null>(null)
  const { busy, run } = useAction()
  const scan = (target = q) => run(async () => {
    const t0 = performance.now()
    const r = await post<{ results: RealityScanResult[]; from: string }>('/tools/reality-scan', { target: target.trim(), nodeId })
    setRes({ ...r, ms: Math.round(performance.now() - t0) })
  })
  useEffect(() => { scan() }, []) // eslint-disable-line react-hooks/exhaustive-deps
  const editCandidates = () => run(async () => {
    const r = await get<{ candidates: string; default: string }>('/tools/reality-candidates')
    setEditing({ value: r.candidates ? r.candidates.split(/[,\s]+/).filter(Boolean).join('\n') : r.default, def: r.default })
  })
  const saveCandidates = () => run(async () => {
    const v = editing!.value.split(/[,\s]+/).filter(Boolean).join(',')
    await put('/settings', { reality_scan_candidates: v === editing!.def.split('\n').join(',') ? '' : v })
    setEditing(null)
    if (!q.trim()) scan('')
  }, t('候选目标已保存'))
  const [shodan, setShodan] = useState<ShodanInfo | null>(null)
  const openShodan = () => run(async () => setShodan(await get<ShodanInfo>('/tools/shodan')))
  const rows = (res?.results ?? []).filter((r) => !okOnly || r.ok)
  const okCount = (res?.results ?? []).filter((r) => r.ok).length
  return (
    <Modal xwide title={t('REALITY 目标扫描器')} onClose={onClose} footer={<>
      <button className="btn" type="button" disabled={busy} onClick={editCandidates}>{t('编辑默认候选')}</button>
      <button className="btn" type="button" style={{ marginRight: 'auto' }} disabled={busy} onClick={() => (shodan ? setShodan(null) : openShodan())}>{t('从 Shodan 发现')}</button>
      <button className="btn" type="button" disabled={busy} onClick={() => scan()}>{busy ? t('扫描中…') : t('重新扫描')}</button>
      <button className="btn pri" type="button" onClick={onClose}>{t('关闭')}</button>
    </>}>
      <div className="hint" style={{ marginBottom: 8 }}>{t('验证域名，或扫描 IP / CIDR 范围（每段最多 256 个地址），从证书中发现新的 REALITY 目标；可用逗号或空格分隔多个。留空则探测默认候选。节点在线时由节点发起，延迟以节点为准。')}</div>
      <div className="joined">
        <input className="input mono" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('IP、CIDR 或域名 — 留空使用默认候选')}
          onKeyDown={(e) => { if (e.key === 'Enter') scan() }} aria-label={t('扫描目标')} />
        <button className="btn pri" type="button" disabled={busy} onClick={() => scan()}>{busy ? t('扫描中…') : t('扫描')}</button>
      </div>
      {editing && (
        <div className="card" style={{ marginTop: 12, display: 'grid', gap: 8, padding: 14 }}>
          <div className="small">{t('默认候选（每行一个 host[:port]，也可以写 CIDR）')}</div>
          <textarea className="input mono" style={{ minHeight: 140 }} value={editing.value} onChange={(e) => setEditing({ ...editing, value: e.target.value })} />
          <div className="row">
            <button className="btn sm pri" type="button" disabled={busy} onClick={saveCandidates}>{t('保存')}</button>
            <button className="btn sm" type="button" onClick={() => setEditing({ ...editing, value: editing.def })}>{t('恢复内置列表')}</button>
            <button className="btn sm" type="button" onClick={() => setEditing(null)}>{t('取消')}</button>
          </div>
        </div>
      )}
      {shodan && <ShodanPanel info={shodan} nodeId={nodeId} busy={busy} run={run} onInfo={setShodan}
        onResult={(r, ms) => setRes({ results: r.results, from: r.from, ms, warnings: r.warnings, shodan: { total: r.total, candidates: r.candidates.length, query: r.query, pagesRead: r.pagesRead } })} />}
      {res?.shodan && <div className="small sub" style={{ marginTop: 10 }}>{t('已读取 {n} 页', { n: res.shodan.pagesRead })} · {t('Shodan 共有 {total} 条匹配，取了 {n} 个候选逐个验证 · 查询：{q}', { total: res.shodan.total, n: res.shodan.candidates, q: res.shodan.query })}</div>}
      {res && (
        <div className="row small sub" style={{ marginTop: 12, justifyContent: 'space-between' }}>
          <span>{t('共 {total} 个目标，基础检测通过 {ok} 个 · 来源：{from} · 用时 {sec} 秒', { total: res.results.length, ok: okCount, from: scanSource(res.from), sec: (res.ms / 1000).toFixed(1) })}</span>
          <label className="chk" style={{ margin: 0 }}><input type="checkbox" checked={okOnly} onChange={(e) => setOkOnly(e.target.checked)} />{t('只看基础检测通过')}</label>
        </div>
      )}
      {res?.from.startsWith('panel-fallback-') && <div className="note warn">{t('当前结果由面板验证，节点连通性与延迟需在节点恢复或升级 Agent 后复测。')}</div>}
      {(res?.warnings ?? []).map((warning, i) => <div className="note warn" key={i}>{warning}</div>)}
      <div className="hint">{t('基础检测覆盖 TLS 1.3、HTTP/2 和 SNI 证书校验；HTTP 首页行为、CDN 与长期稳定性需另行核对。')}</div>
      <div className="scroll scan-table" style={{ marginTop: 8 }} aria-busy={busy}>
        <table>
          <thead><tr><th>{t('目标')}</th><th>{t('状态')}</th><th>TLS</th><th>ALPN</th><th>{t('交换')}</th><th>{t('证书')}</th><th>{t('证书链')}</th><th>{t('握手延迟')}</th><th /></tr></thead>
          <tbody>
            {!res && busy && <tr><td colSpan={9} className="sub">{t('正在扫描…')}</td></tr>}
            {res && rows.length === 0 && <tr><td colSpan={9} className="sub">{okOnly ? t('没有可用的目标') : t('没有响应的目标')}</td></tr>}
            {rows.map((r) => (
              <tr key={r.target + '|' + (r.serverName ?? '')}>
                <td className="mono small">{r.target}
                  {r.serverName && <div>SNI: {r.serverName}</div>}
                  {r.address && r.address !== r.target && <div className="sub" style={{ fontSize: 11 }}>{t('连接地址：{address}', { address: r.address })}</div>}
                  {(r.asn || r.country) && <div className="sub" style={{ fontSize: 11 }}>{[r.asn, r.country].filter(Boolean).join(' · ')}</div>}
                  {r.org && <div className="sub" style={{ fontSize: 11, maxWidth: 200, whiteSpace: 'normal' }}>{r.org}</div>}
                </td>
                <td><span className={'tag ' + (r.ok ? 'ok' : 'bad')}>{r.ok ? t('基础检测通过') : t('未通过')}</span>{r.problem && <div className="sub" style={{ fontSize: 11, maxWidth: 180, whiteSpace: 'normal' }}>{r.problem}</div>}</td>
                <td className="mono small">{r.tls ?? '—'}</td>
                <td className="mono small">{r.alpn || '—'}</td>
                <td className="mono small">{r.keyShare ?? '—'}</td>
                <td className="small" title={[r.issuer && t('签发：{v}', { v: r.issuer }), r.notAfter && t('到期：{v}', { v: r.notAfter.slice(0, 10) }), r.certNames?.length && t('域名：{v}', { v: r.certNames.join(', ') })].filter(Boolean).join('\n')}>
                  {r.certName ?? '—'}
                  {(r.issuer || r.notAfter) && <div className="sub" style={{ fontSize: 11 }}>{r.issuer}{r.notAfter ? ' · ' + t('{date} 到期', { date: r.notAfter.slice(0, 10) }) : ''}</div>}
                </td>
                <td className="mono small">{r.chainSize ? `${r.chainSize} B` : '—'}{r.tls && <div className={'sub'} style={{ fontSize: 11 }} title={t('REALITY ML-DSA-65 签名需要证书链至少约 3.5 KB')}>{r.mldsa ? t('ML-DSA 可用') : t('ML-DSA 不可用')}</div>}</td>
                <td className="mono small">{r.latencyMs != null ? t('{n} 毫秒', { n: r.latencyMs }) : '—'}</td>
                <td>{r.ok && <button className="btn sm link" type="button" onClick={() => { onPick(r.target, r.serverName ? [r.serverName] : r.names ?? []); onClose() }}>{t('使用')}</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Modal>
  )
}

type ShodanInfo = { configured: boolean; defaultQuery: string; credits?: number; plan?: string; error?: string }
type DiscoverResult = { query: string; total: number; candidates: string[]; results: RealityScanResult[]; from: string; warnings?: string[]; pagesRead: number }

// ShodanPanel finds REALITY candidates in Shodan's index of HTTPS servers
// (with the API key saved as an encrypted setting), then checks each one
// with the normal scan.
function ShodanPanel({ info, nodeId, busy, run, onInfo, onResult }: {
  info: ShodanInfo; nodeId?: string; busy: boolean
  run: <T>(fn: () => Promise<T>, ok?: string) => Promise<T | undefined>
  onInfo: (i: ShodanInfo) => void; onResult: (r: DiscoverResult, ms: number) => void
}) {
  const [key, setKey] = useState('')
  const [replacing, setReplacing] = useState(false)
  const [country, setCountry] = useState('')
  const [query, setQuery] = useState('')
  const [pages, setPages] = useState(1)
  const [asn, setAsn] = useState('')
  const [network, setNetwork] = useState('')
  const saveKey = () => run(async () => {
    await put('/settings', { shodan_api_key: key.trim() })
    setKey('')
    setReplacing(false)
    onInfo(await get<ShodanInfo>('/tools/shodan'))
  }, t('Shodan API Key 已保存'))
  const discover = () => run(async () => {
    const t0 = performance.now()
    const r = await post<DiscoverResult>('/tools/reality-discover', { query: query.trim(), country: country.trim(), asn: asn.trim(), network: network.trim(), pages, nodeId })
    onResult(r, Math.round(performance.now() - t0))
    try { onInfo(await get<ShodanInfo>('/tools/shodan')) } catch { /* Keep completed results when credit refresh is unavailable. */ }
  })
  return (
    <div className="card" style={{ marginTop: 12, display: 'grid', gap: 10, padding: 14 }}>
      <div className="small">{t('从 Shodan 获取候选 IP 和证书域名，保留原始地址并使用候选 SNI 实测。ASN、组织和国家信息来自 Shodan 索引。')}</div>
      {!info.configured || replacing ? (
        <div className="joined">
          <input className="input mono" type="password" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)}
            placeholder={info.configured ? t('输入新的 Key 以替换') : t('Shodan API Key（加密保存）')} aria-label="Shodan API Key" />
          <button className="btn pri" type="button" disabled={busy || !key.trim()} onClick={saveKey}>{t('保存 Key')}</button>
        </div>
      ) : (
        <div className="row small" style={{ justifyContent: 'space-between' }}>
          <span>{info.error ? <span style={{ color: 'var(--bad)' }}>{info.error}</span> : t('已配置 Key · 套餐 {plan} · 剩余查询额度 {n}', { plan: info.plan || '—', n: info.credits ?? 0 })}</span>
          <button className="btn sm link" type="button" onClick={() => setReplacing(true)}>{t('更换 Key')}</button>
        </div>
      )}
      {info.configured && <>
        <div className="form-grid">
          <Field label={t('ASN（可选）')} hint={t('按运营商网络筛选，例如 AS13335；同 ASN 不代表同机房')}>
            <input className="input mono" value={asn} onChange={(e) => setAsn(e.target.value.toUpperCase())} placeholder="AS13335" />
          </Field>
          <Field label={t('网段（可选）')} hint={t('只查询 Shodan 索引内该 CIDR 的站点，例如 203.0.113.0/24')}>
            <input className="input mono" value={network} onChange={(e) => setNetwork(e.target.value)} placeholder="203.0.113.0/24" />
          </Field>
          <Field label={t('国家 / 地区')} hint={t('两位代码，如 JP、SG、US；选离节点近的更好')}>
            <input className="input mono" maxLength={2} value={country} onChange={(e) => setCountry(e.target.value.toUpperCase())} placeholder="JP" />
          </Field>
          <Field label={t('页数')} hint={t('每页最多 100 个候选，消耗 1 个查询额度')}>
            <select className="input" value={pages} onChange={(e) => setPages(Number(e.target.value))}>{[1, 2, 3].map((n) => <option key={n} value={n}>{n}</option>)}</select>
          </Field>
        </div>
        <Field label={t('自定义查询（可选）')} hint={t('留空使用：{q}。可加 org:、asn:、ssl.cert.issuer.cn: 等 Shodan 过滤条件', { q: info.defaultQuery })}>
          <input className="input mono" value={query} onChange={(e) => setQuery(e.target.value)} placeholder={info.defaultQuery} />
        </Field>
        <div><button className="btn pri" type="button" disabled={busy} onClick={discover}>{busy ? t('发现并验证中…') : t('发现并验证')}</button></div>
      </>}
    </div>
  )
}

// verLess compares dotted versions numerically.
function verLess(a: string, b: string): boolean {
  const x = a.split('.').map((n) => Number(n) || 0), y = b.split('.').map((n) => Number(n) || 0)
  for (let i = 0; i < 3; i++) if ((x[i] ?? 0) !== (y[i] ?? 0)) return (x[i] ?? 0) < (y[i] ?? 0)
  return false
}
