export type OutboundIPStrategy = 'auto' | 'ipv4_only' | 'ipv6_only' | 'prefer_ipv4' | 'prefer_ipv6'
export type CoreType = 'xray' | 'singbox' | 'mihomo' | 'hysteria' | 'forwarder' | 'gost' | 'caddy' | 'auto'
export type Protocol = 'vless' | 'vmess' | 'trojan' | 'shadowsocks' | 'hysteria2' | 'tuic' | 'anytls' | 'snell' | 'shadowtls' | 'naive' | 'socks' | 'http'
export type Transport = 'tcp' | 'ws' | 'grpc' | 'httpupgrade' | 'xhttp' | 'kcp' | 'quic'
export type Security = 'none' | 'tls' | 'reality'

export interface Fallback { inboundId: string; path?: string; alpn?: string; xver?: number }
export interface ProtocolSpec {
  type: Protocol; flow?: string; visionUDP443?: boolean; method?: string; congestionControl?: string
  upMbps?: number; downMbps?: number; obfs?: string; obfsPassword?: string
  handshakeServer?: string; detour?: string; udp?: boolean
  snellAuth?: 'userkey' | 'psk'; snellUserId?: string
  snellVersion?: 5 | 6; snellMode?: string; snellObfs?: string
  fallbacks?: Fallback[]
  decryption?: string; encryption?: string; testSeed?: number[]
}
export interface TransportSpec { type: Transport; path?: string; host?: string; serviceName?: string; mode?: string; headerType?: string; seed?: string; acceptProxyProtocol?: boolean }
export interface SecuritySpec {
  type: Security; serverName?: string; alpn?: string[]; fingerprint?: string; certificateId?: string
  realityDest?: string; realityServerNames?: string[]; realityPrivateKey?: string; realityPublicKey?: string; realityShortIds?: string[]
  realityMaxTimeDiff?: number; realityMinClientVer?: string; realityMaxClientVer?: string; realitySpiderX?: string
  realityMldsa65Seed?: string; realityMldsa65Verify?: string
  realityLimitUpload?: LimitFallback; realityLimitDownload?: LimitFallback
}
export interface LimitFallback { afterBytes?: number; bytesPerSec?: number; burstBytesPerSec?: number }
export interface SniffSpec { enabled: boolean; destOverride?: string[]; routeOnly?: boolean; metadataOnly?: boolean; ipsExcluded?: string[]; domainsExcluded?: string[] }
export interface RealityScanResult {
  address?: string; serverName?: string; certNames?: string[]; asn?: string; org?: string; country?: string
  target: string; ok: boolean; tls?: string; alpn?: string; keyShare?: string; pq: boolean; mldsa?: boolean; certName?: string; issuer?: string; notAfter?: string
  names?: string[]; chainSize?: number; latencyMs?: number; problem?: string
}

export interface Combo { protocol: Protocol; transport: Transport; security: Security }
export interface CapSet { core: CoreType; version: string; combos: Combo[] | null; flows?: string[]; features?: string[]; hotUsers: boolean; outbounds?: string[]; realIp: boolean; balancers?: string[] }

export interface SystemInfo { cpu: number; mem: number; rxBps: number; txBps: number; uptime: number; load1: number; hostname: string }
export interface Core {
  id: string; nodeId: string; type: CoreType; version: string; status: string; pid: number; isDefault: boolean
  startedAt?: string; error: string; installedVersions: string[]; inbounds: number
  runningVersion?: string; prevVersion?: string; noUserTraffic?: boolean
}
export interface Node {
  id: string; name: string; address: string; domain: string; region: string; flag: string; lat?: number | null; lng?: number | null; labels: string[]
  agentVersion: string; status: string; lastSeen?: string; desiredRev: number; appliedRev: number; lastError: string
  defaultCore: CoreType; trafficRate: number; system: SystemInfo; revoked: boolean; sortOrder: number
  cores: Core[]; inboundCount: number; connected: boolean; progress?: NodeProgress
  ddns: boolean; publicIpv4: string; publicIpv6: string; ipChangedAt?: string; noFirewall?: boolean
  level?: number // tier a group needs to reach the node (0 = not tiered)
  // Server traffic this billing cycle (host NICs) against the provider's allowance.
  netLimit?: number; netMode?: '' | 'sum' | 'up' | 'down' | 'max'; netResetDay?: number; netWarnPct?: number
  netOffset?: number; netRx?: number; netTx?: number; netCycleStart?: string; netWarned?: number
  netHealth?: NetCollectionHealth
}

