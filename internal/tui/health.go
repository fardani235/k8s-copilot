package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// The health screen: what is broken in the cluster right now, and — as
// plainly — what was not looked at.
//
// This file only draws and navigates. Every finding, every class state and
// every sentence explaining one comes from internal/health, which is also what
// the copilot's get_findings tool prints, so the two cannot disagree. The
// screen's own job is honesty of presentation: a class that was not watched is
// drawn as not watched, never as an empty list, and the reading's age is
// always on show.

const (
	// minHealthInterval is the fastest the screen re-reads: it makes more
	// requests than the metrics screen, so its floor is higher.
	minHealthInterval = 15 * time.Second
	healthClockEvery  = 5 * time.Second
)

type healthState struct {
	// obs is the observation on show; report is what internal/health made
	// of it.
	obs    *health.Observation
	report *health.Report
	// rows are the findings on show: indexes into report.Findings that match
	// the filter.
	rows   []int
	filter string

	loading bool
	seq     int
	asked   time.Time
	clock   int

	cursor, offset int
	page           int
}

type healthMsg struct {
	seq int
	obs *health.Observation
}

type healthClockMsg struct{ gen int }

// healthInterval is how often the screen re-reads; 0 when periodic refresh is
// switched off.
func (m *Model) healthInterval() time.Duration {
	if m.deps.Refresh <= 0 {
		return 0
	}
	return max(m.deps.Refresh, minHealthInterval)
}

// healthDue reports whether a tick should start a read: only while the screen
// is visible, and never on top of a read still out.
func (m *Model) healthDue() bool {
	hs := &m.health
	iv := m.healthInterval()
	return m.view == vHealth && m.pick == nil && !m.copilotMax && !hs.loading && iv > 0 &&
		time.Since(hs.asked) >= iv-time.Second
}

// healthClock (re)starts the redraw timer that keeps the stated age counting.
// Each start retires the one before it.
func (m *Model) healthClock() tea.Cmd {
	m.health.clock++
	gen := m.health.clock
	return tea.Tick(healthClockEvery, func(time.Time) tea.Msg { return healthClockMsg{gen} })
}

func (m *Model) onHealthClock(msg healthClockMsg) tea.Cmd {
	if msg.gen != m.health.clock || m.view != vHealth {
		return nil
	}
	return tea.Tick(healthClockEvery, func(time.Time) tea.Msg { return msg })
}

// openHealth shows the health screen. If it is already open underneath — H
// pressed in a view opened from it — it goes back to it, where it was left.
func (m *Model) openHealth() tea.Cmd {
	if m.view == vHealth {
		return nil
	}
	for i := len(m.stack) - 1; i >= 0; i-- {
		if m.stack[i] != vHealth {
			continue
		}
		if m.view == vLogs {
			m.logs.stop()
		}
		m.view, m.stack = vHealth, m.stack[:i]
		m.layout()
		return tea.Batch(m.loadHealth(kube.HealthFreshFor), m.healthClock())
	}
	hs := &m.health
	hs.filter, hs.cursor, hs.offset = "", 0, 0
	m.push(vHealth)
	m.rebuildHealth("")
	// A recent observation — the copilot's, say — is shown as it is, without
	// a further read.
	return tea.Batch(m.loadHealth(kube.HealthFreshFor), m.healthClock())
}

// loadHealth reads off the UI goroutine. An observation up to maxAge old is
// taken as it is; 0 reads the cluster.
func (m *Model) loadHealth(maxAge time.Duration) tea.Cmd {
	hs := &m.health
	hs.seq++
	hs.loading, hs.asked = true, time.Now()
	seq := hs.seq
	// Refused across the cluster, a class is read for the browser's
	// namespace instead.
	fallback := m.curNS
	c := m.deps.Cluster
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, kube.HealthObservationBound+5*time.Second)
		defer cancel()
		return healthMsg{seq: seq, obs: c.HealthObservation(ctx, fallback, maxAge)}
	}
}

func (m *Model) onHealth(msg healthMsg) {
	hs := &m.health
	if msg.seq != hs.seq {
		return // superseded
	}
	hs.loading = false
	keep := hs.selectedKey()
	hs.obs, hs.report = msg.obs, health.Observe(msg.obs)
	m.rebuildHealth(keep)
}

// adoptHealth makes the screen show the findings the copilot was just given,
// from the shared observation — no further read.
func (m *Model) adoptHealth() tea.Cmd {
	if m.view != vHealth || m.health.loading {
		return nil
	}
	return m.loadHealth(kube.HealthFreshFor)
}

