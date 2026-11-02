import OutboundIPField from '../components/OutboundIPField'
import { Fragment, useEffect, useMemo, useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import InboundFields, { type InboundDraft } from '../components/InboundFields'
import { Drawer, Empty, Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { coreLabel, coreShort, coreTagClass, protoLabel, transLabel } from '../format'
import type { CapSet, CDNProfile, Certificate, Group, InboundTemplate, NodePlan, TemplateItem, TemplatePlan, TemplateRealityCheck } from '../types'
import { t, tr } from '../i18n'

type Step = 1 | 2 | 3

function itemTag(it: TemplateItem) {
  const p = protoLabel[it.protocol.type]
  if (it.security.type === 'reality') return `${p}-REALITY`
  if (it.transport.type !== 'tcp' && it.transport.type !== 'quic') return `${p}-${transLabel[it.transport.type]}`
  return p
}

function itemName(it: TemplateItem) { return it.name || itemTag(it) }
function itemKey() { return Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, '0')).join('') }

// Collapses items rendering the same tag into one chip with a count.
function groupItems(items: TemplateItem[]) {
  const out: { it: TemplateItem; n: number; tag: string }[] = []
  for (const it of items) {
    const tag = itemName(it) + '|' + it.core + '|' + (it.protocol.flow ?? '') + '|' + !!it.protocol.visionUDP443
    const g = out.find((x) => x.tag === tag)
    if (g) g.n++
    else out.push({ it, n: 1, tag })
  }
  return out
}

function portText(it: TemplateItem) {
  return it.port.mode === 'fixed' ? t('固定 {port}', { port: it.port.fixed }) : t('{min}–{max} 随机', { min: it.port.min ?? 20000, max: it.port.max ?? 30000 })
}

const certText: Record<string, string> = { acme: t('ACME 签 {node.domain}'), reality: t('REALITY 自动生成密钥'), selfsigned: t('自签') }

function newItem(): TemplateItem {
  return {
    key: itemKey(), core: 'auto', protocol: { type: 'vless', flow: 'xtls-rprx-vision' }, transport: { type: 'tcp' },
    security: { type: 'reality', realityDest: 'www.microsoft.com:443' }, sniffing: { enabled: true }, port: { mode: 'fixed', fixed: 443 },
    tagPattern: '{node.name}-{protocol}', cert: 'reality', endpoints: [{ kind: 'direct' }],
  }
}

