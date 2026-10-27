import { memo, useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { useProbeFeed } from '../useProbeFeed'
import { Lines } from '../components/Charts'
import ProbePresetPicker from '../components/ProbePresetPicker'
import { probePresets, type ProbePreset } from '../probeTargets'
import { CopyBox, Drawer, Empty, Field, Modal, RefreshPicker, Switch, useAction, useConfirm, useLoad, useRefresh } from '../components/ui'
import { bps, datetime, flag, uptime } from '../format'
import { t } from '../i18n'
import { alignedTimes, filterProbeNodes, freshTelemetry, latencyStats, validView } from '../probeMonitor'
import type { ProbeNode, ProbeTarget, ProbeView, ProbeFilter, ProbeSort, LatencyPoint } from '../probeMonitor'
import { NodeBadges, NodeProbeCard, ProbeSummary, ProbeTable, ProbeBadge, useProbeClock, colorOf } from '../components/ProbeMonitor'

// Server probe: node state and carrier latency (TCP connect time to China
// Telecom / Unicom / Mobile endpoints, every 10 seconds).

type PNode = ProbeNode
type TargetView = ProbeTarget
type Target = TargetView & { addr: string; sortOrder: number; enabled: boolean }
type Overview = { nodes: PNode[]; targets: TargetView[]; at: string; probeEnabled: boolean; statusPage: boolean; statusUrl: string; showTraffic: boolean }
const carriers = [{ v: 'ct', l: t('电信') }, { v: 'cu', l: t('联通') }, { v: 'cm', l: t('移动') }, { v: '', l: t('其他') }]

const ranges = [{ h: 6, label: t('6 小时') }, { h: 24, label: t('24 小时') }, { h: 72, label: t('3 天') }, { h: 168, label: t('7 天') }]
const pad = (x: number) => String(x).padStart(2, '0')
type Pt = LatencyPoint

export function LatencyChart({ nodeId }: { nodeId: string }) {
  const [hours, setHours] = useState(24)
  const data = useProbeFeed<{ points: Pt[]; targets: Target[] }>(`/nodes/${nodeId}/latency?hours=${hours}`, 10000)
  const pts = data.data?.points ?? []
  const targets = (data.data?.targets ?? []).filter((tg) => pts.some((p) => p.target === tg.id))
  const times = alignedTimes(pts.map((p) => p.t))
  const idx = new Map(times.map((x, i) => [x, i]))
  const labels = times.map((x) => { const d = new Date(x); return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}` })
  const series = targets.map((tg) => {
    const vs: (number | null)[] = times.map(() => null)
    const loss: (number | null)[] = times.map(() => null)
    pts.filter((p) => p.target === tg.id).forEach((p) => { const i = idx.get(Date.parse(p.t)); if (i != null) { vs[i] = p.avg; loss[i] = p.loss } })
    return { tg, vs, loss }
  })
  const stat = (tg: Target) => latencyStats(pts.filter((p) => p.target === tg.id))
  return (
    <div className="tchart">
      <div className="ctitle">
        <span>{t('网络延迟')}</span>
        <span className="seg-mini">{ranges.map((r) => <button key={r.h} type="button" className={hours === r.h ? 'on' : ''} onClick={() => setHours(r.h)}>{r.label}</button>)}</span>
      </div>
      {times.length < 2 ? <div className="sub" style={{ marginTop: 10 }}>{t('数据还不够：节点在线后每 10 秒测一次，每 5 分钟记录一个点')}</div> : <>
        <div className="small mute" style={{ marginTop: 10 }}>
          {series.map(({ tg }) => <span key={tg.id} className="legend" style={{ marginRight: 12 }}><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span>)}
        </div>
        <Lines labels={labels} fmt={(v) => `${v.toFixed(0)} ms`} height={170} series={series.map(({ tg, vs }) => ({ name: tg.name, color: colorOf(tg.carrier), values: vs }))} />
        <div className="small mute" style={{ marginTop: 6 }}>{t('探测失败率')}</div>
        <Lines labels={labels} max={100} fmt={(v) => `${v.toFixed(0)}%`} height={90} series={series.map(({ tg, loss }) => ({ name: tg.name, color: colorOf(tg.carrier), values: loss }))} />
        <div className="scroll" style={{ marginTop: 8 }}>
          <table>
            <thead><tr><th>{t('目标')}</th><th>{t('平均')}</th><th>{t('最低')}</th><th>{t('最高')}</th><th>{t('探测失败率')}</th></tr></thead>
            <tbody>{targets.map((tg) => { const s = stat(tg); return (
              <tr key={tg.id}><td><span className="legend"><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span></td>
                <td className="mono">{s.avg != null ? s.avg.toFixed(1) + ' ms' : '—'}</td><td className="mono">{s.min != null ? s.min.toFixed(0) : '—'}</td>
                <td className="mono">{s.max != null ? s.max.toFixed(0) : '—'}</td><td className="mono">{s.loss == null ? '—' : s.loss.toFixed(1) + '%'}</td></tr>) })}</tbody>
          </table>
        </div>
      </>}
    </div>
  )
}

const ResourceHistory = memo(function ResourceHistory({ nodeId }: { nodeId: string }) {
  const [hours, setHours] = useState(24)
  const data = useProbeFeed<{ points: { t: string; cpu: number; mem: number; rx: number; tx: number }[] }>(`/nodes/${nodeId}/metrics?hours=${hours}`, 5000)
  const points = data.data?.points ?? []
  const times = alignedTimes(points.map((p) => p.t))
  const byTime = new Map(points.map((p) => [Date.parse(p.t), p]))
  const labels = times.map((at) => { const d = new Date(at); return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}` })
  return <section>
    <div className="ctitle"><h3>{t('资源历史')}</h3><span className="seg-mini">{ranges.map((r) => <button key={r.h} className={hours === r.h ? 'on' : ''} onClick={() => setHours(r.h)}>{r.label}</button>)}</span></div>
    {data.error && <div className="note warn">{data.error}</div>}
    {points.length < 2 ? <div className="pv-empty">{data.loading ? t('加载中…') : t('暂无资源历史数据')}</div> : <div className="pv-history-grid">
      <section><h3>CPU / RAM</h3><Lines labels={labels} max={100} fmt={(v) => v.toFixed(1) + '%'} height={160} series={[{ name: 'CPU', color: 'var(--pv-cyan)', values: times.map((at) => byTime.get(at)?.cpu ?? null) }, { name: 'RAM', color: 'var(--pv-purple)', values: times.map((at) => byTime.get(at)?.mem ?? null) }]} /></section>
      <section><h3>{t('接收 / 发送')}</h3><Lines labels={labels} fmt={bps} height={160} series={[{ name: t('接收速率'), color: 'var(--pv-green)', values: times.map((at) => byTime.get(at)?.rx ?? null) }, { name: t('发送速率'), color: 'var(--pv-blue)', values: times.map((at) => byTime.get(at)?.tx ?? null) }]} /></section>
    </div>}
  </section>
})

