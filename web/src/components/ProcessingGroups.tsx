import { useState } from 'react'
import { api, get, post } from '../api'
import { datetime } from '../format'
import { t } from '../i18n'
import type { Operator, SubProcessingGroup, SubProcessingRef } from '../types'
import { useFeature } from './License'
import OperatorChain from './OperatorChain'
import { Drawer, Empty, ErrorNote, Field, Modal, useAction, useLoad } from './ui'

type Version = { groupId: string; revision: number; name: string; remark: string; operators?: Operator[]; createdAt: string }
type Impact = { etag: string; references: { id: string; name: string; enabled: boolean; revision: number; followsLatest: boolean }[]; affected: number; revision: number; action: string }
type Draft = { name: string; remark: string; operators: Operator[]; expectedRevision: number }
const draftFor = (g: SubProcessingGroup): Draft => ({ name: g.name, remark: g.remark, operators: structuredClone(g.operators ?? []), expectedRevision: g.revision })

function RefRow({ value, groups, onChange, onRemove, move, first, last }: { value: SubProcessingRef; groups: SubProcessingGroup[]; onChange: (v: SubProcessingRef) => void; onRemove: () => void; move: (d: number) => void; first: boolean; last: boolean }) {
  const versions = useLoad(() => get<{ versions: Version[] }>(`/sub-processing-groups/${encodeURIComponent(value.groupId)}/versions`), [value.groupId])
  return <div className="ops-summary ops-stack">
    <select className="input" aria-label={t('选择处理组')} value={value.groupId} onChange={(e) => onChange({ groupId: e.target.value, revision: 0 })}>
      {!groups.some((g) => g.id === value.groupId) && <option value={value.groupId}>{t('不可用，请重新选择')} · {value.groupId}</option>}
      {groups.map((g) => <option key={g.id} value={g.id}>{g.name} · v{g.revision}</option>)}
    </select>
    <div className="row"><select className="input" aria-label={t('处理组版本')} style={{ flex: 1 }} value={value.revision} onChange={(e) => onChange({ ...value, revision: Number(e.target.value) })}>
      <option value={0}>{t('跟随最新版本')}</option>
      {(versions.data?.versions ?? []).map((v) => <option key={v.revision} value={v.revision}>{t('固定版本')} v{v.revision}</option>)}
      {value.revision > 0 && !versions.data?.versions.some((v) => v.revision === value.revision) && <option value={value.revision}>{t('固定版本')} v{value.revision}</option>}
    </select><button className="btn sm" disabled={first} aria-label={t('上移')} onClick={() => move(-1)}>↑</button><button className="btn sm" disabled={last} aria-label={t('下移')} onClick={() => move(1)}>↓</button><button className="btn sm" onClick={onRemove}>{t('移除引用')}</button></div>
    <ErrorNote error={versions.error} />
  </div>
}

export function ProcessingGroupPicker({ value, onChange }: { value: SubProcessingRef[]; onChange: (v: SubProcessingRef[]) => void }) {
  const list = useLoad(() => get<SubProcessingGroup[]>('/sub-processing-groups'))
  const available = (list.data ?? []).filter((g) => !value.some((ref) => ref.groupId === g.id))
  return <Field label={t('可复用处理组')} hint={t('执行顺序：全局规则 → 按顺序引用的处理组 → 本订阅步骤。处理组不增加节点来源或用户权限。')}>
    <div className="ops-stack"><ErrorNote error={list.error} />
      {value.map((ref, i) => <RefRow key={i} value={ref} groups={(list.data ?? []).filter((g) => g.id === ref.groupId || !value.some((x) => x.groupId === g.id))} onChange={(v) => onChange(value.map((x, j) => i === j ? v : x))} onRemove={() => onChange(value.filter((_, j) => i !== j))} first={i === 0} last={i === value.length - 1} move={(d) => { const next = [...value]; [next[i], next[i + d]] = [next[i + d], next[i]]; onChange(next) }} />)}
      <button className="btn sm" disabled={!available.length || value.length >= 20} onClick={() => onChange([...value, { groupId: available[0].id, revision: 0 }])}>{t('添加处理组引用')}</button>
    </div>
  </Field>
}

function GroupHistory({ group, onClose, onRestore }: { group: SubProcessingGroup; onClose: () => void; onRestore: (operators: Operator[]) => void }) {
  const list = useLoad(() => get<{ versions: Version[] }>(`/sub-processing-groups/${group.id}/versions`), [group.id])
  const [detail, setDetail] = useState<Version>()
  const { busy, run } = useAction()
  return <Modal title={t('处理组版本历史')} onClose={onClose} wide><div className="ops-stack"><ErrorNote error={list.error} /><div className="hint">{t('版本不可修改；恢复会以旧规则创建新版本，固定引用仍保留原版本。')}</div>{list.data?.versions.map((v) => <div className="row" key={v.revision}><b>v{v.revision}</b><span>{datetime(v.createdAt)}</span><button className="link" disabled={busy} onClick={() => run(async () => setDetail(await get<Version>(`/sub-processing-groups/${group.id}/versions/${v.revision}`)))}>{t('查看规则')}</button></div>)}{detail && <><pre className="code ops-raw">{JSON.stringify(detail.operators, null, 2)}</pre><button className="btn" onClick={() => onRestore(detail.operators ?? [])}>{t('以此版本编辑')}</button></>}</div></Modal>
}