function TemplateEditor({ tpl, groups, onClose, onSaved }: { tpl?: InboundTemplate; groups: Group[]; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(tpl && !tpl.builtin ? tpl.name : tpl ? t('{name}（副本）', { name: tpl.name }) : '')
  const [desc, setDesc] = useState(tpl?.description ?? '')
  const [items, setItems] = useState<TemplateItem[]>(tpl?.items ?? [newItem()])
  const [tplGroups, setGroups] = useState<string[]>(tpl?.groups ?? [])
  const [labels, setLabels] = useState((tpl?.boundLabels ?? []).join(', '))
  const [idx, setIdx] = useState(0)
  const caps = useLoad(() => get<CapSet[]>('/capabilities'))
  const certificates = useLoad(() => get<Certificate[]>('/certificates'))
  const proxyCaps = (caps.data ?? []).filter((c) => c.combos?.length)
  const { busy, run } = useAction()
  const it = items[idx]
  const setIt = (patch: Partial<TemplateItem>) => setItems((x) => x.map((y, i) => (i === idx ? { ...y, ...patch } : y)))
  const draftCaps = it?.core === 'auto' ? proxyCaps : proxyCaps.filter((c) => c.core === it?.core)
  const isCopy = tpl?.builtin
  return (
    <Drawer wide title={tpl && !isCopy ? t('编辑模板 · {name}', { name: tpl.name }) : isCopy ? t('另存为自定义模板') : t('新建模板')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !name || !items.length} onClick={() => run(async () => {
        const body = { name, description: desc, items, groups: tplGroups, boundLabels: labels.split(/[,\s]+/).filter(Boolean) }
        if (tpl && !isCopy) await put('/templates/' + tpl.id, body)
        else await post('/templates', body)
        onSaved()
      }, t('模板已保存'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('名称')}><input className="input" value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label={t('说明')}><input className="input" value={desc} onChange={(e) => setDesc(e.target.value)} /></Field>
        <Field label={t('生成后自动授权的分组')} full>
          <div className="row">{groups.map((g) => (
            <button key={g.id} type="button" className={'chip' + (tplGroups.includes(g.id) ? ' on' : '')} onClick={() => setGroups((x) => x.includes(g.id) ? x.filter((y) => y !== g.id) : [...x, g.id])}>{tr(g.name)}</button>
          ))}</div>
        </Field>
        <Field label={t('绑定节点标签')} full hint={t('带这些标签的新节点注册上线后自动套用（装好 Agent 即可用）')}><input className="input" value={labels} onChange={(e) => setLabels(e.target.value)} /></Field>
      </div>
      <div className="tabs">
        {items.map((x, i) => <button key={x.key} type="button" className={'tab' + (i === idx ? ' on' : '')} onClick={() => setIdx(i)}>{itemName(x)}</button>)}
        <button type="button" className="tab" onClick={() => { setItems((x) => [...x, newItem()]); setIdx(items.length) }}>{t('+ 入站')}</button>
      </div>
      {it && (
        <>
          <div className="row">
            <button className="btn sm" disabled={items.length >= 100} onClick={() => {
              const copy = structuredClone(it), key = itemKey()
              copy.key = key; copy.name = `${itemName(it).slice(0, 50)} ${t('副本')}`; copy.tagPattern = `${it.tagPattern}-${key.slice(0, 4)}`
              setItems((old) => [...old.slice(0, idx+1), copy, ...old.slice(idx+1)]); setIdx(idx+1)
            }}>{t('复制此入站')}</button>
            <button className="btn sm" disabled={idx === 0} onClick={() => { const next = [...items]; [next[idx-1], next[idx]] = [next[idx], next[idx-1]]; setItems(next); setIdx(idx-1) }}>{t('上移')}</button>
            <button className="btn sm" disabled={idx === items.length-1} onClick={() => { const next = [...items]; [next[idx+1], next[idx]] = [next[idx], next[idx+1]]; setItems(next); setIdx(idx+1) }}>{t('下移')}</button>
          </div>
          <Field label={t('入站名称（可选）')} htmlFor="template-item-name"><input id="template-item-name" className="input" maxLength={64} value={it.name ?? ''} placeholder={itemTag(it)} onChange={(e) => setIt({ name: e.target.value })} /></Field>
          <OutboundIPField value={it.outboundIPStrategy} onChange={(outboundIPStrategy) => setIt({ outboundIPStrategy })} />
          <div className="note">{itemTag(it)} · {portText(it)} · {it.protocol.flow || t('无流控')}{it.protocol.visionUDP443 && ' · UDP 443'}</div>
          <InboundFields draft={{ core: it.core, protocol: it.protocol, transport: it.transport, security: it.security, sniffing: it.sniffing }}
            onChange={(d: InboundDraft) => setIt({ core: d.core, protocol: d.protocol, transport: d.transport, security: d.security, sniffing: d.sniffing,
              cert: d.security.type === 'reality' ? 'reality' : d.security.type === 'tls' ? (it.cert === 'reality' || it.cert === 'none' ? 'acme' : it.cert) : 'none' })}
            caps={draftCaps} allCaps={proxyCaps}
            coreChoices={[{ core: 'auto', hint: t('按节点已装内核与能力自动选') }, ...proxyCaps.map((c) => ({ core: c.core, version: c.version }))]} />
          <div className="form-grid">
            <Field label={t('标签模板')} hint={t('变量：{node.name} {node.region} {node.domain} {node.ip} {protocol}')}>
              <input className="input mono" value={it.tagPattern} onChange={(e) => setIt({ tagPattern: e.target.value })} />
            </Field>
            <Field label={t('端口策略')}>
              <div className="row">
                <select className="input" style={{ width: 110 }} value={it.port.mode} onChange={(e) => setIt({ port: e.target.value === 'fixed' ? { mode: 'fixed', fixed: 443 } : { mode: 'range', min: 20000, max: 30000 } })}>
                  <option value="fixed">{t('固定')}</option><option value="range">{t('随机区间')}</option>
                </select>
                {it.port.mode === 'fixed' ? <input className="input mono" style={{ width: 100 }} value={it.port.fixed ?? ''} onChange={(e) => setIt({ port: { mode: 'fixed', fixed: Number(e.target.value) } })} />
                  : <>
                    <input className="input mono" style={{ width: 90 }} value={it.port.min ?? ''} onChange={(e) => setIt({ port: { ...it.port, min: Number(e.target.value) } })} />
                    <span>–</span>
                    <input className="input mono" style={{ width: 90 }} value={it.port.max ?? ''} onChange={(e) => setIt({ port: { ...it.port, max: Number(e.target.value) } })} />
                  </>}
              </div>
            </Field>
            <Field label={t('证书策略')}>
              <select className="input" value={it.cert} onChange={(e) => setIt({ cert: e.target.value })}>
                {it.security.type === 'tls' ? <><option value="acme">{t('每节点 ACME 签 {node.domain}')}</option><option value="selfsigned">{t('自签')}</option>
                  {(certificates.data ?? []).map((c) => <option key={c.id} value={'wildcard:'+c.id}>{t('已有证书：{domain}', { domain: c.domain })}</option>)}
                </> : it.security.type === 'reality' ? <option value="reality">{t('REALITY（无需证书）')}</option> : <option value="none">{t('无需证书')}</option>}
              </select>
            </Field>
            <Field label={t('入口')}>
              <div className="row">
                {(['direct', 'cdn'] as const).map((k) => (
                  <label key={k} className="chk"><input type="checkbox" checked={it.endpoints.some((e) => e.kind === k)}
                    onChange={(e) => setIt({ endpoints: e.target.checked ? [...it.endpoints, { kind: k }] : it.endpoints.filter((x) => x.kind !== k) })} />{k === 'direct' ? t('直连') : 'CDN'}</label>
                ))}
              </div>
            </Field>
          </div>
          {items.length > 1 && <div><button className="btn sm danger" type="button" onClick={() => { setItems((x) => x.filter((_, i) => i !== idx)); setIdx(0) }}>{t('移除此入站')}</button></div>}
          <div className="note">{t('随机值与密钥（REALITY 密钥对、SS 2022 PSK、WS 路径 {rand.path}）按节点逐个生成，不会多节点共用。')}</div>
        </>
      )}
    </Drawer>
  )
}

function SyncModal({ tpl, onClose }: { tpl: InboundTemplate; onClose: () => void }) {
  type Detail = { field: string; before: unknown; after: unknown; kept: boolean }
  type Change = { inboundId: string; tag: string; nodeName: string; fields: string[] | null; kept: string[] | null; error?: string; details: Detail[] | null; hash: string }
  type Diff = { changes: Change[]; templateHash: string }
  const diff = useLoad(() => post<Diff>(`/templates/${tpl.id}/sync`, { confirm: false }), [tpl.id])
  const [overwrite, setOverwrite] = useState(false)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [failures, setFailures] = useState<Record<string, string>>({})
  const { busy, run } = useAction()
  const changes = diff.data?.changes ?? []
  const key = (id: string, field: string) => `${id}|${field}`
  useEffect(() => {
    setSelected(new Set(changes.filter((c) => !c.error).flatMap((c) => (c.details ?? []).filter((d) => !d.kept || overwrite).map((d) => key(c.inboundId, d.field)))))
  }, [diff.data, overwrite])
  const pending = changes.filter((c) => (c.details ?? []).some((d) => selected.has(key(c.inboundId, d.field))))
  const valueText = (value: unknown) => value == null ? '—' : typeof value === 'boolean' ? (value ? t('启用') : t('停用')) : typeof value === 'object' ? JSON.stringify(value) : tr(String(value))
  return <Modal wide title={t('同步模板 · {name}', { name: tpl.name })} onClose={onClose} footer={<>
    <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
    <button className="btn pri" type="button" disabled={busy || !pending.length} onClick={() => run(async () => {
      const selections = Object.fromEntries(pending.map((c) => [c.inboundId, (c.details ?? []).filter((d) => selected.has(key(c.inboundId, d.field))).map((d) => d.field)]))
      const expected = Object.fromEntries(pending.map((c) => [c.inboundId, c.hash]))
      const result = await post<Diff>(`/templates/${tpl.id}/sync`, { confirm: true, overwrite, selections, expected, templateHash: diff.data?.templateHash })
      const failed = result.changes.filter((c) => c.error)
      if (failed.length) { setFailures(Object.fromEntries(failed.map((c) => [c.inboundId, c.error!]))); throw new Error(t('部分入站同步失败，请查看具体原因')) }
      onClose()
    }, t('同步配置已保存，等待节点应用'))}>{t('确认同步 {n} 个入站', { n: pending.length })}</button>
  </>}>
    <div className="sub" style={{ margin: 0 }}>{t('逐字段选择同步内容；手动修改只保护对应字段，密钥内容不会展示。')}</div>
    <label className="chk"><input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />{t('允许选择手动修改过的字段')}</label>
    <div className="card flush scroll">
      {changes.length === 0 ? <Empty>{diff.loading ? t('计算差异中…') : t('所有入站已与模板一致')}</Empty> : <table>
        <thead><tr><th>{t('节点 / 入站')}</th><th>{t('字段')}</th><th>{t('修改前')}</th><th>{t('修改后')}</th></tr></thead>
        <tbody>{changes.map((c) => <Fragment key={c.inboundId}>
          <tr className="group"><td colSpan={4}>{c.nodeName} · {c.tag}{(c.error || failures[c.inboundId]) && <div className="small" style={{ color: 'var(--bad)' }}>{c.error || failures[c.inboundId]}</div>}</td></tr>
          {(c.details ?? []).map((d) => <tr key={d.field}>
            <td><input type="checkbox" aria-label={`${c.tag} ${d.field}`} disabled={!!c.error || (d.kept && !overwrite)} checked={selected.has(key(c.inboundId, d.field))} onChange={(e) => setSelected((old) => { const next = new Set(old); e.target.checked ? next.add(key(c.inboundId, d.field)) : next.delete(key(c.inboundId, d.field)); return next })} /></td>
            <td className="mono">{d.field}{d.kept && <span className="tag">{t('手动修改')}</span>}</td>
            <td style={{ whiteSpace: 'normal', overflowWrap: 'anywhere', maxWidth: 240 }}>{valueText(d.before)}</td>
            <td style={{ whiteSpace: 'normal', overflowWrap: 'anywhere', maxWidth: 240 }}>{valueText(d.after)}</td>
          </tr>)}
        </Fragment>)}</tbody>
      </table>}
    </div>
  </Modal>
}

export default function Templates() {
  const { nodes } = useNodes()
  const tpls = useLoad(() => get<InboundTemplate[]>('/templates'))
  const groups = useLoad(() => get<Group[]>('/groups'))
  const cdn = useLoad(() => get<CDNProfile[]>('/cdn-profiles'))
  const [step, setStep] = useState<Step>(1)
  const [tplId, setTplId] = useState<string>('')
  const [query, setQuery] = useState('')
  const [scenario, setScenario] = useState('all')
  const [nodeIds, setNodeIds] = useState<string[]>([])
  const [label, setLabel] = useState('')
  const [opt, setOpt] = useState({ reassign: true, autoPort: true, bind: false, cdnProfileId: '', verifyReality: true })
  const [realityOverrides, setRealityOverrides] = useState<Record<string, Record<string, { dest: string; serverNames: string[] }>>>({})
  const [plan, setPlan] = useState<TemplatePlan | null>(null)
  const [previewInputs, setPreviewInputs] = useState('')
  const [clock, setClock] = useState(Date.now())
  const [refreshError, setRefreshError] = useState('')
  const [history, setHistory] = useState<InboundTemplate | null>(null)
  const [editor, setEditor] = useState<InboundTemplate | 'new' | null>(null)
  const [sync, setSync] = useState<InboundTemplate | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const tpl = tpls.data?.find((t) => t.id === tplId)
  const labels = useMemo(() => [...new Set(nodes.flatMap((n) => n.labels ?? []))], [nodes])
  const visibleTemplates = (tpls.data ?? []).filter((tpl) => {
    const text = `${tpl.name} ${tpl.description} ${tpl.items.map(itemName).join(' ')}`.toLowerCase()
    return text.includes(query.trim().toLowerCase()) && (scenario === 'all' || scenario === 'custom' && !tpl.builtin || scenario === 'reality' && tpl.items.some((i) => i.security.type === 'reality') || scenario === 'cdn' && tpl.items.some((i) => i.endpoints.some((e) => e.kind === 'cdn')) || scenario === 'quic' && tpl.items.some((i) => ['hysteria2', 'tuic'].includes(i.protocol.type)))
  })
  useEffect(() => { if (!visibleTemplates.some((tpl) => tpl.id === tplId)) setTplId((scenario === 'all' && !query ? visibleTemplates.find((tpl) => tpl.name === '\u5168\u534f\u8bae') : undefined)?.id ?? visibleTemplates[0]?.id ?? '') }, [tpls.data, tplId, scenario, query])

  useEffect(() => { setRealityOverrides({}) }, [tplId])
  const body = () => ({ nodeIds, reassign: opt.reassign, autoPort: opt.autoPort, bindLabel: opt.bind ? label : '', cdnProfileId: opt.cdnProfileId, verifyReality: opt.verifyReality,
    realityOverrides: Object.fromEntries(Object.entries(realityOverrides).filter(([id]) => nodeIds.includes(id))) })
  const currentInputs = JSON.stringify({ tplId, ...body() })
  const dirty = !!plan && previewInputs !== currentInputs
  const expired = !!plan && !plan.state && clock >= Date.parse(plan.expiresAt)
  const doPreview = () => run(async () => {
    const inputs = currentInputs
    setPlan(await post<TemplatePlan>(`/templates/${tplId}/preview`, body()))
    setPreviewInputs(inputs)
    setClock(Date.now())
    setRefreshError('')
    setStep(3)
  })
  useEffect(() => {
    const timer = window.setInterval(() => setClock(Date.now()), 15000)
    return () => window.clearInterval(timer)
  }, [])
  useEffect(() => {
    if (!plan?.state || !plan.planId) return
    let active = true, pending = false
    const poll = async () => {
      if (pending) return
      pending = true
      try {
        const updated = await get<TemplatePlan>(`/templates/${tplId}/runs/${plan.planId}`)
        if (active) { setPlan(updated); setRefreshError('') }
      } catch (e) { if (active) setRefreshError((e as Error).message) }
      finally { pending = false }
    }
    const timer = window.setInterval(poll, 4000)
    return () => { active = false; window.clearInterval(timer) }
  }, [tplId, plan?.planId, plan?.state])
  const needsCDN = tpl?.items.some((i) => i.endpoints.some((e) => e.kind === 'cdn' && !e.cdnProfileId))
  const realityItems = tpl?.items.filter((i) => i.security.type === 'reality') ?? []
  const hasReality = (p: NodePlan) => p.items.some((i) => i.result !== 'skip' && realityItems.some((item) => item.key === i.itemKey))
  const checksPassed = plan?.plans.every((p) => !hasReality(p) || plan.checks?.[p.nodeId]?.ok) ?? false

  return (
    <>
      <div className="top">
        <div><h1>{t('入站模板')}</h1><div className="sub">{t('一套模板 × 多个节点，密钥与随机值逐节点生成')}</div></div>
        <div className="steps" aria-label={t('步骤')}>
          {([t('选模板'), t('选节点'), t('预览并生成')] as const).map((l, i) => {
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
          <Field label={t('搜索模板')}><input className="input" value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t('名称、说明或协议')} /></Field>
          <div className="row">{[{ id: 'all', label: t('全部') }, { id: 'reality', label: t('REALITY 直连') }, { id: 'cdn', label: t('CDN 备用') }, { id: 'quic', label: 'Hy2 / TUIC' }, { id: 'custom', label: t('自定义') }].map((s) => <button key={s.id} className={'chip' + (scenario === s.id ? ' on' : '')} onClick={() => setScenario(s.id)}>{s.label}</button>)}</div>
          {visibleTemplates.length === 0 && <Empty>{t('没有匹配的模板')}</Empty>}
          <div className="grid g2">
            {visibleTemplates.map((tp) => (
              <article key={tp.id} className="card" style={{ borderColor: tp.id === tplId ? 'var(--pri)' : undefined, borderWidth: tp.id === tplId ? 2 : 1, cursor: 'pointer' }} onClick={() => setTplId(tp.id)}>
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <h2>{tp.builtin ? tr(tp.name) : tp.name}</h2>{tp.builtin ? <span className="tag">{t('内置')}</span> : <span className="tag xray">{t('自定义')}</span>}
                </div>
                <div className="sub">{(tp.builtin ? tr(tp.description) : tp.description) || '—'} · {t('已生成 {inbounds} 个入站（{nodes} 个节点）', { inbounds: tp.inbounds, nodes: tp.nodes })}</div>
                <div style={{ marginTop: 10, display: 'flex', flexWrap: 'wrap', gap: 6 }}>{groupItems(tp.items).map(({ it, n }) => <span key={it.key} className={coreTagClass(it.core === 'auto' ? (it.transport.type === 'xhttp' ? 'xray' : ['hysteria2', 'tuic', 'anytls'].includes(it.protocol.type) ? 'singbox' : 'xray') : it.core)}>{itemName(it)}{it.protocol.visionUDP443 && ' · UDP 443'}{n > 1 && <b style={{ marginLeft: 4 }}>×{n}</b>}</span>)}</div>
                <div className="row" style={{ marginTop: 12 }} onClick={(e) => e.stopPropagation()}>
                  <button className="btn sm" type="button" onClick={() => setEditor(tp)}>{tp.builtin ? t('另存为自定义') : t('编辑')}</button>
                  {!tp.builtin && <button className="btn sm" type="button" onClick={() => setEditor({ ...tp, builtin: true })}>{t('复制模板')}</button>}
                  <button className="btn sm" type="button" onClick={() => setHistory(tp)}>{t('应用记录')}</button>
                  {tp.inbounds > 0 && <>
                    <button className="btn sm" type="button" onClick={() => setSync(tp)}>{t('同步')}</button>
                    <button className="btn sm" type="button" onClick={async () => { if (await confirm(t('停用「{name}」生成的全部 {n} 个入站？', { name: tp.name, n: tp.inbounds }))) run(async () => { await post(`/templates/${tp.id}/bulk`, { action: 'disable' }); tpls.reload() }, t('已停用')) }}>{t('批量停用')}</button>
                    <button className="btn sm" type="button" onClick={() => run(async () => { await post(`/templates/${tp.id}/bulk`, { action: 'enable' }); tpls.reload() }, t('已启用'))}>{t('批量启用')}</button>
                    <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除「{name}」生成的全部 {n} 个入站？', { name: tp.name, n: tp.inbounds }), { danger: true })) run(async () => { await post(`/templates/${tp.id}/bulk`, { action: 'delete' }); tpls.reload() }, t('已删除')) }}>{t('批量删除')}</button>
                  </>}
                  {!tp.builtin && <button className="btn sm danger" type="button" onClick={async () => { if (await confirm(t('删除模板「{name}」？已生成的入站会保留。', { name: tp.name }), { danger: true })) run(async () => { await del('/templates/' + tp.id); tpls.reload() }) }}>{t('删除模板')}</button>}
                </div>
              </article>
            ))}
          </div>
          <div className="row">
            <button className="btn" type="button" onClick={() => setEditor('new')}>{t('+ 新建模板')}</button>
            <span className="spacer" />
            <button className="btn pri" type="button" disabled={!tplId} onClick={() => setStep(2)}>{t('下一步：选节点')}</button>
          </div>
        </>
      )}

      {step === 2 && (
        <>
          <div className="row">
            <span className="lbl">{t('按标签筛选：')}</span>
            {labels.map((l) => <button key={l} type="button" className={'chip' + (label === l ? ' on' : '')} onClick={() => { setLabel(l); setNodeIds(nodes.filter((n) => n.labels?.includes(l)).map((n) => n.id)) }}>{l}</button>)}
            {labels.length === 0 && <span className="hint">{t('节点还没有标签')}</span>}
            <span className="spacer" />
            <button className="btn sm" type="button" onClick={() => setNodeIds(nodes.map((n) => n.id))}>{t('全选')}</button>
            <button className="btn sm" type="button" onClick={() => setNodeIds([])}>{t('清空')}</button>
          </div>
          <div className="card flush scroll">
            <table>
              <thead><tr><th style={{ width: 32 }} /><th>{t('节点')}</th><th>{t('地区')}</th><th>{t('已装内核')}</th><th>{t('标签')}</th><th>{t('现有入站')}</th></tr></thead>
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
          {realityItems.length > 0 && nodeIds.length > 0 && <details className="card">
            <summary>{t('按节点设置 REALITY 目标（可选）')}</summary>
            <div className="hint">{t('默认使用模板目标；自定义目标会保留为该节点的手动设置。')}</div>
            {nodes.filter((n) => nodeIds.includes(n.id)).map((n) => <div key={n.id}>
              <h3>{n.name}</h3>
              {realityItems.map((item) => {
                const value = realityOverrides[n.id]?.[item.key]
                const change = (patch: Partial<{ dest: string; serverNames: string[] }>) => setRealityOverrides((old) => ({ ...old, [n.id]: { ...old[n.id], [item.key]: { dest: value?.dest ?? item.security.realityDest ?? '', serverNames: value?.serverNames ?? item.security.realityServerNames ?? [], ...patch } } }))
                return <div className="form-grid" key={item.key}>
                  <Field label={t('目标 · {name}', { name: itemTag(item) })}><input className="input mono" value={value?.dest ?? ''} placeholder={item.security.realityDest} onChange={(e) => change({ dest: e.target.value })} /></Field>
                  <Field label="SNI"><input className="input mono" value={value?.serverNames.join(', ') ?? ''} placeholder={item.security.realityServerNames?.join(', ')} onChange={(e) => change({ serverNames: e.target.value.split(/[,，\s]+/).filter(Boolean) })} /></Field>
                  {value && <button className="btn sm" onClick={() => setRealityOverrides((old) => { const entries = { ...old[n.id] }; delete entries[item.key]; return { ...old, [n.id]: entries } })}>{t('恢复模板目标')}</button>}
                </div>
              })}
            </div>)}
          </details>}
          <div className="row">
            <button className="btn" type="button" onClick={() => setStep(1)}>{t('上一步')}</button>
            <span className="spacer" />
            <button className="btn pri" type="button" disabled={!nodeIds.length || busy} onClick={doPreview}>{t('预览 {n} 个节点', { n: nodeIds.length })}</button>
          </div>
        </>
      )}

      {step === 3 && plan && tpl && (
        <div className="split">
          <aside className="aside" style={{ flex: '1 1 300px', maxWidth: 340 }}>
            <section className="card">
              <h2>{t('模板 · {name}', { name: tpl.name })}</h2>
              <div className="sub">{tpl.builtin ? t('内置模板') : t('自定义模板')} · {t('已被 {n} 个节点使用', { n: tpl.nodes })}</div>
              <dl className="kv" style={{ gridTemplateColumns: '80px minmax(0,1fr)' }}>
                <dt>{t('内核')}</dt><dd>{[...new Set(tpl.items.map((i) => i.core === 'auto' ? t('auto（按能力自动选）') : coreLabel[i.core]))].join(t('、'))}</dd>
                <dt>{t('入站')}</dt><dd>{tpl.items.map((i) => <span key={i.key} className="tag">{itemTag(i)}</span>)}</dd>
                <dt>{t('端口')}</dt><dd>{tpl.items.map((i) => `${itemTag(i)} ${portText(i)}`).join(t('；'))}</dd>
                <dt>{t('标签')}</dt><dd className="mono">{tpl.items[0]?.tagPattern}</dd>
                <dt>{t('证书')}</dt><dd>{[...new Set(tpl.items.map((i) => certText[i.cert] ?? i.cert))].join(t('、'))}</dd>
                <dt>{t('授权')}</dt><dd>{tpl.groups.map((g) => groups.data?.find((x) => x.id === g)?.name ?? g).join(t('、')) || '—'}</dd>
              </dl>
            </section>
            <section className="card">
              <h2>{t('选项')}</h2>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4, marginTop: 10 }}>
                <label className="chk"><input type="checkbox" checked={opt.reassign} onChange={(e) => setOpt({ ...opt, reassign: e.target.checked })} />{t('内核不支持时改派同节点另一内核')}</label>
                <label className="chk"><input type="checkbox" checked={opt.autoPort} onChange={(e) => setOpt({ ...opt, autoPort: e.target.checked })} />{t('端口冲突时自动换端口')}</label>
                {realityItems.length > 0 && <label className="chk"><input type="checkbox" checked={opt.verifyReality} onChange={(e) => setOpt({ ...opt, verifyReality: e.target.checked })} />{t('生成前要求节点 REALITY 检测通过')}</label>}
                <label className="chk"><input type="checkbox" checked={opt.bind} disabled={!label} onChange={(e) => setOpt({ ...opt, bind: e.target.checked })} />{t('把模板绑定到节点标签')}{label ? t('「{label}」', { label }) : t('（先按标签选节点）')}</label>
                {needsCDN && (
                  <Field label={t('CDN 入口使用的配置档')}>
                    <select className="input" value={opt.cdnProfileId} onChange={(e) => setOpt({ ...opt, cdnProfileId: e.target.value })}>
                      <option value="">{t('不添加 CDN 入口')}</option>{(cdn.data ?? []).filter((c) => c.originMode !== 'tunnel').map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
                    </select>
                  </Field>
                )}
                <button className="btn sm" type="button" style={{ alignSelf: 'flex-start', marginTop: 6 }} disabled={busy} onClick={doPreview}>{t('按选项重新预览')}</button>
              </div>
            </section>
          </aside>
          <div className="grow">
            {!plan.state && <div className="note">{t('预览固定端口和随机配置，10 分钟内有效。模板、节点配置或选项变化后需重新预览。')}</div>}
            {!plan.state && (dirty || expired) && <div className="note">{t('预览已过期或选项已变化，请重新预览后生成。')}</div>}
            {!plan.state && opt.verifyReality && !checksPassed && <div className="note">{t('请逐节点检测 REALITY 目标；检测未通过时可修改目标，或跳过该节点并重新预览。')}</div>}
            {refreshError && <div className="note">{t('应用状态刷新失败：{error}', { error: refreshError })}</div>}
            {plan.state && <div className="note">{t('配置已写入，实际生效以节点回报为准；离线节点上线后自动接收。')}</div>}
            <div className="grid g4">
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>{t('节点')}</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.nodes}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>{t('将创建入站')}</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.create}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>{t('自动调整')}</span><div className="mono" style={{ fontSize: 22, color: 'var(--warn-ink)' }}>{plan.summary.adjusted}</div></div>
              <div className="card" style={{ padding: '14px 16px' }}><span className="sub" style={{ margin: 0 }}>{t('跳过')}</span><div className="mono" style={{ fontSize: 22 }}>{plan.summary.skipped}</div></div>
            </div>
            <section className="card flush scroll">
              <table>
                <thead><tr><th>{t('入站标签')}</th><th>{t('内核')}</th><th>{t('端口')}</th><th>{t('证书 / 密钥')}</th><th>{t('入口')}</th><th>{t('结果')}</th></tr></thead>
                <tbody>
                  {plan.plans.map((p) => (
                    <PlanRows key={p.nodeId} p={p} check={plan.checks?.[p.nodeId]} actions={!plan.state && hasReality(p) ? <>
                      <button className="btn sm" disabled={busy || dirty || expired} onClick={() => run(async () => {
                        const id = plan.planId
                        const check = await post<TemplateRealityCheck>(`/templates/${tplId}/runs/${id}/reality-check`, { nodeId: p.nodeId })
                        setPlan((old) => old?.planId === id ? { ...old, checks: { ...old.checks, [p.nodeId]: check } } : old)
                      })}>{t('从此节点检测目标')}</button>
                      <button className="btn sm" disabled={busy} onClick={() => { setNodeIds((ids) => ids.filter((id) => id !== p.nodeId)); setStep(2) }}>{t('跳过此节点')}</button>
                    </> : undefined} />
                  ))}
                </tbody>
              </table>
            </section>
            <div className="row" style={{ justifyContent: 'space-between' }}>
              <div style={{ fontSize: 13 }}>{plan.state ? t('应用进度每 4 秒刷新') : t('确认后执行本次预览计划，单个节点失败不影响其他节点。')}</div>
              <div className="row">
                <button className="btn" type="button" onClick={() => setStep(2)}>{t('上一步')}</button>
                {plan.state ? <button className="btn" type="button" disabled={busy || !plan.plans.some((p) => p.applyStatus === 'error')} onClick={() => run(async () => {
                  setPlan(await post<TemplatePlan>(`/templates/${tplId}/runs/${plan.planId}/retry`))
                }, t('已重试失败节点'))}>{t('只重试失败节点')}</button> : <button className="btn pri" type="button" disabled={busy || dirty || expired || !plan.summary.create || (opt.verifyReality && !checksPassed)} onClick={() => run(async () => {
                  const r = await post<TemplatePlan>(`/templates/${tplId}/apply`, { ...body(), planId: plan.planId })
                  setPlan(r)
                  tpls.reload()
                }, t('生成计划已执行，请查看节点应用状态'))}>{t('生成并下发')}</button>}
              </div>
            </div>
          </div>
        </div>
      )}
      {editor && groups.data && <TemplateEditor tpl={editor === 'new' ? undefined : editor} groups={groups.data} onClose={() => setEditor(null)} onSaved={() => { setEditor(null); tpls.reload() }} />}
      {sync && <SyncModal tpl={sync} onClose={() => { setSync(null); tpls.reload() }} />}
      {history && <TemplateHistory tpl={history} onClose={() => setHistory(null)} />}
    </>
  )
}

