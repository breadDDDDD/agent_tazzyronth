package tui

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"tazzyronth/assets"
	"tazzyronth/internal/activity"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
)

// Render draws the model as exactly h lines, none wider than w cells (the
// last one is kept a cell short so writing it can't scroll the screen).
func Render(m *Model, w, h int) []string {
	var lines []string
	switch {
	case w < 40 || h < 10:
		lines = []string{fmt.Sprintf("terminal too small (%dx%d)", w, h)}
	case m.isOffline():
		lines = m.offline(w, h)
	default:
		var body []string
		switch m.view {
		case vAgent:
			body = m.agentView(w, h-1)
		case vActivity:
			body = m.activityView(w, h-1)
		case vFiles:
			body = m.filesView(w, h-1)
		case vGraph:
			body = m.graphView(w, h-1)
		case vMessages:
			body = m.messagesView(w, h-1)
		case vBoard:
			body = m.boardView(w, h-1)
		case vUsage:
			body = m.usageView(w, h-1)
		default:
			body = m.overview(w, h-1)
		}
		body = body[:min(len(body), h-1)]
		for len(body) < h-1 {
			body = append(body, "")
		}
		lines = append(body, m.footer(w))
	}
	frame := make([]string, h)
	for i := range frame {
		lw := w
		if i == h-1 {
			lw = w - 1
		}
		if i < len(lines) {
			frame[i] = fit(lines[i], lw)
		} else {
			frame[i] = fit("", lw)
		}
	}
	return frame
}

func (m *Model) p() painter { return painter(m.Color) }

func (m *Model) rule(w int) string { return m.p().paint(stDim, strings.Repeat("─", w)) }

// --- overview (plan §6) ---

func (m *Model) logo() []string {
	if m.noLogo {
		return []string{"", "", ""}
	}
	src := assets.LogoMiniPlain
	if m.Color {
		src = assets.LogoMini
	}
	lines := strings.Split(strings.TrimRight(src, "\n"), "\n")
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = " " + fit(strings.TrimRight(lines[i], "\r"), 7)
	}
	return lines[:3]
}

func (m *Model) header(w int) []string {
	p := m.p()
	logo := m.logo()
	right := ""
	if g := m.goal(); g != "" {
		right = "goal: " + trunc(clean(g), max(10, w/3)) + "   "
	}
	if start := m.started(); !start.IsZero() {
		right += "elapsed " + hms(m.Now.Sub(start))
	}
	l0 := spread(logo[0]+p.paint(stTitle, "AGENT TAZZYRONTH"), p.paint(stDim, right), w)

	s := m.Snap
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
	sum := fmt.Sprintf("swarm %s agents · %d/%d tasks · %d blocked · ", agents, done, len(s.Tasks), blocked)
	fail := fmt.Sprintf("%d failed", failed)
	if failed > 0 {
		fail = p.paint(stErr, fail)
	}
	sum += fail
	if u := m.usage(""); u.Total() > 0 {
		sum += " · " + usageText(u)
	}
	l1 := logo[1] + m.badge() + p.paint(stText, sum)

	l2 := logo[2]
	switch {
	case m.Conn != "":
		l2 += p.paint(stErr, "✖ "+m.Conn)
	default:
		l2 += p.paint(stDim, clean(m.Dir))
	}
	return []string{l0, l1, l2}
}

// badge marks a paused or stopped swarm at the start of the summary line.
func (m *Model) badge() string {
	p := m.p()
	switch m.runState() {
	case hub.RunPaused:
		return p.paint(stAccent, "⏸ PAUSED") + "  "
	case hub.RunStopped:
		return p.paint(stErr, "■ STOPPED") + "  "
	}
	return ""
}

// started is when the swarm began: the earliest join or activity.
func (m *Model) started() time.Time {
	var t time.Time
	for _, a := range m.Snap.Agents {
		if t.IsZero() || a.Joined.Before(t) {
			t = a.Joined
		}
	}
	if len(m.Acts) > 0 && (t.IsZero() || m.Acts[0].TS.Before(t)) {
		t = m.Acts[0].TS
	}
	return t
}

func tableWidths(w int) []int {
	rest := max(20, w-9-9-10-5)
	task := rest * 55 / 100
	return []int{9, 9, 10, task, rest - task}
}

