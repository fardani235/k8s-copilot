package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

// headerCols is the column header line of the listing as drawn at w×h.
func (u *ui) headerCols(w, h int) []string {
	u.t.Helper()
	lines := strings.Split(ansi.Strip(u.m.viewList(w, h)), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "NAME") || strings.HasPrefix(l, "NAMESPACE") {
			return strings.Fields(l)
		}
	}
	u.t.Fatalf("no header line:\n%s", strings.Join(lines, "\n"))
	return nil
}

func TestListingShowsUsageColumns(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	u.want("pods · namespace shop · 3", "CPU/MEM sampled", "15s window")
	if got := strings.Join(u.headerCols(118, 30), " "); got != "NAME STATUS AGE CPU %CPU/R %CPU/L MEM %MEM/R %MEM/L" {
		t.Fatalf("header: %s", got)
	}
	u.want("490m", "163%", "no lim", "510Mi")
	// The pending pod has no sample: unknown, not zero.
	for _, l := range strings.Split(u.screen(), "\n") {
		if strings.Contains(l, "web-pending") && (!strings.Contains(l, "—") || looksLikeAReading.MatchString(l)) {
			t.Fatalf("pending pod: %q", l)
		}
	}

	// Nodes, cluster-scoped: their own usage columns and no namespace.
	nodes, err := kube.ResolveIn(u.m.types, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	u.m.curType = nodes
	u.pump(u.m.loadList())
	u.want("nodes · cluster-wide · 2", "CPU/MEM sampled")
	if got := strings.Join(u.headerCols(118, 30), " "); got != "NAME STATUS AGE CPU %CPU MEM %MEM" {
		t.Fatalf("node header: %s", got)
	}
	u.want("1.9", "95%")
}

func TestListingWithoutUsageSaysWhy(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.want("pods · namespace shop · 2", "web-1",
		"No CPU/memory columns — Metrics are not available on this cluster. Usage is unknown, not zero.")
	u.wantNot("%CPU/R", "MEM ")
	if got := strings.Join(u.headerCols(118, 30), " "); got != "NAME STATUS AGE" {
		t.Fatalf("header: %s", got)
	}
	// Types other than pods and nodes carry no line at all.
	u.keys("t")
	u.typeText("deployments")
	u.keys("enter")
	u.want("deployments.apps · namespace shop · 1")
	u.wantNot("No CPU/memory columns", "CPU/MEM")
}

// The table still fills the pane under the extra line.
func TestListingWithUsageFillsThePane(t *testing.T) {
	objs := []runtime.Object{kubetest.Namespace("shop"), kubetest.Node("node-a", "4", "8Gi")}
	for i := range 60 {
		objs = append(objs, kubetest.PodOn("shop", fmt.Sprintf("web-%02d", i), "node-a", kubetest.Container("app", "100m", "200m", "64Mi", "128Mi")))
	}
	f := kubetest.New(objs...)
	for i := range 60 {
		f.SetPodMetrics("shop", fmt.Sprintf("web-%02d", i), kubetest.Usage{Container: "app", CPU: "50m", Memory: "32Mi"})
	}
	f.SetNodeMetrics("node-a", "3", "2Gi")
	u := newUI(t, f, nil)
	w, h := u.m.browserInner()
	out := ansi.Strip(u.m.viewList(w, h))
	if n := strings.Count(out, "\n") + 1; n != h {
		t.Fatalf("the listing drew %d lines in a pane of %d:\n%s", n, h, out)
	}
	if !strings.Contains(out, "CPU/MEM sampled") || strings.Count(out, "web-") != h-3 {
		t.Fatalf("want the line and %d rows:\n%s", h-3, out)
	}
	if u.m.listRows() != h-4 {
		t.Fatalf("page is %d rows, pane shows %d", u.m.listRows(), h-3)
	}
	// Paging still lands where it says.
	u.keys("end")
	u.want("web-59")
}

func TestListingFilterIgnoresUsage(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	for _, tc := range []struct {
		filter string
		rows   int
	}{
		{"490m", 0}, {"no lim", 0}, {"163%", 0}, // usage figures do not match
		{"web-1", 1}, {"running", 2}, {"pending", 1}, {"", 3}, // the listing's own fields do
	} {
		u.m.setFilter(tc.filter)
		if len(u.m.rows) != tc.rows {
			t.Errorf("filter %q: %d rows, want %d", tc.filter, len(u.m.rows), tc.rows)
		}
	}
}

// On a narrow pane the usage columns go first — percentages, then MEM, then
// CPU — and the listing's own never go for them.
func TestListingNarrowPaneDropsUsageFirst(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	keepOrder := []string{"CPU", "MEM", "%CPU/L", "%MEM/L", "%CPU/R", "%MEM/R"}
	seen := map[int]bool{}
	for w := 100; w >= 22; w-- {
		cols := u.headerCols(w, 20)
		if strings.Contains(strings.Join(cols, " "), "…") {
			// Even the listing's own columns overflow: they are clipped, as
			// before, and by now every usage column has given way.
			if !seen[0] {
				t.Fatalf("width %d: clipping while usage columns remain: %v", w, cols)
			}
			break
		}
		if len(cols) < 3 || cols[0] != "NAME" || cols[1] != "STATUS" || cols[2] != "AGE" {
			t.Fatalf("width %d: own columns lost: %v", w, cols)
		}
		usage := cols[3:]
		// What is left is always the first k of the keep order, as a set.
		want := map[string]bool{}
		for _, c := range keepOrder[:len(usage)] {
			want[c] = true
		}
		for _, c := range usage {
			if !want[c] {
				t.Fatalf("width %d: %v kept before %v", w, usage, keepOrder[:len(usage)])
			}
		}
		seen[len(usage)] = true
		for _, l := range strings.Split(ansi.Strip(u.m.viewList(w, 20)), "\n") {
			if lipWidth(l) > w {
				t.Fatalf("width %d: line of %d cells: %q", w, lipWidth(l), l)
			}
		}
	}
	for k := 0; k <= len(keepOrder); k++ {
		if !seen[k] {
			t.Errorf("no width showed exactly %d usage columns", k)
		}
	}
}

// The age keeps counting with nothing else redrawing the listing (refresh is
// off in newUI), and the timer reads nothing from the cluster.
func TestListingUsageAgeKeepsCounting(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil)
	if !u.m.listClockOn || u.m.listClock == 0 {
		t.Fatal("no redraw timer for a listing with usage columns")
	}
	gen := u.m.listClock
	u.want("CPU/MEM sampled")
	u.wantNot("OLD")

	old := *u.m.table.Overlay.Snap // a copy: the cache's own snapshot stays as it is
	old.SampleTime = time.Now().Add(-4 * time.Minute)
	u.m.table.Overlay.Snap = &old
	actions := len(f.Dynamic.Actions()) + len(f.Clientset.Actions())
	_, cmd := u.m.Update(listClockMsg{gen})
	if cmd == nil {
		t.Fatal("the redraw timer stopped while the listing is showing")
	}
	u.want("CPU/MEM sampled 4m", "OLD")
	if msg := cmdMsg(cmd); msg != nil { // waits 200ms
		t.Fatalf("the timer produced %T at once: it should only wait and redraw", msg)
	}
	if got := len(f.Dynamic.Actions()) + len(f.Clientset.Actions()); got != actions {
		t.Fatalf("the redraw timer read the cluster (%d → %d actions)", actions, got)
	}

	// Another view retires it; coming back starts a new one.
	u.keys("?")
	if u.m.onListClock(listClockMsg{gen}) != nil || u.m.listClockOn {
		t.Fatal("the timer runs while another view is showing")
	}
	u.keys("esc")
	if u.m.listClock == gen || !u.m.listClockOn || u.m.onListClock(listClockMsg{gen}) != nil {
		t.Fatal("no fresh timer after returning, or the old one is still honoured")
	}

	// No overlay, no timer.
	plain := newUI(t, fakeCluster(), nil)
	if plain.m.listClockOn {
		t.Fatal("a timer for a listing without usage columns")
	}
}

