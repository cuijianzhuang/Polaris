import { useState } from 'react'
import { del, get, post, put } from '../api'
import { Lines } from '../components/Charts'
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
}
type TargetView = { id: string; name: string; carrier: string }
type Target = TargetView & { addr: string; sortOrder: number; enabled: boolean }
type Overview = { nodes: PNode[]; targets: TargetView[]; at: string; probeEnabled: boolean; statusPage: boolean; statusUrl: string }

export const carrierColor: Record<string, string> = { ct: '#2563EB', cu: '#DC2626', cm: '#16A34A' }
const colorOf = (c: string) => carrierColor[c] ?? '#7C3AED'
const carriers = [{ v: 'ct', l: t('电信') }, { v: 'cu', l: t('联通') }, { v: 'cm', l: t('移动') }, { v: '', l: t('其他') }]

function Meter({ pct }: { pct: number }) {
  const cls = pct >= 90 ? ' bad' : pct >= 75 ? ' warn' : ''
  return <div className={'nc-net-bar' + cls}><span style={{ width: Math.min(100, Math.max(0, pct)) + '%' }} /></div>
}

function Spark({ vs, color }: { vs: (number | null)[]; color: string }) {
  const ok = vs.filter((v): v is number => v != null)
  const max = Math.max(0, ...ok)
  if (!max) return <svg className="pr-spark" />
  const min = Math.min(...ok)
  const lo = Math.max(0, min - Math.max(5, (max - min) * 0.3)), hi = Math.max(max, lo + 10) // relative: shows the wobble
  const n = vs.length
  const segs: string[][] = []
  let cur: string[] = []
  vs.forEach((v, i) => {
    if (v == null) { if (cur.length) segs.push(cur); cur = []; return }
    cur.push(`${((i / (n - 1)) * 100).toFixed(1)},${(17 - ((v - lo) / (hi - lo)) * 15).toFixed(1)}`)
  })
  if (cur.length) segs.push(cur)
  return (
    <svg className="pr-spark" viewBox="0 0 100 18" preserveAspectRatio="none">
      {segs.map((p, i) => p.length === 1
        ? <circle key={i} cx={p[0].split(',')[0]} cy={p[0].split(',')[1]} r="1" fill={color} />
        : <polyline key={i} fill="none" stroke={color} strokeWidth="1.4" vectorEffect="non-scaling-stroke" points={p.join(' ')} />)}
    </svg>
  )
}

export function LatencyRows({ n, targets }: { n: PNode; targets: TargetView[] }) {
  if (!targets.length) return null
  return (
    <div className="pr-lat">
      {targets.map((tg) => {
        const l = n.latency[tg.id]
        const c = colorOf(tg.carrier)
        let v = <span className="mute">—</span>
        if (l && n.online) v = l.ms ? <>{l.ms.toFixed(0)} ms{l.loss >= 1 && <span className="pr-loss"> {l.loss.toFixed(0)}%</span>}</> : <span className="pr-dead">{t('超时')}</span>
        return (
          <div key={tg.id} className="pr-lt" title={l?.avg != null ? t('近 1 小时平均 {ms} ms · 丢包 {loss}%', { ms: l.avg.toFixed(0), loss: l.loss.toFixed(1) }) : undefined}>
            <span className="pr-k"><i style={{ background: c }} />{tg.name}</span>
            {l ? <Spark vs={l.spark} color={c} /> : <svg className="pr-spark" />}
            <span className="pr-v mono">{v}</span>
          </div>
        )
      })}
    </div>
  )
}

