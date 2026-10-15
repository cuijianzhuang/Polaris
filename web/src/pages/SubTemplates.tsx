import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { datetime } from '../format'
import type { SubTemplate } from '../types'

const formats: Record<string, { label: string; hint: string }> = {
  clash: { label: 'Clash / mihomo / Stash', hint: '{{ .Proxies }} 展开为 proxies 列表，{{ .Names }} 节点名数组，{{ .NamesInline }} 逗号分隔节点名，{{ range .Regions }} 按地区分组（.Name .NamesJSON）' },
  singbox: { label: 'sing-box / SFA / SFI', hint: '{{ .Outbounds }} 展开为出站 JSON 片段（{{ if .HasOutbounds }} 判断非空），{{ .Tags }} 节点 tag 数组' },
  surge: { label: 'Surge', hint: '{{ .Proxies }} 为 [Proxy] 行，{{ .NamesCSV }} 为 ", 节点名" 列表，{{ .URL }} 为订阅地址' },
  loon: { label: 'Loon', hint: '{{ .Proxies }} 为 [Proxy] 行，{{ .NamesCSV }} 为节点名列表，{{ range .Regions }} 按地区分组' },
  quanx: { label: 'Quantumult X', hint: '{{ .Proxies }} 为 [server_local] 行，{{ .NamesCSV }} 为节点名列表，{{ range .Regions }} 按地区分组' },
}