function NodeDrawer({ n, now, onClose, onChanged }: { n: PNode; now: number; onClose: () => void; onChanged: () => void }) {
  const { busy, run } = useAction()
  const fresh = freshTelemetry(n, now)
  return <Drawer title={<>{flag(n.flag)} {n.name}</>} onClose={onClose} wide>
    <div className="pv">
      <div className="row" style={{ justifyContent: 'space-between', marginBottom: 14 }}><NodeBadges n={n} now={now} /><ProbeBadge n={n} now={now} /></div>
      <div className="pv-detail-summary"><div><small>{t('运行时间')}</small><b>{fresh ? uptime(n.uptime) : '—'}</b></div><div><small>{t('接收速率')}</small><b>{fresh ? bps(n.rxBps) : '—'}</b></div><div><small>{t('发送速率')}</small><b>{fresh ? bps(n.txBps) : '—'}</b></div></div>
      <div className="pv-subtitle">{n.address && <span className="mono">{n.address}</span>}{n.agentVersion && <span>Agent {n.agentVersion}</span>}{n.lastSeen && <span>{t('最近上报')} · {datetime(n.lastSeen)}</span>}</div>
      <ResourceHistory nodeId={n.id} />
      <LatencyChart nodeId={n.id} />
      <div className="pv-footnote">{t('失败率表示 TCP 连接探测失败的比例，不是 ICMP 丢包率。')}</div>
      <Field label={t('公开状态页')} hint={t('关闭后这个节点不出现在公开状态页上，后台仍然照常探测')}>
        <label className="row small"><Switch label={t('在公开状态页显示')} on={!n.hidden} disabled={busy} onChange={(v) => run(async () => { await put(`/probe/nodes/${n.id}`, { hidden: !v }); onChanged() })} />{t('在公开状态页显示')}</label>
      </Field>
    </div>
  </Drawer>
}

