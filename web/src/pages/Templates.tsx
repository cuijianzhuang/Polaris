import { useEffect, useMemo, useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import InboundFields, { type InboundDraft } from '../components/InboundFields'
import { Drawer, Empty, Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { coreLabel, coreShort, coreTagClass, protoLabel, transLabel } from '../format'
import type { CapSet, CDNProfile, Group, InboundTemplate, NodePlan, TemplateItem } from '../types'

type Step = 1 | 2 | 3

function itemTag(it: TemplateItem) {
  const p = protoLabel[it.protocol.type]
  if (it.security.type === 'reality') return `${p}-REALITY`
  if (it.transport.type !== 'tcp' && it.transport.type !== 'quic') return `${p}-${transLabel[it.transport.type]}`
  return p
}

// Collapses items rendering the same tag into one chip with a count.
function groupItems(items: TemplateItem[]) {
  const out: { it: TemplateItem; n: number; tag: string }[] = []
  for (const it of items) {
    const tag = itemTag(it) + '|' + it.core
    const g = out.find((x) => x.tag === tag)
    if (g) g.n++
    else out.push({ it, n: 1, tag })
  }
  return out
}

function portText(it: TemplateItem) {
  return it.port.mode === 'fixed' ? `固定 ${it.port.fixed}` : `${it.port.min ?? 20000}–${it.port.max ?? 30000} 随机`
}

const certText: Record<string, string> = { acme: 'ACME 签 {node.domain}', reality: 'REALITY 自动生成密钥', selfsigned: '自签' }

function newItem(): TemplateItem {
  return {
    key: Math.random().toString(36).slice(2, 8), core: 'auto', protocol: { type: 'vless', flow: 'xtls-rprx-vision' }, transport: { type: 'tcp' },
    security: { type: 'reality', realityDest: 'www.microsoft.com:443' }, sniffing: { enabled: true }, port: { mode: 'fixed', fixed: 443 },
    tagPattern: '{node.name}-{protocol}', cert: 'reality', endpoints: [{ kind: 'direct' }],
  }
}

function TemplateEditor({ tpl, groups, onClose, onSaved }: { tpl?: InboundTemplate; groups: Group[]; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(tpl && !tpl.builtin ? tpl.name : tpl ? tpl.name + '（副本）' : '')
  const [desc, setDesc] = useState(tpl?.description ?? '')
  const [items, setItems] = useState<TemplateItem[]>(tpl?.items ?? [newItem()])
  const [tplGroups, setGroups] = useState<string[]>(tpl?.groups ?? [])
  const [labels, setLabels] = useState((tpl?.boundLabels ?? []).join(', '))
  const [idx, setIdx] = useState(0)
  const caps = useLoad(() => get<CapSet[]>('/capabilities'))
  const proxyCaps = (caps.data ?? []).filter((c) => c.combos?.length)
  const { busy, run } = useAction()
  const it = items[idx]
  const setIt = (patch: Partial<TemplateItem>) => setItems((x) => x.map((y, i) => (i === idx ? { ...y, ...patch } : y)))
  const draftCaps = it?.core === 'auto' ? proxyCaps : proxyCaps.filter((c) => c.core === it?.core)
  const isCopy = tpl?.builtin
  return (
    <Drawer wide title={tpl && !isCopy ? '编辑模板 · ' + tpl.name : isCopy ? '另存为自定义模板' : '新建模板'} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || !name || !items.length} onClick={() => run(async () => {
        const body = { name, description: desc, items, groups: tplGroups, boundLabels: labels.split(/[,\s]+/).filter(Boolean) }
        if (tpl && !isCopy) await put('/templates/' + tpl.id, body)
        else await post('/templates', body)
        onSaved()
      }, '模板已保存')}>保存</button>
    </>}>
      <div className="form-grid">
        <Field label="名称"><input className="input" value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label="说明"><input className="input" value={desc} onChange={(e) => setDesc(e.target.value)} /></Field>
        <Field label="生成后自动授权的分组" full>
          <div className="row">{groups.map((g) => (
            <button key={g.id} type="button" className={'chip' + (tplGroups.includes(g.id) ? ' on' : '')} onClick={() => setGroups((x) => x.includes(g.id) ? x.filter((y) => y !== g.id) : [...x, g.id])}>{g.name}</button>
          ))}</div>
        </Field>
        <Field label="绑定节点标签" full hint="带这些标签的新节点注册上线后自动套用（装好 Agent 即可用）"><input className="input" value={labels} onChange={(e) => setLabels(e.target.value)} /></Field>
      </div>
      <div className="tabs">
        {items.map((x, i) => <button key={x.key} type="button" className={'tab' + (i === idx ? ' on' : '')} onClick={() => setIdx(i)}>{itemTag(x)}</button>)}
        <button type="button" className="tab" onClick={() => { setItems((x) => [...x, newItem()]); setIdx(items.length) }}>+ 入站</button>
      </div>
      {it && (
        <>
          <InboundFields draft={{ core: it.core, protocol: it.protocol, transport: it.transport, security: it.security, sniffing: it.sniffing }}
            onChange={(d: InboundDraft) => setIt({ core: d.core, protocol: d.protocol, transport: d.transport, security: d.security, sniffing: d.sniffing })}
            caps={draftCaps} allCaps={proxyCaps}
            coreChoices={[{ core: 'auto', hint: '按节点已装内核与能力自动选' }, ...proxyCaps.map((c) => ({ core: c.core, version: c.version }))]} />
          <div className="form-grid">
            <Field label="标签模板" hint="变量：{node.name} {node.region} {node.domain} {node.ip} {protocol}">
              <input className="input mono" value={it.tagPattern} onChange={(e) => setIt({ tagPattern: e.target.value })} />
            </Field>
            <Field label="端口策略">
              <div className="row">
                <select className="input" style={{ width: 110 }} value={it.port.mode} onChange={(e) => setIt({ port: e.target.value === 'fixed' ? { mode: 'fixed', fixed: 443 } : { mode: 'range', min: 20000, max: 30000 } })}>
                  <option value="fixed">固定</option><option value="range">随机区间</option>
                </select>
                {it.port.mode === 'fixed' ? <input className="input mono" style={{ width: 100 }} value={it.port.fixed ?? ''} onChange={(e) => setIt({ port: { mode: 'fixed', fixed: Number(e.target.value) } })} />
                  : <>
                    <input className="input mono" style={{ width: 90 }} value={it.port.min ?? ''} onChange={(e) => setIt({ port: { ...it.port, min: Number(e.target.value) } })} />
                    <span>–</span>
                    <input className="input mono" style={{ width: 90 }} value={it.port.max ?? ''} onChange={(e) => setIt({ port: { ...it.port, max: Number(e.target.value) } })} />
                  </>}
              </div>
            </Field>
            <Field label="证书策略">
              <select className="input" value={it.cert} onChange={(e) => setIt({ cert: e.target.value })}>
                <option value="acme">每节点 ACME 签 {'{node.domain}'}</option><option value="selfsigned">自签</option><option value="reality">REALITY（无需证书）</option>
              </select>
            </Field>
            <Field label="入口">
              <div className="row">
                {(['direct', 'cdn'] as const).map((k) => (
                  <label key={k} className="chk"><input type="checkbox" checked={it.endpoints.some((e) => e.kind === k)}
                    onChange={(e) => setIt({ endpoints: e.target.checked ? [...it.endpoints, { kind: k }] : it.endpoints.filter((x) => x.kind !== k) })} />{k === 'direct' ? '直连' : 'CDN'}</label>
                ))}
              </div>
            </Field>
          </div>
          {items.length > 1 && <div><button className="btn sm danger" type="button" onClick={() => { setItems((x) => x.filter((_, i) => i !== idx)); setIdx(0) }}>移除此入站</button></div>}
          <div className="note">随机值与密钥（REALITY 密钥对、SS 2022 PSK、WS 路径 {'{rand.path}'}）按节点逐个生成，不会多节点共用。</div>
        </>
      )}
    </Drawer>
  )
}

function SyncModal({ tpl, onClose }: { tpl: InboundTemplate; onClose: () => void }) {
  type Change = { inboundId: string; tag: string; nodeName: string; fields: string[] | null; kept: string[] | null; error?: string }
  const diff = useLoad(() => post<{ changes: Change[] }>(`/templates/${tpl.id}/sync`, { confirm: false }), [tpl.id])
  const [overwrite, setOverwrite] = useState(false)
  const { busy, run } = useAction()
  const changes = diff.data?.changes ?? []
  const pending = changes.filter((c) => c.fields?.length)
  return (
    <Modal wide title={'同步模板 · ' + tpl.name} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy || (!pending.length && !overwrite)} onClick={() => run(async () => {
        await post(`/templates/${tpl.id}/sync`, { confirm: true, overwrite })
        onClose()
      }, '已同步并下发')}>确认同步 {pending.length} 个入站</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>手动改过的字段标记为「已覆盖」，同步时默认保留，不会被模板冲掉。</div>
      <div className="card flush scroll">
        {changes.length === 0 ? <Empty>{diff.loading ? '计算差异中…' : '所有入站已与模板一致'}</Empty> : (
          <table>
            <thead><tr><th>节点</th><th>入站</th><th>将更新字段</th><th>保留（已覆盖）</th></tr></thead>
            <tbody>{changes.map((c) => (
              <tr key={c.inboundId}><td>{c.nodeName}</td><td className="mono">{c.tag}</td>
                <td>{c.fields?.map((f) => <span key={f} className="tag warn">{f}</span>) ?? '—'}{c.error && <div className="small" style={{ color: 'var(--bad)' }}>{c.error}</div>}</td>
                <td>{c.kept?.map((f) => <span key={f} className="tag">{f}</span>) ?? '—'}</td></tr>
            ))}</tbody>
          </table>
        )}
      </div>
      <label className="chk"><input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />同时覆盖手动修改过的字段</label>
    </Modal>
  )
}

