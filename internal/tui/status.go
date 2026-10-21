package tui

import (
	"fmt"
	"strings"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
)

const statusWidth = 100

func statusModel(s hub.Snapshot, acts []activity.Activity, now time.Time) *Model {
	m := NewModel(false)
	m.Now = now
	m.SetSnapshot(s)
	m.AddActivity(acts...)
	m.noLogo = true
	return m
}

// StatusText is the plain one-shot summary of `tazzy status`.
func StatusText(s hub.Snapshot, acts []activity.Activity, now time.Time) string {
	m := statusModel(s, acts, now)
	var lines []string
	for _, l := range m.header(statusWidth)[:2] {
		lines = append(lines, strings.TrimSpace(l))
	}
	tw := tableWidths(statusWidth)
	lines = append(lines, "", cols(tw, " AGENT", "ADAPTER", "STATE", "TASK", "FILES (leased)", "♥"))
	rows := m.rows()
	for _, id := range rows {
		lines = append(lines, m.agentRow(id, tw, false))
	}
	if len(rows) == 0 {
		lines = append(lines, "  no agents")
	}
	lines = append(lines, "", " TASKS")
	tasks, _ := layered(s.Tasks)
	for _, t := range tasks {
		owner := ""
		if t.Owner != "" {
			owner = " (" + t.Owner + ")"
		}
		lines = append(lines, fmt.Sprintf("  %s %-24s %s%s", taskGlyph(false, t.State), clean(t.ID), t.State, clean(owner)))
	}
	if len(tasks) == 0 {
		lines = append(lines, "  none")
	}
	entries := feed(m.Acts, nil)
	if len(entries) > 0 {
		lines = append(lines, "", " RECENT")
		for i := max(0, len(entries)-8); i < len(entries); i++ {
			var prev *entry
			if i > 0 {
				prev = &entries[i-1]
			}
			lines = append(lines, trunc(m.feedLine(entries[i], prev, true), statusWidth))
		}
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// StatusLine is the one short line of `tazzy status --line`, for host
// status lines: "tazzy 3/5 agents · 7/12 tasks · 1 blocked · ✎ w1 src/a.go".
func StatusLine(s hub.Snapshot, acts []activity.Activity) string {
	done, blocked, failed := 0, 0, 0
	for _, t := range s.Tasks {
		switch t.State {
		case graph.Done:
			done++
		case graph.Blocked:
			blocked++
		case graph.Failed:
			failed++
		}
	}
	agents := fmt.Sprint(len(s.Agents))
	if s.Swarm.MaxAgents > 0 {
		agents += fmt.Sprint("/", s.Swarm.MaxAgents)
	}
	parts := []string{fmt.Sprintf("tazzy %s agents", agents), fmt.Sprintf("%d/%d tasks", done, len(s.Tasks))}
	switch s.State {
	case hub.RunPaused:
		parts = append(parts, "paused")
	case hub.RunStopped:
		parts = append(parts, "stopped")
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	for i := len(acts) - 1; i >= 0; i-- {
		if a := acts[i]; a.Kind == activity.Edit && len(a.Files) > 0 {
			parts = append(parts, "✎ "+clean(a.Agent)+" "+trunc(clean(a.Files[0]), 40))
			break
		}
	}
	return strings.Join(parts, " · ")
}
