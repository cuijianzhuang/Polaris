import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { Drawer, Empty, Field, Switch, fromLines, toLines, useAction, useConfirm, useLoad } from '../components/ui'
import { ago } from '../format'
import type { DNSConfig, DNSServer, DNSSpec, Node, Outbound, RuleSet } from '../types'

// --- rule sets ---

function RuleSetDrawer({ rs, onClose, onSaved }: { rs?: RuleSet; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState({
    tag: rs?.tag ?? '', name: rs?.name ?? '', remark: rs?.remark ?? '', sourceUrl: rs?.sourceUrl ?? '',
    domain: toLines(rs?.domain), domainSuffix: toLines(rs?.domainSuffix), domainKeyword: toLines(rs?.domainKeyword), ipCidr: toLines(rs?.ipCidr),
  })
  const [remote, setRemote] = useState(!!rs?.sourceUrl)
  const { busy, run } = useAction()
  const set = (k: keyof typeof f, v: string) => setF({ ...f, [k]: v })
  const save = () => run(async () => {
    const body = {
      tag: f.tag, name: f.name, remark: f.remark, sourceUrl: remote ? f.sourceUrl : '',
      domain: fromLines(f.domain), domainSuffix: fromLines(f.domainSuffix), domainKeyword: fromLines(f.domainKeyword), ipCidr: fromLines(f.ipCidr),
    }
    if (rs) await put(`/rulesets/${rs.id}`, body)
    else await post('/rulesets', body)
    onSaved()
  }, '规则集已保存，正在下发到节点')
  return (
    <Drawer title={rs ? `编辑规则集 ${rs.tag}` : '新建规则集'} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !f.tag || (remote && !f.sourceUrl)} onClick={save}>保存</button>
    </>}>
      <div className="form-grid">
        <Field label="标签" hint="路由规则和 DNS 里用它引用，例如 ruleset:ai"><input className="input mono" value={f.tag} onChange={(e) => set('tag', e.target.value.toLowerCase())} placeholder="ai" /></Field>
        <Field label="名称"><input className="input" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="AI 服务" /></Field>
      </div>
      <Field label="来源">
        <div className="seg">
          <button type="button" className={'opt' + (!remote ? ' on' : '')} onClick={() => setRemote(false)}>手动填写<small>在这里维护列表</small></button>
          <button type="button" className={'opt' + (remote ? ' on' : '')} onClick={() => setRemote(true)}>远程列表<small>面板每天拉取一次，再下发给所有节点</small></button>
        </div>
      </Field>
      {remote ? (
        <Field label="列表地址" hint="支持纯域名列表、v2fly 格式（full: / domain: / keyword:）、Clash 规则（DOMAIN-SUFFIX,… / IP-CIDR,…）和 Clash 规则集 payload">
          <input className="input mono" value={f.sourceUrl} onChange={(e) => set('sourceUrl', e.target.value)} placeholder="https://example.com/list.txt" />
        </Field>
      ) : <>
        <Field label="域名后缀" hint="每行一个，匹配该域名及其子域名"><textarea className="input mono" rows={5} value={f.domainSuffix} onChange={(e) => set('domainSuffix', e.target.value)} placeholder={'openai.com\nclaude.ai'} /></Field>
        <Field label="完整域名" hint="只匹配这个域名本身"><textarea className="input mono" rows={3} value={f.domain} onChange={(e) => set('domain', e.target.value)} /></Field>
        <Field label="关键词"><textarea className="input mono" rows={2} value={f.domainKeyword} onChange={(e) => set('domainKeyword', e.target.value)} /></Field>
        <Field label="IP 段" hint="CIDR 或单个 IP"><textarea className="input mono" rows={3} value={f.ipCidr} onChange={(e) => set('ipCidr', e.target.value)} placeholder="1.1.1.0/24" /></Field>
      </>}
      <Field label="备注"><input className="input" value={f.remark} onChange={(e) => set('remark', e.target.value)} /></Field>
    </Drawer>
  )
}

