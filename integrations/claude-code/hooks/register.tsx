// Agent Tazzyronth in-terminal UI (plan §7.3): follows `tazzy watch --json`
// for the session's project and draws a live pane, a band above the prompt,
// a status line entry and toasts. Activity reporting is the classic hooks'
// job (hooks.json), not this module's, so nothing is reported twice.

import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { TazzyActivity, TazzySnapshot, TazzySwarm } from '../types'
import { addActivity, agentFeed, agentRows, changes, isActive, latestDiff, leaveToast, splitLines, summary } from './swarm'

const PANE = 'tazzyronth'
const TITLE = 'Tazzyronth'
const RETRY_MS = 15_000
const LOGO = ['▄████▄', ' ████ ', '  ▀▀  ']

const swarm = atom({ plugin: 'tazzyronth', key: 'swarm' } as const, { online: false, snap: null } as TazzySwarm)
const feed = atom({ plugin: 'tazzyronth', key: 'feed' } as const, [] as TazzyActivity[])
const selected = atom({ plugin: 'tazzyronth', key: 'selected' } as const, null as string | null)

type Event = { type: string; data?: unknown }

async function onEvent($: EngineInterface, ev: Event): Promise<void> {
  switch (ev.type) {
    case 'status': {
      const snap = ev.data as TazzySnapshot
      const prev = await read($, swarm)
      for (const text of changes(prev.snap, snap)) $.ui.toast(text, { timeoutMs: 6000 })
      await update($, swarm, () => ({ online: true, snap }))
      break
    }
    case 'activity':
      await update($, feed, list => addActivity(list, ev.data as TazzyActivity))
      break
    case 'agent.leave': {
      const text = leaveToast((ev.data ?? {}) as { agent?: string; reason?: string })
      if (text) $.ui.toast(text, { timeoutMs: 6000 })
      break
    }
    case 'disconnected':
      await update($, swarm, s => ({ ...s, online: false }))
      break
    default:
      return
  }
  const s = await read($, swarm)
  $.ui.status(isActive(s) ? summary(s.snap!, await read($, feed)) : undefined)
}

// Runs `tazzy watch --json` for the session's life; watch itself reconnects
// to the hub, so this only restarts it if it exits (or tazzy is missing).
async function watch($: EngineInterface, dir: string): Promise<void> {
  let rest = ''
  try {
    for await (const { stream, text } of $.process.spawn({ argv: ['tazzy', 'watch', '--json', '-dir', dir] })) {
      if (stream !== 'stdout') continue
      const [lines, more] = splitLines(rest + text)
      rest = more
      for (const line of lines) {
        try {
          await onEvent($, JSON.parse(line) as Event)
        } catch (err) {
          $.ui.log(`tazzyronth: bad watch line: ${String(err)}`, { to: 'debug' })
        }
      }
    }
  } catch (err) {
    $.ui.log(`tazzyronth: tazzy watch: ${String(err)}`, { to: 'debug' })
  }
  try {
    await update($, swarm, s => ({ ...s, online: false }))
    $.ui.status(undefined)
    $.clock.after(RETRY_MS, () => void watch($, dir))
  } catch {
    // The module was unloaded (a reload or the session's end): stop here.
  }
}

const pad = (s: string, n: number) => (s.length >= n ? s.slice(0, n) : s + ' '.repeat(n - s.length))

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'tazzy-pane', description: 'Open the Tazzyronth swarm pane' })
    const started = await next(e)
    if (e.isInteractive) void watch($, e.cwd)
    return started
  })

  on('command.run', { command: 'tazzy-pane' }, async $ => {
    await $.ui.open({ id: PANE, title: TITLE })
    return { text: 'Tazzyronth pane opened.' }
  })

  // When this session starts a swarm, open the pane; if it is drawn, tell
  // tazzy_start (ui: "host") so the launcher opens no external window.
  on('tool.call', async ($, e, next) => {
    if (!String(e.tool).endsWith('__tazzy_start')) return next(e)
    const opened = await $.ui.open({ id: PANE, title: TITLE })
    return next(opened.isPlaced ? ({ ...e, ui: 'host' } as typeof e) : e)
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const s = await read($, swarm)
    if (e.props.hasSurvey || !isActive(s)) return next(e)
    const { Box, Button, Text } = $.ui.resolve(e)
    return (
      <Box flexDirection="row" gap={1}>
        <Text wrap="truncate">{summary(s.snap!, await read($, feed))}</Text>
        <Button key="pane" label="pane" plain onPress={() => void $.ui.open({ id: PANE, title: TITLE })} />
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Button, Text } = $.ui.resolve(e)
    const s = await read($, swarm)
    const list = await read($, feed)
    const width = Math.max(20, e.props.bodyColumns)
    const header = (
      <Box flexDirection="row" gap={2}>
        <Box flexDirection="column">
          {LOGO.map(l => (
            <Text color="claude">{l}</Text>
          ))}
        </Box>
        <Box flexDirection="column">
          <Text bold>Agent Tazzyronth</Text>
          <Text dimColor wrap="truncate">
            {!s.online ? 'hub not running — waiting' : s.snap ? summary(s.snap, list) : 'connected'}
          </Text>
        </Box>
      </Box>
    )
    if (!s.online || !s.snap || !isActive(s)) {
      return (
        <Box flexDirection="column">
          {header}
          <Text dimColor>No swarm yet. Start one with /tazzy &lt;goal&gt;.</Text>
        </Box>
      )
    }

    const rows = agentRows(s.snap, list)
    const pick = (await read($, selected)) ?? rows.find(r => r.role !== 'lead')?.id ?? rows[0]?.id
    const nowWidth = Math.max(0, width - 34)
    const recent = pick ? agentFeed(list, pick, 10) : []
    const edit = pick ? latestDiff(list, pick) : undefined
    const diff = (edit?.detail?.diff ?? '').split('\n').filter(l => l !== '').slice(0, 12)

    return (
      <Box flexDirection="column">
        {header}
        <Text dimColor>{pad('  AGENT', 12) + pad('STATE', 10) + pad('TASK', 12) + 'NOW'}</Text>
        {rows.map(r => (
          <Button key={'agent-' + r.id} plain onPress={() => update($, selected, () => r.id)}>
            {(r.id === pick ? '▸ ' : '  ') + pad(r.id, 10) + pad(r.state, 10) + pad(r.task, 12)}
            <Text dimColor>{r.now.slice(0, nowWidth)}</Text>
          </Button>
        ))}
        {pick && <Text bold>{'\n' + pick + ' — recent activity'}</Text>}
        {recent.length === 0 && <Text dimColor>nothing yet</Text>}
        {recent.map(a => (
          <Text wrap="truncate" color={a.kind === 'alert' ? 'error' : undefined} dimColor={a.kind === 'usage'}>
            {a.kind.padEnd(10) + a.summary}
          </Text>
        ))}
        {edit && (
          <Text bold>
            {'\nlatest diff: ' + (edit.detail?.path ?? edit.files?.[0] ?? '') +
              (edit.detail ? ` (+${edit.detail.added ?? 0} −${edit.detail.removed ?? 0})` : '')}
          </Text>
        )}
        {diff.map(l => (
          <Text wrap="truncate" color={l.startsWith('+') ? 'diffAdded' : l.startsWith('-') ? 'diffRemoved' : undefined} dimColor={l.startsWith('@@')}>
            {l}
          </Text>
        ))}
      </Box>
    )
  })
}
