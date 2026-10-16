// Port forwarding (realm) on nodes, after realm-xwPF: several targets with
// load balancing and failover, ws / tls / wss tunnels, PROXY protocol.
import { useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { Icon } from '../components/Icons'
import { Drawer, Empty, Field, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { bytes } from '../format'

interface Remote { addr: string; weight?: number; enabled: boolean }
interface Rule {
  id: string; nodeId: string; nodeName: string; engine: string; tunnelAuth: string; rateIn: number; rateOut: number; maxConns: number; name: string; listenIp: string; listenPort: number; network: string
  remotes: Remote[]; balance: string; failover: boolean; through: string
  tunnelRole: string; tunnelType: string; tunnelHost: string; tunnelPath: string; tunnelSni: string
  sendProxy: number; acceptProxy: boolean; enabled: boolean; health?: Record<string, boolean>; remark: string
}

const netLabel: Record<string, string> = { tcp: 'TCP', udp: 'UDP', 'tcp,udp': 'TCP + UDP' }
const balLabel: Record<string, string> = { '': '主备', roundrobin: '轮询', iphash: 'IP 哈希' }
const gostTunnels: [string, string][] = [['wss', 'WSS（WebSocket + TLS）'], ['ws', 'WS'], ['tls', 'TLS'], ['mwss', 'MWSS（多路复用 WSS）'], ['mtls', 'MTLS（多路复用 TLS）'],
  ['grpc', 'gRPC'], ['quic', 'QUIC（UDP）'], ['kcp', 'KCP（UDP）']]
const realmTunnels: [string, string][] = [['wss', 'WSS（WebSocket + TLS）'], ['ws', 'WS'], ['tls', 'TLS']]
const randPass = () => Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => 'abcdefghijkmnpqrstuvwxyz23456789'[b % 32]).join('')

function RuleDrawer({ rule, onClose, onSaved }: { rule?: Rule; onClose: () => void; onSaved: () => void }) {
  const { nodes } = useNodes()
  const [f, setF] = useState({
    nodeId: rule?.nodeId ?? nodes[0]?.id ?? '', engine: rule?.engine ?? '', tunnelAuth: rule?.tunnelAuth ?? '',
    rateIn: rule?.rateIn ? String(rule.rateIn) : '', rateOut: rule?.rateOut ? String(rule.rateOut) : '', maxConns: rule?.maxConns ? String(rule.maxConns) : '',
    name: rule?.name ?? '', listenIp: rule?.listenIp ?? '', listenPort: rule ? String(rule.listenPort) : '',
    network: rule?.network ?? 'tcp,udp', balance: rule?.balance ?? '', failover: rule?.failover ?? false, through: rule?.through ?? '',
    tunnelRole: rule?.tunnelRole ?? '', tunnelType: rule?.tunnelType || 'wss', tunnelHost: rule?.tunnelHost ?? 'www.bing.com', tunnelPath: rule?.tunnelPath ?? '/ws',
    tunnelSni: rule?.tunnelSni ?? '', sendProxy: String(rule?.sendProxy ?? 0), acceptProxy: rule?.acceptProxy ?? false, remark: rule?.remark ?? '',
  })
  const [remotes, setRemotes] = useState<Remote[]>(rule?.remotes?.length ? rule.remotes : [{ addr: '', weight: 1, enabled: true }])
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const setRemote = (i: number, x: Partial<Remote>) => setRemotes(remotes.map((r, j) => (j === i ? { ...r, ...x } : r)))
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const save = () => run(async () => {
    const body = { ...f, listenPort: Number(f.listenPort) || 0, sendProxy: Number(f.sendProxy) || 0, enabled: rule ? rule.enabled : true,
      rateIn: Number(f.rateIn) || 0, rateOut: Number(f.rateOut) || 0, maxConns: Number(f.maxConns) || 0,
      tunnelType: f.tunnelRole ? f.tunnelType : '', remotes: remotes.filter((r) => r.addr.trim()).map((r) => ({ ...r, addr: r.addr.trim(), weight: Number(r.weight) || 1 })) }
    if (rule) await put('/forwards/' + rule.id, body)
    else await post('/forwards', body)
    onSaved()
  }, rule ? '已保存' : '转发规则已创建')
  const multi = remotes.filter((r) => r.addr.trim()).length > 1
  const gost = f.engine === 'gost'
  const tunnels = gost ? gostTunnels : realmTunnels
  return (
    <Drawer title={rule ? '编辑转发 · ' + rule.name : '添加端口转发'} onClose={onClose} footer={<>
      {rule && <button className="btn danger" type="button" style={{ marginRight: 'auto' }} onClick={async () => {
        if (await confirm(`删除转发规则 ${rule.name}？`, { danger: true, ok: '删除' })) run(async () => { await del('/forwards/' + rule.id); onSaved() }, '已删除')
      }}>删除</button>}
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !f.nodeId} onClick={save}>保存</button>
    </>}>
      <Field label="转发引擎">
        <div className="seg">
          <button type="button" className={'opt' + (!gost ? ' on' : '')} onClick={() => setF((x) => ({ ...x, engine: '', tunnelType: realmTunnels.some(([v]) => v === x.tunnelType) ? x.tunnelType : 'wss' }))}>realm<small>Rust 实现，资源占用极低，适合纯转发</small></button>
          <button type="button" className={'opt' + (gost ? ' on' : '')} onClick={() => set('engine', 'gost')}>GOST<small>隧道类型更多、UDP 隧道、限速限连、流量统计</small></button>
        </div>
      </Field>
      <div className="form-grid">
        <Field label="节点" full>
          <select className="input" value={f.nodeId} disabled={!!rule} onChange={(e) => set('nodeId', e.target.value)}>
            {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}
          </select>
        </Field>
        <Field label="名称"><input className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="例如 hk-to-jp" /></Field>
        <Field label="监听端口" hint="留空自动选择节点上未占用的端口；保存后自动在防火墙放行">
          <div className="joined">
            <input className="input mono" inputMode="numeric" value={f.listenPort} onChange={(e) => set('listenPort', e.target.value)} placeholder="自动" />
            <button className="btn" type="button" disabled={!f.nodeId} onClick={() => run(async () => { const r = await get<{ port: number }>(`/nodes/${f.nodeId}/free-port`); set('listenPort', String(r.port)) })}>随机</button>
          </div>
        </Field>
        <Field label="协议">
          <div className="seg">{(['tcp,udp', 'tcp', 'udp'] as const).map((n) => (
            <button key={n} type="button" className={'opt' + (f.network === n ? ' on' : '')} onClick={() => set('network', n)}>{netLabel[n]}</button>
          ))}</div>
        </Field>
        <Field label="监听地址" hint="留空监听全部地址"><input className="input mono" value={f.listenIp} onChange={(e) => set('listenIp', e.target.value)} placeholder="0.0.0.0 / ::" /></Field>
      </div>

      <Field label="转发目标" full hint="host:port；可添加多个，按负载方式分配。权重只在轮询 / IP 哈希时生效">
        <div style={{ display: 'grid', gap: 6 }}>
          {remotes.map((r, i) => (
            <div key={i} className="row" style={{ flexWrap: 'nowrap', gap: 6 }}>
              <input className="input mono" style={{ flex: 1 }} value={r.addr} onChange={(e) => setRemote(i, { addr: e.target.value })} placeholder="1.2.3.4:443" />
              <input className="input mono" style={{ width: 70 }} value={r.weight ?? 1} onChange={(e) => setRemote(i, { weight: Number(e.target.value) || 1 })} title="权重" aria-label="权重" />
              <Switch label="启用目标" on={r.enabled} onChange={(v) => setRemote(i, { enabled: v })} />
              {rule?.health?.[r.addr] && <span className="st bad">不可达</span>}
              <button className="icon-btn" type="button" aria-label="删除目标" disabled={remotes.length === 1} onClick={() => setRemotes(remotes.filter((_, j) => j !== i))}><Icon.Close /></button>
            </div>
          ))}
          <div><button className="btn sm" type="button" onClick={() => setRemotes([...remotes, { addr: '', weight: 1, enabled: true }])}>+ 添加目标</button></div>
        </div>
      </Field>
      {multi && (
        <div className="form-grid">
          <Field label="负载方式">
            <select className="input" value={f.balance} onChange={(e) => set('balance', e.target.value)}>
              <option value="">主备（只用第一个可用目标）</option><option value="roundrobin">轮询（按权重）</option><option value="iphash">IP 哈希（同一来源固定目标）</option>
            </select>
          </Field>
          <Field label="故障转移" hint={gost ? 'GOST 内置：连接目标失败即暂时跳过 30 秒' : '每 20 秒从节点探测各目标，连续 2 次不通就暂时移出，恢复后加回'}>
            <label className="row small"><Switch label="故障转移" on={f.failover} onChange={(v) => set('failover', v)} />{f.failover ? '开启' : '关闭'}</label>
          </Field>
        </div>
      )}

      <Field label="加密隧道（中转 ↔ 落地）" full hint={gost
        ? '两端都用 GOST：中转端把流量经 relay 隧道发给落地端（目标填落地端的地址:端口），落地端解密后转给它自己的目标；TCP 与 UDP 都能走隧道。两端的类型、Path、密码要一致'
        : '两端都用 realm：中转端加密发出（目标填落地端的地址:端口），落地端解密后转给本机服务；两端的类型、Host、Path、SNI 要一致。只支持 TCP'}>
        <div className="seg">
          {([['', '不加密'], ['client', '中转端（加密发送）'], ['server', '落地端（解密接收）']] as const).map(([v, l]) => (
            <button key={v} type="button" className={'opt' + (f.tunnelRole === v ? ' on' : '')} disabled={!!v && f.network === 'udp' && !gost} onClick={() => set('tunnelRole', v)}>{l}</button>
          ))}
        </div>
      </Field>
      {f.tunnelRole && (
        <div className="form-grid">
          <Field label="类型">
            <select className="input" value={f.tunnelType} onChange={(e) => set('tunnelType', e.target.value)}>
              {tunnels.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
            </select>
          </Field>
          {['ws', 'wss', 'mwss'].includes(f.tunnelType) && <Field label="Host"><input className="input mono" value={f.tunnelHost} onChange={(e) => set('tunnelHost', e.target.value)} /></Field>}
          {['ws', 'wss', 'mwss', 'grpc'].includes(f.tunnelType) && <Field label="Path"><input className="input mono" value={f.tunnelPath} onChange={(e) => set('tunnelPath', e.target.value)} /></Field>}
          {f.tunnelType !== 'ws' && f.tunnelType !== 'kcp' && <Field label="SNI" hint="留空同 Host；落地端用自签证书，中转端跳过校验"><input className="input mono" value={f.tunnelSni} onChange={(e) => set('tunnelSni', e.target.value)} /></Field>}
          {gost && (
            <Field label="隧道密码" hint="两端相同；落地端设置后只接受带此密码的中转端，防止被当作开放代理">
              <div className="joined">
                <input className="input mono" value={f.tunnelAuth} onChange={(e) => set('tunnelAuth', e.target.value)} placeholder="建议设置" />
                <button className="btn" type="button" onClick={() => set('tunnelAuth', randPass())}>生成</button>
              </div>
            </Field>
          )}
        </div>
      )}

      <details className="more">
        <summary>高级选项</summary>
        <div className="form-grid" style={{ marginTop: 10 }}>
          {gost ? <>
            <Field label="上行限速 (KB/s)" hint="整条规则合计；留空不限"><input className="input mono" inputMode="numeric" value={f.rateIn} onChange={(e) => set('rateIn', e.target.value)} /></Field>
            <Field label="下行限速 (KB/s)"><input className="input mono" inputMode="numeric" value={f.rateOut} onChange={(e) => set('rateOut', e.target.value)} /></Field>
            <Field label="最大并发连接" hint="留空不限"><input className="input mono" inputMode="numeric" value={f.maxConns} onChange={(e) => set('maxConns', e.target.value)} /></Field>
          </> : (
            <Field label="发送 PROXY protocol" hint="把客户端真实 IP 传给目标（目标需开启接收）">
              <select className="input" value={f.sendProxy} onChange={(e) => set('sendProxy', e.target.value)}>
                <option value="0">关闭</option><option value="1">v1</option><option value="2">v2</option>
              </select>
            </Field>
          )}
          <Field label="接收 PROXY protocol" hint="前面还有一层转发并发送 PROXY 头时开启">
            <label className="row small"><Switch label="接收 PROXY protocol" on={f.acceptProxy} onChange={(v) => set('acceptProxy', v)} />{f.acceptProxy ? '开启' : '关闭'}</label>
          </Field>
          {!gost && <Field label="出口地址" hint="多 IP 机器指定出站源 IP"><input className="input mono" value={f.through} onChange={(e) => set('through', e.target.value)} /></Field>}
          <Field label="备注"><input className="input" value={f.remark} onChange={(e) => set('remark', e.target.value)} /></Field>
        </div>
      </details>
    </Drawer>
  )
}

export default function Forwards() {
  const { nodes } = useNodes()
  const list = useLoad(() => get<Rule[]>('/forwards'))
  const stats = useLoad(() => get<{ nodeId: string; tag: string; up: number; down: number }[]>('/stats/tags?kind=inbound').catch(() => []))
  const traffic = (r: Rule) => (stats.data ?? []).find((x) => x.nodeId === r.nodeId && x.tag === 'fwd-' + r.id)
  const [drawer, setDrawer] = useState<Rule | 'new' | null>(null)
  const toast = useToast()
  const rows = list.data ?? []
  const toggle = async (r: Rule) => {
    try { await put('/forwards/' + r.id, { ...r, enabled: !r.enabled }); list.reload() } catch (e) { toast(String(e), true) }
  }
  return (
    <>
      <div className="top">
        <div><h1>端口转发</h1><div className="sub">节点上的端口转发，可选 realm 或 GOST 引擎：多目标负载均衡与故障转移、中转 ↔ 落地加密隧道；GOST 另支持 UDP 隧道、限速限连和流量统计；端口自动在防火墙放行</div></div>
        <button className="btn pri" type="button" disabled={!nodes.length} onClick={() => setDrawer('new')}>+ 添加转发</button>
      </div>
      <div className="card flush scroll">
        {rows.length === 0 ? <Empty>{list.loading ? '加载中…' : '还没有转发规则。中转机把端口转到落地机，或把服务端口映射出去，都可以在这里配置。'}</Empty> : (
          <table>
            <thead><tr><th>名称</th><th>节点</th><th>引擎</th><th>监听</th><th>协议</th><th>目标</th><th>负载</th><th>隧道 / 限制</th><th>流量</th><th>启用</th></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id} className="clickable" onClick={() => setDrawer(r)}>
                  <td style={{ fontWeight: 500 }}>{r.name}{r.remark && <div className="small mute">{r.remark}</div>}</td>
                  <td>{r.nodeName || '—'}</td>
                  <td><span className={r.engine === 'gost' ? 'tag singbox' : 'tag'}>{r.engine === 'gost' ? 'GOST' : 'realm'}</span></td>
                  <td className="mono">{r.listenIp ? r.listenIp + ':' : ':'}{r.listenPort}</td>
                  <td>{netLabel[r.network] ?? r.network}</td>
                  <td className="mono small">{r.remotes.map((m) => (
                    <div key={m.addr} style={{ opacity: m.enabled ? 1 : 0.45 }}>
                      <span className="dot" style={{ background: !m.enabled ? 'var(--input-strong)' : r.health?.[m.addr] ? 'var(--bad)' : 'var(--ok)' }} />{m.addr}{r.balance && (m.weight ?? 1) > 1 ? ` ×${m.weight}` : ''}
                    </div>
                  ))}</td>
                  <td>{r.remotes.length > 1 ? <>{balLabel[r.balance] ?? r.balance}{r.failover && <span className="tag" style={{ marginLeft: 4 }}>故障转移</span>}</> : '—'}</td>
                  <td>{r.tunnelRole ? <span className="tag">{r.tunnelType.toUpperCase()} · {r.tunnelRole === 'client' ? '发送' : '接收'}</span> : null}
                    {r.sendProxy > 0 && <span className="tag">PROXY v{r.sendProxy}</span>}
                    {(r.rateIn > 0 || r.rateOut > 0) && <span className="tag">限速 {r.rateIn || '∞'}/{r.rateOut || '∞'} KB/s</span>}
                    {r.maxConns > 0 && <span className="tag">≤{r.maxConns} 连接</span>}
                    {!r.tunnelRole && !r.sendProxy && !r.rateIn && !r.rateOut && !r.maxConns && '—'}</td>
                  <td className="mono small">{r.engine === 'gost' ? (() => { const t = traffic(r); return t ? <>↑ {bytes(t.up)}<br />↓ {bytes(t.down)}</> : '0 B' })() : <span className="mute" title="realm 不提供流量统计，改用 GOST 引擎可统计">—</span>}</td>
                  <td onClick={(e) => e.stopPropagation()}><Switch label="启用" on={r.enabled} onChange={() => toggle(r)} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {drawer && <RuleDrawer rule={drawer === 'new' ? undefined : drawer} onClose={() => setDrawer(null)} onSaved={() => { setDrawer(null); list.reload() }} />}
    </>
  )
}
