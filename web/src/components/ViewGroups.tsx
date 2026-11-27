import { useState } from 'react'
import { del, get, post, put } from '../api'
import { datetime } from '../format'
import { t } from '../i18n'
import type { Chain, Operator, SubProcessingRef, SubViewGroup } from '../types'
import { ProcessingGroupPicker } from './ProcessingGroups'
import OperatorChain from './OperatorChain'
import { Drawer, Empty, ErrorNote, Field, Switch, useAction, useLoad } from './ui'

// Subscription view groups (订阅视图组): a predicate picks a subset of the
// nodes a user is already authorised for, a processing chain (referenced
// processing groups, then local steps) reshapes them, and the rest falls
// through to the default group untouched. Bound to plans (ordered) and,
// later, to combined subscriptions.

type Draft = {
  name: string; remark: string; selector: Operator; processingGroups: SubProcessingRef[]; operators: Operator[]
  kinds: string[]; chainIds: string[]; icon: string; sortOrder: number; pinned: boolean; grouped: boolean
  enabled: boolean; revision: number
}

const blankDraft: Draft = {
  name: '', remark: '', selector: { type: 'filter' }, processingGroups: [], operators: [],
  kinds: [], chainIds: [], icon: '', sortOrder: 0, pinned: false, grouped: false, enabled: true, revision: 0,
}

const draftFor = (g: SubViewGroup): Draft => ({
  name: g.name, remark: g.remark, selector: structuredClone(g.selector ?? { type: 'filter' }),
  processingGroups: structuredClone(g.processingGroups ?? []), operators: structuredClone(g.operators ?? []),
  kinds: [...(g.kinds ?? [])], chainIds: [...(g.chainIds ?? [])], icon: g.icon ?? '', sortOrder: g.sortOrder ?? 0,
  pinned: !!g.pinned, grouped: !!g.grouped, enabled: g.enabled, revision: g.revision,
})

const kindLabels: Record<string, string> = { direct: t('直连'), cdn: 'CDN', relay: t('中转') }

export function ViewGroupPicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const list = useLoad((signal) => get<SubViewGroup[]>('/sub-view-groups', signal))
  const groups = list.data ?? []
  const byId = new Map(groups.map((g) => [g.id, g]))
  const available = groups.filter((g) => !value.includes(g.id))
  const move = (i: number, d: number) => {
    const j = i + d
    if (j < 0 || j >= value.length) return
    const next = [...value];[next[i], next[j]] = [next[j], next[i]]; onChange(next)
  }
  return <Field label={t('订阅视图组')} hint={t('按顺序执行：每个视图组从用户的已授权节点里挑走命中的子集加工，其余节点原样进入默认组；不绑定任何视图组时订阅输出与原来完全一致。')}>
    <div className="ops-stack"><ErrorNote error={list.error} />
      {value.map((id, i) => <div className="row" key={id}>
        <span style={{ flex: 1 }}>{byId.get(id) ? (byId.get(id)!.icon ? byId.get(id)!.icon + ' ' : '') + byId.get(id)!.name : t('不可用，请重新选择') + ' · ' + id}{byId.get(id) && !byId.get(id)!.enabled ? ' · ' + t('已停用') : ''}</span>
        <button className="btn sm" type="button" disabled={i === 0} aria-label={t('上移')} onClick={() => move(i, -1)}>↑</button>
        <button className="btn sm" type="button" disabled={i === value.length - 1} aria-label={t('下移')} onClick={() => move(i, 1)}>↓</button>
        <button className="btn sm" type="button" onClick={() => onChange(value.filter((_, j) => j !== i))}>{t('移除')}</button>
      </div>)}
      <button className="btn sm" type="button" disabled={!available.length || value.length >= 10} onClick={() => onChange([...value, available[0].id])}>{t('添加视图组')}</button>
    </div>
  </Field>
}

