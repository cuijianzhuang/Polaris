// Installed plugins, shared by the sidebar (their pages) and the plugin
// manager. Loaded once; refreshPlugins() reloads after a change.
import { useEffect, useState } from 'react'
import { get } from './api'

export type PluginPage = { id: string; title: string; icon?: string; path: string }
export type PluginSetting = { key: string; label: string; type: string; required?: boolean; default?: string; hint?: string; options?: string[] }
export type PluginManifest = {
  id: string; name: string; version: string; description?: string; author?: string; homepage?: string; minPanel?: string
  scopes?: string[]; events?: string[]; pages?: PluginPage[]; portal?: { title: string; path: string }[]; settings?: PluginSetting[]
  subscription?: { formats?: { name: string; title: string; contentType: string }[]; transform?: boolean }
}
export type PluginInfo = {
  id: string; name: string; version: string; baseUrl: string; enabled: boolean; status: string; lastSeenAt?: string; lastError: string; createdAt: string
  manifest: PluginManifest; values: Record<string, string>; hasSecret: Record<string, boolean>
}
export type PluginList = { plugins: PluginInfo[]; limit: number; used: number; transformOrder?: string[] }

let cache: PluginList | null = null
let pending: Promise<void> | null = null
const subs = new Set<(l: PluginList | null) => void>()

function load(): Promise<void> {
  if (!pending) {
    pending = get<PluginList>('/plugins')
      .then((l) => { cache = l; subs.forEach((f) => f(l)) })
      .catch(() => { /* not an admin, or the panel is old: no plugin pages */ })
      .finally(() => { pending = null })
  }
  return pending
}

export function refreshPlugins(): Promise<void> { return load() }

// usePlugins returns the installed plugins (null until loaded). enabled=false
// skips the request, for restricted sessions.
export function usePlugins(enabled = true): PluginList | null {
  const [list, setList] = useState<PluginList | null>(cache)
  useEffect(() => {
    if (!enabled) return
    subs.add(setList)
    if (!cache) void load()
    return () => { subs.delete(setList) }
  }, [enabled])
  return list
}
