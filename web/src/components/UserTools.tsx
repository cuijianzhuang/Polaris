// User management tools modelled on 3x-ui: bulk create, bulk adjust,
// import / export and manual traffic correction.
import { useState } from 'react'
import { post, put } from '../api'
import { bytes, GB } from '../format'
import type { Group, User } from '../types'
import { Field, Modal, Switch, useAction } from './ui'
import { t } from '../i18n'

const pad = (n: number, w: number) => String(n).padStart(w, '0')

export function BulkCreateModal({ groups, onClose, onDone }: { groups: Group[]; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({
    method: 'seq', prefix: 'user', postfix: '', start: '1', count: '10', limitGB: '100', days: '30', firstUse: false,
    ipLimit: '0', deviceLimit: '0', resetPolicy: 'none', groupIds: groups.filter((g) => g.isDefault).map((g) => g.id),
  })
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  const count = Math.max(0, Math.min(1000, Number(f.count) || 0)), start = Math.max(1, Number(f.start) || 1)
  const w = Math.max(3, String(start + count - 1).length)
  const sample = f.method === 'random' ? `${f.prefix}k3x9qa${f.postfix}` : count ? `${f.prefix}${pad(start, w)}${f.postfix} … ${f.prefix}${pad(start + count - 1, w)}${f.postfix}` : '—'
  return (
    <Modal title={t('批量创建用户')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !count} onClick={() => run(async () => {
        const days = Number(f.days) || 0
        const exp = new Date(); exp.setDate(exp.getDate() + days)
        const r = await post<{ affected: number; skipped?: string[] }>('/users/bulk', {
          action: 'create', method: f.method, prefix: f.prefix, postfix: f.postfix, start, count, template: {
            username: 'x', trafficLimit: Math.round(Number(f.limitGB || 0) * GB),
            expireAt: days && !f.firstUse ? exp.toISOString() : null, expireAfterDays: days && f.firstUse ? days : 0,
            ipLimit: Number(f.ipLimit) || 0, deviceLimit: Number(f.deviceLimit) || 0, resetPolicy: f.resetPolicy, resetDay: 1, groupIds: f.groupIds,
          },
        })
        if (r.skipped?.length) alert(t('已创建 {n} 个；以下用户名已存在，已跳过：', { n: r.affected }) + `\n${r.skipped.slice(0, 50).join(t('、'))}${r.skipped.length > 50 ? ' …' : ''}`)
        onDone()
      }, t('已创建'))}>{t('创建 {count} 个', { count })}</button>
    </>}>
      <Field label={t('命名方式')}>
        <div className="seg">
          <button type="button" className={'opt' + (f.method === 'seq' ? ' on' : '')} onClick={() => set('method', 'seq')}>{t('前缀 + 序号 + 后缀')}</button>
          <button type="button" className={'opt' + (f.method === 'random' ? ' on' : '')} onClick={() => set('method', 'random')}>{t('前缀 + 随机 + 后缀')}</button>
        </div>
      </Field>
      <div className="form-grid">
        <Field label={t('前缀')}><input className="input mono" value={f.prefix} onChange={(e) => set('prefix', e.target.value)} /></Field>
        <Field label={t('后缀')} hint={t('如 @shop')}><input className="input mono" value={f.postfix} onChange={(e) => set('postfix', e.target.value)} /></Field>
        {f.method === 'seq' && <Field label={t('起始序号')}><input className="input mono" inputMode="numeric" value={f.start} onChange={(e) => set('start', e.target.value)} /></Field>}
        <Field label={t('数量')} hint={t('最多 1000；已存在的用户名会跳过')}><input className="input mono" inputMode="numeric" value={f.count} onChange={(e) => set('count', e.target.value)} /></Field>
      </div>
      <div className="hint mono">{t('示例：')}{sample}</div>
      <div className="form-grid">
        <Field label={t('流量 (GB)')} hint={t('0 为不限')}><input className="input mono" value={f.limitGB} onChange={(e) => set('limitGB', e.target.value)} /></Field>
        <Field label={t('有效天数')} hint={t('0 为永不到期')}><input className="input mono" value={f.days} onChange={(e) => set('days', e.target.value)} /></Field>
        <Field label={t('同时在线 IP')} hint={t('0 为不限')}><input className="input mono" value={f.ipLimit} onChange={(e) => set('ipLimit', e.target.value)} /></Field>
        <Field label={t('设备数（HWID）')} hint={t('0 为不限')}><input className="input mono" value={f.deviceLimit} onChange={(e) => set('deviceLimit', e.target.value)} /></Field>
        <Field label={t('流量重置')}>
          <select className="input" value={f.resetPolicy} onChange={(e) => set('resetPolicy', e.target.value)}>
            <option value="none">{t('不重置')}</option><option value="daily">{t('每天')}</option><option value="weekly">{t('每周一')}</option><option value="monthly">{t('每月 1 号')}</option>
          </select>
        </Field>
        <Field label={t('首次使用后开始计时')} hint={t('产生第一笔流量时才开始算有效天数')}>
          <label className="row small"><Switch label={t('首次使用后开始计时')} on={f.firstUse} onChange={(v) => set('firstUse', v)} />{f.firstUse ? t('开启') : t('关闭')}</label>
        </Field>
      </div>
      <Field label={t('分组')}>
        <div className="row">{groups.map((g) => (
          <button key={g.id} type="button" className={'chip' + (f.groupIds.includes(g.id) ? ' on' : '')}
            onClick={() => set('groupIds', f.groupIds.includes(g.id) ? f.groupIds.filter((x) => x !== g.id) : [...f.groupIds, g.id])}>{g.name}</button>
        ))}</div>
      </Field>
    </Modal>
  )
}

