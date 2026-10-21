// Pure helpers over `tazzy watch --json` data: no `$`, so tests call them
// directly. Mirrors internal/tui's StatusLine and agent table.

import type { TazzyActivity, TazzySnapshot, TazzySwarm, TazzyTask } from '../types'

const TERMINAL = new Set(['done', 'failed', 'cancelled'])
const BUSY = new Set(['claimed', 'running', 'blocked', 'review'])

export const FEED_MAX = 300

export function isActive(s: TazzySwarm | null): boolean {
  const snap = s?.snap
  return !!s?.online && !!snap && ((snap.agents ?? []).length > 0 || (snap.tasks ?? []).length > 0)
}

function count(tasks: TazzyTask[], state: string): number {
  return tasks.filter(t => t.state === state).length
}

function lastEdit(feed: readonly TazzyActivity[], agent?: string): TazzyActivity | undefined {
  for (let i = feed.length - 1; i >= 0; i--) {
    const a = feed[i]!
    if (a.kind === 'edit' && (a.files?.length ?? 0) > 0 && (agent === undefined || a.agent === agent)) return a
  }
  return undefined
}

function trunc(s: string, n: number): string {
  return s.length <= n ? s : '…' + s.slice(s.length - n + 1)
}

// One line: "🕷 3/5 agents · 7/12 tasks · 1 blocked · ✎ w1 src/auth.go".
export function summary(snap: TazzySnapshot, feed: readonly TazzyActivity[]): string {
  const tasks = snap.tasks ?? []
  const max = snap.swarm?.max_agents
  const parts = [
    `🕷 ${(snap.agents ?? []).length}${max ? '/' + max : ''} agents`,
    `${count(tasks, 'done')}/${tasks.length} tasks`,
  ]
  const blocked = count(tasks, 'blocked')
  const failed = count(tasks, 'failed')
  if (snap.state === 'paused') parts.push('⏸ paused')
  if (snap.state === 'stopped') parts.push('■ stopped')
  if (blocked) parts.push(`${blocked} blocked`)
  if (failed) parts.push(`${failed} failed`)
  const edit = lastEdit(feed)
  if (edit) parts.push(`✎ ${edit.agent} ${trunc(edit.files![0]!, 40)}`)
  return parts.join(' · ')
}

export type AgentRow = { id: string; role: string; state: string; task: string; now: string }

export function agentRows(snap: TazzySnapshot, feed: readonly TazzyActivity[]): AgentRow[] {
  const tasks = snap.tasks ?? []
  return (snap.agents ?? []).map(a => {
    const t = tasks.find(x => x.owner === a.id && BUSY.has(x.state))
    let now = ''
    for (let i = feed.length - 1; i >= 0; i--) {
      const x = feed[i]!
      if (x.agent === a.id && x.kind !== 'usage') {
        now = x.summary
        break
      }
    }
    return { id: a.id, role: a.role, state: t ? t.state : 'idle', task: t?.id ?? '', now }
  })
}

export function agentFeed(feed: readonly TazzyActivity[], agent: string, n: number): TazzyActivity[] {
  return feed.filter(a => a.agent === agent && a.kind !== 'usage').slice(-n)
}

export function latestDiff(feed: readonly TazzyActivity[], agent: string): TazzyActivity | undefined {
  return lastEdit(feed, agent)
}

// Appends an activity event: dedup by id, drop bulky details except the
// diff engine's, keep the newest FEED_MAX.
export function addActivity(feed: readonly TazzyActivity[], a: TazzyActivity): TazzyActivity[] {
  if (feed.some(x => x.id === a.id)) return feed as TazzyActivity[]
  const keep: TazzyActivity = { ...a }
  if (!(a.kind === 'edit' && a.source === 'diff')) delete keep.detail
  return [...feed, keep].slice(-FEED_MAX)
}

// Toasts for what changed between two status snapshots: a task failed for
// good, a task changed hands, the swarm finished.
export function changes(prev: TazzySnapshot | null, cur: TazzySnapshot): string[] {
  if (!prev) return []
  const out: string[] = []
  const before = new Map((prev.tasks ?? []).map(t => [t.id, t]))
  for (const t of cur.tasks ?? []) {
    const p = before.get(t.id)
    if (!p) continue
    if (t.state === 'failed' && p.state !== 'failed') {
      const why = t.failures?.at(-1)?.text
      out.push(`✖ task ${t.id} failed${why ? ': ' + why : ''}`)
    } else if (p.owner && p.owner !== t.owner && !TERMINAL.has(t.state)) {
      out.push(`↻ task ${t.id} reassigned from ${p.owner}${t.owner ? ' to ' + t.owner : ' (back to ready)'}`)
    }
  }
  const run = (s: TazzySnapshot): 'paused' | 'stopped' | 'running' => (s.state === 'paused' || s.state === 'stopped' ? s.state : 'running')
  if (run(cur) !== run(prev)) {
    out.push({ paused: '⏸ swarm paused', stopped: '■ swarm stopped', running: '▶ swarm resumed' }[run(cur)])
  }
  const tasks = cur.tasks ?? []
  const finished = (ts: TazzyTask[]) => ts.length > 0 && ts.every(t => TERMINAL.has(t.state))
  if (finished(tasks) && !finished(prev.tasks ?? [])) {
    const failed = count(tasks, 'failed')
    out.push(`✔ swarm done: ${count(tasks, 'done')}/${tasks.length} tasks done${failed ? `, ${failed} failed` : ''}`)
  }
  return out
}

// The toast for an agent.leave event, if the agent died rather than left.
export function leaveToast(data: { agent?: string; reason?: string }): string | undefined {
  const reason = data.reason ?? ''
  if (!/exit|stall|crash|kill|died|lost/i.test(reason)) return undefined
  return `✖ agent ${data.agent ?? '?'} died (${reason})`
}

// Splits streamed text into whole lines; returns them and the unfinished rest.
export function splitLines(buffer: string): [string[], string] {
  const parts = buffer.split('\n')
  const rest = parts.pop() ?? ''
  return [parts.map(l => l.replace(/\r$/, '')).filter(l => l.trim() !== ''), rest]
}
