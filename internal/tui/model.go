// Package tui is the terminal dashboard (plan §6): a Model fed by hub
// events, pure render functions from Model to a frame of lines, and a small
// terminal loop (term.go) that diffs frames onto the screen.
package tui

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/store"
)

type view int

const (
	vOverview view = iota
	vAgent
	vActivity
	vFiles
	vGraph
	vMessages
	vBoard
	vUsage
)

// maxActs bounds the activity kept in memory (the hub keeps 500 per agent).
const maxActs = 5000

// message is one bus message, as far as the dashboard can see it.
type message struct {
	TS                   time.Time
	From, To, Kind, Body string
}

// gone is an agent that left the swarm.
type gone struct {
	ID, Reason string
	TS         time.Time
}

// Model is everything the dashboard shows. It is only touched by the UI
// goroutine; the data loop hands it updates as functions.
type Model struct {
	Snap    hub.Snapshot
	Acts    []activity.Activity // by id, oldest first
	Msgs    []message           // oldest first
	Gone    []gone
	Details map[int64]json.RawMessage
	Conn    string // "" when connected; otherwise what is wrong
	Flash   string // one-off notice shown in the header
	Color   bool
	Dir     string
	Others  []hub.Running // live hubs for other directories, while offline
	Now     time.Time     // render time (fixed in tests)

	view     view
	sel      int    // overview row, files/graph cursor
	agent    string // agent detail subject
	cur      int    // agent detail cursor into its feed
	follow   bool
	expand   bool
	diffFile string // pinned diff file in agent detail; "" follows the latest edit
	scroll   int
	filter   string // messages view
	input    *input
	want     map[int64]bool // details the views asked for
	stale    bool           // Snap is behind the event stream
	noLogo   bool
	osel     int // selected entry of Others
}

// input is a one-line prompt (message an agent, confirm a kill, filter).
type input struct {
	label   string
	text    string
	kind    string // message kind, toggled with tab
	confirm bool   // y/N question: any key answers
	submit  func(m *Model, text string) Action
}

// Action is what the terminal loop must do after a key.
type Action struct {
	Quit    bool
	Send    *hub.SendParams
	Retire  string
	Control string // swarm.control action: hub.ActionPause, ActionResume or ActionStop
	Agent   string // fetch this agent's full recent activity
	Attach  string // switch to the hub for this project directory
}

// NewModel returns an empty model on the overview.
func NewModel(color bool) *Model {
	return &Model{Color: color, Details: map[int64]json.RawMessage{}, want: map[int64]bool{}, follow: true, cur: -1, Now: time.Now()}
}

// SetSnapshot replaces the hub state. Unacked messages from the hub itself
// are only visible in the queues, so they are picked up from there.
func (m *Model) SetSnapshot(s hub.Snapshot) {
	m.Snap = s
	for _, q := range s.Queues {
		for _, msg := range q {
			if msg.From == "hub" {
				m.addMsg(message{msg.Time, msg.From, msg.To, msg.Kind, msg.Body})
			}
		}
	}
}

// AddActivity merges activity in (duplicates by id are ignored).
func (m *Model) AddActivity(as ...activity.Activity) {
	for _, a := range as {
		i, found := slices.BinarySearchFunc(m.Acts, a.ID, func(x activity.Activity, id int64) int { return cmp.Compare(x.ID, id) })
		if found {
			continue
		}
		m.Acts = slices.Insert(m.Acts, i, a)
		switch {
		case a.Kind == activity.Msg:
			var p hub.SendParams
			json.Unmarshal(a.Detail, &p)
			m.addMsg(message{a.TS, cmp.Or(p.From, a.Agent), p.To, cmp.Or(p.Kind, "info"), p.Body})
		case a.Kind == activity.State && strings.HasPrefix(a.Summary, "left: "):
			m.Gone = slices.DeleteFunc(m.Gone, func(g gone) bool { return g.ID == a.Agent })
			m.Gone = append(m.Gone, gone{a.Agent, strings.TrimPrefix(a.Summary, "left: "), a.TS})
		}
	}
	if len(m.Acts) > maxActs {
		m.Acts = slices.Delete(m.Acts, 0, len(m.Acts)-maxActs)
	}
	if m.view == vAgent && m.follow {
		m.cur = -1
	}
}

