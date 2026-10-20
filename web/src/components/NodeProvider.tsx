import { useState } from 'react'
import { del, get, post, put } from '../api'
import { Field, Modal, Switch, useAction, useConfirm, useLoad } from './ui'
import { ago, bytes, datetime } from '../format'
import { t } from '../i18n'

// Hosting provider API (BandwagonHost / KiwiVM) on the node page: VPS state,
// the month's traffic as the provider counts it, and optional sync of the
// node's traffic cycle to it.

type Info = {
  plan: string; location: string; os: string; hostname: string; ips: string[]
  ram: number; disk: number; dataUsed: number; dataLimit: number; multiplier: number; nextReset: string; suspended: boolean
  status?: string; diskUsed?: number; memAvail?: number; load?: string; throttled?: boolean; fetched: string
}
type Provider = { nodeId: string; kind: string; veid: string; sync: boolean; keySet: boolean; lastSync?: string; lastError: string; info?: Info }
type Resp = { configured: boolean; provider?: Provider }

export default function NodeProvider({ nodeId, onSynced }: { nodeId: string; onSynced?: () => void }) {
  const r = useLoad(() => get<Resp>(`/nodes/${nodeId}/provider`), [nodeId])
  const [editing, setEditing] = useState(false)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const p = r.data?.provider
  const in_ = p?.info
  const pct = in_ && in_.dataLimit ? Math.min(100, (100 * in_.dataUsed) / in_.dataLimit) : 0
  return (
    <section className="card">
      <div className="ctitle">
        <span>{t('服务商 API')} <span className="sub" style={{ margin: 0, fontWeight: 400 }}>{t('搬瓦工（KiwiVM）')}</span></span>
        <div className="row">
          {p && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => {
            await post(`/nodes/${nodeId}/provider/refresh`); r.reload(); onSynced?.()
          }, t('已刷新'))}>{t('刷新')}</button>}
          <button className="btn sm" type="button" onClick={() => setEditing(true)}>{p ? t('设置') : t('接入')}</button>
          {p && <button className="btn sm danger" type="button" onClick={async () => {
            if (await confirm(t('移除此节点的服务商 API 设置？节点已有的流量额度和重置日保持不变。'), { danger: true, ok: t('移除') }))
              run(async () => { await del(`/nodes/${nodeId}/provider`); r.reload() }, t('已移除'))
          }}>{t('移除')}</button>}
        </div>
      </div>
      {!p ? (
        <div className="sub" style={{ margin: 0 }}>{t('填入搬瓦工 KiwiVM 的 VEID 和 API Key，即可在这里查看 VPS 状态和服务商统计的本月流量，并可让节点流量额度、重置日和已用量自动跟随服务商。')}</div>
      ) : (
        <>
          {p.lastError && <div className="note bad" style={{ marginBottom: 8 }}>{t('最近一次读取失败：')}{p.lastError}</div>}
          {in_ && <>
            <div className="small" style={{ marginBottom: 6 }}>
              {t('本月流量')} <b className="mono">{bytes(in_.dataUsed)}</b> / <span className="mono">{bytes(in_.dataLimit)}</span>
              {in_.multiplier && in_.multiplier !== 1 ? <span className="mute">{t('（计费倍率 ×{m}）', { m: in_.multiplier })}</span> : null}
              {in_.nextReset && <span className="mute"> · {t('{at} 重置', { at: datetime(in_.nextReset) })}</span>}
            </div>
            <div className="bar" style={{ height: 6, borderRadius: 3, background: 'var(--line)', overflow: 'hidden', marginBottom: 10 }}>
              <div style={{ width: pct + '%', height: '100%', background: pct >= 100 ? 'var(--bad)' : pct >= 80 ? 'var(--warn)' : 'var(--ok)' }} />
            </div>
            <div className="kv small" style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(160px, 1fr))', gap: '6px 16px' }}>
              <div><span className="mute">{t('状态')}　</span>{in_.suspended ? <b style={{ color: 'var(--bad)' }}>{t('已暂停')}</b> : in_.status || t('正常')}{in_.throttled ? ' · ' + t('CPU 受限') : ''}</div>
              <div><span className="mute">{t('套餐')}　</span>{in_.plan || '—'}</div>
              <div><span className="mute">{t('机房')}　</span>{in_.location || '—'}</div>
              <div><span className="mute">{t('系统')}　</span>{in_.os || '—'}</div>
              <div><span className="mute">{t('内存')}　</span>{in_.memAvail ? t('{free} 可用 / {all}', { free: bytes(in_.memAvail), all: bytes(in_.ram) }) : bytes(in_.ram)}</div>
              <div><span className="mute">{t('硬盘')}　</span>{in_.diskUsed ? t('{used} 已用 / {all}', { used: bytes(in_.diskUsed), all: bytes(in_.disk) }) : bytes(in_.disk)}</div>
              {in_.load && <div><span className="mute">{t('负载')}　</span><span className="mono">{in_.load}</span></div>}
              {in_.ips?.length > 0 && <div><span className="mute">IP　</span><span className="mono">{in_.ips.join(', ')}</span></div>}
            </div>
          </>}
          <div className="sub" style={{ margin: '8px 0 0' }}>
            VEID <span className="mono">{p.veid}</span> · {p.sync ? t('节点流量跟随服务商') : t('只查看，不改节点流量设置')}
            {p.lastSync && ' · ' + t('{ago}读取', { ago: ago(p.lastSync) })}
          </div>
        </>
      )}
      {editing && <ProviderForm nodeId={nodeId} p={p} onClose={() => setEditing(false)} onSaved={() => { setEditing(false); r.reload(); onSynced?.() }} />}
    </section>
  )
}

function ProviderForm({ nodeId, p, onClose, onSaved }: { nodeId: string; p?: Provider; onClose: () => void; onSaved: () => void }) {
  const [veid, setVeid] = useState(p?.veid ?? '')
  const [key, setKey] = useState('')
  const [sync, setSync] = useState(p?.sync ?? true)
  const { busy, run } = useAction()
  return (
    <Modal title={t('搬瓦工 API')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !veid || (!key && !p?.keySet)} onClick={() => run(async () => {
        await put(`/nodes/${nodeId}/provider`, { kind: 'bandwagon', veid, apiKey: key, sync }); onSaved()
      }, t('已保存'))}>{t('检查并保存')}</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>{t('在 KiwiVM 控制面板左侧「API」页面生成 API Key，同页可看到 VEID。只调用只读接口，每 15 分钟读取一次。')}</div>
      <Field label="VEID" htmlFor="bwh-veid"><input id="bwh-veid" className="input mono" value={veid} onChange={(e) => setVeid(e.target.value)} placeholder="1234567" /></Field>
      <Field label="API Key" htmlFor="bwh-key" hint={p?.keySet ? t('已保存；留空则不修改') : undefined}>
        <input id="bwh-key" className="input mono" type="password" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)} placeholder={p?.keySet ? '••••••••' : 'private_xxxxxxxx'} />
      </Field>
      <Field label={t('同步节点流量')} hint={t('开启后节点的月流量额度、重置日和本周期已用量按服务商的数据校准（含计费倍率），超额提醒也以此为准')}>
        <label className="row small"><Switch label={t('同步节点流量')} on={sync} onChange={setSync} />{t('流量额度和已用量跟随服务商')}</label>
      </Field>
    </Modal>
  )
}
