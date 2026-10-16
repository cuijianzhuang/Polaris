import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { BrowserRouter, NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { BASE, get, getToken, onInflight, post, setToken, setUnauthorizedHandler, takeOAuthResult, wsURL } from './api'
import { Icon } from './components/Icons'
import { useTheme } from './theme'
import { ConfirmProvider, ToastProvider } from './components/ui'
import type { Node } from './types'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import Nodes from './pages/Nodes'
import Cores from './pages/Cores'
import Forwards from './pages/Forwards'
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

// Scope of the session: Telegram node operators see only their own nodes.
const ScopeCtx = createContext<{ scoped: boolean }>({ scoped: false })
export function useScope() { return useContext(ScopeCtx) }

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

type NavItem = { to: string; label: string; icon: ReactNode; end?: boolean; children?: { to: string; label: string; icon: ReactNode }[] }

const nav: NavItem[] = [
  { to: '/', label: '仪表盘', icon: <Icon.Dashboard />, end: true },
  { to: '/nodes', label: '节点与内核', icon: <Icon.Nodes />, children: [
    { to: '/nodes', label: '节点', icon: <Icon.Server /> },
    { to: '/cores', label: '内核', icon: <Icon.Cpu /> },
  ] },
  { to: '/inbounds', label: '入站', icon: <Icon.Inbound /> },
  { to: '/chains', label: '链路与 CDN', icon: <Icon.Chain /> },
  { to: '/forwards', label: '端口转发', icon: <Icon.Forward /> },
  { to: '/templates', label: '入站模板', icon: <Icon.Template /> },
  { to: '/users', label: '用户', icon: <Icon.Users /> },
  { to: '/sub-templates', label: '订阅设置', icon: <Icon.Sub /> },
  { to: '/convert', label: '订阅转换', icon: <Icon.Convert /> },
  { to: '/domains', label: '域名与 DDNS', icon: <Icon.Globe /> },
]
const sysNav = [
  { to: '/certificates', label: '证书', icon: <Icon.Lock /> },
  { to: '/telegram', label: 'Telegram 机器人', icon: <Icon.Bot /> },
  { to: '/settings', label: '设置', icon: <Icon.Gear /> },
  { to: '/audit', label: '审计日志', icon: <Icon.Log /> },
]

// TopProgress is a thin bar shown while API requests are in flight (after a
// short delay, so fast requests do not flicker).
function TopProgress() {
  const [show, setShow] = useState(false)
  useEffect(() => {
    let t: ReturnType<typeof setTimeout> | undefined
    const off = onInflight((n) => {
      clearTimeout(t)
      if (n > 0) t = setTimeout(() => setShow(true), 150)
      else setShow(false)
    })
    return () => { off(); clearTimeout(t) }
  }, [])
  return <div className={'topbar' + (show ? ' on' : '')} aria-hidden="true" />
}

// NavGroup is a collapsible sidebar entry; it opens itself while one of its
// pages is shown.
function NavGroup({ item }: { item: NavItem }) {
  const loc = useLocation()
  const here = item.children!.some((c) => loc.pathname === c.to || loc.pathname.startsWith(c.to + '/'))
  const [open, setOpen] = useState(here)
  useEffect(() => { if (here) setOpen(true) }, [here])
  return (
    <div className={'navgroup' + (open ? ' open' : '')}>
      <button type="button" className={'nav' + (here && !open ? ' active' : '')} aria-expanded={open} onClick={() => setOpen(!open)}>
        {item.icon}{item.label}<span className="chev" aria-hidden="true"><Icon.Chevron /></span>
      </button>
      <div className="navsub">
        {item.children!.map((c) => <NavLink key={c.to} to={c.to} className="nav">{c.icon}{c.label}</NavLink>)}
      </div>
    </div>
  )
}

// ThemeToggle cycles light → dark → follow system; accents live in 设置.
function ThemeToggle() {
  const [t, setT] = useTheme()
  const next = { light: 'dark', dark: 'system', system: 'light' } as const
  const label = { light: '浅色', dark: '深色', system: '跟随系统' }[t.mode]
  return (
    <button type="button" className="theme-btn" title={`主题：${label}（点击切换）`} aria-label={`主题：${label}`}
      onClick={() => setT({ ...t, mode: next[t.mode] })}>
      {t.mode === 'light' ? <Icon.Sun /> : t.mode === 'dark' ? <Icon.Moon /> : <Icon.Monitor />}
    </button>
  )
}

// Bottom tab bar on phones: the pages used most, plus 更多 for the full menu.
const tabs = [
  { to: '/', label: '仪表盘', icon: <Icon.Dashboard />, end: true },
  { to: '/nodes', label: '节点', icon: <Icon.Server /> },
  { to: '/inbounds', label: '入站', icon: <Icon.Inbound /> },
  { to: '/users', label: '用户', icon: <Icon.Users /> },
]

function Layout({ user, onLogout, children }: { user: string; onLogout: () => void; children: ReactNode }) {
  const [title, setTitle] = useState('Polaris')
  const [menu, setMenu] = useState(false) // the sidebar, as a drawer on phones
  useEffect(() => { get<{ title: string }>('/info').then((i) => setTitle(i.title)).catch(() => {}) }, [])
  useEffect(() => { document.title = title }, [title])
  const loc = useLocation()
  useEffect(() => { setMenu(false) }, [loc.pathname])
  useEffect(() => {
    if (!menu) return
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setMenu(false) }
    document.addEventListener('keydown', esc)
    document.body.classList.add('noscroll')
    return () => { document.removeEventListener('keydown', esc); document.body.classList.remove('noscroll') }
  }, [menu])
  const { scoped } = useScope()
  const inTabs = tabs.some((t) => (t.end ? loc.pathname === t.to : loc.pathname.startsWith(t.to)))
  return (
    <div className="app">
      <TopProgress />
      <header className="mtop">
        <button type="button" className="mtop-btn" aria-label="打开菜单" aria-expanded={menu} onClick={() => setMenu(true)}><Icon.Menu /></button>
        <div className="mtop-title"><Icon.Logo /><span>{title}</span></div>
        <ThemeToggle />
      </header>
      <div className={'side-mask' + (menu ? ' on' : '')} onClick={() => setMenu(false)} aria-hidden="true" />
      <nav className={'side' + (menu ? ' open' : '')} aria-label="主导航">
        <div className="brand"><Icon.Logo />{title}<button type="button" className="side-close" aria-label="关闭菜单" onClick={() => setMenu(false)}><Icon.Close /></button></div>
        {scoped ? (
          <>
            <NavLink to="/nodes" className="nav"><Icon.Server />我的节点</NavLink>
            <div className="navsec">Telegram 节点管理员 · 仅可查看分配给你的节点</div>
          </>
        ) : (
          <>
            {nav.map((n) => n.children ? <NavGroup key={n.label} item={n} /> : <NavLink key={n.to} to={n.to} end={n.end} className="nav">{n.icon}{n.label}</NavLink>)}
            <div className="navsec">系统</div>
            {sysNav.map((n) => <NavLink key={n.to} to={n.to} className="nav">{n.icon}{n.label}</NavLink>)}
          </>
        )}
        <div className="side-foot"><span>{user}</span><span className="row" style={{ gap: 6, flexWrap: 'nowrap' }}><ThemeToggle /><button type="button" onClick={onLogout}>退出</button></span></div>
      </nav>
      <main className="main" key={loc.pathname}>{children}</main>
      {!scoped && <nav className="mtabs" aria-label="快捷导航">
        {tabs.map((t) => <NavLink key={t.to} to={t.to} end={t.end} className="mtab">{t.icon}<span>{t.label}</span></NavLink>)}
        <button type="button" className={'mtab' + (!inTabs || menu ? ' active' : '')} onClick={() => setMenu(true)}><Icon.More /><span>更多</span></button>
      </nav>}
    </div>
  )
}

