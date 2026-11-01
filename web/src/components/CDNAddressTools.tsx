import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import CDNRegionSelect from './CDNRegionSelect'
import CDNCheckSchedule, { CDNCheckSummary, type CDNSchedule } from './CDNCheckSchedule'
import { ago, datetime } from '../format'
import { t } from '../i18n'
import type { CDNProfile, Endpoint } from '../types'
import { Empty, Field, Modal, fromLines, toLines, useAction, useLoad, usePoll } from './ui'

type Target = NonNullable<Endpoint['target']>
type Context = { explicitAddress?: string; endpointId: string; inboundId: string; label: string; transport: string; target: Target; warnings: string[] }
type Result = { ok: boolean; partial?: boolean; message: string; target: Target; checkedAt: string; viewpoint: 'panel'; businessVerified: false; warnings?: string[]; stages?: { kind: string; state: string; detail?: string; addresses?: string[] }[] }
type Row = { blockKey?: string; address: string; type: string; source: string; fixed: boolean; blocked: boolean; selected: boolean; reason: string; sources: string[]; reportedAt?: string; measurement?: { latencyMs: number; loss: number; speed?: number }; stale?: boolean; failedAt?: string; check?: Result }
type Tools = { schedule?: CDNSchedule; matchMode?: string; contexts: Context[]; context?: Context; items: Row[]; selected: string[]; scheme: string; port: number; carrier: string }
const stageLabel = (s: string) => ({ origin_dns: t('入口域名解析'), address_dns: t('优选地址解析'), tcp: 'TCP', tls: 'TLS', http: t('HTTP 响应'), transport: t('传输握手') }[s] ?? s)
const stateLabel = (s: string) => ({ passed: t('通过'), failed: t('失败'), partial: t('部分结果'), skipped: t('跳过'), not_tested: t('未测试') }[s] ?? s)
const reasonLabel = (s: string) => ({ check_failed: t('检测失败，已排除'), entry_override: t('入口指定地址'), blocked: t('已禁用'), no_endpoint: t('等待关联入口'), origin_only: t('仅回源域名'), fixed: t('固定追加'), selected: t('按当前策略选中'), not_selected: t('未被当前策略选中') }[s] ?? s)
function Policy({ p, onClose, onSaved }: { p: CDNProfile; onClose: () => void; onSaved: () => void }) {
  const [blocked, setBlocked] = useState(toLines(p.blockedHosts))
  const [policy, setPolicy] = useState(p.stalePolicy || 'source')
  const { busy, run } = useAction()
  return <Modal title={t('地址失效与禁用策略')} onClose={onClose} footer={<>
    <button className="btn" disabled={busy} onClick={onClose}>{t('取消')}</button>
    <button className="btn pri" disabled={busy} onClick={() => run(async () => { await put(`/cdn-profiles/${p.id}/address-policy`, { blockedHosts: fromLines(blocked), stalePolicy: policy }); onSaved() }, t('已保存'))}>{t('保存')}</button>
  </>}>
    <Field label={t('测速结果过期后')} htmlFor="cdn-stale-policy"><select id="cdn-stale-policy" className="input" value={policy} onChange={(e) => setPolicy(e.target.value as 'keep' | 'source')}>
      <option value="source">{t('尝试来源 URL，失败保留旧列表')}</option><option value="keep">{t('保留旧列表，等待测速恢复')}</option>
    </select></Field>
    <p className="hint">{t('过期时限沿用配置档的上报告警小时数；自动维护时尝试备用来源，新的测速结果优先。未配置来源 URL 时保留旧列表。')}</p>
    <Field label={t('禁用地址')} htmlFor="cdn-blocked-addresses"><textarea id="cdn-blocked-addresses" className="input mono" rows={5} value={blocked} onChange={(e) => setBlocked(e.target.value)} /></Field>
    <p className="hint">{t('一行一个 IP 或域名。禁用优先于固定和自动来源；刷新不会使其重新进入订阅。移出禁用列表后恢复参与选择。')}</p>
  </Modal>
}
export default function CDNAddressTools({ p, onMaintain, onChanged }: { p: CDNProfile; onMaintain: () => void; onChanged: () => void }) {
  const [endpoint, setEndpoint] = useState(''), [carrier, setCarrier] = useState(''), [region, setRegion] = useState('')
  const [result, setResult] = useState<Result>(), [policy, setPolicy] = useState(false)
  const [scheduling, setScheduling] = useState(false)
  const signature = JSON.stringify([p.preferredIps, p.pinnedHosts, p.blockedHosts, p.portChecks, p.expandPerIp, p.ipTopN, p.stalePolicy, p.staleHours, p.regionSplit, p.carrierSplit])
  useEffect(() => setResult(undefined), [signature, endpoint, carrier, region])
  const data = useLoad(() => get<Tools>(`/cdn-profiles/${p.id}/address-tools?endpoint=${encodeURIComponent(endpoint)}&carrier=${carrier}&region=${encodeURIComponent(region)}`), [p.id, endpoint, carrier, region, signature])
  usePoll(data.reload, data.data?.schedule?.intervalMinutes ? 10000 : 0)
  const { busy, run } = useAction()
  const contexts = data.data?.contexts ?? [], context = data.data?.context
  // Older panels and mock fixtures can still display the maintained addresses.
  const fallback: Row[] = [...new Set([...p.preferredIps, ...(p.pinnedHosts ?? []), ...(p.blockedHosts ?? [])])].map((address) => ({ address, type: address.includes(':') ? 'ipv6' : /^[\d.]+$/.test(address) ? 'ipv4' : 'domain', source: (p.pinnedHosts ?? []).includes(address) ? 'fixed' : 'retained', fixed: (p.pinnedHosts ?? []).includes(address), blocked: (p.blockedHosts ?? []).includes(address), selected: false, reason: 'no_endpoint', sources: [] }))
  const rows = data.data?.items ?? fallback
  const changed = () => { setResult(undefined); data.reload(); onChanged() }
  return <section className="card cdn-address-tools" aria-busy={data.loading} aria-label={t('优选地址与订阅预览')}>
    <div className="row" style={{ justifyContent: 'space-between', flexWrap: 'wrap', gap: 8 }}><h2>{t('优选地址与订阅预览')} <span className="tag">{rows.length}</span></h2>
      <div className="row" style={{ flexWrap: 'wrap' }}><button className="btn sm" onClick={onMaintain}>{t('维护地址')}</button><button className="btn sm" onClick={() => setPolicy(true)}>{t('地址策略')}</button>
        <button className="btn sm" type="button" onClick={() => setScheduling(true)}>{t('定时检测')}</button>
        <button className="btn sm" disabled={busy || !p.ipSourceUrl} onClick={() => run(async () => { await post(`/cdn-profiles/${p.id}/refresh-ips`); changed() }, t('已刷新'))}>{t('刷新')}</button></div>
    </div>
    <p className="sub">{t('固定地址不会被自动更新覆盖；来源、端口、运营商、前 N 个和禁用规则共同决定订阅选用。')}</p>
    {data.error && <div className="note bad" role="alert">{data.error}<button className="link" onClick={data.reload}>{t('重试')}</button></div>}
    <CDNCheckSummary schedule={data.data?.schedule} />
    <div className="form-grid">
      <Field label={t('用于检查和预览的入口')} htmlFor={p.id + "-preview-entry"}><select id={p.id + "-preview-entry"} className="input" disabled={busy || !contexts.length} value={endpoint || context?.endpointId || ''} onChange={(e) => { setEndpoint(e.target.value); setResult(undefined) }}>
        {!contexts.length && <option value="">{t('请先关联一个启用的 CDN 入口')}</option>}{contexts.map((c) => <option key={c.endpointId} value={c.endpointId}>{c.label} · {c.target.scheme?.toUpperCase()}:{c.target.port}</option>)}
      </select></Field>
      <Field label={t('预览地区')}><CDNRegionSelect label={t('预览地区')} emptyLabel={t('全部地区')} value={region} disabled={busy} onChange={setRegion} /></Field>
      <Field label={t('预览运营商')} htmlFor={p.id + "-preview-carrier"}><select id={p.id + "-preview-carrier"} className="input" disabled={busy} value={carrier} onChange={(e) => { setCarrier(e.target.value); setResult(undefined) }}>
        <option value="">{t('合并列表')}</option><option value="ct">{t('电信')}</option><option value="cu">{t('联通')}</option><option value="cm">{t('移动')}</option>
      </select></Field>
    </div>
    <div className="hint" style={{ margin: '8px 0' }}>{t('固定地址追加到前 N 个自动地址之后，不占用 N 的名额。')} {t('测速过期：{h} 小时', { h: p.staleHours || 24 })} · {(p.stalePolicy || 'source') === 'keep' ? t('保留旧列表，等待测速恢复') : t('尝试来源 URL，失败保留旧列表')}</div>
    {context && p.expandPerIp && !context.explicitAddress && data.data?.matchMode && <p className="hint">{data.data.matchMode === 'region_carrier' ? t('已匹配同地区、同运营商') : data.data.matchMode === 'region' ? t('已匹配同地区') : data.data.matchMode === 'carrier' ? region && p.regionSplit ? t('本地区无匹配结果，回退同运营商其他地区') : t('使用同运营商结果') : t('无匹配分组，使用合并或保留列表')}</p>}
    {carrier && !p.carrierSplit && <p className="hint">{t('运营商分流未开启，当前预览使用合并列表。')}</p>}
    {!p.expandPerIp && <div className="note small">{t('当前订阅只用回源域名；在「编辑」中开启订阅展开后，自动和固定优选地址才会用于订阅。')}</div>}
    {context && <div className="note small mono" style={{ overflowWrap: 'anywhere' }}>SNI: {context.target.sni} · Host: {context.target.host} · {context.target.path}<br />{t('选用地址：{list}', { list: (data.data?.selected ?? []).join(' · ') || '—' })}</div>}
    {context && !data.loading && !(data.data?.selected ?? []).length && rows.some((r) => r.reason === 'check_failed') && <div className="note warn small">{t('当前入口的候选已被排除，订阅不输出此 CDN 入口；可重新检测失败地址。')}</div>}
    {(context?.warnings ?? []).map((w, i) => <div className="note warn small" key={i}>{w}</div>)}
    <div className="scroll"><table className="rtable">
      <thead><tr><th>{t('地址')}</th><th>{t('来源 / 状态')}</th><th>{t('测量与检测')}</th><th>{t('订阅选用')}</th><th>{t('操作')}</th></tr></thead>
      <tbody>{rows.map((r) => <tr key={r.address} data-address={r.address}>
        <td className="mono rt-head" style={{ overflowWrap: 'anywhere' }}>{r.address}<div className="mute small">{r.type === 'domain' ? t('域名') : r.type.toUpperCase()}{(p.thirdParty ?? []).includes(r.address) && <span className="tag warn">{t('反代')}</span>}</div></td>
        <td data-label={t('来源 / 状态')}><div><span className={'tag ' + (r.blocked ? 'bad' : r.fixed ? 'info-tag' : '')}>{r.blocked ? t('已禁用') : r.fixed ? t('固定保留') : r.source === 'measurement' ? t('测速上报') : t('来源或保留列表')}</span><div className="small mute">{r.sources.join(' · ')}</div></div></td>
        <td className="small" data-label={t('测量与检测')}><div>{r.measurement && <div>{r.measurement.latencyMs} ms · {Math.round(r.measurement.loss * 100)}% · {r.measurement.speed || 0} MB/s</div>}{r.reportedAt && <div className={r.stale ? 'warn' : 'mute'}>{ago(r.reportedAt)}{r.stale && ' · ' + t('已过期')}</div>}
          <div>{r.check ? <button className="link" onClick={() => setResult(r.check)}>{r.check.ok ? t('传输检测通过') : r.check.partial ? t('检测部分完成') : t('检测未通过')} · {ago(r.check.checkedAt)}{Date.now() - Date.parse(r.check.checkedAt) > 1800000 && ' · ' + t('检测结果较旧')}</button> : r.failedAt ? <span>{t('检测未通过')} · {ago(r.failedAt)}</span> : t('未检测')}</div></div></td>
        <td data-label={t('订阅选用')}><span className={'tag ' + (r.selected ? 'ok' : r.reason === 'check_failed' ? 'bad' : '')}>{reasonLabel(r.reason)}</span></td>
        <td className="rt-actions"><div className="row" style={{ flexWrap: 'wrap' }}><button className="btn sm" disabled={busy || data.loading || !context} onClick={() => run(async () => { setResult(undefined); const res = await post<Result>(`/cdn-profiles/${p.id}/check-address`, { endpointId: context!.endpointId, address: r.address }); setResult(res); data.reload() })}>{t('检测')}</button>
          {r.failedAt && <button className="btn sm" disabled={busy || data.loading || !context} onClick={() => run(async () => { await del(`/cdn-profiles/${p.id}/check-address`, { endpointId: context!.endpointId, address: r.address }); changed() }, t('已重置为未检测'))}>{t('重置检测')}</button>}
          <button className="btn sm" disabled={busy} onClick={() => run(async () => { const blocked = r.blocked ? (p.blockedHosts ?? []).filter((a) => a !== (r.blockKey || r.address)) : [...(p.blockedHosts ?? []), r.address]; await put(`/cdn-profiles/${p.id}/address-policy`, { blockedHosts: blocked, stalePolicy: p.stalePolicy || 'source' }); changed() }, t('已保存'))}>{r.blocked ? t('恢复') : t('禁用')}</button></div></td>
      </tr>)}</tbody>
    </table></div>
    {!rows.length && <Empty>{t('还没有优选地址')}</Empty>}
    {p.nodeCheck && <details style={{ marginTop: 8 }}><summary className="sub">{t('各端口检测结果')}</summary>
      {Object.entries(p.portChecks ?? {}).map(([port, c]) => <div className="hint" key={port}>{p.scheme?.toUpperCase() || 'HTTPS'}:{port} · {c.allFailed ? t('全部地址不可达，暂保留列表') : t('{n} 个地址不可达', { n: c.badIps.length })} · {ago(c.at)}</div>)}
      {!Object.keys(p.portChecks ?? {}).length && <div className="hint">{t('节点检测：等待第一次检测')}</div>}
    </details>}
    <p className="hint">{t('检测来自面板网络，只验证 DNS、TCP、TLS 和传输握手，不代表客户端网络或代理认证通过。明确失败的地址不进入当前入口的订阅，重新检测通过后恢复；未检测或部分完成不作为失败。')}</p>
    <p className="hint">{t('手动重置检测会清除当前入口的结论，将地址恢复为未检测并重新参与选择。')}</p>
    {result && <div className={'note ' + (result.ok ? '' : result.partial ? 'warn' : 'bad')} role="status" style={{ display: 'block', overflowWrap: 'anywhere' }}>
      <b>{result.ok ? t('传输检测通过') : result.partial ? t('检测部分完成') : t('检测未通过')}</b> · {datetime(result.checkedAt)}
      <div className="mono small">{result.target.scheme?.toUpperCase()}://{result.target.server}:{result.target.port} · SNI {result.target.sni} · Host {result.target.host} · {result.target.path}</div>
      <p>{result.message}</p>
      <ul>{(result.stages ?? []).map((s) => <li key={s.kind}><b>{stageLabel(s.kind)}</b> · {stateLabel(s.state)} <span className="mono small">{s.detail} {s.addresses?.join(' · ')}</span></li>)}</ul>
      {(result.warnings ?? []).map((w, i) => <div key={i}>{w}</div>)}
    </div>}
    {policy && <Policy p={p} onClose={() => setPolicy(false)} onSaved={() => { setPolicy(false); changed() }} />}
    {scheduling && <CDNCheckSchedule profileId={p.id} schedule={data.data?.schedule} onClose={() => setScheduling(false)} onSaved={() => { setScheduling(false); data.reload(); onChanged() }} />}
  </section>
}