func TestListingRefreshKeepsSelectionAndKeys(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil)
	u.keys("down") // db-0 → web-1
	if _, name := u.m.selectedKey(); name != "web-1" {
		t.Fatalf("selected %q", name)
	}
	// New readings, and a refresh: the selection stays, the usage follows.
	f.SetPodMetrics("shop", "web-1", kubetest.Usage{Container: "app", CPU: "100m", Memory: "100Mi"}, kubetest.Usage{Container: "sidecar", CPU: "10m", Memory: "20Mi"})
	f.Metrics(context.Background(), kube.AllNamespaces, 0)
	u.keys("r")
	if _, name := u.m.selectedKey(); name != "web-1" {
		t.Fatalf("after refresh selected %q", name)
	}
	u.want("110m")
	u.wantNot("490m")

	u.keys("enter")
	u.want("web-1")
	if u.m.view != vDetail || u.m.subj.name != "web-1" {
		t.Fatalf("enter opened %v %q", u.m.view, u.m.subj.name)
	}
	u.keys("esc", "l")
	if u.m.view != vLogs || u.m.subj.name != "web-1" {
		t.Fatalf("l opened %v %q", u.m.view, u.m.subj.name)
	}
	u.keys("esc", "e")
	if u.m.view != vEvents || u.m.subj.name != "web-1" {
		t.Fatalf("e opened %v %q", u.m.view, u.m.subj.name)
	}
	u.keys("esc")
	// A namespace change re-lists with that namespace's usage.
	u.keys("n")
	u.typeText("ops")
	u.keys("enter")
	u.want("pods · namespace ops · 1", "cron-1", "CPU/MEM sampled", "0m")
}

