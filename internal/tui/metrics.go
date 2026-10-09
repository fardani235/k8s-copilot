package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// The metrics screen: how loaded the cluster is right now, from the big
// picture (nodes, namespaces) down to one container.
//
// This file only draws and navigates. Every number, percentage, caveat and
// "this is unavailable because…" sentence comes from internal/metrics, which
// is also what the copilot's get_metrics tool prints — so the two cannot
// disagree. The screen's own job is honesty of presentation: a reading that
// is not there is never drawn as 0, and a source that is not there is never
// drawn as an empty table.

const (
	// minMetricsInterval is the fastest the screen re-reads. The metrics API
	// has a new sample every 15s or so; asking more often shows the same
	// numbers again.
	minMetricsInterval = 10 * time.Second
	barWidth           = 10
)

type crumbKind int

const (
	crumbNode crumbKind = iota
	crumbNamespace
	crumbPod
)

// crumb is one step of drilling down.
type crumb struct {
	kind      crumbKind
	namespace string
	name      string
	// back is the row the cursor was on when stepping in, so that stepping
	// out lands on it again.
	back string
}

func (c crumb) String() string {
	switch c.kind {
	case crumbNode:
		return "node " + c.name
	case crumbNamespace:
		return "namespace " + c.name
	}
	return "pod " + c.namespace + "/" + c.name
}

type metricsState struct {
	snap *metrics.Snapshot
	// list is what is on screen: snap seen through the current tab, path,
	// sort and filter. widths are its columns' natural widths.
	list   *metrics.Listing
	widths []int

	loading bool
	seq     int
	asked   time.Time // when the last read was started

	// scope is the namespace whose pods are on show; empty is the whole
	// cluster, which is what the screen asks for every time. narrowed is set
	// when the cluster as a whole was refused and the screen fell back to
	// the browser's namespace: it holds the refusal, which stays on show.
	scope    string
	narrowed *metrics.SourceStatus

	// clock numbers the running redraw timer; see metricsClock.
	clock int

	tab    metrics.Level // the top-level listing
	path   []crumb       // drill-down below it
	sort   metrics.Sort
	filter string

	cursor, offset int
	page           int // rows visible at the last draw
}

type metricsMsg struct {
	seq      int
	snap     *metrics.Snapshot
	narrowed *metrics.SourceStatus
}

// metricsClockMsg redraws the screen so that the age it states keeps
// counting. Without it "sampled 8s ago" would stay on a screen nobody
// touches — for as long as the refresh interval, or for ever with refresh
// switched off — and old readings would go on claiming to be fresh.
type metricsClockMsg struct{ gen int }

const metricsClockEvery = 5 * time.Second

// metricsClock (re)starts the redraw timer. Each start retires the one
// before it, so there is never more than one running.
func (m *Model) metricsClock() tea.Cmd {
	m.metrics.clock++
	gen := m.metrics.clock
	return tea.Tick(metricsClockEvery, func(time.Time) tea.Msg { return metricsClockMsg{gen} })
}

// onMetricsClock keeps the timer going while the screen is the current view.
// Returning a message at all is what makes the runtime redraw.
func (m *Model) onMetricsClock(msg metricsClockMsg) tea.Cmd {
	if msg.gen != m.metrics.clock || m.view != vMetrics {
		return nil
	}
	return tea.Tick(metricsClockEvery, func(time.Time) tea.Msg { return msg })
}

// metricsInterval is how often the screen re-reads; 0 when periodic refresh
// is switched off.
func (m *Model) metricsInterval() time.Duration {
	if m.deps.Refresh <= 0 {
		return 0
	}
	return max(m.deps.Refresh, minMetricsInterval)
}

// metricsDue reports whether a tick should start a read: only while the
// screen is actually visible, and never on top of a read still running (a
// slow metrics API is waited for, not piled onto).
func (m *Model) metricsDue() bool {
	ms := &m.metrics
	return m.view == vMetrics && m.pick == nil && !m.copilotMax && !ms.loading &&
		time.Since(ms.asked) >= m.metricsInterval()-time.Second
}

