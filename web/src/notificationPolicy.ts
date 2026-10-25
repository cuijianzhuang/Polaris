export type MaintenanceWindow = { id: string; nodeIds: string[]; channels: string[]; startsAt: string; endsAt: string }
export type NotificationPolicy = {
  repeatWindowSeconds: number
  enabledChannels?: Record<string, boolean>
  notifyRecovery?: boolean
  channels: Record<string, Record<string, boolean>>
  maintenance: MaintenanceWindow[]
}
export type EventKind = { group: string; paired: boolean; recovery: boolean; defaultOff?: boolean }
export type EventCatalog = Record<string, EventKind>
export type NotificationProfile = 'essential' | 'standard' | 'custom'
export type GroupMode = 'off' | 'fault' | 'all' | 'custom'

const essential = new Set(['node_offline', 'node_online', 'inbound_down', 'inbound_up', 'apply_failed', 'backup_failed', 'cert_failed', 'operational_alert'])

export const eventEnabled = (policy: NotificationPolicy, events: EventCatalog, channel: string, event: string) => policy.channels[channel]?.[event] ?? !events[event]?.defaultOff

export function selectedChannels(policy: NotificationPolicy, events: EventCatalog, channels: string[]): string[] {
  return channels.filter((channel) => policy.enabledChannels?.[channel] ?? Object.keys(events).some((event) => eventEnabled(policy, events, channel, event)))
}

export function notificationProfile(policy: NotificationPolicy, events: EventCatalog, channels: string[]): NotificationProfile {
  const selected = selectedChannels(policy, events, channels)
  if (!selected.length) return 'custom'
  for (const profile of ['standard', 'essential'] as const) {
    if (selected.every((channel) => Object.keys(events).every((event) => eventEnabled(policy, events, channel, event) === (profile === 'standard' ? !events[event].defaultOff : essential.has(event))))) return profile
  }
  return 'custom'
}

export function applyNotificationProfile(policy: NotificationPolicy, events: EventCatalog, channels: string[], profile: Exclude<NotificationProfile, 'custom'>): NotificationPolicy {
  // A preset is an explicit replacement of detail preferences. Opening the
  // page, changing a channel gate or silencing recovery never normalizes them.
  const selected = new Set(selectedChannels(policy, events, channels))
  return { ...policy, enabledChannels: { ...policy.enabledChannels, ...Object.fromEntries(channels.map((channel) => [channel, selected.has(channel)])) }, channels: { ...policy.channels, ...Object.fromEntries(channels.map((channel) => [channel, Object.fromEntries(Object.keys(events).map((event) => [event, profile === 'standard' ? !events[event].defaultOff : essential.has(event)]))])) } }
}

export function setNotificationChannel(policy: NotificationPolicy, events: EventCatalog, channels: string[], channel: string, enabled: boolean): NotificationPolicy {
  let preferences = policy.channels
  if (enabled && !Object.keys(events).some((event) => eventEnabled(policy, events, channel, event))) {
    // An old policy may contain an entirely disabled column. Enabling that
    // destination should adopt existing active events, rather than send nothing.
    const peers = selectedChannels(policy, events, channels).filter((peer) => peer !== channel)
    const rules = Object.fromEntries(Object.keys(events).map((event) => [event, peers.length ? peers.some((peer) => eventEnabled(policy, events, peer, event)) : !events[event].defaultOff]))
    preferences = { ...preferences, [channel]: rules }
  }
  return { ...policy, channels: preferences, enabledChannels: { ...policy.enabledChannels, [channel]: enabled } }
}

export function notificationGroups(events: EventCatalog): { id: string; events: string[] }[] {
  const ordered = ['node_availability', 'inbound_availability', 'plugin_availability', 'apply_failed', 'backup_failed', 'cert_failed', 'routing_health', 'node_traffic', 'operational_alert', 'route_changed']
  const groups = new Map<string, string[]>()
  for (const [event, kind] of Object.entries(events)) {
    const id = kind.group || event
    groups.set(id, [...(groups.get(id) ?? []), event])
  }
  return [...groups].sort(([a], [b]) => (ordered.indexOf(a) < 0 ? ordered.length : ordered.indexOf(a)) - (ordered.indexOf(b) < 0 ? ordered.length : ordered.indexOf(b)) || a.localeCompare(b)).map(([id, members]) => ({ id, events: members }))
}

export function notificationGroupMode(policy: NotificationPolicy, events: EventCatalog, channels: string[], members: string[]): GroupMode {
  if (!channels.length) return 'off'
  if (channels.every((channel) => members.every((event) => !eventEnabled(policy, events, channel, event)))) return 'off'
  if (channels.every((channel) => members.every((event) => eventEnabled(policy, events, channel, event)))) return 'all'
  if (members.some((event) => events[event].recovery) && channels.every((channel) => members.every((event) => eventEnabled(policy, events, channel, event) === !events[event].recovery))) return 'fault'
  return 'custom'
}

export function setNotificationGroup(policy: NotificationPolicy, events: EventCatalog, channels: string[], members: string[], mode: Exclude<GroupMode, 'custom'>): NotificationPolicy {
  const selected = new Set(selectedChannels(policy, events, channels))
  return { ...policy, enabledChannels: { ...policy.enabledChannels, ...Object.fromEntries(channels.map((channel) => [channel, selected.has(channel)])) }, channels: { ...policy.channels, ...Object.fromEntries(channels.map((channel) => [channel, { ...policy.channels[channel], ...Object.fromEntries(members.map((event) => [event, mode !== 'off' && (mode !== 'fault' || !events[event].recovery)])) }])) } }
}
