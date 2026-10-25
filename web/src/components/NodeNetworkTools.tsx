import { useEffect, useRef, useState } from 'react'
import { Link, useLocation } from 'react-router-dom'
import { get, post, put } from '../api'
import { geoLocationText, geoStateText, type GeoLocation } from '../geoip'
import { datetime } from '../format'
import { t } from '../i18n'
import type { Node } from '../types'
import ProbePresetPicker from './ProbePresetPicker'
import { Empty, Field, Modal, Switch, useAction, useLoad, usePoll } from './ui'

export type ProbeTarget = { name: string; carrier: string; addr: string }
type ProbePolicy = { mode: 'inherit' | 'custom' | 'off'; targets: ProbeTarget[]; effectiveTargets: ProbeTarget[] }
type Targets = { ipv4: string; ipv6: string }
type Schedule = { policy: { enabled: boolean; targets: Partial<Targets>; intervalMinutes: number; revision: number }; runtime?: { nextAt: string; lastStartedAt?: string; lastState: string }; agentSupported: boolean; online: boolean; pausedByGlobalProbeSwitch: boolean }
type TraceMetadata = { state: string; reason?: string; databaseSha256?: string; databaseType?: string; buildEpoch?: number; limited?: boolean }
type TraceResult = { state: string; raw: string; asn?: TraceMetadata; geo?: TraceMetadata; hops: { ttl: number; addresses: string[]; asn?: { ip: string; state: string; asn?: number; organization?: string }[]; geo?: GeoLocation[] }[] }
type Trace = { id: number; family: string; target: string; source: string; state: string; startedAt: string; result: TraceResult; comparison: { state: string; reason?: string; previousId?: number; changedTtls: number[] } }
const stateText = (v: string) => ({ reached: t('已到达'), partial: t('部分结果'), timeout: t('超时'), unavailable: t('不可用'), failed: t('失败'), running: t('运行中'), completed: t('已完成'), interrupted: t('已中断'), baseline: t('首次基线'), same: t('路径相同'), changed: t('路径变化'), not_comparable: t('不可比较'), scheduled: t('定时'), manual: t('手动'), disabled: t('已关闭'), ready: t('就绪') }[v] ?? v)

function ProbePolicyEditor({ nodeId }: { nodeId: string }) {
  const info = useLoad(() => get<ProbePolicy>(`/nodes/${nodeId}/probe-policy`), [nodeId])
  const [mode, setMode] = useState<ProbePolicy['mode']>('inherit')
  const [targets, setTargets] = useState<ProbeTarget[]>([])
  const [candidate, setCandidate] = useState<ProbeTarget>({ name: '', carrier: '', addr: '' })
  const [preset, setPreset] = useState(false)
  const { busy, run } = useAction()
  useEffect(() => { if (info.data) { setMode(info.data.mode); setTargets((info.data.targets ?? []).map(({ name, carrier, addr }) => ({ name, carrier, addr }))) } }, [info.data])
  return <div className="ops-stack">
    {info.error && <div className="note warn" role="alert">{info.error}<button className="link" onClick={info.reload}>{t('重试')}</button></div>}
    <Field label={t('节点测试点策略')} hint={t('全局探测开关优先；关闭此处不会停止代理服务。测试为 TCP 连接，不是 ICMP 丢包。')}>
      <select className="input" aria-label={t('节点测试点策略')} value={mode} disabled={!info.data || busy} onChange={(e) => setMode(e.target.value as ProbePolicy['mode'])}>
        <option value="inherit">{t('继承全局')}</option><option value="custom">{t('独立测试点')}</option><option value="off">{t('关闭探测')}</option>
      </select>
    </Field>
    {mode === 'custom' && <>
      {targets.map((target, i) => <div className="ops-target" key={i}>
        <input className="input" aria-label={t('名称')} value={target.name} onChange={(e) => setTargets(targets.map((x, j) => i === j ? { ...x, name: e.target.value } : x))} />
        <select className="input" aria-label={t('运营商')} value={target.carrier} onChange={(e) => setTargets(targets.map((x, j) => i === j ? { ...x, carrier: e.target.value } : x))}><option value="">{t('自定义')}</option><option value="ct">{t('电信')}</option><option value="cu">{t('联通')}</option><option value="cm">{t('移动')}</option></select>
        <input className="input mono" aria-label={t('测试地址')} value={target.addr} onChange={(e) => setTargets(targets.map((x, j) => i === j ? { ...x, addr: e.target.value } : x))} />
        <button className="btn sm" onClick={() => setTargets(targets.filter((_, j) => i !== j))}>{t('删除')}</button>
      </div>)}
      <div className="row"><button className="btn sm" disabled={targets.length >= 20} onClick={() => setTargets([...targets, { name: '', carrier: '', addr: '' }])}>{t('添加自定义测试点')}</button><button className="btn sm" disabled={targets.length >= 20} onClick={() => setPreset(!preset)}>{t('从省市运营商选择')}</button></div>
      {preset && <div className="ops-stack"><ProbePresetPicker onChange={(p) => setCandidate(p ? { name: [p.province, p.city, ({ ct: t('电信'), cu: t('联通'), cm: t('移动') }[p.carrier])].filter(Boolean).join(' · '), carrier: p.carrier, addr: p.addr } : { name: '', carrier: '', addr: '' })} /><button className="btn" disabled={!candidate.addr || targets.length >= 20 || targets.some((x) => x.addr.toLowerCase() === candidate.addr.toLowerCase())} onClick={() => { setTargets([...targets, candidate]); setPreset(false) }}>{t('加入测试点')}</button></div>}
    </>}
    <button className="btn pri" disabled={busy || !info.data || (mode === 'custom' && (targets.length < 1 || targets.some((x) => !x.name.trim() || !x.addr.trim())))} onClick={() => run(async () => { await put(`/nodes/${nodeId}/probe-policy`, { mode, ...(mode === 'custom' ? { targets } : {}) }); info.reload() }, t('已保存'))}>{t('保存测试点策略')}</button>
    <div className="hint">{t('当前生效测试点')}</div>
    {(info.data?.effectiveTargets ?? []).map((x) => <div className="small" key={x.addr}>{x.name} · <span className="mono">{x.addr}</span></div>)}
    {info.data && !info.data.effectiveTargets?.length && <div className="mute small">{t('没有生效的测试点')}</div>}
  </div>
}