export interface NetCollectionHealth {
  state: 'never' | 'healthy' | 'stale'; mode: 'boot' | 'epoch' | 'legacy' | 'unknown'
  checkedAt: string; lastSampleAt?: string; sampleAgeSeconds?: number; staleAfterSeconds: number
  baselineAt?: string; baselineReason?: string; lastGapAt?: string; lastGapSeconds: number
  receivedAtMonotonic?: number // client receipt time, retained when navigating between pages
}
// What the node's agent is doing right now (applying a revision, installing
// a core, upgrading itself); a finished one stays for about a minute.
export interface NodeProgress { active: boolean; stage: string; done: number; total: number; rev?: number; startedAt: number; error?: string; at: string }
export interface InboundHealth { ok: boolean; latencyMs: number; error: string; fails: number; since: string; checkedAt: string }
export interface Inbound {
  id: string; nodeId: string; core: CoreType; tag: string; listen: string; port: number
  protocol: ProtocolSpec; transport: TransportSpec; security: SecuritySpec; sniffing: SniffSpec; extra?: any
  enabled: boolean; templateId?: string; overriddenFields: string[]; remark: string
  nodeName: string; userCount: number; groupIds: string[]; endpoints: number; network: string; cdnCapable: boolean
  outboundTag: string; speedLimitMbps: number; outboundIPStrategy?: OutboundIPStrategy; relayOnly?: boolean; relayAccess?: { enabled: boolean; proxyProtocol: number; sourceOverrides?: Record<string, string[]> }
  health?: InboundHealth
}
export interface NodeQuota { userId: string; nodeId: string; nodeName: string; extra: number; extraUsed: number; limit: number; used: number }
export interface OnlineDevice { ip: string; nodes: string[]; seen: string; model?: string; os?: string }
export interface User {
  id: string; username: string; uuid: string; password: string; trafficLimit: number; usedUp: number; usedDown: number
  expireAt?: string | null; expireAfterDays: number; ipLimit: number; status: string; subToken: string; resetPolicy: string; resetDay: number
  onlineAt?: string | null; bannedUntil?: string | null; onlineIps: string[]; onlineDevices: OnlineDevice[]; remark: string; groupIds: string[]; createdAt: string
  nodeIds: string[]; inboundIds: string[]; telegramId: number; email?: string; planId?: string; extraTraffic?: number
  renewDays: number; renewMax: number; renewCount: number; deviceLimit: number; speedUpMbps?: number; speedDownMbps?: number; lastSubAt?: string | null; lastSubUa?: string
}
export interface UserDevice { id: string; hwid: string; os: string; model: string; userAgent: string; ip: string; firstSeen: string; lastSeen: string }
export interface Group {
  id: string; name: string; remark: string; inboundIds: string[]; nodeIds: string[]; level: number; userCount: number; isDefault?: boolean; speedUpMbps?: number; speedDownMbps?: number
  // A plan is a group sold as a package.
  plan?: boolean; trafficLimit?: number; days?: number; resetPolicy?: string; resetDay?: number; deviceLimit?: number; ipLimit?: number
  // Self-service port forwarding (professional edition).
  fwdMax?: number; fwdPortLo?: number; fwdPortHi?: number; fwdTraffic?: number; fwdRate?: number; fwdSpeedMbps?: number
  price?: string; fallbackId?: string; fallbackName?: string; sort?: number
  sale?: boolean; priceStars?: number; priceCny?: number; priceUsdt?: number
  nodeRates?: Record<string, number>
}
export interface TrafficPack {
  id: string; name: string; traffic: number; planIds: string[]; sale: boolean; priceStars: number; priceCny: number; priceUsdt: number; sort: number
}
export interface Endpoint { id: string; inboundId: string; kind: 'direct' | 'cdn' | 'relay'; address: string; port: number; sni: string; host: string; path: string; cdnProfileId: string; relayChainId: string; remark: string }
export interface CDNProfile {
  id: string; name: string; provider: string; originDomain: string; tlsMode: string; ports: number[]; preferredIps: string[]
  ipSourceUrl: string; ipTopN: number; expandPerIp: boolean; autoDns: boolean; caddyFronting: boolean; hasToken: boolean
  zoneId: string; lastRefreshAt?: string; lastError: string; inbounds: number
  nodeCheck: boolean; staleHours: number; badIps: string[] | null; checkedAt?: string; reports: CDNIPReport[]
}
export interface CDNIPReport { source: string; at: string; ips: { ip: string; latencyMs: number; loss: number; speed?: number; colo?: string }[] }
export interface ChainHop { nodeId: string; inboundId?: string; port?: number }
export interface HopProbe { nodeId: string; latencyMs: number; ok: boolean; error?: string }
export interface Chain { id: string; name: string; mode: 'l4' | 'proxy'; hops: ChainHop[]; serviceUserId: string; probe: HopProbe[]; probedAt?: string; remark: string }
export interface Outbound { id: string; nodeId: string; tag: string; type: string; settings?: any; nodeRefInboundId: string; sourceNodeId?: string; detour: string; chainId: string; remark: string }
export interface RouteMatch { inbound?: string[]; userGroups?: string[]; users?: string[]; domain?: string[]; domainSuffix?: string[]; domainKeyword?: string[]; geosite?: string[]; geoip?: string[]; ipCidr?: string[]; ruleSets?: string[]; port?: string[]; network?: string }
export interface RuleSet {
  id: string; tag: string; name: string; remark: string; sourceUrl: string; domain: string[]; domainSuffix: string[]; domainKeyword: string[]; ipCidr: string[]
  fetchedAt?: string; fetchError: string; count: number; used: string[]
}
export interface DNSServer { tag: string; address: string; domains?: string[]; detour?: string }
export interface DNSSpec {
  servers: DNSServer[]; strategy?: string; hosts?: Record<string, string[]>; domainStrategy?: string
  fakeip?: { inet4Range?: string; inet6Range?: string; domains?: string[] }
}
export interface DNSConfig { scope: string; mode: string; spec: DNSSpec; updatedAt?: string }
export interface RoutingRule { id: string; nodeId: string; match: RouteMatch; outboundTag: string; priority: number; chainId: string; remark: string }
export interface PortPolicy { mode: 'fixed' | 'range'; fixed?: number; min?: number; max?: number }
export interface EndpointTemplate { kind: string; cdnProfileId?: string; relayChainId?: string; address?: string; port?: number }
export interface TemplateItem {
  outboundIPStrategy?: OutboundIPStrategy
  key: string; name?: string; core: CoreType; protocol: ProtocolSpec; transport: TransportSpec; security: SecuritySpec; sniffing: SniffSpec
  port: PortPolicy; tagPattern: string; cert: string; endpoints: EndpointTemplate[]
}
export interface InboundTemplate { id: string; name: string; builtin: boolean; description: string; items: TemplateItem[]; groups: string[]; boundLabels: string[]; inbounds: number; nodes: number }
export interface PlannedInbound { nodeId: string; nodeName: string; itemKey: string; tag: string; core?: CoreType; port?: number; network?: string; cert: string; entries: string; result: 'new' | 'fix' | 'skip'; reason?: string }
export interface NodePlan { nodeId: string; nodeName: string; summary: string; items: PlannedInbound[]; error?: string; applyStatus?: 'pending' | 'applied' | 'offline' | 'error' | 'skipped'; revision?: number }
export interface TemplateRealityCheck { ok: boolean; from: string; checkedAt: string; results: RealityScanResult[] }
export interface TemplatePlan { planId: string; expiresAt: string; state?: string; plans: NodePlan[]; checks?: Record<string, TemplateRealityCheck>; summary: { nodes: number; create: number; adjusted: number; skipped: number } }
export interface Certificate { id: string; domain: string; provider: string; challenge?: string; nodeId?: string; cert: string; expireAt?: string; autoRenew: boolean; lastError: string; usedBy: number }
export interface Revision { id: number; nodeId: string; rev: number; reason: string; status: string; method: string; error: string; createdAt: string }
export interface AuditLog { id: number; admin: string; action: string; target: string; detail: string; ip: string; createdAt: string }
export interface SubTemplate { id: string; format: string; name: string; content: string; isDefault: boolean; updatedAt: string }

