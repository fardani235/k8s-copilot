package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// Level is one rung of the hierarchy: the cluster's nodes, its namespaces,
// pods (all of them, or those of one node or namespace), one pod's
// containers.
type Level string

const (
	LevelNodes      Level = "nodes"
	LevelNamespaces Level = "namespaces"
	LevelPods       Level = "pods"
	LevelContainers Level = "containers"
)

// Levels lists the levels from the big picture down.
var Levels = []Level{LevelNodes, LevelNamespaces, LevelPods, LevelContainers}

// Sort orders a listing.
type Sort string

const (
	ByCPU    Sort = "cpu"
	ByMemory Sort = "memory"
	ByName   Sort = "name"
)

// Next cycles cpu → memory → name.
func (s Sort) Next() Sort {
	switch s {
	case ByCPU:
		return ByMemory
	case ByMemory:
		return ByName
	}
	return ByCPU
}

// Query selects one listing out of a Snapshot. The screen builds one from
// where the user has drilled to; the copilot's tool builds one from its
// arguments. Both get the same Listing for the same Query.
type Query struct {
	Level Level
	// Namespace narrows pods to one namespace, and names the pod's namespace
	// for containers.
	Namespace string
	// Node narrows pods to those running on one node.
	Node string
	// Pod is the pod whose containers are listed.
	Pod  string
	Sort Sort
	// Filter keeps rows whose name contains it (case-insensitive).
	Filter string
}

// Column is one column of a Listing.
type Column struct {
	Name  string
	Right bool
	// Drop orders what is left out when there is no room: 0 is never
	// dropped, and the highest number goes first.
	Drop int
	// Bar marks a column whose cells hold a bare percentage (or nothing), to
	// be drawn as a bar. Plain-text renderings leave it out: the percentage
	// is already in the column before it.
	Bar bool
}

// Row is one line of a Listing. Cells line up with Listing.Columns.
type Row struct {
	// Key identifies the row across refreshes: a node name, "namespace/pod",
	// a namespace, a container name.
	Key       string
	Namespace string
	Name      string
	Cells     []string
	// Heat is the highest heat among the row's readings against a hard
	// bound (allocatable, limit); Flags say which.
	Heat  Heat
	Flags []string
}

// Gauge is one reading set against what bounds it.
type Gauge struct {
	Label string
	Used  Amount
	// Of is the bound; unknown when there is none.
	Of     Amount
	OfWhat string // "allocatable", "limit", "requested"
	// Aside is appended as is ("no limit", "100m requested").
	Aside string
	cpu   bool
}

func (g Gauge) format(a Amount) string {
	if g.cpu {
		return FormatCPU(a)
	}
	return FormatMem(a)
}

// Percent is the gauge's reading as a percentage of its bound.
func (g Gauge) Percent() (int, bool) { return Percent(g.Used, g.Of) }

// Heat is the gauge's heat. A reading above its *request* is normal, so only
// hard bounds count.
func (g Gauge) Heat() Heat {
	if pct, ok := g.Percent(); ok && g.OfWhat != "requested" {
		return HeatOf(pct)
	}
	return Calm
}

// Bounded reports whether the gauge is set against a hard bound it can be
// drawn as a share of. A request is not one: usage above it is normal.
func (g Gauge) Bounded() bool {
	_, ok := g.Percent()
	return ok && g.OfWhat != "requested"
}

// Short is Text for a narrow pane: "CPU 3.2/8 alloc (40%)".
func (g Gauge) Short() string {
	out := g.Label + " " + g.format(g.Used)
	if pct, ok := g.Percent(); ok {
		what := map[string]string{"allocatable": "alloc", "limit": "lim", "requested": "req"}[g.OfWhat]
		return out + fmt.Sprintf("/%s %s (%d%%)", g.format(g.Of), what, pct)
	}
	return out
}

// Tiny is the least that still says something: the share of a hard bound
// if there is one, else the reading itself.
func (g Gauge) Tiny() string {
	if pct, _ := g.Percent(); g.Bounded() {
		return fmt.Sprintf("%s %d%%", g.Label, pct)
	}
	return g.Label + " " + g.format(g.Used)
}

// Text writes the gauge in words: "CPU 3.2 of 8 allocatable (40%)".
func (g Gauge) Text() string {
	out := g.Label + " " + g.format(g.Used)
	if g.Of.OK {
		out += " of " + g.format(g.Of) + " " + g.OfWhat
		if pct, ok := g.Percent(); ok {
			out += fmt.Sprintf(" (%d%%)", pct)
		}
	}
	if g.Aside != "" {
		out += ", " + g.Aside
	}
	return out
}