function TraceDetail({ trace, nodeId, onClose }: { trace: Trace; nodeId: string; onClose: () => void }) {
  const r = trace.result
  const previous = useLoad(() => trace.comparison.previousId ? get<Trace>(`/nodes/${nodeId}/route-traces/${trace.comparison.previousId}`) : Promise.resolve(undefined), [nodeId, trace.id, trace.comparison.previousId])
  const hops = [...new Set([...(r.hops ?? []).map((h) => h.ttl), ...(previous.data?.result.hops ?? []).map((h) => h.ttl)])].sort((a, b) => a - b)
  return <Modal title={t('回程检测详情')} onClose={onClose} wide>
    <div className="ops-stack">
      <div>{trace.family.toUpperCase()} · <span className="mono">{trace.target}</span> · {stateText(r.state || trace.state)} · {datetime(trace.startedAt)}</div>
      <div className="note">{stateText(trace.comparison.state)}{trace.comparison.changedTtls?.length ? ' · TTL ' + trace.comparison.changedTtls.join(', ') : ''}{trace.comparison.reason && <div className="small mono">{trace.comparison.reason}</div>}</div>
      <div className="hint">{t('路径不完整或采样条件不符时不会比较。ASN 是数据库归属注释，不代表 BGP 路径或商业线路等级。')}</div>
      {r.asn && <div className="small">ASN · {stateText(r.asn.state)} {r.asn.reason}<div className="mono ops-break">{r.asn.databaseSha256}</div>{r.asn.databaseType}{r.asn.buildEpoch ? ' · ' + datetime(new Date(r.asn.buildEpoch * 1000).toISOString()) : ''}{r.asn.limited && <div>{t('部分地址未注释')}</div>}</div>}
      {r.geo && <div className="small">{t('地理归属')} · {stateText(r.geo.state)} {r.geo.reason}<div className="mono ops-break">{r.geo.databaseSha256}</div>{r.geo.databaseType}{r.geo.buildEpoch ? ' · ' + datetime(new Date(r.geo.buildEpoch * 1000).toISOString()) : ''}{r.geo.limited && <div>{t('部分地址未注释')}</div>}</div>}
      <div className="hint">{t('地理归属是数据库估计；国家、省份或城市缺失时不推测补全。')}</div>
      {previous.error && <div className="note warn" role="alert">{t('上次对照记录不可用')} · {previous.error}</div>}
      <div className="scroll"><table><thead><tr><th>TTL</th>{trace.comparison.previousId && <th>{t('上次路径')}</th>}<th>{t('当前路径')}</th><th>ASN</th><th>{t('国家 / 省份 / 城市')}</th></tr></thead><tbody>{hops.map((ttl) => {
        const current = r.hops?.find((h) => h.ttl === ttl), before = previous.data?.result.hops?.find((h) => h.ttl === ttl)
        return <tr key={ttl}><td style={trace.comparison.changedTtls?.includes(ttl) ? { color: 'var(--warn)', fontWeight: 700 } : undefined}>{ttl}</td>{trace.comparison.previousId && <td className="mono">{previous.loading ? t('加载中…') : before?.addresses?.join(', ') || '*'}</td>}<td className="mono">{current?.addresses?.join(', ') || '*'}</td><td>{current?.asn?.map((a) => <div key={a.ip}>{a.asn ? `AS${a.asn} · ${a.organization ?? ''}` : a.state}</div>) ?? '—'}</td><td>{current?.geo?.length ? current.geo.map((g) => <div key={g.ip} style={{ minWidth: 140, maxWidth: 240, overflowWrap: 'anywhere' }}><span className="mono small">{g.ip}</span><div>{g.state === 'matched' ? geoLocationText(g) : geoStateText(g.state)}</div></div>) : '—'}</td></tr>
      })}</tbody></table></div>
      <details><summary>{t('原始检测输出')}</summary><pre className="code ops-raw">{r.raw || t('没有原始输出')}</pre></details>
    </div>
  </Modal>
}