export default function ViewGroups({ meta }: { meta?: { regions: { Code: string; Name: string }[] } }) {
  const list = useLoad((signal) => get<SubViewGroup[]>('/sub-view-groups', signal))
  const chains = useLoad((signal) => get<Chain[]>('/chains', signal))
  const [editing, setEditing] = useState<SubViewGroup | 'new'>()
  const [draft, setDraft] = useState<Draft>(blankDraft)
  const [delError, setDelError] = useState('')
  const { busy, run } = useAction()
  const change = (patch: Partial<Draft>) => setDraft((d) => ({ ...d, ...patch }))
  const edit = (g: SubViewGroup | 'new') => { setEditing(g); setDraft(g === 'new' ? { ...blankDraft } : draftFor(g)); setDelError('') }
  const save = () => run(async () => {
    if (!editing) return
    const body = { ...draft, selector: { ...draft.selector, type: 'filter' as const } }
    if (editing === 'new') await post('/sub-view-groups', body)
    else await put(`/sub-view-groups/${editing.id}`, body)
    setEditing(undefined); list.reload()
  }, t('已保存'))
  const remove = (g: SubViewGroup) => run(async () => {
    setDelError('')
    try { await del(`/sub-view-groups/${g.id}`) } catch (e) { setDelError(e instanceof Error ? e.message : String(e)); throw e }
    list.reload()
  }, t('已删除'))
  const toggleKind = (k: string) => change({ kinds: draft.kinds.includes(k) ? draft.kinds.filter((x) => x !== k) : [...draft.kinds, k] })
  const toggleChain = (id: string) => change({ chainIds: draft.chainIds.includes(id) ? draft.chainIds.filter((x) => x !== id) : [...draft.chainIds, id] })
  return <div className="ops-stack">
    <div className="row"><div className="hint" style={{ flex: 1 }}>{t('把「一类节点在订阅里长什么样」沉淀成组：谓词挑节点、加工链改节点，在套餐里按顺序绑定。视图组只能在用户已授权的节点里做减法，未命中的节点进默认组，不会丢节点。')}</div><button className="btn pri" onClick={() => edit('new')}>+ {t('新建视图组')}</button><button className="btn" onClick={list.reload}>{t('刷新')}</button></div>
    <ErrorNote error={list.error} />
    {delError && <div className="note warn">{delError}</div>}
    {list.data?.length === 0 && <Empty>{t('暂无视图组')}</Empty>}
    {(list.data ?? []).map((g) => <section className="card ops-stack" key={g.id}><div className="row"><h2>{g.icon ? g.icon + ' ' : ''}{g.name}</h2><span className="tag">v{g.revision}</span>{g.pinned && <span className="tag">{t('置顶')}</span>}{!g.enabled && <span className="tag">{t('已停用')}</span>}<span className="small mute">{datetime(g.updatedAt)}</span></div>{g.remark && <div className="small mute">{g.remark}</div>}<div className="row"><button className="btn sm" onClick={() => edit(g)}>{t('编辑')}</button><button className="btn sm danger" disabled={busy} onClick={() => remove(g)}>{t('删除')}</button></div></section>)}
    {editing && <Drawer wide title={editing === 'new' ? t('新建视图组') : t('编辑视图组')} onClose={() => setEditing(undefined)} footer={<><button className="btn" disabled={busy} onClick={() => setEditing(undefined)}>{t('取消')}</button><button className="btn pri" disabled={busy || !draft.name.trim()} onClick={save}>{t('保存')}</button></>}>
      <div className="ops-stack">
        <div className="form-grid">
          <Field label={t('名称')}><input className="input" aria-label={t('视图组名称')} value={draft.name} onChange={(e) => change({ name: e.target.value })} /></Field>
          <Field label={t('备注')}><input className="input" aria-label={t('视图组备注')} value={draft.remark} onChange={(e) => change({ remark: e.target.value })} /></Field>
          <Field label={t('图标')} hint={t('emoji 或短文本，客户端分组用')}><input className="input" aria-label={t('视图组图标')} value={draft.icon} onChange={(e) => change({ icon: e.target.value })} /></Field>
          <Field label={t('排序权重')} hint={t('小的排前面（列表排序，不影响绑定顺序）')}><input className="input mono" type="number" aria-label={t('视图组排序')} value={draft.sortOrder} onChange={(e) => change({ sortOrder: Number(e.target.value) || 0 })} /></Field>
        </div>
        <div className="row">
          <Switch label={t('启用')} on={draft.enabled} onChange={(v) => change({ enabled: v })} />
          <Switch label={t('置顶')} on={draft.pinned} onChange={(v) => change({ pinned: v })} />
          <Switch label={t('在客户端单独成组')} on={draft.grouped} onChange={(v) => change({ grouped: v })} />
        </div>
        <Field label={t('选择谓词')} hint={t('一条过滤规则：命中谓词的节点归本组加工，其余留给后面的组或默认组。')}>
          <OperatorChain ops={[draft.selector]} onChange={(ops) => change({ selector: ops[0] ?? { type: 'filter' } })} meta={meta} maxSteps={1} />
        </Field>
        <Field label={t('形态勾选')} hint={t('在谓词之上再按形态收窄（与谓词里的形态条件取交集）')}>
          <div className="row">{Object.entries(kindLabels).map(([k, l]) => <button key={k} type="button" className={'chip' + (draft.kinds.includes(k) ? ' on' : '')} onClick={() => toggleKind(k)}>{l}</button>)}</div>
        </Field>
        {(chains.data ?? []).length > 0 && <Field label={t('只含这些中转链')} hint={t('不选为不限；仅对中转节点有意义')}>
          <div className="row">{(chains.data ?? []).filter((ch) => ch.mode === 'proxy').map((ch) => <button key={ch.id} type="button" className={'chip' + (draft.chainIds.includes(ch.id) ? ' on' : '')} onClick={() => toggleChain(ch.id)}>{ch.name}</button>)}</div>
        </Field>}
        <ProcessingGroupPicker value={draft.processingGroups} onChange={(processingGroups) => change({ processingGroups })} />
        <Field label={t('本组加工步骤')} hint={t('在引用的处理组之后执行')}>
          <OperatorChain ops={draft.operators} onChange={(operators) => change({ operators })} meta={meta} maxSteps={64} />
        </Field>
      </div>
    </Drawer>}
  </div>
}