func (m *Model) addMsg(msg message) {
	for _, x := range m.Msgs {
		if x == msg {
			return
		}
	}
	i, _ := slices.BinarySearchFunc(m.Msgs, msg.TS, func(x message, t time.Time) int {
		if x.TS.After(t) {
			return 1
		}
		return -1 // equal times keep arrival order
	})
	m.Msgs = slices.Insert(m.Msgs, i, msg)
	if len(m.Msgs) > 1000 {
		m.Msgs = m.Msgs[len(m.Msgs)-1000:]
	}
}

// AddEvent applies one hub event and reports whether it changed hub state
// (so the snapshot is stale).
func (m *Model) AddEvent(ev store.Event) bool {
	switch ev.Type {
	case "activity":
		var a activity.Activity
		if json.Unmarshal(ev.Data, &a) == nil {
			m.AddActivity(a)
		}
		return a.Usage != nil
	case "msg.send":
		var p hub.SendParams
		// Agents' messages arrive as activity; the hub's own don't.
		if json.Unmarshal(ev.Data, &p) == nil && p.From == "hub" {
			m.addMsg(message{ev.Time, p.From, p.To, cmp.Or(p.Kind, "info"), p.Body})
		}
	}
	return ev.Seq > 0
}

// SetDetail stores a fetched activity detail.
func (m *Model) SetDetail(id int64, d json.RawMessage) { m.Details[id] = d }

// NeedDetails returns (and forgets) the details the last render wanted.
func (m *Model) NeedDetails() []int64 {
	var ids []int64
	for id := range m.want {
		ids = append(ids, id)
	}
	clear(m.want)
	return ids
}

// detail returns a's full detail, or nil and a fetch request if it is
// stored separately and not loaded yet.
func (m *Model) detail(a activity.Activity) json.RawMessage {
	if a.DetailSize == 0 {
		return a.Detail
	}
	if d, ok := m.Details[a.ID]; ok {
		return d
	}
	m.want[a.ID] = true
	return nil
}

// --- derived data ---

func activeState(s graph.State) bool {
	return s == graph.Claimed || s == graph.Running || s == graph.Blocked || s == graph.Review
}

// taskOf returns the active task owned by agent, if any.
func (m *Model) taskOf(agent string) *graph.Task {
	for i, t := range m.Snap.Tasks {
		if t.Owner == agent && activeState(t.State) {
			return &m.Snap.Tasks[i]
		}
	}
	return nil
}

func (m *Model) agentByID(id string) *hub.Agent {
	for i := range m.Snap.Agents {
		if m.Snap.Agents[i].ID == id {
			return &m.Snap.Agents[i]
		}
	}
	return nil
}

// rows are the agents listed in the overview: live ones, then the most
// recently gone (at most 3, and never one that is live again).
func (m *Model) rows() []string {
	var ids []string
	for _, a := range m.Snap.Agents {
		ids = append(ids, a.ID)
	}
	var g []string
	for i := len(m.Gone) - 1; i >= 0 && len(g) < 3; i-- {
		if !slices.Contains(ids, m.Gone[i].ID) {
			g = append(g, m.Gone[i].ID)
		}
	}
	return append(ids, g...)
}

func (m *Model) goneReason(id string) (string, bool) {
	for i := len(m.Gone) - 1; i >= 0; i-- {
		if m.Gone[i].ID == id {
			return m.Gone[i].Reason, true
		}
	}
	return "", false
}

// lastSeen is the time of agent's latest activity.
func (m *Model) lastSeen(agent string) time.Time {
	for i := len(m.Acts) - 1; i >= 0; i-- {
		if m.Acts[i].Agent == agent {
			return m.Acts[i].TS
		}
	}
	return time.Time{}
}

func (m *Model) usage(agent string) activity.Tokens {
	if m.Snap.Usage == nil {
		return activity.Tokens{}
	}
	if agent == "" {
		return m.Snap.Usage.Swarm
	}
	return m.Snap.Usage.Agents[agent]
}