func (m *Model) overview(w, h int) []string {
	p := m.p()
	out := m.header(w)
	out = append(out, m.rule(w))
	tw := tableWidths(w)
	out = append(out, p.paint(stDim, cols(tw, " AGENT", "ADAPTER", "STATE", "TASK", "FILES (leased)", "♥")))

	graphN := 2
	if h >= 34 {
		graphN = 3
	}
	rows := m.rows()
	maxRows := max(1, h-len(out)-1-graphN-1-2)
	m.sel = min(m.sel, max(0, len(rows)-1))
	start := max(0, m.sel-maxRows+1)
	if len(rows) == 0 {
		out = append(out, p.paint(stDim, "  no agents yet"))
	}
	for i := start; i < len(rows) && i < start+maxRows; i++ {
		out = append(out, m.agentRow(rows[i], tw, i == m.sel))
	}
	out = append(out, m.rule(w))

	label := p.paint(stBold, " TASK GRAPH  ")
	for i, l := range m.graphLines(w-13, graphN) {
		if i > 0 {
			label = strings.Repeat(" ", 13)
		}
		out = append(out, label+l)
	}
	out = append(out, m.rule(w))

	label = p.paint(stBold, " MESSAGES  ")
	n := h - len(out)
	if len(m.Msgs) == 0 {
		out = append(out, label+p.paint(stDim, "none yet"))
	}
	for i := len(m.Msgs) - 1; i >= 0 && n > 0; i-- {
		out = append(out, label+m.msgLine(m.Msgs[i], false))
		label = strings.Repeat(" ", 11)
		n--
	}
	return out
}

func (m *Model) agentRow(id string, tw []int, selected bool) string {
	p := m.p()
	if selected && m.Color {
		p = false // the whole row is painted reversed below
	}
	adapter, state, task, files, beat := "—", p.paint(stDim, "○ idle"), "—", "—", "—"
	if a := m.agentByID(id); a != nil {
		adapter = cmp.Or(a.Adapter, "host")
		if a.Role == "lead" {
			adapter = cmp.Or(a.Adapter, "lead")
		}
		if t := m.taskOf(id); t != nil {
			task = t.ID
			switch t.State {
			case graph.Blocked:
				state = p.paint(stAccent, "◐ wait")
			case graph.Review:
				state = p.paint(stAccent, "◐ review")
			default:
				state = p.paint(stAccent, "● run")
			}
		} else if a.Role == "lead" {
			state = p.paint(stAccent, "● lead")
		}
		if l := m.leasesOf(id); len(l) > 0 {
			files = strings.Join(l, ", ")
		}
		if t := m.lastSeen(id); !t.IsZero() {
			beat = ago(m.Now.Sub(t))
		}
	} else if why, ok := m.goneReason(id); ok {
		state, task = p.paint(stErr, "✖ gone"), "("+clean(why)+")"
	}
	mark := " "
	if selected {
		mark = "▸"
	}
	row := cols(tw, mark+clean(id), adapter, state, clean(task), clean(files), beat)
	if selected {
		return painter(m.Color).paint(stSel, row)
	}
	return row
}

func taskGlyph(p painter, s graph.State) string {
	switch s {
	case graph.Done:
		return p.paint(stText, "✔")
	case graph.Claimed, graph.Running:
		return p.paint(stAccent, "●")
	case graph.Blocked, graph.Review:
		return p.paint(stAccent, "◐")
	case graph.Failed:
		return p.paint(stErr, "✖")
	case graph.Cancelled:
		return p.paint(stDim, "⊘")
	case graph.Ready:
		return p.paint(stText, "○")
	}
	return p.paint(stDim, "○")
}

// depths gives each task its layer: the longest dependency chain above it.
func depths(tasks []graph.Task) map[string]int {
	byID := map[string]graph.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	d := map[string]int{}
	var depth func(id string, guard int) int
	depth = func(id string, guard int) int {
		if v, ok := d[id]; ok || guard > len(tasks) {
			return v
		}
		v := 0
		for _, dep := range byID[id].Deps {
			v = max(v, depth(dep, guard+1)+1)
		}
		d[id] = v
		return v
	}
	for _, t := range tasks {
		depth(t.ID, 0)
	}
	return d
}

// layered sorts tasks by layer, then id.
func layered(tasks []graph.Task) ([]graph.Task, map[string]int) {
	d := depths(tasks)
	out := slices.Clone(tasks)
	slices.SortStableFunc(out, func(a, b graph.Task) int {
		return cmp.Or(cmp.Compare(d[a.ID], d[b.ID]), strings.Compare(a.ID, b.ID))
	})
	return out, d
}

