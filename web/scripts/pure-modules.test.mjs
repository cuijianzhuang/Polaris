// Pure helpers used across the admin UI; run with
//   node --import ./scripts/ts-resolve.mjs --experimental-strip-types --test scripts/pure-modules.test.mjs
import test from 'node:test'
import assert from 'node:assert/strict'
import { bytes, daysLeft, inboundSummary, portLabel, uptime } from '../src/format.ts'
import { clusterPoints, countryCode, resolveNodeGeo, ringOffsets } from '../src/nodeGeo.ts'
import { cdnClientPort, cdnOriginPort, cdnOriginTLSAt, cdnScheme } from '../src/cdn.ts'
import { t } from '../src/i18n.ts'

test('bytes and uptime format the way tables show them', () => {
  assert.equal(bytes(0), '0 B')
  assert.equal(bytes(null), '0 B')
  assert.equal(bytes(1023), '1023 B')
  assert.equal(bytes(1536), '1.50 KB')
  assert.equal(bytes(15 * 1024 ** 3), '15.0 GB')
  assert.equal(bytes(150 * 1024 ** 3), '150 GB')
  assert.equal(uptime(0), '—')
  assert.equal(uptime(90061), t('{n} 天', { n: 1 }))
  assert.equal(uptime(7200), t('{n} 小时', { n: 2 }))
  assert.equal(uptime(300), t('{n} 分钟', { n: 5 }))
  assert.equal(daysLeft(null), null)
})

test('inbound labels', () => {
  assert.match(inboundSummary({ protocol: { type: 'vless' }, transport: { type: 'ws' }, security: { type: 'tls' } }), /WS/)
  assert.ok(portLabel({ port: 443 }).includes('443'))
})

test('t interpolates variables and keeps unknown keys verbatim', () => {
  assert.equal(t('{n} 天', { n: 3 }), '3 天')
  assert.equal(t('完全没有翻译的句子 {x}', { x: 'y' }), '完全没有翻译的句子 y')
})

test('node geography prefers coordinates, then city, then country', () => {
  assert.deepEqual(resolveNodeGeo({ lat: 1, lng: 2, flag: 'JP' }), { lat: 1, lng: 2, source: 'coords', cc: 'JP' })
  assert.equal(resolveNodeGeo({ flag: 'JP' })?.source, 'country')
  assert.equal(resolveNodeGeo({ flag: 'ZZ' }), null)
  assert.equal(countryCode({ flag: 'jp' }), 'JP')
  const groups = clusterPoints([{ id: 'a', x: 0, y: 0 }, { id: 'b', x: 3, y: 4 }, { id: 'c', x: 100, y: 100 }], 10)
  assert.deepEqual(groups.map((g) => g.map((p) => p.id)), [['a', 'b'], ['c']])
  assert.deepEqual(ringOffsets(1, 10), [[0, 0]])
  assert.equal(ringOffsets(12, 10).length, 12)
})

test('CDN ports follow the provider, scheme and fronting mode', () => {
  const base = { originMode: '', caddyFronting: false, scheme: 'https', ports: [8443], provider: 'cloudflare', tlsMode: 'full' }
  assert.equal(cdnScheme(base), 'https')
  assert.equal(cdnScheme({ scheme: 'http' }), 'http')
  assert.equal(cdnClientPort(base, 2053), 2053) // a Cloudflare HTTPS port passes through
  assert.equal(cdnClientPort(base, 9999), 8443) // anything else uses the profile port
  assert.equal(cdnClientPort({ ...base, originMode: 'tunnel' }, 2053), 443)
  assert.equal(cdnClientPort({ ...base, caddyFronting: true }, 2053), 8443)
  assert.equal(cdnOriginPort({ ...base, tlsMode: 'flexible' }, 443), 80)
  assert.equal(cdnOriginTLSAt({ ...base, tlsMode: 'flexible' }, 443), false)
  assert.equal(cdnOriginTLSAt(base, 443), true)
})