func (m *Model) leasesOf(agent string) []string {
	var out []string
	for _, l := range m.Snap.Leases {
		if l.Holder == agent {
			out = append(out, l.Path)
		}
	}
	return out
}

func (m *Model) goal() string {
	for _, e := range m.Snap.Board {
		if e.Key == "goal" {
			return e.Value
		}
	}
	return ""
}

// entry is one feed line: an activity, with the +/- counts of its edit when
// the diff engine reported them.
type entry struct {
	activity.Activity
	plus, minus int
	counted     bool
}

// feed turns activity into feed lines. An edit shows up twice in the
// stream: as the tool call (stream/hook) and, up to a poll later, as the
// diff engine's edit of the same file. The rule: a diff edit is folded into
// the latest still-uncounted tool edit of the same agent and file (it can
// only come after it); a diff edit with no such call (no stream, no hooks)
// stays a line of its own. Usage-only events are dropped.
func feed(acts []activity.Activity, keep func(activity.Activity) bool) []entry {
	var out []entry
	pending := map[string]int{} // agent + file -> index in out
	key := func(agent, file string) string { return agent + "\x00" + strings.ToLower(file) }
	for _, a := range acts {
		if a.Kind == activity.Usage || keep != nil && !keep(a) {
			continue
		}
		if a.Kind == activity.Edit && len(a.Files) > 0 {
			k := key(a.Agent, a.Files[0])
			if a.Source == activity.SrcDiff {
				plus, minus := editCounts(a)
				if i, ok := pending[k]; ok {
					out[i].plus, out[i].minus, out[i].counted = plus, minus, true
					delete(pending, k)
					continue
				}
				out = append(out, entry{a, plus, minus, true})
				continue
			}
			pending[k] = len(out)
		}
		out = append(out, entry{Activity: a})
	}
	return out
}

// editCounts reads +N −M from a diff edit's summary ("path +N −M").
func editCounts(a activity.Activity) (plus, minus int) {
	f := strings.Fields(a.Summary)
	if len(f) >= 2 {
		plus, _ = strconv.Atoi(strings.TrimPrefix(f[len(f)-2], "+"))
		minus, _ = strconv.Atoi(strings.TrimPrefix(f[len(f)-1], "−"))
	}
	return plus, minus
}

// diffEdit is a parsed diff-engine edit detail.
type diffEdit struct {
	Path           string
	Diff           string
	Added, Removed int
}

// latestEdit returns agent's most recent diff edit (of file, if set).
func (m *Model) latestEdit(agent, file string) (activity.Activity, bool) {
	for i := len(m.Acts) - 1; i >= 0; i-- {
		a := m.Acts[i]
		if a.Kind == activity.Edit && a.Source == activity.SrcDiff && len(a.Files) > 0 &&
			(agent == "" || a.Agent == agent) && (file == "" || strings.EqualFold(a.Files[0], file)) {
			return a, true
		}
	}
	return activity.Activity{}, false
}

// touched lists the files agent edited (per the diff engine), most recent
// first, with the latest counts.
func (m *Model) touched(agent string) []entry {
	var out []entry
	seen := map[string]bool{}
	for i := len(m.Acts) - 1; i >= 0; i-- {
		a := m.Acts[i]
		if a.Kind != activity.Edit || a.Source != activity.SrcDiff || len(a.Files) == 0 || (agent != "" && a.Agent != agent) {
			continue
		}
		k := strings.ToLower(a.Files[0])
		if seen[k] {
			continue
		}
		seen[k] = true
		p, n := editCounts(a)
		out = append(out, entry{a, p, n, true})
	}
	return out
}

// --- keys ---