// openMetrics shows the metrics screen, at the top of the hierarchy. If the
// screen is already open underneath — M pressed in the logs or detail view
// that was opened from it — it goes back to it, where it was left, instead
// of opening a second one on top.
func (m *Model) openMetrics() tea.Cmd {
	if m.view == vMetrics {
		return nil
	}
	for i := len(m.stack) - 1; i >= 0; i-- {
		if m.stack[i] != vMetrics {
			continue
		}
		if m.view == vLogs {
			m.logs.stop()
		}
		m.view, m.stack = vMetrics, m.stack[:i]
		m.layout()
		return tea.Batch(m.loadMetrics(false), m.metricsClock())
	}
	ms := &m.metrics
	if ms.tab == "" {
		ms.tab, ms.sort = metrics.LevelNodes, metrics.ByCPU
	}
	ms.path, ms.filter, ms.cursor, ms.offset = nil, "", 0, 0
	m.push(vMetrics)
	m.rebuildMetrics("")
	return tea.Batch(m.loadMetrics(false), m.metricsClock())
}

// loadMetrics reads the cluster's load off the UI goroutine. force asks the
// cluster even if recent readings exist.
func (m *Model) loadMetrics(force bool) tea.Cmd {
	ms := &m.metrics
	ms.seq++
	ms.loading, ms.asked = true, time.Now()
	seq, home := ms.seq, m.curNS
	maxAge := m.metricsInterval() / 2
	if force {
		maxAge = 0
	}
	c := m.deps.Cluster
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, kube.MetricsTimeout+5*time.Second)
		defer cancel()
		// The whole cluster is asked for every time, so that the screen
		// widens again by itself if a refusal is lifted.
		msg := metricsMsg{seq: seq, snap: c.Metrics(ctx, kube.AllNamespaces, maxAge)}
		// Someone who may read pods in their own namespace but not across
		// the cluster still gets a screen: the browser's namespace, with the
		// refusal on show. (The cluster-wide read just made is reused for
		// the nodes; only the namespace's pods are read in addition.)
		if st, denied := msg.snap.PodsDeniedClusterWide(); denied && home != kube.AllNamespaces {
			if narrow := c.Metrics(ctx, home, max(maxAge, time.Second)); narrow.Sources.PodMetrics.OK() {
				msg.snap, msg.narrowed = narrow, &st
			}
		}
		return msg
	}
}

func (m *Model) onMetrics(msg metricsMsg) {
	ms := &m.metrics
	if msg.seq != ms.seq {
		return // a newer read superseded this one
	}
	ms.loading = false
	keep := ms.selectedKey()
	ms.snap, ms.scope, ms.narrowed = msg.snap, msg.snap.Scope, msg.narrowed
	// Drilled into a namespace the readings no longer cover (the screen has
	// just been limited to another one): there is nothing truthful to show
	// there. Back to the top, saying why.
	for _, c := range ms.path {
		ns := c.namespace // a pod's
		if c.kind == crumbNamespace {
			ns = c.name
		}
		if ns != "" && ms.scope != kube.AllNamespaces && ns != ms.scope {
			ms.path, ms.filter, ms.cursor, ms.offset, keep = nil, "", 0, 0, ""
			m.status = fmt.Sprintf("namespace %s is no longer readable: the metrics screen now covers namespace %s only", textutil.Sanitize(ns), textutil.Sanitize(ms.scope))
			break
		}
	}
	m.rebuildMetrics(keep)
}

// adoptMetrics makes the screen show the readings the copilot was just
// given. The tool may have had to read the cluster (when what the screen had
// was too old to hand out); this takes those readings from the shared cache
// — no further read — so that what the model quotes is what is on show.
func (m *Model) adoptMetrics() tea.Cmd {
	if m.view != vMetrics || m.metrics.loading {
		return nil // not on show, or a read is out and will bring the same
	}
	ms := &m.metrics
	ms.seq++
	seq, home := ms.seq, m.curNS
	c := m.deps.Cluster
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, kube.MetricsTimeout+5*time.Second)
		defer cancel()
		msg := metricsMsg{seq: seq, snap: c.Metrics(ctx, kube.AllNamespaces, kube.MetricsFreshFor)}
		if st, denied := msg.snap.PodsDeniedClusterWide(); denied && home != kube.AllNamespaces {
			if narrow := c.Metrics(ctx, home, kube.MetricsFreshFor); narrow.Sources.PodMetrics.OK() {
				msg.snap, msg.narrowed = narrow, &st
			}
		}
		return msg
	}
}