function NodeProbeCard({ n, targets, onOpen }: { n: PNode; targets: TargetView[]; onOpen: () => void }) {
  return (
    <button type="button" className={'card pr-card' + (n.online ? '' : ' off')} onClick={onOpen}>
      <div className="pr-top">
        <span className={'pr-dot' + (n.online ? '' : ' off')} />
        <span>{flag(n.flag)}</span>
        <b className="pr-name">{n.name}</b>
        {n.hidden && <span className="tag">{t('不公开')}</span>}
        <span className="mute small">{n.region}</span>
      </div>
      {n.online ? <div className="nc-net">
        <div className="nc-net-row"><span>CPU</span><span className="mono">{n.cpu.toFixed(0)}%</span></div><Meter pct={n.cpu} />
        <div className="nc-net-row"><span>{t('内存')}</span><span className="mono">{n.mem.toFixed(0)}%</span></div><Meter pct={n.mem} />
        <div className="nc-net-row"><span>{t('网速')}</span><span className="mono">↓ {bps(n.rxBps)} · ↑ {bps(n.txBps)}</span></div>
        <div className="nc-net-row"><span>{t('运行')}</span><span className="mono">{uptime(n.uptime)} · {t('负载')} {n.load1.toFixed(2)}</span></div>
        {n.netLimit > 0 && <><div className="nc-net-row"><span>{t('本月流量')}</span><span className="mono">{bytes(n.netUsed)} / {bytes(n.netLimit)}</span></div><Meter pct={(100 * n.netUsed) / n.netLimit} /></>}
      </div> : <div className="nc-net"><div className="nc-net-row"><span>{t('离线')}</span><span className="mono">{n.lastSeen ? datetime(n.lastSeen) : '—'}</span></div></div>}
      <LatencyRows n={n} targets={targets} />
    </button>
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
  const [f, setF] = useState({ name: tg?.name ?? '', carrier: tg?.carrier ?? 'ct', addr: tg?.addr ?? '', sortOrder: tg?.sortOrder ?? 0, enabled: tg?.enabled ?? true })
  const online = nodes.filter((n) => n.online)
  const [node, setNode] = useState(online[0]?.id ?? '')
  const [test, setTest] = useState('')
  const { busy, run } = useAction()
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  return (
    <Modal title={tg ? t('编辑探测目标') : t('添加探测目标')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.name || !f.addr} onClick={() => run(async () => {
        if (tg) await put(`/probe/targets/${tg.id}`, f); else await post('/probe/targets', f)
        onSaved()
      }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <Field label={t('名称')} htmlFor="pt-name"><input id="pt-name" className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder={t('广东电信')} /></Field>
      <Field label={t('运营商')} hint={t('决定图表里的颜色')}>
        <div className="seg">{carriers.map((c) => <button key={c.v} type="button" className={'opt' + (f.carrier === c.v ? ' on' : '')} aria-pressed={f.carrier === c.v} onClick={() => set('carrier', c.v)}>{c.l}</button>)}</div>
      </Field>
      <Field label={t('地址')} htmlFor="pt-addr" hint={t('主机:端口，用 TCP 连接测延迟。省级三网地址格式：省份-ct/cu/cm-v4.ip.zstaticcdn.com:80，如 gd-ct-v4.ip.zstaticcdn.com:80')}>
        <input id="pt-addr" className="input mono" value={f.addr} onChange={(e) => set('addr', e.target.value)} placeholder="gd-ct-v4.ip.zstaticcdn.com:80" />
      </Field>
      <Field label={t('排序')} htmlFor="pt-sort"><input id="pt-sort" className="input mono" type="number" value={f.sortOrder} onChange={(e) => set('sortOrder', Number(e.target.value) || 0)} /></Field>
      <Field label={t('启用')}><label className="row small"><Switch label={t('启用')} on={f.enabled} onChange={(v) => set('enabled', v)} />{t('参与定时探测')}</label></Field>
      {online.length > 0 && <Field label={t('试测')} hint={test || undefined}>
        <div className="row">
          <select className="input" value={node} onChange={(e) => setNode(e.target.value)} aria-label={t('节点')}>{online.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select>
          <button className="btn sm" type="button" disabled={busy || !f.addr} onClick={() => run(async () => {
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
  const v = f ?? (s.data ? { probe_enabled: s.data.probe_enabled ?? 'true', status_page: s.data.status_page ?? 'false', status_title: s.data.status_title ?? '', status_note: s.data.status_note ?? '', status_show_traffic: s.data.status_show_traffic ?? 'false' } : null)
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
  const nodes = (d?.nodes ?? []).filter((n) => !q || (n.name + n.region).toLowerCase().includes(q.toLowerCase()))
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
        <div className="pr-grid">
          {nodes.map((n) => <NodeProbeCard key={n.id} n={n} targets={d?.targets ?? []} onOpen={() => setOpen(n.id)} />)}
        </div>
      )}
      {sel && <NodeDrawer n={sel} onClose={() => setOpen(null)} onChanged={data.reload} />}
      {targets && <Targets nodes={d?.nodes ?? []} onClose={() => setTargets(false)} onChanged={data.reload} />}
      {settings && <StatusSettings url={d?.statusUrl ?? ''} onClose={() => setSettings(false)} onSaved={() => { setSettings(false); data.reload() }} />}
    </>
  )
}