function PlanRows({ p, actions, check }: { p: NodePlan; actions?: React.ReactNode; check?: TemplateRealityCheck }) {
  return (
    <>
      <tr className="group"><td colSpan={6}>{p.nodeName} · {p.summary}
        {p.applyStatus && <span className={'tag ' + (p.applyStatus === 'applied' ? 'ok' : p.applyStatus === 'error' ? 'warn' : '')}>{applyLabel(p.applyStatus)}{p.revision ? ` · r${p.revision}` : ''}</span>}
        {p.error && <span style={{ color: 'var(--bad)' }}> · {p.error}</span>}{actions && <div className="row" style={{ marginTop: 8 }}>{actions}</div>}</td></tr>
      {check && <tr><td colSpan={6} style={{ whiteSpace: 'normal', overflowWrap: 'anywhere' }}>
        <span className={'tag ' + (check.ok ? 'ok' : 'warn')}>{check.ok ? t('节点基础检测通过') : t('节点基础检测未通过')}</span>
        {check.results.map((r, i) => <div key={i} className="small mono">{r.target} · SNI: {r.serverName ?? '—'} · {r.latencyMs != null ? `${r.latencyMs} ms` : '—'}{r.problem && ` · ${tr(r.problem)}`}</div>)}
      </td></tr>}
      {p.items.map((it) => (
        <tr key={it.itemKey}>
          <td className="mono">{it.tag}</td>
          <td>{it.core ? <span className={coreTagClass(it.core)}>{coreShort[it.core]}</span> : '—'}</td>
          <td className="mono">{it.port ? `${it.port}${it.network === 'udp' ? '/udp' : ''}` : '—'}</td>
          <td>{it.cert || '—'}</td>
          <td>{it.entries || '—'}</td>
          <td style={{ whiteSpace: 'normal' }}><span className={'st ' + (it.result === 'new' ? 'ok' : it.result === 'fix' ? 'warn' : 'off')} style={{ whiteSpace: 'normal' }}>{it.result === 'new' ? t('新建') : it.result === 'fix' ? it.reason : t('跳过：{reason}', { reason: it.reason })}</span>
            {(it.warnings ?? []).map((w, i) => <div key={i} className="hint" style={{ color: 'var(--warn)', overflowWrap: 'anywhere' }}>{tr(w)}</div>)}</td>
        </tr>
      ))}
    </>
  )
}

