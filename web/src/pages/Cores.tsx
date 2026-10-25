// Cores across every node: versions, status, upgrades and rollbacks in one
// place (per-node management stays on the node page).
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { get, post } from '../api'
import { useNodes } from '../App'
import RolloutPanel from '../components/Rollouts'
import GeodataCard from '../components/GeodataCard'
import { NodeActivity } from '../components/NodeCard'
import { Empty, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { ago, coreColor, coreLabel, coreTagClass, coreInstalling } from '../format'
import { t } from '../i18n'
import type { Core, CoreType, Node } from '../types'
import { BatchCoreUpgrade, CoreForm, versionLess } from './Nodes'

type Row = { node: Node; core: Core }

export default function Cores() {
  const { nodes, refresh } = useNodes()
  const [tick, setTick] = useState(0)
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
  // Nodes installing / switching cores right now, or whose last try failed.
  const active = nodes.filter((n) => n.progress || (n.connected && n.appliedRev < n.desiredRev && n.status !== 'error'))

  const upgrade = async (r: Row, v: string) => {
    if (await confirm(t('把 {node} 的 {core} 从 v{from} 升级到 v{to}？', { node: r.node.name, core: coreLabel[r.core.type], from: r.core.version, to: v }) + '\n' + t('新版本启动失败会自动回退；升级期间该内核的连接会中断几秒。')))
      run(async () => { await post(`/nodes/${r.node.id}/cores`, { type: r.core.type, version: v, isDefault: r.core.isDefault }); refresh() }, t('已下发升级'))
  }

  return (
    <>
      <div className="top">
        <div><h1>{t('内核')}</h1><div className="sub">{t('全部节点上的内核实例、版本与升级；单个节点的安装与卸载在「节点」页')}</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={() => latest.reload()}>{t('刷新最新版本')}</button>
          {rows.length > 0 && <button className="btn pri" type="button" onClick={() => setBatch(true)}>{t('批量升级')}</button>}
        </div>
      </div>
      <RolloutPanel nodes={nodes} refresh={tick} />

      {active.length > 0 && (
        <section className="card" aria-label={t('节点活动')}>
          <div className="ctitle"><span>{t('进行中')}</span><span className="sub" style={{ margin: 0 }}>{t('下载、安装和切换内核的实时进度')}</span></div>
          <div style={{ display: 'grid', gap: 10 }}>
            {active.map((n) => (
              <div key={n.id}>
                <div className="small" style={{ fontWeight: 500, marginBottom: 4 }}>{n.flag ? n.flag + ' ' : ''}{n.name}</div>
                <NodeActivity n={n} />
              </div>
            ))}
          </div>
        </section>
      )}

      {types.length > 0 && (
        <section className="grid g4" aria-label={t('内核概览')}>
          {types.map((ty) => {
            const of = rows.filter((r) => r.core.type === ty)
            const vers = new Map<string, number>()
            of.forEach((r) => vers.set(r.core.version, (vers.get(r.core.version) ?? 0) + 1))
            const run_ = of.filter((r) => r.core.status === 'running').length
            const old = of.filter(outdated).length
            return (
              <button key={ty} type="button" className={'card kpi ncard' + (filter === ty ? ' on' : '')} style={{ textAlign: 'left' }} onClick={() => setFilter(filter === ty ? 'all' : ty)}>
                <div className="label"><span className="swatch" style={{ background: coreColor[ty], marginRight: 6 }} />{coreLabel[ty]}</div>
                <div className="val">{of.length} <span className="small mute" style={{ fontWeight: 400 }}>{t('实例 · {n} 运行中', { n: run_ })}</span></div>
                <div className="foot mono">{[...vers.entries()].sort((a, b) => (versionLess(a[0], b[0]) ? 1 : -1)).map(([v, n]) => `v${v}×${n}`).join('  ')}</div>
                <div className="foot">{lv[ty] ? <>{t('最新')} <b className="mono">v{lv[ty]}</b>{old ? <span style={{ color: 'var(--warn)' }}> · {t('{n} 个可升级', { n: old })}</span> : ' · ' + t('均为最新')}</> : t('最新版本未知')}</div>
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
                {f === 'all' ? t('全部 {n}', { n: rows.length }) : f === 'outdated' ? t('可升级 {n}', { n: nOutdated }) : coreLabel[f]}
              </button>
            ))}
          </div>
        </div>
        {shown.length === 0 ? <Empty>{rows.length === 0 ? t('还没有节点安装内核') : t('没有符合条件的内核')}</Empty> : (
          <div className="scroll">
            <table>
              <thead><tr><th>{t('节点')}</th><th>{t('内核')}</th><th>{t('版本')}</th><th>{t('状态')}</th><th>{t('入站')}</th><th>{t('运行时长')}</th><th /></tr></thead>
              <tbody>
                {shown.map((r) => {
                  const { node: n, core: c } = r
                  const running = c.status === 'running'
                  const up = outdated(r)
                  const back = c.prevVersion && c.prevVersion !== c.version
                  return (
                    <tr key={c.id}>
                      <td><button className="link" type="button" onClick={() => nav('/nodes?id=' + n.id)}>{n.name}</button>{n.region && <span className="small mute"> · {n.region}</span>}</td>
                      <td><span className={coreTagClass(c.type)}>{coreLabel[c.type]}</span>{c.isDefault && <span className="tag">{t('默认')}</span>}</td>
                      <td className="mono">
                        v{c.version}{up && <span className="tag warn" style={{ marginLeft: 6 }}>{t('可升级到 v{v}', { v: lv[c.type] })}</span>}
                        {c.runningVersion && c.runningVersion !== c.version && <div className="small" style={{ color: 'var(--warn)' }}>{t('切换中 · 运行 v{v}', { v: c.runningVersion })}</div>}
                      </td>
                      <td>
                        <span className={'st ' + (running ? 'ok' : coreInstalling(c, n) && n.connected ? 'warn' : 'off')}>{running ? t('● 运行中') : coreInstalling(c, n) ? (n.connected ? t('安装中') : t('等待节点上线')) : c.inbounds ? t('已停止') : t('空闲')}</span>
                        {!n.connected && !coreInstalling(c, n) && <div className="small mute">{t('节点离线')}</div>}
                        {n.progress?.active && n.progress.stage.includes(c.type) && <div style={{ marginTop: 4, maxWidth: 260 }}><NodeActivity n={n} compact /></div>}
                        {c.error && <div className="small" style={{ color: 'var(--bad)', maxWidth: 260 }}>{c.error}</div>}
                      </td>
                      <td className="mono">{c.inbounds}</td>
                      <td className="mono small">{running && c.startedAt ? ago(c.startedAt).replace(/\s*(?:\u524d|ago)$/, '') : '—'}</td>
                      <td>
                        <div className="row" style={{ flexWrap: 'nowrap', justifyContent: 'flex-end' }}>
                          {up && <button className="btn sm pri" type="button" disabled={busy} onClick={() => upgrade(r, lv[c.type])}>{t('升级')}</button>}
                          {back && <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                            if (await confirm(t('把 {node} 的 {core} 回退到 v{v}？（旧版本已在节点上，秒级切换）', { node: n.name, core: coreLabel[c.type], v: c.prevVersion })))
                              run(async () => { await post(`/cores/${c.id}/rollback`); refresh() }, t('已回退到 v{v}', { v: c.prevVersion }))
                          }}>{t('回退')}</button>}
                          <button className="btn sm" type="button" onClick={() => setSwitching(r)}>{t('切换版本')}</button>
                          <button className="btn sm" type="button" disabled={!n.connected || busy} onClick={() => run(() => post(`/cores/${c.id}/restart`), t('已重启'))}>{t('重启')}</button>
                          <button className="btn sm" type="button" disabled={!n.connected} onClick={() => run(async () => {
                            const x = await get<{ logs: string; error: string }>(`/cores/${c.id}/logs?lines=300`)
                            setPreview({ title: `${n.name} · ${coreLabel[c.type]} ` + t('日志'), text: x.logs || x.error || t('（暂无日志）') })
                          })}>{t('日志')}</button>
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

      {batch && <BatchCoreUpgrade nodes={nodes} latest={lv} onClose={() => setBatch(false)} onDone={() => { setBatch(false); setTick((x) => x + 1); refresh() }} />}
      {switching && <CoreForm node={switching.node} core={switching.core} onClose={() => setSwitching(null)} onSaved={() => { setSwitching(null); refresh() }} />}
      {preview && (
        <Modal wide title={preview.title} onClose={() => setPreview(null)} footer={<button className="btn" type="button" onClick={() => setPreview(null)}>{t('关闭')}</button>}>
          <pre className="mono" style={{ maxHeight: '60vh', overflow: 'auto', margin: 0, fontSize: 12, whiteSpace: 'pre-wrap' }}>{preview.text}</pre>
        </Modal>
      )}
    </>
  )
}