// rebuildHealth recomputes the rows on show and puts the cursor back on the
// finding called keep, wherever a refresh moved it.
func (m *Model) rebuildHealth(keep string) {
	hs := &m.health
	hs.rows = hs.rows[:0]
	if hs.report == nil {
		return
	}
	q := strings.ToLower(strings.TrimSpace(hs.filter))
	for i := range hs.report.Findings {
		if q == "" || strings.Contains(hs.report.Findings[i].Search(), q) {
			hs.rows = append(hs.rows, i)
		}
	}
	hs.cursor = clamp(hs.cursor, 0, len(hs.rows)-1)
	if keep == "" {
		return
	}
	for i, idx := range hs.rows {
		if hs.report.Findings[idx].Key() == keep {
			hs.cursor = i
			return
		}
	}
}

func (hs *healthState) selected() (*health.Finding, bool) {
	if hs.report == nil || hs.cursor < 0 || hs.cursor >= len(hs.rows) {
		return nil, false
	}
	return &hs.report.Findings[hs.rows[hs.cursor]], true
}

func (hs *healthState) selectedKey() string {
	if f, ok := hs.selected(); ok {
		return f.Key()
	}
	return ""
}

// --- keys -----------------------------------------------------------------------

func (m *Model) onHealthKey(msg tea.KeyMsg) tea.Cmd {
	hs := &m.health
	page := max(hs.page, 1)
	move := func(d int) { hs.cursor = clamp(hs.cursor+d, 0, len(hs.rows)-1) }
	switch msg.String() {
	case "up", "k":
		move(-1)
	case "down", "j":
		move(1)
	case "pgup", "ctrl+b":
		move(-page)
	case "pgdown", "ctrl+f", " ":
		move(page)
	case "home", "g":
		hs.cursor = 0
	case "end", "G":
		move(1 << 30)
	case "/":
		m.filtering = true
		m.filterIn.SetValue(hs.filter)
		m.filterIn.CursorEnd()
		return m.filterIn.Focus()
	case "r":
		m.status = "reading the cluster…"
		return m.loadHealth(0)
	case "enter", "d":
		return m.openFromHealth(vDetail)
	case "l":
		return m.openFromHealth(vLogs)
	case "e":
		return m.openFromHealth(vEvents)
	}
	return nil
}

// healthBack clears the filter, if there is one, and reports whether it did.
func (m *Model) healthBack(key string) bool {
	hs := &m.health
	if key == "esc" && hs.filter != "" {
		hs.filter = ""
		m.rebuildHealth(hs.selectedKey())
		return true
	}
	return false
}

// openFromHealth opens the browser's detail, logs or events view for the
// selected finding's object. Leaving that view comes back here, to the same
// finding.
func (m *Model) openFromHealth(v viewID) tea.Cmd {
	f, ok := m.health.selected()
	if !ok {
		m.status = "nothing is selected"
		return nil
	}
	if v == vLogs && f.Kind != "Pod" {
		m.status = "logs belong to pods: this finding is about a " + f.Kind
		return nil
	}
	t, err := kube.ResolveIn(m.types, f.Kind)
	if err != nil {
		m.status = "cannot open " + f.Kind + ": the cluster's resource types are not known"
		return nil
	}
	ns := f.Namespace
	if !t.Namespaced {
		ns = ""
	}
	m.subj, m.subjErr = subject{typ: t, namespace: ns, name: f.Name}, nil
	cmd := m.openSubject(v)
	if v == vLogs {
		m.logs.want = f.Container
	}
	return cmd
}

// healthFocus tells the copilot what the health screen shows, so "why is this
// one failing?" resolves to the selected finding.
func (m *Model) healthFocus(f *agent.Focus) {
	hs := &m.health
	// The namespace is the browser's: it is what the screen falls back to
	// when a cluster-wide read is refused, and what get_findings should be
	// given. The selected finding's own namespace is stated with it.
	f.Namespace, f.AllNamespaces = m.curNS, m.curNS == kube.AllNamespaces
	f.Type, f.Kind, f.Namespaced, f.Selected, f.SelectedNamespace = "", "", false, "", ""
	f.View = "health screen: the cluster's current findings, grouped by class"
	if hs.report == nil {
		f.View += " (still reading)"
		return
	}
	var states []string
	for i := range hs.report.Classes {
		c := &hs.report.Classes[i]
		states = append(states, string(c.ID)+" "+c.Short())
	}
	f.View += fmt.Sprintf("; read %s ago; %d findings; classes: %s", hs.report.Age(time.Now()), len(hs.report.Findings), strings.Join(states, ", "))
	if hs.filter != "" {
		f.View += fmt.Sprintf("; filtered by %q, %d shown", textutil.OneLine(textutil.Sanitize(hs.filter)), len(hs.rows))
	}
	sel, ok := hs.selected()
	if !ok {
		f.View += "; no finding selected"
		return
	}
	f.Kind, f.Selected, f.SelectedNamespace = sel.Kind, sel.Name, sel.Namespace
	f.Type, f.Namespaced = strings.ToLower(sel.Kind), sel.Namespace != ""
	if t, err := kube.ResolveIn(m.types, sel.Kind); err == nil {
		f.Type, f.Namespaced = t.String(), t.Namespaced
	}
	what := sel.Object()
	if sel.Container != "" {
		what += " container " + sel.Container
	}
	f.View += fmt.Sprintf("; selected finding: [%s] %s on %s", sel.Class, sel.Reason, what)
}

