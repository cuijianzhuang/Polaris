import shell from './shell'
import g1 from './g1'
import g2 from './g2'
import g3 from './g3'
import g4 from './g4'
import g5 from './g5'
import g6 from './g6'
import g7 from './g7'
import g8 from './g8'
import g9 from './g9'
import core from './core'
import operations from './operations'
import appearance from './appearance'
import recovery from './recovery'
import adminRoles from './adminRoles'
import nodeBrowser from './nodeBrowser'
import userOnline from './userOnline'
import cdnSchedule from './cdnSchedule'
import cdnTunnel from './cdnTunnel'
import cdnTunnelAgent from './cdnTunnelAgent'
import cdnRemoteToken from './cdnRemoteToken'
import cdnPoolEditor from './cdnPoolEditor'
import websites from './websites'
import ai from './ai'

// English UI strings keyed by the Chinese source text, split by area so
// files can be edited independently. shell holds the first-paint keys
// (see i18n.ts); it is spread first so the files it was moved from keep
// their original precedence for any key that still appears in both.
const en: Record<string, string> = { ...shell, ...core, ...g1, ...g2, ...g3, ...g4, ...g5, ...g6, ...g7, ...g8, ...g9, ...operations, ...appearance, ...recovery, ...adminRoles, ...nodeBrowser, ...userOnline, ...cdnSchedule, ...cdnTunnel, ...cdnTunnelAgent, ...cdnRemoteToken, ...cdnPoolEditor, ...websites, ...ai }
export default en