/** Adjust many users at once: extend expiry, add traffic, set limits. */
export function AdjustModal({ ids, onClose, onDone }: { ids: string[]; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({ days: '30', addGB: '', setLimit: false, limitGB: '', setIP: false, ip: '0', setDev: false, dev: '0' })
  const set = (k: string, v: unknown) => setF((x) => ({ ...x, [k]: v }))
  const { busy, run } = useAction()
  return (
    <Modal title={t('调整 {n} 个用户', { n: ids.length })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        const days = Number(f.days) || 0, add = Math.round((Number(f.addGB) || 0) * GB)
        if (days || add) await post('/users/bulk', { action: 'extend', ids, days, addBytes: add })
        if (f.setLimit || f.setIP || f.setDev) {
          await post('/users/bulk', {
            action: 'set-limits', ids, ipLimit: f.setIP ? Number(f.ip) || 0 : undefined, deviceLimit: f.setDev ? Number(f.dev) || 0 : undefined,
            template: f.setLimit ? { username: 'x', trafficLimit: Math.round((Number(f.limitGB) || 0) * GB) } : undefined,
          })
        }
        onDone()
      }, t('已调整'))}>{t('应用')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('延长天数')} hint={t('从当前到期日（已过期则从今天）起算；永不到期的不变；负数为缩短')}><input className="input mono" value={f.days} onChange={(e) => set('days', e.target.value)} /></Field>
        <Field label={t('增加流量 (GB)')} hint={t('只对有配额的用户生效；负数为减少')}><input className="input mono" value={f.addGB} onChange={(e) => set('addGB', e.target.value)} placeholder="0" /></Field>
      </div>
      <div className="form-grid">
        <Field label={<label className="row small"><input type="checkbox" checked={f.setLimit} onChange={(e) => set('setLimit', e.target.checked)} />{t('设置流量配额 (GB)')}</label>}>
          <input className="input mono" disabled={!f.setLimit} value={f.limitGB} onChange={(e) => set('limitGB', e.target.value)} placeholder={t('0 为不限')} />
        </Field>
        <Field label={<label className="row small"><input type="checkbox" checked={f.setIP} onChange={(e) => set('setIP', e.target.checked)} />{t('设置同时在线 IP')}</label>}>
          <input className="input mono" disabled={!f.setIP} value={f.ip} onChange={(e) => set('ip', e.target.value)} />
        </Field>
        <Field label={<label className="row small"><input type="checkbox" checked={f.setDev} onChange={(e) => set('setDev', e.target.checked)} />{t('设置设备数')}</label>}>
          <input className="input mono" disabled={!f.setDev} value={f.dev} onChange={(e) => set('dev', e.target.value)} />
        </Field>
      </div>
    </Modal>
  )
}

