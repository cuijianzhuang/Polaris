import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { CopyBox, Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, coreColor, coreLabel, datetime, nodeStatus, protoLabel, uptime } from '../format'
import type { CapSet, Core, CoreType, Node, Revision } from '../types'

const DEFAULT_VERSIONS: Record<string, string> = { xray: '26.3.27', singbox: '1.13.5', hysteria: '2.6.1', forwarder: '2.7.0', mihomo: '1.19.15', caddy: '2.10.2' }

function capsSummary(c?: CapSet): string[] {
  if (!c) return []
  const out = new Set<string>()
  for (const x of c.combos) {
    out.add(protoLabel[x.protocol])
    if (x.security === 'reality') out.add('REALITY')
    if (x.transport === 'xhttp') out.add('XHTTP')
    if (x.transport === 'kcp') out.add('mKCP')
  }
  if (c.flows?.length) out.add('Vision')
  return [...out]
}

function NodeForm({ node, onClose, onSaved }: { node?: Node; onClose: () => void; onSaved: (cmd?: string) => void }) {
  const [f, setF] = useState({
    name: node?.name ?? '', address: node?.address ?? '', domain: node?.domain ?? '', region: node?.region ?? '',
    flag: node?.flag ?? '', labels: (node?.labels ?? []).join(', '), trafficRate: node?.trafficRate ?? 1,
    defaultCore: (node?.defaultCore ?? 'xray') as CoreType, xray: true, singbox: false, ddns: node?.ddns ?? false,
  })
  const { busy, run } = useAction()
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    const body: any = {
      name: f.name, address: f.address, domain: f.domain, region: f.region, flag: f.flag, trafficRate: Number(f.trafficRate),
      labels: f.labels.split(/[,\s]+/).filter(Boolean), defaultCore: f.defaultCore, ddns: f.ddns,
    }
    if (node) {
      await put('/nodes/' + node.id, body)
      onSaved()
    } else {
      body.cores = [f.xray && { type: 'xray', version: DEFAULT_VERSIONS.xray }, f.singbox && { type: 'singbox', version: DEFAULT_VERSIONS.singbox }].filter(Boolean)
      if (!body.cores.length) throw new Error('至少选择一个内核')
      if (!body.cores.find((c: any) => c.type === f.defaultCore)) body.defaultCore = body.cores[0].type
      const r = await post<{ command: string }>('/nodes', body)
      onSaved(r.command)
    }
  }, node ? '已保存' : '节点已创建')
  return (
    <Drawer title={node ? '编辑节点' : '添加节点'} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !f.name} onClick={save}>{node ? '保存' : '创建并生成安装命令'}</button>
    </>}>
      <div className="form-grid">
        <Field label="名称" htmlFor="n-name"><input id="n-name" className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="tokyo-1" /></Field>
        <Field label="地区" htmlFor="n-region"><input id="n-region" className="input" value={f.region} onChange={(e) => set('region', e.target.value)} placeholder="东京" /></Field>
        <Field label="公网地址" htmlFor="n-addr" hint="订阅里客户端连接的 IP 或域名"><input id="n-addr" className="input mono" value={f.address} onChange={(e) => set('address', e.target.value)} placeholder="203.0.113.10" /></Field>
        <Field label="节点域名" htmlFor="n-domain" hint="用于 ACME 证书与 {node.domain}"><input id="n-domain" className="input mono" value={f.domain} onChange={(e) => set('domain', e.target.value)} placeholder="jp1.example.com" /></Field>
        <Field label="国家代码" htmlFor="n-flag" hint="如 JP，订阅显示旗帜"><input id="n-flag" className="input mono" maxLength={2} value={f.flag} onChange={(e) => set('flag', e.target.value.toUpperCase())} /></Field>
        <Field label="流量倍率" htmlFor="n-rate" hint="结算时按倍率换算"><input id="n-rate" className="input mono" type="number" step="0.1" min="0.1" value={f.trafficRate} onChange={(e) => set('trafficRate', e.target.value)} /></Field>
        <Field label="DDNS" full hint="开启后 Agent 上报的公网 IP 变化时，自动更新「域名与 DDNS」中绑定到此节点的记录；地址为空时订阅使用上报的 IP">
          <label className="row small"><Switch label="DDNS" on={f.ddns} onChange={(v) => set('ddns', v)} />自动跟随节点公网 IP</label>
        </Field>
        <Field label="标签" htmlFor="n-labels" full hint="逗号分隔；模板可绑定标签，新节点上线后自动套用"><input id="n-labels" className="input" value={f.labels} onChange={(e) => set('labels', e.target.value)} placeholder="asia, cn2" /></Field>
      </div>
      {!node && (
        <div className="field">
          <span className="lbl">安装内核</span>
          <label className="chk"><input type="checkbox" checked={f.xray} onChange={(e) => set('xray', e.target.checked)} />Xray-core v{DEFAULT_VERSIONS.xray}</label>
          <label className="chk"><input type="checkbox" checked={f.singbox} onChange={(e) => set('singbox', e.target.checked)} />sing-box v{DEFAULT_VERSIONS.singbox}</label>
        </div>
      )}
      <Field label="默认内核" hint="两个内核都支持某组合时，新入站默认落到这里">
        <div className="seg">
          {(['xray', 'singbox'] as CoreType[]).map((c) => (
            <button key={c} type="button" className={'opt' + (f.defaultCore === c ? ' on' : '')} aria-pressed={f.defaultCore === c} onClick={() => set('defaultCore', c)}>{coreLabel[c]}</button>
          ))}
        </div>
      </Field>
    </Drawer>
  )
}

