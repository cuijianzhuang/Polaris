import { useEffect, useRef, useState } from 'react'
import { api, get, put } from '../../api'
import { t, tr } from '../../i18n'
import { Field, Switch, useConfirm, useLoad } from '../../components/ui'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from '../../components/SettingsDrafts'
import { AIWorkbench } from '../../components/AIAssistant'

type Config = { enabled: boolean; portalEnabled: boolean; provider: string; baseUrl: string; format: string; model: string; apiKey?: string; knowledge: string; portalDailyLimit: number; dailyBudget: number; maxOutputTokens: number; allowPrivate: boolean }
type Provider = { id: string; name: string; baseUrl: string; format: string }
type Settings = { config: Config; keySet: boolean; revision: string; providers: Provider[] }
type Model = { id: string; display_name?: string; created?: number; created_at?: string }

export default function AISettings() {
  const load = useLoad((signal) => get<Settings>('/ai/settings', signal)), [saved, setSaved] = useState<Settings>(), [form, setForm] = useState<Config>(), [clearKey, setClearKey] = useState(false), [models, setModels] = useState<Model[]>([]), [modelSearch, setModelSearch] = useState(''), [modelsNote, setModelsNote] = useState(''), [modelError, setModelError] = useState(''), [fetching, setFetching] = useState(false), [testing, setTesting] = useState(false), [testNote, setTestNote] = useState(''), [reloadError, setReloadError] = useState('')
  const saver = useSettingsSave(), confirm = useConfirm(), request = useRef<AbortController | null>(null), serial = useRef(0), seen = useRef(''), testRequest = useRef<AbortController | null>(null)
  const accept = (data: Settings) => { setSaved(data); setForm({ ...data.config, apiKey: '' }); setClearKey(false); setTestNote(''); setReloadError('') }
  useEffect(() => { if (load.data && !saved) accept(load.data) }, [load.data, saved])
  useEffect(() => () => { serial.current++; request.current?.abort(); testRequest.current?.abort() }, [])
  const dirty = !!form && !!saved && (JSON.stringify(form) !== JSON.stringify({ ...saved.config, apiKey: '' }) || clearKey)
  const locked = saver.busy || testing
  const invalid = !!form && (!form.baseUrl.trim() || (form.enabled && !form.model.trim()) || !Number.isInteger(form.portalDailyLimit) || form.portalDailyLimit < 1 || form.portalDailyLimit > 200 || !Number.isInteger(form.dailyBudget) || form.dailyBudget < 1 || form.dailyBudget > 10000 || !Number.isInteger(form.maxOutputTokens) || form.maxOutputTokens < 256 || form.maxOutputTokens > 8192)
  const update = (patch: Partial<Config>) => { serial.current++; request.current?.abort(); request.current = null; setFetching(false); setModels([]); setModelSearch(''); setModelsNote(''); setModelError(''); setTestNote(''); setForm((old) => old && ({ ...old, ...patch })) }
  const fetchModels = async (config = form) => {
    if (!config || request.current) return
    const seq = ++serial.current, abort = new AbortController(); request.current = abort; setFetching(true); setModelError('')
    const timer = setTimeout(() => abort.abort(), 25000)
    try {
      const result = await api<{ models: Model[]; note: string }>('POST', '/ai/models', { config, clearKey }, {}, abort.signal)
      if (seq === serial.current) { setModels(result.models); setModelSearch(''); setModelsNote(result.note) }
    } catch (e) { if (seq === serial.current) setModelError(tr(e instanceof Error ? e.message : t('模型列表获取失败'))) }
    finally { clearTimeout(timer); if (seq === serial.current) { request.current = null; setFetching(false) } }
  }
  useEffect(() => { if (saved && (saved.keySet || saved.config.provider === 'custom') && seen.current !== saved.revision) { seen.current = saved.revision; void fetchModels({ ...saved.config, apiKey: '' }) } }, [saved])
  const filteredModels = models.filter((model) => `${model.id} ${model.display_name || ''}`.toLowerCase().includes(modelSearch.trim().toLowerCase()))
  const save = async () => {
    if (!dirty) return true
    if (!form || !saved || locked || invalid) return false
    return saver.run(async () => { accept(await put<Settings>('/ai/settings', { config: form, revision: saved.revision, clearKey })) }, t('AI 设置已保存'))
  }
  useSettingsDraft({ label: t('AI 助手'), dirty, busy: saver.busy, invalid, save, discard: () => { if (saved) accept(saved) } })
  if (!form || !saved) return <section className="card"><p className="hint">{load.loading ? t('正在读取设置…') : load.error || t('设置读取失败')}</p><button className="btn" type="button" onClick={load.reload}>{t('重试')}</button></section>
  return <div className="ai-settings">
    <section className="card settings-fields"><div className="ctitle"><h2>{t('AI 助手')}</h2><button className="btn sm" type="button" disabled={locked} onClick={async () => { if (!dirty || await confirm(t('重新加载会丢弃尚未保存的 AI 设置，继续吗？'))) { try { const latest = await get<Settings>('/ai/settings'); accept(latest) } catch (e) { setReloadError(tr(e instanceof Error ? e.message : t('设置读取失败'))) } } }}>{t('重新加载')}</button></div>
      {reloadError && <div className="note bad" role="alert">{reloadError}</div>}
      <p className="hint">{t('配置模型服务后，可辅助创建入站、诊断故障、检查配置、汇总运维及起草工单回复。')}</p>
      <fieldset className="settings-fields" disabled={locked}>
        <div className="toggle"><span>{t('启用 AI 助手')}</span><Switch label={t('启用 AI 助手')} on={form.enabled} onChange={(enabled) => update({ enabled })} /></div>
        <div className="toggle"><span>{t('开放用户中心 AI 客服')}</span><Switch label={t('开放用户中心 AI 客服')} on={form.portalEnabled} onChange={(portalEnabled) => update({ portalEnabled })} /></div>
        <div className="form-grid"><Field label={t('模型服务')}><select className="input" value={form.provider} onChange={(e) => { const p = saved.providers.find((p) => p.id === e.target.value)!; update({ provider: p.id, baseUrl: p.baseUrl, format: p.format, model: '', apiKey: '' }); setClearKey(false) }}>{saved.providers.map((p) => <option key={p.id} value={p.id}>{p.id === 'custom' ? t('自定义') : p.id === 'qwen' ? 'Qwen' : p.name}</option>)}</select></Field><Field label={t('接口格式')}><select className="input" value={form.format} disabled={form.provider !== 'custom'} onChange={(e) => update({ format: e.target.value })}><option value="openai">OpenAI Chat Completions</option><option value="anthropic">Claude Messages</option></select></Field></div>
        <Field label={t('API 基础地址')} hint={t('填写 API 根地址，例如以 /v1 结尾；本地服务需允许私网来源。')}><input className="input mono" value={form.baseUrl} maxLength={512} onChange={(e) => update({ baseUrl: e.target.value })} onBlur={() => { if (form.apiKey || saved.keySet && form.baseUrl === saved.config.baseUrl && form.provider === saved.config.provider) void fetchModels() }} placeholder="https://api.openai.com/v1" /></Field>
        <label className="chk"><input type="checkbox" checked={form.allowPrivate} onChange={(e) => update({ allowPrivate: e.target.checked })} />{t('允许 HTTP／私网模型服务')}</label>
        <Field label="API Key" hint={saved.keySet ? t('已加密保存，留空保留；更换服务或地址时需重新填写或明确清除。') : t('密钥只保存在服务端；自建匿名服务可留空。')}><input className="input mono" type="password" autoComplete="new-password" value={form.apiKey || ''} maxLength={4096} onChange={(e) => update({ apiKey: e.target.value })} onBlur={() => { if (form.apiKey) void fetchModels() }} /></Field>
        {saved.keySet && <label className="chk"><input type="checkbox" checked={clearKey} onChange={(e) => { setClearKey(e.target.checked); update({ apiKey: '' }) }} />{t('清除已保存的 API Key')}</label>}
        <Field label={t('模型名称')} htmlFor="ai-model-id" hint={t('服务商实时返回当前模型；也可直接填写模型 ID。选择后用连接测试确认权限与兼容性。')}><div className="row"><input className="input mono" id="ai-model-id" value={form.model} maxLength={160} onFocus={() => { if (!models.length && !fetching && (form.apiKey || saved.keySet || form.provider === 'custom')) void fetchModels() }} onChange={(e) => setForm({ ...form, model: e.target.value })} placeholder={t('选择或填写模型 ID')} /><button className="btn" type="button" disabled={fetching} onClick={() => fetchModels()}>{fetching ? t('正在获取…') : t('刷新模型列表')}</button></div></Field>
        {models.length > 0 && <section className="ai-model-picker" aria-label={t('已获取的模型')}>
          <div className="ai-model-picker-heading">{t('已获取的模型')}<span className="hint">{t('点击模型即可选用，保存后生效')}</span></div>
          {models.length > 8 && <input className="input" type="search" aria-label={t('搜索模型')} placeholder={t('搜索模型')} value={modelSearch} onChange={(e) => setModelSearch(e.target.value)} />}
          <div className="ai-model-options">{filteredModels.map((model) => <button className="ai-model-option" type="button" key={model.id} aria-pressed={form.model === model.id} onClick={() => setForm({ ...form, model: model.id })}>
            <span><span className="mono">{model.id}</span>{model.display_name && model.display_name !== model.id && <small className="hint">{model.display_name}</small>}</span><span className="ai-model-check" aria-hidden="true">{form.model === model.id ? '✓' : ''}</span>
          </button>)}</div>
          {!filteredModels.length && <p className="hint" role="status">{t('没有匹配的模型，可直接填写模型 ID')}</p>}
        </section>}
        {modelsNote && <div className="hint" role="status">{t('已获取 {n} 个模型', { n: models.length })} · {tr(modelsNote)}</div>}{modelError && <div className="note warn" role="alert">{modelError}</div>}
        <div className="form-grid"><Field label={t('每位用户每日额度')}><input className="input mono" type="number" min={1} max={200} value={form.portalDailyLimit} onChange={(e) => setForm({ ...form, portalDailyLimit: Number(e.target.value) })} /></Field><Field label={t('全站每日调用额度')}><input className="input mono" type="number" min={1} max={10000} value={form.dailyBudget} onChange={(e) => setForm({ ...form, dailyBudget: Number(e.target.value) })} /></Field></div>
        <Field label={t('每次输出额度（tokens）')} hint={t('推理模型的额度也包含思考过程；范围为 256–8192。')}><input className="input mono" type="number" min={256} max={8192} value={form.maxOutputTokens} onChange={(e) => setForm({ ...form, maxOutputTokens: Number(e.target.value) })} /></Field>
        <Field label={t('站点 FAQ')} hint={t('补充客服可引用的站点规则与常见问题。项目使用文档和运维文档随版本内置，回答展示文档摘录来源。')}><textarea className="input" rows={6} maxLength={3500} value={form.knowledge} onChange={(e) => setForm({ ...form, knowledge: e.target.value })} /></Field>
      </fieldset>
      <SettingsSaveBar dirty={dirty} busy={saver.busy} error={saver.error} disabled={invalid} onSave={save} label={t('保存 AI 设置')} />
      <div className="row"><button className="btn" type="button" disabled={locked || dirty || !saved.config.model} onClick={async () => { setTesting(true); setTestNote(''); const abort = new AbortController(); testRequest.current = abort; const timer = setTimeout(() => abort.abort(), 55000); try { await api('POST', '/ai/test', {}, {}, abort.signal); setTestNote(t('模型连接与文字生成测试通过')) } catch (e) { setTestNote(tr(e instanceof Error ? e.message : t('连接测试失败'))) } finally { clearTimeout(timer); testRequest.current = null; setTesting(false) } }}>{testing ? t('正在测试…') : t('测试模型连接')}</button><span className="hint">{t('保存后测试，会发起一次简短的模型调用。')}</span></div>{testNote && <p role="status">{testNote}</p>}
    </section>
    {saved.config.enabled && <AIWorkbench />}
  </div>
}