// Summary is the context above a listing: what the rows add up to.
type Summary struct {
	// Subject is what the gauges describe: "cluster", "node node-a",
	// "namespace shop", "pod shop/web-1".
	Subject string
	Gauges  []Gauge
	Facts   []string
	// Missing says why there are no gauges, when there are none.
	Missing string
}

// Lines writes the summary as text, one or two lines.
func (s Summary) Lines() []string {
	head := s.Subject + ": "
	var parts []string
	for _, g := range s.Gauges {
		parts = append(parts, g.Text())
	}
	switch {
	case len(parts) > 0:
		head += strings.Join(parts, " · ")
	case s.Missing != "":
		head += s.Missing
	default:
		head = s.Subject
	}
	lines := []string{clean(head)}
	if len(s.Facts) > 0 {
		lines = append(lines, clean(strings.Join(s.Facts, " · ")))
	}
	return lines
}

// Note is a caveat about a listing.
type Note struct {
	Text string
	Warn bool
}

// Listing is one table of readings, ready to draw. It carries no styling and
// no layout: the screen adds colour and fits it to the pane, the copilot's
// tool prints it as text.
type Listing struct {
	Query Query
	// What names the rows: "nodes", "pods on node node-a",
	// "containers of pod shop/web-1".
	What    string
	Summary Summary

	// Unavailable is set when the readings this listing is made of could not
	// be obtained. There are then no rows, and this says why. It is the
	// difference between "nothing is busy" and "nothing is known".
	Unavailable *SourceStatus
	// Empty is set when the source answered and there is simply nothing to
	// list.
	Empty string

	Columns []Column
	Rows    []Row
	// Total is the number of rows before Query.Filter was applied.
	Total int
	Notes []Note
}

// List builds the listing a query asks for.
func (s *Snapshot) List(q Query) *Listing {
	if q.Sort == "" {
		q.Sort = ByCPU
	}
	l := &Listing{Query: q}
	switch q.Level {
	case LevelNodes:
		s.listNodes(l)
	case LevelNamespaces:
		s.listNamespaces(l)
	case LevelContainers:
		s.listContainers(l)
	default:
		l.Query.Level = LevelPods
		s.listPods(l)
	}
	for _, src := range s.Truncated {
		l.Notes = append(l.Notes, Note{Warn: true, Text: fmt.Sprintf(
			"The cluster has more %s than were read; totals and rankings cover only the part that was.", src)})
	}
	l.filter()
	return l
}

func (l *Listing) filter() {
	l.Total = len(l.Rows)
	want := strings.ToLower(strings.TrimSpace(l.Query.Filter))
	if want == "" {
		return
	}
	kept := l.Rows[:0]
	for _, r := range l.Rows {
		if strings.Contains(strings.ToLower(r.Key), want) {
			kept = append(kept, r)
		}
	}
	l.Rows = kept
}

// rank orders two readings for a listing: the chosen reading descending,
// unknown last, then by name so the order is stable between refreshes.
func rank(by Sort, cpuA, memA Amount, nameA string, cpuB, memB Amount, nameB string) bool {
	a, b := cpuA, cpuB
	if by == ByMemory {
		a, b = memA, memB
	}
	if by != ByName {
		switch {
		case a.OK != b.OK:
			return a.OK
		case a.OK && a.V != b.V:
			return a.V > b.V
		}
	}
	return nameA < nameB
}

// pct writes used/of as a percentage cell. none is what to write when the
// bound is known not to exist ("no lim"), as opposed to being unknown.
func pct(used, of Amount, boundKnowable bool, none string) (cell string, value int, ok bool) {
	switch {
	case !used.OK:
		return NoData, 0, false
	case !of.OK && boundKnowable:
		return none, 0, false
	}
	p, ok := Percent(used, of)
	if !ok {
		return NoData, 0, false
	}
	return strconv.Itoa(p) + "%", p, true
}

func barCell(value int, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.Itoa(value)
}

// flag records a reading against a hard bound on the row.
func (r *Row) flag(what string, value int, ok bool, of string) {
	if !ok {
		return
	}
	h := HeatOf(value)
	if h == Calm {
		return
	}
	if h > r.Heat {
		r.Heat = h
	}
	r.Flags = append(r.Flags, fmt.Sprintf("%s %d%% of %s", what, value, of))
}