function RouteTools({ nodeId }: { nodeId: string }) {
  const schedule = useLoad(() => get<Schedule>(`/nodes/${nodeId}/route-schedule`), [nodeId])
  const [targets, setTargets] = useState<Targets>({ ipv4: '', ipv6: '' })
  const [enabled, setEnabled] = useState(false)
  const [interval, setInterval] = useState(60)
  const [family, setFamily] = useState('')
  const [targetFilter, setTargetFilter] = useState('')
  const [filterDraft, setFilterDraft] = useState('')
  const [before, setBefore] = useState<number>()
  const history = useLoad(() => { const q = new URLSearchParams({ limit: '20' }); if (family) q.set('family', family); if (targetFilter.trim()) q.set('target', targetFilter.trim()); if (before) q.set('before', String(before)); return get<{ items: Trace[]; nextBefore?: number }>(`/nodes/${nodeId}/route-traces?${q}`) }, [nodeId, family, targetFilter, before])
  usePoll(history.reload, 15000)
  const [detail, setDetail] = useState<Trace>()
  const { busy, run } = useAction()
  useEffect(() => { if (schedule.data) { setTargets({ ipv4: schedule.data.policy.targets.ipv4 ?? '', ipv6: schedule.data.policy.targets.ipv6 ?? '' }); setEnabled(schedule.data.policy.enabled); setInterval(schedule.data.policy.intervalMinutes) } }, [schedule.data])
  const capable = !!schedule.data?.agentSupported
  const refresh = () => { schedule.reload(); history.reload() }
  return <div className="ops-stack">
    <div className="note">{t('从节点向公网 IP 执行 UDP traceroute，不等同于真实代理业务。需要 Linux Agent 和系统 traceroute；不会自动安装。')}</div>
    {schedule.error && <div className="note warn" role="alert">{schedule.error}<button className="link" onClick={schedule.reload}>{t('重试')}</button></div>}
    {schedule.data && !capable && <div className="note warn">{t('节点 Agent 不支持回程检测，请先升级')}</div>}
    <div className="form-grid"><Field label={t('IPv4 检测目标')}><input className="input mono" aria-label={t('IPv4 检测目标')} value={targets.ipv4} placeholder="1.1.1.1" onChange={(e) => setTargets({ ...targets, ipv4: e.target.value.trim() })} /></Field><Field label={t('IPv6 检测目标')}><input className="input mono" aria-label={t('IPv6 检测目标')} value={targets.ipv6} placeholder="2606:4700:4700::1111" onChange={(e) => setTargets({ ...targets, ipv6: e.target.value.trim() })} /></Field></div>
    <div className="row"><button className="btn" disabled={busy || !capable || !schedule.data?.online || (!targets.ipv4 && !targets.ipv6)} onClick={() => run(async () => { try { await post(`/nodes/${nodeId}/route-trace`, targets) } finally { setBefore(undefined); history.reload() } })}>{busy ? t('检测中…') : t('立即检测回程')}</button><button className="btn" disabled={busy} onClick={refresh}>{t('刷新')}</button></div>
    <div className="toggle"><span>{t('定时回程检测')}</span><Switch label={t('定时回程检测')} on={enabled} onChange={setEnabled} /></div>
    <Field label={t('检测间隔（分钟）')} hint={t('范围 5–1440 分钟；保存后等待一个间隔，不补跑遗漏任务。')}><input className="input mono" aria-label={t('检测间隔（分钟）')} type="number" min={5} max={1440} value={interval} onChange={(e) => setInterval(Number(e.target.value))} /></Field>
    {schedule.data?.pausedByGlobalProbeSwitch && <div className="note warn">{t('全局探测已关闭，定时回程暂停；手动检测仍可使用。')}</div>}
    <button className="btn pri" disabled={busy || !schedule.data || (enabled && (!capable || (!targets.ipv4 && !targets.ipv6))) || interval < 5 || interval > 1440} onClick={() => run(async () => { try { await put(`/nodes/${nodeId}/route-schedule`, { enabled, targets, intervalMinutes: interval, expectedRevision: schedule.data!.policy.revision }); schedule.reload() } catch (e) { if ((e as { status?: number }).status === 409) schedule.reload(); throw e } }, t('已保存'))}>{t('保存定时策略')}</button>
    {schedule.data?.runtime && <div className="small mute">{t('下次检测')}：{datetime(schedule.data.runtime.nextAt)} · {t('上次任务')}：{stateText(schedule.data.runtime.lastState || '—')}</div>}
    <div className="hint">{t('开启 ASN 或地理归属后，新的回程历史详情会显示注释；旧记录不会补算。')} <Link to="/settings#network" className="link">{t('配置归属数据库')}</Link></div>
    <h3>{t('回程历史')}</h3><div className="row"><select className="input" style={{ width: 'auto' }} aria-label={t('地址族筛选')} value={family} onChange={(e) => { setFamily(e.target.value); setBefore(undefined) }}><option value="">{t('全部')}</option><option value="ipv4">IPv4</option><option value="ipv6">IPv6</option></select><input className="input mono" aria-label={t('目标 IP 筛选')} placeholder={t('目标 IP 筛选')} value={filterDraft} onChange={(e) => setFilterDraft(e.target.value)} /><button className="btn" onClick={() => { setTargetFilter(filterDraft.trim()); setBefore(undefined) }}>{t('筛选')}</button></div>
    {history.error && <div className="note warn" role="alert">{history.error}</div>}
    <div className="scroll"><table><thead><tr><th>{t('时间')}</th><th>{t('目标')}</th><th>{t('来源')}</th><th>{t('状态')}</th><th>{t('路径对比')}</th><th /></tr></thead><tbody>{(history.data?.items ?? []).map((row) => <tr key={row.id}><td>{datetime(row.startedAt)}</td><td className="mono">{row.target}</td><td>{stateText(row.source)}</td><td>{stateText(row.result?.state || row.state)}</td><td>{stateText(row.comparison.state)}</td><td><button className="link" disabled={busy} onClick={() => run(async () => setDetail(await get<Trace>(`/nodes/${nodeId}/route-traces/${row.id}`)))}>{t('详情')}</button></td></tr>)}</tbody></table></div>
    {!history.loading && !history.error && !history.data?.items.length && <Empty>{t('暂无回程记录')}</Empty>}
    <div className="row"><button className="btn sm" disabled={!before || history.loading} onClick={() => setBefore(undefined)}>{t('最新记录')}</button><button className="btn sm" disabled={!history.data?.nextBefore || history.loading} onClick={() => setBefore(history.data?.nextBefore)}>{t('更早记录')}</button></div>
    {detail && <TraceDetail nodeId={nodeId} trace={detail} onClose={() => setDetail(undefined)} />}
  </div>
}

export default function NodeNetworkTools({ node }: { node: Node }) {
  const { search } = useLocation()
  const requested = new URLSearchParams(search).get('network') === 'route'
  const section = useRef<HTMLElement>(null)
  const [tab, setTab] = useState<'probe' | 'route' | ''>(() => requested ? 'route' : '')
  useEffect(() => { if (requested) { setTab('route'); section.current?.scrollIntoView({ block: 'start', inline: 'nearest' }) } }, [requested])
  return <section ref={section} className="card ops-stack" style={{ scrollMarginTop: 76 }}><div className="ctitle"><h2>{t('节点网络检测')}</h2><div className="row"><button className={'btn sm' + (tab === 'probe' ? ' pri' : '')} onClick={() => setTab(tab === 'probe' ? '' : 'probe')}>{t('测试点策略')}</button><button className={'btn sm' + (tab === 'route' ? ' pri' : '')} onClick={() => setTab(tab === 'route' ? '' : 'route')}>{t('回程与定时检测')}</button></div></div>{tab === 'probe' && <ProbePolicyEditor key={node.id} nodeId={node.id} />}{tab === 'route' && <RouteTools key={node.id} nodeId={node.id} />}</section>
}
