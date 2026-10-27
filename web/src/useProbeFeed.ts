import { useCallback, useEffect, useRef, useState } from 'react'
import { get } from './api'
import { usePoll } from './components/ui'
import { errMsg } from './format'

// Serialize live requests: a response slower than the polling interval must
// still reach the page. Bound stalled requests, and abort on navigation.
export function useProbeFeed<T>(path: string, ms: number, liveNodes?: unknown) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const loadRef = useRef<() => void>(() => {})
  const started = useRef(-Infinity)
  useEffect(() => {
    let active = true, pending: AbortController | null = null
    setData(null)
    setError('')
    const load = async () => {
      if (pending) return
      const controller = new AbortController()
      pending = controller
      started.current = performance.now()
      setLoading(true)
      const timeout = window.setTimeout(() => controller.abort(), 8000)
      try {
        const result = await get<T>(path, controller.signal)
        if (active) { setData(result); setError('') }
      } catch (e) {
        if (active) setError(errMsg(e))
      } finally {
        window.clearTimeout(timeout)
        pending = null
        if (active) setLoading(false)
      }
    }
    loadRef.current = () => { void load() }
    void load()
    return () => { active = false; pending?.abort(); loadRef.current = () => {} }
  }, [path])
  const reload = useCallback(() => loadRef.current(), [])
  usePoll(reload, ms)
  // Reuse the app's existing WebSocket as an invalidation signal. Read the
  // authoritative probe projection, preserving privacy and traffic modes.
  useEffect(() => {
    if (liveNodes !== undefined && ms && !document.hidden && performance.now() - started.current >= ms) reload()
  }, [liveNodes, ms, reload])
  return { data, error, loading, reload }
}
