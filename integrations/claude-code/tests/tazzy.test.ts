import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import { addActivity, changes, leaveToast, splitLines, summary } from '../hooks/swarm'
import type { TazzySnapshot } from '../types'

const snap = (tasks: TazzySnapshot['tasks'], agents = [{ id: 'lead', role: 'lead' }, { id: 'w1', role: 'worker' }]): TazzySnapshot => ({
  agents,
  tasks,
  swarm: { max_agents: 3 },
})

test('summary, toasts and line splitting', () => {
  const s = snap([
    { id: 'a', state: 'done' },
    { id: 'b', state: 'running', owner: 'w1' },
    { id: 'c', state: 'blocked', owner: 'w2' },
  ])
  const feed = addActivity([], { id: 1, agent: 'w1', ts: '', kind: 'edit', source: 'diff', summary: 'Edit x', files: ['src/x.go'], detail: { diff: '+a' } })
  expect(summary(s, feed)).toBe('🕷 2/3 agents · 1/3 tasks · 1 blocked · ✎ w1 src/x.go')
  expect(addActivity(feed, feed[0]!)).toBe(feed)

  const next = snap([
    { id: 'a', state: 'done' },
    { id: 'b', state: 'ready', attempts: 1 },
    { id: 'c', state: 'failed', failures: [{ text: 'tests fail' }] },
  ])
  expect(changes(s, next)).toEqual(['↻ task b reassigned from w1 (back to ready)', '✖ task c failed: tests fail'])
  const done = snap([{ id: 'a', state: 'done' }, { id: 'b', state: 'done' }, { id: 'c', state: 'failed' }])
  expect(changes(next, done)).toEqual(['✔ swarm done: 2/3 tasks done, 1 failed'])
  expect(changes(done, done)).toEqual([])
  expect(changes(null, done)).toEqual([])

  expect(leaveToast({ agent: 'w2', reason: 'exited: exit status 1' })).toBe('✖ agent w2 died (exited: exit status 1)')
  expect(leaveToast({ agent: 'w2', reason: 'retired' })).toBeUndefined()
  expect(splitLines('{"a":1}\r\n{"b"')).toEqual([['{"a":1}'], '{"b"'])
})

// The engine beneath the plugin: a fake `tazzy watch --json` and records of
// what the plugin shows.
function engine(on: On, lines: string[]) {
  const clock = mock.clock(on)
  const toasts: string[] = []
  const status: (string | undefined)[] = []
  let release = () => {}
  const held = new Promise<void>(r => (release = r))
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('command.register', ($, e) => ({ value: { command: e.name } }))
  on('ui.open', () => ({ value: { isPlaced: true as const } }))
  on('ui.toast', ($, e) => (toasts.push(e.text), { value: undefined }))
  on('ui.status', ($, e) => (status.push(e.text), { value: undefined }))
  on('ui.log', () => ({ value: undefined }))
  on('process.spawn', async function* () {
    yield { stream: 'stdout' as const, text: lines.join('\n') + '\n' }
    await held
    return { code: 0, signal: null }
  })
  return { clock, toasts, status, release }
}

const band = {
  plugin: 'tazzyronth',
  component: 'AbovePrompt' as const,
  props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 100, scroll: { offset: 0, bodyRows: 10 }, view: {} },
}

test('band, pane, status line and toasts follow tazzy watch', { timeoutMs: 20000 }, async ($, on) => {
  const first = snap([{ id: 'a', state: 'running', owner: 'w1' }])
  const second = snap([{ id: 'a', state: 'done', owner: 'w1' }])
  const ev = (type: string, data: unknown) => JSON.stringify({ seq: 0, type, data })
  const w = engine(on, [ev('status', first), ev('agent.leave', { agent: 'w9', reason: 'stalled' }), ev('status', second)])

  await $.session.start({ cwd: '/proj', surface: 'terminal', isInteractive: true })
  await w.clock.settle()

  expect(w.toasts).toEqual(['✖ agent w9 died (stalled)', '✔ swarm done: 1/1 tasks done'])
  expect(w.status.at(-1)).toBe('🕷 2/3 agents · 1/1 tasks')
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...band, surface })
    expect(await ui.find({ type: 'Text', text: /1\/1 tasks/ })).toBeDefined()
  }
  const pane = await $.ui.mount({
    plugin: 'tazzyronth', surface: 'terminal', component: 'Pane', requestId: 'tazzyronth',
    props: { title: 'Tazzyronth', isFocused: false, bodyColumns: 80, placement: 'dock', scroll: { offset: 0, bodyRows: 30 }, view: {} },
  })
  expect(await pane.find({ key: 'agent-w1' })).toBeDefined()
  expect(await pane.find({ type: 'Text', text: /w1 — recent activity/ })).toBeDefined()
  w.release()
})

test('tazzy_start gets ui "host" when the pane is placed', async ($, on) => {
  const w = engine(on, [])
  let ui: unknown
  on('tool.call', ($, e) => {
    ui = (e as Record<string, unknown>).ui
    return { result: 'started' }
  })
  await $.tool.call({ tool: 'mcp__plugin_tazzyronth_tazzyronth__tazzy_start', goal: 'g' })
  expect(ui).toBe('host')
  w.release()
})
