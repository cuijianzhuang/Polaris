import { useState } from 'react'
import { get, put } from '../api'
import { t } from '../i18n'
import type { CDNProfile, Node } from '../types'
import { Field, Modal, Switch, useAction, useLoad, usePoll } from './ui'

type State = { credentialMode?: string; remoteSupported?: boolean; activeEntries: number; ready: boolean; enabled: boolean; nodeId: string; hasCredentials: boolean; nodeStatus: string; nodeName: string; pending: boolean; running: boolean; error: string; version: string; cleanupConfirmed: boolean }
const feature = 'cloudflare-tunnel-v1'
const remoteFeature = 'cloudflare-tunnel-token-v1'

export default function CDNTunnelAgent({ profile, onClose, onSaved }: { profile: CDNProfile; onClose: () => void; onSaved: () => void }) {
  const state = useLoad(() => get<State>(`/cdn-profiles/${profile.id}/tunnel-agent`), [profile.id])
  const nodes = useLoad(() => get<Node[]>('/nodes'))
  const [enabled, setEnabled] = useState(!!profile.tunnelManaged), [nodeId, setNodeId] = useState(profile.tunnelNodeId || '')
  const [mode, setMode] = useState(profile.tunnelCredentialMode === 'remote' ? 'remote' : 'local')
  const [token, setToken] = useState('')
  const [credentials, setCredentials] = useState(''), [fileError, setFileError] = useState(''), [fileName, setFileName] = useState('')
  const { busy, run } = useAction()
  usePoll(state.reload, 5000)
  usePoll(nodes.reload, 5000)
  const selected = nodes.data?.find((n) => n.id === nodeId)
  const supports = (n: Node) => n.agentFeatures?.includes(feature) && (mode !== 'remote' || n.agentFeatures?.includes(remoteFeature))
  const supported = selected && supports(selected)
  const moving = enabled && profile.tunnelNodeId && nodeId !== profile.tunnelNodeId && !state.data?.cleanupConfirmed
  const modeChanging = (profile.tunnelCredentialMode || 'local') !== mode && profile.tunnelNodeId && !state.data?.cleanupConfirmed
  const supplied = mode === 'remote' ? token.trim() : credentials
  const canSave = !busy && !state.loading && !state.error && (!enabled || (!fileError && nodeId && supported && (supplied || state.data?.hasCredentials) && !moving && !modeChanging))
  return <Modal title={t('Agent 托管隧道')} onClose={onClose} footer={<>
    <button className="btn" type="button" disabled={busy} onClick={onClose}>{t('取消')}</button>
    <button className="btn pri" type="button" disabled={!canSave} onClick={() => run(async () => {
      await put(`/cdn-profiles/${profile.id}/tunnel-agent`, { enabled, nodeId, credentialMode: mode, ...(enabled && mode === 'remote' && token.trim() ? { token: token.trim() } : {}), ...(enabled && mode === 'local' && credentials ? { credentials } : {}) })
      setCredentials(''); setToken(''); onSaved()
    }, t('托管设置已保存，等待 Agent 同步'))}>{mode === 'remote' && enabled && !profile.tunnelManaged ? t('一键安装并启动') : t('保存')}</button>
  </>}>
    <p className="hint">{t('Agent 自动安装并校验 cloudflared，维护连接器进程。选择本地 JSON 或远程 Token；远程回源规则仍由 Cloudflare 管理。')}</p>
    <Switch label={t('由 Agent 托管 cloudflared')} on={enabled} onChange={setEnabled} disabled={busy} />
    {enabled && <>
      <Field label={t('隧道管理方式')} htmlFor="tunnel-credential-mode"><select id="tunnel-credential-mode" className="input" value={mode} disabled={busy || !!profile.tunnelManaged} onChange={(e) => { setMode(e.target.value); setCredentials(''); setToken(''); setFileError(''); setFileName('') }}>
        <option value="local">{t('本地 JSON（Agent 维护路由）')}</option><option value="remote">{t('远程 Token（Cloudflare 维护路由）')}</option>
      </select></Field>
      <Field label={t('托管节点')} htmlFor="tunnel-agent-node"><select id="tunnel-agent-node" className="input" value={nodeId} disabled={busy || !!profile.tunnelManaged} onChange={(e) => setNodeId(e.target.value)}>
        <option value="">{t('选择节点')}</option>{(nodes.data ?? []).map((n) => <option key={n.id} value={n.id}>{n.name}{!supports(n) ? ' · ' + t('需要升级 Agent') : ''}</option>)}
      </select></Field>
      {nodes.error && <div className="note bad" role="alert">{nodes.error}<button className="link" type="button" onClick={nodes.reload}>{t('重试')}</button></div>}
      {selected && !supported && <p className="note warn">{t('该节点尚未上报隧道托管能力，请升级 Agent 并等待重新连接。')}</p>}
      {mode === 'remote' ? <Field label="Tunnel Token" htmlFor="tunnel-remote-token" hint={state.data?.hasCredentials ? t('Token 已加密保存，留空保留；填写新 Token 会重启此连接器。') : t('只粘贴完整 Token，不要粘贴安装命令。Token 所属 UUID 必须与当前配置档一致。')}><input id="tunnel-remote-token" className="input mono" type="password" disabled={busy} autoComplete="off" spellCheck={false} maxLength={16384} value={token} onChange={(e) => setToken(e.target.value)} /></Field> : <Field label={t('本地隧道凭据 JSON')} hint={state.data?.hasCredentials ? t('已加密保存；不重新导入则保留。重新导入会轮换本地凭据版本并重启连接器。') : t('选择 cloudflared 创建命名隧道时生成的 JSON。远程 Token 不支持自动配置本地回源。')}>
        <input type="file" accept="application/json,.json" disabled={busy} aria-label={t('导入隧道凭据')} onChange={async (e) => {
          const chosen = e.target.files?.[0]; setCredentials(''); setFileName(''); setFileError('')
          if (!chosen) return
          if (chosen.size > 16384) { setFileError(t('凭据文件不能超过 16 KiB')); return }
          try { const raw = await chosen.text(); JSON.parse(raw); setCredentials(raw); setFileName(chosen.name) }
          catch { setFileError(t('无法读取有效的 JSON 凭据文件')) }
        }} />
        {fileName && <div className="small mute">{fileName}</div>}
      </Field>}
      {fileError && <p role="alert" className="note bad">{fileError}</p>}
    </>}
    {state.error && <div className="note bad" role="alert">{state.error}<button className="link" type="button" onClick={state.reload}>{t('重试')}</button></div>}
    {state.data && <div className="note small" role="status">
      {state.data.enabled ? mode !== 'remote' && state.data.activeEntries === 0 ? t('等待启用隧道入站') : state.data.ready ? t('节点隧道连接已就绪') : state.data.pending ? t('配置待应用') : state.data.running ? t('cloudflared 进程已启动') : t('cloudflared 尚未启动') : state.data.cleanupConfirmed ? t('Agent 已确认清理') : t('等待 Agent 停止并清理本机凭据')}
      {state.data.nodeName && <div>{state.data.nodeName} · {state.data.nodeStatus} · {state.data.version || '—'}</div>}
      {(state.data.enabled || state.data.pending) && state.data.error && <div>{state.data.error}</div>}
    </div>}
    {mode === 'remote' && <p className="note small">{t('一键安装只启动连接器，不会创建 Cloudflare 隧道或修改云端路由。请在 Cloudflare 配置公开域名、路径和本机回源地址，再检测实际入口。')}</p>}
    <p className="hint">{t('进程启动不等于隧道已连接；请等待连接就绪，再同步 DNS 并检测入口。停用后，本机清理由 Agent 下次同步执行，离线节点不能立即清理。')}</p>
    <p className="hint">{t('凭据加密保存并仅下发给绑定节点，不进入配置历史或日志。停用会删除面板和本机副本，但不等于在 Cloudflare 吊销凭据；丢失节点时应在 Cloudflare 撤销或更换隧道授权。')}</p>
  </Modal>
}