const coreHint: Record<string, string> = {
  xray: 'gRPC 热增删用户', singbox: 'SIGHUP 重载', hysteria: '官方服务端 · 多入站 · HTTP 认证回调',
  mihomo: 'listeners · 控制器热重载', caddy: 'CDN 443 端口共享回源（按需自动安装）', forwarder: 'L4 端口转发 · 中转链路',
}

function CoreForm({ node, core, onClose, onSaved }: { node: Node; core?: Core; onClose: () => void; onSaved: () => void }) {
  const [type, setType] = useState<CoreType>(core?.type ?? (node.cores.find((c) => c.type === 'singbox') ? 'xray' : 'singbox'))
  const [version, setVersion] = useState(core?.version ?? DEFAULT_VERSIONS[type] ?? '')
  const [isDefault, setDefault] = useState(core?.isDefault ?? false)
  const { busy, run } = useAction()
  return (
    <Modal title={core ? `切换 ${coreLabel[core.type]} 版本` : '安装内核'} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !version} onClick={() => run(async () => {
        await post(`/nodes/${node.id}/cores`, { type, version, isDefault })
        onSaved()
      }, '已下发，Agent 将下载并校验 SHA256 后切换')}>确定</button>
    </>}>
      {!core && (
        <Field label="内核">
          <div className="seg">
            {(['xray', 'singbox', 'hysteria', 'mihomo', 'caddy', 'forwarder'] as CoreType[]).map((c) => (
              <button key={c} type="button" className={'opt' + (type === c ? ' on' : '')}
                onClick={() => { setType(c); setVersion(DEFAULT_VERSIONS[c] ?? '') }}>
                {coreLabel[c]}<small>{coreHint[c]}</small>
              </button>
            ))}
          </div>
        </Field>
      )}
      <Field label="版本" htmlFor="c-ver" hint={core?.installedVersions?.length ? `已安装：${core.installedVersions.join('、')}（切回旧版本可秒级回滚）` : '从官方 GitHub Release 下载，并校验 SHA256'}>
        <input id="c-ver" className="input mono" value={version} onChange={(e) => setVersion(e.target.value)} list="core-versions" />
        <datalist id="core-versions">{(core?.installedVersions ?? []).map((v) => <option key={v} value={v} />)}</datalist>
      </Field>
      <label className="chk"><input type="checkbox" checked={isDefault} onChange={(e) => setDefault(e.target.checked)} />设为节点默认内核</label>
    </Modal>
  )
}