// graphLines is the compact DAG: layers joined by arrows, wrapped to n lines.
func (m *Model) graphLines(w, n int) []string {
	p := m.p()
	tasks, d := layered(m.Snap.Tasks)
	if len(tasks) == 0 {
		return []string{p.paint(stDim, "no tasks yet")}
	}
	var lines []string
	cur, layer := "", 0
	for i, t := range tasks {
		tok := clean(t.ID) + " " + taskGlyph(p, t.State)
		sep := "  "
		if i == 0 {
			sep = ""
		} else if d[t.ID] != layer {
			sep = p.paint(stDim, " → ")
		}
		layer = d[t.ID]
		if cur != "" && width(cur)+width(sep)+width(tok) > w {
			lines = append(lines, cur)
			cur, sep = "", strings.TrimLeft(sep, " ")
		}
		cur += sep + tok
	}
	lines = append(lines, cur)
	if len(lines) > n {
		lines = lines[:n]
		lines[n-1] = trunc(lines[n-1], w-2) + " …"
	}
	return lines
}

func (m *Model) msgLine(msg message, long bool) string {
	p := m.p()
	ts := msg.TS.Local().Format("15:04")
	if long {
		ts = msg.TS.Local().Format("15:04:05")
	}
	who := clean(msg.From) + "→" + clean(msg.To)
	kind := ""
	if msg.Kind != "info" {
		kind = p.paint(stAccent, "["+msg.Kind+"] ")
	}
	body := fmt.Sprintf("%q", clean(msg.Body))
	style := stText
	if msg.From == "hub" {
		style = stDim
	}
	return p.paint(stDim, ts) + " " + p.paint(style, who) + "  " + kind + p.paint(style, body)
}

type key struct{ k, long, short string }

// pauseKey is the footer entry of p: pause, or resume while paused.
func (m *Model) pauseKey() key {
	if m.runState() == hub.RunPaused {
		return key{"p", "resume", "resume"}
	}
	return key{"p", "pause", "pause"}
}

func (m *Model) keys() []key {
	switch m.view {
	case vAgent:
		return []key{{"↑↓", "scroll", "scroll"}, {"enter", "expand event", "expand"}, {"f", "follow", "follow"},
			{"d", "diff file", "diff"}, {"o", "output", "out"}, {"←→", "agent", "agent"},
			{"m", "message agent", "msg"}, {"k", "kill", "kill"}, {"esc", "back", "back"}}
	case vOverview:
		return []key{{"↑↓", "select", "sel"}, {"enter", "agent", "open"}, {"a", "activity", "act"}, {"F", "files", "files"},
			{"g", "graph", "graph"}, {"m", "messages", "msgs"}, {"b", "board", "board"}, {"u", "usage", "usage"},
			{"k", "kill", "kill"}, m.pauseKey(), {"x", "stop", "stop"}, {"q", "quit", "quit"}}
	case vMessages:
		return []key{{"↑↓", "scroll", "scroll"}, {"/", "filter", "filter"}, {"esc", "back", "back"}, {"q", "quit", "quit"}}
	}
	return []key{{"↑↓", "scroll", "scroll"}, {"a", "activity", "act"}, {"F", "files", "files"}, {"g", "graph", "graph"},
		{"m", "messages", "msgs"}, {"b", "board", "board"}, {"u", "usage", "usage"}, {"esc", "back", "back"}, {"q", "quit", "quit"}}
}

func (m *Model) footer(w int) string {
	p := m.p()
	if in := m.input; in != nil {
		label := in.label
		if in.kind != "" {
			label += " [" + in.kind + ", tab toggles]"
		}
		if in.confirm {
			return p.paint(stAccent, " "+label+" ")
		}
		return p.paint(stAccent, " "+label+": ") + clean(in.text) + "█"
	}
	prefix := ""
	if m.Conn != "" {
		prefix = p.paint(stErr, " ✖ "+m.Conn) + " "
	} else if m.Flash != "" {
		prefix = p.paint(stAccent, " "+clean(m.Flash)) + " "
	}
	ks := m.keys()
	long, short := prefix, prefix
	for _, k := range ks {
		long += " " + p.paint(stAccent, "["+k.k+"]") + " " + k.long + " "
		short += " " + p.paint(stAccent, k.k) + " " + k.short
	}
	if width(long) < w {
		return long
	}
	// Still too wide: drop view shortcuts (least needed first) until it fits.
	for _, drop := range []string{"u", "b", "F", "g", "a"} {
		if width(short) < w {
			break
		}
		ks = slices.DeleteFunc(ks, func(k key) bool { return k.k == drop })
		short = prefix
		for _, k := range ks {
			short += " " + p.paint(stAccent, k.k) + " " + k.short
		}
	}
	return short
}