function TargetForm({ tg, nodes, onClose, onSaved }: { tg?: Target; nodes: PNode[]; onClose: () => void; onSaved: () => void }) {
  const initialPreset = probePresets.find((p) => p.addr === tg?.addr && p.carrier === tg?.carrier)
  const [source, setSource] = useState<'catalog' | 'custom'>(!tg || initialPreset ? 'catalog' : 'custom')
  const [preset, setPreset] = useState<ProbePreset | undefined>(initialPreset)
  const [f, setF] = useState({ name: tg?.name ?? '', carrier: tg?.carrier ?? 'ct', addr: tg?.addr ?? '', sortOrder: tg?.sortOrder ?? 0, enabled: tg?.enabled ?? true })
  const online = nodes.filter((n) => n.online)
  const [node, setNode] = useState(online[0]?.id ?? '')
  const [test, setTest] = useState('')
  const { busy, run } = useAction()
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const choose = (p?: ProbePreset) => {
    setPreset(p)
    setTest('')
    if (!p) { setF((x) => ({ ...x, name: '', addr: '' })); return }
    const name = p.province + p.city + carriers.find((c) => c.v === p.carrier)!.l + (p.family === 'ipv4' ? '' : ' · ' + (p.family === 'ipv6' ? 'IPv6' : t('双栈')))
    setF((x) => ({ ...x, name, carrier: p.carrier, addr: p.addr }))
  }
  const validAddress = !!f.addr && (source === 'custom' || !!preset)
  return (
    <Modal title={tg ? t('编辑探测目标') : t('添加探测目标')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.name || !validAddress} onClick={() => run(async () => {
        if (tg) await put(`/probe/targets/${tg.id}`, f); else await post('/probe/targets', f)
        onSaved()
      }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <Field label={t('目标来源')} htmlFor="pt-source"><select id="pt-source" className="input" value={source} onChange={(e) => {
        const next = e.target.value as 'catalog' | 'custom'
        setSource(next)
        setPreset(probePresets.find((p) => p.addr === f.addr && p.carrier === f.carrier))
        setTest('')
      }}><option value="catalog">{t('Zstatic 节点目录')}</option><option value="custom">{t('自定义')}</option></select></Field>
      {source === 'catalog' && <ProbePresetPicker initial={preset} onChange={choose} />}
      <Field label={t('名称')} htmlFor="pt-name"><input id="pt-name" className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder={t('广东电信')} /></Field>
      {source === 'custom' && <Field label={t('运营商')} hint={t('决定图表里的颜色')}>
        <div className="seg">{carriers.map((c) => <button key={c.v} type="button" className={'opt' + (f.carrier === c.v ? ' on' : '')} aria-pressed={f.carrier === c.v} onClick={() => set('carrier', c.v)}>{c.l}</button>)}</div>
      </Field>}
      <Field label={t('地址')} htmlFor="pt-addr" hint={source === 'catalog' ? t('选择节点后自动填写；需要修改地址时切换到自定义') : t('主机:端口，用 TCP 连接测延迟。省级三网地址格式：省份-ct/cu/cm-v4.ip.zstaticcdn.com:80，如 gd-ct-v4.ip.zstaticcdn.com:80')}>
        <input id="pt-addr" className="input mono" readOnly={source === 'catalog'} value={source === 'catalog' && !preset ? '' : f.addr} onChange={(e) => { set('addr', e.target.value); setTest('') }} placeholder="gd-ct-v4.ip.zstaticcdn.com:80" />
      </Field>
      <Field label={t('排序')} htmlFor="pt-sort"><input id="pt-sort" className="input mono" type="number" value={f.sortOrder} onChange={(e) => set('sortOrder', Number(e.target.value) || 0)} /></Field>
      <Field label={t('启用')}><label className="row small"><Switch label={t('启用')} on={f.enabled} onChange={(v) => set('enabled', v)} />{t('参与定时探测')}</label></Field>
      {online.length > 0 && <Field label={t('试测')} hint={test || undefined}>
        <div className="row">
          <select className="input" value={node} onChange={(e) => setNode(e.target.value)} aria-label={t('节点')}>{online.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select>
          <button className="btn sm" type="button" disabled={busy || !validAddress} onClick={() => run(async () => {
            const r = await post<{ ok: boolean; ms?: number; error?: string }>('/probe/targets/test', { nodeId: node, addr: f.addr })
            setTest(r.ok ? t('{ms} ms', { ms: r.ms ?? 0 }) : t('失败：{e}', { e: r.error ?? '' }))
          })}>{t('从该节点测一次')}</button>
        </div>
      </Field>}
    </Modal>
  )
}

function Targets({ nodes, onClose, onChanged }: { nodes: PNode[]; onClose: () => void; onChanged: () => void }) {
  const list = useLoad(() => get<Target[]>('/probe/targets'))
  const [edit, setEdit] = useState<Target | null | undefined>(undefined)
  const { run } = useAction()
  const confirm = useConfirm()
  const done = () => { list.reload(); onChanged() }
  return (
    <Modal title={t('探测目标')} onClose={onClose} wide footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('关闭')}</button>
      <button className="btn pri" type="button" onClick={() => setEdit(null)}>+ {t('添加目标')}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{t('每个在线节点每 10 秒对每个启用的目标做一次 TCP 连接，记录耗时和是否失败；按 5 分钟汇总，保留 7 天。')}</div>
      {(list.data ?? []).length === 0 ? <Empty>{list.loading ? t('加载中…') : t('还没有探测目标')}</Empty> : (
        <div className="scroll"><table>
          <thead><tr><th>{t('名称')}</th><th>{t('地址')}</th><th>{t('状态')}</th><th></th></tr></thead>
          <tbody>{(list.data ?? []).map((tg) => (
            <tr key={tg.id}>
              <td><span className="legend"><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span></td>
              <td className="mono small">{tg.addr}</td>
              <td>{tg.enabled ? <span className="st ok">{t('启用')}</span> : <span className="st off">{t('停用')}</span>}</td>
              <td className="row" style={{ justifyContent: 'flex-end' }}>
                <button className="btn sm" type="button" onClick={() => setEdit(tg)}>{t('编辑')}</button>
                <button className="btn sm danger" type="button" onClick={async () => {
                  if (await confirm(t('删除探测目标 {name}？它的历史数据也会删除。', { name: tg.name }), { danger: true })) run(async () => { await del(`/probe/targets/${tg.id}`); done() }, t('已删除'))
                }}>{t('删除')}</button>
              </td>
            </tr>))}</tbody>
        </table></div>
      )}
      {edit !== undefined && <TargetForm tg={edit ?? undefined} nodes={nodes} onClose={() => setEdit(undefined)} onSaved={() => { setEdit(undefined); done() }} />}
    </Modal>
  )
}

function StatusSettings({ url, onClose, onSaved }: { url: string; onClose: () => void; onSaved: () => void }) {
  const s = useLoad(() => get<Record<string, string>>('/settings'))
  const [f, setF] = useState<Record<string, string> | null>(null)
  const v = f ?? (s.data ? { probe_enabled: s.data.probe_enabled ?? 'true', status_page: s.data.status_page ?? 'false', status_title: s.data.status_title ?? '', status_note: s.data.status_note ?? '', status_show_traffic: s.data.status_show_traffic ?? 'false', status_show_cost: s.data.status_show_cost ?? 'false' } : null)
  const set = (k: string, x: string) => setF({ ...(v ?? {}), [k]: x })
  const { busy, run } = useAction()
  return (
    <Modal title={t('探针设置')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !v} onClick={() => run(async () => { await put('/settings', v); onSaved() }, t('已保存'))}>{t('保存')}</button>
    </>}>
      {!v ? <div className="mute">{t('加载中…')}</div> : <>
        <Field label={t('延迟探测')}><label className="row small"><Switch label={t('延迟探测')} on={v.probe_enabled === 'true'} onChange={(x) => set('probe_enabled', String(x))} />{t('每 10 秒从各节点测一次延迟')}</label></Field>
        <Field label={t('公开状态页')} hint={t('任何人打开都能看到节点在线状态、资源占用和三网延迟；不显示 IP、域名和 Agent 版本，可在节点详情里单独隐藏节点')}>
          <label className="row small"><Switch label={t('公开状态页')} on={v.status_page === 'true'} onChange={(x) => set('status_page', String(x))} />{t('开启')} <span className="mono">{url}</span></label>
        </Field>
        {v.status_page === 'true' && <CopyBox text={url} />}
        <Field label={t('页面标题')} htmlFor="st-title"><input id="st-title" className="input" value={v.status_title} onChange={(e) => set('status_title', e.target.value)} placeholder={t('留空则用「站点名称 · 服务器状态」')} /></Field>
        <Field label={t('公告')} htmlFor="st-note"><textarea id="st-note" className="input" rows={3} value={v.status_note} onChange={(e) => set('status_note', e.target.value)} placeholder={t('可选，显示在页面顶部')} /></Field>
        <Field label={t('显示服务器流量')}><label className="row small"><Switch label={t('显示服务器流量')} on={v.status_show_traffic === 'true'} onChange={(x) => set('status_show_traffic', String(x))} />{t('在公开页显示各节点本月流量和额度')}</label></Field>
        <Field label={t('显示价格和到期')}><label className="row small"><Switch label={t('显示价格和到期')} on={v.status_show_cost === 'true'} onChange={(x) => set('status_show_cost', String(x))} />{t('在公开页显示服务器价格和剩余天数（来自节点的成本设置）')}</label></Field>
      </>}
    </Modal>
  )
}

export default function Probe() {
  const [refresh, setRefresh] = useRefresh(2000, 'polaris.probe.refresh')
  const { nodes: liveNodes } = useNodes()
  const data = useProbeFeed<Overview>('/probe', refresh, liveNodes)
  const [open, setOpen] = useState<string | null>(null)
  const [targets, setTargets] = useState(false)
  const [settings, setSettings] = useState(false)
  const [q, setQ] = useState('')
  const [region, setRegion] = useState('')
  const [state, setState] = useState<ProbeFilter>('all')
  const [sort, setSort] = useState<ProbeSort>('default')
  const [view, setView] = useState<ProbeView>(() => { try { return validView(localStorage.getItem('probe-view')) } catch { return 'card' } })
  useEffect(() => { try { localStorage.setItem('probe-view', view) } catch { /* storage unavailable */ } }, [view])
  const d = data.data, now = useProbeClock(d?.at)
  const nodes = filterProbeNodes(d?.nodes ?? [], q, region, state, sort, now)
  const regions = [...(d?.nodes ?? []).reduce((m, n) => n.flag ? m.set(n.flag, (m.get(n.flag) ?? 0) + 1) : m, new Map<string, number>())]
  const sel = d?.nodes.find((n) => n.id === open)
  const clear = () => { setQ(''); setRegion(''); setState('all') }
  const statuses: [ProbeFilter, string][] = [['all', t('全部')], ['online', t('在线')], ['offline', t('离线')], ['attention', t('需关注')]]
  return <div className="pv pv-monitor">
    <div className="pv-dashboard-head"><div><div className="pv-eyebrow">POLARIS / SERVER MONITOR</div><h1>{t('服务器探针')}</h1><div className="pv-subtitle"><span>{t('资源状态与网络质量')}</span>{d && <span className={'pv-live' + (data.error ? ' stale' : '')}><i />{t('更新于 {time}', { time: new Date(d.at).toLocaleTimeString() })}</span>}</div></div><div className="pv-head-actions"><RefreshPicker ms={refresh} onChange={setRefresh} onNow={data.reload} /><button className="btn" onClick={() => setTargets(true)}>{t('探测目标')}</button><button className="btn" onClick={() => setSettings(true)}>{t('设置')}{d?.statusPage ? ' · ' + t('已公开') : ''}</button></div></div>
    {data.error && <div className="note warn" role="alert">{t('更新失败，已保留上次数据。')} {data.error} <button className="link" onClick={data.reload}>{t('重试')}</button></div>}
    {d && !d.probeEnabled && <div className="note warn">{t('延迟探测已关闭')}</div>}
    {!d ? <div className="pv-loading" role="status">{data.error ? t('暂无可显示的数据') : t('正在读取节点状态…')}</div> : <>
      <ProbeSummary nodes={d.nodes} now={now} showTraffic={d.showTraffic !== false} />
      <div className="pv-toolbar">
        <div className="pv-toolbar-top"><input className="pv-search" type="search" placeholder={t('搜索名称、地区或系统')} value={q} onChange={(e) => setQ(e.target.value)} aria-label={t('搜索节点')} /><div className="pv-toolbar-controls"><select value={sort} onChange={(e) => setSort(e.target.value as ProbeSort)} aria-label={t('节点排序')}><option value="default">{t('默认排序')}</option><option value="name">{t('按名称')}</option><option value="cpu">{t('CPU 从高到低')}</option><option value="network">{t('网速从高到低')}</option><option value="latency">{t('延迟从低到高')}</option><option value="expiry">{t('到期时间从近到远')}</option></select><div className="pv-seg" aria-label={t('展示方式')}>{([['card', t('条形')], ['ring', t('环形')], ['table', t('表格')]] as [ProbeView, string][]).map(([v, label]) => <button key={v} className={view === v ? 'on' : ''} aria-pressed={view === v} onClick={() => setView(v)}>{label}</button>)}</div></div></div>
        <div className="pv-toolbar-bottom"><div className="pv-chips"><button className={'pv-chip' + (!region ? ' on' : '')} aria-pressed={!region} onClick={() => setRegion('')}>{t('全部地区')} {d.nodes.length}</button>{regions.map(([f, count]) => <button key={f} className={'pv-chip' + (region === f ? ' on' : '')} aria-pressed={region === f} onClick={() => setRegion(region === f ? '' : f)}>{flag(f)} {f} <small>{count}</small></button>)}</div><div className="pv-seg" aria-label={t('节点状态筛选')}>{statuses.map(([v, label]) => <button key={v} className={state === v ? 'on' : ''} aria-pressed={state === v} onClick={() => setState(v)}>{label}</button>)}</div><span className="pv-count">{t('显示 {n} / {all} 个节点', { n: nodes.length, all: d.nodes.length })}</span></div>
      </div>
      {!nodes.length ? <div className="pv-empty">{d.nodes.length ? t('没有符合筛选的节点') : t('还没有节点')}<div><button className="btn sm" onClick={clear}>{t('清除筛选')}</button></div></div> : view === 'table' ? <ProbeTable nodes={nodes} targets={d.targets} onOpen={setOpen} now={now} showTraffic={d.showTraffic !== false} /> : <div className="pv-grid">{nodes.map((n) => <NodeProbeCard key={n.id} n={n} targets={d.targets} onOpen={() => setOpen(n.id)} view={view} now={now} showTraffic={d.showTraffic !== false} />)}</div>}
      <div className="pv-footnote">{t('延迟为节点到目标的 TCP 连接耗时；小图展示近 2 小时，每格 5 分钟。灰格表示无样本。')}</div>
    </>}
    {sel && <NodeDrawer n={sel} now={now} onClose={() => setOpen(null)} onChanged={data.reload} />}
    {targets && <Targets nodes={d?.nodes ?? []} onClose={() => setTargets(false)} onChanged={data.reload} />}
    {settings && <StatusSettings url={d?.statusUrl ?? ''} onClose={() => setSettings(false)} onSaved={() => { setSettings(false); data.reload() }} />}
  </div>
}