export default function ProcessingGroups({ meta }: { meta?: { regions: { Code: string; Name: string }[] } }) {
  const list = useLoad(() => get<SubProcessingGroup[]>('/sub-processing-groups'))
  const [editing, setEditing] = useState<SubProcessingGroup | 'new'>()
  const [draft, setDraft] = useState<Draft>({ name: '', remark: '', operators: [], expectedRevision: 0 })
  const [impact, setImpact] = useState<Impact>()
  const [removing, setRemoving] = useState(false)
  const [history, setHistory] = useState<SubProcessingGroup>()
  const { busy, run } = useAction()
  const ok = useFeature('sub_convert')
  const change = (patch: Partial<Draft>) => { setDraft((d) => ({ ...d, ...patch })); setImpact(undefined) }
  const edit = (g: SubProcessingGroup | 'new', remove = false) => { setEditing(g); setDraft(g === 'new' ? { name: '', remark: '', operators: [], expectedRevision: 0 } : draftFor(g)); setRemoving(remove); setImpact(undefined) }
  const preview = () => run(async () => { if (!editing || editing === 'new') return; setImpact(await post<Impact>(`/sub-processing-groups/${editing.id}/preview`, { ...draft, action: removing ? 'delete' : 'update' })) })
  const save = () => run(async () => {
    if (!editing) return
    if (editing === 'new') await post('/sub-processing-groups', draft)
    else { if (!impact) return; try { await api(removing ? 'DELETE' : 'PUT', `/sub-processing-groups/${editing.id}`, { ...draft, action: removing ? 'delete' : 'update' }, { 'If-Match': impact.etag }) } catch (e) { setImpact(undefined); throw e } }
    setEditing(undefined); list.reload()
  }, removing ? t('已删除') : t('已保存'))
  return <div className="ops-stack">
    <div className="row"><div className="hint" style={{ flex: 1 }}>{t('将共同的过滤、重命名、排序和去重步骤保存一次，供多个订阅按顺序引用。')}</div><button className="btn pri" disabled={!ok} onClick={() => edit('new')}>+ {t('新建处理组')}</button><button className="btn" onClick={list.reload}>{t('刷新')}</button></div>
    <ErrorNote error={list.error} />
    {list.data?.length === 0 && <Empty>{t('暂无处理组')}</Empty>}
    {(list.data ?? []).map((g) => <section className="card ops-stack" key={g.id}><div className="row"><h2>{g.name}</h2><span className="tag">v{g.revision}</span><span className="small mute">{t('{n} 步', { n: g.operators?.length ?? 0 })}</span></div>{g.remark && <div className="small mute">{g.remark}</div>}<div className="row"><button className="btn sm" disabled={!ok} onClick={() => edit(g)}>{t('编辑与影响预览')}</button><button className="btn sm" onClick={() => setHistory(g)}>{t('版本历史')}</button><button className="btn sm danger" disabled={!ok} onClick={() => edit(g, true)}>{t('删除')}</button></div></section>)}
    {editing && <Drawer wide title={removing ? t('删除处理组') : editing === 'new' ? t('新建处理组') : t('编辑处理组')} onClose={() => setEditing(undefined)} footer={<><button className="btn" disabled={busy} onClick={() => setEditing(undefined)}>{t('取消')}</button>{editing !== 'new' && <button className="btn" disabled={busy || !draft.name.trim()} onClick={preview}>{t('预览影响范围')}</button>}<button className={'btn ' + (removing ? 'danger' : 'pri')} disabled={busy || !ok || !draft.name.trim() || (editing !== 'new' && !impact) || (removing && !!impact?.references.length)} onClick={save}>{removing ? t('确认删除') : t('保存')}</button></>}>
      <div className="ops-stack"><Field label={t('名称')}><input className="input" aria-label={t('处理组名称')} disabled={removing} value={draft.name} onChange={(e) => change({ name: e.target.value })} /></Field><Field label={t('备注')}><input className="input" aria-label={t('处理组备注')} disabled={removing} value={draft.remark} onChange={(e) => change({ remark: e.target.value })} /></Field>
      {!removing && <OperatorChain ops={draft.operators} onChange={(operators) => change({ operators })} meta={meta} maxSteps={64} />}
      {impact && <div className="ops-stack"><div className="note">{t('{n} 个订阅跟随最新版本', { n: impact.affected })} · {t('固定版本引用不随更新改变')}</div>{impact.references.length === 0 && <div>{t('没有订阅引用此处理组')}</div>}{impact.references.map((ref) => <div className="ops-summary" key={ref.id}><b>{ref.name}</b> · {ref.followsLatest ? t('跟随最新版本') : t('固定版本') + ' v' + ref.revision} · {ref.enabled ? t('启用') : t('已停用')}</div>)}{removing && impact.references.length > 0 && <div className="note warn">{t('包含已停用和固定版本的引用；解除全部引用后才能删除。')}</div>}</div>}
      </div>
    </Drawer>}
    {history && <GroupHistory group={history} onClose={() => setHistory(undefined)} onRestore={(operators) => { edit(history); setDraft({ ...draftFor(history), operators }); setHistory(undefined) }} />}
  </div>
}