// offline is the whole screen while no hub has been reached yet.
func (m *Model) offline(w, h int) []string {
	p := m.p()
	out := make([]string, max(0, h/2-3-len(m.Others)))
	for _, l := range m.logo() {
		out = append(out, center(l, w))
	}
	out = append(out, "", center(p.paint(stTitle, "AGENT TAZZYRONTH"), w), "",
		center(p.paint(stErr, m.Conn), w),
		center(p.paint(stDim, clean(m.Dir)), w), "")
	if len(m.Others) == 0 {
		return append(out, center(p.paint(stDim, "start one with `tazzy hub` (or /tazzy in your agent CLI) · q quits"), w))
	}
	out = append(out, center(p.paint(stTitle, "hubs running elsewhere"), w))
	rows := make([]string, len(m.Others))
	wide := 0
	for i, r := range m.Others {
		rows[i] = fmt.Sprintf("%s  (pid %d)", clean(r.Dir), r.PID)
		wide = max(wide, width(rows[i])+2)
	}
	for i, r := range rows {
		mark, st := "  ", stDim
		if i == m.osel {
			mark, st = "▸ ", stAccent
		}
		out = append(out, center(p.paint(st, fit(mark+r, min(wide, w-2))), w))
	}
	return append(out, "", center(p.paint(stDim, "↑↓ select · enter attach · q quits"), w))
}

// --- agent detail (plan §6.1) ---

func (m *Model) agentView(w, h int) []string {
	p := m.p()
	id := m.agent
	parts := []string{p.paint(stTitle, clean(id))}
	if a := m.agentByID(id); a != nil {
		parts = append(parts, cmp.Or(a.Adapter, a.Role))
		if t := m.taskOf(id); t != nil {
			parts = append(parts, p.paint(stAccent, "● "+string(t.State)), clean(t.ID))
		} else {
			parts = append(parts, p.paint(stDim, "○ idle"))
		}
		parts = append(parts, dur(m.Now.Sub(a.Joined)))
	} else if why, ok := m.goneReason(id); ok {
		parts = append(parts, p.paint(stErr, "✖ gone ("+clean(why)+")"))
	}
	if u := m.usage(id); u.Total() > 0 {
		parts = append(parts, usageText(u))
	}
	fol := "[f]ollow OFF"
	if m.follow {
		fol = p.paint(stAccent, "[f]ollow ON")
	}
	out := []string{spread(" "+strings.Join(parts, " · "), fol+" ", w)}

	now := "—"
	for i := len(m.Acts) - 1; i >= 0; i-- {
		if a := m.Acts[i]; a.Agent == id && a.Kind == activity.Intent {
			now = fmt.Sprintf("%q", clean(a.Summary))
			break
		}
	}
	out = append(out, " "+p.paint(stAccent, "NOW ▸ ")+p.paint(stBold, now))

	entries := m.agentFeed()
	cur := m.cursor(len(entries))
	ph := max(4, h-len(out)-2)
	var right []string
	var rtitle string
	if m.expand && cur >= 0 && cur < len(entries) {
		rtitle, right = m.eventPane(entries[cur].Activity)
	} else {
		rtitle, right = m.diffPane(id, m.diffFile)
	}
	if w >= 100 {
		lw := w * 45 / 100
		left := m.box("ACTIVITY", m.feedLines(entries, cur, lw-3, ph-2, false), lw, ph)
		rbox := m.box(rtitle, right, w-lw, ph)
		for i := range left {
			out = append(out, left[i]+rbox[i])
		}
	} else {
		fh := max(3, ph*55/100)
		out = append(out, m.box("ACTIVITY", m.feedLines(entries, cur, w-3, fh-2, false), w, fh)...)
		out = append(out, m.box(rtitle, right, w, ph-fh)...)
	}

	files := " " + p.paint(stBold, "FILES   ")
	for _, e := range m.touched(id) {
		files += "✎ " + clean(e.Files[0]) + " " + counts(p, e.plus, e.minus) + "   "
	}
	for _, l := range m.Snap.Leases {
		if l.Holder == id {
			files += p.paint(stAccent, "◇ ") + clean(l.Path) + p.paint(stDim, " ("+string(l.Mode)+")") + "   "
		}
	}
	out = append(out, files)
	output := p.paint(stDim, "—")
	for i := len(m.Acts) - 1; i >= 0; i-- {
		if a := m.Acts[i]; a.Agent == id && a.Kind == activity.CmdResult {
			output = m.outputTail(a)
			break
		}
	}
	out = append(out, " "+p.paint(stBold, "OUTPUT  ")+output)
	return out
}