export default function SubTemplates() {
  const list = useLoad(() => get<SubTemplate[]>('/sub-templates'))
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const [fmt, setFmt] = useState('clash')
  const [sel, setSel] = useState('')
  const [content, setContent] = useState('')
  const [nameTpl, setNameTpl] = useState('')
  const [create, setCreate] = useState<{ name: string; copy: boolean } | null>(null)
  const [rename, setRename] = useState<string | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ofFmt = (list.data ?? []).filter((t) => t.format === fmt)
  const cur = ofFmt.find((t) => t.id === sel) ?? ofFmt.find((t) => t.isDefault) ?? ofFmt[0]
  useEffect(() => { if (cur) { setSel(cur.id); setContent(cur.content) } }, [cur?.id, cur?.content])
  useEffect(() => { if (settings.data) setNameTpl(settings.data.sub_name_template ?? '') }, [settings.data])
  const fmts = Object.keys(formats).filter((f) => (list.data ?? []).some((t) => t.format === f))

  return (
    <>
      <div className="top">
        <div><h1>订阅设置</h1><div className="sub">按客户端 UA 或 ?format= 输出：通用链接 · Clash · sing-box · Surge · Loon · Quantumult X；每种格式可维护多套规则模板，订阅组可分别选用</div></div>
      </div>
      <SubSettings />
      <section className="card">
        <h2>节点名称模板</h2>
        <div className="row" style={{ marginTop: 12, alignItems: 'flex-end' }}>
          <Field label="模板" hint="变量：{flag} 旗帜 · {region} 地区 · {node} 节点名 · {protocol} 协议 · {tag} 入站标签 · {kind} 入口类型">
            <input className="input mono" style={{ minWidth: 320 }} value={nameTpl} onChange={(e) => setNameTpl(e.target.value)} />
          </Field>
          <button className="btn" type="button" disabled={busy} onClick={() => run(() => put('/settings', { sub_name_template: nameTpl }), '已保存')}>保存</button>
        </div>
      </section>
      <div className="tabs" role="tablist">
        {fmts.map((f) => (
          <button key={f} type="button" role="tab" aria-selected={f === fmt} className={'tab' + (f === fmt ? ' on' : '')} onClick={() => { setFmt(f); setSel('') }}>{formats[f].label}</button>
        ))}
      </div>
      <div className="row">
        {ofFmt.map((t) => (
          <button key={t.id} type="button" className={'chip' + (t.id === cur?.id ? ' on' : '')} onClick={() => { setSel(t.id); setContent(t.content) }}>{t.name}{t.isDefault && t.name !== '默认' ? ' · 默认' : ''}</button>
        ))}
        <button className="btn sm" type="button" onClick={() => setCreate({ name: '', copy: true })}>+ 新模板</button>
      </div>
      {cur && (
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div className="hint">{formats[cur.format]?.hint} · Go text/template 语法 · 最后修改 {datetime(cur.updatedAt)}</div>
          <textarea className="input" style={{ minHeight: 460 }} spellCheck={false} value={content} onChange={(e) => setContent(e.target.value)} aria-label="模板内容" />
          <div className="row">
            <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/sub-templates/' + cur.id, { content }); list.reload() }, '模板已保存（保存前已用示例节点校验）')}>保存</button>
            {!cur.isDefault && <button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await put('/sub-templates/' + cur.id, { content, isDefault: true }); list.reload() }, '已设为默认')}>设为默认</button>}
            <button className="btn" type="button" onClick={() => setRename(cur.name)}>重命名</button>
            <button className="btn" type="button" disabled={busy} onClick={async () => {
              if (await confirm('恢复为内置默认内容？')) run(async () => { await put('/sub-templates/' + cur.id, { reset: true }); list.reload() }, '已恢复默认')
            }}>恢复内置内容</button>
            <span className="spacer" />
            {!cur.isDefault && <button className="btn danger" type="button" onClick={async () => {
              if (await confirm(`删除模板「${cur.name}」？使用它的订阅组会回落到默认模板。`, { danger: true })) run(async () => { await del('/sub-templates/' + cur.id); setSel(''); list.reload() })
            }}>删除</button>}
          </div>
        </section>
      )}
      {create && (
        <Modal title={`新建 ${formats[fmt]?.label} 模板`} onClose={() => setCreate(null)} footer={<>
          <button className="btn" type="button" onClick={() => setCreate(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !create.name} onClick={() => run(async () => {
            const t = await post<SubTemplate>('/sub-templates', { format: fmt, name: create.name, content: create.copy ? content : '' })
            setCreate(null); list.reload(); setSel(t.id)
          }, '已创建')}>创建</button>
        </>}>
          <Field label="名称"><input className="input" value={create.name} onChange={(e) => setCreate({ ...create, name: e.target.value })} placeholder="精简规则 / 游戏分流" /></Field>
          <label className="chk"><input type="checkbox" checked={create.copy} onChange={(e) => setCreate({ ...create, copy: e.target.checked })} />复制当前模板内容（不勾选则使用内置默认内容）</label>
        </Modal>
      )}
      {rename !== null && cur && (
        <Modal title="重命名模板" onClose={() => setRename(null)} footer={<>
          <button className="btn" type="button" onClick={() => setRename(null)}>取消</button>
          <button className="btn pri" type="button" disabled={busy || !rename} onClick={() => run(async () => { await put('/sub-templates/' + cur.id, { name: rename }); setRename(null); list.reload() })}>保存</button>
        </>}>
          <Field label="名称"><input className="input" value={rename} onChange={(e) => setRename(e.target.value)} /></Field>
        </Modal>
      )}
    </>
  )
}

const settingKeys = ['sub_enabled', 'sub_base_url', 'sub_title', 'sub_update_interval', 'sub_support_url', 'sub_show_info', 'sub_show_bot', 'sub_announce', 'sub_routing_enabled', 'sub_routing_rules'] as const

const routingExample = `{
  "Name": "直连国内",
  "GlobalProxy": "true",
  "DomesticDNSType": "DoH",
  "DomesticDNSIP": "223.5.5.5",
  "DirectSites": ["geosite:cn", "geosite:private"],
  "DirectIp": ["geoip:cn", "geoip:private"],
  "BlockSites": ["geosite:category-ads-all"]
}`