export default function Nodes() {
  const { nodes, refresh } = useNodes()
  const [params, setParams] = useSearchParams()
  const [selId, setSelId] = useState<string | null>(params.get('id'))
  const [editing, setEditing] = useState<Node | 'new' | null>(params.get('new') ? 'new' : null)
  const [installCmd, setInstallCmd] = useState('')
  const [coreForm, setCoreForm] = useState<{ core?: Core } | null>(null)
  const [preview, setPreview] = useState<{ title: string; text: string } | null>(null)
  const toast = useToast()
  const confirm = useConfirm()
  const { busy, run } = useAction()
  const sel = useMemo(() => nodes.find((n) => n.id === selId) ?? nodes[0], [nodes, selId])
  const caps = useLoad(() => (sel ? get<CapSet[]>('/capabilities?node=' + sel.id) : Promise.resolve([])), [sel?.id, sel?.cores.map((c) => c.version).join()])
  const revs = useLoad(() => (sel ? get<Revision[]>(`/nodes/${sel.id}/revisions`) : Promise.resolve([])), [sel?.id, sel?.desiredRev, sel?.appliedRev])

  useEffect(() => { if (params.get('new')) { setParams({}, { replace: true }) } }, [params, setParams])

  const showInstall = (n: Node) => run(async () => {
    const r = await post<{ command: string }>(`/nodes/${n.id}/install-token`)
    setInstallCmd(r.command)
  })

  return (
    <>
      <div className="top">
        <div><h1>节点与内核</h1><div className="sub">每个节点运行一个 Agent，Agent 托管一个或多个内核子进程</div></div>
        <button className="btn pri" type="button" onClick={() => setEditing('new')}>+ 添加节点</button>
      </div>
      {nodes.length === 0 ? <div className="card"><Empty>还没有节点。点击「添加节点」生成一行安装命令，在服务器上执行即可接入。</Empty></div> : (
        <>
          <div className="nodes" role="group" aria-label="选择节点">
            {nodes.map((n) => {
              const st = nodeStatus[n.status] ?? nodeStatus.pending
              return (
                <button key={n.id} type="button" className={'ncard' + (sel?.id === n.id ? ' on' : '')} aria-pressed={sel?.id === n.id} onClick={() => setSelId(n.id)}>
                  <div style={{ fontWeight: 600 }}><span className="dot" style={{ background: st.color }} />{n.name}</div>
                  <div className="sub">{[n.region, n.address].filter(Boolean).join(' · ') || '未填写地址'} · {n.status === 'online' ? `${n.cores.length} 个内核` : st.label}</div>
                </button>
              )
            })}
          </div>
          {sel && (
            <>
              <section className="card">
                <div className="sync">
                  <div><span className="sub" style={{ margin: 0 }}>Agent</span><div className="mono">{sel.agentVersion ? 'v' + sel.agentVersion.replace(/^v/, '') : '未接入'}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>期望 / 已应用版本</span><div className="mono" style={sel.appliedRev < sel.desiredRev ? { color: 'var(--warn)' } : undefined}>rev {sel.desiredRev} / rev {sel.appliedRev}{sel.appliedRev === sel.desiredRev && sel.desiredRev ? ' ✓' : ''}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>最后心跳</span><div className="mono">{ago(sel.lastSeen)}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>公网 IP{sel.ddns ? ' · DDNS' : ''}</span><div className="mono">{[sel.publicIpv4, sel.publicIpv6].filter(Boolean).join(' / ') || '—'}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>系统</span><div className="mono">{sel.connected ? `CPU ${sel.system.cpu}% · 内存 ${sel.system.mem}% · 运行 ${uptime(sel.system.uptime)}` : '—'}</div></div>
                  <div className="row" style={{ marginLeft: 'auto' }}>
                    <button className="btn sm" type="button" disabled={busy} onClick={() => run(() => post(`/nodes/${sel.id}/sync`), '已强制同步')}>强制同步</button>
                    <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                      if (await confirm('回滚到上一个成功应用的配置版本？')) run(() => post(`/nodes/${sel.id}/rollback`, {}), '已回滚')
                    }}>回滚到上一版</button>
                    <button className="btn sm" type="button" onClick={() => showInstall(sel)}>安装命令</button>
                    <button className="btn sm" type="button" onClick={() => setEditing(sel)}>编辑</button>
                  </div>
                </div>
                {sel.lastError && <div className="note bad" style={{ marginTop: 12 }}>最近一次下发失败：{sel.lastError}</div>}
              </section>
              <h2>内核</h2>
              <div className="grid g2">
                {sel.cores.map((c) => {
                  const cap = caps.data?.find((x) => x.core === c.type)
                  const running = c.status === 'running'
                  return (
                    <article key={c.id} className="core">
                      <div className="row" style={{ justifyContent: 'space-between' }}>
                        <div className="cname"><span className="swatch" style={{ background: coreColor[c.type] }} />{coreLabel[c.type]}{c.isDefault && <span className="tag">默认</span>}</div>
                        <span className={'st ' + (running ? 'ok' : c.status === 'installing' ? 'warn' : 'off')}>{running ? '● 运行中' : c.status === 'installing' ? '安装中' : c.inbounds ? '已停止' : '空闲'}</span>
                      </div>
                      <div className="meta">
                        <div><span>版本</span><button className="link mono" type="button" onClick={() => setCoreForm({ core: c })}>v{c.version}</button></div>
                        <div><span>入站</span><span className="mono" style={{ color: 'var(--ink)', fontSize: 13 }}>{c.inbounds}</span></div>
                        <div><span>运行时长</span><span className="mono" style={{ color: 'var(--ink)', fontSize: 13 }}>{running && c.startedAt ? ago(c.startedAt).replace('前', '') : '—'}</span></div>
                      </div>
                      <div className="caps">{capsSummary(cap).map((x) => <span key={x} className="tag">{x}</span>)}</div>
                      <div className="sub" style={{ margin: 0 }}>用户变更：{cap?.hotUsers ? (c.type === 'xray' ? 'gRPC 热更新，不中断连接' : 'HTTP 认证回调，无需重载') : c.type === 'singbox' ? 'SIGHUP 重载' : '重启进程'}</div>
                      {c.error && <div className="note bad">{c.error}</div>}
                      <div className="row">
                        <button className="btn sm" type="button" disabled={!sel.connected} onClick={() => run(() => post(`/cores/${c.id}/restart`), '已重启')}>重启</button>
                        <button className="btn sm" type="button" onClick={() => run(async () => {
                          const r = await get<{ config: string }>(`/nodes/${sel.id}/render?core=${c.type}`)
                          setPreview({ title: `${coreLabel[c.type]} 原生配置`, text: r.config })
                        })}>查看原生配置</button>
                        <button className="btn sm" type="button" disabled={!sel.connected} onClick={() => run(async () => {
                          const r = await get<{ logs: string; error: string }>(`/cores/${c.id}/logs?lines=300`)
                          setPreview({ title: `${coreLabel[c.type]} 日志`, text: r.logs || r.error || '（暂无日志）' })
                        })}>日志</button>
                        <button className="btn sm danger" type="button" onClick={async () => {
                          if (await confirm(`卸载 ${coreLabel[c.type]}？仍承载入站时会被拒绝。`, { danger: true })) run(async () => { await del('/cores/' + c.id); refresh() }, '已卸载')
                        }}>卸载</button>
                      </div>
                    </article>
                  )
                })}
                <article className="core add">
                  <div className="cname" style={{ fontSize: 15 }}>安装更多内核</div>
                  <div className="sub" style={{ margin: 0 }}>sing-box · Hysteria2 官方服务端 · mihomo · Caddy 回源 · realm 转发</div>
                  <button className="btn sm" type="button" style={{ marginTop: 8 }} onClick={() => setCoreForm({})}>选择内核与版本</button>
                </article>
                <div className="card flush" style={{ gridColumn: '1 / -1' }}>
                  <div className="ctitle" style={{ padding: '12px 16px 4px' }}><span>配置版本历史</span><span className="sub" style={{ margin: 0 }}>下发方式：hot = 热更新，reload = 重载</span></div>
                  <div className="scroll">
                    <table>
                      <thead><tr><th>版本</th><th>原因</th><th>状态</th><th>方式</th><th>时间</th><th></th></tr></thead>
                      <tbody>
                        {(revs.data ?? []).slice(0, 12).map((r) => (
                          <tr key={r.id}>
                            <td className="mono">rev {r.rev}</td>
                            <td style={{ whiteSpace: 'normal', maxWidth: 360 }}>{r.reason}{r.error && <div className="small" style={{ color: 'var(--bad)' }}>{r.error}</div>}</td>
                            <td><span className={'st ' + (r.status === 'applied' ? 'ok' : r.status === 'failed' ? 'bad' : 'warn')}>{r.status === 'applied' ? '已应用' : r.status === 'failed' ? '失败' : '待应用'}</span></td>
                            <td className="mono">{r.method || '—'}</td>
                            <td className="mono">{datetime(r.createdAt)}</td>
                            <td>{r.status === 'applied' && r.rev !== sel.appliedRev && (
                              <button className="link" type="button" onClick={async () => {
                                if (await confirm(`回滚到 rev ${r.rev}？`)) run(() => post(`/nodes/${sel.id}/rollback`, { rev: r.rev }), '已回滚')
                              }}>回滚到此版</button>
                            )}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              </div>
              <div className="row">
                <button className="btn sm danger" type="button" onClick={async () => {
                  if (await confirm(`吊销 ${sel.name} 的客户端证书？Agent 将无法再连接，需重新安装。`, { danger: true, ok: '吊销' }))
                    run(() => post(`/nodes/${sel.id}/revoke`), '已吊销')
                }}>吊销证书</button>
                <button className="btn sm danger" type="button" onClick={async () => {
                  if (await confirm(`删除节点 ${sel.name} 及其所有入站？此操作不可恢复。`, { danger: true, ok: '删除' }))
                    run(async () => { await del('/nodes/' + sel.id); setSelId(null); refresh() }, '已删除')
                }}>删除节点</button>
              </div>
            </>
          )}
        </>
      )}
      {editing && <NodeForm node={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} onSaved={(cmd) => { setEditing(null); refresh(); if (cmd) setInstallCmd(cmd) }} />}
      {coreForm && sel && <CoreForm node={sel} core={coreForm.core} onClose={() => setCoreForm(null)} onSaved={() => { setCoreForm(null); refresh() }} />}
      {installCmd && (
        <Modal title="安装命令" onClose={() => setInstallCmd('')} wide>
          <div className="sub" style={{ margin: 0 }}>在节点服务器上以 root 执行（24 小时内有效，一次性）。Agent 会注册上线、按需下载内核并校验 SHA256。</div>
          <CopyBox text={installCmd} />
          <div className="note">Agent 主动连接主控，节点无需开放管理端口，NAT 后的机器也能接入。</div>
        </Modal>
      )}
      {preview && (
        <Modal title={preview.title} onClose={() => setPreview(null)} wide footer={<button className="btn" type="button" onClick={() => { navigator.clipboard.writeText(preview.text).then(() => toast('已复制')) }}>复制</button>}>
          <div className="code">{preview.text}</div>
        </Modal>
      )}
    </>
  )
}