// box frames lines (fitted to the inside) in a w×h box titled title.
func (m *Model) box(title string, lines []string, w, h int) []string {
	p := m.p()
	inner := max(1, w-3)
	top := "┌ " + title + " "
	top = trunc(top, w-1)
	top += strings.Repeat("─", max(0, w-1-width(top))) + "┐"
	out := []string{p.paint(stDim, top)}
	for i := 0; i < h-2; i++ {
		s := ""
		if i < len(lines) {
			s = lines[i]
		}
		out = append(out, p.paint(stDim, "│")+" "+fit(s, inner)+p.paint(stDim, "│"))
	}
	return append(out, p.paint(stDim, "└"+strings.Repeat("─", max(0, w-2))+"┘"))
}

// feedLines renders the window of entries (n rows, w cells) that keeps the
// cursor visible; cur < 0 means no cursor.
func (m *Model) feedLines(entries []entry, cur, w, n int, withAgent bool) []string {
	if len(entries) == 0 {
		return []string{m.p().paint(stDim, "no activity yet")}
	}
	end := len(entries)
	if cur >= 0 {
		end = min(len(entries), max(cur+1, n))
	}
	start := max(0, end-n)
	if m.follow && !m.expand {
		cur = -1 // following: no cursor, just the newest at the bottom
	}
	var out []string
	for i := start; i < end; i++ {
		var prev *entry
		if i > 0 {
			prev = &entries[i-1]
		}
		l := m.feedLine(entries[i], prev, withAgent)
		if i == cur {
			l = painter(m.Color).paint(stSel, "▸"+stripStyle(l)[1:])
		}
		out = append(out, trunc(l, w))
	}
	return out
}

var glyphs = map[string]string{
	activity.Edit: "✎", activity.Read: "◉", activity.Cmd: "▶", activity.Search: "⌕", activity.Tool: "·",
	activity.Msg: "✉", activity.Lease: "◇", activity.State: "●", activity.Intent: "✦", activity.Say: "»",
	activity.Alert: "!",
}

// feedLine is one feed line: "HH:MM:SS ✎ Edit   src/a.go +3 −1".
func (m *Model) feedLine(e entry, prev *entry, withAgent bool) string {
	p := m.p()
	ts := " " + p.paint(stDim, e.TS.Local().Format("15:04:05")) + " "
	if withAgent {
		ts += p.paint(agentColor(e.Agent), fit(clean(e.Agent), 6)) + " "
	}
	sum := clean(e.Summary)
	switch e.Kind {
	case activity.CmdResult:
		status, rest, _ := strings.Cut(sum, " ")
		if prev != nil && prev.Kind == activity.Cmd && prev.Agent == e.Agent {
			if after, ok := strings.CutPrefix(rest, clean(prev.Summary)); ok {
				rest = strings.TrimPrefix(after, ": ")
				if !withAgent {
					ts = strings.Repeat(" ", 10)
				}
			}
		}
		style := stAccent
		if status == "✖" {
			style = stErr
		}
		return ts + p.paint(style, status) + " " + cmp.Or(rest, "ok")
	case activity.Alert:
		return ts + p.paint(stErr, "! "+strings.TrimPrefix(sum, "⚠ "))
	case activity.Edit, activity.Read, activity.Cmd, activity.Search, activity.Tool:
		tool, arg, _ := strings.Cut(sum, " ")
		if e.Source == activity.SrcDiff {
			tool = "diff"
		}
		if len(e.Files) > 0 && (e.Kind == activity.Edit || e.Kind == activity.Read) {
			arg = clean(e.Files[0])
		}
		s := ts + p.paint(stAccent, glyphs[e.Kind]) + " " + fit(tool, 6) + " " + arg
		if e.counted {
			s += "  " + counts(p, e.plus, e.minus)
		}
		return s
	case activity.Say:
		return ts + p.paint(stDim, "» ") + p.paint(stText, sum)
	}
	return ts + p.paint(stAccent, cmp.Or(glyphs[e.Kind], "·")) + " " + sum
}

func counts(p painter, plus, minus int) string {
	return p.paint(stAccent, fmt.Sprintf("+%d", plus)) + " " + p.paint(stErr, fmt.Sprintf("−%d", minus))
}