export default function App() {
  const [user, setUser] = useState<string | null>(() => {
    const r = takeOAuthResult() // GitHub sign-in lands here with a session token
    if (r.token) { setToken(r.token); return r.user ?? '…' }
    if (r.tg) return null // Telegram sign-in link: exchanged below
    return getToken() ? '…' : null
  })
  const [scoped, setScoped] = useState(false)
  const [tgBusy, setTgBusy] = useState(() => !!takeOAuthResult().tg)
  useEffect(() => {
    setUnauthorizedHandler(() => { setToken(null); setUser(null); setScoped(false) })
    const r = takeOAuthResult()
    if (r.tg) {
      setToken(null)
      post<{ token: string; username: string }>('/auth/tg', { code: r.tg })
        .then((x) => { setToken(x.token); setUser(x.username) })
        .catch((e) => { r.error = e instanceof Error ? e.message : String(e) })
        .finally(() => setTgBusy(false))
    }
  }, [])
  useEffect(() => {
    if (!user || !getToken()) return
    get<{ username: string; nodeIds?: string[] }>('/auth/me').then((m) => { setUser(m.username); setScoped(!!m.nodeIds) }).catch(() => {})
  }, [user === null])
  const logout = () => { setToken(null); setUser(null); setScoped(false) }
  if (tgBusy) return <div className="login"><div className="card" style={{ alignItems: 'center' }}>正在通过 Telegram 登录…</div></div>
  return (
    <ToastProvider>
      <ConfirmProvider>
        <BrowserRouter basename={BASE.replace(/\/$/, '') || '/'}>
          {!user ? (
            <Login onLogin={(u) => setUser(u)} />
          ) : (
            <ScopeCtx.Provider value={{ scoped }}>
              <NodesProvider>
                <Layout user={user} onLogout={logout}>
                  {scoped ? (
                    <Routes>
                      <Route path="/nodes" element={<Nodes />} />
                      <Route path="*" element={<Navigate to="/nodes" replace />} />
                    </Routes>
                  ) : (
                    <Routes>
                      <Route path="/" element={<Dashboard />} />
                      <Route path="/nodes" element={<Nodes />} />
                      <Route path="/cores" element={<Cores />} />
                      <Route path="/forwards" element={<Forwards />} />
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
                  )}
                </Layout>
              </NodesProvider>
            </ScopeCtx.Provider>
          )}
        </BrowserRouter>
      </ConfirmProvider>
    </ToastProvider>
  )
}
