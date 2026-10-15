// REST client. The panel injects window.__POLARIS_BASE__ (the secret admin
// path) into index.html; the API lives at <base>api/v1.
declare global {
  interface Window { __POLARIS_BASE__?: string }
}

export const BASE = (window.__POLARIS_BASE__ ?? '/').replace(/\/?$/, '/')
export const API = BASE + 'api/v1'

const TOKEN_KEY = 'polaris.token'

export function getToken(): string | null {
  try { return localStorage.getItem(TOKEN_KEY) } catch { return null }
}
export function setToken(t: string | null) {
  try { t ? localStorage.setItem(TOKEN_KEY, t) : localStorage.removeItem(TOKEN_KEY) } catch { /* private mode */ }
}

export class ApiError extends Error {
  status: number
  data: any
  constructor(status: number, message: string, data?: any) {
    super(message)
    this.status = status
    this.data = data
  }
}

let onUnauthorized: () => void = () => {}
export function setUnauthorizedHandler(fn: () => void) { onUnauthorized = fn }

export async function api<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  const tok = getToken()
  if (tok) headers['Authorization'] = 'Bearer ' + tok
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const resp = await fetch(API + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  if (resp.status === 204) return undefined as T
  const text = await resp.text()
  let data: any = undefined
  try { data = text ? JSON.parse(text) : undefined } catch { data = text }
  if (!resp.ok) {
    if (resp.status === 401 && !path.startsWith('/auth/login')) onUnauthorized()
    throw new ApiError(resp.status, (data && data.error) || resp.statusText, data)
  }
  return data as T
}

export const get = <T = any>(p: string) => api<T>('GET', p)
export const post = <T = any>(p: string, b?: unknown) => api<T>('POST', p, b ?? {})
export const put = <T = any>(p: string, b?: unknown) => api<T>('PUT', p, b ?? {})
export const del = <T = any>(p: string) => api<T>('DELETE', p)

// wsURL builds the realtime endpoint URL (token passed as query parameter).
export function wsURL(): string {
  const u = new URL(API + '/ws', location.href)
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:'
  u.searchParams.set('token', getToken() ?? '')
  return u.toString()
}