function SubSettings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const { busy, run } = useAction()
  const confirm = useConfirm()
  useEffect(() => { if (s.data) setF(Object.fromEntries(settingKeys.map((k) => [k, String(s.data?.[k] ?? '')]))) }, [s.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const on = (k: string, def = false) => (f[k] === '' || f[k] === undefined ? def : f[k] === 'true')
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2>订阅与信息页</h2>
        <label className="row small">对外提供订阅<Switch label="启用订阅" on={on('sub_enabled', true)} onChange={(v) => set('sub_enabled', String(v))} /></label>
      </div>
      {!on('sub_enabled', true) && <div className="note warn">已关闭：所有 /sub/ 链接返回 404，客户端将无法更新订阅。</div>}
      <div className="form-grid">
        <Field label="订阅域名" hint="订阅链接使用的地址，可与后台分开（如走 CDN 的独立域名）；留空用公开地址"><input className="input mono" value={f.sub_base_url ?? ''} onChange={(e) => set('sub_base_url', e.target.value)} placeholder={s.data?.public_url || 'https://sub.example.com'} /></Field>
        <Field label="订阅标题" hint="客户端里显示的配置名（profile-title），留空用站点标题"><input className="input" value={f.sub_title ?? ''} onChange={(e) => set('sub_title', e.target.value)} placeholder={s.data?.site_title} /></Field>
        <Field label="客户端自动更新间隔（小时）"><input className="input mono" inputMode="numeric" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
        <Field label="客服 / 支持链接" hint="support-url 头，信息页显示「联系客服」"><input className="input mono" value={f.sub_support_url ?? ''} onChange={(e) => set('sub_support_url', e.target.value)} placeholder="https://t.me/your_support" /></Field>
      </div>
      <Field label="在节点列表中显示用量" hint="不读取 subscription-userinfo 头的客户端也能看到剩余流量和到期">
        <div className="seg c3">
          {([['none', '不显示', '只通过响应头'], ['nodes', '信息节点', '顶部插入「剩余流量 / 到期 / 公告」'], ['suffix', '名称后缀', '每个节点名后加「| 剩余 · 天数」']] as [string, string, string][]).map(([k, l, h]) => (
            <button key={k} type="button" className={'opt' + ((f.sub_show_info || 'none') === k ? ' on' : '')} onClick={() => set('sub_show_info', k)}>{l}<small>{h}</small></button>
          ))}
        </div>
      </Field>
      <div className="toggle"><span>信息页显示 Telegram 一键绑定（机器人运行时）</span><Switch label="机器人绑定" on={on('sub_show_bot', true)} onChange={(v) => set('sub_show_bot', String(v))} /></div>
      <Field label="公告" hint="显示在订阅信息页顶部，并通过 announce 头推送给支持的客户端（Happ、Streisand 等）；开启「信息节点」时也会出现在节点列表">
        <textarea className="input" style={{ minHeight: 70 }} value={f.sub_announce ?? ''} onChange={(e) => set('sub_announce', e.target.value)} placeholder="例：本周六 02:00–04:00 维护，请提前更新订阅" />
      </Field>
      <div className="toggle"><span>下发客户端路由规则（routing 头，Happ 等支持）</span><Switch label="路由" on={on('sub_routing_enabled')} onChange={(v) => set('sub_routing_enabled', String(v))} /></div>
      {on('sub_routing_enabled') && (
        <Field label="路由配置" hint="粘贴 Happ 路由 JSON，或现成的 happ://routing/… 链接；Clash / sing-box 等客户端的分流由上方规则模板决定">
          <textarea className="input mono" style={{ minHeight: 140 }} spellCheck={false} value={f.sub_routing_rules ?? ''} onChange={(e) => set('sub_routing_rules', e.target.value)} placeholder={routingExample} />
        </Field>
      )}
      <div className="row">
        <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
          if (f.sub_routing_enabled === 'true' && f.sub_routing_rules && !f.sub_routing_rules.trim().startsWith('happ://')) JSON.parse(f.sub_routing_rules)
          await put('/settings', f); s.reload()
        }, '订阅设置已保存')}>保存</button>
        <button className="btn" type="button" disabled={busy || !f.sub_announce?.trim()} onClick={async () => {
          if (await confirm('把当前公告通过 Telegram 推送给所有已绑定的用户？')) run(async () => {
            const r = await post<{ queued: number }>('/telegram/broadcast', { text: f.sub_announce }); return r
          }, '已开始推送')
        }}>推送公告到 Telegram</button>
      </div>
    </section>
  )
}
