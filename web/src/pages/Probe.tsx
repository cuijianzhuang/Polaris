import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { Lines } from '../components/Charts'
import ProbePresetPicker from '../components/ProbePresetPicker'
import { probePresets, type ProbePreset } from '../probeTargets'
import { CopyBox, Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, usePoll } from '../components/ui'
import { bps, bytes, datetime, flag, uptime } from '../format'
import { t } from '../i18n'

// Server probe: node state and carrier latency (TCP connect time to China
// Telecom / Unicom / Mobile endpoints, every 30 seconds).

type Now = { ms: number; avg: number | null; loss: number; at: string; spark: (number | null)[] }
type PNode = {
  id: string; name: string; region: string; flag: string; online: boolean; status: string
  cpu: number; mem: number; rxBps: number; txBps: number; uptime: number; load1: number
  netUsed: number; netLimit: number; lastSeen?: string; hidden?: boolean; address?: string; agentVersion?: string
  latency: Record<string, Now>
  disk: number; memTotal?: number; diskTotal?: number; cores?: number; os?: string; arch?: string; ipv4: boolean; ipv6: boolean
  price?: string; expireAt?: string
}
type TargetView = { id: string; name: string; carrier: string }
type Target = TargetView & { addr: string; sortOrder: number; enabled: boolean }
type Overview = { nodes: PNode[]; targets: TargetView[]; at: string; probeEnabled: boolean; statusPage: boolean; statusUrl: string }

export const carrierColor: Record<string, string> = { ct: '#2563EB', cu: '#DC2626', cm: '#16A34A' }
const colorOf = (c: string) => carrierColor[c] ?? '#7C3AED'
const carriers = [{ v: 'ct', l: t('电信') }, { v: 'cu', l: t('联通') }, { v: 'cm', l: t('移动') }, { v: '', l: t('其他') }]

// Colours shared with the public status page: green / yellow / red by load.
export const usageColor = (p: number) => p >= 85 ? 'var(--pv-red)' : p >= 60 ? 'var(--pv-yellow)' : 'var(--pv-green)'
export const pingColor = (ms: number | null | undefined) => ms == null || ms <= 0 ? 'var(--pv-red)' : ms <= 80 ? 'var(--pv-green)' : ms <= 160 ? 'var(--pv-cyan)' : ms <= 260 ? 'var(--pv-yellow)' : 'var(--pv-red)'

function Bar({ k, pct, val, title }: { k: string; pct: number; val: string; title?: string }) {
  return (
    <div className="pv-row" title={title}>
      <span className="pv-k">{k}</span>
      <div className="pv-track"><i style={{ width: Math.min(100, Math.max(0, pct)) + '%', background: usageColor(pct) }} /></div>
      <span className="pv-val">{val}</span>
    </div>
  )
}

// Buckets: one bar per 5-minute average over the last two hours.
function Buckets({ vs }: { vs: (number | null)[] }) {
  const ok = vs.filter((v): v is number => v != null)
  const max = Math.max(1, ...ok)
  return (
    <span className="pv-bk" aria-hidden>
      {vs.map((v, i) => <span key={i} style={{ height: v == null ? '18%' : Math.max(15, (v / max) * 100) + '%', background: pingColor(v), opacity: v == null ? 0.5 : 0.85 }} />)}
    </span>
  )
}

export function LatencyRows({ n, targets }: { n: PNode; targets: TargetView[] }) {
  if (!targets.length) return null
  return (
    <div className="pv-lat">
      {targets.map((tg) => {
        const l = n.latency[tg.id]
        const live = l && n.online
        return (
          <div key={tg.id} className="pv-lr" title={l?.avg != null ? t('近 1 小时平均 {ms} ms · 丢包 {loss}%', { ms: l.avg.toFixed(0), loss: l.loss.toFixed(1) }) : undefined}>
            <span className="pv-ln"><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span>
            <Buckets vs={l?.spark ?? []} />
            <b className="pv-lv" style={{ color: live ? pingColor(l.ms) : 'var(--pv-mute)' }}>{!live ? '—' : l.ms ? l.ms.toFixed(0) + 'ms' : t('超时')}</b>
            <span className="pv-ls" style={{ color: live && l.loss >= 5 ? 'var(--pv-red)' : live && l.loss >= 1 ? 'var(--pv-yellow)' : undefined }}>{live ? l.loss.toFixed(0) + '%' : ''}</span>
          </div>
        )
      })}
    </div>
  )
}

