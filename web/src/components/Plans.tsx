import { useState } from 'react'
import { del, get, post, put } from '../api'
import { useNodes } from '../App'
import { ProTag, useFeature } from './License'
import { Field, Modal, Switch, useAction, useConfirm, useLoad } from './ui'
import { bytes, GB } from '../format'
import type { Group, Node } from '../types'
import { t } from '../i18n'

// Plans are groups sold as packages: the level and nodes decide the
// servers, the limits are set on the user when the plan is applied.

export function planNodes(p: Group, nodes: Node[]) {
  return nodes.filter((n) => ((n.level ?? 0) > 0 && (n.level ?? 0) <= p.level) || (p.nodeIds ?? []).includes(n.id))
}

export function planSummary(p: Group) {
  return [
    p.trafficLimit ? bytes(p.trafficLimit) : t('不限流量'),
    p.days ? t('{n} 天', { n: p.days }) : t('不限期'),
    p.deviceLimit ? t('{n} 台设备', { n: p.deviceLimit }) : '',
    p.speedUpMbps || p.speedDownMbps ? `↑${p.speedUpMbps || '∞'} ↓${p.speedDownMbps || '∞'} Mbps` : '',
    p.fwdMax ? t('{n} 条转发', { n: p.fwdMax }) : '',
  ].filter(Boolean).join(' · ')
}

const resetText = (p: string | undefined, day: number | undefined) => ({
  daily: t('每日重置'), weekly: t('每周重置'), monthly: t('每月 {n} 日重置', { n: day || 1 }), cycle: t('每 {n} 天重置', { n: day || 1 }),
}[p as 'daily'] ?? '')

type Edit = {
  id?: string; name: string; price: string; sort: number; level: number; nodeIds: string[]; limitGB: string; days: string
  resetPolicy: string; resetDay: string; deviceLimit: string; ipLimit: string; speedUp: string; speedDown: string; fallbackId: string
  sale: boolean; priceStars: string; priceCny: string; priceUsdt: string
  nodeRates: Record<string, string>
  fwdMax: string; fwdPortLo: string; fwdPortHi: string; fwdGB: string; fwdRate: string; fwdSpeed: string
}

const blank: Edit = { name: '', price: '', sort: 0, level: 1, nodeIds: [], limitGB: '', days: '30', resetPolicy: 'none', resetDay: '1', deviceLimit: '0', ipLimit: '0', speedUp: '', speedDown: '', fallbackId: '', sale: false, priceStars: '', priceCny: '', priceUsdt: '', nodeRates: {}, fwdMax: '', fwdPortLo: '', fwdPortHi: '', fwdGB: '', fwdRate: '', fwdSpeed: '' }

function toEdit(p: Group): Edit {
  return {
    id: p.id, name: p.name, price: p.price ?? '', sort: p.sort ?? 0, level: p.level, nodeIds: p.nodeIds ?? [],
    limitGB: p.trafficLimit ? String(+(p.trafficLimit / GB).toFixed(2)) : '', days: p.days ? String(p.days) : '',
    resetPolicy: p.resetPolicy || 'none', resetDay: String(p.resetDay || 1), deviceLimit: String(p.deviceLimit ?? 0), ipLimit: String(p.ipLimit ?? 0),
    speedUp: p.speedUpMbps ? String(p.speedUpMbps) : '', speedDown: p.speedDownMbps ? String(p.speedDownMbps) : '', fallbackId: p.fallbackId ?? '',
    sale: !!p.sale, priceStars: p.priceStars ? String(p.priceStars) : '', priceCny: p.priceCny ? String(p.priceCny) : '', priceUsdt: p.priceUsdt ? String(p.priceUsdt) : '',
    nodeRates: Object.fromEntries(Object.entries(p.nodeRates ?? {}).map(([k, v]) => [k, String(v)])),
    fwdMax: p.fwdMax ? String(p.fwdMax) : '', fwdPortLo: p.fwdPortLo ? String(p.fwdPortLo) : '', fwdPortHi: p.fwdPortHi ? String(p.fwdPortHi) : '',
    fwdGB: p.fwdTraffic ? String(+(p.fwdTraffic / GB).toFixed(2)) : '', fwdRate: p.fwdRate ? String(p.fwdRate) : '', fwdSpeed: p.fwdSpeedMbps ? String(p.fwdSpeedMbps) : '',
  }
}