func agentColor(id string) string {
	if id == "hub" {
		return stDim
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	return agentColors[h.Sum32()%uint32(len(agentColors))]
}

// diffPane is the live diff of file (or of agent's latest edit).
func (m *Model) diffPane(agent, file string) (string, []string) {
	p := m.p()
	a, ok := m.latestEdit(agent, file)
	if !ok {
		return "LIVE DIFF", []string{p.paint(stDim, "no edits yet")}
	}
	plus, minus := editCounts(a)
	title := "LIVE DIFF  " + clean(a.Files[0]) + "  " + counts(p, plus, minus)
	d := m.detail(a)
	if d == nil {
		return title, []string{p.paint(stDim, "loading…")}
	}
	var de diffEdit
	json.Unmarshal(d, &de)
	return title, m.diffLines(de.Diff)
}

// diffLines renders a unified diff with new-file line numbers.
func (m *Model) diffLines(diff string) []string {
	p := m.p()
	var out []string
	n := 0
	for _, l := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++") || l == "":
		case strings.HasPrefix(l, "@@"):
			var a, b, c, d int
			fmt.Sscanf(l, "@@ -%d,%d +%d,%d", &a, &b, &c, &d)
			if c == 0 {
				fmt.Sscanf(l, "@@ -%d +%d", &a, &c)
			}
			n = c
			if len(out) > 0 {
				out = append(out, p.paint(stDim, "  ⋯"))
			}
		default:
			text := clean(strings.ReplaceAll(l[1:], "\t", "    "))
			switch l[0] {
			case '+':
				out = append(out, p.paint(stDim, fmt.Sprintf("%4d ", n))+p.paint(stAccent, "+ "+text))
				n++
			case '-':
				out = append(out, "     "+p.paint(stErr, "- "+text))
			default:
				out = append(out, p.paint(stDim, fmt.Sprintf("%4d ", n))+"  "+p.paint(stText, text))
				n++
			}
		}
	}
	if len(out) == 0 {
		out = []string{p.paint(stDim, "(no textual change)")}
	}
	return out
}

// eventPane is one activity in full (enter in the agent detail).
func (m *Model) eventPane(a activity.Activity) (string, []string) {
	p := m.p()
	title := fmt.Sprintf("EVENT #%d  %s · %s", a.ID, a.Kind, cmp.Or(a.Source, "?"))
	out := wrap(clean(a.Summary), 200)
	if a.Task != "" {
		out = append(out, p.paint(stDim, "task "+clean(a.Task)))
	}
	out = append(out, "")
	d := m.detail(a)
	if d == nil && a.DetailSize > 0 {
		return title, append(out, p.paint(stDim, "loading…"))
	}
	var probe struct {
		Diff   *string `json:"diff"`
		Output *string `json:"output"`
	}
	json.Unmarshal(d, &probe)
	switch {
	case probe.Diff != nil:
		out = append(out, m.diffLines(*probe.Diff)...)
	case probe.Output != nil:
		for _, l := range strings.Split(*probe.Output, "\n") {
			out = append(out, clean(strings.ReplaceAll(l, "\t", "    ")))
		}
	case len(d) > 0:
		var buf bytes.Buffer
		if json.Indent(&buf, d, "", "  ") != nil {
			buf.Reset()
			buf.Write(d)
		}
		for _, l := range strings.Split(buf.String(), "\n") {
			out = append(out, clean(l))
		}
	}
	return title, out
}

// outputTail is the last non-empty output line of a command result.
func (m *Model) outputTail(a activity.Activity) string {
	var d struct{ Output string }
	if json.Unmarshal(m.detail(a), &d) == nil && strings.TrimSpace(d.Output) != "" {
		lines := strings.Split(strings.TrimSpace(d.Output), "\n")
		lines = lines[max(0, len(lines)-2):]
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		return clean(strings.Join(lines, "  "))
	}
	return clean(a.Summary)
}

// --- other views (plan §6.2) ---

func (m *Model) title(text, right string, w int) []string {
	p := m.p()
	return []string{spread(" "+p.paint(stTitle, "AGENT TAZZYRONTH")+p.paint(stDim, " · ")+p.paint(stBold, text), p.paint(stDim, right)+" ", w), m.rule(w)}
}

// tail shows the last n lines, scrolled up by m.scroll (clamped).
func (m *Model) tail(lines []string, n int) []string {
	m.scroll = min(m.scroll, max(0, len(lines)-n))
	end := len(lines) - m.scroll
	return lines[max(0, end-n):end]
}

// head shows n lines from offset m.scroll (clamped).
func (m *Model) head(lines []string, n int) []string {
	m.scroll = min(m.scroll, max(0, len(lines)-n))
	return lines[m.scroll:min(len(lines), m.scroll+n)]
}

func (m *Model) activityView(w, h int) []string {
	entries := feed(m.Acts, nil)
	state := "following"
	if m.scroll > 0 {
		state = fmt.Sprintf("scrolled %d up", m.scroll)
	}
	out := m.title("SWARM ACTIVITY", fmt.Sprintf("%d events · %s", len(entries), state), w)
	var lines []string
	for i := range entries {
		var prev *entry
		if i > 0 {
			prev = &entries[i-1]
		}
		lines = append(lines, m.feedLine(entries[i], prev, true))
	}
	if len(lines) == 0 {
		lines = []string{m.p().paint(stDim, " no activity yet")}
	}
	return append(out, m.tail(lines, h-len(out))...)
}

