import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { Field, useAction, useConfirm, useLoad } from '../components/ui'
import { datetime } from '../format'
import type { SubTemplate } from '../types'

const formats: Record<string, { label: string; hint: string }> = {
  clash: { label: 'Clash / mihomo / Stash', hint: '{{ .Proxies }} 展开为 proxies 列表，{{ .Names }} 为节点名数组，{{ .NamesInline }} 为逗号分隔的节点名' },
  singbox: { label: 'sing-box / SFA / SFI', hint: '{{ .Outbounds }} 展开为出站 JSON 片段（{{ if .HasOutbounds }}判断非空），{{ .Tags }} 为节点 tag 数组' },
  surge: { label: 'Surge', hint: '{{ .Proxies }} 为 [Proxy] 行，{{ .NamesCSV }} 为 ", 节点名" 列表，{{ .URL }} 为订阅地址' },
}

export default function SubTemplates() {
  const list = useLoad(() => get<SubTemplate[]>('/sub-templates'))
  const settings = useLoad(() => get<Record<string, any>>('/settings'))
  const [sel, setSel] = useState<string>('')
  const [content, setContent] = useState('')
  const [nameTpl, setNameTpl] = useState('')
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const cur = list.data?.find((t) => t.id === sel) ?? list.data?.[0]
  useEffect(() => { if (cur) { setSel(cur.id); setContent(cur.content) } }, [cur?.id, cur?.content])
  useEffect(() => { if (settings.data) setNameTpl(settings.data.sub_name_template ?? '') }, [settings.data])

  return (
    <>
      <div className="top">
        <div><h1>订阅模板</h1><div className="sub">按客户端 UA 或 ?format= 输出：通用链接 · Clash · sing-box · Surge；规则组按格式分别维护</div></div>
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
        {(list.data ?? []).map((t) => (
          <button key={t.id} type="button" role="tab" aria-selected={t.id === cur?.id} className={'tab' + (t.id === cur?.id ? ' on' : '')} onClick={() => { setSel(t.id); setContent(t.content) }}>
            {formats[t.format]?.label ?? t.format}
          </button>
        ))}
      </div>
      {cur && (
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div className="hint">{formats[cur.format]?.hint} · Go text/template 语法 · 最后修改 {datetime(cur.updatedAt)}</div>
          <textarea className="input" style={{ minHeight: 460 }} spellCheck={false} value={content} onChange={(e) => setContent(e.target.value)} aria-label="模板内容" />
          <div className="row">
            <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/sub-templates/' + cur.id, { content }); list.reload() }, '模板已保存（保存前已用示例节点校验）')}>保存</button>
            <button className="btn" type="button" disabled={busy} onClick={async () => {
              if (await confirm('恢复为内置默认模板？')) run(async () => { await put('/sub-templates/' + cur.id, { reset: true }); list.reload() }, '已恢复默认')
            }}>恢复默认</button>
          </div>
        </section>
      )}
    </>
  )
}