/** Import users from a Polaris export or a 3x-ui client list. */
export function ImportModal({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [text, setText] = useState('')
  const [err, setErr] = useState('')
  const { busy, run } = useAction()
  const parse = (): unknown[] => {
    const v = JSON.parse(text)
    // Accept a list, {users: […]}, {clients: […]}, a 3x-ui inbound (settings.clients) or a list of inbounds.
    const pick = (x: any): any[] => {
      if (Array.isArray(x)) return x.flatMap((y) => (y && (y.settings || y.clients || y.users) ? pick(y) : [y]))
      if (x?.users) return x.users
      if (x?.clients) return x.clients
      if (x?.settings) return pick(typeof x.settings === 'string' ? JSON.parse(x.settings) : x.settings)
      if (x?.obj) return pick(x.obj)
      return []
    }
    return pick(v)
  }
  let n = 0
  try { n = text.trim() ? parse().length : 0 } catch { n = -1 }
  return (
    <Modal wide title={t('导入用户')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || n <= 0} onClick={() => run(async () => {
        setErr('')
        const r = await post<{ created: number; skipped?: string[] }>('/users/import', { users: parse() })
        if (r.skipped?.length) alert(t('已导入 {n} 个；以下用户名已存在或为空，已跳过：', { n: r.created }) + `\n${r.skipped.slice(0, 50).join(t('、'))}`)
        onDone()
      }, t('导入完成'))}>{n > 0 ? t('导入 {n} 个', { n }) : t('导入')}</button>
    </>}>
      <div className="hint">{t('粘贴或选择 JSON：本面板「导出」的文件，或 3x-ui 的客户端列表 / 入站 JSON（自动读取 settings.clients，字段 email、id、totalGB、expiryTime、limitIp、enable、tgId、subId、comment）。已存在的用户名会跳过；UUID、密码和订阅 token 会保留，客户端无需重新导入。')}</div>
      <input type="file" accept="application/json,.json" onChange={async (e) => { const fl = e.target.files?.[0]; if (fl) setText(await fl.text()) }} />
      <textarea className="input mono" style={{ minHeight: 220, fontSize: 12 }} value={text} onChange={(e) => setText(e.target.value)} placeholder='[{"email":"alice","id":"…","totalGB":107374182400,"expiryTime":0}]' />
      {n === -1 && <div className="note bad">{t('JSON 格式不正确')}</div>}
      {n === 0 && text.trim() && <div className="note warn">{t('没有找到用户')}</div>}
      {err && <div className="note bad">{err}</div>}
    </Modal>
  )
}

/** Correct a user's used traffic. */
export function TrafficEdit({ user, onSaved }: { user: User; onSaved: () => void }) {
  const [open, setOpen] = useState(false)
  const [up, setUp] = useState(String(+(user.usedUp / GB).toFixed(3)))
  const [down, setDown] = useState(String(+(user.usedDown / GB).toFixed(3)))
  const { busy, run } = useAction()
  if (!open) return <button className="btn sm" type="button" onClick={() => setOpen(true)}>{t('修正已用流量')}</button>
  return (
    <div className="row" style={{ gap: 6 }}>
      <span className="small">↑</span><input className="input mono" style={{ width: 100 }} value={up} onChange={(e) => setUp(e.target.value)} aria-label={t('上行 GB')} />
      <span className="small">↓</span><input className="input mono" style={{ width: 100 }} value={down} onChange={(e) => setDown(e.target.value)} aria-label={t('下行 GB')} />
      <span className="small mute">{t('GB · 当前 {v}', { v: bytes(user.usedUp + user.usedDown) })}</span>
      <button className="btn sm pri" type="button" disabled={busy} onClick={() => run(async () => {
        await put(`/users/${user.id}/traffic`, { usedUp: Math.round((Number(up) || 0) * GB), usedDown: Math.round((Number(down) || 0) * GB) })
        setOpen(false); onSaved()
      }, t('已修正'))}>{t('保存')}</button>
      <button className="btn sm" type="button" onClick={() => setOpen(false)}>{t('取消')}</button>
    </div>
  )
}

/** Save data as a JSON file in the browser. */
export function downloadJSON(name: string, data: unknown) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }))
  const a = document.createElement('a')
  a.href = url; a.download = name
  document.body.appendChild(a); a.click(); a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