// metricsQuery is where the user has drilled to, as a query.
func (ms *metricsState) query() metrics.Query {
	q := metrics.Query{Level: ms.tab, Sort: ms.sort, Filter: ms.filter}
	for _, c := range ms.path {
		switch c.kind {
		case crumbNode:
			q.Level, q.Node = metrics.LevelPods, c.name
		case crumbNamespace:
			q.Level, q.Namespace = metrics.LevelPods, c.name
		case crumbPod:
			q.Level, q.Namespace, q.Pod, q.Node = metrics.LevelContainers, c.namespace, c.name, ""
		}
	}
	return q
}

// rebuildMetrics recomputes the listing on screen and puts the cursor back
// on the row called keep, wherever a refresh or a re-sort moved it.
func (m *Model) rebuildMetrics(keep string) {
	ms := &m.metrics
	ms.list, ms.widths = nil, nil
	if ms.snap == nil {
		return
	}
	l := ms.snap.List(ms.query())
	ms.widths = make([]int, len(l.Columns))
	for i, c := range l.Columns {
		ms.widths[i] = lipWidth(c.Name)
		if c.Bar {
			ms.widths[i] = barWidth
		}
	}
	for r := range l.Rows {
		for i := range l.Rows[r].Cells {
			// Names come from the cluster: nothing in them gets to drive the
			// terminal.
			l.Rows[r].Cells[i] = textutil.OneLine(textutil.Sanitize(l.Rows[r].Cells[i]))
			if i < len(ms.widths) && !l.Columns[i].Bar {
				ms.widths[i] = max(ms.widths[i], lipWidth(l.Rows[r].Cells[i]))
			}
		}
	}
	ms.list = l
	ms.cursor = clamp(ms.cursor, 0, len(l.Rows)-1)
	if keep != "" {
		for i := range l.Rows {
			if l.Rows[i].Key == keep {
				ms.cursor = i
				break
			}
		}
	}
}

func (ms *metricsState) selectedRow() (metrics.Row, bool) {
	if ms.list == nil || ms.cursor < 0 || ms.cursor >= len(ms.list.Rows) {
		return metrics.Row{}, false
	}
	return ms.list.Rows[ms.cursor], true
}

func (ms *metricsState) selectedKey() string {
	r, _ := ms.selectedRow()
	return r.Key
}

func (ms *metricsState) move(delta int) {
	n := 0
	if ms.list != nil {
		n = len(ms.list.Rows)
	}
	ms.cursor = clamp(ms.cursor+delta, 0, n-1)
}

// --- keys -----------------------------------------------------------------------

func (m *Model) onMetricsKey(msg tea.KeyMsg) tea.Cmd {
	ms := &m.metrics
	page := max(ms.page, 1)
	switch key := msg.String(); key {
	case "up", "k":
		ms.move(-1)
	case "down", "j":
		ms.move(1)
	case "pgup", "ctrl+b":
		ms.move(-page)
	case "pgdown", "ctrl+f", " ":
		ms.move(page)
	case "home", "g":
		ms.cursor = 0
	case "end", "G":
		ms.move(1 << 30)
	case "1", "2", "3":
		m.metricsTab(metrics.Levels[int(key[0]-'1')])
	case "left", "right":
		if len(ms.path) > 0 {
			m.status = "esc goes back up a level; 1 2 3 jump to nodes, namespaces, pods"
			return nil
		}
		step := 1
		if key == "left" {
			step = 2
		}
		for i, lv := range metrics.Levels[:3] {
			if lv == ms.tab {
				m.metricsTab(metrics.Levels[(i+step)%3])
				break
			}
		}
	case "enter":
		m.metricsDrill()
	case "s":
		ms.sort = ms.sort.Next()
		m.rebuildMetrics(ms.selectedKey())
	case "/":
		m.filtering = true
		m.filterIn.SetValue(ms.filter)
		m.filterIn.CursorEnd()
		return m.filterIn.Focus()
	case "r":
		m.status = "reading metrics…"
		return m.loadMetrics(true)
	case "d":
		return m.openFromMetrics(vDetail)
	case "l":
		return m.openFromMetrics(vLogs)
	case "e":
		return m.openFromMetrics(vEvents)
	}
	return nil
}

// metricsTab jumps to one of the top-level listings.
func (m *Model) metricsTab(level metrics.Level) {
	ms := &m.metrics
	ms.tab, ms.path, ms.filter, ms.cursor, ms.offset = level, nil, "", 0, 0
	m.rebuildMetrics("")
}

