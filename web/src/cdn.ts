import type { CDNProfile } from './types'

export const cloudflareHTTPPorts = [80, 8080, 8880, 2052, 2082, 2086, 2095]
export const cloudflareHTTPSPorts = [443, 2053, 2083, 2087, 2096, 8443]
export const cdnScheme = (p?: Pick<CDNProfile, 'scheme'>) => p?.scheme === 'http' ? 'http' : 'https'
export const cdnPorts = (scheme: string) => scheme === 'http' ? cloudflareHTTPPorts : cloudflareHTTPSPorts
export const cdnClientPort = (p: CDNProfile, port: number) => p.originMode === 'tunnel' ? 443 : (p.caddyFronting || !cdnPorts(cdnScheme(p)).includes(port)) ? (p.ports[0] ?? (cdnScheme(p) === 'http' ? 80 : 443)) : port
export const cdnOriginTLSAt = (p: CDNProfile, port: number) => p.originMode !== 'tunnel' && cdnScheme(p) === 'https' && !(p.tlsMode === 'flexible' && (p.provider !== 'cloudflare' || port === 443))
export const cdnOriginPort = (p: CDNProfile, port: number) => p.provider === 'cloudflare' && cdnScheme(p) === 'https' && p.tlsMode === 'flexible' && port === 443 ? 80 : port
