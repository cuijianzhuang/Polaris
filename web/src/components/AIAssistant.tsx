import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, get } from '../api'
import { useNodes } from '../App'
import { Field, useLoad } from './ui'
import type { CoreType, Protocol, Security, Transport } from '../types'
import { t, tr } from '../i18n'
import { datetime } from '../format'

export type AIPlan = { core: CoreType; protocol: Protocol; transport: Transport; security: Security; port: number; serverName?: string; tag?: string; certificateId?: string }
type Mode = 'diagnose' | 'review' | 'summary' | 'ticket' | 'draft'
type Source = { id: string; title: string; document: string; excerpt: string }
type Answer = { text: string; facts: string[]; sources: Source[]; draft?: AIPlan; checkedAt: string }

export function AIComposer({ mode, nodeId, inboundId, ticketId, draft, onDraft, onReply }: {
  mode: Mode; nodeId?: string; inboundId?: string; ticketId?: string; draft?: AIPlan
  onDraft?: (p: AIPlan) => void; onReply?: (text: string) => void
}) {
  const [prompt, setPrompt] = useState(''), [logs, setLogs] = useState(''), [answer, setAnswer] = useState<Answer>(), [error, setError] = useState(''), [busy, setBusy] = useState(false)
  const controller = useRef<AbortController | null>(null), generation = useRef(0)
  const context = JSON.stringify({ mode, nodeId, inboundId, ticketId, draft }), current = useRef(context); current.current = context
  useEffect(() => { generation.current++; controller.current?.abort(); controller.current = null; setBusy(false); setAnswer(undefined); setError('')
    return () => { generation.current++; controller.current?.abort() }
  }, [context])
  const ask = async () => {
    if (controller.current || !prompt.trim()) return
    const seq = ++generation.current, contextAtStart = context, abort = new AbortController(); controller.current = abort
    const timer = window.setTimeout(() => abort.abort(), 55000)
    setBusy(true); setError(''); setAnswer(undefined)
    try {
      const result = await api<Answer>('POST', '/ai/ask', { mode, nodeId: nodeId || undefined, inboundId, ticketId, draft, prompt, logs: mode === 'diagnose' ? logs : undefined }, {}, abort.signal)
      if (seq === generation.current && contextAtStart === current.current) setAnswer(result)
    } catch (e) { if (seq === generation.current) setError(e instanceof DOMException && e.name === 'AbortError' ? t('请求已停止或超时，请重试') : tr(e instanceof Error ? e.message : t('请求失败'))) }
    finally { clearTimeout(timer); if (seq === generation.current) { controller.current = null; setBusy(false) } }
  }
  return <div className="ai-composer">
    <p className="hint">{t('结合项目知识库和当前服务状态生成建议；请勿粘贴密码、订阅链接或付款凭据。')}</p>
    <Field label={t('问题或需求')} htmlFor={`ai-prompt-${mode}`}><textarea id={`ai-prompt-${mode}`} className="input" rows={3} maxLength={1800} value={prompt} disabled={busy} onChange={(e) => setPrompt(e.target.value)} placeholder={mode === 'draft' ? t('例如：为当前节点创建支持 REALITY 的 VLESS 入站，避开已占用端口') : mode === 'summary' ? t('例如：汇总当前异常和近期工单，按影响范围给出处理顺序') : t('描述现象、错误提示或希望检查的内容')} /></Field>
    {mode === 'diagnose' && <Field label={t('客户端日志（可选）')}><textarea className="input mono" rows={4} maxLength={3000} value={logs} disabled={busy} onChange={(e) => setLogs(e.target.value)} /></Field>}
    <div className="row"><button className="btn pri" type="button" disabled={busy || !prompt.trim() || (['draft', 'review', 'diagnose'].includes(mode) && !nodeId)} onClick={ask}>{busy ? t('正在分析…') : mode === 'draft' ? t('生成入站草稿') : t('开始分析')}</button>{busy && <button className="btn" type="button" onClick={() => controller.current?.abort()}>{t('停止')}</button>}</div>
    {error && <div className="note bad" role="alert">{error}</div>}
    {answer && <section className="ai-answer" aria-live="polite">
      <div className="ai-text">{answer.text}</div>
      {answer.draft && <><dl className="kv"><dt>{t('内核')}</dt><dd>{answer.draft.core}</dd><dt>{t('协议')}</dt><dd>{answer.draft.protocol} · {answer.draft.transport} · {answer.draft.security}</dd><dt>{t('端口')}</dt><dd>{answer.draft.port}</dd></dl>{onDraft && <button className="btn pri" type="button" onClick={() => onDraft(answer.draft!)}>{t('应用到入站表单')}</button>}</>}
      {onReply && <button className="btn" type="button" onClick={() => onReply(answer.text)}>{t('填入回复草稿')}</button>}
      {!!answer.facts.length && <details><summary>{t('状态依据')}</summary><ul>{answer.facts.map((fact, i) => <li key={i}>{fact}</li>)}</ul><small>{datetime(answer.checkedAt)}</small></details>}
      {!!answer.sources.length && <details><summary>{t('知识库来源')}</summary>{answer.sources.map((source) => <article key={source.id}><b>[{source.id}] {source.title}</b><p className="hint">{source.document}</p><p className="ai-text">{source.excerpt}</p></article>)}</details>}
    </section>}
  </div>
}

