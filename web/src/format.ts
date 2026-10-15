import type { CoreType, Inbound, Protocol, Security, Transport } from './types'

export function bytes(n: number | undefined | null): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(v >= 10 ? 1 : 2)} ${u[i]}`
}

export function bps(n: number | undefined): string {
  if (!n) return '0'
  if (n >= 1e9) return (n / 1e9).toFixed(2) + ' Gbps'
  if (n >= 1e6) return (n / 1e6).toFixed(1) + ' Mbps'
  if (n >= 1e3) return (n / 1e3).toFixed(0) + ' Kbps'
  return n + ' bps'
}

export function ago(iso?: string | null): string {
  if (!iso) return '—'
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return `${Math.round(s)} 秒前`
  if (s < 3600) return `${Math.round(s / 60)} 分钟前`
  if (s < 86400) return `${Math.round(s / 3600)} 小时前`
  return `${Math.round(s / 86400)} 天前`
}

export function date(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export function datetime(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${date(iso)} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function daysLeft(iso?: string | null): number | null {
  if (!iso) return null
  return Math.ceil((new Date(iso).getTime() - Date.now()) / 86400000)
}

export function uptime(sec?: number): string {
  if (!sec) return '—'
  const d = Math.floor(sec / 86400)
  if (d > 0) return `${d} 天`
  const h = Math.floor(sec / 3600)
  if (h > 0) return `${h} 小时`
  return `${Math.floor(sec / 60)} 分钟`
}

export const coreLabel: Record<string, string> = {
  xray: 'Xray-core', singbox: 'sing-box', mihomo: 'mihomo', hysteria: 'Hysteria2', forwarder: '转发器 (realm)', auto: '自动',
}
export const coreShort: Record<string, string> = { xray: 'Xray', singbox: 'sing-box', mihomo: 'mihomo', hysteria: 'Hy2', forwarder: 'realm', auto: 'auto' }
export const coreColor: Record<string, string> = { xray: '#0E7490', singbox: '#6D28D9', mihomo: '#B45309', hysteria: '#15803D', forwarder: '#5B6470' }

export const protoLabel: Record<Protocol, string> = {
  vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', shadowsocks: 'SS 2022', hysteria2: 'Hysteria2', tuic: 'TUIC v5',
  anytls: 'AnyTLS', shadowtls: 'ShadowTLS', naive: 'Naive', socks: 'SOCKS', http: 'HTTP',
}
export const transLabel: Record<Transport, string> = { tcp: 'TCP', ws: 'WS', grpc: 'gRPC', httpupgrade: 'HTTPUpgrade', xhttp: 'XHTTP', kcp: 'mKCP', quic: 'QUIC' }
export const secLabel: Record<Security, string> = { none: '无', tls: 'TLS', reality: 'REALITY' }

export function inboundSummary(i: Pick<Inbound, 'protocol' | 'transport' | 'security'>): string {
  const p = protoLabel[i.protocol.type] + (i.protocol.flow ? ' + Vision' : '')
  return `${p} · ${transLabel[i.transport.type] ?? i.transport.type} · ${secLabel[i.security.type] ?? i.security.type}`
}

export function portLabel(i: Pick<Inbound, 'port' | 'network'>): string {
  if (i.network === 'udp') return `${i.port}/udp`
  return String(i.port)
}

export function coreTagClass(c: CoreType | string): string {
  return c === 'xray' ? 'tag xray' : c === 'singbox' ? 'tag singbox' : 'tag'
}

export const nodeStatus: Record<string, { label: string; color: string; cls: string }> = {
  online: { label: '在线', color: 'var(--ok)', cls: 'ok' },
  syncing: { label: '同步中', color: 'var(--warn)', cls: 'warn' },
  error: { label: '应用失败', color: 'var(--bad)', cls: 'bad' },
  offline: { label: '离线', color: 'var(--bad)', cls: 'bad' },
  pending: { label: '待接入', color: 'var(--faint)', cls: 'off' },
}

export const userStatus: Record<string, { label: string; cls: string }> = {
  active: { label: '正常', cls: 'ok' },
  limited: { label: '已超额', cls: 'bad' },
  expired: { label: '已到期', cls: 'bad' },
  disabled: { label: '已禁用', cls: 'off' },
}

export function isOnline(iso?: string | null): boolean {
  return !!iso && Date.now() - new Date(iso).getTime() < 120000
}

export const GB = 1024 ** 3

export function flag(code?: string): string {
  if (!code || code.length !== 2) return ''
  const c = code.toUpperCase()
  return String.fromCodePoint(0x1f1e6 + c.charCodeAt(0) - 65, 0x1f1e6 + c.charCodeAt(1) - 65)
}

export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
