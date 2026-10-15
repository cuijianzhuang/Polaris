import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { Field, Modal, useAction, useConfirm, useLoad } from '../components/ui'
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
        <div><h1>订阅模板</h1><div className="sub">按客户端 UA 或 ?format= 输出：通用链接 · Clash · sing-box · Surge · Loon · Quantumult X；每种格式可维护多套规则模板，订阅组可分别选用</div></div>
      </div>
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
          <button key={t.id} type="button" className={'chip' + (t.id === cur?.id ? ' on' : '')} onClick={() => { setSel(t.id); setContent(t.content) }}>{t.name}{t.isDefault ? ' · 默认' : ''}</button>
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
