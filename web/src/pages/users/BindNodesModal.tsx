import { useState } from 'react'
import { useNodes } from '../../App'
import { Modal } from '../../components/ui'
import type { Node } from '../../types'
import { t } from '../../i18n'
import { toggle } from './shared'

/** Bulk bind / unbind whole nodes for the selected users. */
export function BindNodesModal({ count, detach, onClose, onSubmit }: { count: number; detach?: boolean; onClose: () => void; onSubmit: (ids: string[]) => void }) {
  const { nodes } = useNodes()
  const [ids, setIds] = useState<string[]>([])
  return (
    <Modal title={detach ? t('解绑节点 · {n} 个用户', { n: count }) : t('绑定节点 · {n} 个用户', { n: count })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className={'btn ' + (detach ? 'danger solid' : 'pri')} type="button" disabled={!ids.length} onClick={() => onSubmit(ids)}>{detach ? t('解绑') : t('绑定')}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{detach ? t('移除所选用户对这些节点的整台绑定（分组与单独入站授权不受影响）。') : t('所选用户获得这些节点上全部入站的使用权，节点以后新增的入站也自动生效。')}</div>
      <div className="row">
        {nodes.map((n: Node) => <button key={n.id} type="button" className={'chip' + (ids.includes(n.id) ? ' on' : '')} onClick={() => setIds(toggle(ids, n.id))}>{n.name}</button>)}
      </div>
    </Modal>
  )
}