// metricsDrill steps into the selected row: a node's or a namespace's pods,
// a pod's containers.
func (m *Model) metricsDrill() {
	ms := &m.metrics
	row, ok := ms.selectedRow()
	if !ok {
		m.status = "nothing is selected"
		return
	}
	c := crumb{name: row.Name, namespace: row.Namespace, back: row.Key}
	switch ms.list.Query.Level {
	case metrics.LevelNodes:
		c.kind = crumbNode
	case metrics.LevelNamespaces:
		c.kind = crumbNamespace
	case metrics.LevelPods:
		c.kind = crumbPod
	default:
		m.status = "a container is as far down as it goes — l shows its logs"
		return
	}
	ms.path = append(ms.path, c)
	ms.filter, ms.cursor, ms.offset = "", 0, 0
	m.rebuildMetrics("")
}

// metricsBack undoes one step — clear the filter, or go up a level — and
// reports whether there was one to undo. At the top it reports false and the
// caller leaves the screen.
func (m *Model) metricsBack(key string) bool {
	ms := &m.metrics
	if key == "esc" && ms.filter != "" {
		ms.filter = ""
		m.rebuildMetrics(ms.selectedKey())
		return true
	}
	if n := len(ms.path); n > 0 {
		back := ms.path[n-1].back
		ms.path, ms.filter, ms.offset = ms.path[:n-1], "", 0
		m.rebuildMetrics(back)
		return true
	}
	return false
}

// openFromMetrics opens the browser's detail, logs or events view for the
// selected row, so a hot pod is one key away from its logs.
func (m *Model) openFromMetrics(v viewID) tea.Cmd {
	ms := &m.metrics
	row, ok := ms.selectedRow()
	if !ok {
		m.status = "nothing is selected"
		return nil
	}
	typeName, ns, name, container := "pods", row.Namespace, row.Name, ""
	switch q := ms.list.Query; q.Level {
	case metrics.LevelNodes:
		typeName, ns = "nodes", ""
	case metrics.LevelNamespaces:
		typeName, ns = "namespaces", ""
	case metrics.LevelContainers:
		ns, name, container = q.Namespace, q.Pod, row.Name
	}
	if v == vLogs && typeName != "pods" {
		m.status = "logs belong to pods: press enter to see the pods of this " + strings.TrimSuffix(typeName, "s")
		return nil
	}
	t, err := kube.ResolveIn(m.types, typeName)
	if err != nil {
		m.status = "cannot open " + typeName + ": the cluster's resource types are not known"
		return nil
	}
	m.subj, m.subjErr = subject{typ: t, namespace: ns, name: name}, nil
	cmd := m.openSubject(v)
	if v == vLogs {
		m.logs.want = container
	}
	return cmd
}

// metricsFocus tells the copilot what the metrics screen is showing, so "why
// is this one so busy?" needs no further explanation.
func (m *Model) metricsFocus(f *agent.Focus) {
	ms := &m.metrics
	f.Namespace, f.AllNamespaces = ms.scope, ms.scope == kube.AllNamespaces
	f.Type, f.Kind, f.Namespaced, f.Selected, f.SelectedNamespace = "", "", false, "", ""
	q := ms.query()
	f.View = "metrics screen, listing " + string(q.Level)
	if ms.list != nil {
		f.View = "metrics screen, listing " + ms.list.What + " by " + string(q.Sort)
	}
	row, ok := ms.selectedRow()
	switch q.Level {
	case metrics.LevelNodes:
		f.Type, f.Kind = "nodes", "Node"
		f.Namespace, f.AllNamespaces = "(not applicable: cluster-scoped type)", false
	case metrics.LevelNamespaces:
		f.Type, f.Kind = "namespaces", "Namespace"
		f.Namespace, f.AllNamespaces = "(not applicable: cluster-scoped type)", false
	case metrics.LevelPods:
		f.Type, f.Kind, f.Namespaced = "pods", "Pod", true
		if q.Namespace != "" {
			f.Namespace, f.AllNamespaces = q.Namespace, false
		}
	case metrics.LevelContainers:
		f.Type, f.Kind, f.Namespaced = "pods", "Pod", true
		f.Namespace, f.AllNamespaces = q.Namespace, false
		f.Selected, f.SelectedNamespace = q.Pod, q.Namespace
		if ok {
			f.View += ", container " + row.Name + " highlighted"
		}
		return
	}
	if ok {
		f.Selected, f.SelectedNamespace = row.Name, row.Namespace
	}
}

// --- drawing --------------------------------------------------------------------