func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// --- nodes --------------------------------------------------------------------

func (s *Snapshot) listNodes(l *Listing) {
	l.What = "nodes"
	l.Summary = s.clusterSummary()
	if st := s.Sources.NodeMetrics; !st.OK() {
		l.Unavailable = &st
		return
	}
	if len(s.Nodes) == 0 {
		l.Empty = "The metrics API answered with no nodes."
		return
	}
	showPods := s.Scope == "" && s.Sources.Pods.OK()
	l.Columns = []Column{
		{Name: "NODE"},
		{Name: "CPU", Right: true, Drop: 2}, {Name: "%CPU", Right: true}, {Bar: true, Drop: 4},
		{Name: "MEM", Right: true, Drop: 2}, {Name: "%MEM", Right: true}, {Bar: true, Drop: 4},
	}
	if showPods {
		l.Columns = append(l.Columns, Column{Name: "PODS", Right: true, Drop: 3})
	}

	nodes := append([]Node(nil), s.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		return rank(l.Query.Sort, nodes[i].CPU, nodes[i].Mem, nodes[i].Name, nodes[j].CPU, nodes[j].Mem, nodes[j].Name)
	})
	silent := 0
	for _, n := range nodes {
		r := Row{Key: n.Name, Name: n.Name}
		name := n.Name
		if n.Ready == "NotReady" {
			name += " (NotReady)"
		}
		capKnown := false // an unknown allocatable is never "none"
		cpuCell, cpuPct, cpuOK := pct(n.CPU, n.CPUAlloc, capKnown, "")
		memCell, memPct, memOK := pct(n.Mem, n.MemAlloc, capKnown, "")
		r.flag("CPU", cpuPct, cpuOK, "allocatable")
		r.flag("memory", memPct, memOK, "allocatable")
		r.Cells = []string{name,
			FormatCPU(n.CPU), cpuCell, barCell(cpuPct, cpuOK),
			FormatMem(n.Mem), memCell, barCell(memPct, memOK)}
		if showPods {
			r.Cells = append(r.Cells, strconv.FormatInt(n.Pods.V, 10))
		}
		if !n.CPU.OK && !n.Mem.OK {
			silent++
		}
		l.Rows = append(l.Rows, r)
	}

	if st := s.Sources.Nodes; !st.OK() {
		l.Notes = append(l.Notes, Note{Warn: true, Text: fmt.Sprintf(
			"Node capacity is unknown (%s), so usage is shown without percentages.", lowerFirst(st.Headline()))})
	}
	if silent > 0 {
		l.Notes = append(l.Notes, Note{Text: fmt.Sprintf(
			"%s shown as %s: the metrics API returned no sample (a node that is NotReady or was just added reports nothing). That is unknown, not zero.",
			plural(silent, "node"), NoData)})
	}
}

func (s *Snapshot) clusterSummary() Summary {
	sum := Summary{Subject: "cluster"}
	if st := s.Sources.NodeMetrics; st.OK() {
		c := s.Cluster
		sum.Gauges = []Gauge{
			{Label: "CPU", Used: c.CPU, Of: c.CPUAlloc, OfWhat: "allocatable", cpu: true},
			{Label: "memory", Used: c.Mem, Of: c.MemAlloc, OfWhat: "allocatable"},
		}
		if c.Reporting < c.Nodes {
			sum.Facts = append(sum.Facts, fmt.Sprintf("%d of %s reporting (totals cover those)", c.Reporting, plural(c.Nodes, "node")))
		} else {
			sum.Facts = append(sum.Facts, plural(c.Nodes, "node"))
		}
	} else {
		sum.Missing = "totals unknown — " + lowerFirst(st.Headline())
	}
	if s.Sources.PodMetrics.OK() {
		all := make([]*Pod, len(s.Pods))
		for i := range s.Pods {
			all[i] = &s.Pods[i]
		}
		t := totalsOf(all)
		fact := plural(t.Pods, "pod") + " in " + ScopeText(s.Scope)
		if !s.Sources.NodeMetrics.OK() && t.Reporting > 0 {
			fact += fmt.Sprintf(" using CPU %s, memory %s", FormatCPU(t.CPU), FormatMem(t.Mem))
		}
		sum.Facts = append(sum.Facts, fact)
	}
	return sum
}