// Key handles one key press (names from parseKeys: "up", "enter", "esc",
// "tab", "backspace", "ctrl+c", or the typed character).
func (m *Model) Key(k string) Action {
	if k == "ctrl+c" {
		return Action{Quit: true}
	}
	if m.input != nil {
		return m.inputKey(k)
	}
	m.Flash = ""
	if m.isOffline() {
		switch k {
		case "up":
			m.osel = max(0, m.osel-1)
		case "down":
			m.osel = min(max(0, len(m.Others)-1), m.osel+1)
		case "enter":
			if m.osel < len(m.Others) {
				m.Dir, m.Conn, m.Others = m.Others[m.osel].Dir, "connecting to the hub…", nil
				return Action{Attach: m.Dir}
			}
		case "q":
			return Action{Quit: true}
		}
		return Action{}
	}
	switch k {
	case "q":
		return Action{Quit: true}
	case "esc":
		if m.view == vAgent && m.expand {
			m.expand = false
		} else {
			m.setView(vOverview)
		}
		return Action{}
	case "a":
		m.setView(vActivity)
		return Action{}
	case "F":
		m.setView(vFiles)
		return Action{}
	case "g":
		m.setView(vGraph)
		return Action{}
	case "b":
		m.setView(vBoard)
		return Action{}
	case "u":
		m.setView(vUsage)
		return Action{}
	case "p":
		return m.togglePause()
	case "x":
		m.askStop()
		return Action{}
	}
	switch m.view {
	case vOverview:
		rows := m.rows()
		switch k {
		case "up":
			m.sel = max(0, m.sel-1)
		case "down":
			m.sel = min(max(0, len(rows)-1), m.sel+1)
		case "enter":
			if m.sel < len(rows) {
				return m.openAgent(rows[m.sel])
			}
		case "m":
			m.setView(vMessages)
		case "k":
			if m.sel < len(rows) {
				m.askRetire(rows[m.sel])
			}
		}
	case vAgent:
		return m.agentKey(k)
	case vFiles:
		n := len(m.touched(""))
		switch k {
		case "up":
			m.sel = max(0, m.sel-1)
		case "down":
			m.sel = min(max(0, n-1), m.sel+1)
		}
	case vMessages:
		switch k {
		case "/":
			m.input = &input{label: "filter (agent or topic)", text: m.filter, submit: func(m *Model, s string) Action {
				m.filter = strings.TrimSpace(s)
				return Action{}
			}}
		case "m":
		default:
			m.scrollKey(k, true)
		}
	default:
		m.scrollKey(k, m.view == vActivity)
	}
	return Action{}
}

// isOffline is true while no hub has been reached (the offline screen).
func (m *Model) isOffline() bool {
	return m.Conn != "" && m.Snap.Agents == nil && m.Snap.Tasks == nil && len(m.Acts) == 0
}

// SetOthers updates the hubs offered on the offline screen, keeping the
// selection on the same directory when it is still listed.
func (m *Model) SetOthers(rs []hub.Running) {
	cur := ""
	if m.osel < len(m.Others) {
		cur = m.Others[m.osel].Dir
	}
	m.Others, m.osel = rs, 0
	for i, r := range rs {
		if r.Dir == cur {
			m.osel = i
		}
	}
}

func (m *Model) setView(v view) {
	m.view, m.scroll, m.expand = v, 0, false
	if v != vOverview {
		m.sel = 0
	} else if m.agent != "" {
		m.sel = max(0, slices.Index(m.rows(), m.agent))
	}
}

// scrollKey scrolls a list; fromEnd lists follow their newest line.
func (m *Model) scrollKey(k string, fromEnd bool) {
	d := 0
	switch k {
	case "up":
		d = -1
	case "down":
		d = 1
	case "pgup":
		d = -10
	case "pgdn":
		d = 10
	case "home":
		d = -1 << 20
	case "end":
		d = 1 << 20
	}
	if fromEnd {
		d = -d
	}
	m.scroll = max(0, m.scroll+d)
}

func (m *Model) openAgent(id string) Action {
	m.view, m.agent, m.cur, m.follow, m.expand, m.diffFile = vAgent, id, -1, true, false, ""
	return Action{Agent: id}
}