func (m *Model) filesView(w, h int) []string {
	p := m.p()
	files := m.touched("")
	alerts := map[string]bool{}
	for _, a := range m.Acts {
		if a.Kind == activity.Alert && len(a.Files) > 0 {
			k := strings.ToLower(a.Files[0])
			alerts[k] = true
			if !slices.ContainsFunc(files, func(e entry) bool { return strings.EqualFold(e.Files[0], k) }) {
				files = append(files, entry{Activity: a})
			}
		}
	}
	out := m.title("FILES", fmt.Sprintf("%d changed", len(files)), w)
	fw := max(20, w-8-12-24)
	widths := []int{fw, 8, 12}
	out = append(out, p.paint(stDim, cols(widths, " FILE", "AGENT", "+/−", "LEASE")))
	listH := max(3, (h-len(out))/2)
	m.sel = min(m.sel, max(0, len(files)-1))
	start := max(0, m.sel-listH+1)
	for i := start; i < len(files) && i < start+listH; i++ {
		e := files[i]
		path := e.Files[0]
		holder := ""
		for _, l := range m.Snap.Leases {
			if lease.Overlap(l.Path, strings.ToLower(path)) {
				holder = l.Holder + " (" + string(l.Mode) + ")"
				break
			}
		}
		pp := p
		if i == m.sel && m.Color {
			pp = false
		}
		cnt, who := counts(pp, e.plus, e.minus), clean(e.Agent)
		bad := ""
		switch {
		case alerts[strings.ToLower(path)] && !e.counted:
			cnt, who, bad = "—", "?", "unattributed"
		case alerts[strings.ToLower(path)]:
			bad = "unattributed edit seen"
		case holder != "" && !strings.HasPrefix(holder, e.Agent+" "):
			bad = "conflict"
		}
		mark := " "
		if i == m.sel {
			mark = "▸"
		}
		row := cols(widths, mark+clean(path), who, cnt, clean(cmp.Or(holder, "—")))
		if bad != "" {
			row += "  " + pp.paint(stErr, "✖ "+bad)
		}
		if i == m.sel {
			row = painter(m.Color).paint(stSel, row)
		}
		out = append(out, row)
	}
	if len(files) == 0 {
		out = append(out, p.paint(stDim, " no changed files yet"))
	}
	title, diff := "DIFF", []string{p.paint(stDim, "select a file")}
	if m.sel < len(files) {
		title, diff = m.diffPane("", files[m.sel].Files[0])
	}
	return append(out, m.box(title, diff, w, max(3, h-len(out)))...)
}

func (m *Model) graphView(w, h int) []string {
	p := m.p()
	tasks, _ := layered(m.Snap.Tasks)
	done := 0
	for _, t := range tasks {
		if t.State == graph.Done {
			done++
		}
	}
	out := m.title("TASK GRAPH", fmt.Sprintf("%d/%d done · * critical path", done, len(tasks)), w)
	widths := []int{max(14, w/4), 13, 8, 5, max(10, w/5)}
	out = append(out, p.paint(stDim, cols(widths, " TASK", "STATE", "OWNER", "ATT", "DEPS", "NOTE")))
	var lines []string
	for _, t := range tasks {
		crit := " "
		if slices.Contains(m.Snap.CriticalPath, t.ID) {
			crit = p.paint(stAccent, "*")
		}
		att := "—"
		if t.Attempts > 0 {
			att = fmt.Sprint(t.Attempts)
			if t.MaxAttempts > 0 {
				att += fmt.Sprint("/", t.MaxAttempts)
			}
		}
		note := ""
		if n := len(t.Failures); n > 0 && (t.State == graph.Failed || t.State == graph.Ready) {
			note = p.paint(stErr, clean(t.Failures[n-1].Text))
		} else if n := len(t.Checkpoints); n > 0 {
			note = p.paint(stDim, clean(t.Checkpoints[n-1].Text))
		} else if t.Result != nil {
			note = p.paint(stDim, clean(t.Result.Summary))
		}
		lines = append(lines, cols(widths, crit+clean(t.ID), taskGlyph(p, t.State)+" "+string(t.State),
			clean(cmp.Or(t.Owner, "—")), att, clean(cmp.Or(strings.Join(t.Deps, ","), "—")), note))
	}
	if len(lines) == 0 {
		lines = []string{p.paint(stDim, " no tasks yet")}
	}
	return append(out, m.head(lines, h-len(out))...)
}