const daysLeft = (iso?: string) => iso ? Math.ceil((new Date(iso).getTime() - Date.now()) / 864e5) : null

export function NodeBadges({ n }: { n: PNode }) {
  const d = daysLeft(n.expireAt)
  const size = [n.cores ? n.cores + 'C' : '', n.memTotal ? bytes(n.memTotal).replace(' ', '') : '', n.diskTotal ? bytes(n.diskTotal).replace(' ', '') : ''].filter(Boolean).join(' / ')
  return (
    <div className="pv-meta">
      {n.ipv4 && <span className="pv-badge v4">IPv4</span>}
      {n.ipv6 && <span className="pv-badge v6">IPv6</span>}
      {n.os && <span className="pv-badge" title={n.os}>{n.os.length > 18 ? n.os.slice(0, 18) + '…' : n.os}</span>}
      {n.arch && <span className="pv-badge">{n.arch}</span>}
      {size && <span className="pv-badge">{size}</span>}
      {n.price && <span className="pv-badge cost">{n.price}</span>}
      {d != null && <span className={'pv-badge exp' + (d <= 7 ? ' late' : '')}>{d >= 0 ? t('剩 {n} 天', { n: d }) : t('已过期')}</span>}
    </div>
  )
}

function NodeProbeCard({ n, targets, onOpen }: { n: PNode; targets: TargetView[]; onOpen: () => void }) {
  return (
    <button type="button" className={'pv-card' + (n.online ? '' : ' off')} onClick={onOpen}>
      <div className="pv-hd">
        <span className="pv-flag">{flag(n.flag)}</span>
        <span className="pv-name">{n.name}</span>
        {n.hidden && <span className="pv-badge">{t('不公开')}</span>}
        <span className={'pv-st ' + (n.online ? 'on' : 'off')}>{n.online ? t('在线') : t('离线')}</span>
      </div>
      <NodeBadges n={n} />
      {n.online ? <div className="pv-rows">
        <Bar k="CPU" pct={n.cpu} val={n.cpu.toFixed(1) + '%'} />
        <Bar k="RAM" pct={n.mem} val={n.mem.toFixed(1) + '%'} title={n.memTotal ? bytes((n.memTotal * n.mem) / 100) + ' / ' + bytes(n.memTotal) : undefined} />
        <Bar k="DISK" pct={n.disk} val={n.disk ? n.disk.toFixed(1) + '%' : '—'} title={n.diskTotal ? bytes(n.diskTotal) : t('Agent 升级后显示')} />
        {n.netLimit > 0 && <Bar k="TRF" pct={(100 * n.netUsed) / n.netLimit} val={bytes(n.netUsed)} title={bytes(n.netUsed) + ' / ' + bytes(n.netLimit)} />}
        <div className="pv-row"><span className="pv-k">NET</span><span className="pv-pair"><span className="pv-dn">▼ {bps(n.rxBps)}</span><span className="pv-up">▲ {bps(n.txBps)}</span></span></div>
        <div className="pv-row"><span className="pv-k">UP</span><span className="pv-pair"><span>{uptime(n.uptime)}</span><span style={{ color: 'var(--pv-mute)' }}>load {n.load1.toFixed(2)}</span></span></div>
      </div> : <div className="pv-off">{t('离线')} · {n.lastSeen ? datetime(n.lastSeen) : '—'}</div>}
      <LatencyRows n={n} targets={targets} />
    </button>
  )
}

