// The tazzyronth mod's $.state contract: what `tazzy watch --json` streams,
// trimmed to the fields the pane, band and toasts read. Go nil slices arrive
// as null.

export type TazzyAgent = { id: string; name?: string; role: string; adapter?: string }

export type TazzyTask = {
  id: string
  title?: string
  state: string
  owner?: string
  attempts?: number
  failures?: { text: string }[] | null
}

export type TazzySnapshot = {
  agents: TazzyAgent[] | null
  tasks: TazzyTask[] | null
  swarm?: { spawn?: string; max_agents?: number }
  state?: string // run state: "paused", "stopped", "running" or absent (running)
}

export type TazzyActivity = {
  id: number
  agent: string
  task?: string
  ts: string
  kind: string
  source?: string
  summary: string
  files?: string[]
  // Kept only for diff-engine edits: { path, diff, added, removed }.
  detail?: { path?: string; diff?: string; added?: number; removed?: number }
}

// online: watch is connected to a hub; snap: its last status.
export type TazzySwarm = { online: boolean; snap: TazzySnapshot | null }

declare module 'claude-code' {
  interface PluginState {
    tazzyronth: {
      swarm: TazzySwarm
      feed: TazzyActivity[]
      selected: string | null
    }
  }
}