// planPresets are starting points for new plans, airport style: Lv.1 plain
// lines, Lv.2 better lines, Lv.3 premium lines (a level includes the lower
// ones). Paid plans fall back to the free plan when they run out.
const planPresets: { name: string; note: string; fallback?: boolean; e: Partial<Edit> }[] = [
  { name: t('免费版'), note: t('不限期 · 每月 5 GB · 限速 10 Mbps'), e: { level: 1, limitGB: '5', days: '', resetPolicy: 'monthly', resetDay: '1', deviceLimit: '1', speedUp: '10', speedDown: '10' } },
  { name: t('体验版'), note: t('3 天 · 10 GB，适合配兑换码'), fallback: true, e: { level: 1, limitGB: '10', days: '3', deviceLimit: '2' } },
  { name: t('基础月付'), note: t('Lv.1 · 30 天 · 100 GB · ¥10'), fallback: true, e: { level: 1, limitGB: '100', days: '30', deviceLimit: '3', sale: true, priceCny: '10', price: t('¥{n}/月', { n: 10 }) } },
  { name: t('标准月付'), note: t('Lv.2 · 30 天 · 300 GB · ¥20'), fallback: true, e: { level: 2, limitGB: '300', days: '30', deviceLimit: '5', sale: true, priceCny: '20', price: t('¥{n}/月', { n: 20 }) } },
  { name: t('高级月付'), note: t('Lv.3 · 30 天 · 1000 GB · ¥40'), fallback: true, e: { level: 3, limitGB: '1000', days: '30', deviceLimit: '8', sale: true, priceCny: '40', price: t('¥{n}/月', { n: 40 }) } },
  { name: t('标准年付'), note: t('Lv.2 · 365 天 · 每 30 天 300 GB · ¥200'), fallback: true, e: { level: 2, limitGB: '300', days: '365', resetPolicy: 'cycle', resetDay: '30', deviceLimit: '5', sale: true, priceCny: '200', price: t('¥{n}/年', { n: 200 }) } },
  { name: t('不限时流量'), note: t('Lv.2 · 500 GB 用完为止 · ¥50'), e: { level: 2, limitGB: '500', days: '', deviceLimit: '5', sale: true, priceCny: '50', price: '¥50' } },
]

function planBody(e: Edit) {
  return {
    name: e.name.trim(), price: e.price.trim(), sort: Number(e.sort) || 0, level: e.level, nodeIds: e.nodeIds,
    trafficLimit: Math.round(Number(e.limitGB || 0) * GB), days: Number(e.days) || 0,
    resetPolicy: e.resetPolicy === 'none' ? '' : e.resetPolicy, resetDay: Number(e.resetDay) || 0,
    deviceLimit: Number(e.deviceLimit) || 0, ipLimit: Number(e.ipLimit) || 0,
    speedUpMbps: Number(e.speedUp) || 0, speedDownMbps: Number(e.speedDown) || 0, fallbackId: e.fallbackId,
    sale: e.sale, priceStars: Math.round(Number(e.priceStars) || 0), priceCny: Number(e.priceCny) || 0, priceUsdt: Number(e.priceUsdt) || 0,
    nodeRates: Object.fromEntries(Object.entries(e.nodeRates).map(([k, v]) => [k, Number(v) || 0]).filter(([, v]) => (v as number) > 0)),
    fwdMax: Math.round(Number(e.fwdMax) || 0), fwdPortLo: Math.round(Number(e.fwdPortLo) || 0), fwdPortHi: Math.round(Number(e.fwdPortHi) || 0),
    fwdTraffic: Math.round(Number(e.fwdGB || 0) * GB), fwdRate: Number(e.fwdRate) || 0, fwdSpeedMbps: Math.round(Number(e.fwdSpeed) || 0),
  }
}

