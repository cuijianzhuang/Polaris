import assert from 'node:assert/strict'
import test from 'node:test'
import { applyNotificationProfile, notificationGroupMode, notificationGroups, notificationProfile, selectedChannels, setNotificationChannel, setNotificationGroup } from '../src/notificationPolicy.ts'

const channels = ['telegram', 'email', 'webhook']
const events = {
  node_offline: { group: 'node_availability', paired: true, recovery: false },
  node_online: { group: 'node_availability', paired: true, recovery: true },
  backup_failed: { group: 'backup_failed', paired: false, recovery: false },
  health: { group: 'routing_health', paired: true, recovery: false },
  route_changed: { group: 'route_changed', paired: false, recovery: false, defaultOff: true },
}
const defaults = () => ({ repeatWindowSeconds: 300, channels: {}, maintenance: [] })

test('legacy defaults and disabled columns produce the same notification scope', () => {
  assert.equal(notificationProfile(defaults(), events, channels), 'standard')
  assert.deepEqual(selectedChannels(defaults(), events, channels), channels)
  const p = defaults()
  for (const channel of ['email', 'webhook']) p.channels[channel] = Object.fromEntries(Object.keys(events).map((event) => [event, false]))
  assert.deepEqual(selectedChannels(p, events, channels), ['telegram'])
  assert.equal(notificationProfile(p, events, channels), 'standard')
  assert.deepEqual(selectedChannels(applyNotificationProfile(p, events, channels, 'essential'), events, channels), ['telegram'])
})

test('channel gates round-trip custom rules and preserve maintenance and recovery', () => {
  const p = { ...defaults(), notifyRecovery: false, channels: { telegram: { node_online: false, route_changed: true } }, maintenance: [{ id: 'maintenance' }] }
  const before = structuredClone(p)
  const disabled = setNotificationChannel(p, events, channels, 'telegram', false)
  assert.deepEqual(disabled.channels, p.channels)
  const restored = setNotificationChannel(disabled, events, channels, 'telegram', true)
  assert.deepEqual(restored.channels, p.channels)
  assert.equal(restored.notifyRecovery, false)
  assert.deepEqual(restored.maintenance, p.maintenance)
  assert.deepEqual(p, before)
})

test('enabling a legacy empty channel adopts active preferences without enabling default-off events', () => {
  const p = defaults()
  p.channels.email = Object.fromEntries(Object.keys(events).map((event) => [event, false]))
  p.channels.telegram = { health: false }
  p.enabledChannels = { webhook: false }
  const enabled = setNotificationChannel(p, events, channels, 'email', true)
  assert.equal(enabled.channels.email.node_offline, true)
  assert.equal(enabled.channels.email.health, false)
  assert.equal(enabled.channels.email.route_changed, false)
  assert.equal(enabled.enabledChannels.webhook, false)
})

test('presets deliberately replace details but preserve channel and recovery gates', () => {
  const p = { ...defaults(), enabledChannels: { email: false }, notifyRecovery: false }
  const essential = applyNotificationProfile(p, events, channels, 'essential')
  assert.equal(notificationProfile(essential, events, channels), 'essential')
  assert.equal(essential.channels.telegram.route_changed, false)
  assert.equal(essential.channels.telegram.health, false)
  const standard = applyNotificationProfile(essential, events, channels, 'standard')
  assert.equal(notificationProfile(standard, events, channels), 'standard')
  assert.equal(standard.enabledChannels.email, false)
  assert.equal(standard.notifyRecovery, false)
})

test('fault/recovery groups preserve partial preferences until explicitly edited', () => {
  const p = { ...defaults(), channels: { telegram: { node_online: false }, email: { node_offline: false, node_online: true } } }
  const members = ['node_offline', 'node_online']
  assert.equal(notificationGroupMode(p, events, ['telegram'], members), 'fault')
  assert.equal(notificationGroupMode(p, events, ['email'], members), 'custom')
  assert.equal(notificationGroupMode(p, events, channels, members), 'custom')
  const next = setNotificationGroup(p, events, ['telegram'], members, 'all')
  assert.equal(notificationGroupMode(next, events, ['telegram'], members), 'all')
  assert.deepEqual(next.channels.email, p.channels.email)
  assert.equal(next.channels.telegram.route_changed, undefined)
})

test('no destination is silently reenabled and future event groups remain editable', () => {
  const p = { ...defaults(), enabledChannels: Object.fromEntries(channels.map((c) => [c, false])) }
  const next = applyNotificationProfile(p, events, channels, 'standard')
  assert.deepEqual(selectedChannels(next, events, channels), [])
  const groups = notificationGroups({ ...events, future: { group: 'future_group', paired: false, recovery: false } })
  assert.deepEqual(groups[0].events, ['node_offline', 'node_online'])
  assert.ok(groups.some((g) => g.id === 'future_group' && g.events.includes('future')))
  const legacy = defaults()
  legacy.channels.email = Object.fromEntries(Object.keys(events).map((event) => [event, false]))
  const edited = setNotificationGroup(legacy, events, ['email'], ['backup_failed'], 'all')
  assert.equal(edited.enabledChannels.email, false)
  assert.equal(selectedChannels(edited, events, channels).includes('email'), false)
})