func (m *Model) messagesView(w, h int) []string {
	right := fmt.Sprintf("%d messages", len(m.Msgs))
	if m.filter != "" {
		right = "filter: " + clean(m.filter) + " · " + right
	}
	out := m.title("MESSAGES", right, w)
	var lines []string
	for _, msg := range m.Msgs {
		if m.filter == "" || strings.Contains(strings.ToLower(msg.From+" "+msg.To), strings.ToLower(m.filter)) {
			lines = append(lines, " "+m.msgLine(msg, true))
		}
	}
	if len(lines) == 0 {
		lines = []string{m.p().paint(stDim, " no messages")}
	}
	return append(out, m.tail(lines, h-len(out))...)
}

func (m *Model) boardView(w, h int) []string {
	p := m.p()
	out := m.title("BOARD", fmt.Sprintf("%d notes · %d decisions", len(m.Snap.Board), len(m.Snap.Decisions)), w)
	lines := []string{p.paint(stBold, " NOTES")}
	kw := 4
	for _, e := range m.Snap.Board {
		kw = max(kw, min(width(e.Key), w/3))
	}
	for _, e := range m.Snap.Board {
		lines = append(lines, "  "+p.paint(stAccent, fit(clean(e.Key), kw))+"  "+clean(e.Value)+
			p.paint(stDim, fmt.Sprintf("  (%s, v%d)", cmp.Or(e.By, "?"), e.Version)))
	}
	if len(m.Snap.Board) == 0 {
		lines = append(lines, p.paint(stDim, "  none"))
	}
	lines = append(lines, "", p.paint(stBold, " DECISIONS"))
	for _, d := range m.Snap.Decisions {
		lines = append(lines, fmt.Sprintf("  %3d. %s", d.N, clean(d.Text))+p.paint(stDim, "  ("+cmp.Or(d.By, "?")+", "+d.Time.Local().Format("15:04")+")"))
	}
	if len(m.Snap.Decisions) == 0 {
		lines = append(lines, p.paint(stDim, "  none"))
	}
	return append(out, m.head(lines, h-len(out))...)
}

func (m *Model) usageView(w, h int) []string {
	p := m.p()
	out := m.title("USAGE", "tokens and cost as reported by each CLI", w)
	widths := []int{12, 12, 12, 12, 13, 12}
	out = append(out, p.paint(stDim, cols(widths, " AGENT", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE", "TOTAL", "COST")))
	row := func(id string, u activity.Tokens) string {
		return cols(widths, " "+clean(id), tok(u.Input), tok(u.Output), tok(u.CacheRead), tok(u.CacheWrite), tok(u.Total()), cost(u.CostUSD))
	}
	var lines []string
	if m.Snap.Usage != nil {
		ids := make([]string, 0, len(m.Snap.Usage.Agents))
		for id := range m.Snap.Usage.Agents {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			lines = append(lines, row(id, m.Snap.Usage.Agents[id]))
		}
	}
	if len(lines) == 0 {
		lines = []string{p.paint(stDim, " no usage reported yet")}
	}
	lines = append(lines, m.rule(w), p.paint(stBold, row("swarm", m.usage(""))))
	return append(out, m.head(lines, h-len(out))...)
}

// --- small formatting helpers ---

func spread(left, right string, w int) string {
	gap := w - width(left) - width(right)
	if gap < 1 {
		return trunc(left, max(0, w-width(right)-1)) + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func center(s string, w int) string {
	return strings.Repeat(" ", max(0, (w-width(s))/2)) + s
}

// stripStyle removes SGR escapes.
func stripStyle(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// wrap splits s into lines of at most w cells.
func wrap(s string, w int) []string {
	var out []string
	var cur strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w {
			out = append(out, cur.String())
			cur.Reset()
			n = 0
		}
		cur.WriteRune(r)
		n += rw
	}
	return append(out, cur.String())
}

func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func hms(d time.Duration) string {
	d = max(0, d)
	return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func dur(d time.Duration) string {
	d = max(0, d)
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func tok(n int64) string {
	switch {
	case n == 0:
		return "—"
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1e6)
}

func cost(c float64) string {
	if c == 0 {
		return "—"
	}
	return fmt.Sprintf("~$%.2f", c)
}

func usageText(u activity.Tokens) string {
	s := tok(u.Total()) + " tok"
	if u.CostUSD > 0 {
		s += " · " + cost(u.CostUSD)
	}
	return s
}
