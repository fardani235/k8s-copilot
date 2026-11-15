package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// The ordinary pods and nodes listings carry current usage beside their own
// columns. The cells are the metrics listing's own (metrics.PodUsageCells,
// metrics.NodeUsageCells) taken from the shared metrics cache, so the
// browser, the copilot's list_resources and the metrics screen cannot show
// different numbers for the same pod or node.
//
// The rule that shapes everything here: when the readings are not there, no
// usage column is added at all — not a column of "—", not a column of zeros
// — and the overlay says in one line why.

// UsageOverlayWait is how long a listing waits for the readings. The read
// itself carries on (it is detached and bounded by MetricsTimeout), so a
// slow metrics API costs the first listing this long and the next listing
// finds the readings in the cache.
const UsageOverlayWait = 3 * time.Second

// UsageOverlay describes the usage columns appended to a Table.
type UsageOverlay struct {
	// From is the index of the first overlay column in Table.Columns; it is
	// len(Table.Columns) when there are none.
	From int
	// Columns ranks the overlay columns for dropping on a narrow pane, one
	// per overlay column: higher goes first, and every overlay column goes
	// before any of the listing's own.
	Columns []int
	// Note is the one line to show: why the columns are absent, or the one
	// caveat that qualifies them. Empty when there is nothing to say.
	Note string
	// Snap is the snapshot the cells came from; nil when there are no
	// columns.
	Snap *metrics.Snapshot

	// warn marks Note as a failure or caveat to be drawn as a warning, as
	// opposed to the plain absence of a metrics API.
	warn bool
}

// Present reports whether the overlay added any column.
func (o *UsageOverlay) Present() bool { return o != nil && o.Snap != nil && len(o.Columns) > 0 }

// Line is the one line the browser and the copilot both print above the
// table: why there is no usage, or how old it is. warn says it should be
// read as a warning (old, held, or a failure).
func (o *UsageOverlay) Line(now time.Time) (text string, warn bool) {
	if o == nil {
		return "", false
	}
	if !o.Present() {
		return o.Note, o.warn && o.Note != ""
	}
	f := o.Snap.Freshness(now)
	switch {
	case o.Snap.Held != nil:
		text = fmt.Sprintf("CPU/MEM NOT CURRENT: %s (%s)", f.Text, oneLine(o.Snap.Held.Cause.Headline()))
	default:
		text = "CPU/MEM " + f.Text
	}
	if o.Note != "" {
		text += " · " + o.Note
	}
	return text, f.Warn || o.warn
}

// OwnCells are the cells of r that belong to the listing itself, not to the
// usage overlay: what a filter or a tint should look at.
func (t *Table) OwnCells(r Row) []string {
	if t.Overlay == nil || t.Overlay.From > len(r.Cells) {
		return r.Cells
	}
	return r.Cells[:t.Overlay.From]
}

// usageKind says which overlay a type takes: core pods, core nodes, or none.
// A custom resource that happens to be called "pods" is not decorated.
func usageKind(t ResourceType) string {
	if t.Group != "" {
		return ""
	}
	switch t.Resource {
	case "pods", "nodes":
		return t.Resource
	}
	return ""
}

// ListWithUsage is List, and for core pods and nodes also the usage overlay.
// The listing and the readings are read side by side; the readings are
// waited for at most UsageOverlayWait and come from the shared cache when it
// holds some no older than maxAge. The table is returned once, complete, so
// the columns never appear later than the rows.
func (c *Cluster) ListWithUsage(ctx context.Context, t ResourceType, namespace string, limit int, maxAge time.Duration) (*Table, error) {
	if usageKind(t) == "" {
		return c.List(ctx, t, namespace, limit)
	}
	if !t.Namespaced {
		namespace = AllNamespaces
	}
	mctx, cancel := context.WithTimeout(ctx, UsageOverlayWait)
	defer cancel()
	var (
		wg   sync.WaitGroup
		snap *metrics.Snapshot
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		snap = c.Metrics(mctx, namespace, maxAge)
	}()
	tbl, err := c.List(ctx, t, namespace, limit)
	if err != nil {
		cancel() // nothing to decorate: stop waiting for the readings
		wg.Wait()
		return nil, err
	}
	wg.Wait()
	return AttachUsage(tbl, snap, namespace), nil
}

// AttachUsage appends the usage columns to a core pods or nodes listing,
// from snap, and records what it did in t.Overlay. namespace is the
// listing's scope. Any other type is returned untouched.
func AttachUsage(t *Table, snap *metrics.Snapshot, namespace string) *Table {
	if t == nil {
		return t
	}
	kind := usageKind(t.Type)
	if kind == "" {
		return t
	}
	o := &UsageOverlay{From: len(t.Columns)}
	t.Overlay = o
	if snap == nil {
		o.Note, o.warn = "No CPU/memory columns: the readings were not obtained. Usage is unknown, not zero.", true
		return t
	}
	if kind == "nodes" {
		attachNodes(t, o, snap)
	} else {
		attachPods(t, o, snap, namespace)
	}
	return t
}

// overlayColumn is one candidate column: its name, its drop rank and
// whether it is a percentage of a bound.
type overlayColumn struct {
	name    string
	drop    int
	percent bool
}

