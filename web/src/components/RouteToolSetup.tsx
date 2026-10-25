import { useCallback, useEffect, useRef, useState } from 'react'
import { get, post } from '../api'
import { datetime, errMsg } from '../format'
import { t } from '../i18n'
import { useAction } from './ui'

export type RouteToolStatus = { state: string; issue?: string; path?: string; checkOutput?: string; manager?: string; canInstall: boolean; manualCommand?: string; checkedAt?: string; installState?: string; installOutput?: string; installIssue?: string }

export function routeIssueText(issue?: string): string {
  return ({
    tool_missing: t('节点未安装 traceroute，请点击一键安装。'),
    permission_denied: t('工具执行权限或网络探测权限不足；请检查 Agent 运行用户和容器网络权限。'),
    address_family_unavailable: t('节点不支持此地址族或没有可用源地址；请检查 IPv4/IPv6 配置。'),
    network_unreachable: t('节点没有到目标的可用路由；请检查对应地址族的默认路由。'),
    incompatible_tool: t('traceroute 不支持所需参数；请安装发行版提供的完整 traceroute 工具。'),
    probe_timeout: t('检测超出时间限制，已保留收到的逐跳结果；UDP 或 ICMP 回包可能被过滤。'),
    destination_unreachable: t('路径返回不可达或被策略拒绝；请查看逐跳结果及原始输出。'),
    incomplete_path: t('已收到部分路由响应，但未到达目标；不能据此判定代理业务故障。'),
    no_reply: t('没有收到逐跳响应；请检查 UDP 出站及 ICMP 回包，目标也可能不响应探测。'),
    command_failed: t('traceroute 执行失败，请查看原始输出确认原因。'),
    linux_required: t('回程环境检查和安装仅支持 Linux Agent。'),
    agent_upgrade_required: t('请升级 Linux Agent 以检查和安装 traceroute'),
    package_manager_unavailable: t('未找到支持的包管理器，请在 Agent 所在环境手动安装 traceroute。'),
    root_required: t('Agent 未以 root 运行，无法安装系统包；请手动安装后重新检查。'),
    install_interrupted: t('安装超时或 Agent 连接中断，请检查安装日志和包管理器状态后重试。'),
    install_failed: t('安装失败，请检查软件源、网络或包管理器锁，修复后重试。'),
    install_verification_failed: t('安装命令已完成，但工具验证未通过；请检查执行权限和安装日志。'),
  } as Record<string, string>)[issue ?? ''] ?? issue ?? ''
}

export default function RouteToolSetup({ nodeId, onStatus }: { nodeId: string; onStatus: (status: RouteToolStatus) => void }) {
  const [status, setStatus] = useState<RouteToolStatus>()
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState('')
  const sequence = useRef(0)
  const checkingRef = useRef(false)
  const { busy, run } = useAction()
  const refresh = useCallback(async () => {
    if (checkingRef.current) return
    checkingRef.current = true
    const seq = ++sequence.current
    setChecking(true)
    try {
      const result = await get<RouteToolStatus>(`/nodes/${nodeId}/route-tool`)
      if (seq === sequence.current) { setStatus(result); onStatus(result); setError('') }
    } catch (e) { if (seq === sequence.current) setError(errMsg(e)) }
    finally { if (seq === sequence.current) { checkingRef.current = false; setChecking(false) } }
  }, [nodeId, onStatus])
  useEffect(() => { void refresh(); return () => { sequence.current++; checkingRef.current = false } }, [refresh])
  const installing = status?.installState === 'installing'
  useEffect(() => {
    if (!installing) return
    // Operation polling is independent of the dashboard auto-refresh preference.
    const timer = window.setInterval(() => { void refresh() }, 4000)
    return () => window.clearInterval(timer)
  }, [installing, refresh])
  const install = () => run(async () => {
    const seq = ++sequence.current
    setError('')
    try {
      const result = await post<RouteToolStatus>(`/nodes/${nodeId}/route-tool/install`)
      if (seq === sequence.current) { setStatus(result); onStatus(result) }
    } catch (e) { if (seq === sequence.current) setError(errMsg(e)); throw e }
    finally { if (seq === sequence.current) setChecking(false) }
  })
  return <div className="ops-summary ops-stack" aria-label={t('回程检测环境')}>
    <b>{t('回程检测环境')}</b>
    <div role="status">{installing ? t('正在节点安装 traceroute…') : status?.state === 'ready' ? t('traceroute 已就绪') : status?.state === 'missing' ? t('traceroute 未安装') : status?.state === 'unusable' ? t('traceroute 已安装但不可用') : checking ? t('正在检查节点环境…') : t('环境尚未就绪')}</div>
    {status?.issue && !installing && <div className="hint">{routeIssueText(status.issue)}</div>}
    {status?.state === 'ready' && <div className="hint">{t('工具已通过本机 UDP 自检；公网连通性和 IPv4/IPv6 路由仍以实际检测结果为准。')}</div>}
    {status?.path && <div className="mono small ops-break">{status.path}</div>}
    {status?.installState === 'succeeded' && <div className="note ok-note">{t('traceroute 安装并验证成功')}</div>}
    {status?.installIssue && <div className="note warn" role="alert">{routeIssueText(status.installIssue)}</div>}
    {error && <div className="note warn" role="alert">{error}</div>}
    <div className="row"><button className="btn sm" disabled={busy || checking} onClick={() => void refresh()}>{t('重新检查环境')}</button>{status?.canInstall && <button className="btn pri sm" disabled={busy || checking || installing} onClick={install}>{t('一键安装 traceroute')}</button>}</div>
    {status?.canInstall && <div className="hint">{t('仅在节点安装 traceroute 系统包，最长 3 分钟；不会自动修改防火墙或网络权限。')}</div>}
    {status?.manualCommand && status.state !== 'ready' && !installing && <details><summary>{t('手动安装指令')}</summary><pre className="code ops-raw">{status.manualCommand}</pre><div className="hint">{t('在 Agent 所在主机或容器内以 root 执行。')}</div></details>}
    {status?.checkOutput && <details><summary>{t('环境自检输出')}</summary><pre className="code ops-raw">{status.checkOutput}</pre></details>}
    {status?.installOutput && <details><summary>{t('安装日志')}</summary><pre className="code ops-raw">{status.installOutput}</pre></details>}
    {status?.checkedAt && <div className="small mute">{t('检查时间')}：{datetime(status.checkedAt)}</div>}
  </div>
}