// ProbeSummary: the strip of totals above the cards.
export function ProbeSummary({ nodes }: { nodes: PNode[] }) {
  const on = nodes.filter((n) => n.online)
  const rx = on.reduce((a, n) => a + n.rxBps, 0), tx = on.reduce((a, n) => a + n.txBps, 0)
  const used = nodes.reduce((a, n) => a + (n.netUsed || 0), 0)
  const cpu = on.length ? on.reduce((a, n) => a + n.cpu, 0) / on.length : 0
  const regions = new Set(nodes.map((n) => n.flag).filter(Boolean)).size
  return (
    <div className="pv-sum">
      <div className="pv-tile"><div className="k">{t('在线')}</div><div className="v g">{on.length}<small> / {nodes.length}</small></div></div>
      <div className="pv-tile"><div className="k">{t('总下行')}</div><div className="v">{bps(rx)}</div></div>
      <div className="pv-tile"><div className="k">{t('总上行')}</div><div className="v b">{bps(tx)}</div></div>
      {used > 0 && <div className="pv-tile"><div className="k">{t('本月流量')}</div><div className="v y">{bytes(used)}</div></div>}
      <div className="pv-tile"><div className="k">{t('平均 CPU')}</div><div className="v c">{cpu.toFixed(1)}%</div></div>
      <div className="pv-tile"><div className="k">{t('地区')}</div><div className="v">{regions}</div></div>
    </div>
  )
}

export function ProbeTable({ nodes, targets, onOpen }: { nodes: PNode[]; targets: TargetView[]; onOpen?: (id: string) => void }) {
  const mini = (p: number, v: string) => <span className="pv-mini"><span className="pv-track"><i style={{ width: Math.min(100, p) + '%', background: usageColor(p) }} /></span>{v}</span>
  return (
    <div className="pv-scroll"><table className="pv-table">
      <thead><tr><th>{t('节点')}</th><th>{t('状态')}</th><th>CPU</th><th>RAM</th><th>DISK</th><th>NET ▼/▲</th><th>{t('运行')}</th>{targets.map((tg) => <th key={tg.id}>{tg.name}</th>)}</tr></thead>
      <tbody>{nodes.map((n) => (
        <tr key={n.id} className={onOpen ? 'click' : ''} onClick={onOpen ? () => onOpen(n.id) : undefined}>
          <td>{flag(n.flag)} <b>{n.name}</b></td>
          <td><span className={'pv-st ' + (n.online ? 'on' : 'off')}>{n.online ? t('在线') : t('离线')}</span></td>
          <td>{n.online ? mini(n.cpu, n.cpu.toFixed(0) + '%') : '—'}</td>
          <td>{n.online ? mini(n.mem, n.mem.toFixed(0) + '%') : '—'}</td>
          <td>{n.online && n.disk ? mini(n.disk, n.disk.toFixed(0) + '%') : '—'}</td>
          <td>{n.online ? <><span className="pv-dn">{bps(n.rxBps)}</span> / <span className="pv-up">{bps(n.txBps)}</span></> : '—'}</td>
          <td>{n.online ? uptime(n.uptime) : '—'}</td>
          {targets.map((tg) => { const l = n.latency[tg.id]; return <td key={tg.id} style={{ color: l && n.online ? pingColor(l.ms) : 'var(--pv-mute)', fontWeight: 600 }}>{l && n.online ? (l.ms ? l.ms.toFixed(0) + 'ms' : t('超时')) : '—'}</td> })}
        </tr>))}</tbody>
    </table></div>
  )
}

const ranges = [{ h: 6, label: t('6 小时') }, { h: 24, label: t('24 小时') }, { h: 72, label: t('3 天') }, { h: 168, label: t('7 天') }]
const pad = (x: number) => String(x).padStart(2, '0')
type Pt = { t: string; target: string; avg: number | null; min: number; max: number; loss: number }