// --- namespaces ---------------------------------------------------------------

func (s *Snapshot) listNamespaces(l *Listing) {
	l.What = "namespaces"
	l.Summary = s.clusterSummary()
	if st := s.Sources.PodMetrics; !st.OK() {
		l.Unavailable = &st
		return
	}
	if len(s.Namespaces) == 0 {
		l.Empty = "No pods in " + ScopeText(s.Scope) + "."
		return
	}
	l.Columns = []Column{
		{Name: "NAMESPACE"}, {Name: "PODS", Right: true, Drop: 3},
		{Name: "CPU", Right: true}, {Name: "%CPU/R", Right: true, Drop: 2},
		{Name: "MEM", Right: true}, {Name: "%MEM/R", Right: true, Drop: 2},
	}
	nss := append([]Namespace(nil), s.Namespaces...)
	sort.SliceStable(nss, func(i, j int) bool {
		return rank(l.Query.Sort, nss[i].CPU, nss[i].Mem, nss[i].Name, nss[j].CPU, nss[j].Mem, nss[j].Name)
	})
	specs := s.Sources.Pods.OK()
	var partial []string
	for _, n := range nss {
		cpuCell, _, _ := pct(n.CPU, n.CPUReq, n.BoundsKnown, "no req")
		memCell, _, _ := pct(n.Mem, n.MemReq, n.BoundsKnown, "no req")
		l.Rows = append(l.Rows, Row{Key: n.Name, Name: n.Name, Cells: []string{
			n.Name, strconv.Itoa(n.Pods), FormatCPU(n.CPU), cpuCell, FormatMem(n.Mem), memCell}})
		if specs && n.Reporting < n.Running {
			partial = append(partial, fmt.Sprintf("%s (%d of %d)", n.Name, n.Reporting, n.Running))
		}
	}
	if len(partial) > 0 {
		shown := partial
		if len(shown) > 3 {
			shown = append(shown[:3:3], fmt.Sprintf("and %d more", len(partial)-3))
		}
		l.Notes = append(l.Notes, Note{Text: "Totals are a lower bound where running pods have not reported yet: " + strings.Join(shown, ", ") + "."})
	}
	s.specNote(l)
}

// specNote says that requests and limits are unknown when the pod objects
// could not be read.
func (s *Snapshot) specNote(l *Listing) {
	if st := s.Sources.Pods; !st.OK() {
		l.Notes = append(l.Notes, Note{Warn: true, Text: fmt.Sprintf(
			"Requests and limits are unknown (%s). %s there means unknown, not \"none set\".", lowerFirst(st.Headline()), NoData)})
	}
}

// --- pods ---------------------------------------------------------------------

