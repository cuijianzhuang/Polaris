// Cores across every node: versions, status, upgrades and rollbacks in one
// place (per-node management stays on the node page).
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { get, post } from '../api'
import { useNodes } from '../App'
import GeodataCard from '../components/GeodataCard'
import { Empty, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { ago, coreColor, coreLabel, coreTagClass } from '../format'
import type { Core, CoreType, Node } from '../types'
import { BatchCoreUpgrade, CoreForm, versionLess } from './Nodes'

type Row = { node: Node; core: Core }

export default function Cores() {
  const { nodes, refresh } = useNodes()
  const nav = useNavigate()
  const latest = useLoad(() => get<Record<string, string>>('/core-latest').catch(() => ({} as Record<string, string>)))
  const [filter, setFilter] = useState<CoreType | 'all' | 'outdated'>('all')
  const [batch, setBatch] = useState(false)
  const [switching, setSwitching] = useState<Row | null>(null)
  const [preview, setPreview] = useState<{ title: string; text: string } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const lv = latest.data ?? {}

  const rows: Row[] = useMemo(() => nodes.flatMap((n) => n.cores.map((c) => ({ node: n, core: c }))), [nodes])
  const outdated = (r: Row) => !!lv[r.core.type] && versionLess(r.core.version, lv[r.core.type])
  const types = [...new Set(rows.map((r) => r.core.type))] as CoreType[]
  const shown = rows.filter((r) => filter === 'all' || (filter === 'outdated' ? outdated(r) : r.core.type === filter))
  const nOutdated = rows.filter(outdated).length

  const upgrade = async (r: Row, v: string) => {
    if (await confirm(`把 ${r.node.name} 的 ${coreLabel[r.core.type]} 从 v${r.core.version} 升级到 v${v}？\n新版本启动失败会自动回退；升级期间该内核的连接会中断几秒。`))
      run(async () => { await post(`/nodes/${r.node.id}/cores`, { type: r.core.type, version: v, isDefault: r.core.isDefault }); refresh() }, '已下发升级')
  }

  return (
    <>
      <div className="top">
        <div><h1>内核</h1><div className="sub">全部节点上的内核实例、版本与升级；单个节点的安装与卸载在「节点」页</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => latest.reload()}>刷新最新版本</button>
          {rows.length > 0 && <button className="btn pri" type="button" onClick={() => setBatch(true)}>批量升级</button>}
        </div>
      </div>

      {types.length > 0 && (
        <section className="grid g4" aria-label="内核概览">
          {types.map((t) => {
            const of = rows.filter((r) => r.core.type === t)
            const vers = new Map<string, number>()
            of.forEach((r) => vers.set(r.core.version, (vers.get(r.core.version) ?? 0) + 1))
            const run_ = of.filter((r) => r.core.status === 'running').length
            const old = of.filter(outdated).length
            return (
              <button key={t} type="button" className={'card kpi ncard' + (filter === t ? ' on' : '')} style={{ textAlign: 'left' }} onClick={() => setFilter(filter === t ? 'all' : t)}>
                <div className="label"><span className="swatch" style={{ background: coreColor[t], marginRight: 6 }} />{coreLabel[t]}</div>
                <div className="val">{of.length} <span className="small mute" style={{ fontWeight: 400 }}>实例 · {run_} 运行中</span></div>
                <div className="foot mono">{[...vers.entries()].sort((a, b) => (versionLess(a[0], b[0]) ? 1 : -1)).map(([v, n]) => `v${v}×${n}`).join('  ')}</div>
                <div className="foot">{lv[t] ? <>最新 <b className="mono">v{lv[t]}</b>{old ? <span style={{ color: 'var(--warn)' }}> · {old} 个可升级</span> : ' · 均为最新'}</> : '最新版本未知'}</div>
              </button>
            )
          })}
        </section>
      )}

      <section className="card flush">
        <div className="ctitle" style={{ padding: '12px 16px 4px' }}>
          <div className="row">
            {(['all', 'outdated', ...types] as const).map((f) => (
              <button key={f} type="button" className={'chip' + (filter === f ? ' on' : '')} onClick={() => setFilter(f)}>
                {f === 'all' ? `全部 ${rows.length}` : f === 'outdated' ? `可升级 ${nOutdated}` : coreLabel[f]}
              </button>
            ))}
          </div>
        </div>
        {shown.length === 0 ? <Empty>{rows.length === 0 ? '还没有节点安装内核' : '没有符合条件的内核'}</Empty> : (
          <div className="scroll">
            <table>
              <thead><tr><th>节点</th><th>内核</th><th>版本</th><th>状态</th><th>入站</th><th>运行时长</th><th /></tr></thead>
              <tbody>
                {shown.map((r) => {
                  const { node: n, core: c } = r
                  const running = c.status === 'running'
                  const up = outdated(r)
                  const back = c.prevVersion && c.prevVersion !== c.version
                  return (
                    <tr key={c.id}>
                      <td><button className="link" type="button" onClick={() => nav('/nodes?id=' + n.id)}>{n.name}</button>{n.region && <span className="small mute"> · {n.region}</span>}</td>
                      <td><span className={coreTagClass(c.type)}>{coreLabel[c.type]}</span>{c.isDefault && <span className="tag">默认</span>}</td>
                      <td className="mono">
                        v{c.version}{up && <span className="tag warn" style={{ marginLeft: 6 }}>可升级到 v{lv[c.type]}</span>}
                        {c.runningVersion && c.runningVersion !== c.version && <div className="small" style={{ color: 'var(--warn)' }}>切换中 · 运行 v{c.runningVersion}</div>}
                      </td>
                      <td>
                        <span className={'st ' + (running ? 'ok' : c.status === 'installing' ? 'warn' : 'off')}>{running ? '● 运行中' : c.status === 'installing' ? '安装中' : c.inbounds ? '已停止' : '空闲'}</span>
                        {!n.connected && <div className="small mute">节点离线</div>}
                        {c.error && <div className="small" style={{ color: 'var(--bad)', maxWidth: 260 }}>{c.error}</div>}
                      </td>
                      <td className="mono">{c.inbounds}</td>
                      <td className="mono small">{running && c.startedAt ? ago(c.startedAt).replace('前', '') : '—'}</td>
                      <td>
                        <div className="row" style={{ flexWrap: 'nowrap', justifyContent: 'flex-end' }}>
                          {up && <button className="btn sm pri" type="button" disabled={busy} onClick={() => upgrade(r, lv[c.type])}>升级</button>}
                          {back && <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                            if (await confirm(`把 ${n.name} 的 ${coreLabel[c.type]} 回退到 v${c.prevVersion}？（旧版本已在节点上，秒级切换）`))
                              run(async () => { await post(`/cores/${c.id}/rollback`); refresh() }, `已回退到 v${c.prevVersion}`)
                          }}>回退</button>}
                          <button className="btn sm" type="button" onClick={() => setSwitching(r)}>切换版本</button>
                          <button className="btn sm" type="button" disabled={!n.connected || busy} onClick={() => run(() => post(`/cores/${c.id}/restart`), '已重启')}>重启</button>
                          <button className="btn sm" type="button" disabled={!n.connected} onClick={() => run(async () => {
                            const x = await get<{ logs: string; error: string }>(`/cores/${c.id}/logs?lines=300`)
                            setPreview({ title: `${n.name} · ${coreLabel[c.type]} 日志`, text: x.logs || x.error || '（暂无日志）' })
                          })}>日志</button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {rows.some((r) => r.core.type === 'xray') && <GeodataCard />}

      {batch && <BatchCoreUpgrade nodes={nodes} latest={lv} onClose={() => setBatch(false)} onDone={() => { setBatch(false); refresh() }} />}
      {switching && <CoreForm node={switching.node} core={switching.core} onClose={() => setSwitching(null)} onSaved={() => { setSwitching(null); refresh() }} />}
      {preview && (
        <Modal wide title={preview.title} onClose={() => setPreview(null)} footer={<button className="btn" type="button" onClick={() => setPreview(null)}>关闭</button>}>
          <pre className="mono" style={{ maxHeight: '60vh', overflow: 'auto', margin: 0, fontSize: 12, whiteSpace: 'pre-wrap' }}>{preview.text}</pre>
        </Modal>
      )}
    </>
  )
}