export function LatencyChart({ nodeId }: { nodeId: string }) {
  const [hours, setHours] = useState(24)
  const data = useLoad(() => get<{ points: Pt[]; targets: Target[] }>(`/nodes/${nodeId}/latency?hours=${hours}`), [nodeId, hours])
  usePoll(data.reload, 60000)
  const pts = data.data?.points ?? []
  const targets = (data.data?.targets ?? []).filter((tg) => pts.some((p) => p.target === tg.id))
  const times = [...new Set(pts.map((p) => p.t))].sort()
  const idx = new Map(times.map((x, i) => [x, i]))
  const labels = times.map((x) => { const d = new Date(x); return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}` })
  const series = targets.map((tg) => {
    const vs: (number | null)[] = times.map(() => null)
    const loss: (number | null)[] = times.map(() => null)
    pts.filter((p) => p.target === tg.id).forEach((p) => { const i = idx.get(p.t)!; vs[i] = p.avg; loss[i] = p.loss })
    return { tg, vs, loss }
  })
  const stat = (tg: Target) => {
    const ps = pts.filter((p) => p.target === tg.id)
    const ok = ps.filter((p) => p.avg != null)
    const avg = ok.length ? ok.reduce((a, p) => a + (p.avg as number), 0) / ok.length : null
    const loss = ps.length ? ps.reduce((a, p) => a + p.loss, 0) / ps.length : 0
    return { avg, loss, min: ok.length ? Math.min(...ok.map((p) => p.min)) : null, max: ok.length ? Math.max(...ok.map((p) => p.max)) : null }
  }
  return (
    <div className="tchart">
      <div className="ctitle">
        <span>{t('三网延迟')}</span>
        <span className="seg-mini">{ranges.map((r) => <button key={r.h} type="button" className={hours === r.h ? 'on' : ''} onClick={() => setHours(r.h)}>{r.label}</button>)}</span>
      </div>
      {times.length < 2 ? <div className="sub" style={{ marginTop: 10 }}>{t('数据还不够：节点在线后每 30 秒测一次，每 5 分钟记录一个点')}</div> : <>
        <div className="small mute" style={{ marginTop: 10 }}>
          {series.map(({ tg }) => <span key={tg.id} className="legend" style={{ marginRight: 12 }}><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span>)}
        </div>
        <Lines labels={labels} fmt={(v) => `${v.toFixed(0)} ms`} height={170} series={series.map(({ tg, vs }) => ({ name: tg.name, color: colorOf(tg.carrier), values: vs }))} />
        <div className="small mute" style={{ marginTop: 6 }}>{t('丢包率')}</div>
        <Lines labels={labels} max={100} fmt={(v) => `${v.toFixed(0)}%`} height={90} series={series.map(({ tg, loss }) => ({ name: tg.name, color: colorOf(tg.carrier), values: loss }))} />
        <div className="scroll" style={{ marginTop: 8 }}>
          <table>
            <thead><tr><th>{t('目标')}</th><th>{t('平均')}</th><th>{t('最低')}</th><th>{t('最高')}</th><th>{t('丢包')}</th></tr></thead>
            <tbody>{targets.map((tg) => { const s = stat(tg); return (
              <tr key={tg.id}><td><span className="legend"><i style={{ background: colorOf(tg.carrier) }} />{tg.name}</span></td>
                <td className="mono">{s.avg != null ? s.avg.toFixed(1) + ' ms' : '—'}</td><td className="mono">{s.min != null ? s.min.toFixed(0) : '—'}</td>
                <td className="mono">{s.max != null ? s.max.toFixed(0) : '—'}</td><td className="mono">{s.loss.toFixed(1)}%</td></tr>) })}</tbody>
          </table>
        </div>
      </>}
    </div>
  )
}

function NodeDrawer({ n, onClose, onChanged }: { n: PNode; onClose: () => void; onChanged: () => void }) {
  const { busy, run } = useAction()
  return (
    <Drawer title={<>{flag(n.flag)} {n.name}</>} onClose={onClose} wide>
      <div className="sub" style={{ margin: 0 }}>
        {n.address && <span className="mono">{n.address}</span>}{n.agentVersion && <> · Agent {n.agentVersion}</>}
      </div>
      <LatencyChart nodeId={n.id} />
      <Field label={t('公开状态页')} hint={t('关闭后这个节点不出现在公开状态页上，后台仍然照常探测')}>
        <label className="row small"><Switch label={t('在公开状态页显示')} on={!n.hidden} disabled={busy}
          onChange={(v) => run(async () => { await put(`/probe/nodes/${n.id}`, { hidden: !v }); onChanged() })} />{t('在公开状态页显示')}</label>
      </Field>
    </Drawer>
  )
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
      <div className="sub" style={{ margin: 0 }}>{t('每个在线节点每 30 秒对每个启用的目标做一次 TCP 连接，记录耗时和是否失败；按 5 分钟汇总，保留 7 天。')}</div>
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
        <Field label={t('延迟探测')}><label className="row small"><Switch label={t('延迟探测')} on={v.probe_enabled === 'true'} onChange={(x) => set('probe_enabled', String(x))} />{t('每 30 秒从各节点测一次延迟')}</label></Field>
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
  const data = useLoad(() => get<Overview>('/probe'))
  usePoll(data.reload, 15000)
  const [open, setOpen] = useState<string | null>(null)
  const [targets, setTargets] = useState(false)
  const [settings, setSettings] = useState(false)
  const [q, setQ] = useState('')
  const d = data.data
  const [region, setRegion] = useState('')
  const [view, setView] = useState<'card' | 'table'>(() => { try { return localStorage.getItem('probe-view') === 'table' ? 'table' : 'card' } catch { return 'card' } })
  useEffect(() => { try { localStorage.setItem('probe-view', view) } catch { /* private mode */ } }, [view])
  const nodes = (d?.nodes ?? []).filter((n) => (!q || (n.name + n.region).toLowerCase().includes(q.toLowerCase())) && (!region || n.flag === region))
  const regions = [...(d?.nodes ?? []).reduce((m, n) => n.flag ? m.set(n.flag, (m.get(n.flag) ?? 0) + 1) : m, new Map<string, number>())]
  const online = (d?.nodes ?? []).filter((n) => n.online).length
  const sel = d?.nodes.find((n) => n.id === open)
  return (
    <>
      <div className="top">
        <div>
          <h1>{t('探针')}</h1>
          <div className="sub">{d ? t('在线 {on} / {all} · 延迟为节点到各目标的 TCP 连接耗时', { on: online, all: d.nodes.length }) : t('加载中…')}
            {d && !d.probeEnabled && <> · <b style={{ color: 'var(--warn)' }}>{t('延迟探测已关闭')}</b></>}</div>
        </div>
        <div className="row">
          <input className="input" style={{ width: 160 }} placeholder={t('搜索节点')} value={q} onChange={(e) => setQ(e.target.value)} aria-label={t('搜索节点')} />
          <button className="btn" type="button" onClick={() => setTargets(true)}>{t('探测目标')}</button>
          <button className="btn" type="button" onClick={() => setSettings(true)}>{t('设置')}{d?.statusPage ? ' · ' + t('已公开') : ''}</button>
        </div>
      </div>
      {d && d.nodes.length === 0 ? <Empty>{t('还没有节点')}</Empty> : (
        <div className="pv">
          <ProbeSummary nodes={d?.nodes ?? []} />
          <div className="pv-bar-row">
            <div className="pv-chips">
              <button type="button" className={'pv-chip' + (region === '' ? ' on' : '')} onClick={() => setRegion('')}>{t('全部')} {d?.nodes.length ?? 0}</button>
              {regions.map(([f, c]) => <button key={f} type="button" className={'pv-chip' + (region === f ? ' on' : '')} onClick={() => setRegion(region === f ? '' : f)}>{flag(f)} {c}</button>)}
            </div>
            <span className="pv-seg">
              <button type="button" className={view === 'card' ? 'on' : ''} onClick={() => setView('card')}>{t('卡片')}</button>
              <button type="button" className={view === 'table' ? 'on' : ''} onClick={() => setView('table')}>{t('表格')}</button>
            </span>
          </div>
          {view === 'table' ? <ProbeTable nodes={nodes} targets={d?.targets ?? []} onOpen={setOpen} /> : (
            <div className="pv-grid">
              {nodes.map((n) => <NodeProbeCard key={n.id} n={n} targets={d?.targets ?? []} onOpen={() => setOpen(n.id)} />)}
            </div>
          )}
        </div>
      )}
      {sel && <NodeDrawer n={sel} onClose={() => setOpen(null)} onChanged={data.reload} />}
      {targets && <Targets nodes={d?.nodes ?? []} onClose={() => setTargets(false)} onChanged={data.reload} />}
      {settings && <StatusSettings url={d?.statusUrl ?? ''} onClose={() => setSettings(false)} onSaved={() => { setSettings(false); data.reload() }} />}
    </>
  )
}