func heatStyle(h metrics.Heat) lipgloss.Style {
	switch h {
	case metrics.High:
		return stBad
	case metrics.Elevated:
		return stWarn
	}
	return lipgloss.NewStyle()
}

// metricsRowStyle is how a row is drawn. The row under the cursor keeps its
// heat colour: the cursor starts on the first row, which by default is the
// busiest one — the very row whose colour matters most.
func metricsRowStyle(h metrics.Heat, selected bool) lipgloss.Style {
	if !selected {
		return heatStyle(h)
	}
	switch h {
	case metrics.High:
		return stSelected.Foreground(colBad)
	case metrics.Elevated:
		return stSelected.Foreground(colWarn)
	}
	return stSelected
}

// bar draws a percentage as a bar of width cells. Anything above zero shows
// at least one cell; anything above 100% is simply full.
func bar(pct, width int) string {
	filled := clamp((pct*width+50)/100, 0, width)
	if pct > 0 && filled == 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func (m *Model) viewMetrics(w, h int) string {
	ms := &m.metrics
	now := time.Now()
	lines := []string{m.metricsTitle(w, now)}
	if ms.snap == nil || ms.list == nil {
		// The first read is still out. Say so; do not draw an empty table.
		return fitBox(lines[0]+"\n\n"+stDim.Render(wrap("Reading metrics from the cluster…", w)), w, h)
	}
	roomy := h >= 16
	lines = append(lines, m.metricsSummary(w, roomy)...)
	lines = append(lines, m.metricsTabs(w))
	rest := max(h-len(lines), 1)
	return fitBox(strings.Join(lines, "\n")+"\n"+m.metricsBody(w, rest), w, h)
}

// metricsTitle is the first line: where in the hierarchy this is, and how
// old the readings are.
func (m *Model) metricsTitle(w int, now time.Time) string {
	ms := &m.metrics
	where := "cluster"
	for _, c := range ms.path {
		where += " › " + textutil.OneLine(textutil.Sanitize(c.String()))
	}
	if ms.snap == nil {
		return clip(stHeader.Render("metrics")+stDim.Render(" · "+where), w)
	}
	fresh := ms.snap.Freshness(now)
	age := fresh.Text
	if m.metricsInterval() == 0 {
		age += " · auto-refresh off"
	}
	ageStyle := stDim
	if fresh.Warn {
		ageStyle = stWarn
	}
	left := stHeader.Render("metrics") + stDim.Render(" · ") + where
	for _, age := range []string{age, fresh.Short} {
		if gap := w - lipWidth(left) - lipWidth(age); gap >= 2 {
			return left + strings.Repeat(" ", gap) + ageStyle.Render(age)
		}
	}
	// No room for both: how old the numbers are matters more than the
	// breadcrumb, which the line below the summary repeats.
	return clip(stHeader.Render("metrics")+stDim.Render(" · ")+ageStyle.Render(fresh.Short), w)
}

// metricsSummary is what the rows add up to: the gauges of the cluster, node,
// namespace or pod being looked at.
func (m *Model) metricsSummary(w int, roomy bool) []string {
	sum := m.metrics.list.Summary
	subject := textutil.OneLine(textutil.Sanitize(sum.Subject))
	var lines []string
	if len(sum.Gauges) == 0 && m.metrics.list.Unavailable != nil {
		// The body is about to say why there are no readings; a summary line
		// saying "unknown" for the same reason is the same thing twice.
		return nil
	}
	if len(sum.Gauges) == 0 {
		text := subject
		if sum.Missing != "" {
			text += ": " + textutil.OneLine(textutil.Sanitize(sum.Missing))
		}
		lines = append(lines, stWarn.Render(clip(text, w)))
	} else {
		// The same gauges at decreasing cost in cells. The first rendering
		// that fits is used: in full with bars; stacked, if there are lines
		// to spare; abbreviated; and at the narrowest just the shares.
		gauge := func(g metrics.Gauge, text string, withBar bool) string {
			text = textutil.OneLine(textutil.Sanitize(text))
			if pct, _ := g.Percent(); withBar && g.Bounded() {
				return heatStyle(g.Heat()).Render(bar(pct, barWidth)) + " " + text
			}
			return heatStyle(g.Heat()).Render(text)
		}
		join := func(text func(metrics.Gauge) string, withBar, withSubject bool) string {
			parts := make([]string, len(sum.Gauges))
			for i, g := range sum.Gauges {
				parts[i] = gauge(g, text(g), withBar)
			}
			out := strings.Join(parts, "   ")
			if withSubject {
				out = stBold.Render(subject) + "  " + out
			}
			return out
		}
		full, short, tiny := metrics.Gauge.Text, metrics.Gauge.Short, metrics.Gauge.Tiny
		stacked := roomy
		for _, g := range sum.Gauges {
			stacked = stacked && lipWidth(gauge(g, full(g), true))+2 <= w
		}
		switch {
		case lipWidth(join(full, true, true)) <= w:
			lines = append(lines, join(full, true, true))
		case stacked:
			lines = append(lines, clip(stBold.Render(subject), w))
			for _, g := range sum.Gauges {
				lines = append(lines, "  "+gauge(g, full(g), true))
			}
		case lipWidth(join(short, true, true)) <= w:
			lines = append(lines, join(short, true, true))
		case lipWidth(join(short, false, true)) <= w:
			lines = append(lines, join(short, false, true))
		case lipWidth(join(short, false, false)) <= w:
			lines = append(lines, join(short, false, false))
		default:
			lines = append(lines, clip(join(tiny, false, false), w))
		}
	}
	if roomy && len(sum.Facts) > 0 {
		lines = append(lines, stDim.Render(clip(textutil.OneLine(textutil.Sanitize(strings.Join(sum.Facts, " · "))), w)))
	}
	return lines
}

// metricsTabs is the line above the table: the three top-level listings, or
// what the drilled-into listing is, with the sort order and filter.
func (m *Model) metricsTabs(w int) string {
	ms := &m.metrics
	l, snap := ms.list, ms.snap
	var s string
	if len(ms.path) == 0 {
		count := func(lv metrics.Level) string {
			switch {
			case lv == metrics.LevelNodes && (snap.Sources.NodeMetrics.OK() || snap.Sources.Nodes.OK()):
				return " (" + strconv.Itoa(len(snap.Nodes)) + ")"
			case lv == metrics.LevelNamespaces && snap.Sources.PodMetrics.OK():
				return " (" + strconv.Itoa(len(snap.Namespaces)) + ")"
			case lv == metrics.LevelPods && snap.Sources.PodMetrics.OK():
				return " (" + strconv.Itoa(len(snap.Pods)) + ")"
			}
			return ""
		}
		for i, lv := range metrics.Levels[:3] {
			label := fmt.Sprintf(" %d %s%s ", i+1, lv, count(lv))
			if lv == ms.tab {
				s += stSelected.Render(label)
			} else {
				s += stDim.Render(label)
			}
		}
	} else {
		s = stBold.Render(textutil.OneLine(textutil.Sanitize(l.What)))
		if l.Unavailable == nil && l.Empty == "" {
			s += stDim.Render(fmt.Sprintf(" · %d", l.Total))
		}
	}
	if l.Unavailable == nil && l.Empty == "" {
		order := "by " + string(l.Query.Sort)
		if l.Query.Sort != metrics.ByName {
			order += " ↓"
		}
		s += stDim.Render("  " + order)
	}
	// A filter goes first. It changes what every line below means, so in a
	// pane too narrow for the whole line it is the tabs that are cut, not
	// the fact that only some rows are showing.
	count := stDim.Render(fmt.Sprintf(" %d of %d", len(l.Rows), l.Total))
	switch {
	case m.filtering:
		s = m.filterIn.View() + count + "  " + s
	case ms.filter != "":
		s = stWarn.Render("/"+textutil.Sanitize(ms.filter)) + count + "  " + s
	}
	return clip(s, w)
}

// metricsBody is the table — or, when there is no table to show, the reason
// in plain words.
func (m *Model) metricsBody(w, rest int) string {
	ms := &m.metrics
	l := ms.list
	ms.page = 1
	// A state in place of the table still carries the screen's caveats —
	// above all that it has been limited to one namespace.
	state := func(text string) string {
		lines := strings.Split(text, "\n")
		notes := m.metricsNotes(w, max(rest-len(lines)-1, 0))
		if len(notes) > 0 {
			lines = append(append(lines, ""), notes...)
		}
		return fitLines(strings.Join(lines, "\n"), rest)
	}
	switch {
	case l.Unavailable != nil:
		return state(m.metricsUnavailable(*l.Unavailable, w))
	case l.Empty != "":
		return state("\n" + stDim.Render(wrap(textutil.Sanitize(l.Empty), w)))
	case len(l.Rows) == 0:
		return state("\n" + stDim.Render(wrap(fmt.Sprintf("Nothing matches the filter %q. Press esc to clear it.", textutil.Sanitize(ms.filter)), w)))
	}

	// Caveats get a quarter of the pane, and whatever else the rows do not
	// need: with few rows there is no reason to cut a caveat short.
	notes := m.metricsNotes(w, max(clamp(rest/4, 1, 4), rest-1-len(l.Rows)))
	rows := max(rest-1-len(notes), 1)
	ms.page = rows

	keep, widths := fitColumns(l.Columns, ms.widths, w)
	line := func(cell func(i int) string) string {
		var sb strings.Builder
		for j, i := range keep {
			text := cell(i)
			if l.Columns[i].Right {
				text = strings.Repeat(" ", max(widths[i]-lipWidth(text), 0)) + clip(text, widths[i])
			} else {
				text = fit(text, widths[i])
			}
			sb.WriteString(text)
			if j < len(keep)-1 {
				sb.WriteString("  ")
			}
		}
		return sb.String()
	}

	out := []string{stColHead.Render(clip(line(func(i int) string { return l.Columns[i].Name }), w))}

	if ms.cursor < ms.offset {
		ms.offset = ms.cursor
	}
	if ms.cursor >= ms.offset+rows {
		ms.offset = ms.cursor - rows + 1
	}
	ms.offset = clamp(ms.offset, 0, max(len(l.Rows)-rows, 0))
	for i := ms.offset; i < len(l.Rows) && i < ms.offset+rows; i++ {
		row := l.Rows[i]
		text := line(func(c int) string {
			if c >= len(row.Cells) {
				return ""
			}
			if l.Columns[c].Bar {
				if pct, err := strconv.Atoi(row.Cells[c]); err == nil {
					return bar(pct, barWidth)
				}
				return "" // no reading: no bar, rather than an empty one
			}
			return row.Cells[c]
		})
		if i == ms.cursor {
			out = append(out, metricsRowStyle(row.Heat, true).Render(fit(text, w)))
		} else {
			out = append(out, metricsRowStyle(row.Heat, false).Render(clip(text, w)))
		}
	}
	if len(notes) > 0 {
		// Notes sit at the bottom of the pane, below however many rows there
		// are.
		for len(out) < rows+1 {
			out = append(out, "")
		}
		out = append(out, notes...)
	}
	return strings.Join(out, "\n")
}

// metricsNotes are the caveats about what is on screen, wrapped and cut to
// at most limit lines.
func (m *Model) metricsNotes(w, limit int) []string {
	ms := &m.metrics
	var notes []metrics.Note
	if h := ms.snap.Held; h != nil {
		text := "Not current: " + h.Cause.Headline()
		if h.Cause.Reason != "" {
			text += " (" + h.Cause.Reason + ")"
		}
		notes = append(notes, metrics.Note{Warn: true, Text: text + ". These are the last readings that were obtained."})
	}
	if st := ms.narrowed; st != nil {
		notes = append(notes, metrics.Note{Warn: true, Text: fmt.Sprintf(
			"%s: showing namespace %s only — the browser's namespace.", st.Headline(), ms.scope)})
	}
	notes = append(notes, ms.list.Notes...)

	var out []string
	for _, n := range notes {
		style := stDim
		if n.Warn {
			style = stWarn
		}
		for _, ln := range strings.Split(wrap(textutil.OneLine(textutil.Sanitize(n.Text)), w), "\n") {
			out = append(out, style.Render(ln))
		}
	}
	if limit <= 0 {
		return nil
	}
	if len(out) > limit {
		out = out[:limit]
		out[limit-1] = clip(out[limit-1], max(w-2, 1)) + stDim.Render(" …")
	}
	return out
}

// metricsUnavailable is the screen when the readings a listing is made of
// could not be obtained. It is deliberately not a table: an operator must
// not be able to mistake "nothing is known" for "nothing is busy".
func (m *Model) metricsUnavailable(st metrics.SourceStatus, w int) string {
	ms := &m.metrics
	var b strings.Builder
	// The wording names the namespace, and that came from outside.
	b.WriteString("\n" + stBad.Render(wrap(textutil.Sanitize(st.Headline()), w)) + "\n\n")
	b.WriteString(wrap(textutil.Sanitize(st.Explain()), w) + "\n")
	if st.Reason != "" {
		b.WriteString("\n" + stDim.Render(wrap("The server said: "+textutil.OneLine(textutil.Sanitize(st.Reason)), w)) + "\n")
	}

	var next []string
	src := ms.snap.Sources
	switch {
	case st.Source == metrics.PodMetrics && src.NodeMetrics.OK():
		next = append(next, "Node metrics are readable: press 1.")
	case st.Source == metrics.NodeMetrics && src.PodMetrics.OK():
		next = append(next, "Pod metrics are readable: press 2 or 3.")
	}
	if _, denied := ms.snap.PodsDeniedClusterWide(); denied && st.Source == metrics.PodMetrics {
		if m.curNS == kube.AllNamespaces {
			next = append(next, "A single namespace may still be readable: choose one in the browser (esc, then n) and open metrics again.")
		} else {
			next = append(next, fmt.Sprintf("Namespace %s could not be read either.", textutil.Sanitize(m.curNS)))
		}
	}
	if iv := m.metricsInterval(); iv > 0 {
		next = append(next, fmt.Sprintf("Trying again every %s; r tries now.", metrics.Age(iv)))
	} else {
		next = append(next, "r tries again.")
	}
	b.WriteString("\n" + stDim.Render(wrap(strings.Join(next, " "), w)))
	return b.String()
}

// fitBox makes text fit a pane of w by h cells whatever it contains. The
// parts above already fit themselves to the pane; this is the guarantee that
// does not depend on each of them getting it right, so that the frame never
// has to wrap or cut what the screen drew.
func fitBox(s string, w, h int) string {
	lines := strings.Split(fitLines(s, h), "\n")
	for i, l := range lines {
		if lipWidth(l) > w {
			lines[i] = clip(l, w)
		}
	}
	return strings.Join(lines, "\n")
}

// fitLines cuts text to at most n lines (and never to less than one).
func fitLines(s string, n int) string {
	n = max(n, 1)
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// fitColumns chooses the columns that fit in w cells and how wide each is.
// It gives up, in order: the name column's excess, the columns marked as
// droppable (most droppable first), and finally most of the name itself.
// Whatever is still too wide is clipped by the caller; nothing wraps.
func fitColumns(cols []metrics.Column, natural []int, w int) (keep []int, widths []int) {
	widths = append([]int(nil), natural...)
	for i := range cols {
		keep = append(keep, i)
	}
	// The name column: the last left-aligned column that is never dropped
	// (NODE, NAMESPACE, POD, CONTAINER).
	name := 0
	for i, c := range cols {
		if !c.Right && !c.Bar && c.Drop == 0 {
			name = i
		}
	}
	need := func() int {
		total := 2 * (len(keep) - 1)
		for _, i := range keep {
			total += widths[i]
		}
		return total
	}
	shrink := func(floor int) {
		if over := need() - w; over > 0 && len(widths) > name {
			widths[name] = max(widths[name]-over, min(widths[name], floor))
		}
	}
	shrink(24)
	for need() > w {
		drop := -1
		for j, i := range keep {
			if cols[i].Drop > 0 && (drop < 0 || cols[i].Drop >= cols[keep[drop]].Drop) {
				drop = j
			}
		}
		if drop < 0 {
			break
		}
		keep = append(keep[:drop], keep[drop+1:]...)
	}
	shrink(8)
	return keep, widths
}

// metricsHints are the footer's key hints for the metrics screen.
func (m *Model) metricsHints() string {
	ms := &m.metrics
	level := ms.query().Level
	var pairs []string
	// Only what does something here: with no rows there is nothing to move
	// through, sort or open.
	rows := ms.list != nil && ms.list.Unavailable == nil && ms.list.Empty == ""
	if rows {
		pairs = append(pairs, "↑↓", "move")
		if level != metrics.LevelContainers {
			pairs = append(pairs, "enter", "drill down")
		}
	}
	if len(ms.path) == 0 {
		pairs = append(pairs, "1 2 3", "nodes/namespaces/pods")
	}
	if rows {
		pairs = append(pairs, "s", "sort", "/", "filter")
		if level == metrics.LevelPods || level == metrics.LevelContainers {
			pairs = append(pairs, "l", "logs")
		}
		pairs = append(pairs, "e", "events", "d", "detail", "r", "refresh")
	} else {
		pairs = append(pairs, "r", "try again")
	}
	pairs = append(pairs, "esc", "back", "tab", "ask", "?", "help", "q", "quit")
	return hints(pairs...)
}
