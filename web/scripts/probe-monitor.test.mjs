import test from 'node:test'
import assert from 'node:assert/strict'
import { alignedTimes, attention, daysLeft, filterProbeNodes, freshLatency, freshTelemetry, latencyStats, nodeTargets, validView } from '../src/probeMonitor.ts'

const now = Date.parse('2026-10-26T12:00:00Z')
const node = (extra = {}) => ({ id: 'a', name: 'Tokyo 2', region: 'Tokyo', flag: 'JP', online: true, status: 'online', cpu: 0, mem: 0, disk: 0, diskTotal: 100, rxBps: 0, txBps: 0, uptime: 60, load1: 0, netUsed: 0, netLimit: 0, lastSeen: new Date(now).toISOString(), latency: {}, ...extra })

test('fresh zero readings remain measured; missing, stale and future samples stay unknown', () => {
  assert.equal(freshTelemetry(node(), now), true)
  assert.equal(attention(node(), now), false)
  for (const extra of [{ lastSeen: undefined }, { lastSeen: 'invalid' }, { lastSeen: new Date(now - 91000).toISOString() }, { lastSeen: new Date(now + 61000).toISOString() }, { telemetryFresh: false }, { online: false }]) assert.equal(freshTelemetry(node(extra), now), false)
  assert.equal(freshLatency({ at: new Date(now - 180001).toISOString() }, now), false)
  assert.equal(freshLatency({ at: new Date(now).toISOString(), ms: 0 }, now), true)
})

test('search, country and state filters compose without changing the input order', () => {
  const nodes = [node(), node({ id: 'b', name: 'Tokyo 10', online: false }), node({ id: 'c', name: 'Frankfurt', flag: 'DE', os: 'Debian', cpu: 90 }), node({ id: 'd', telemetryFresh: false })]
  assert.deepEqual(filterProbeNodes(nodes, ' DEBIAN ', 'DE', 'attention', 'default', now).map((n) => n.id), ['c'])
  assert.deepEqual(filterProbeNodes(nodes, '', '', 'offline', 'default', now).map((n) => n.id), ['b'])
  assert.deepEqual(filterProbeNodes(nodes, '', 'JP', 'attention', 'default', now).map((n) => n.id), ['b', 'd'])
  assert.equal(nodes[0].id, 'a')
})

test('resource sorting keeps stale values after real values and expiry treats invalid dates as unknown', () => {
  const nodes = [node({ id: 'a', cpu: 40 }), node({ id: 'b', cpu: 99, telemetryFresh: false }), node({ id: 'c', cpu: 80 })]
  assert.deepEqual(filterProbeNodes(nodes, '', '', 'all', 'cpu', now).map((n) => n.id), ['c', 'a', 'b'])
  assert.equal(daysLeft('invalid', now), null)
  assert.equal(daysLeft(new Date(now - 3600000).toISOString(), now), -1)
  assert.equal(daysLeft(new Date(now + 864e5).toISOString(), now), 1)
})

test('history statistics weight uneven sample counts and preserve all-failed intervals', () => {
  const stats = latencyStats([{ avg: 10, min: 10, max: 10, sent: 10, lost: 0, loss: 0 }, { avg: 100, min: 100, max: 100, sent: 10, lost: 9, loss: 90 }, { avg: null, min: 0, max: 0, sent: 5, lost: 5, loss: 100 }])
  assert.equal(stats.avg, 200 / 11)
  assert.equal(stats.loss, 56)
  assert.equal(stats.min, 10)
  assert.equal(stats.max, 100)
  assert.equal(latencyStats([]).loss, null)
  assert.equal(latencyStats([{ avg: null, sent: 2, lost: 2, loss: 100 }]).avg, null)
})

test('chart timelines retain missing five-minute slots and stay bounded', () => {
  assert.deepEqual(alignedTimes(['2026-10-26T11:40:00Z', '2026-10-26T11:50:00Z']), [now - 1200000, now - 900000, now - 600000])
  assert.deepEqual(alignedTimes(['invalid']), [])
  assert.equal(alignedTimes(['2000-01-01T00:00:00Z', '2026-10-26T12:00:00Z']).length, 2017)
})

test('a node with probes disabled does not inherit global targets and saved view values are validated', () => {
  assert.deepEqual(nodeTargets(node({ probeTargets: [] }), [{ id: 'global' }]), [])
  assert.equal(validView('ring'), 'ring')
  assert.equal(validView('unknown'), 'card')
})
