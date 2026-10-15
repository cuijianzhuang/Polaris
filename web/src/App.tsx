import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { BrowserRouter, NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { BASE, get, getToken, setToken, setUnauthorizedHandler, wsURL } from './api'
import { Icon } from './components/Icons'
import { ConfirmProvider, ToastProvider } from './components/ui'
import type { Node } from './types'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import Nodes from './pages/Nodes'
import Inbounds from './pages/Inbounds'
import Chains from './pages/Chains'
import Templates from './pages/Templates'
import Users from './pages/Users'
import SubTemplates from './pages/SubTemplates'
import Certificates from './pages/Certificates'
import Settings from './pages/Settings'
import Audit from './pages/Audit'
import Convert from './pages/Convert'
import Domains from './pages/Domains'
import Telegram from './pages/Telegram'

// Live node list shared by pages, fed by the WebSocket.
const NodesCtx = createContext<{ nodes: Node[]; refresh: () => void }>({ nodes: [], refresh: () => {} })
export function useNodes() { return useContext(NodesCtx) }

function NodesProvider({ children }: { children: ReactNode }) {
  const [nodes, setNodes] = useState<Node[]>([])
  const refresh = useCallback(() => { get<Node[]>('/nodes').then(setNodes).catch(() => {}) }, [])
  useEffect(() => {
    refresh()
    let ws: WebSocket | null = null
    let stop = false
    let retry: ReturnType<typeof setTimeout>
    const connect = () => {
      ws = new WebSocket(wsURL())
      ws.onmessage = (e) => {
        try {
          const m = JSON.parse(e.data)
          if (m.type === 'nodes') setNodes(m.data)
        } catch { /* ignore */ }
      }
      ws.onclose = () => { if (!stop) retry = setTimeout(connect, 5000) }
    }
    connect()
    return () => { stop = true; clearTimeout(retry); ws?.close() }
  }, [refresh])
  return <NodesCtx.Provider value={{ nodes, refresh }}>{children}</NodesCtx.Provider>
}

const nav = [
  { to: '/', label: '仪表盘', icon: <Icon.Dashboard />, end: true },
  { to: '/nodes', label: '节点与内核', icon: <Icon.Nodes /> },
  { to: '/inbounds', label: '入站', icon: <Icon.Inbound /> },
  { to: '/chains', label: '链路与 CDN', icon: <Icon.Chain /> },
  { to: '/templates', label: '入站模板', icon: <Icon.Template /> },
  { to: '/users', label: '用户', icon: <Icon.Users /> },
  { to: '/sub-templates', label: '订阅模板', icon: <Icon.Sub /> },
  { to: '/convert', label: '订阅转换', icon: <Icon.Convert /> },
  { to: '/domains', label: '域名与 DDNS', icon: <Icon.Globe /> },
]
const sysNav = [
  { to: '/certificates', label: '证书', icon: <Icon.Lock /> },
  { to: '/telegram', label: 'Telegram 机器人', icon: <Icon.Bot /> },
  { to: '/settings', label: '设置', icon: <Icon.Gear /> },
  { to: '/audit', label: '审计日志', icon: <Icon.Log /> },
]

function Layout({ user, onLogout, children }: { user: string; onLogout: () => void; children: ReactNode }) {
  const [title, setTitle] = useState('Polaris')
  useEffect(() => { get<{ title: string }>('/info').then((i) => setTitle(i.title)).catch(() => {}) }, [])
  useEffect(() => { document.title = title }, [title])
  return (
    <div className="app">
      <nav className="side" aria-label="主导航">
        <div className="brand"><Icon.Logo />{title}</div>
        {nav.map((n) => <NavLink key={n.to} to={n.to} end={n.end} className="nav">{n.icon}{n.label}</NavLink>)}
        <div className="navsec">系统</div>
        {sysNav.map((n) => <NavLink key={n.to} to={n.to} className="nav">{n.icon}{n.label}</NavLink>)}
        <div className="side-foot"><span>{user}</span><button type="button" onClick={onLogout}>退出</button></div>
      </nav>
      <main className="main">{children}</main>
    </div>
  )
}

export default function App() {
  const [user, setUser] = useState<string | null>(getToken() ? '…' : null)
  useEffect(() => {
    setUnauthorizedHandler(() => { setToken(null); setUser(null) })
    if (getToken()) get<{ username: string }>('/auth/me').then((m) => setUser(m.username)).catch(() => {})
  }, [])
  const logout = () => { setToken(null); setUser(null) }
  return (
    <ToastProvider>
      <ConfirmProvider>
        <BrowserRouter basename={BASE.replace(/\/$/, '') || '/'}>
          {!user ? (
            <Login onLogin={(u) => setUser(u)} />
          ) : (
            <NodesProvider>
              <Layout user={user} onLogout={logout}>
                <Routes>
                  <Route path="/" element={<Dashboard />} />
                  <Route path="/nodes" element={<Nodes />} />
                  <Route path="/inbounds" element={<Inbounds />} />
                  <Route path="/chains" element={<Chains />} />
                  <Route path="/templates" element={<Templates />} />
                  <Route path="/users" element={<Users />} />
                  <Route path="/sub-templates" element={<SubTemplates />} />
                  <Route path="/certificates" element={<Certificates />} />
                  <Route path="/settings" element={<Settings />} />
                  <Route path="/audit" element={<Audit />} />
                  <Route path="/convert" element={<Convert />} />
                  <Route path="/domains" element={<Domains />} />
                  <Route path="/telegram" element={<Telegram />} />
                  <Route path="*" element={<Navigate to="/" replace />} />
                </Routes>
              </Layout>
            </NodesProvider>
          )}
        </BrowserRouter>
      </ConfirmProvider>
    </ToastProvider>
  )
}