export function RuleSets() {
  const list = useLoad(() => get<RuleSet[]>('/rulesets'))
  const [drawer, setDrawer] = useState<RuleSet | 'new' | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  return (
    <section className="card flush">
      <div className="row" style={{ padding: '12px 16px 4px', justifyContent: 'space-between' }}>
        <div><h2>规则集</h2><div className="sub" style={{ margin: 0 }}>在面板统一维护的域名 / IP 列表。路由规则的「规则集」条件和 DNS 分流（ruleset:标签）都可以引用，修改后自动下发到所有节点。</div></div>
        <button className="btn pri" type="button" onClick={() => setDrawer('new')}>+ 规则集</button>
      </div>
      {list.data?.length === 0 ? <Empty>还没有规则集</Empty> : (
        <div className="scroll">
          <table>
            <thead><tr><th>标签</th><th>名称</th><th>条目</th><th>来源</th><th>使用位置</th><th></th></tr></thead>
            <tbody>
              {(list.data ?? []).map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.tag}</td>
                  <td>{r.name || '—'}{r.remark && <div className="small mute">{r.remark}</div>}</td>
                  <td className="mono">{r.count}</td>
                  <td style={{ maxWidth: 360 }}>{r.sourceUrl ? <>
                    <div className="mono small" style={{ wordBreak: 'break-all' }}>{r.sourceUrl}</div>
                    <div className="small mute" style={r.fetchError ? { color: 'var(--bad)', wordBreak: 'break-word' } : undefined}>{r.fetchError ? `拉取失败：${r.fetchError}` : r.fetchedAt ? `${ago(r.fetchedAt)}更新` : '尚未拉取'}</div>
                  </> : <span className="mute">手动</span>}</td>
                  <td>{r.used?.length ? r.used.map((u) => <span key={u} className="tag">{u}</span>) : <span className="mute">未使用</span>}</td>
                  <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    {r.sourceUrl && <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/rulesets/${r.id}/refresh`); list.reload() }, '已重新拉取')}>立即更新</button>}
                    <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setDrawer(r)}>编辑</button>
                    <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                      if (await confirm(`删除规则集 ${r.tag}？`, { danger: true })) run(async () => { await del(`/rulesets/${r.id}`); list.reload() })
                    }}>删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {drawer && <RuleSetDrawer rs={drawer === 'new' ? undefined : drawer} onClose={() => setDrawer(null)} onSaved={() => { setDrawer(null); list.reload() }} />}
    </section>
  )
}

// --- DNS ---

const serverPresets: { label: string; s: DNSServer }[] = [
  { label: 'Cloudflare DoH', s: { tag: 'cloudflare', address: 'https://1.1.1.1/dns-query' } },
  { label: 'Google DoH', s: { tag: 'google', address: 'https://8.8.8.8/dns-query' } },
  { label: '阿里 DNS（国内域名）', s: { tag: 'alidns', address: '223.5.5.5', domains: ['geosite:cn'] } },
  { label: '系统 DNS', s: { tag: 'system', address: 'local' } },
]

const emptySpec = (): DNSSpec => ({ servers: [{ tag: 'cloudflare', address: 'https://1.1.1.1/dns-query' }] })

function hostsText(h?: Record<string, string[]>) {
  return Object.entries(h ?? {}).map(([k, v]) => `${k} ${v.join(' ')}`).join('\n')
}
function parseHosts(s: string): Record<string, string[]> | undefined {
  const out: Record<string, string[]> = {}
  for (const line of s.split('\n')) {
    const [name, ...ips] = line.trim().split(/\s+/)
    if (name && ips.length) out[name] = ips
  }
  return Object.keys(out).length ? out : undefined
}

function SpecEditor({ spec, onChange, tags }: { spec: DNSSpec; onChange: (s: DNSSpec) => void; tags: string[] }) {
  const servers = spec.servers ?? []
  const setServer = (i: number, s: DNSServer) => onChange({ ...spec, servers: servers.map((x, j) => (j === i ? s : x)) })
  const [hosts, setHosts] = useState(hostsText(spec.hosts))
  return <>
    <Field label="DNS 服务器" hint={<>按顺序匹配：填了「域名」的只解析这些域名（分流），第一个不填域名的作为默认。域名写法：geosite:cn、full:a.com、domain:a.com（含子域名）、keyword:a、ruleset:标签。地址写法：8.8.8.8、tcp://…、tls://…（DoT，Xray 不支持）、https://…/dns-query（DoH）、quic://…、local（系统）。</>}>
      <div className="dns-servers">
        <div className="dns-row lbl" aria-hidden="true"><span>标签</span><span>地址</span><span>域名（留空 = 默认）</span><span>经由出站</span><span /></div>
        {servers.map((s, i) => (
          <div key={i} className="dns-row">
            <input className="input mono" aria-label="标签" value={s.tag} onChange={(e) => setServer(i, { ...s, tag: e.target.value })} />
            <input className="input mono" aria-label="地址" value={s.address} onChange={(e) => setServer(i, { ...s, address: e.target.value })} placeholder="https://1.1.1.1/dns-query" />
            <input className="input mono" aria-label="域名" value={(s.domains ?? []).join(', ')} onChange={(e) => setServer(i, { ...s, domains: fromLines(e.target.value) })} placeholder="geosite:cn, ruleset:ai" />
            <select className="input" aria-label="经由出站" value={s.detour ?? ''} onChange={(e) => setServer(i, { ...s, detour: e.target.value || undefined })}>
              <option value="">直接查询</option>{tags.map((t) => <option key={t} value={t}>{t}</option>)}
            </select>
            <button className="btn sm" type="button" aria-label={`删除 ${s.tag}`} disabled={servers.length < 2} onClick={() => onChange({ ...spec, servers: servers.filter((_, j) => j !== i) })}>删除</button>
          </div>
        ))}
        <div className="row">
          <button className="btn sm" type="button" onClick={() => onChange({ ...spec, servers: [...servers, { tag: `dns${servers.length + 1}`, address: '' }] })}>+ 服务器</button>
          {serverPresets.map((p) => (
            <button key={p.label} className="chip" type="button" disabled={servers.some((s) => s.tag === p.s.tag)} onClick={() => onChange({ ...spec, servers: [...servers, p.s] })}>{p.label}</button>
          ))}
        </div>
      </div>
    </Field>
    <div className="form-grid">
      <Field label="解析策略">
        <select className="input" value={spec.strategy ?? ''} onChange={(e) => onChange({ ...spec, strategy: e.target.value || undefined })}>
          <option value="">内核默认</option><option value="prefer_ipv4">优先 IPv4</option><option value="prefer_ipv6">优先 IPv6</option>
          <option value="ipv4_only">只用 IPv4</option><option value="ipv6_only">只用 IPv6</option>
        </select>
      </Field>
      <Field label="路由域名策略" hint="IP 类规则（geoip、IP 段）是否先解析域名再匹配">
        <select className="input" value={spec.domainStrategy ?? ''} onChange={(e) => onChange({ ...spec, domainStrategy: e.target.value || undefined })}>
          <option value="">不解析（AsIs，默认）</option><option value="IPIfNonMatch">域名规则都不匹配时解析（IPIfNonMatch）</option><option value="IPOnDemand">遇到 IP 规则就解析（IPOnDemand）</option>
        </select>
      </Field>
    </div>
    <Field label="Hosts" hint="每行：域名 IP [IP…]">
      <textarea className="input mono" rows={3} value={hosts} onChange={(e) => { setHosts(e.target.value); onChange({ ...spec, hosts: parseHosts(e.target.value) }) }} placeholder="panel.internal 10.0.0.2" />
    </Field>
    <Field label="FakeIP" hint="只对经由内核 DNS 入口发起的查询生效，一般只在节点作网关 / 透明代理时需要；普通代理节点保持关闭">
      <div className="row"><Switch on={!!spec.fakeip} label="FakeIP" onChange={(v) => onChange({ ...spec, fakeip: v ? {} : undefined })} /><span className="small mute">{spec.fakeip ? '已开启' : '关闭'}</span></div>
    </Field>
    {spec.fakeip && (
      <div className="form-grid">
        <Field label="IPv4 地址段"><input className="input mono" value={spec.fakeip.inet4Range ?? ''} placeholder="198.18.0.0/15" onChange={(e) => onChange({ ...spec, fakeip: { ...spec.fakeip, inet4Range: e.target.value || undefined } })} /></Field>
        <Field label="IPv6 地址段"><input className="input mono" value={spec.fakeip.inet6Range ?? ''} placeholder="fc00::/18" onChange={(e) => onChange({ ...spec, fakeip: { ...spec.fakeip, inet6Range: e.target.value || undefined } })} /></Field>
        <Field label="只对这些域名（留空 = 全部）" full><input className="input mono" value={(spec.fakeip.domains ?? []).join(', ')} onChange={(e) => onChange({ ...spec, fakeip: { ...spec.fakeip, domains: fromLines(e.target.value) } })} placeholder="geosite:netflix" /></Field>
      </div>
    )}
  </>
}

export function DNSSettings({ nodes }: { nodes: Node[] }) {
  const cfg = useLoad(() => get<{ global: DNSConfig; nodes: DNSConfig[] }>('/dns'))
  const [scope, setScope] = useState('*')
  const global = scope === '*'
  const outs = useLoad(() => (global
    ? Promise.all(nodes.map((n) => get<Outbound[]>(`/nodes/${n.id}/outbounds`))).then((ls) => [...new Set(ls.flat().map((o) => o.tag))])
    : get<Outbound[]>(`/nodes/${scope}/outbounds`).then((ls) => ls.map((o) => o.tag))), [scope, nodes.length])
  const current = global ? cfg.data?.global : cfg.data?.nodes.find((n) => n.scope === scope)
  const [mode, setMode] = useState('off')
  const [spec, setSpec] = useState<DNSSpec>(emptySpec())
  const [ver, setVer] = useState(0) // remounts the editor when the loaded config changes
  useEffect(() => {
    if (!cfg.data) return
    const g = cfg.data.global
    if (global) {
      setMode(g.mode || 'off')
      setSpec(g.spec?.servers?.length ? g.spec : emptySpec())
    } else {
      setMode(current?.mode || 'inherit')
      setSpec(current?.spec?.servers?.length ? current.spec : g.spec?.servers?.length ? g.spec : emptySpec())
    }
    setVer((v) => v + 1)
  }, [cfg.data, scope])
  const { busy, run } = useAction()
  const save = () => run(async () => { await put(`/dns/${scope}`, { mode, spec }); cfg.reload() }, 'DNS 设置已保存并下发')
  const editing = global ? mode === 'on' : mode === 'custom'
  const tags = ['direct', ...(outs.data ?? [])]
  return (
    <section className="card">
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <div><h2>内核 DNS</h2><div className="sub" style={{ margin: 0 }}>统一配置节点上 Xray / sing-box / mihomo 的 DNS：DoH / DoT、按域名分流、FakeIP。关闭时内核使用系统解析。</div></div>
        <select className="input" style={{ width: 'auto', minWidth: 200 }} value={scope} onChange={(e) => setScope(e.target.value)} aria-label="范围">
          <option value="*">全局（所有节点）</option>
          {nodes.map((n) => {
            const o = cfg.data?.nodes.find((x) => x.scope === n.id)
            return <option key={n.id} value={n.id}>{n.name}{o ? (o.mode === 'off' ? '（关闭）' : '（单独配置）') : ''}</option>
          })}
        </select>
      </div>
      <div style={{ marginTop: 12 }}>
        {global ? (
          <div className="row"><Switch on={mode === 'on'} label="启用内核 DNS" onChange={(v) => setMode(v ? 'on' : 'off')} /><span>{mode === 'on' ? '已启用，下发到所有未单独配置的节点' : '未启用（使用系统解析）'}</span></div>
        ) : (
          <div className="seg">
            {([['inherit', '跟随全局', cfg.data?.global.mode === 'on' ? '使用全局 DNS 配置' : '全局未启用：系统解析'], ['custom', '单独配置', '只对这个节点生效'], ['off', '关闭', '这个节点用系统解析']] as const).map(([k, l, d]) => (
              <button key={k} type="button" className={'opt' + (mode === k ? ' on' : '')} onClick={() => setMode(k)}>{l}<small>{d}</small></button>
            ))}
          </div>
        )}
      </div>
      {editing && <div style={{ marginTop: 16 }}><SpecEditor key={`${scope}-${ver}`} spec={spec} onChange={setSpec} tags={tags} /></div>}
      <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end' }}>
        <button className="btn pri" type="button" disabled={busy || !cfg.data} onClick={save}>保存并下发</button>
      </div>
    </section>
  )
}