var (
	podOverlay = []overlayColumn{
		{"CPU", 1, false}, {"%CPU/R", 4, true}, {"%CPU/L", 3, true},
		{"MEM", 2, false}, {"%MEM/R", 4, true}, {"%MEM/L", 3, true},
	}
	nodeOverlay = []overlayColumn{
		{"CPU", 1, false}, {"%CPU", 3, true}, {"MEM", 2, false}, {"%MEM", 3, true},
	}
)

func attachNodes(t *Table, o *UsageOverlay, snap *metrics.Snapshot) {
	if st := snap.Sources.NodeMetrics; !st.OK() {
		absent(o, st)
		return
	}
	byName := make(map[string]*metrics.Node, len(snap.Nodes))
	for i := range snap.Nodes {
		byName[snap.Nodes[i].Name] = &snap.Nodes[i]
	}
	cells := make([][]string, len(t.Rows))
	for i, r := range t.Rows {
		n, ok := byName[r.Name]
		if !ok {
			n = &metrics.Node{} // not in the readings: unknown, never zero
		}
		c := metrics.NodeUsageCells(n)
		cells[i] = c[:]
	}
	caveat := ""
	if st := snap.Sources.Nodes; !st.OK() {
		caveat = fmt.Sprintf("Node capacity is unknown (%s), so usage is shown without percentages.", lowerFirst(oneLine(st.Headline())))
	}
	place(t, o, snap, nodeOverlay, cells, "node", caveat)
}

func attachPods(t *Table, o *UsageOverlay, snap *metrics.Snapshot, namespace string) {
	if st := snap.Sources.PodMetrics; !st.OK() {
		absent(o, st)
		return
	}
	if snap.Scope != "" && snap.Scope != namespace {
		// Most rows would be "—": a column that looks like data and is not.
		o.warn = true
		if namespace == AllNamespaces {
			o.Note = fmt.Sprintf("No CPU/memory columns: the readings cover namespace %s only, and this listing covers all namespaces.", oneLine(snap.Scope))
		} else {
			o.Note = fmt.Sprintf("No CPU/memory columns: the readings cover namespace %s only, not namespace %s.", oneLine(snap.Scope), oneLine(namespace))
		}
		return
	}
	byKey := make(map[string]*metrics.Pod, len(snap.Pods))
	for i := range snap.Pods {
		byKey[snap.Pods[i].Key()] = &snap.Pods[i]
	}
	cells := make([][]string, len(t.Rows))
	for i, r := range t.Rows {
		p, ok := byKey[r.Namespace+"/"+r.Name]
		if !ok {
			// Finished, or created after the sample: unknown, never zero.
			p = &metrics.Pod{}
		}
		c := metrics.PodUsageCells(p)
		cells[i] = c[:]
	}
	caveat := ""
	if st := snap.Sources.Pods; !st.OK() {
		caveat = fmt.Sprintf("Requests and limits are unknown (%s), so usage is shown without percentages.", lowerFirst(oneLine(st.Headline())))
	} else {
		caveat = "Requests and limits are unknown for every listed pod, so usage is shown without percentages."
	}
	place(t, o, snap, podOverlay, cells, "pod", caveat)
}

// place appends the columns that have at least one known cell. A column of
// nothing but "—" is not a column of data; "no req", "no lim" and "none" are
// facts, so a column of them is kept.
func place(t *Table, o *UsageOverlay, snap *metrics.Snapshot, cols []overlayColumn, cells [][]string, noun, boundsCaveat string) {
	if len(t.Rows) == 0 {
		return // nothing listed, nothing to decorate or explain
	}
	var keep []int
	droppedPercent, keptUsage := false, false
	for j, col := range cols {
		known := false
		for i := range cells {
			if cells[i][j] != metrics.NoData {
				known = true
				break
			}
		}
		switch {
		case known:
			keep = append(keep, j)
			keptUsage = keptUsage || !col.percent
		case col.percent:
			droppedPercent = true
		}
	}
	if !keptUsage {
		o.Note, o.warn = fmt.Sprintf("No CPU/memory columns: the metrics API returned no sample for any listed %s. Usage is unknown, not zero.", noun), true
		return
	}
	for i := range t.Rows {
		r := &t.Rows[i]
		for len(r.Cells) < o.From {
			r.Cells = append(r.Cells, "")
		}
		for _, j := range keep {
			r.Cells = append(r.Cells, cells[i][j])
		}
	}
	for _, j := range keep {
		t.Columns = append(t.Columns, cols[j].name)
		o.Columns = append(o.Columns, cols[j].drop)
	}
	o.Snap = snap
	if droppedPercent {
		o.Note, o.warn = boundsCaveat, true
	}
}

// absent records why there are no columns, in the metrics screen's words.
func absent(o *UsageOverlay, st metrics.SourceStatus) {
	why := st.Headline()
	switch st.State {
	case metrics.Denied, metrics.Unauthenticated, metrics.Failed:
		if r := oneLine(st.Reason); r != "" {
			why += ": " + textutil.Truncate(r, 160)
		}
	}
	o.Note = "No CPU/memory columns — " + oneLine(strings.TrimSuffix(why, ".")) + ". Usage is unknown, not zero."
	// A cluster without a metrics API is a standing fact, not an alarm.
	o.warn = st.State != metrics.Absent
}

func oneLine(s string) string { return textutil.OneLine(textutil.Sanitize(s)) }

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