// PlansPanel is the plans page body: plan cards, the node matrix and the
// plan editor (a dialog over the page).
export function PlansPanel({ onChanged }: { onChanged?: () => void }) {
  const speedOK = useFeature('speed_limit')
  const addonsOK = useFeature('plan_addons')
  const plans = useLoad(() => get<Group[]>('/plans'))
  const [edit, setEdit] = useState<Edit | null>(null)
  const onClose = () => setEdit(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const { nodes } = useNodes()
  const list = plans.data ?? []
  const set = (k: keyof Edit, v: unknown) => setEdit((e) => (e ? { ...e, [k]: v } : e))

  const freeID = () => list.find((p) => p.name === t('免费版'))?.id ?? ''
  const fromPreset = (i: number) => {
    const pr = planPresets[i]
    setEdit({ ...blank, ...pr.e, name: pr.name, sort: list.length, fallbackId: pr.fallback ? freeID() : '' })
  }
  // createPresets adds every preset not there yet, the free plan first so
  // the others can fall back to it.
  const createPresets = async () => {
    const missing = planPresets.filter((pr) => !list.some((p) => p.name === pr.name))
    if (!missing.length || !await confirm(t('创建 {n} 个推荐套餐：{names}？价格和流量之后都可以修改；在售的套餐需要配置付款方式后才能购买。', { n: missing.length, names: missing.map((pr) => pr.name).join('、') }), { ok: t('创建') })) return
    run(async () => {
      let free = freeID()
      let sort = list.length
      for (const pr of missing) {
        const created = await post<Group>('/plans', planBody({ ...blank, ...pr.e, name: pr.name, sort: sort++, fallbackId: pr.fallback ? free : '' }))
        if (pr.name === t('免费版')) free = created.id
      }
      plans.reload()
      onChanged?.()
    }, t('已创建'))
  }

  const save = (e: Edit) => run(async () => {
    const body = planBody(e)
    if (e.id) await put('/plans/' + e.id, body)
    else await post('/plans', body)
    setEdit(null)
    plans.reload()
    onChanged?.()
  }, t('已保存'))

  if (edit) {
    const reach = planNodes({ id: '', name: '', remark: '', inboundIds: [], userCount: 0, level: edit.level, nodeIds: edit.nodeIds }, nodes)
    return (
      <Modal title={edit.id ? t('编辑套餐 · {name}', { name: edit.name }) : t('新建套餐')} onClose={onClose} wide footer={<>
        <button className="btn" type="button" onClick={() => setEdit(null)}>{t('返回')}</button>
        <button className="btn pri" type="button" disabled={busy || !edit.name.trim()} onClick={() => save(edit)}>{t('保存')}</button>
      </>}>
        {!edit.id && (
          <Field label={t('从模板开始')} hint={t('点一下自动填好，再按需修改；Lv.1 普通线路、Lv.2 优质线路、Lv.3 专线')}>
            <div className="chips" style={{ flexWrap: 'wrap' }}>
              {planPresets.map((pr, i) => <button key={pr.name} type="button" className={'chip' + (edit.name === pr.name ? ' on' : '')} title={pr.note} onClick={() => fromPreset(i)}>{pr.name}</button>)}
            </div>
          </Field>
        )}
        <div className="form-grid">
          <Field label={t('名称')} htmlFor="p-name"><input id="p-name" className="input" value={edit.name} onChange={(e) => set('name', e.target.value)} placeholder={t('例如 专业版')} /></Field>
          <Field label={t('售价')} htmlFor="p-price" hint={t('只用于展示，支付插件也会读取')}><input id="p-price" className="input" value={edit.price} onChange={(e) => set('price', e.target.value)} placeholder="¥15" /></Field>
          <Field label={t('流量 (GB)')} htmlFor="p-limit" hint={t('留空或 0 为不限')}><input id="p-limit" className="input mono" inputMode="decimal" value={edit.limitGB} onChange={(e) => set('limitGB', e.target.value)} /></Field>
          <Field label={t('时长（天）')} htmlFor="p-days" hint={t('留空或 0 为不限期，适合免费版')}><input id="p-days" className="input mono" inputMode="numeric" value={edit.days} onChange={(e) => set('days', e.target.value)} /></Field>
          <Field label={t('流量重置')} htmlFor="p-reset">
            <select id="p-reset" className="input" value={edit.resetPolicy} onChange={(e) => set('resetPolicy', e.target.value)}>
              <option value="none">{t('不重置')}</option><option value="daily">{t('每天')}</option><option value="weekly">{t('每周固定日')}</option>
              <option value="monthly">{t('每月固定日')}</option><option value="cycle">{t('按周期滚动')}</option>
            </select>
          </Field>
          {edit.resetPolicy !== 'none' && edit.resetPolicy !== 'daily' && (
            <Field label={edit.resetPolicy === 'weekly' ? t('每周几（1–7）') : edit.resetPolicy === 'monthly' ? t('每月几号') : t('周期（天）')} htmlFor="p-rday">
              <input id="p-rday" className="input mono" inputMode="numeric" value={edit.resetDay} onChange={(e) => set('resetDay', e.target.value)} />
            </Field>
          )}
          <Field label={t('设备数（HWID）')} htmlFor="p-dev" hint={t('0 为不限')}><input id="p-dev" className="input mono" inputMode="numeric" value={edit.deviceLimit} onChange={(e) => set('deviceLimit', e.target.value)} /></Field>
          <Field label={t('同时在线 IP 数')} htmlFor="p-ip" hint={t('0 为不限')}><input id="p-ip" className="input mono" inputMode="numeric" value={edit.ipLimit} onChange={(e) => set('ipLimit', e.target.value)} /></Field>
          <Field label={<>{t('限速 上行 / 下行 (Mbps)')}<ProTag on={speedOK} /></>} hint={t('留空为不限')}>
            <div className="row" style={{ flexWrap: 'nowrap' }}>
              <input aria-label={t('上行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedUp} placeholder={t('上行')} onChange={(e) => set('speedUp', e.target.value)} />
              <input aria-label={t('下行 Mbps')} disabled={!speedOK} className="input mono" inputMode="numeric" value={edit.speedDown} placeholder={t('下行')} onChange={(e) => set('speedDown', e.target.value)} />
            </div>
          </Field>
          <Field label={t('到期后')} htmlFor="p-fallback" hint={t('到期时自动换成这个套餐（例如免费版），不选则账号到期停用')}>
            <select id="p-fallback" className="input" value={edit.fallbackId} onChange={(e) => set('fallbackId', e.target.value)}>
              <option value="">{t('停用账号')}</option>
              {list.filter((p) => p.id !== edit.id).map((p) => <option key={p.id} value={p.id}>{t('降级为 {name}', { name: p.name })}</option>)}
            </select>
          </Field>
          <Field label={t('节点等级')} htmlFor="p-level" hint={t('可用等级 1 到该等级的全部节点（在节点设置里给节点定等级）；0 表示不按等级')}>
            <input id="p-level" className="input mono" type="number" min={0} max={99} value={edit.level} onChange={(e) => set('level', Math.max(0, Math.min(99, Number(e.target.value) || 0)))} />
          </Field>
          <Field label={t('排序')} htmlFor="p-sort" hint={t('小的排前面')}><input id="p-sort" className="input mono" type="number" value={edit.sort} onChange={(e) => set('sort', Number(e.target.value) || 0)} /></Field>
        </div>
        <Field label={t('另外授权整个节点')} hint={t('不论等级，套餐用户可用这些节点')}>
          <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
            {nodes.map((n) => {
              const on = edit.nodeIds.includes(n.id)
              return <button key={n.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
                onClick={() => set('nodeIds', on ? edit.nodeIds.filter((x) => x !== n.id) : [...edit.nodeIds, n.id])}>{n.name}{(n.level ?? 0) > 0 && <span className="mute small" style={{ marginLeft: 4 }}>Lv.{n.level}</span>}</button>
            })}
            {nodes.length === 0 && <span className="hint">{t('还没有节点')}</span>}
          </div>
        </Field>
        <div className="toggle"><span>{t('在 Telegram 机器人售卖（价格留空表示不支持该付款方式；付款方式在「用户 → 售卖 → 支付设置」里开启）')}</span><Switch label={t('售卖')} on={edit.sale} onChange={(v) => set('sale', v)} /></div>
        {edit.sale && (
          <div className="form-grid">
            <Field label={t('Telegram Stars 价格')} htmlFor="p-stars" hint={t('整数，单位 ⭐')}><input id="p-stars" className="input mono" inputMode="numeric" value={edit.priceStars} onChange={(e) => set('priceStars', e.target.value)} /></Field>
            <Field label={t('易支付价格（元）')} htmlFor="p-cny"><input id="p-cny" className="input mono" inputMode="decimal" value={edit.priceCny} onChange={(e) => set('priceCny', e.target.value)} /></Field>
            <Field label={t('USDT 价格')} htmlFor="p-usdt" hint={t('实付会多 0.0001–0.05 用于对账')}><input id="p-usdt" className="input mono" inputMode="decimal" value={edit.priceUsdt} onChange={(e) => set('priceUsdt', e.target.value)} /></Field>
          </div>
        )}
        <div className="note">{t('可用节点：')}{reach.map((n) => n.name).join('、') || t('暂无（给节点设置等级，或在上面选择节点）')}</div>
        {reach.length > 0 && (
          <Field label={<>{t('套餐专属节点倍率')}<ProTag on={addonsOK} /></>} hint={t('这个套餐的用户在该节点按此倍率扣流量，替代节点自己的倍率；留空 = 用节点倍率')}>
            <div className="form-grid">
              {reach.map((n) => (
                <label key={n.id} className="row" style={{ gap: 8, flexWrap: 'nowrap' }}>
                  <span style={{ minWidth: 0, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.name}</span>
                  <input className="input mono" style={{ width: 110 }} inputMode="decimal" disabled={!addonsOK} aria-label={t('{node} 的倍率', { node: n.name })}
                    placeholder={'×' + (n.trafficRate || 1)} value={edit.nodeRates[n.id] ?? ''}
                    onChange={(e) => set('nodeRates', { ...edit.nodeRates, [n.id]: e.target.value })} />
                </label>
              ))}
            </div>
          </Field>
        )}
        <Field label={<>{t('端口转发')}<ProTag on={addonsOK} /></>} hint={t('用户可在用户中心自助添加端口转发（GOST），只能用在上面这些可用节点上；转发流量单独计算，用完后转发暂停、代理不受影响。条数为 0 = 不开放。')}>
          <div className="form-grid">
            <Field label={t('最多条数')} htmlFor="p-fwd-max"><input id="p-fwd-max" className="input mono" inputMode="numeric" disabled={!addonsOK} placeholder="0" value={edit.fwdMax} onChange={(e) => set('fwdMax', e.target.value)} /></Field>
            <Field label={t('每周期转发流量 (GB)')} htmlFor="p-fwd-gb" hint={t('留空为不限')}><input id="p-fwd-gb" className="input mono" inputMode="decimal" disabled={!addonsOK} value={edit.fwdGB} onChange={(e) => set('fwdGB', e.target.value)} /></Field>
            <Field label={t('端口范围')} hint={t('留空为 10000–60000')}>
              <div className="row" style={{ flexWrap: 'nowrap', gap: 6 }}>
                <input className="input mono" inputMode="numeric" aria-label={t('起始端口')} disabled={!addonsOK} placeholder="10000" value={edit.fwdPortLo} onChange={(e) => set('fwdPortLo', e.target.value)} />
                <span className="mute">–</span>
                <input className="input mono" inputMode="numeric" aria-label={t('结束端口')} disabled={!addonsOK} placeholder="60000" value={edit.fwdPortHi} onChange={(e) => set('fwdPortHi', e.target.value)} />
              </div>
            </Field>
            <Field label={t('转发倍率')} htmlFor="p-fwd-rate" hint={t('在节点倍率之上再乘；留空为 1')}><input id="p-fwd-rate" className="input mono" inputMode="decimal" disabled={!addonsOK} placeholder="1" value={edit.fwdRate} onChange={(e) => set('fwdRate', e.target.value)} /></Field>
            <Field label={t('每条限速 (Mbps)')} htmlFor="p-fwd-speed" hint={t('上下行各自限速；留空为不限')}><input id="p-fwd-speed" className="input mono" inputMode="numeric" disabled={!addonsOK} value={edit.fwdSpeed} onChange={(e) => set('fwdSpeed', e.target.value)} /></Field>
          </div>
        </Field>
        {edit.id && <div className="sub">{t('修改后：已开通的用户立即按新的节点和限速生效；流量、到期时间在下次开通或续费时更新。')}</div>}
      </Modal>
    )
  }

  return (
    <>
      <div className="top">
        <div><h1>{t('套餐')}</h1><div className="sub">{t('像机场一样分级售卖：套餐决定可用哪些服务器（按节点等级）和流量、时长、设备数。开通或换套餐时已用流量清零，续费同一套餐顺延到期时间。')}</div></div>
        <div className="row"><button className="btn pri" type="button" onClick={() => setEdit({ ...blank, sort: list.length })}>{t('+ 新建套餐')}</button></div>
      </div>
      {plans.data && list.length === 0 && <div className="note row" style={{ justifyContent: 'space-between' }}>
        <span>{t('还没有套餐。常见做法：先建一个不限期的「免费版」（Lv.1，少量流量），再建付费套餐并把「到期后」设为降级到免费版。')}</span>
        <button className="btn sm pri" type="button" disabled={busy} onClick={createPresets}>{t('一键创建推荐套餐')}</button>
      </div>}
      <div className="plans">
        {list.map((p) => {
          const reach = planNodes(p, nodes)
          return (
            <div key={p.id} className="card plan-card">
              <div className="row" style={{ justifyContent: 'space-between', alignItems: 'baseline' }}>
                <div style={{ fontWeight: 600, fontSize: 16 }}>{p.name}</div>
                {p.price && <div className="plan-price">{p.price}</div>}
              </div>
              <div className="row" style={{ gap: 6 }}>
                {p.sale ? <span className="tag ok">{t('在售')}</span> : null}
                {p.sale && <span className="small mute">{[p.priceCny ? `¥${p.priceCny}` : '', p.priceStars ? `${p.priceStars} ⭐` : '', p.priceUsdt ? `${p.priceUsdt} USDT` : ''].filter(Boolean).join(' · ')}</span>}
              </div>
              <div className="row" style={{ gap: 6 }}>
                {p.level > 0 && <span className="tag info-tag">Lv.{p.level}</span>}
                <span className="small">{planSummary(p)}</span>
              </div>
              <div className="small mute">{[resetText(p.resetPolicy, p.resetDay), p.fallbackName ? t('到期后降级为 {name}', { name: p.fallbackName }) : p.days ? t('到期后停用') : ''].filter(Boolean).join(' · ')}</div>
              <div className="small" title={reach.map((n) => n.name).join('、')}>{t('{n} 个节点', { n: reach.length })}{reach.length > 0 && <span className="mute">：{reach.slice(0, 6).map((n) => n.name).join('、')}{reach.length > 6 ? '…' : ''}</span>}</div>
              <div className="row" style={{ justifyContent: 'space-between', marginTop: 'auto' }}>
                <span className="small mute">{t('{n} 个用户', { n: p.userCount })}</span>
                <span>
                  <button className="link" type="button" onClick={() => setEdit(toEdit(p))}>{t('编辑')}</button>
                  <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                    if (await confirm(t('删除套餐 {name}？已开通的用户保留当前流量和到期时间，但失去套餐里的节点。', { name: p.name }), { danger: true })) run(async () => { await del('/plans/' + p.id); plans.reload(); onChanged?.() })
                  }}>{t('删除')}</button>
                </span>
              </div>
            </div>
          )
        })}
      </div>
      {list.length > 0 && nodes.length > 0 && (
        <div className="card flush scroll">
          <table>
            <thead><tr><th>{t('节点')}</th><th>{t('等级')}</th>{list.map((p) => <th key={p.id} style={{ textAlign: 'center' }}>{p.name}</th>)}</tr></thead>
            <tbody>
              {nodes.map((n) => (
                <tr key={n.id}>
                  <td>{n.name}</td><td>{(n.level ?? 0) > 0 ? <span className="tag info-tag">Lv.{n.level}</span> : <span className="mute">—</span>}</td>
                  {list.map((p) => <td key={p.id} style={{ textAlign: 'center' }}>{planNodes(p, [n]).length ? <span style={{ color: 'var(--ok)' }} aria-label={t('可用')}>✓</span> : <span className="mute">·</span>}</td>)}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// UserPlanModal starts or renews a plan for one user or a selection.
export function UserPlanModal({ ids, current, onClose, onDone }: { ids: string[]; current?: string; onClose: () => void; onDone: () => void }) {
  const plans = useLoad(() => get<Group[]>('/plans'))
  const [planId, setPlanId] = useState(current ?? '')
  const [mode, setMode] = useState<'renew' | 'switch'>('renew')
  const { busy, run } = useAction()
  const p = plans.data?.find((x) => x.id === planId)
  const same = !!current && current === planId
  return (
    <Modal title={ids.length === 1 ? t('开通 / 续费套餐') : t('为 {n} 个用户开通套餐', { n: ids.length })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !planId} onClick={() => run(async () => {
        if (ids.length === 1) await post(`/users/${ids[0]}/plan`, { planId, mode })
        else await post('/users/bulk', { action: 'plan', ids, planId, mode })
        onDone()
      }, t('已开通'))}>{same && mode === 'renew' ? t('续费') : t('开通')}</button>
    </>}>
      {plans.data && plans.data.length === 0 && <div className="note">{t('还没有套餐，请先在「套餐」里新建。')}</div>}
      <Field label={t('套餐')} htmlFor="up-plan">
        <select id="up-plan" className="input" value={planId} onChange={(e) => setPlanId(e.target.value)}>
          <option value="">{t('选择套餐')}</option>
          {(plans.data ?? []).map((x) => <option key={x.id} value={x.id}>{x.name}{x.price ? ` · ${x.price}` : ''}</option>)}
        </select>
      </Field>
      {p && <div className="note">{planSummary(p)}</div>}
      <Field label={t('方式')}>
        <div className="seg">
          <button type="button" className={'opt' + (mode === 'renew' ? ' on' : '')} onClick={() => setMode('renew')}>{t('续费 / 开通')}<small>{t('已是这个套餐则顺延到期时间、保留已用流量；否则从今天开始')}</small></button>
          <button type="button" className={'opt' + (mode === 'switch' ? ' on' : '')} onClick={() => setMode('switch')}>{t('重新开始')}<small>{t('从今天重新计时，已用流量清零')}</small></button>
        </div>
      </Field>
      <div className="sub">{t('开通会把流量、到期、设备数和限速设为套餐的值；换套餐时已用流量清零。')}</div>
    </Modal>
  )
}
