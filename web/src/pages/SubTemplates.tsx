import { SettingsGate, useSettingsForm } from '../components/SettingsLoad'
import { useRef, useState } from 'react'
import { SettingsSaveBar, useSettingsDraft, useSettingsLeave, useSettingsSave } from '../components/SettingsDrafts'
import { del, get, post, put } from '../api'
import { Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { datetime } from '../format'
import { t, tr } from '../i18n'
import type { SubTemplate } from '../types'

// Built-in default template name as stored by the server ("默认").
const defaultName = '\u9ed8\u8ba4'

const formats: Record<string, { label: string; hint: string }> = {
  clash: { label: 'Clash / mihomo / Stash', hint: t('{{ .Proxies }} 展开为 proxies 列表，{{ .Names }} 节点名数组，{{ .NamesInline }} 逗号分隔节点名，{{ range .Regions }} 按地区分组（.Name .NamesJSON）') },
  singbox: { label: 'sing-box / SFA / SFI', hint: t('{{ .Outbounds }} 展开为出站 JSON 片段（{{ if .HasOutbounds }} 判断非空），{{ .Tags }} 节点 tag 数组') },
  surge: { label: 'Surge', hint: t('{{ .Proxies }} 为 [Proxy] 行，{{ .NamesCSV }} 为 ", 节点名" 列表，{{ .URL }} 为订阅地址') },
  loon: { label: 'Loon', hint: t('{{ .Proxies }} 为 [Proxy] 行，{{ .NamesCSV }} 为节点名列表，{{ range .Regions }} 按地区分组') },
  quanx: { label: 'Quantumult X', hint: t('{{ .Proxies }} 为 [server_local] 行，{{ .NamesCSV }} 为节点名列表，{{ range .Regions }} 按地区分组') },
}

export default function SubTemplates() {
  const list = useLoad((signal) => get<SubTemplate[]>('/sub-templates', signal))
  const [fmt, setFmt] = useState('clash')
  const [sel, setSel] = useState('')
  const [saved, setSaved] = useState<Record<string, SubTemplate>>({})
  const savedRef = useRef(saved)
  const [draft, setDraft] = useState<{ id: string; value: string; base: string; revision?: number } | null>(null)
  const [create, setCreate] = useState<{ name: string; copy: boolean; source: SubTemplate } | null>(null)
  const [rename, setRename] = useState<{ name: string; source: SubTemplate } | null>(null)
  const saver = useSettingsSave(), actions = useSettingsSave()
  const busy = saver.busy || actions.busy
  const confirm = useConfirm(), leave = useSettingsLeave()
  const all = new Map((list.data ?? []).map((row) => [row.id, row]))
  for (const row of Object.values(saved)) if (!all.has(row.id) || (all.get(row.id)?.revision ?? 0) <= (row.revision ?? 0)) all.set(row.id, row)
  const ofFmt = [...all.values()].filter((row) => row.format === fmt)
  const cur = ofFmt.find((row) => row.id === sel) ?? ofFmt.find((row) => row.isDefault) ?? ofFmt[0]
  const editing = draft?.id === cur?.id ? draft : null
  const content = editing?.value ?? cur?.content ?? ''
  const dirty = !!editing && editing.value !== editing.base
  const conflict = dirty && editing?.revision !== undefined && cur?.revision !== undefined && editing.revision !== cur.revision
  const accept = (row: SubTemplate) => { savedRef.current = { ...savedRef.current, [row.id]: row }; setSaved(savedRef.current) }
  const snapshot = (row: SubTemplate) => { const cached = savedRef.current[row.id]; return cached && (cached.revision ?? 0) >= (row.revision ?? 0) ? cached : row }
  const save = async () => {
    if (!dirty) return true
    if (!cur || busy) return false
    const id = cur.id, submitted = content, revision = editing?.revision
    return saver.run(async () => {
      const row = await put<SubTemplate>('/sub-templates/' + id, { content: submitted, revision })
      accept(row)
      setDraft((current) => current?.id !== id ? current : current.value === submitted ? null : { ...current, base: row.content, revision: row.revision })
      list.reload()
    }, t('模板已保存（保存前已用示例节点校验）'))
  }
  useSettingsDraft({ label: t('订阅规则模板'), dirty, busy, save, discard: () => setDraft(null) })
  const select = async (format: string, id = '') => {
    if (format === fmt && (!id || id === cur?.id)) return
    if (!busy && await leave()) { setDraft(null); setFmt(format); setSel(id) }
  }
  const mutate = async (patch: Record<string, unknown>, message: string) => {
    if (!cur || busy || !await leave()) return
    const source = snapshot(cur)
    await actions.run(async () => { accept(await put<SubTemplate>('/sub-templates/' + source.id, { ...patch, revision: source.revision })); list.reload() }, message)
  }
  const fmts = Object.keys(formats).filter((format) => [...all.values()].some((row) => row.format === format))
  return <>
    <div className="top"><div><h1>{t('订阅设置')}</h1><div className="sub">{t('按客户端 UA 或 ?format= 输出：通用链接 · Clash · sing-box · Surge · Loon · Quantumult X；每种格式可维护多套规则模板，订阅组可分别选用')}</div></div></div>
    <SubSettings />
    <NodeNameTemplate />
    {list.error && <div className="note bad" role="alert">{list.error}<button className="btn sm" type="button" onClick={list.reload}>{t('重试')}</button></div>}
    <div className="tabs" role="tablist">{fmts.map((format) => <button key={format} type="button" role="tab" aria-selected={format === fmt} className={'tab' + (format === fmt ? ' on' : '')} disabled={busy} onClick={() => void select(format)}>{formats[format].label}</button>)}</div>
    <div className="row">{ofFmt.map((row) => <button key={row.id} type="button" className={'chip' + (row.id === cur?.id ? ' on' : '')} disabled={busy} onClick={() => void select(fmt, row.id)}>{tr(row.name)}{row.isDefault && row.name !== defaultName ? ' · ' + t('默认') : ''}</button>)}
      <button className="btn sm" type="button" disabled={!cur || busy} onClick={async () => { if (cur && await leave()) setCreate({ name: '', copy: true, source: snapshot(cur) }) }}>+ {t('新模板')}</button>
    </div>
    {cur && <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="hint">{formats[cur.format]?.hint} · {t('Go text/template 语法')} · {t('最后修改 {time}', { time: datetime(cur.updatedAt) })}</div>
      {conflict && <div className="note warn" role="status">{t('服务器模板已更新，当前草稿已保留。保存前请重新加载并核对。')}</div>}
      <textarea className="input" style={{ minHeight: 460 }} spellCheck={false} value={content} disabled={actions.busy} onChange={(event) => {
        const value = event.target.value
        setDraft((current) => current?.id === cur.id ? { ...current, value } : { id: cur.id, value, base: cur.content, revision: cur.revision })
      }} aria-label={t('模板内容')} />
      <SettingsSaveBar dirty={dirty} busy={saver.busy} error={saver.error || actions.error} disabled={actions.busy} onSave={save} />
      <div className="row">
        <button className="btn" type="button" disabled={busy} onClick={async () => {
          if (dirty && !await confirm(t('重新加载会丢弃本模板尚未保存的内容，是否继续？'))) return
          const next = { ...savedRef.current }; delete next[cur.id]; savedRef.current = next; setSaved(next); setDraft(null); saver.clearError(); actions.clearError(); list.reload()
        }}>{t('重新加载服务器内容')}</button>
        {!cur.isDefault && <button className="btn" type="button" disabled={busy} onClick={() => void mutate({ isDefault: true }, t('已设为默认'))}>{t('设为默认')}</button>}
        <button className="btn" type="button" disabled={busy} onClick={async () => { if (await leave()) { const source = snapshot(cur); setRename({ name: source.name, source }) } }}>{t('重命名')}</button>
        <button className="btn" type="button" disabled={busy} onClick={async () => { if (await confirm(t('恢复为内置默认内容？'))) await mutate({ reset: true }, t('已恢复默认')) }}>{t('恢复内置内容')}</button>
        <span className="spacer" />
        {!cur.isDefault && <button className="btn danger" type="button" disabled={busy} onClick={async () => {
          if (!await leave() || !await confirm(t('删除模板「{name}」？使用它的订阅组会回落到默认模板。', { name: cur.name }), { danger: true })) return
          const source = snapshot(cur)
          await actions.run(async () => { await del('/sub-templates/' + source.id, { revision: source.revision }); const next = { ...savedRef.current }; delete next[source.id]; savedRef.current = next; setSaved(next); setSel(''); setDraft(null); list.reload() })
        }}>{t('删除')}</button>}
      </div>
    </section>}
    {create && <Modal title={t('新建 {format} 模板', { format: formats[fmt]?.label })} dismissDisabled={actions.busy} onClose={() => setCreate(null)} footer={<>
      <button className="btn" type="button" disabled={actions.busy} onClick={() => setCreate(null)}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={actions.busy || !create.name} onClick={() => actions.run(async () => {
        const row = await post<SubTemplate>('/sub-templates', { format: fmt, name: create.name, content: create.copy ? create.source.content : '' }); accept(row); setCreate(null); setSel(row.id); setDraft(null); list.reload()
      }, t('已创建'))}>{t('创建')}</button>
    </>}><Field label={t('名称')}><input className="input" value={create.name} disabled={actions.busy} onChange={(event) => setCreate({ ...create, name: event.target.value })} placeholder={t('精简规则 / 游戏分流')} /></Field><label className="chk"><input type="checkbox" checked={create.copy} disabled={actions.busy} onChange={(event) => setCreate({ ...create, copy: event.target.checked })} />{t('复制当前模板内容（不勾选则使用内置默认内容）')}</label></Modal>}
    {rename && <Modal title={t('重命名模板')} dismissDisabled={actions.busy} onClose={() => setRename(null)} footer={<>
      <button className="btn" type="button" disabled={actions.busy} onClick={() => setRename(null)}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={actions.busy || !rename.name} onClick={() => actions.run(async () => { accept(await put<SubTemplate>('/sub-templates/' + rename.source.id, { name: rename.name, revision: rename.source.revision })); setRename(null); list.reload() })}>{t('保存')}</button>
    </>}><Field label={t('名称')}><input className="input" value={rename.name} disabled={actions.busy} onChange={(event) => setRename({ ...rename, name: event.target.value })} /></Field></Modal>}
  </>
}

function NodeNameTemplate() {
  const settings = useLoad((signal) => get<Record<string, unknown>>('/settings', signal))
  const [saved, setSaved] = useState<string | null>(null)
  const [draft, setDraft] = useState<{ value: string; base: string } | null>(null)
  const initial = saved ?? String(settings.data?.sub_name_template ?? '')
  const value = draft?.value ?? initial, dirty = !!draft && draft.value !== draft.base
  const saver = useSettingsSave()
  const save = async () => {
    if (!dirty) return true
    const submitted = value
    return saver.run(async () => { await put('/settings', { sub_name_template: submitted }); setSaved(submitted); setDraft((current) => current?.value === submitted ? null : current && ({ ...current, base: submitted })) }, t('已保存'))
  }
  useSettingsDraft({ label: t('节点名称模板'), dirty, busy: saver.busy, save, discard: () => setDraft(null) })
  return <section className="card"><h2>{t('节点名称模板')}</h2>
    <Field label={t('模板')} hint={t('变量：{flag} 旗帜 · {region} 地区 · {node} 节点名 · {protocol} 协议 · {tag} 入站标签 · {kind} 入口类型 · {rate} 流量倍率')}><input className="input mono" value={value} disabled={!settings.data || !!settings.error} onChange={(event) => { const next = event.target.value; setDraft((current) => ({ value: next, base: current?.base ?? initial })) }} /></Field>
    {settings.error && <p className="note bad" role="alert">{settings.error}<button className="btn sm" onClick={settings.reload}>{t('重试')}</button></p>}
    <SettingsSaveBar dirty={dirty} busy={saver.busy} loading={!settings.data && settings.loading} error={saver.error} onSave={save} />
  </section>
}

const settingKeys = ['sub_enabled', 'sub_base_url', 'sub_host_strict', 'sub_title', 'sub_update_interval', 'sub_support_url', 'sub_show_info', 'sub_show_bot', 'portal_enabled', 'portal_email_register', 'register_email_domains', 'register_trial_plan', 'register_ip_daily', 'sub_show_rate', 'sub_announce', 'sub_routing_enabled', 'sub_routing_rules'] as const

const routingExample = `{
  "Name": "${t('直连国内')}",
  "GlobalProxy": "true",
  "DomesticDNSType": "DoH",
  "DomesticDNSIP": "223.5.5.5",
  "DirectSites": ["geosite:cn", "geosite:private"],
  "DirectIp": ["geoip:cn", "geoip:private"],
  "BlockSites": ["geosite:category-ads-all"]
}`

function SubSettings() {
  const plansQ = useLoad((signal) => get<{ id: string; name: string }[]>('/plans', signal))
  const plans = plansQ.data ?? []
  const { state: s, fields: f, setFields: setF, dirty, reset } = useSettingsForm({ ...Object.fromEntries(settingKeys.map((key) => [key, ''])), sub_enabled: 'true', sub_show_bot: 'true', sub_show_rate: 'true', sub_host_strict: '1', sub_update_interval: '1' })
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const saver = useSettingsSave()
  const save = async () => {
    if (!dirty) return true
    return saver.run(async () => {
      if (f.sub_routing_enabled === 'true' && f.sub_routing_rules && !f.sub_routing_rules.trim().startsWith('happ://')) JSON.parse(f.sub_routing_rules)
      await put('/settings', f); s.reload()
    }, t('订阅设置已保存'))
  }
  useSettingsDraft({ label: t('订阅与信息页'), dirty, busy: busy || saver.busy, save, discard: reset })
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const on = (k: string) => k === 'sub_enabled' ? f[k] !== 'false' : f[k] === 'true'
  return (
    <SettingsGate state={s}><section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <fieldset className="settings-fields" disabled={busy || saver.busy || s.loading}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>{t('订阅与信息页')}</h2>
        <label className="row small">{t('对外提供订阅')}<Switch label={t('启用订阅')} on={on('sub_enabled')} onChange={(v) => set('sub_enabled', String(v))} /></label>
      </div>
      {!on('sub_enabled') && <div className="note warn">{t('已关闭：所有 /sub/ 链接返回 404，客户端将无法更新订阅。')}</div>}
      <div className="form-grid">
        <Field label={t('订阅域名')} hint={t('订阅链接使用的地址，可与后台分开（如走 CDN 的独立域名），格式 https://sub.example.com；留空用公开地址。HTTPS 证书可在「设置 → 面板访问」一键配置')}><input className="input mono" value={f.sub_base_url ?? ''} onChange={(e) => set('sub_base_url', e.target.value)} placeholder={s.data?.public_url || 'https://sub.example.com'} /></Field>
        <Field label={t('订阅标题')} hint={t('客户端里显示的配置名（profile-title），留空用站点标题')}><input className="input" value={f.sub_title ?? ''} onChange={(e) => set('sub_title', e.target.value)} placeholder={s.data?.site_title} /></Field>
        <Field label={t('客户端自动更新间隔（小时）')} hint={t('客户端多久重新拉取一次订阅，用量和到期显示随之更新；建议 1')}><input className="input mono" inputMode="numeric" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
        <Field label={t('客服 / 支持链接')} hint={t('support-url 头，信息页显示「联系客服」')}><input className="input mono" value={f.sub_support_url ?? ''} onChange={(e) => set('sub_support_url', e.target.value)} placeholder="https://t.me/your_support" /></Field>
      </div>
      {!!f.sub_base_url?.trim() && <label className="row small">
        <Switch label={t('订阅域名只开放订阅')} on={f.sub_host_strict !== '0'} onChange={(v) => set('sub_host_strict', v ? '1' : '0')} />
        {t('订阅域名只开放订阅和用户中心（后台、API 返回伪装页），推荐开启')}
      </label>}
      <Field label={t('在节点列表中显示用量')} hint={t('订阅名和用量默认通过响应头（profile-title / subscription-userinfo）告诉客户端；不读响应头的客户端只能靠节点名显示')}>
        <div className="seg c2">
          {([['auto', t('自动（推荐）'), t('能读响应头的客户端（Clash / Hiddify / Loon / QX / Shadowrocket…）直接显示；v2rayN、NekoBox、官方 sing-box、Surge 插入信息节点')], ['none', t('不显示'), t('只通过响应头')], ['nodes', t('信息节点'), t('顶部插入「套餐 / 剩余流量 / 到期 / 公告」')], ['suffix', t('名称后缀'), t('每个节点名后加「| 剩余 · 天数」')]] as [string, string, string][]).map(([k, l, h]) => (
            <button key={k} type="button" className={'opt' + ((f.sub_show_info || 'none') === k ? ' on' : '')} onClick={() => set('sub_show_info', k)}>{l}<small>{h}</small></button>
          ))}
        </div>
      </Field>
      <div className="toggle"><span>{t('信息页显示 Telegram 一键绑定（机器人运行时）')}</span><Switch label={t('机器人绑定')} on={on('sub_show_bot')} onChange={(v) => set('sub_show_bot', String(v))} /></div>
      <div className="toggle"><span>{t('节点名显示流量倍率（倍率不为 1 时加「×2」等；名称模板里可用 {rate} 指定位置）')}</span><Switch label={t('显示倍率')} on={on('sub_show_rate')} onChange={(v) => set('sub_show_rate', String(v))} /></div>
      <div className="toggle"><span>{t('用户中心：独立的用户登录入口，查看订阅、用量、节点与订单')}</span><Switch label={t('用户中心')} on={on('portal_enabled')} onChange={(v) => set('portal_enabled', String(v))} /></div>
      {on('portal_enabled') && <div className="hint">{t('配置了邮件（设置 → 邮件）后，用户也可以用邮箱验证码登录用户中心，并在用户中心绑定邮箱。')}</div>}
      {on('portal_enabled') && <div className="toggle"><span>{t('开放邮箱注册：显示注册入口，验证后创建账号；关闭后已有账号仍可登录（需配置邮件）')}</span><Switch label={t('邮箱注册')} on={on('portal_email_register')} onChange={(v) => set('portal_email_register', String(v))} /></div>}
      {on('portal_enabled') && on('portal_email_register') && <div className="form-grid">
        <Field label={t('试用套餐')} hint={t('新用户自动开通的套餐；不选则账号没有可用服务，需要先购买')}>
          <select className="input" value={f.register_trial_plan ?? ''} onChange={(e) => set('register_trial_plan', e.target.value)}>
            <option value="">{t('不送试用')}</option>
            {plans.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </Field>
        <Field label={t('允许的邮箱域名')} hint={t('逗号分隔，如 gmail.com, qq.com；留空不限')}><input className="input mono" value={f.register_email_domains ?? ''} onChange={(e) => set('register_email_domains', e.target.value)} placeholder="gmail.com, qq.com, 163.com" /></Field>
        <Field label={t('每个 IP 每天最多注册')} hint={t('防止批量注册；0 表示不限，默认 3')}><input className="input mono" inputMode="numeric" value={f.register_ip_daily ?? ''} onChange={(e) => set('register_ip_daily', e.target.value.replace(/[^\d]/g, ''))} placeholder="3" /></Field>
      </div>}
      {on('portal_enabled') && s.data?.portal_url && <div className="hint">{t('用户登录地址：')}<a className="mono" href={s.data.portal_url + '/login'} target="_blank" rel="noreferrer">{s.data.portal_url + '/login'}</a>{t('（保存后生效；机器人发送订阅链接时也会附上）')}</div>}
      <Field label={t('公告')} hint={t('显示在订阅信息页顶部，并通过 announce 头推送给支持的客户端（Happ、Streisand 等）；开启「信息节点」时也会出现在节点列表')}>
        <textarea className="input" style={{ minHeight: 70 }} value={f.sub_announce ?? ''} onChange={(e) => set('sub_announce', e.target.value)} placeholder={t('例：本周六 02:00–04:00 维护，请提前更新订阅')} />
      </Field>
      <div className="toggle"><span>{t('下发客户端路由规则（routing 头，Happ 等支持）')}</span><Switch label={t('路由')} on={on('sub_routing_enabled')} onChange={(v) => set('sub_routing_enabled', String(v))} /></div>
      {on('sub_routing_enabled') && (
        <Field label={t('路由配置')} hint={t('粘贴 Happ 路由 JSON，或现成的 happ://routing/… 链接；Clash / sing-box 等客户端的分流由上方规则模板决定')}>
          <textarea className="input mono" style={{ minHeight: 140 }} spellCheck={false} value={f.sub_routing_rules ?? ''} onChange={(e) => set('sub_routing_rules', e.target.value)} placeholder={routingExample} />
        </Field>
      )}
      {saver.error && <div className="note bad" role="alert">{saver.error}</div>}
      <div className="row">
        <button className="btn pri" type="button" disabled={busy || saver.busy || !dirty} onClick={save}>{saver.busy ? t('保存中…') : t('保存')}</button>
        <button className="btn" type="button" disabled={busy || !f.sub_announce?.trim()} onClick={async () => {
          if (await confirm(t('把当前公告通过 Telegram 推送给所有已绑定的用户？'))) run(async () => {
            const r = await post<{ queued: number }>('/telegram/broadcast', { text: f.sub_announce }); return r
          }, t('已开始推送'))
        }}>{t('推送公告到 Telegram')}</button>
      </div>
      </fieldset>
    </section></SettingsGate>
  )
}