// --- drawing --------------------------------------------------------------------

func classStyle(c *health.Class) lipgloss.Style {
	switch {
	case c.State != health.Watched:
		return stBad
	case !c.Complete():
		return stWarn
	}
	return stGood
}

func (m *Model) viewHealth(w, h int) string {
	hs := &m.health
	now := time.Now()
	title := m.healthTitle(w, now)
	if hs.report == nil {
		// The first read is still out. Say so; do not draw an empty list.
		return fitBox(title+"\n\n"+stDim.Render(wrap("Reading the cluster: pods, nodes, rollouts, Warning events and metrics…", w)), w, h)
	}
	r := hs.report
	lines := []string{title}

	// What each class is: one line each where there is room, else one line
	// for all of them. The state is what tells a clear class from one that
	// was not looked at, so it is never dropped.
	if h >= 16 {
		for i := range r.Classes {
			c := &r.Classes[i]
			detail := c.Short()
			if c.Findings > 0 {
				detail += fmt.Sprintf(" · %d", c.Findings)
			}
			if c.Total > c.Findings {
				detail += fmt.Sprintf(" of %d", c.Total)
			}
			if c.State != health.Watched {
				detail += " — " + c.Why
			} else if notes := c.Notes(); len(notes) > 0 {
				detail += " — " + strings.Join(notes, "; ")
			}
			lines = append(lines, clip(classStyle(c).Render(fit(string(c.ID), 9)+" "+textutil.OneLine(detail)), w))
		}
	} else {
		// One or two lines for all five. What was not watched goes first: in
		// a narrow pane it is the clear classes that are cut, never the
		// fact that one was not looked at.
		var parts []string
		for _, pass := range []func(*health.Class) bool{
			func(c *health.Class) bool { return c.State != health.Watched },
			func(c *health.Class) bool { return c.State == health.Watched && !c.Clear() },
			func(c *health.Class) bool { return c.Clear() },
		} {
			for i := range r.Classes {
				if c := &r.Classes[i]; pass(c) {
					parts = append(parts, classStyle(c).Render(c.ID.Tag()+" "+c.Brief()))
				}
			}
		}
		wrapped := strings.Split(wrap(strings.Join(parts, stDim.Render(" · ")), w), "\n")
		limit := 1
		if h >= 10 {
			limit = 2
		}
		if len(wrapped) > limit {
			wrapped = wrapped[:limit]
			wrapped[limit-1] = clip(wrapped[limit-1], max(w-2, 1)) + stDim.Render(" …")
		}
		for _, l := range wrapped {
			lines = append(lines, clip(l, w))
		}
	}

	// The filter goes first on its line, so a narrow pane still shows that
	// only some findings are listed.
	if m.filtering || hs.filter != "" {
		count := stDim.Render(fmt.Sprintf(" %d of %d", len(hs.rows), len(r.Findings)))
		if m.filtering {
			lines = append(lines, clip(m.filterIn.View()+count, w))
		} else {
			lines = append(lines, clip(stWarn.Render("/"+textutil.OneLine(textutil.Sanitize(hs.filter)))+count, w))
		}
	}

	rest := max(h-len(lines), 1)
	return fitBox(strings.Join(lines, "\n")+"\n"+m.healthBody(w, rest, now), w, h)
}