func (s *Snapshot) listPods(l *Listing) {
	q := l.Query
	var pods []*Pod
	for i := range s.Pods {
		p := &s.Pods[i]
		if (q.Namespace == "" || p.Namespace == q.Namespace) && (q.Node == "" || p.Node == q.Node) {
			pods = append(pods, p)
		}
	}
	where := "in " + ScopeText(s.Scope)
	switch {
	case q.Node != "":
		l.What = "pods on node " + q.Node
		where = "on node " + q.Node
		if q.Namespace != "" {
			l.What += " in namespace " + q.Namespace
			where += " in namespace " + q.Namespace
		}
		l.Summary = s.nodeSummary(q.Node, pods)
	case q.Namespace != "":
		l.What = "pods in namespace " + q.Namespace
		where = "in namespace " + q.Namespace
		l.Summary = namespaceSummary(q.Namespace, pods)
	default:
		l.What = "pods"
		l.Summary = s.clusterSummary()
	}

	if st := s.Sources.PodMetrics; !st.OK() {
		l.Unavailable = &st
		return
	}
	if st := s.Sources.Pods; q.Node != "" && !st.OK() {
		// Without the pod objects nothing says which node a pod is on.
		l.Unavailable = &st
		return
	}
	if q.Namespace != "" && s.Scope != "" && q.Namespace != s.Scope {
		// Not "no pods": this namespace was never asked about.
		l.Empty = fmt.Sprintf("Namespace %s was not read: these readings cover %s only.", q.Namespace, ScopeText(s.Scope))
		return
	}
	if len(pods) == 0 {
		l.Empty = "No pods " + where + "."
		return
	}

	showNS := q.Namespace == "" && s.Scope == ""
	showNode := q.Node == "" && s.Sources.Pods.OK()
	if showNS {
		l.Columns = append(l.Columns, Column{Name: "NAMESPACE", Drop: 2})
	}
	l.Columns = append(l.Columns,
		Column{Name: "POD"},
		Column{Name: "CPU", Right: true}, Column{Name: "%CPU/R", Right: true, Drop: 5}, Column{Name: "%CPU/L", Right: true, Drop: 4},
		Column{Name: "MEM", Right: true}, Column{Name: "%MEM/R", Right: true, Drop: 5}, Column{Name: "%MEM/L", Right: true, Drop: 3})
	if showNode {
		l.Columns = append(l.Columns, Column{Name: "NODE", Drop: 6})
	}

	sort.SliceStable(pods, func(i, j int) bool {
		return rank(q.Sort, pods[i].CPU, pods[i].Mem, pods[i].Key(), pods[j].CPU, pods[j].Mem, pods[j].Key())
	})
	notRunning, silent, undescribed := 0, 0, 0
	for _, p := range pods {
		r := Row{Key: p.Key(), Namespace: p.Namespace, Name: p.Name}
		cpuR, _, _ := pct(p.CPU, p.CPUReq, p.BoundsKnown, "no req")
		cpuL, _, _ := pct(p.CPU, p.CPULim, p.BoundsKnown, "no lim")
		memR, _, _ := pct(p.Mem, p.MemReq, p.BoundsKnown, "no req")
		memL, _, _ := pct(p.Mem, p.MemLim, p.BoundsKnown, "no lim")
		// Limits bind containers, not pods: one container at its limit is
		// throttled or killed however much room its neighbours have. So the
		// row is as hot as its hottest container, and says which.
		for _, c := range p.Containers {
			who := ""
			if len(p.Containers) > 1 {
				who = c.Name + ": "
			}
			cpu, cpuOK := Percent(c.CPU, c.CPULim)
			mem, memOK := Percent(c.Mem, c.MemLim)
			r.flag(who+"CPU", cpu, cpuOK, "limit")
			r.flag(who+"memory", mem, memOK, "limit")
		}
		if showNS {
			r.Cells = append(r.Cells, p.Namespace)
		}
		r.Cells = append(r.Cells, p.Name, FormatCPU(p.CPU), cpuR, cpuL, FormatMem(p.Mem), memR, memL)
		if showNode {
			node := p.Node
			switch {
			case !p.SpecKnown:
				node = NoData // the pod object was not read: unknown
			case node == "":
				node = "<none>" // not scheduled: a fact, not a missing reading
			}
			r.Cells = append(r.Cells, node)
		}
		if p.SpecKnown && !p.BoundsKnown {
			undescribed++
		}
		if !p.Reporting() {
			if p.SpecKnown && p.Phase != "Running" {
				notRunning++
			} else {
				silent++
			}
		}
		l.Rows = append(l.Rows, r)
	}
	if notRunning+silent > 0 {
		var why []string
		if notRunning > 0 {
			why = append(why, fmt.Sprintf("%d not running, so there is nothing to measure", notRunning))
		}
		if silent > 0 {
			why = append(why, fmt.Sprintf("%d running but not yet reported by the metrics API", silent))
		}
		l.Notes = append(l.Notes, Note{Text: fmt.Sprintf("%s shown as %s (%s). That is unknown, not zero.",
			plural(notRunning+silent, "pod"), NoData, strings.Join(why, "; "))})
	}
	if undescribed > 0 {
		l.Notes = append(l.Notes, Note{Text: fmt.Sprintf(
			"%s running a container its spec does not describe: requests and limits are unknown there (%s), not \"none set\".",
			plural(undescribed, "pod"), NoData)})
	}
	s.specNote(l)
}

