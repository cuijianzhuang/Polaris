import { useEffect, useMemo, useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import InboundFields, { type InboundDraft } from '../components/InboundFields'
import { Drawer, Empty, Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
import { coreLabel, coreShort, coreTagClass, protoLabel, transLabel } from '../format'
import type { CapSet, CDNProfile, Group, InboundTemplate, NodePlan, TemplateItem } from '../types'
import { t, tr } from '../i18n'

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
  return it.port.mode === 'fixed' ? t('固定 {port}', { port: it.port.fixed }) : t('{min}–{max} 随机', { min: it.port.min ?? 20000, max: it.port.max ?? 30000 })
}

const certText: Record<string, string> = { acme: t('ACME 签 {node.domain}'), reality: t('REALITY 自动生成密钥'), selfsigned: t('自签') }

function newItem(): TemplateItem {
  return {
    key: Math.random().toString(36).slice(2, 8), core: 'auto', protocol: { type: 'vless', flow: 'xtls-rprx-vision' }, transport: { type: 'tcp' },
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
        {items.map((x, i) => <button key={x.key} type="button" className={'tab' + (i === idx ? ' on' : '')} onClick={() => setIdx(i)}>{itemTag(x)}</button>)}
        <button type="button" className="tab" onClick={() => { setItems((x) => [...x, newItem()]); setIdx(items.length) }}>{t('+ 入站')}</button>
      </div>
      {it && (
        <>
          <InboundFields draft={{ core: it.core, protocol: it.protocol, transport: it.transport, security: it.security, sniffing: it.sniffing }}
            onChange={(d: InboundDraft) => setIt({ core: d.core, protocol: d.protocol, transport: d.transport, security: d.security, sniffing: d.sniffing })}
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
                <option value="acme">{t('每节点 ACME 签 {node.domain}')}</option><option value="selfsigned">{t('自签')}</option><option value="reality">{t('REALITY（无需证书）')}</option>
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
  type Change = { inboundId: string; tag: string; nodeName: string; fields: string[] | null; kept: string[] | null; error?: string }
  const diff = useLoad(() => post<{ changes: Change[] }>(`/templates/${tpl.id}/sync`, { confirm: false }), [tpl.id])
  const [overwrite, setOverwrite] = useState(false)
  const { busy, run } = useAction()
  const changes = diff.data?.changes ?? []
  const pending = changes.filter((c) => c.fields?.length)
  return (
    <Modal wide title={t('同步模板 · {name}', { name: tpl.name })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || (!pending.length && !overwrite)} onClick={() => run(async () => {
        await post(`/templates/${tpl.id}/sync`, { confirm: true, overwrite })
        onClose()
      }, t('已同步并下发'))}>{t('确认同步 {n} 个入站', { n: pending.length })}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{t('手动改过的字段标记为「已覆盖」，同步时默认保留，不会被模板冲掉。')}</div>
      <div className="card flush scroll">
        {changes.length === 0 ? <Empty>{diff.loading ? t('计算差异中…') : t('所有入站已与模板一致')}</Empty> : (
          <table>
            <thead><tr><th>{t('节点')}</th><th>{t('入站')}</th><th>{t('将更新字段')}</th><th>{t('保留（已覆盖）')}</th></tr></thead>
            <tbody>{changes.map((c) => (
              <tr key={c.inboundId}><td>{c.nodeName}</td><td className="mono">{c.tag}</td>
                <td>{c.fields?.map((f) => <span key={f} className="tag warn">{f}</span>) ?? '—'}{c.error && <div className="small" style={{ color: 'var(--bad)' }}>{c.error}</div>}</td>
                <td>{c.kept?.map((f) => <span key={f} className="tag">{f}</span>) ?? '—'}</td></tr>
            ))}</tbody>
          </table>
        )}
      </div>
      <label className="chk"><input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />{t('同时覆盖手动修改过的字段')}</label>
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
  useEffect(() => { if (!tplId && tpls.data?.length) setTplId(tpls.data.find((t) => t.name === '\u5168\u534f\u8bae')?.id ?? tpls.data[0].id) }, [tpls.data, tplId])

  const body = () => ({ nodeIds, reassign: opt.reassign, autoPort: opt.autoPort, bindLabel: opt.bind ? label : '', cdnProfileId: opt.cdnProfileId })
  const doPreview = () => run(async () => {
    setPlan(await post(`/templates/${tplId}/preview`, body()))
    setStep(3)
  })
  const needsCDN = tpl?.items.some((i) => i.endpoints.some((e) => e.kind === 'cdn' && !e.cdnProfileId))

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
          <div className="grid g2">
            {(tpls.data ?? []).map((tp) => (
              <article key={tp.id} className="card" style={{ borderColor: tp.id === tplId ? 'var(--pri)' : undefined, borderWidth: tp.id === tplId ? 2 : 1, cursor: 'pointer' }} onClick={() => setTplId(tp.id)}>
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <h2>{tp.builtin ? tr(tp.name) : tp.name}</h2>{tp.builtin ? <span className="tag">{t('内置')}</span> : <span className="tag xray">{t('自定义')}</span>}
                </div>
                <div className="sub">{(tp.builtin ? tr(tp.description) : tp.description) || '—'} · {t('已生成 {inbounds} 个入站（{nodes} 个节点）', { inbounds: tp.inbounds, nodes: tp.nodes })}</div>
                <div style={{ marginTop: 10, display: 'flex', flexWrap: 'wrap', gap: 6 }}>{groupItems(tp.items).map(({ it, n }) => <span key={it.key} className={coreTagClass(it.core === 'auto' ? (it.transport.type === 'xhttp' ? 'xray' : ['hysteria2', 'tuic', 'anytls'].includes(it.protocol.type) ? 'singbox' : 'xray') : it.core)}>{itemTag(it)}{n > 1 && <b style={{ marginLeft: 4 }}>×{n}</b>}</span>)}</div>
                <div className="row" style={{ marginTop: 12 }} onClick={(e) => e.stopPropagation()}>
                  <button className="btn sm" type="button" onClick={() => setEditor(tp)}>{tp.builtin ? t('另存为自定义') : t('编辑')}</button>
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
                <label className="chk"><input type="checkbox" checked={opt.bind} disabled={!label} onChange={(e) => setOpt({ ...opt, bind: e.target.checked })} />{t('把模板绑定到节点标签')}{label ? t('「{label}」', { label }) : t('（先按标签选节点）')}</label>
                {needsCDN && (
                  <Field label={t('CDN 入口使用的配置档')}>
                    <select className="input" value={opt.cdnProfileId} onChange={(e) => setOpt({ ...opt, cdnProfileId: e.target.value })}>
                      <option value="">{t('不添加 CDN 入口')}</option>{(cdn.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
                    </select>
                  </Field>
                )}
                <button className="btn sm" type="button" style={{ alignSelf: 'flex-start', marginTop: 6 }} disabled={busy} onClick={doPreview}>{t('按选项重新预览')}</button>
              </div>
            </section>
          </aside>
          <div className="grow">
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
                    <PlanRows key={p.nodeId} p={p} />
                  ))}
                </tbody>
              </table>
            </section>
            <div className="row" style={{ justifyContent: 'space-between' }}>
              <div style={{ fontSize: 13 }}>{t('将在')} <b>{plan.summary.nodes}</b> {t('个节点生成')} <b>{plan.summary.create}</b> {t('个入站，各节点配置版本 +1；单个节点失败不影响其他')}</div>
              <div className="row">
                <button className="btn" type="button" onClick={() => setStep(2)}>{t('上一步')}</button>
                <button className="btn pri" type="button" disabled={busy || !plan.summary.create} onClick={() => run(async () => {
                  const r = await post<typeof plan>(`/templates/${tplId}/apply`, body())
                  setPlan(r)
                  tpls.reload()
                }, t('已生成并下发'))}>{t('生成并下发')}</button>
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
      <tr className="group"><td colSpan={6}>{p.nodeName} · {p.summary}{p.error && <span style={{ color: 'var(--bad)' }}> · {t('写入失败：{err}', { err: p.error })}</span>}</td></tr>
      {p.items.map((it) => (
        <tr key={it.itemKey}>
          <td className="mono">{it.tag}</td>
          <td>{it.core ? <span className={coreTagClass(it.core)}>{coreShort[it.core]}</span> : '—'}</td>
          <td className="mono">{it.port ? `${it.port}${it.network === 'udp' ? '/udp' : ''}` : '—'}</td>
          <td>{it.cert || '—'}</td>
          <td>{it.entries || '—'}</td>
          <td style={{ whiteSpace: 'normal' }}><span className={'st ' + (it.result === 'new' ? 'ok' : it.result === 'fix' ? 'warn' : 'off')} style={{ whiteSpace: 'normal' }}>{it.result === 'new' ? t('新建') : it.result === 'fix' ? it.reason : t('跳过：{reason}', { reason: it.reason })}</span></td>
        </tr>
      ))}
    </>
  )
}