// healthTitle is the first line: the count, and how old the reading is.
func (m *Model) healthTitle(w int, now time.Time) string {
	hs := &m.health
	left := stHeader.Render("health")
	if hs.report == nil {
		return clip(left+stDim.Render(" · reading…"), w)
	}
	left += stDim.Render(" · ") + fmt.Sprintf("%d findings", len(hs.report.Findings))
	age := "read " + hs.report.Age(now) + " ago"
	short := hs.report.Age(now) + " old"
	if hs.loading {
		age += " · reading…"
		short += " · reading…"
	}
	if m.healthInterval() == 0 {
		age += " · auto-refresh off"
	}
	for _, a := range []string{age, short} {
		if gap := w - lipWidth(left) - lipWidth(a); gap >= 2 {
			return left + strings.Repeat(" ", gap) + stDim.Render(a)
		}
	}
	// No room for both: the age matters more than the count.
	return clip(stHeader.Render("health")+stDim.Render(" · "+short), w)
}

// healthBody is the list of findings — or, when there are none to show, what
// that does and does not mean — with the selected finding's detail below.
func (m *Model) healthBody(w, rest int, now time.Time) string {
	hs := &m.health
	r := hs.report
	hs.page = 1
	if len(hs.rows) == 0 {
		var text string
		switch {
		case len(r.Findings) > 0:
			text = stDim.Render(wrap(fmt.Sprintf("Nothing matches the filter %q. Press esc to clear it.", textutil.Sanitize(hs.filter)), w))
		case r.AllClear():
			text = stGood.Render(wrap("No findings. Every class was watched across the whole cluster and none reported a problem.", w)) +
				"\n" + stDim.Render(wrap(health.NotCovered, w))
		default:
			text = stWarn.Render(wrap("No findings among what was read. A class marked above as not watched, or watched in part, was not fully checked: problems there are unknown, not absent.", w)) +
				"\n" + stDim.Render(wrap(health.NotCovered, w))
		}
		return fitLines("\n"+text, rest)
	}

	// The selected finding's whole message goes at the bottom, when there
	// is room for it beside at least three rows.
	var detail []string
	if f, ok := hs.selected(); ok && rest >= 6 {
		text := f.Message
		if age := f.Age(now); age != "" {
			if f.Class == health.Events {
				text = fmt.Sprintf("last %s ago, %d× · %s", age, f.Count, text)
			} else {
				text = "for " + age + " · " + text
			}
		}
		for _, l := range strings.Split(wrap(text, w), "\n") {
			detail = append(detail, stDim.Render(l))
		}
		limit := clamp(rest-4, 1, 3)
		if len(detail) > limit {
			detail = detail[:limit]
			detail[limit-1] = clip(detail[limit-1], max(w-2, 1)) + stDim.Render(" …")
		}
	}
	rows := max(rest-len(detail)-boolInt(len(detail) > 0), 1)
	hs.page = rows
	if hs.cursor < hs.offset {
		hs.offset = hs.cursor
	}
	if hs.cursor >= hs.offset+rows {
		hs.offset = hs.cursor - rows + 1
	}
	hs.offset = clamp(hs.offset, 0, max(len(hs.rows)-rows, 0))

	var out []string
	for i := hs.offset; i < len(hs.rows) && i < hs.offset+rows; i++ {
		f := &r.Findings[hs.rows[i]]
		// Class and object first: in a narrow pane it is the reason and the
		// message that are cut, with a marker.
		obj := f.Object()
		if f.Container != "" {
			obj += " [" + f.Container + "]"
		}
		text := f.Class.Tag() + " " + obj + "  " + f.Reason
		if f.Class == health.Events && f.Count > 1 {
			text += fmt.Sprintf(" ×%d", f.Count)
		}
		if age := f.Age(now); age != "" {
			text += "  " + age
		}
		style := stWarn
		if f.Severity == health.Critical {
			style = stBad
		}
		if i == hs.cursor {
			out = append(out, stSelected.Foreground(style.GetForeground()).Render(fit(text, w)))
		} else {
			out = append(out, style.Render(clip(text, w)))
		}
	}
	if len(detail) > 0 {
		for len(out) < rows {
			out = append(out, "")
		}
		out = append(out, "")
		out = append(out, detail...)
	}
	return strings.Join(out, "\n")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// healthHints are the footer's key hints for the health screen: only what
// does something here.
func (m *Model) healthHints() string {
	var pairs []string
	if len(m.health.rows) > 0 {
		pairs = append(pairs, "↑↓", "move", "enter", "detail")
		if f, ok := m.health.selected(); ok && f.Kind == "Pod" {
			pairs = append(pairs, "l", "logs")
		}
		pairs = append(pairs, "e", "events")
	}
	if m.health.report != nil && len(m.health.report.Findings) > 0 {
		pairs = append(pairs, "/", "filter")
	}
	pairs = append(pairs, "r", "refresh", "esc", "back", "tab", "ask", "?", "help", "q", "quit")
	return hints(pairs...)
}