func (s *Snapshot) nodeSummary(name string, pods []*Pod) Summary {
	sum := Summary{Subject: "node " + name}
	n, found := s.Node(name)
	switch {
	case !s.Sources.NodeMetrics.OK():
		sum.Missing = "usage unknown — " + lowerFirst(s.Sources.NodeMetrics.Headline())
	case !found:
		sum.Missing = "not in the readings (the node is gone, or has no sample)"
	default:
		sum.Gauges = []Gauge{
			{Label: "CPU", Used: n.CPU, Of: n.CPUAlloc, OfWhat: "allocatable", cpu: true},
			{Label: "memory", Used: n.Mem, Of: n.MemAlloc, OfWhat: "allocatable"},
		}
		if n.Ready != "" {
			sum.Facts = append(sum.Facts, n.Ready)
		}
	}
	if s.Sources.PodMetrics.OK() && s.Sources.Pods.OK() {
		t := totalsOf(pods)
		fact := plural(t.Pods, "pod")
		if s.Scope != "" {
			fact += " in namespace " + s.Scope + " (other namespaces not read)"
		}
		if t.Reporting > 0 {
			fact += fmt.Sprintf(" using CPU %s, memory %s", FormatCPU(t.CPU), FormatMem(t.Mem))
		}
		sum.Facts = append(sum.Facts, fact)
	}
	return sum
}

func namespaceSummary(name string, pods []*Pod) Summary {
	t := totalsOf(pods)
	sum := Summary{Subject: "namespace " + name}
	sum.Gauges = []Gauge{
		{Label: "CPU", Used: t.CPU, Of: t.CPUReq, OfWhat: "requested", cpu: true},
		{Label: "memory", Used: t.Mem, Of: t.MemReq, OfWhat: "requested"},
	}
	fact := plural(t.Pods, "pod")
	if t.Reporting < t.Running {
		fact += fmt.Sprintf(", %d of %d running pods reporting (totals are a lower bound)", t.Reporting, t.Running)
	}
	sum.Facts = append(sum.Facts, fact)
	return sum
}

// --- containers ---------------------------------------------------------------

func (s *Snapshot) listContainers(l *Listing) {
	q := l.Query
	l.What = "containers of pod " + q.Namespace + "/" + q.Pod
	l.Summary = Summary{Subject: "pod " + q.Namespace + "/" + q.Pod}
	if st := s.Sources.PodMetrics; !st.OK() {
		l.Summary.Missing = "usage unknown — " + lowerFirst(st.Headline())
		l.Unavailable = &st
		return
	}
	p, found := s.Pod(q.Namespace, q.Pod)
	if !found {
		l.Empty = fmt.Sprintf("Pod %s/%s is not in the readings: it has finished or was deleted, or it is outside what was read (%s).",
			q.Namespace, q.Pod, ScopeText(s.Scope))
		return
	}
	l.Summary = podSummary(p)

	l.Columns = []Column{
		{Name: "CONTAINER"},
		{Name: "CPU", Right: true}, {Name: "CPU-REQ", Right: true, Drop: 6}, {Name: "CPU-LIM", Right: true, Drop: 5}, {Name: "%CPU/L", Right: true, Drop: 3},
		{Name: "MEM", Right: true}, {Name: "MEM-REQ", Right: true, Drop: 6}, {Name: "MEM-LIM", Right: true, Drop: 5}, {Name: "%MEM/L", Right: true, Drop: 2},
	}
	if p.SpecKnown {
		l.Columns = append(l.Columns, Column{Name: "RESTARTS", Right: true, Drop: 4}, Column{Name: "LAST-EXIT", Drop: 4})
	}
	bound := func(c Container, a Amount, format func(Amount) string) string {
		if !a.OK && c.SpecKnown {
			return "none"
		}
		return format(a)
	}

	cs := append([]Container(nil), p.Containers...)
	sort.SliceStable(cs, func(i, j int) bool {
		return rank(q.Sort, cs[i].CPU, cs[i].Mem, cs[i].Name, cs[j].CPU, cs[j].Mem, cs[j].Name)
	})
	silent := 0
	for _, c := range cs {
		r := Row{Key: c.Name, Namespace: p.Namespace, Name: c.Name}
		name := c.Name
		if c.Role != "" {
			name += " (" + c.Role + ")"
		}
		cpuL, cpuLv, cpuLok := pct(c.CPU, c.CPULim, c.SpecKnown, "no lim")
		memL, memLv, memLok := pct(c.Mem, c.MemLim, c.SpecKnown, "no lim")
		r.flag("CPU", cpuLv, cpuLok, "limit")
		r.flag("memory", memLv, memLok, "limit")
		r.Cells = []string{name,
			FormatCPU(c.CPU), bound(c, c.CPUReq, FormatCPU), bound(c, c.CPULim, FormatCPU), cpuL,
			FormatMem(c.Mem), bound(c, c.MemReq, FormatMem), bound(c, c.MemLim, FormatMem), memL}
		if p.SpecKnown {
			last := c.LastExit
			if last == "" {
				last = "-"
			}
			r.Cells = append(r.Cells, strconv.FormatInt(c.Restarts, 10), last)
			if c.LastExit == "OOMKilled" {
				// Not a reading, but the most direct evidence there is that
				// memory ran out; it belongs beside the memory column.
				if r.Heat < Elevated {
					r.Heat = Elevated
				}
				r.Flags = append(r.Flags, "previous instance was OOMKilled")
			}
		}
		if !c.CPU.OK && !c.Mem.OK {
			silent++
		}
		l.Rows = append(l.Rows, r)
	}
	if silent > 0 {
		l.Notes = append(l.Notes, Note{Text: fmt.Sprintf(
			"%s shown as %s: no sample (not running, or not yet reported by the metrics API). That is unknown, not zero.",
			plural(silent, "container"), NoData)})
	}
	if p.SpecKnown && !p.BoundsKnown {
		l.Notes = append(l.Notes, Note{Text: fmt.Sprintf(
			"A running container is not in the pod's spec: its requests and limits, and so the pod's, are unknown (%s), not \"none set\".", NoData)})
	}
	s.specNote(l)
}

