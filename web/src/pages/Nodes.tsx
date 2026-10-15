import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { CopyBox, copyText, Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, bytes, coreColor, coreLabel, datetime, nodeStatus, protoLabel, uptime, userStatus } from '../format'
import type { CapSet, Core, CoreType, Node, Revision, User } from '../types'

// useCoreDefaults returns the version each core type gets when added
// (the configured default, or the newest release when it is "latest").
function useCoreDefaults(): Record<string, string> {
  const [d, setD] = useState<Record<string, string>>({})
  useEffect(() => {
    let live = true
    get<Record<string, string>>('/core-defaults').then((r) => { if (live) setD(r) }).catch(() => {})
    return () => { live = false }
  }, [])
  return d
}

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

const vparts = (v: string) => v.replace(/^v/, '').split('.').map((x) => parseInt(x, 10) || 0)
export const versionLess = (a: string, b: string) => {
  const pa = vparts(a), pb = vparts(b)
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) if ((pa[i] ?? 0) !== (pb[i] ?? 0)) return (pa[i] ?? 0) < (pb[i] ?? 0)
  return false
}

/** Upgrade one core type on many nodes at once. */
export function BatchCoreUpgrade({ nodes, latest, onClose, onDone }: { nodes: Node[]; latest: Record<string, string>; onClose: () => void; onDone: () => void }) {
  const types = [...new Set(nodes.flatMap((n) => n.cores.map((c) => c.type)))] as CoreType[]
  const [type, setType] = useState<CoreType>(types[0] ?? 'xray')
  const [version, setVersion] = useState(latest[types[0] ?? 'xray'] ?? '')
  const withCore = nodes.filter((n) => n.cores.some((c) => c.type === type))
  const [ids, setIds] = useState<string[]>(withCore.map((n) => n.id))
  const { busy, run } = useAction()
  const pick = (t: CoreType) => {
    setType(t); setVersion(latest[t] ?? '')
    setIds(nodes.filter((n) => n.cores.some((c) => c.type === t)).map((n) => n.id))
  }
  return (
    <Modal title="批量升级内核" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !version || !ids.length} onClick={() => run(async () => {
        const r = await post<{ results: { nodeId: string; ok: boolean; error?: string }[] }>('/cores/upgrade', { type, version, nodeIds: ids })
        const bad = r.results.filter((x) => !x.ok)
        if (bad.length) throw new Error(bad.map((x) => `${nodes.find((n) => n.id === x.nodeId)?.name}：${x.error}`).join('；'))
        onDone()
      }, `已下发：${ids.length} 个节点切换到 v${version}`)}>升级 {ids.length} 个节点</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>旧版本保留在节点上；新版本启动失败时 Agent 会自动回退，之后也可一键回退。</div>
      <Field label="内核">
        <div className="row">{types.map((t) => <button key={t} type="button" className={'chip' + (type === t ? ' on' : '')} onClick={() => pick(t)}>{coreLabel[t]}</button>)}</div>
      </Field>
      <Field label="目标版本" hint={latest[type] ? `GitHub 最新稳定版 v${latest[type]}` : '未能获取最新版本，请手动填写'}>
        <input className="input mono" value={version} onChange={(e) => setVersion(e.target.value.replace(/^v/, ''))} placeholder="26.3.27" />
      </Field>
      <Field label="节点">
        <div className="pick-list">
          {withCore.map((n) => {
            const cur = n.cores.find((c) => c.type === type)
            return (
              <label key={n.id} className="chk">
                <input type="checkbox" checked={ids.includes(n.id)} onChange={(e) => setIds(e.target.checked ? [...ids, n.id] : ids.filter((x) => x !== n.id))} />
                {n.name}<span className="mono small mute">v{cur?.version}{cur && version && cur.version === version ? '（已是）' : ''}</span>
              </label>
            )
          })}
        </div>
      </Field>
    </Modal>
  )
}

type NodeUser = { id: string; username: string; status: string; via: string[]; inbounds: number; up: number; down: number; onlineIps: string[] }

const viaLabel = (v: string) => (v === 'node' ? '整台节点' : v === 'inbound' ? '单独入站' : '分组 · ' + v.replace(/^group:/, ''))

