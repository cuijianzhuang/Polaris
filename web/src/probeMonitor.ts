// Presentation rules shared by the probe dashboard and its focused tests.
export type ProbeTarget = { id: string; name: string; carrier: string }
export type ProbeLatency = { ms: number; avg: number | null; loss: number; at: string; spark: (number | null)[]; sparkLoss?: (number | null)[]; sparkStart?: string }
export type ProbeNode = {
  id: string; name: string; region: string; flag: string; online: boolean; status: string
  cpu: number; mem: number; rxBps: number; txBps: number; uptime: number; load1: number
  netUsed: number; netLimit: number; netRx?: number; netTx?: number; lastSeen?: string; telemetryFresh?: boolean
  hidden?: boolean; address?: string; agentVersion?: string; latency: Record<string, ProbeLatency>; probeTargets?: ProbeTarget[]
  disk: number; memTotal?: number; diskTotal?: number; cores?: number; os?: string; arch?: string; ipv4: boolean; ipv6: boolean
  price?: string; expireAt?: string
}
export type ProbeView = 'card' | 'ring' | 'table'
export type ProbeFilter = 'all' | 'online' | 'offline' | 'attention'
export type ProbeSort = 'default' | 'name' | 'cpu' | 'network' | 'latency' | 'expiry'
export type LatencyPoint = { t: string; target: string; avg: number | null; min: number; max: number; loss: number; sent?: number; lost?: number }

export const finite = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v)
export const percent = (v: number) => Math.min(100, Math.max(0, finite(v) ? v : 0))
export const validView = (v: string | null): ProbeView => v === 'ring' || v === 'table' ? v : 'card'
export const dateMs = (v?: string) => v ? Date.parse(v) : NaN
export const freshTelemetry = (n: ProbeNode, now: number) => n.online && n.telemetryFresh !== false && finite(dateMs(n.lastSeen)) && now - dateMs(n.lastSeen) <= 90000 && dateMs(n.lastSeen) - now <= 60000
export const freshLatency = (l: ProbeLatency | undefined, now: number) => !!l && finite(dateMs(l.at)) && now - dateMs(l.at) <= 180000 && dateMs(l.at) - now <= 60000
export const daysLeft = (iso: string | undefined, now: number) => finite(dateMs(iso)) ? dateMs(iso) <= now ? -Math.max(1, Math.ceil((now - dateMs(iso)) / 864e5)) : Math.ceil((dateMs(iso) - now) / 864e5) : null
export const nodeTargets = (n: ProbeNode, fallback: ProbeTarget[]) => n.probeTargets ?? fallback
export const attention = (n: ProbeNode, now: number) => !n.online || !freshTelemetry(n, now) || n.status === 'error' || n.cpu >= 85 || n.mem >= 85 || (n.diskTotal && n.disk >= 85) || Object.values(n.latency).some((l) => freshLatency(l, now) && (l.ms <= 0 || l.loss >= 5))

export function filterProbeNodes(nodes: ProbeNode[], q: string, region: string, state: ProbeFilter, sort: ProbeSort, now: number) {
  const search = q.trim().toLocaleLowerCase()
  const result = nodes.filter((n) => (!search || `${n.name} ${n.region} ${n.os ?? ''} ${n.arch ?? ''}`.toLocaleLowerCase().includes(search)) && (!region || n.flag === region) && (state === 'all' || (state === 'online' && n.online) || (state === 'offline' && !n.online) || (state === 'attention' && attention(n, now))))
  const latency = (n: ProbeNode) => {
    if (!n.online) return Infinity
    const ls = Object.values(n.latency).filter((l) => freshLatency(l, now) && l.ms > 0)
    return ls.length ? ls.reduce((a, l) => a + l.ms, 0) / ls.length : Infinity
  }
  const value = (n: ProbeNode) => sort === 'cpu' ? (freshTelemetry(n, now) ? -n.cpu : Infinity) : sort === 'network' ? (freshTelemetry(n, now) ? -(n.rxBps + n.txBps) : Infinity) : sort === 'latency' ? latency(n) : finite(dateMs(n.expireAt)) ? dateMs(n.expireAt) : Infinity
  if (sort === 'name') result.sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }))
  else if (sort !== 'default') result.sort((a, b) => value(a) - value(b) || a.name.localeCompare(b.name))
  return result
}

// Weight 5-minute means by successful samples, never by the number of slots.
export function latencyStats(points: LatencyPoint[]) {
  let sent = 0, lost = 0, sum = 0, successes = 0
  const ok = points.filter((p) => p.avg != null)
  for (const p of points) {
    const count = p.sent ?? 1, failed = p.lost ?? (p.loss / 100) * count, success = Math.max(0, count - failed)
    sent += count; lost += failed
    if (p.avg != null) { sum += p.avg * success; successes += success }
  }
  return { avg: successes ? sum / successes : null, loss: sent ? 100 * lost / sent : null, min: ok.length ? Math.min(...ok.map((p) => p.min)) : null, max: ok.length ? Math.max(...ok.map((p) => p.max)) : null }
}

// Keep real gaps between samples; never join separate observation periods.
export function alignedTimes(values: string[], maxSlots = 2017) {
  const times = [...new Set(values.map((v) => Date.parse(v)).filter(Number.isFinite))].sort((a, b) => a - b)
  if (!times.length) return []
  const step = 300000, end = times[times.length - 1], start = Math.max(times[0], end - (maxSlots - 1) * step)
  return Array.from({ length: Math.floor((end - start) / step) + 1 }, (_, i) => start + i * step)
}