func podSummary(p *Pod) Summary {
	sum := Summary{Subject: "pod " + p.Key()}
	aside := func(lim, req Amount, format func(Amount) string) string {
		var parts []string
		if !lim.OK && p.BoundsKnown {
			parts = append(parts, "no limit")
		}
		if req.OK {
			parts = append(parts, format(req)+" requested")
		} else if p.BoundsKnown {
			parts = append(parts, "no request")
		}
		return strings.Join(parts, ", ")
	}
	sum.Gauges = []Gauge{
		{Label: "CPU", Used: p.CPU, Of: p.CPULim, OfWhat: "limit", cpu: true, Aside: aside(p.CPULim, p.CPUReq, FormatCPU)},
		{Label: "memory", Used: p.Mem, Of: p.MemLim, OfWhat: "limit", Aside: aside(p.MemLim, p.MemReq, FormatMem)},
	}
	if p.Node != "" {
		sum.Facts = append(sum.Facts, "on node "+p.Node)
	}
	if p.Phase != "" {
		sum.Facts = append(sum.Facts, p.Phase)
	}
	if !p.Reporting() {
		sum.Facts = append(sum.Facts, "no sample from the metrics API")
	}
	if !p.BoundsKnown {
		sum.Facts = append(sum.Facts, "requests and limits unknown")
	}
	return sum
}

// --- as text ------------------------------------------------------------------

// clean makes a cell safe to put in a line of text: names come from the
// cluster, and one with a newline in it must not be able to pass itself off
// as another row.
func clean(s string) string { return textutil.OneLine(textutil.Sanitize(s)) }

// Table writes the rows as aligned plain text, at most limit of them (0 for
// all). Bar columns are left out; a row's flags are appended.
func (l *Listing) Table(limit int) string {
	var cols []int
	for i, c := range l.Columns {
		if !c.Bar {
			cols = append(cols, i)
		}
	}
	rows := l.Rows
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	widths := make([]int, len(cols))
	for j, i := range cols {
		widths[j] = len([]rune(l.Columns[i].Name))
		for _, r := range rows {
			if i < len(r.Cells) {
				widths[j] = max(widths[j], len([]rune(clean(r.Cells[i]))))
			}
		}
	}
	var b strings.Builder
	line := func(cell func(i int) string, tail string) {
		for j, i := range cols {
			text := cell(i)
			pad := strings.Repeat(" ", max(widths[j]-len([]rune(text)), 0))
			if l.Columns[i].Right {
				b.WriteString(pad + text)
			} else if j == len(cols)-1 && tail == "" {
				b.WriteString(text)
			} else {
				b.WriteString(text + pad)
			}
			if j < len(cols)-1 {
				b.WriteString("  ")
			}
		}
		if tail != "" {
			b.WriteString("  " + tail)
		}
		b.WriteByte('\n')
	}
	line(func(i int) string { return l.Columns[i].Name }, "")
	for _, r := range rows {
		tail := ""
		if len(r.Flags) > 0 {
			tail = "<- " + r.Heat.String() + ": " + clean(strings.Join(r.Flags, ", "))
		}
		line(func(i int) string {
			if i < len(r.Cells) {
				return clean(r.Cells[i])
			}
			return ""
		}, tail)
	}
	return b.String()
}