function applyLabel(status: string) {
  const labels: Record<string, string> = { pending: t('等待节点应用'), applied: t('节点已应用'), offline: t('节点离线，等待上线'), error: t('应用失败'), skipped: t('没有可生成的入站') }
  return labels[status] ?? status
}

function TemplateHistory({ tpl, onClose }: { tpl: InboundTemplate; onClose: () => void }) {
  const history = useLoad(() => get<{ runs: { id: string; createdAt: string; state: string }[] }>(`/templates/${tpl.id}/runs`))
  const [selected, setSelected] = useState('')
  const [details, setDetails] = useState<TemplatePlan | null>(null)
  const [error, setError] = useState('')
  const { busy, run } = useAction()
  useEffect(() => { if (!selected && history.data?.runs.length) setSelected(history.data.runs[0].id) }, [history.data, selected])
  useEffect(() => {
    if (!selected) return
    let active = true, pending = false
    setDetails(null)
    const poll = async () => {
      if (pending) return
      pending = true
      try { const data = await get<TemplatePlan>(`/templates/${tpl.id}/runs/${selected}`); if (active) { setDetails(data); setError('') } }
      catch (e) { if (active) setError((e as Error).message) }
      finally { pending = false }
    }
    void poll()
    const timer = window.setInterval(poll, 4000)
    return () => { active = false; window.clearInterval(timer) }
  }, [tpl.id, selected])
  return <Modal wide title={t('应用记录 · {name}', { name: tpl.name })} onClose={onClose} footer={<>
    <button className="btn" onClick={onClose}>{t('关闭')}</button>
    <button className="btn pri" disabled={busy || !details?.plans.some((p) => p.applyStatus === 'error')} onClick={() => run(async () => {
      setDetails(await post<TemplatePlan>(`/templates/${tpl.id}/runs/${selected}/retry`))
    }, t('已重试失败节点'))}>{t('只重试失败节点')}</button>
  </>}>
    <Field label={t('最近 20 次应用，保留 30 天')}>
      <select className="input" value={selected} onChange={(e) => setSelected(e.target.value)}>
        {(history.data?.runs ?? []).map((r) => <option key={r.id} value={r.id}>{new Date(r.createdAt).toLocaleString()} · {r.id.slice(0, 8)}</option>)}
      </select>
    </Field>
    {error && <div className="note">{error}</div>}
    {details ? <div className="card flush scroll"><table><tbody>{details.plans.map((p) => <PlanRows key={p.nodeId} p={p} />)}</tbody></table></div> : <Empty>{history.loading || selected ? t('加载中…') : t('暂无应用记录')}</Empty>}
  </Modal>
}