export default function Templates() {
  const { nodes } = useNodes()
  const tpls = useLoad(() => get<InboundTemplate[]>('/templates'))
  const groups = useLoad(() => get<Group[]>('/groups'))
  const cdn = useLoad(() => get<CDNProfile[]>('/cdn-profiles'))
  const [step, setStep] = useState<Step>(1)
  const [tplId, setTplId] = useState<string>('')
  const [nodeIds, setNodeIds] = useState<string[]>([])
  const [label, setLabel] = useState('')
  const [opt, setOpt] = useState({ reassign: true, autoPort: true, bind: false, cdnProfileId: '' })
  const [plan, setPlan] = useState<{ plans: NodePlan[]; summary: { nodes: number; create: number; adjusted: number; skipped: number } } | null>(null)
  const [editor, setEditor] = useState<InboundTemplate | 'new' | null>(null)
  const [sync, setSync] = useState<InboundTemplate | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const tpl = tpls.data?.find((t) => t.id === tplId)
  const labels = useMemo(() => [...new Set(nodes.flatMap((n) => n.labels ?? []))], [nodes])
  useEffect(() => { if (!tplId && tpls.data?.length) setTplId(tpls.data.find((t) => t.name === '全协议')?.id ?? tpls.data[0].id) }, [tpls.data, tplId])

  const body = () => ({ nodeIds, reassign: opt.reassign, autoPort: opt.autoPort, bindLabel: opt.bind ? label : '', cdnProfileId: opt.cdnProfileId })
  const doPreview = () => run(async () => {
    setPlan(await post(`/templates/${tplId}/preview`, body()))
    setStep(3)
  })
  const needsCDN = tpl?.items.some((i) => i.endpoints.some((e) => e.kind === 'cdn' && !e.cdnProfileId))

  return (
    <>
      <div className="top">
        <div><h1>入站模板</h1><div className="sub">一套模板 × 多个节点，密钥与随机值逐节点生成</div></div>
        <div className="steps" aria-label="步骤">
          {(['选模板', '选节点', '预览并生成'] as const).map((l, i) => {
            const n = (i + 1) as Step
            return (
              <span key={l} style={{ display: 'contents' }}>
                {i > 0 && <span className="step-sep" aria-hidden="true" />}
                <button type="button" className={'step' + (step > n ? ' done' : step === n ? ' cur' : '')} aria-current={step === n ? 'step' : undefined}
                  disabled={n === 3 && !plan} onClick={() => setStep(n)}><span className="num">{step > n ? '✓' : n}</span>{l}</button>
              </span>
            )
          })}
        </div>
      </div>

      {step === 1 && (
        <>
          <div className="grid g2">
            {(tpls.data ?? []).map((t) => (
              <article key={t.id} className="card" style={{ borderColor: t.id === tplId ? 'var(--pri)' : undefined, borderWidth: t.id === tplId ? 2 : 1, cursor: 'pointer' }} onClick={() => setTplId(t.id)}>
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <h2>{t.name}</h2>{t.builtin ? <span className="tag">内置</span> : <span className="tag xray">自定义</span>}
                </div>
                <div className="sub">{t.description || '—'} · 已生成 {t.inbounds} 个入站（{t.nodes} 个节点）</div>
                <div style={{ marginTop: 10, display: 'flex', flexWrap: 'wrap', gap: 6 }}>{groupItems(t.items).map(({ it, n }) => <span key={it.key} className={coreTagClass(it.core === 'auto' ? (it.transport.type === 'xhttp' ? 'xray' : ['hysteria2', 'tuic', 'anytls'].includes(it.protocol.type) ? 'singbox' : 'xray') : it.core)}>{itemTag(it)}{n > 1 && <b style={{ marginLeft: 4 }}>×{n}</b>}</span>)}</div>
                <div className="row" style={{ marginTop: 12 }} onClick={(e) => e.stopPropagation()}>
                  <button className="btn sm" type="button" onClick={() => setEditor(t)}>{t.builtin ? '另存为自定义' : '编辑'}</button>
                  {t.inbounds > 0 && <>
                    <button className="btn sm" type="button" onClick={() => setSync(t)}>同步</button>
                    <button className="btn sm" type="button" onClick={async () => { if (await confirm(`停用「${t.name}」生成的全部 ${t.inbounds} 个入站？`)) run(async () => { await post(`/templates/${t.id}/bulk`, { action: 'disable' }); tpls.reload() }, '已停用') }}>批量停用</button>
                    <button className="btn sm" type="button" onClick={() => run(async () => { await post(`/templates/${t.id}/bulk`, { action: 'enable' }); tpls.reload() }, '已启用')}>批量启用</button>
                    <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(`删除「${t.name}」生成的全部 ${t.inbounds} 个入站？`, { danger: true })) run(async () => { await post(`/templates/${t.id}/bulk`, { action: 'delete' }); tpls.reload() }, '已删除') }}>批量删除</button>
                  </>}
                  {!t.builtin && <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(`删除模板「${t.name}」？已生成的入站会保留。`, { danger: true })) run(async () => { await del('/templates/' + t.id); tpls.reload() }) }}>删除模板</button>}
                </div>
              </article>
            ))}
          </div>
          <div className="row">
            <button className="btn" type="button" onClick={() => setEditor('new')}>+ 新建模板</button>
            <span className="spacer" />
            <button className="btn pri" type="button" disabled={!tplId} onClick={() => setStep(2)}>下一步：选节点</button>
          </div>
        </>
      )}

      {step === 2 && (
        <>
          <div className="row">
            <span className="lbl">按标签筛选：</span>
            {labels.map((l) => <button key={l} type="button" className={'chip' + (label === l ? ' on' : '')} onClick={() => { setLabel(l); setNodeIds(nodes.filter((n) => n.labels?.includes(l)).map((n) => n.id)) }}>{l}</button>)}
            {labels.length === 0 && <span className="hint">节点还没有标签</span>}
            <span className="spacer" />
            <button className="btn sm" type="button" onClick={() => setNodeIds(nodes.map((n) => n.id))}>全选</button>
            <button className="btn sm" type="button" onClick={() => setNodeIds([])}>清空</button>
          </div>
          <div className="card flush scroll">
            <table>
              <thead><tr><th style={{ width: 32 }} /><th>节点</th><th>地区</th><th>已装内核</th><th>标签</th><th>现有入站</th></tr></thead>
              <tbody>{nodes.map((n) => (
                <tr key={n.id} className="clickable" onClick={() => setNodeIds((x) => x.includes(n.id) ? x.filter((y) => y !== n.id) : [...x, n.id])}>
                  <td><input type="checkbox" aria-label={n.name} checked={nodeIds.includes(n.id)} readOnly /></td>
                  <td style={{ fontWeight: 500 }}>{n.name}</td><td>{n.region || '—'}</td>
                  <td>{n.cores.map((c) => <span key={c.id} className={coreTagClass(c.type)}>{coreShort[c.type]}</span>)}</td>
                  <td>{(n.labels ?? []).map((l) => <span key={l} className="tag">{l}</span>)}</td>
                  <td className="mono">{n.inboundCount}</td>
                </tr>
              ))}</tbody>
            </table>
          </div>
          <div className="row">
            <button className="btn" type="button" onClick={() => setStep(1)}>上一步</button>
            <span className="spacer" />
            <button className="btn pri" type="button" disabled={!nodeIds.length || busy} onClick={doPreview}>预览 {nodeIds.length} 个节点</button>
          </div>
        </>
      )}

      {step === 3 && plan && tpl && (
        <div className="split">
          <aside className="aside" style={{ flex: '1 1 300px', maxWidth: 340 }}>
            <section className="card">
              <h2>模板 · {tpl.name}</h2>
              <div className="sub">{tpl.builtin ? '内置模板' : '自定义模板'} · 已被 {tpl.nodes} 个节点使用</div>
              <dl className="kv" style={{ gridTemplateColumns: '80px minmax(0,1fr)' }}>
                <dt>内核</dt><dd>{[...new Set(tpl.items.map((i) => i.core === 'auto' ? 'auto（按能力自动选）' : coreLabel[i.core]))].join('、')}</dd>
                <dt>入站</dt><dd>{tpl.items.map((i) => <span key={i.key} className="tag">{itemTag(i)}</span>)}</dd>
                <dt>端口</dt><dd>{tpl.items.map((i) => `${itemTag(i)} ${portText(i)}`).join('；')}</dd>
                <dt>标签</dt><dd className="mono">{tpl.items[0]?.tagPattern}</dd>
                <dt>证书</dt><dd>{[...new Set(tpl.items.map((i) => certText[i.cert] ?? i.cert))].join('、')}</dd>
                <dt>授权</dt><dd>{tpl.groups.map((g) => groups.data?.find((x) => x.id === g)?.name ?? g).join('、') || '—'}</dd>
              </dl>
            </section>
            <section className="card">
              <h2>选项</h2>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4, marginTop: 10 }}>
                <label className="chk"><input type="checkbox" checked={opt.reassign} onChange={(e) => setOpt({ ...opt, reassign: e.target.checked })} />内核不支持时改派同节点另一内核</label>
                <label className="chk"><input type="checkbox" checked={opt.autoPort} onChange={(e) => setOpt({ ...opt, autoPort: e.target.checked })} />端口冲突时自动换端口</label>
                <label className="chk"><input type="checkbox" checked={opt.bind} disabled={!label} onChange={(e) => setOpt({ ...opt, bind: e.target.checked })} />把模板绑定到节点标签{label ? `「${label}」` : '（先按标签选节点）'}</label>
                {needsCDN && (
                  <Field label="CDN 入口使用的配置档">
                    <select className="input" value={opt.cdnProfileId} onChange={(e) => setOpt({ ...opt, cdnProfileId: e.target.value })}>
                      <option value="">不添加 CDN 入口</option>{(cdn.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
                    </select>
                  </Field>
                )}
                <button className="btn sm" type="button" style={{ alignSelf: 'flex-start', marginTop: 6 }} disabled={busy} onClick={doPreview}>按选项重新预览</button>
              </div>
            </section>
          </aside>
          <div className="grow">
            <div className="grid g4">
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>节点</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.nodes}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>将创建入站</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.create}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>自动调整</span><div className="mono" style={{ fontSize: 22, color: 'var(--warn-ink)' }}>{plan.summary.adjusted}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>跳过</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.skipped}</div></div>
            </div>
            <section className="card flush scroll">
              <table>
                <thead><tr><th>入站标签</th><th>内核</th><th>端口</th><th>证书 / 密钥</th><th>入口</th><th>结果</th></tr></thead>
                <tbody>
                  {plan.plans.map((p) => (
                    <PlanRows key={p.nodeId} p={p} />
                  ))}
                </tbody>
              </table>
            </section>
            <div className="row" style={{ justifyContent: 'space-between' }}>
              <div style={{ fontSize: 13 }}>将在 <b>{plan.summary.nodes}</b> 个节点生成 <b>{plan.summary.create}</b> 个入站，各节点配置版本 +1；单个节点失败不影响其他</div>
              <div className="row">
                <button className="btn" type="button" onClick={() => setStep(2)}>上一步</button>
                <button className="btn pri" type="button" disabled={busy || !plan.summary.create} onClick={() => run(async () => {
                  const r = await post<typeof plan>(`/templates/${tplId}/apply`, body())
                  setPlan(r)
                  tpls.reload()
                }, '已生成并下发')}>生成并下发</button>
              </div>
            </div>
          </div>
        </div>
      )}
      {editor && groups.data && <TemplateEditor tpl={editor === 'new' ? undefined : editor} groups={groups.data} onClose={() => setEditor(null)} onSaved={() => { setEditor(null); tpls.reload() }} />}
      {sync && <SyncModal tpl={sync} onClose={() => { setSync(null); tpls.reload() }} />}
    </>
  )
}

function PlanRows({ p }: { p: NodePlan }) {
  return (
    <>
      <tr className="group"><td colSpan={6}>{p.nodeName} · {p.summary}{p.error && <span style={{ color: 'var(--bad)' }}> · 写入失败：{p.error}</span>}</td></tr>
      {p.items.map((it) => (
        <tr key={it.itemKey}>
          <td className="mono">{it.tag}</td>
          <td>{it.core ? <span className={coreTagClass(it.core)}>{coreShort[it.core]}</span> : '—'}</td>
          <td className="mono">{it.port ? `${it.port}${it.network === 'udp' ? '/udp' : ''}` : '—'}</td>
          <td>{it.cert || '—'}</td>
          <td>{it.entries || '—'}</td>
          <td style={{ whiteSpace: 'normal' }}><span className={'st ' + (it.result === 'new' ? 'ok' : it.result === 'fix' ? 'warn' : 'off')} style={{ whiteSpace: 'normal' }}>{it.result === 'new' ? '新建' : it.result === 'fix' ? it.reason : '跳过：' + it.reason}</span></td>
        </tr>
      ))}
    </>
  )
}