func (m *Model) agentKey(k string) Action {
	entries := m.agentFeed()
	cur := m.cursor(len(entries))
	switch k {
	case "up", "down", "pgup", "pgdn", "home", "end":
		step := map[string]int{"up": -1, "down": 1, "pgup": -10, "pgdn": 10, "home": -1 << 20, "end": 1 << 20}[k]
		m.cur = min(max(0, cur+step), max(0, len(entries)-1))
		m.follow = m.cur == len(entries)-1 && step > 0
		if m.follow {
			m.cur = -1
		}
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.cur, m.diffFile = -1, ""
		} else {
			m.cur = cur
		}
	case "enter":
		m.expand = !m.expand && len(entries) > 0
		if m.expand {
			m.follow, m.cur = false, cur
		}
	case "o":
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Kind == activity.CmdResult {
				m.cur, m.follow, m.expand = i, false, true
				break
			}
		}
	case "d":
		files := m.touched(m.agent)
		if len(files) == 0 {
			break
		}
		i := slices.IndexFunc(files, func(e entry) bool { return strings.EqualFold(e.Files[0], m.diffFile) })
		m.diffFile = files[(i+1)%len(files)].Files[0]
		m.expand = false
	case "left", "right":
		rows := m.rows()
		if i := slices.Index(rows, m.agent); i >= 0 && len(rows) > 0 {
			d := map[string]int{"left": -1, "right": 1}[k]
			return m.openAgent(rows[(i+d+len(rows))%len(rows)])
		}
	case "m":
		agent := m.agent
		m.input = &input{label: "message " + agent, kind: "directive", submit: func(m *Model, s string) Action {
			if s = strings.TrimSpace(s); s == "" {
				return Action{}
			}
			return Action{Send: &hub.SendParams{From: "human", To: "agent:" + agent, Kind: m.input.kind, Body: s}}
		}}
	case "k":
		m.askRetire(m.agent)
	}
	return Action{}
}

// cursor is the feed index under the cursor (the newest when following).
func (m *Model) cursor(n int) int {
	if m.cur < 0 || m.cur >= n {
		return n - 1
	}
	return m.cur
}

func (m *Model) agentFeed() []entry {
	agent := m.agent
	return feed(m.Acts, func(a activity.Activity) bool { return a.Agent == agent })
}

func (m *Model) askRetire(id string) {
	if m.agentByID(id) == nil {
		m.Flash = id + " is not in the swarm"
		return
	}
	m.input = &input{label: "retire " + id + "? its task is requeued [y/N]", confirm: true, submit: func(*Model, string) Action {
		return Action{Retire: id}
	}}
}

// runState is the swarm's run state: hub.RunRunning, RunPaused or RunStopped.
func (m *Model) runState() string {
	if m.Snap.State == "" {
		return hub.RunRunning
	}
	return m.Snap.State
}

// togglePause pauses a running swarm or resumes a paused one.
func (m *Model) togglePause() Action {
	switch m.runState() {
	case hub.RunStopped:
		m.Flash = "the swarm is stopped"
		return Action{}
	case hub.RunPaused:
		return Action{Control: hub.ActionResume}
	}
	return Action{Control: hub.ActionPause}
}

// askStop confirms, then stops the swarm for good.
func (m *Model) askStop() {
	if m.runState() == hub.RunStopped {
		m.Flash = "the swarm is already stopped"
		return
	}
	m.input = &input{label: "stop the swarm? every open task is cancelled and workers exit [y/N]", confirm: true, submit: func(*Model, string) Action {
		return Action{Control: hub.ActionStop}
	}}
}

func (m *Model) inputKey(k string) Action {
	in := m.input
	if in.confirm {
		m.input = nil
		if k == "y" || k == "Y" {
			return in.submit(m, "")
		}
		return Action{}
	}
	switch k {
	case "esc":
		m.input = nil
	case "enter":
		a := in.submit(m, in.text)
		m.input = nil
		return a
	case "backspace":
		if r := []rune(in.text); len(r) > 0 {
			in.text = string(r[:len(r)-1])
		}
	case "tab":
		if in.kind != "" {
			in.kind = map[string]string{"directive": "info", "info": "directive"}[in.kind]
		}
	default:
		if len([]rune(k)) == 1 && k >= " " {
			in.text += k
		}
	}
	return Action{}
}
