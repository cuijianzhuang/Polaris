import { ProTag, useFeature } from '../../components/License'
import { useMemo, useState } from 'react'
import { del, get, post, put } from '../../api'
import { useNodes } from '../../App'
import { Field, Modal, useAction, useConfirm, useLoad } from '../../components/ui'
import { coreShort } from '../../format'
import type { Group, Inbound } from '../../types'
import { t } from '../../i18n'

export function GroupsModal({ onClose, onChanged }: { onClose: () => void; onChanged: () => void }) {
  const speedOK = useFeature('speed_limit')
  const groups = useLoad(() => get<Group[]>('/groups'))
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const [edit, setEdit] = useState<{ id?: string; name: string; inboundIds: string[]; nodeIds: string[]; level: number; speedUpMbps: number; speedDownMbps: number } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const { nodes } = useNodes()
  const tiered = (lvl: number) => nodes.filter((n) => (n.level ?? 0) > 0 && (n.level ?? 0) <= lvl)
  const byNode = useMemo(() => {
    const m = new Map<string, Inbound[]>()
    for (const i of inbounds.data ?? []) m.set(i.nodeName, [...(m.get(i.nodeName) ?? []), i])
    return [...m.entries()]
  }, [inbounds.data])
  return (
    <Modal title={t('分组')} onClose={onClose} wide>
      {!edit ? (
        <>
          <div className="card flush">
            <table>
              <thead><tr><th>{t('分组')}</th><th>{t('等级')}</th><th>{t('限速')}</th><th>{t('用户')}</th><th>{t('节点')}</th><th>{t('入站')}</th><th></th></tr></thead>
              <tbody>
                {(groups.data ?? []).filter((g) => !g.plan).map((g) => (
                  <tr key={g.id}>
                    <td style={{ fontWeight: 500 }}>{g.name}{g.isDefault && <span className="tag info-tag" style={{ marginLeft: 6 }}>{t('默认')}</span>}</td><td>{g.level > 0 ? <span className="tag info-tag">Lv.{g.level}</span> : <span className="mute">—</span>}</td><td className="mono small">{g.speedUpMbps || g.speedDownMbps ? `↑${g.speedUpMbps || '∞'} ↓${g.speedDownMbps || '∞'}` : <span className="mute">—</span>}</td><td className="mono">{g.userCount}</td>
                    <td className="mono" title={[...new Set([...tiered(g.level), ...nodes.filter((n) => g.nodeIds?.includes(n.id))].map((n) => n.name))].join('、')}>{new Set([...tiered(g.level).map((n) => n.id), ...(g.nodeIds ?? [])]).size}</td><td className="mono">{g.inboundIds.length}</td>
                    <td style={{ textAlign: 'right' }}>
                      <button className="link" type="button" onClick={() => setEdit({ id: g.id, name: g.name, inboundIds: g.inboundIds, nodeIds: g.nodeIds ?? [], level: g.level ?? 0, speedUpMbps: g.speedUpMbps ?? 0, speedDownMbps: g.speedDownMbps ?? 0 })}>{t('编辑')}</button>{'  '}
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(t('删除分组 {name}？组内用户将失去对应入站。', { name: g.name }), { danger: true })) run(async () => { await del('/groups/' + g.id); groups.reload(); onChanged() })
                      }}>{t('删除')}</button>
                    </td>
                  </tr>
                ))}
                {groups.data?.filter((g) => !g.plan).length === 0 && <tr><td colSpan={7} className="mute">{t('还没有分组')}</td></tr>}
              </tbody>
            </table>
          </div>
          <div><button className="btn pri" type="button" onClick={() => setEdit({ name: '', inboundIds: [], nodeIds: [], level: 0, speedUpMbps: 0, speedDownMbps: 0 })}>{t('+ 新建分组')}</button></div>
        </>
      ) : (
        <>
          <div className="form-grid">
            <Field label={t('名称')} htmlFor="g-name"><input id="g-name" className="input" value={edit.name} onChange={(e) => setEdit({ ...edit, name: e.target.value })} /></Field>
            <Field label={t('等级')} htmlFor="g-level" hint={t('组内用户可用等级 1 到该等级的全部节点（在节点设置里给节点定等级）；0 表示不按等级')}>
              <input id="g-level" className="input mono" type="number" min={0} max={99} value={edit.level} onChange={(e) => setEdit({ ...edit, level: Math.max(0, Math.min(99, Number(e.target.value) || 0)) })} />
            </Field>
            <Field label={<>{t('限速套餐 上行 / 下行 (Mbps)')}<ProTag on={speedOK} /></>} hint={t('组内没有单独设置限速的用户按此限速（属于多个分组取最高）；0 为不限')}>
              <div className="row" style={{ flexWrap: 'nowrap' }}>
                <input aria-label={t('上行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedUpMbps || ''} placeholder={t('上行')} onChange={(e) => setEdit({ ...edit, speedUpMbps: Math.max(0, Number(e.target.value) || 0) })} />
                <input aria-label={t('下行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedDownMbps || ''} placeholder={t('下行')} onChange={(e) => setEdit({ ...edit, speedDownMbps: Math.max(0, Number(e.target.value) || 0) })} />
              </div>
            </Field>
          </div>
          {edit.level > 0 && <div className="note">{t('按等级可用：')}{tiered(edit.level).map((n) => `${n.name} (Lv.${n.level})`).join(', ') || t('还没有等级不高于 {n} 的节点', { n: edit.level })}</div>}
          <Field label={t('授权整个节点')} hint={t('不论等级，组内用户可用这些节点上的全部入站（包括以后新增的入站）')}>
            <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
              {nodes.map((n) => {
                const on = edit.nodeIds.includes(n.id)
                return <button key={n.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
                  onClick={() => setEdit({ ...edit, nodeIds: on ? edit.nodeIds.filter((x) => x !== n.id) : [...edit.nodeIds, n.id] })}>{n.name}{(n.level ?? 0) > 0 && <span className="mute small" style={{ marginLeft: 4 }}>Lv.{n.level}</span>}</button>
              })}
              {nodes.length === 0 && <span className="hint">{t('还没有节点')}</span>}
            </div>
          </Field>
          <Field label={t('授权入站')} hint={t('只授权单个入站时使用')}>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8, maxHeight: 360, overflowY: 'auto' }}>
              {byNode.map(([node, ins]) => (
                <div key={node}>
                  <div className="lbl" style={{ marginBottom: 4 }}>{node}</div>
                  {ins.map((i) => (
                    <label key={i.id} className="chk">
                      <input type="checkbox" checked={edit.inboundIds.includes(i.id)} onChange={(e) => setEdit({ ...edit, inboundIds: e.target.checked ? [...edit.inboundIds, i.id] : edit.inboundIds.filter((x) => x !== i.id) })} />
                      <span className="mono">{i.tag}</span><span className="mute small">{coreShort[i.core]} · {i.port}</span>
                    </label>
                  ))}
                </div>
              ))}
              {byNode.length === 0 && <span className="hint">{t('还没有入站')}</span>}
            </div>
          </Field>
          <div className="dfoot">
            <button className="btn" type="button" onClick={() => setEdit(null)}>{t('返回')}</button>
            <button className="btn pri" type="button" disabled={busy || !edit.name} onClick={() => run(async () => {
              if (edit.id) await put('/groups/' + edit.id, edit)
              else await post('/groups', edit)
              setEdit(null)
              groups.reload()
              onChanged()
            }, t('已保存'))}>{t('保存')}</button>
          </div>
        </>
      )}
    </Modal>
  )
}
