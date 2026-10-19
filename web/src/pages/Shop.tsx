import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { CopyBox, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { PlansModal } from '../components/Plans'
import { datetime } from '../format'
import { ProTag, useFeature } from '../components/License'
import { bytes, GB } from '../format'
import type { Group, TrafficPack } from '../types'
import { t } from '../i18n'

// Shop: plans sold in the Telegram bot — orders, redeem codes and the ways
// to pay.

type Order = {
  id: string; kind?: string; telegramId: number; tgName: string; userId: string; planName: string; method: string; amount: string; currency: string
  status: string; payRef?: string | null; detail: string; createdAt: string; paidAt?: string | null; web?: boolean; chain?: string
}
type ShopStatus = { enabled: boolean; stars: boolean; epay: boolean; usdt: boolean; bot: string; botRunning: boolean; notifyUrl: string; returnUrl?: string; publicUrl: boolean; payments?: boolean; webShop?: boolean; webShopUrl?: string; portal?: boolean }
type Code = { code: string; planId: string; packId?: string; batch: string; expiresAt?: string | null; usedBy: string; usedAt?: string | null; createdAt: string }

// USDT networks (ids match the server's usdtNets).
const usdtNets = [
  { id: 'tron', name: 'TRON (TRC20)', short: 'TRC20', evm: false },
  { id: 'bsc', name: 'BNB Smart Chain (BEP20)', short: 'BEP20', evm: true },
  { id: 'polygon', name: 'Polygon PoS', short: 'Polygon', evm: true },
  { id: 'arbitrum', name: 'Arbitrum One', short: 'Arbitrum', evm: true },
]
const methodLabel: Record<string, string> = { stars: 'Telegram Stars', epay: t('易支付'), usdt: 'USDT', code: t('兑换码'), manual: t('手动') }
const statusLabel: Record<string, [string, string]> = {
  pending: [t('待支付'), 'warn'], paid: [t('已支付'), 'ok'], expired: [t('已过期'), 'off'], failed: [t('开通失败'), 'bad'], refunded: [t('已退款'), 'off'], cancelled: [t('已取消'), 'off'],
}

function money(o: Order) {
  if (o.method === 'code') return <span className="mono small">{o.amount}</span>
  if (o.currency === 'XTR') return `${o.amount} ⭐`
  if (o.currency === 'CNY') return `¥${o.amount}`
  return `${o.amount} ${o.currency}`
}

function Orders() {
  const [status, setStatus] = useState('')
  const [q, setQ] = useState('')
  const list = useLoad(() => get<{ items: Order[]; total: number; usernames: Record<string, string>; revenue: { currency: string; total: number; count: number }[]; status: ShopStatus }>(
    `/orders?limit=200&status=${status}&q=${encodeURIComponent(q)}`), [status, q])
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const d = list.data
  return (
    <>
      {(d?.revenue ?? []).length > 0 && (
        <div className="row" style={{ gap: 12 }}>
          {(d?.revenue ?? []).map((r) => (
            <div key={r.currency} className="card" style={{ margin: 0, padding: '12px 16px' }}>
              <div className="small mute">{t('近 30 天收入')} · {r.currency === 'XTR' ? 'Stars' : r.currency}</div>
              <div style={{ fontWeight: 600, fontSize: 20 }}>{r.currency === 'CNY' ? '¥' : ''}{+r.total.toFixed(4)}{r.currency === 'XTR' ? ' ⭐' : r.currency === 'USDT' ? ' USDT' : ''}</div>
              <div className="small mute">{t('{n} 笔', { n: r.count })}</div>
            </div>
          ))}
        </div>
      )}
      <div className="row">
        <input className="input search" style={{ width: 'auto' }} placeholder={t('搜索订单号 / 用户 / 套餐')} value={q} onChange={(e) => setQ(e.target.value)} aria-label={t('搜索订单')} />
        <div className="chips">
          {['', 'paid', 'pending', 'expired', 'cancelled', 'failed', 'refunded'].map((s) => <button key={s} type="button" className={'chip' + (status === s ? ' on' : '')} onClick={() => setStatus(s)}>{s ? statusLabel[s][0] : t('全部')}</button>)}
        </div>
      </div>
      <div className="card flush scroll">
        {d && (d.items ?? []).length === 0 ? <Empty>{t('还没有订单')}</Empty> : (
          <table className="rtable">
            <thead><tr><th>{t('时间')}</th><th>{t('买家')}</th><th>{t('套餐')}</th><th>{t('付款')}</th><th>{t('金额')}</th><th>{t('状态')}</th><th></th></tr></thead>
            <tbody>
              {(d?.items ?? []).map((o) => (
                <tr key={o.id}>
                  <td data-label={t('时间')} className="small">{datetime(o.paidAt || o.createdAt)}<div className="mono small mute">{o.id}</div></td>
                  <td data-label={t('买家')}>{o.userId ? <b>{d?.usernames?.[o.userId] ?? o.userId}</b> : <span className="mute">—</span>}<div className="small mute">{o.web && <span className="tag" style={{ marginRight: 4 }}>{t('网页')}</span>}{o.tgName || (o.telegramId ? `TG ${o.telegramId}` : '')}</div></td>
                  <td data-label={t('套餐')}>{o.kind === 'pack' && <span className="tag" style={{ marginRight: 6 }}>{t('流量包')}</span>}{o.planName}{o.detail && <div className="small mute">{o.detail}</div>}</td>
                  <td data-label={t('付款')} className="small">{methodLabel[o.method] ?? o.method}{o.method === 'usdt' && <div className="mute">{usdtNets.find((n) => n.id === (o.chain || 'tron'))?.short}</div>}</td>
                  <td data-label={t('金额')} className="mono">{money(o)}</td>
                  <td data-label={t('状态')}><span className={'st ' + (statusLabel[o.status]?.[1] ?? '')}>{statusLabel[o.status]?.[0] ?? o.status}</span></td>
                  <td className="rt-actions">
                    {(o.status === 'pending' || o.status === 'expired' || o.status === 'cancelled') && o.method !== 'code' && (
                      <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                        if (await confirm(t('确认已经收到这笔付款（{amount}），手动开通套餐 {plan}？', { amount: `${o.amount} ${o.currency}`, plan: o.planName }), { ok: t('手动开通') }))
                          run(async () => { await post(`/orders/${o.id}/fulfil`); list.reload() }, t('已开通'))
                      }}>{t('手动开通')}</button>
                    )}
                    {o.status === 'pending' && (
                      <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                        if (await confirm(t('取消订单 {id}？买家将不能再用 Stars 付款；易支付 / USDT 如果仍然付款，到账后照常开通。', { id: o.id }), { danger: true, ok: t('取消订单') }))
                          run(async () => { await post(`/orders/${o.id}/cancel`); list.reload() }, t('订单已取消'))
                      }}>{t('取消订单')}</button>
                    )}
                    {o.status === 'paid' && o.method === 'stars' && (
                      <button className="btn sm" type="button" disabled={busy} onClick={async () => {
                        if (await confirm(t('把 {amount} ⭐ 退还给买家？套餐不会自动收回，如需收回请到用户页调整。', { amount: o.amount }), { danger: true, ok: t('退款') }))
                          run(async () => { await post(`/orders/${o.id}/refund`); list.reload() }, t('已退款'))
                      }}>{t('退款')}</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  )
}

function Codes() {
  const [batch, setBatch] = useState('')
  const [used, setUsed] = useState('')
  const list = useLoad(() => get<{ items: Code[]; total: number; batches: string[] }>(`/redeem-codes?batch=${encodeURIComponent(batch)}&used=${used}`), [batch, used])
  const plans = useLoad(() => get<Group[]>('/plans'))
  const packs = useLoad(() => get<{ items: TrafficPack[]; licensed: boolean }>('/traffic-packs'))
  const [gen, setGen] = useState<{ kind: 'plan' | 'pack'; planId: string; packId: string; count: string; batch: string; expireDays: string } | null>(null)
  const [made, setMade] = useState<string[] | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const pname = (c: Code) => c.packId
    ? <><span className="tag" style={{ marginRight: 6 }}>{t('流量包')}</span>{packs.data?.items?.find((p) => p.id === c.packId)?.name ?? t('已删除')}</>
    : plans.data?.find((p) => p.id === c.planId)?.name ?? t('已删除套餐')
  return (
    <>
      <div className="row">
        <button className="btn pri" type="button" onClick={() => setGen({ kind: 'plan', planId: plans.data?.[0]?.id ?? '', packId: packs.data?.items?.[0]?.id ?? '', count: '10', batch: '', expireDays: '' })}>{t('+ 生成兑换码')}</button>
        <select className="input" style={{ width: 'auto' }} value={batch} onChange={(e) => setBatch(e.target.value)} aria-label={t('批次')}>
          <option value="">{t('全部批次')}</option>
          {(list.data?.batches ?? []).map((b) => <option key={b} value={b}>{b}</option>)}
        </select>
        <div className="chips">
          {[['', t('全部')], ['no', t('未使用')], ['yes', t('已使用')]].map(([k, l]) => <button key={k} type="button" className={'chip' + (used === k ? ' on' : '')} onClick={() => setUsed(k)}>{l}</button>)}
        </div>
        <span className="spacer" />
        {batch && <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
          if (await confirm(t('删除批次 {batch} 中所有未使用的兑换码？', { batch }), { danger: true })) run(async () => { await del('/redeem-codes?batch=' + encodeURIComponent(batch)); setBatch(''); list.reload() }, t('已删除'))
        }}>{t('删除本批未使用的码')}</button>}
      </div>
      <div className="card flush scroll">
        {list.data && (list.data.items ?? []).length === 0 ? <Empty>{t('还没有兑换码')}</Empty> : (
          <table className="rtable">
            <thead><tr><th>{t('兑换码')}</th><th>{t('套餐')}</th><th>{t('批次')}</th><th>{t('有效期')}</th><th>{t('状态')}</th><th></th></tr></thead>
            <tbody>
              {(list.data?.items ?? []).map((c) => (
                <tr key={c.code}>
                  <td className="mono" data-label={t('兑换码')}>{c.code}</td>
                  <td data-label={t('套餐')}>{pname(c)}</td>
                  <td data-label={t('批次')}>{c.batch || <span className="mute">—</span>}</td>
                  <td data-label={t('有效期')} className="small">{c.expiresAt ? datetime(c.expiresAt) : t('长期')}</td>
                  <td data-label={t('状态')}>{c.usedAt ? <span className="st off">{t('已使用 {time}', { time: datetime(c.usedAt) })}</span> : <span className="st ok">{t('未使用')}</span>}</td>
                  <td className="rt-actions">{!c.usedAt && <button className="link" type="button" style={{ color: 'var(--bad)' }} onClick={() => run(async () => { await del('/redeem-codes', { codes: [c.code] }); list.reload() })}>{t('删除')}</button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {gen && (
        <Modal title={t('生成兑换码')} onClose={() => setGen(null)} footer={<>
          <button className="btn" type="button" onClick={() => setGen(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || (gen.kind === 'plan' ? !gen.planId : !gen.packId)} onClick={() => run(async () => {
            const target = gen.kind === 'plan' ? { planId: gen.planId } : { packId: gen.packId }
            const r = await post<{ codes: string[] }>('/redeem-codes', { ...target, count: Number(gen.count) || 0, batch: gen.batch.trim(), expireDays: Number(gen.expireDays) || 0 })
            setGen(null); setMade(r.codes); list.reload()
          })}>{t('生成')}</button>
        </>}>
          <Field label={t('兑换内容')}>
            <div className="seg">
              <button type="button" className={'opt' + (gen.kind === 'plan' ? ' on' : '')} onClick={() => setGen({ ...gen, kind: 'plan' })}>{t('套餐')}<small>{t('开通或续费套餐，没有账号会自动创建')}</small></button>
              <button type="button" className={'opt' + (gen.kind === 'pack' ? ' on' : '')} disabled={!packs.data?.licensed} onClick={() => setGen({ ...gen, kind: 'pack' })}>{t('流量包')} <ProTag on={!!packs.data?.licensed} /><small>{t('给已有账号加本周期流量')}</small></button>
            </div>
          </Field>
          {gen.kind === 'plan' && plans.data?.length === 0 && <div className="note">{t('还没有套餐，请先新建套餐。')}</div>}
          {gen.kind === 'pack' && packs.data?.items?.length === 0 && <div className="note">{t('还没有流量包，请先在「流量包」里新建。')}</div>}
          <div className="form-grid">
            {gen.kind === 'plan' ? (
              <Field label={t('套餐')} htmlFor="c-plan">
                <select id="c-plan" className="input" value={gen.planId} onChange={(e) => setGen({ ...gen, planId: e.target.value })}>
                  {(plans.data ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                </select>
              </Field>
            ) : (
              <Field label={t('流量包')} htmlFor="c-pack">
                <select id="c-pack" className="input" value={gen.packId} onChange={(e) => setGen({ ...gen, packId: e.target.value })}>
                  {(packs.data?.items ?? []).map((p) => <option key={p.id} value={p.id}>{p.name} · +{bytes(p.traffic)}</option>)}
                </select>
              </Field>
            )}
            <Field label={t('数量')} htmlFor="c-count" hint="1–1000"><input id="c-count" className="input mono" inputMode="numeric" value={gen.count} onChange={(e) => setGen({ ...gen, count: e.target.value })} /></Field>
            <Field label={t('批次名')} htmlFor="c-batch" hint={t('例如销售渠道，便于筛选和作废')}><input id="c-batch" className="input" value={gen.batch} onChange={(e) => setGen({ ...gen, batch: e.target.value })} /></Field>
            <Field label={t('有效天数')} htmlFor="c-exp" hint={t('多少天内必须兑换；留空为长期')}><input id="c-exp" className="input mono" inputMode="numeric" value={gen.expireDays} onChange={(e) => setGen({ ...gen, expireDays: e.target.value })} /></Field>
          </div>
          <div className="sub">{gen.kind === 'plan' ? t('用户在机器人里发送 /redeem 兑换码 即可开通（续费同一套餐顺延，换套餐清零已用流量）。') : t('用户在机器人里发送 /redeem 兑换码，流量加到本周期配额，下次流量重置或新周期时失效。')}</div>
        </Modal>
      )}
      {made && (
        <Modal title={t('已生成 {n} 个兑换码', { n: made.length })} onClose={() => setMade(null)}>
          <div className="note">{t('请现在复制保存。以后也可以在列表里查看。')}</div>
          <CopyBox text={made.join('\n')} />
        </Modal>
      )}
    </>
  )
}

type PackEdit = { id?: string; name: string; gb: string; planIds: string[]; sale: boolean; priceStars: string; priceCny: string; priceUsdt: string; sort: number }

function Packs() {
  const list = useLoad(() => get<{ items: TrafficPack[]; licensed: boolean }>('/traffic-packs'))
  const plans = useLoad(() => get<Group[]>('/plans'))
  const [edit, setEdit] = useState<PackEdit | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ok = !!list.data?.licensed
  const pname = (id: string) => plans.data?.find((p) => p.id === id)?.name ?? id
  const price = (p: TrafficPack) => [p.priceCny ? `¥${p.priceCny}` : '', p.priceStars ? `${p.priceStars} ⭐` : '', p.priceUsdt ? `${p.priceUsdt} USDT` : ''].filter(Boolean).join(' · ') || '—'
  return (
    <>
      <div className="row">
        <button className="btn pri" type="button" disabled={!ok} onClick={() => setEdit({ name: '', gb: '50', planIds: [], sale: true, priceStars: '', priceCny: '', priceUsdt: '', sort: list.data?.items?.length ?? 0 })}>{t('+ 新建流量包')}</button>
        <ProTag on={ok} />
        <span className="sub" style={{ margin: 0 }}>{t('用户流量不够时在机器人里加购，不改变套餐和到期时间；只在当前流量周期内有效，流量重置或开始新周期时失效。')}</span>
      </div>
      <div className="card flush scroll">
        {list.data && (list.data.items ?? []).length === 0 ? <Empty>{t('还没有流量包')}</Empty> : (
          <table className="rtable">
            <thead><tr><th>{t('名称')}</th><th>{t('流量')}</th><th>{t('适用套餐')}</th><th>{t('价格')}</th><th>{t('状态')}</th><th></th></tr></thead>
            <tbody>
              {(list.data?.items ?? []).map((p) => (
                <tr key={p.id}>
                  <td data-label={t('名称')}><b>{p.name}</b></td>
                  <td data-label={t('流量')} className="mono">+{bytes(p.traffic)}</td>
                  <td data-label={t('适用套餐')}>{p.planIds.length ? p.planIds.map(pname).join('、') : <span className="mute">{t('所有限流量的账号')}</span>}</td>
                  <td data-label={t('价格')} className="small">{price(p)}</td>
                  <td data-label={t('状态')}>{p.sale ? <span className="st ok">{t('在售')}</span> : <span className="st off">{t('未上架')}</span>}</td>
                  <td className="rt-actions">
                    <button className="link" type="button" disabled={!ok} onClick={() => setEdit({ id: p.id, name: p.name, gb: String(+(p.traffic / GB).toFixed(2)), planIds: p.planIds, sale: p.sale,
                      priceStars: p.priceStars ? String(p.priceStars) : '', priceCny: p.priceCny ? String(p.priceCny) : '', priceUsdt: p.priceUsdt ? String(p.priceUsdt) : '', sort: p.sort })}>{t('编辑')}</button>
                    <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                      if (await confirm(t('删除流量包 {name}？已加给用户的流量不受影响。', { name: p.name }), { danger: true })) run(async () => { await del('/traffic-packs/' + p.id); list.reload() })
                    }}>{t('删除')}</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {edit && (
        <Modal title={edit.id ? t('编辑流量包') : t('新建流量包')} onClose={() => setEdit(null)} footer={<>
          <button className="btn" type="button" onClick={() => setEdit(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !edit.name.trim() || !(Number(edit.gb) > 0)} onClick={() => run(async () => {
            const body = { name: edit.name.trim(), traffic: Math.round(Number(edit.gb) * GB), planIds: edit.planIds, sale: edit.sale,
              priceStars: Math.round(Number(edit.priceStars) || 0), priceCny: Number(edit.priceCny) || 0, priceUsdt: Number(edit.priceUsdt) || 0, sort: edit.sort }
            if (edit.id) await put('/traffic-packs/' + edit.id, body)
            else await post('/traffic-packs', body)
            setEdit(null); list.reload()
          }, t('已保存'))}>{t('保存')}</button>
        </>}>
          <div className="form-grid">
            <Field label={t('名称')} htmlFor="k-name"><input id="k-name" className="input" value={edit.name} onChange={(e) => setEdit({ ...edit, name: e.target.value })} placeholder={t('例如 加油包 50G')} /></Field>
            <Field label={t('流量 (GB)')} htmlFor="k-gb"><input id="k-gb" className="input mono" inputMode="decimal" value={edit.gb} onChange={(e) => setEdit({ ...edit, gb: e.target.value })} /></Field>
            <Field label={t('Telegram Stars 价格')} htmlFor="k-stars"><input id="k-stars" className="input mono" inputMode="numeric" value={edit.priceStars} onChange={(e) => setEdit({ ...edit, priceStars: e.target.value })} /></Field>
            <Field label={t('易支付价格（元）')} htmlFor="k-cny"><input id="k-cny" className="input mono" inputMode="decimal" value={edit.priceCny} onChange={(e) => setEdit({ ...edit, priceCny: e.target.value })} /></Field>
            <Field label={t('USDT 价格')} htmlFor="k-usdt"><input id="k-usdt" className="input mono" inputMode="decimal" value={edit.priceUsdt} onChange={(e) => setEdit({ ...edit, priceUsdt: e.target.value })} /></Field>
            <Field label={t('排序')} htmlFor="k-sort" hint={t('小的排前面')}><input id="k-sort" className="input mono" type="number" value={edit.sort} onChange={(e) => setEdit({ ...edit, sort: Number(e.target.value) || 0 })} /></Field>
          </div>
          <Field label={t('适用套餐')} hint={t('不选 = 所有限流量的账号都能买')}>
            <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
              {(plans.data ?? []).map((p) => {
                const on = edit.planIds.includes(p.id)
                return <button key={p.id} type="button" className={'chip' + (on ? ' on' : '')} aria-pressed={on}
                  onClick={() => setEdit({ ...edit, planIds: on ? edit.planIds.filter((x) => x !== p.id) : [...edit.planIds, p.id] })}>{p.name}</button>
              })}
              {plans.data?.length === 0 && <span className="hint">{t('还没有套餐')}</span>}
            </div>
          </Field>
          <div className="toggle"><span>{t('在 Telegram 机器人售卖')}</span><Switch label={t('售卖')} on={edit.sale} onChange={(v) => setEdit({ ...edit, sale: v })} /></div>
        </Modal>
      )}
    </>
  )
}

function PaySettings({ status }: { status?: ShopStatus }) {
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const keys = ['shop_enabled', 'shop_web_enabled', 'shop_auto_create', 'shop_user_prefix', 'shop_order_minutes', 'shop_stars', 'shop_epay_url', 'shop_epay_pid', 'shop_epay_types', 'shop_usdt_address', 'shop_usdt_evm_address', 'shop_usdt_chains', 'shop_usdt_rpc_bsc', 'shop_usdt_rpc_polygon', 'shop_usdt_rpc_arbitrum', 'shop_terms', 'shop_support'] as const
  const defaults: Record<string, string> = { shop_enabled: 'false', shop_web_enabled: 'false', shop_auto_create: 'true', shop_user_prefix: 'tg', shop_order_minutes: '30', shop_stars: 'false', shop_epay_types: 'alipay,wxpay', shop_usdt_chains: 'tron' }
  const [f, setF] = useState<Record<string, string>>({})
  const [secrets, setSecrets] = useState({ shop_epay_key: '', shop_usdt_apikey: '' })
  const payOK = useFeature('shop_payments')
  const { busy, run } = useAction()
  useEffect(() => {
    const d = settings.data
    if (d) setF(Object.fromEntries(keys.map((k) => [k, d[k] ?? defaults[k] ?? ''])))
  }, [settings.data]) // eslint-disable-line react-hooks/exhaustive-deps
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const sw = (k: string) => f[k] === 'true'
  const chains = (f.shop_usdt_chains ?? '').split(',').filter(Boolean)
  const setChain = (id: string, on: boolean) => set('shop_usdt_chains', usdtNets.filter((n) => (n.id === id ? on : chains.includes(n.id))).map((n) => n.id).join(','))
  const evmOn = usdtNets.some((n) => n.evm && chains.includes(n.id))
  if (!settings.data) return null
  return (
    <div className="grid g2">
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <h2>{t('商店')}</h2>
        {!status?.botRunning && <div className="note warn">{t('Telegram 机器人未运行。请先在「系统 → Telegram 机器人」里启用。')}</div>}
        <div className="toggle"><span>{t('在机器人里开放购买（菜单显示「购买套餐」和「兑换码」）')}</span><Switch label={t('开放购买')} on={sw('shop_enabled')} onChange={(v) => set('shop_enabled', String(v))} /></div>
        <div className="toggle"><span>{t('网页商店：在用户中心 /u/shop 购买套餐和流量包（登录用户中心可续费；未登录可直接购买新账号）')}</span><Switch label={t('网页商店')} on={sw('shop_web_enabled')} onChange={(v) => set('shop_web_enabled', String(v))} /></div>
        {sw('shop_web_enabled') && (status?.portal === false
          ? <div className="note warn">{t('网页商店需要先在「订阅设置」里开启用户中心。')}</div>
          : status?.webShopUrl && <Field label={t('网页商店地址')}><CopyBox text={status.webShopUrl} /></Field>)}
        <div className="toggle"><span>{t('没有账号的 Telegram 用户付款后自动创建账号并绑定')}</span><Switch label={t('自动建号')} on={sw('shop_auto_create')} onChange={(v) => set('shop_auto_create', String(v))} /></div>
        <div className="form-grid">
          <Field label={t('新账号用户名前缀')} htmlFor="s-prefix" hint={t('用户名为 前缀 + Telegram ID')}><input id="s-prefix" className="input mono" value={f.shop_user_prefix ?? ''} onChange={(e) => set('shop_user_prefix', e.target.value)} /></Field>
          <Field label={t('订单有效时间（分钟）')} htmlFor="s-ttl" hint={t('超时未付款的订单关闭；之后到账的付款仍会自动开通')}><input id="s-ttl" className="input mono" inputMode="numeric" value={f.shop_order_minutes ?? ''} onChange={(e) => set('shop_order_minutes', e.target.value)} /></Field>
        </div>
        <Field label={t('购买条款')} htmlFor="s-terms" hint={t('用户在机器人里用 /terms 查看；付款即视为同意。Telegram 要求用 Stars 收款的机器人提供条款')}>
          <textarea id="s-terms" className="input" rows={4} value={f.shop_terms ?? ''} onChange={(e) => set('shop_terms', e.target.value)} placeholder={t('例如：套餐付款后立即开通，虚拟商品不支持无理由退款……')} />
        </Field>
        <Field label={t('售后联系方式')} htmlFor="s-support" hint={t('用户在机器人里用 /paysupport 查看，例如 @your_support')}><input id="s-support" className="input" value={f.shop_support ?? ''} onChange={(e) => set('shop_support', e.target.value)} /></Field>
        <div className="sub">{t('在「用户 → 套餐」里给套餐勾选「在机器人售卖」并填写各付款方式的价格。')}</div>
      </section>
      {!payOK && <div className="note">{t('在线收款（Telegram Stars、易支付、USDT）是专业版功能。社区版可以用兑换码售卖：在「兑换码」里生成，放到发卡平台出售，用户在机器人里用 /redeem 兑换。')}</div>}
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>Telegram Stars<ProTag on={payOK} /></h2><span className={'st ' + (status?.stars ? 'ok' : 'off')}>{status?.stars ? t('已启用') : t('未启用')}</span></div>
        <div className="toggle"><span>{t('接受 Telegram Stars 付款（电报内置，无需商户号；收入在 Telegram 账户中，经 Fragment 提现）')}</span><Switch label="Stars" on={sw('shop_stars')} onChange={(v) => set('shop_stars', String(v))} /></div>
      </section>
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>{t('易支付 / EPay')}<ProTag on={payOK} /></h2><span className={'st ' + (status?.epay ? 'ok' : 'off')}>{status?.epay ? t('已配置') : t('未配置')}</span></div>
        <Field label={t('接口地址')} htmlFor="s-epay-url" hint={t('易支付网关地址，例如 https://pay.example.com')}><input id="s-epay-url" className="input mono" value={f.shop_epay_url ?? ''} onChange={(e) => set('shop_epay_url', e.target.value)} placeholder="https://pay.example.com" /></Field>
        <div className="form-grid">
          <Field label={t('商户 ID')} htmlFor="s-epay-pid"><input id="s-epay-pid" className="input mono" value={f.shop_epay_pid ?? ''} onChange={(e) => set('shop_epay_pid', e.target.value)} /></Field>
          <Field label={t('商户密钥')} htmlFor="s-epay-key" hint={settings.data.shop_epay_key_set ? t('已保存（留空不修改）') : t('加密存储')}><input id="s-epay-key" className="input mono" type="password" autoComplete="off" value={secrets.shop_epay_key} onChange={(e) => setSecrets({ ...secrets, shop_epay_key: e.target.value })} /></Field>
        </div>
        <Field label={t('支付方式')} htmlFor="s-epay-types" hint={t('逗号分隔：alipay 支付宝、wxpay 微信、qqpay QQ 钱包')}><input id="s-epay-types" className="input mono" value={f.shop_epay_types ?? ''} onChange={(e) => set('shop_epay_types', e.target.value)} /></Field>
        <Field label={t('异步通知地址')} hint={status?.publicUrl ? t('网关会自动使用这个地址，一般无需在商户后台另外填写') : t('未设置面板公网地址（POLARIS_PUBLIC_URL）：请确认这个地址能从公网访问')}><CopyBox text={status?.notifyUrl ?? ''} /></Field>
        <Field label={t('同步跳转地址')} hint={t('付款后买家先回到这个面板地址，再自动打开机器人。请在易支付商户后台把面板域名加入授权域名，否则会提示「支付返回域名未通过审核」')}><CopyBox text={status?.returnUrl ?? ''} /></Field>
      </section>
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="row" style={{ justifyContent: 'space-between' }}><h2>USDT<ProTag on={payOK} /></h2><span className={'st ' + (status?.usdt ? 'ok' : 'off')}>{status?.usdt ? t('已配置') : t('未配置')}</span></div>
        <div className="sub">{t('选择接受的网络，买家下单时自选网络，付款页显示收款地址和二维码。每笔订单金额带不同的小数尾数，按金额自动对账。')}</div>
        {usdtNets.map((n) => (
          <div className="toggle" key={n.id}><span>{n.name}</span><Switch label={n.short} on={chains.includes(n.id)} onChange={(v) => setChain(n.id, v)} /></div>
        ))}
        {chains.includes('tron') && <>
          <Field label={t('TRON 收款地址')} htmlFor="s-usdt-addr" hint={t('TRON 网络地址（T 开头）')}><input id="s-usdt-addr" className="input mono" value={f.shop_usdt_address ?? ''} onChange={(e) => set('shop_usdt_address', e.target.value)} placeholder="T…" /></Field>
          <Field label="TronGrid API Key" htmlFor="s-usdt-key" hint={settings.data.shop_usdt_apikey_set ? t('已保存（留空不修改）') : t('在 trongrid.io 免费申请；不填也能用，但容易被限流')}><input id="s-usdt-key" className="input mono" type="password" autoComplete="off" value={secrets.shop_usdt_apikey} onChange={(e) => setSecrets({ ...secrets, shop_usdt_apikey: e.target.value })} /></Field>
        </>}
        {evmOn && <>
          <Field label={t('EVM 收款地址')} htmlFor="s-usdt-evm" hint={t('BEP20、Polygon、Arbitrum 共用一个 0x 地址。请用自己掌握私钥的钱包地址，不要用交易所充值地址')}><input id="s-usdt-evm" className="input mono" value={f.shop_usdt_evm_address ?? ''} onChange={(e) => set('shop_usdt_evm_address', e.target.value)} placeholder="0x…" /></Field>
          <details>
            <summary className="small">{t('自定义 RPC 节点（可选）')}</summary>
            <div className="sub" style={{ margin: '8px 0' }}>{t('默认使用公共 RPC；公共节点限流时可填自己的节点（如 Alchemy、QuickNode、Ankr）。')}</div>
            {usdtNets.filter((n) => n.evm && chains.includes(n.id)).map((n) => (
              <Field key={n.id} label={n.name} htmlFor={'s-rpc-' + n.id}><input id={'s-rpc-' + n.id} className="input mono" value={f['shop_usdt_rpc_' + n.id] ?? ''} onChange={(e) => set('shop_usdt_rpc_' + n.id, e.target.value)} placeholder="https://…" /></Field>
            ))}
          </details>
        </>}
        <div className="sub">{t('面板每 30 秒查询一次链上到账，确认后自动开通。')}</div>
      </section>
      <div><button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        await put('/settings', { ...f, ...secrets })
        setSecrets({ shop_epay_key: '', shop_usdt_apikey: '' })
        settings.reload()
      }, t('已保存'))}>{t('保存')}</button></div>
    </div>
  )
}

export default function Shop() {
  const [params, setParams] = useSearchParams()
  const tab = params.get('tab') || 'orders'
  const st = useLoad(() => get<{ status: ShopStatus }>('/orders?limit=1'))
  const [plans, setPlans] = useState(false)
  const s = st.data?.status
  const tabs = [{ id: 'orders', label: t('订单') }, { id: 'packs', label: t('流量包') }, { id: 'codes', label: t('兑换码') }, { id: 'pay', label: t('支付设置') }]
  return (
    <>
      <div className="top">
        <div><h1>{t('售卖')}</h1><div className="sub">{t('用户在 Telegram 机器人里购买套餐：Telegram Stars、易支付、USDT 或兑换码，付款后自动开通')}</div></div>
        <div className="row">
          {s && <span className={'st ' + (s.enabled && s.botRunning ? 'ok' : 'off')}>{s.enabled ? (s.botRunning ? t('● 营业中 @{bot}', { bot: s.bot }) : t('机器人未运行')) : t('未开放')}</span>}
          <button className="btn" type="button" onClick={() => setPlans(true)}>{t('管理套餐')}</button>
        </div>
      </div>
      <div className="tabs" role="tablist" style={{ marginBottom: 14 }}>
        {tabs.map((x) => <button key={x.id} type="button" role="tab" aria-selected={tab === x.id} className={'tab' + (tab === x.id ? ' on' : '')} onClick={() => setParams({ tab: x.id }, { replace: true })}>{x.label}</button>)}
      </div>
      {tab === 'orders' && <Orders />}
      {tab === 'packs' && <Packs />}
      {tab === 'codes' && <Codes />}
      {tab === 'pay' && <PaySettings status={s} />}
      {plans && <PlansModal onClose={() => setPlans(false)} onChanged={() => st.reload()} />}
    </>
  )
}