// End to end: the same pods in the browser model and through the copilot's
// list_resources carry the same usage cells; without a metrics API, neither
// carries a usage column, and both say why in the same words.
func TestBrowserAndCopilotListTheSameUsage(t *testing.T) {
	for _, served := range []bool{true, false} {
		f := loadedCluster()
		if !served {
			f = kubetest.New(kubetest.Namespace("shop"), kubetest.Node("node-a", "4", "8Gi"),
				kubetest.PodOn("shop", "web-1", "node-a", kubetest.Container("app", "250m", "500m", "256Mi", "512Mi")))
		}
		u := newUI(t, f, nil)
		tbl := u.m.table
		if tbl == nil || tbl.Overlay == nil {
			t.Fatalf("served=%v: no listing with an overlay", served)
		}
		out, err := tools.NewRegistry(f.Cluster, tools.DefaultOptions()).Read(context.Background(), "list_resources",
			[]byte(`{"type":"pods","namespace":"shop"}`))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out, "\n")
		browserLine, _ := tbl.Overlay.Line(time.Now())
		if !served {
			if tbl.Overlay.Present() || strings.Contains(out, "%CPU") || strings.Contains(u.screen(), "%CPU") {
				t.Fatalf("usage columns without a metrics API:\n%s\n%s", out, u.screen())
			}
			if lines[1] != browserLine || !strings.Contains(browserLine, "Metrics are not available on this cluster") {
				t.Fatalf("copilot %q, browser %q", lines[1], browserLine)
			}
			u.want(browserLine)
			continue
		}
		if !tbl.Overlay.Present() {
			t.Fatalf("no usage columns: %q", tbl.Overlay.Note)
		}
		// Every usage cell of every row, against the copilot's text table:
		// a row is the same cells, in order, separated by padding.
		usageCols := tbl.Columns[tbl.Overlay.From:]
		compared := 0
		for _, r := range tbl.Rows {
			var row string
			for _, l := range lines {
				if strings.HasPrefix(l, r.Name+" ") {
					row = l
				}
			}
			if row == "" {
				t.Fatalf("%s missing from list_resources:\n%s", r.Name, out)
			}
			rest := row
			for i, c := range r.Cells {
				k := strings.Index(rest, c)
				if k < 0 {
					t.Fatalf("%s: copilot row %q lacks browser cell %q (column %s)", r.Name, row, c, tbl.Columns[i])
				}
				rest = rest[k+len(c):]
				if i >= tbl.Overlay.From {
					compared++
				}
			}
		}
		if compared != len(usageCols)*len(tbl.Rows) || compared == 0 {
			t.Fatalf("compared %d usage cells", compared)
		}
		if lines[1] == "" || !strings.HasPrefix(lines[1], "CPU/MEM sampled") || !strings.HasPrefix(browserLine, "CPU/MEM sampled") {
			t.Fatalf("copilot line %q, browser line %q", lines[1], browserLine)
		}
	}
}