export interface DNSProvider { id: string; name: string; kind: 'cloudflare' | 'webhook'; endpoint: string; hasToken: boolean }
export interface DomainBinding {
  id: string; nodeId: string; providerId: string; domain: string; family: 'ipv4' | 'ipv6' | 'dual'; proxied: boolean; ttl: number
  current: string; status: string; lastError: string; syncedAt?: string
}
export interface Operator {
  type: 'filter' | 'rename' | 'sort' | 'dedup'; disabled?: boolean
  protocols?: string[]; regions?: string[]; include?: string; exclude?: string; dropInfo?: boolean
  pattern?: string; replace?: string; template?: string
  sortBy?: string[]; regionOrder?: string[]; desc?: boolean
}
export interface SubTraffic { upload: number; download: number; total: number; expire: number }
export interface SubSource {
  id: string; name: string; kind: 'url' | 'manual'; url?: string; content?: string; hasUrl?: boolean; hasContent?: boolean; urlHost?: string; revision?: number; userAgent: string; group: string; enabled: boolean
  refreshMin: number; nodeCount: number; userInfo: SubTraffic; fetchedAt?: string; lastError: string; sortOrder: number; remark: string
}
export interface SubProcessingRef { groupId: string; revision: number }
export interface SubProcessingGroup { id: string; name: string; remark: string; operators: Operator[]; revision: number; createdAt: string; updatedAt: string }
export interface SubProfile {
  processingGroups?: SubProcessingRef[]
  id: string; name: string; token: string; enabled: boolean; sourceIds: string[]; panelUserId: string; operators: Operator[]
  useGlobalOps: boolean; templates: Record<string, string>; converter: string; backend: string; backendConfig: string
  scheme: string; skipCert: boolean | null; forceUdp: boolean | null
  downloads: number; remark: string; url: string
}
export interface NodeRow { name: string; protocol: string; server: string; port: number; region: string; source: string; network: string; security: string }
export interface TGAdmin { id: string; chatId: number; name: string; role: 'owner' | 'operator'; nodeIds: string[]; notify: boolean; createdAt: string }
export interface RulePreset { key: string; name: string; description: string; outbound: string; priority: number; rules: RouteMatch[] }

export interface NodeTraffic {
  todayUp: number
  todayDown: number
  monthUp: number
  monthDown: number
  rawUp: number
  rawDown: number
}

export interface SourceReference { id: string; nodeId: string; tag: string; inboundIds: string[] }
export interface SourceNode { id: string; sourceId: string; name: string; protocol: string; server: string; port: number; network: string; security: string; missing: boolean; available: boolean; supported: boolean; reason?: string; source: string; references: SourceReference[] }
export interface SourceRefresh { id: string; revision: number; createdAt: string; unchanged: number; changes: { id: string; name: string; kind: string; fields?: string[] }[] }