/** Users authorised on this node, how they got access, and their usage here. */
function NodeUsers({ node }: { node: Node }) {
  const list = useLoad(() => get<NodeUser[]>(`/nodes/${node.id}/users`), [node.id])
  const [picking, setPicking] = useState(false)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const rows = list.data ?? []
  const online = rows.filter((r) => r.onlineIps.length > 0).length
  return (
    <div className="card flush" style={{ gridColumn: '1 / -1' }}>
      <div className="ctitle" style={{ padding: '12px 16px 4px' }}>
        <span>用户 <span className="sub" style={{ margin: 0, fontWeight: 400 }}>{rows.length} 个有权使用 · {online} 个在线</span></span>
        <button className="btn sm" type="button" onClick={() => setPicking(true)}>+ 绑定用户</button>
      </div>
      {rows.length === 0 ? <Empty>{list.loading ? '加载中…' : '还没有用户能使用此节点。可在这里绑定用户，或在用户 / 分组中授权。'}</Empty> : (
        <div className="scroll">
          <table>
            <thead><tr><th>用户</th><th>状态</th><th>授权方式</th><th>可用入站</th><th>本节点流量（30 天）</th><th>在线 IP</th><th></th></tr></thead>
            <tbody>
              {rows.map((r) => {
                const st = userStatus[r.status] ?? { label: r.status, cls: 'off' }
                return (
                  <tr key={r.id}>
                    <td style={{ fontWeight: 500 }}>{r.onlineIps.length > 0 && <span className="dot live" style={{ background: 'var(--ok)', color: 'var(--ok)' }} />}{r.username}</td>
                    <td><span className={'st ' + st.cls}>{st.label}</span></td>
                    <td>{r.via.map((v) => <span key={v} className={'tag' + (v === 'node' ? ' xray' : '')}>{viaLabel(v)}</span>)}</td>
                    <td className="mono">{r.inbounds}</td>
                    <td className="mono">↑ {bytes(r.up)} · ↓ {bytes(r.down)}</td>
                    <td className="mono small">{r.onlineIps.join(', ') || '—'}</td>
                    <td style={{ textAlign: 'right' }}>
                      {r.via.includes('node') && (
                        <button className="link" type="button" style={{ color: 'var(--bad)' }} disabled={busy} onClick={async () => {
                          if (await confirm(`解除 ${r.username} 对 ${node.name} 的整台绑定？${r.via.length > 1 ? '\n（通过分组 / 单独入站的授权仍然保留）' : ''}`, { danger: true, ok: '解绑' }))
                            run(async () => { await post(`/nodes/${node.id}/users`, { userIds: [r.id], detach: true }); list.reload() }, '已解绑')
                        }}>解绑</button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
      {picking && <PickUsers exclude={rows.filter((r) => r.via.includes('node')).map((r) => r.id)} onClose={() => setPicking(false)} onPick={(ids) => run(async () => {
        await post(`/nodes/${node.id}/users`, { userIds: ids }); setPicking(false); list.reload()
      }, `已绑定 ${ids.length} 个用户`)} />}
    </div>
  )
}

function PickUsers({ exclude, onClose, onPick }: { exclude: string[]; onClose: () => void; onPick: (ids: string[]) => void }) {
  const [q, setQ] = useState('')
  const users = useLoad(() => get<{ items: User[] }>('/users?limit=1000'))
  const [ids, setIds] = useState<string[]>([])
  const shown = (users.data?.items ?? []).filter((u) => !exclude.includes(u.id) && u.username.toLowerCase().includes(q.toLowerCase()))
  return (
    <Modal title="绑定用户到此节点" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={!ids.length} onClick={() => onPick(ids)}>绑定 {ids.length || ''}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>整台绑定：用户可使用本节点的全部入站，以后新增的入站也自动生效。</div>
      <input className="input" placeholder="搜索用户名" value={q} onChange={(e) => setQ(e.target.value)} autoFocus />
      <div className="pick-list">
        {shown.map((u) => (
          <label key={u.id} className="chk">
            <input type="checkbox" checked={ids.includes(u.id)} onChange={(e) => setIds(e.target.checked ? [...ids, u.id] : ids.filter((x) => x !== u.id))} />
            {u.username}<span className="small mute">{u.remark}</span>
          </label>
        ))}
        {shown.length === 0 && <div className="mute small">{users.loading ? '加载中…' : '没有可绑定的用户'}</div>}
      </div>
    </Modal>
  )
}

type Install = { cmd: string; nodeId: string }

/** Install command with download / mirror variants and a live "waiting for the node" state. */
function InstallModal({ install, node, onClose }: { install: Install; node?: Node; onClose: () => void }) {
  const [tool, setTool] = useState<'curl' | 'wget'>('curl')
  const [mirror, setMirror] = useState('')
  const [wasOnline] = useState(node?.status === 'online')
  const script = install.cmd.match(/(\S+\/install\.sh)/)?.[1] ?? ''
  const args = install.cmd.split('bash -s -- ')[1] ?? ''
  const fetchCmd = tool === 'curl' ? `curl -fsSL ${script}` : `wget -qO- ${script}`
  const extra = mirror.trim() ? ` --mirror ${mirror.trim()}` : ''
  const cmd = `${fetchCmd} | bash -s -- ${args}${extra}`
  const online = node?.status === 'online' && !wasOnline
  return (
    <Modal title={`安装节点 · ${node?.name ?? ''}`} onClose={onClose} wide>
      <ol className="isteps">
        <li className="done"><b>复制命令</b><span>24 小时内有效，仅能使用一次</span></li>
        <li className={online ? 'done' : 'active'}><b>在节点服务器以 root 执行</b><span>自动检查连通性、下载并校验 Agent、注册上线</span></li>
        <li className={online ? 'done' : ''}><b>{online ? '节点已上线' : '等待节点连接…'}</b><span>{online ? `Agent ${node?.agentVersion ?? ''} 已接入，内核将按需安装` : '执行完成后这里会自动更新'}</span></li>
      </ol>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <div className="seg" style={{ width: 200 }}>
          {(['curl', 'wget'] as const).map((t) => <button key={t} type="button" className={'opt' + (tool === t ? ' on' : '')} onClick={() => setTool(t)}>{t}</button>)}
        </div>
        <input className="input mono" style={{ flex: 1, minWidth: 220 }} value={mirror} onChange={(e) => setMirror(e.target.value)} placeholder="GitHub 镜像（可选，如 https://ghfast.top/）" aria-label="GitHub 镜像" />
      </div>
      <CopyBox text={cmd} />
      {online ? <div className="note ok-note">节点已成功接入，可以关闭此窗口，到「入站」中为它添加服务。</div>
        : <div className="wait-line"><span className="pulse" />等待节点连接</div>}
      <details className="more">
        <summary>本机安装、升级与卸载</summary>
        <div className="small" style={{ display: 'grid', gap: 8, marginTop: 8 }}>
          <div>在<b>主控服务器本机</b>安装节点：直接执行上面的命令即可，脚本会自动改用 127.0.0.1 连接；也可在主控上运行 <span className="mono">polaris node</span>（或菜单 17）一键完成。</div>
          <div>升级 Agent 到面板版本：<span className="mono">{fetchCmd} | bash -s -- --upgrade</span></div>
          <div>卸载：<span className="mono">{fetchCmd} | bash -s -- --uninstall</span>（加 <span className="mono">--purge</span> 删除数据）</div>
          <div className="mute">Agent 主动连接主控，节点无需开放管理端口，NAT 后的机器也能接入；主控需放行节点接入端口（默认 9443）。</div>
        </div>
      </details>
    </Modal>
  )
}

function NodeForm({ node, onClose, onSaved }: { node?: Node; onClose: () => void; onSaved: (install?: Install) => void }) {
  const [f, setF] = useState({
    name: node?.name ?? '', address: node?.address ?? '', domain: node?.domain ?? '', region: node?.region ?? '',
    flag: node?.flag ?? '', labels: (node?.labels ?? []).join(', '), trafficRate: node?.trafficRate ?? 1,
    defaultCore: (node?.defaultCore ?? 'xray') as CoreType, xray: true, singbox: false, ddns: node?.ddns ?? false, firewall: !(node?.noFirewall ?? false),
  })
  const { busy, run } = useAction()
  const defaults = useCoreDefaults()
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    const body: any = {
      name: f.name, address: f.address, domain: f.domain, region: f.region, flag: f.flag, trafficRate: Number(f.trafficRate),
      labels: f.labels.split(/[,\s]+/).filter(Boolean), defaultCore: f.defaultCore, ddns: f.ddns, noFirewall: !f.firewall, sortOrder: node?.sortOrder ?? 0,
    }
    if (node) {
      await put('/nodes/' + node.id, body)
      onSaved()
    } else {
      // An empty version is resolved server-side to the configured default.
      body.cores = [f.xray && { type: 'xray', version: '' }, f.singbox && { type: 'singbox', version: '' }].filter(Boolean)
      if (!body.cores.length) throw new Error('至少选择一个内核')
      if (!body.cores.find((c: any) => c.type === f.defaultCore)) body.defaultCore = body.cores[0].type
      const r = await post<{ command: string; node: Node }>('/nodes', body)
      onSaved({ cmd: r.command, nodeId: r.node.id })
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
        <Field label="公网地址" htmlFor="n-addr" hint="订阅里客户端连接的 IP 或域名；留空则使用 Agent 自动检测到的公网 IP"><input id="n-addr" className="input mono" value={f.address} onChange={(e) => set('address', e.target.value)} placeholder="203.0.113.10" /></Field>
        <Field label="节点域名" htmlFor="n-domain" hint="用于 ACME 证书与 {node.domain}"><input id="n-domain" className="input mono" value={f.domain} onChange={(e) => set('domain', e.target.value)} placeholder="jp1.example.com" /></Field>
        <Field label="国家代码" htmlFor="n-flag" hint="如 JP，订阅显示旗帜"><input id="n-flag" className="input mono" maxLength={2} value={f.flag} onChange={(e) => set('flag', e.target.value.toUpperCase())} /></Field>
        <Field label="流量倍率" htmlFor="n-rate" hint="结算时按倍率换算"><input id="n-rate" className="input mono" type="number" step="0.1" min="0.1" value={f.trafficRate} onChange={(e) => set('trafficRate', e.target.value)} /></Field>
        <Field label="DDNS" full hint="开启后 Agent 上报的公网 IP 变化时，自动更新「域名与 DDNS」中绑定到此节点的记录；地址为空时订阅使用上报的 IP">
          <label className="row small"><Switch label="DDNS" on={f.ddns} onChange={(v) => set('ddns', v)} />自动跟随节点公网 IP</label>
        </Field>
        <Field label="防火墙" full hint="Agent 自动在节点的 ufw / firewalld / iptables 中放行入站端口，删除入站后收回；只关闭自己放行的端口，不动已有规则。云厂商的安全组（如 Oracle 安全列表、AWS 安全组）需在云控制台另行放行">
          <label className="row small"><Switch label="自动放行端口" on={f.firewall} onChange={(v) => set('firewall', v)} />自动放行入站端口</label>
        </Field>
        <Field label="标签" htmlFor="n-labels" full hint="逗号分隔；模板可绑定标签，新节点上线后自动套用"><input id="n-labels" className="input" value={f.labels} onChange={(e) => set('labels', e.target.value)} placeholder="asia, cn2" /></Field>
      </div>
      {!node && (
        <div className="field">
          <span className="lbl">安装内核</span>
          <label className="chk"><input type="checkbox" checked={f.xray} onChange={(e) => set('xray', e.target.checked)} />Xray-core{defaults.xray ? ` v${defaults.xray}` : ''}</label>
          <label className="chk"><input type="checkbox" checked={f.singbox} onChange={(e) => set('singbox', e.target.checked)} />sing-box{defaults.singbox ? ` v${defaults.singbox}` : ''}</label>
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

export function CoreForm({ node, core, onClose, onSaved }: { node: Node; core?: Core; onClose: () => void; onSaved: () => void }) {
  const [type, setType] = useState<CoreType>(core?.type ?? (node.cores.find((c) => c.type === 'singbox') ? 'xray' : 'singbox'))
  const defaults = useCoreDefaults()
  const [version, setVersion] = useState(core?.version ?? '')
  useEffect(() => { if (!core && !version && defaults[type]) setVersion(defaults[type]) }, [defaults]) // eslint-disable-line react-hooks/exhaustive-deps
  const [isDefault, setDefault] = useState(core?.isDefault ?? false)
  const [available, setAvailable] = useState<string[] | null>(null)
  useEffect(() => {
    let live = true
    setAvailable(null)
    get<{ versions: string[] }>('/core-versions?type=' + type)
      .then((r) => { if (live) setAvailable(r.versions) })
      .catch(() => { if (live) setAvailable([]) })
    return () => { live = false }
  }, [type])
  const installed = core?.installedVersions ?? []
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
                onClick={() => { setType(c); setVersion(defaults[c] ?? '') }}>
                {coreLabel[c]}<small>{coreHint[c]}</small>
              </button>
            ))}
          </div>
        </Field>
      )}
      <Field label="版本" htmlFor="c-ver" hint={core?.installedVersions?.length ? `已安装：${core.installedVersions.join('、')}（切回旧版本可秒级回滚）` : '从官方 GitHub Release 下载，并校验 SHA256'}>
        <input id="c-ver" className="input mono" value={version} onChange={(e) => setVersion(e.target.value)} list="core-versions" />
        <datalist id="core-versions">{[...new Set([...(available ?? []), ...installed])].map((v) => <option key={v} value={v} />)}</datalist>
      </Field>
      <div className="chips" style={{ marginBottom: 12 }} aria-label="可选版本">
        {available === null && <span className="sub">正在获取官方版本列表…</span>}
        {available?.length === 0 && <span className="sub">无法获取版本列表（可在设置中配置 GitHub 镜像），请手动填写版本号</span>}
        {available?.slice(0, 10).map((v, i) => (
          <button key={v} type="button" className={'chip' + (version === v ? ' on' : '')} onClick={() => setVersion(v)}>
            v{v}{i === 0 ? ' · 最新' : ''}{installed.includes(v) ? ' · 已安装' : ''}{core?.version === v ? ' · 当前' : ''}
          </button>
        ))}
      </div>
      <label className="chk"><input type="checkbox" checked={isDefault} onChange={(e) => setDefault(e.target.checked)} />设为节点默认内核</label>
    </Modal>
  )
}

export default function Nodes() {
  const { nodes, refresh } = useNodes()
  const [params, setParams] = useSearchParams()
  const [selId, setSelId] = useState<string | null>(params.get('id'))
  const [editing, setEditing] = useState<Node | 'new' | null>(params.get('new') ? 'new' : null)
  const [install, setInstall] = useState<Install | null>(null)
  const [coreForm, setCoreForm] = useState<{ core?: Core } | null>(null)
  const [preview, setPreview] = useState<{ title: string; text: string } | null>(null)
  const toast = useToast()
  const confirm = useConfirm()
  const { busy, run } = useAction()
  const sel = useMemo(() => nodes.find((n) => n.id === selId) ?? nodes[0], [nodes, selId])
  const caps = useLoad(() => (sel ? get<CapSet[]>('/capabilities?node=' + sel.id) : Promise.resolve([])), [sel?.id, sel?.cores.map((c) => c.version).join()])
  const revs = useLoad(() => (sel ? get<Revision[]>(`/nodes/${sel.id}/revisions`) : Promise.resolve([])), [sel?.id, sel?.desiredRev, sel?.appliedRev])

  useEffect(() => { if (params.get('new')) { setParams({}, { replace: true }) } }, [params, setParams])

  const latest = useLoad(() => get<Record<string, string>>('/core-latest').catch(() => ({} as Record<string, string>)))
  const [batchCore, setBatchCore] = useState(false)
  // Agent upgrades: nodes whose agent differs from the panel version.
  const info = useLoad(() => get<{ version: string }>('/info'))
  const panelVer = info.data?.version ?? ''
  const vnorm = (v: string) => v.replace(/^v/, '')
  const outdated = (n: Node) => !!panelVer && panelVer !== 'dev' && n.status === 'online' && !!n.agentVersion && vnorm(n.agentVersion) !== vnorm(panelVer)
  const outdatedNodes = nodes.filter(outdated)
  const [upgrading, setUpgrading] = useState<Record<string, boolean>>({})
  const upgradeOne = async (n: Node) => {
    if (!(await confirm(`把 ${n.name} 的 Agent 从 v${vnorm(n.agentVersion)} 升级到 v${vnorm(panelVer)}？\n升级时内核会重启，节点上的连接会中断几秒。`))) return
    setUpgrading((x) => ({ ...x, [n.id]: true }))
    await run(() => post(`/nodes/${n.id}/upgrade-agent`), `${n.name} 已升级，正在重新连接`)
    setUpgrading((x) => ({ ...x, [n.id]: false }))
  }
  const upgradeAll = async () => {
    if (!(await confirm(`升级 ${outdatedNodes.length} 个节点的 Agent 到 v${vnorm(panelVer)}？\n每个节点的内核会重启，连接会中断几秒。`))) return
    setUpgrading(Object.fromEntries(outdatedNodes.map((n) => [n.id, true])))
    await run(async () => {
      const r = await post<{ results: { node: string; ok: boolean; error?: string }[] }>('/nodes/upgrade-agents')
      const bad = r.results.filter((x) => !x.ok)
      if (bad.length) throw new Error(`${r.results.length - bad.length} 个成功，失败：` + bad.map((x) => `${x.node}（${x.error}）`).join('；'))
    }, '全部节点已升级')
    setUpgrading({})
  }

  const showInstall = (n: Node) => run(async () => {
    const r = await post<{ command: string }>(`/nodes/${n.id}/install-token`)
    setInstall({ cmd: r.command, nodeId: n.id })
  })

  return (
    <>
      <div className="top">
        <div><h1>节点与内核</h1><div className="sub">每个节点运行一个 Agent，Agent 托管一个或多个内核子进程</div></div>
        <div className="row">
          {outdatedNodes.length > 0 && (
            <button className="btn" type="button" disabled={busy} onClick={upgradeAll}>
              <span className="badge-dot" />升级全部 Agent（{outdatedNodes.length}）
            </button>
          )}
          {nodes.some((n) => n.cores.length > 0) && <button className="btn" type="button" onClick={() => setBatchCore(true)}>批量升级内核</button>}
          <button className="btn pri" type="button" onClick={() => setEditing('new')}>+ 添加节点</button>
        </div>
      </div>
      {nodes.length === 0 ? <div className="card"><Empty>还没有节点。点击「添加节点」生成一行安装命令，在服务器上执行即可接入。</Empty></div> : (
        <>
          <div className="nodes" role="group" aria-label="选择节点">
            {nodes.map((n) => {
              const st = nodeStatus[n.status] ?? nodeStatus.pending
              return (
                <button key={n.id} type="button" className={'ncard' + (sel?.id === n.id ? ' on' : '')} aria-pressed={sel?.id === n.id} onClick={() => setSelId(n.id)}>
                  <div style={{ fontWeight: 600 }}><span className={'dot' + (n.status === 'online' ? ' live' : '')} style={{ background: st.color, color: st.color }} />{n.name}</div>
                  <div className="sub">{[n.region, n.address || n.domain || (n.publicIpv4 || n.publicIpv6 ? `${n.publicIpv4 || n.publicIpv6}（自动检测）` : '')].filter(Boolean).join(' · ') || <span style={{ color: 'var(--warn-ink)' }}>无公网地址</span>} · {n.status === 'online' ? `${n.cores.length} 个内核` : st.label}
                    {upgrading[n.id] ? <span className="tag info-tag">升级中…</span> : outdated(n) && <span className="tag warn">可升级</span>}</div>
                </button>
              )
            })}
          </div>
          {sel && (
            <>
              <section className="card">
                <div className="sync">
                  <div><span className="sub" style={{ margin: 0 }}>Agent</span><div className="mono">{sel.agentVersion ? 'v' + vnorm(sel.agentVersion) : '未接入'}
                    {outdated(sel) && <span className="small" style={{ color: 'var(--warn)', marginLeft: 6 }}>→ v{vnorm(panelVer)}</span>}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>期望 / 已应用版本</span><div className="mono" style={sel.appliedRev < sel.desiredRev ? { color: 'var(--warn)' } : undefined}>rev {sel.desiredRev} / rev {sel.appliedRev}{sel.appliedRev === sel.desiredRev && sel.desiredRev ? ' ✓' : ''}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>最后心跳</span><div className="mono">{ago(sel.lastSeen)}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>公网 IP{sel.ddns ? ' · DDNS' : ''}</span><div className="mono">{[sel.publicIpv4, sel.publicIpv6].filter(Boolean).join(' / ') || '—'}</div></div>
                  <div><span className="sub" style={{ margin: 0 }}>系统</span><div className="mono">{sel.connected ? `CPU ${sel.system.cpu}% · 内存 ${sel.system.mem}% · 运行 ${uptime(sel.system.uptime)}` : '—'}</div></div>
                  <div className="row" style={{ marginLeft: 'auto' }}>
                    {outdated(sel) && (
                      <button className="btn sm pri" type="button" disabled={busy || upgrading[sel.id]} onClick={() => upgradeOne(sel)}>
                        {upgrading[sel.id] ? <><span className="spinner" />升级中…</> : '升级 Agent'}
                      </button>
                    )}
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
                        <div><span>版本</span><button className="link mono" type="button" onClick={() => setCoreForm({ core: c })}>v{c.version}</button>
                          {c.runningVersion && c.runningVersion !== c.version && <div className="small" style={{ color: 'var(--warn)' }}>切换中 · 运行 v{c.runningVersion}</div>}</div>
                        <div><span>入站</span><span className="mono" style={{ color: 'var(--ink)', fontSize: 13 }}>{c.inbounds}</span></div>
                        <div><span>运行时长</span><span className="mono" style={{ color: 'var(--ink)', fontSize: 13 }}>{running && c.startedAt ? ago(c.startedAt).replace('前', '') : '—'}</span></div>
                      </div>
                      <div className="caps">{capsSummary(cap).map((x) => <span key={x} className="tag">{x}</span>)}</div>
                      <div className="sub" style={{ margin: 0 }}>用户变更：{cap?.hotUsers ? (c.type === 'xray' ? 'gRPC 热更新，不中断连接' : 'HTTP 认证回调，无需重载') : c.type === 'singbox' ? 'SIGHUP 重载' : '重启进程'}</div>
                      {c.error && <div className="note bad">{c.error}</div>}
                      {(() => {
                        const lv = latest.data?.[c.type]
                        const canUp = lv && versionLess(c.version, lv)
                        const canBack = c.prevVersion && c.prevVersion !== c.version
                        if (!canUp && !canBack) return null
                        return (
                          <div className="core-ver">
                            {canUp && <span className="small">新版本 <b className="mono">v{lv}</b></span>}
                            {canUp && <button className="btn sm pri" type="button" disabled={busy} onClick={async () => {
                              if (await confirm(`把 ${sel.name} 的 ${coreLabel[c.type]} 从 v${c.version} 升级到 v${lv}？\n新版本启动失败会自动回退；升级期间该内核的连接会中断几秒。`))
                                run(async () => { await post(`/nodes/${sel.id}/cores`, { type: c.type, version: lv, isDefault: c.isDefault }); refresh() }, '已下发升级')
                            }}>升级</button>}
                            {canBack && <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                              if (await confirm(`把 ${coreLabel[c.type]} 回退到 v${c.prevVersion}？（旧版本已在节点上，秒级切换）`))
                                run(async () => { await post(`/cores/${c.id}/rollback`); refresh() }, `已回退到 v${c.prevVersion}`)
                            }}>回退到 v{c.prevVersion}</button>}
                          </div>
                        )
                      })()}
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
                        {c.type === 'xray' && (
                          <button className="btn sm" type="button" disabled={!sel.connected} title="立即下载设置中配置的 .dat 文件并重载 Xray" onClick={() => run(async () => {
                            const r = await post<{ output: string }>(`/nodes/${sel.id}/geodata-update`, {})
                            setPreview({ title: '更新 Geo 文件', text: r.output || '完成' })
                          })}>更新 Geo 文件</button>
                        )}
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
                <NodeUsers node={sel} />
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
      {editing && <NodeForm node={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} onSaved={(i) => { setEditing(null); refresh(); if (i) setInstall(i) }} />}
      {coreForm && sel && <CoreForm node={sel} core={coreForm.core} onClose={() => setCoreForm(null)} onSaved={() => { setCoreForm(null); refresh() }} />}
      {batchCore && <BatchCoreUpgrade nodes={nodes} latest={latest.data ?? {}} onClose={() => setBatchCore(false)} onDone={() => { setBatchCore(false); refresh() }} />}
      {install && <InstallModal install={install} node={nodes.find((n) => n.id === install.nodeId)} onClose={() => setInstall(null)} />}
      {preview && (
        <Modal title={preview.title} onClose={() => setPreview(null)} wide footer={<button className="btn" type="button" onClick={() => { copyText(preview.text).then(() => toast('已复制'), () => toast('复制失败，请手动选择', true)) }}>复制</button>}>
          <div className="code">{preview.text}</div>
        </Modal>
      )}
    </>
  )
}