export function AIInbound({ nodeId, inboundId, draft, onApply }: { nodeId: string; inboundId?: string; draft: AIPlan; onApply: (p: AIPlan) => void }) {
  const status = useLoad((signal) => get<{ enabled: boolean }>('/ai/status', signal))
  const [mode, setMode] = useState<Mode>(inboundId ? 'review' : 'draft')
  if (!status.data?.enabled) return null
  return <details className="ai-inline"><summary>{t('AI 辅助创建与检查')}</summary><div className="ai-composer"><Field label={t('任务')}><select className="input" value={mode} onChange={(e) => setMode(e.target.value as Mode)}>{!inboundId && <option value="draft">{t('创建入站草稿')}</option>}<option value="review">{t('配置检查')}</option><option value="diagnose">{t('故障诊断')}</option></select></Field><AIComposer mode={mode} nodeId={nodeId} inboundId={inboundId} draft={mode === 'review' ? draft : undefined} onDraft={onApply} /></div></details>
}

export function AITicket({ id, onReply }: { id: string; onReply: (text: string) => void }) {
  const status = useLoad((signal) => get<{ enabled: boolean }>('/ai/status', signal))
  return status.data?.enabled ? <details className="ai-inline"><summary>{t('AI 工单辅助')}</summary><AIComposer mode="ticket" ticketId={id} onReply={onReply} /></details> : null
}

export function AIWorkbench() {
  const { nodes } = useNodes(), [nodeId, setNodeId] = useState(''), [inboundId, setInboundId] = useState(''), [mode, setMode] = useState<Mode>('summary')
  const inbounds = useLoad((signal) => nodeId ? get<{ id: string; tag: string }[]>('/inbounds?node=' + encodeURIComponent(nodeId), signal) : Promise.resolve([]), [nodeId])
  return <section className="card ai-workbench"><div className="ctitle"><h2>{t('AI 运维助手')}</h2><Link className="btn sm" to="/inbounds">{t('创建入站草稿')}</Link></div><div className="form-grid"><Field label={t('任务')}><select className="input" value={mode} onChange={(e) => setMode(e.target.value as Mode)}><option value="summary">{t('运维摘要')}</option><option value="diagnose">{t('故障诊断')}</option><option value="review">{t('配置检查')}</option></select></Field>{mode !== 'summary' && <Field label={t('节点')}><select className="input" value={nodeId} onChange={(e) => { setNodeId(e.target.value); setInboundId('') }}><option value="">{t('请选择')}</option>{nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select></Field>}{mode !== 'summary' && <Field label={t('入站（可选）')}><select className="input" value={inboundId} onChange={(e) => setInboundId(e.target.value)}><option value="">{t('全部')}</option>{(inbounds.data ?? []).map((inbound) => <option key={inbound.id} value={inbound.id}>{inbound.tag}</option>)}</select></Field>}</div><AIComposer mode={mode} nodeId={mode === 'summary' ? undefined : nodeId} inboundId={mode === 'summary' ? undefined : inboundId || undefined} /></section>
}
