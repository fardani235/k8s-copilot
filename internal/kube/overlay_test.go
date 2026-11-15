package kube_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

func listUsage(t *testing.T, f *kubetest.Fake, typ, ns string) *kube.Table {
	t.Helper()
	tbl, err := f.ListWithUsage(context.Background(), resolve(t, f, typ), ns, 0, kube.MetricsFreshFor)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Overlay == nil {
		t.Fatalf("%s: no overlay recorded", typ)
	}
	return tbl
}

// cell returns the cell under column col of the row named name.
func cell(t *testing.T, tbl *kube.Table, name, col string) string {
	t.Helper()
	ci := -1
	for i, c := range tbl.Columns {
		if c == col {
			ci = i
		}
	}
	if ci < 0 {
		t.Fatalf("no column %q in %v", col, tbl.Columns)
	}
	for _, r := range tbl.Rows {
		if r.Name == name {
			return r.Cells[ci]
		}
	}
	t.Fatalf("no row %q", name)
	return ""
}

func TestOverlayPodsAndNodesCarryUsage(t *testing.T) {
	f := busyCluster()
	pods := listUsage(t, f, "pods", "shop")
	o := pods.Overlay
	if o.From != 3 || len(pods.Columns) != 9 || !o.Present() {
		t.Fatalf("columns %v, from %d", pods.Columns, o.From)
	}
	if got := strings.Join(pods.Columns[o.From:], " "); got != "CPU %CPU/R %CPU/L MEM %MEM/R %MEM/L" {
		t.Fatalf("overlay columns: %s", got)
	}
	if got := o.Columns; len(got) != 6 || got[0] != 1 || got[3] != 2 || got[1] <= got[3] || got[2] <= got[3] {
		t.Fatalf("drop ranks %v: percentages, then MEM, then CPU", got)
	}
	// web-1: 490m of 300m requested; the sidecar has no limit, so the pod
	// has none.
	for col, want := range map[string]string{"CPU": "490m", "%CPU/R": "163%", "%CPU/L": "no lim", "MEM": "510Mi", "%MEM/R": "177%", "%MEM/L": "no lim"} {
		if got := cell(t, pods, "web-1", col); got != want {
			t.Errorf("web-1 %s = %q, want %q", col, got, want)
		}
	}
	// The same cells as the metrics listing's own, for every row.
	snap := f.Metrics(context.Background(), "shop", kube.MetricsFreshFor)
	if snap != o.Snap && snap.SampleTime != o.Snap.SampleTime {
		t.Fatal("the overlay did not use the shared readings")
	}
	for _, r := range pods.Rows {
		p, ok := snap.Pod(r.Namespace, r.Name)
		if !ok {
			continue
		}
		want := metrics.PodUsageCells(p)
		if got := strings.Join(r.Cells[o.From:], "|"); got != strings.Join(want[:], "|") {
			t.Errorf("%s: %s, metrics says %v", r.Name, got, want)
		}
	}
	// The pending pod has no sample: unknown, not zero.
	if got := cell(t, pods, "web-pending", "CPU"); got != metrics.NoData {
		t.Errorf("pending pod CPU = %q", got)
	}

	nodes := listUsage(t, f, "nodes", "shop") // namespace ignored: cluster-scoped
	if got := strings.Join(nodes.Columns[nodes.Overlay.From:], " "); got != "CPU %CPU MEM %MEM" {
		t.Fatalf("node overlay columns: %s", got)
	}
	for col, want := range map[string]string{"CPU": "1.2", "%CPU": "30%", "MEM": "6Gi", "%MEM": "75%"} {
		if got := cell(t, nodes, "node-a", col); got != want {
			t.Errorf("node-a %s = %q, want %q", col, got, want)
		}
	}
	if line, warn := nodes.Overlay.Line(time.Now()); warn || !strings.HasPrefix(line, "CPU/MEM sampled ") {
		t.Errorf("line %q warn %v", line, warn)
	}

	// Other types, and a custom resource, are left alone.
	for _, typ := range []string{"deployments", "widgets"} {
		tbl, err := f.ListWithUsage(context.Background(), resolve(t, f, typ), "shop", 0, kube.MetricsFreshFor)
		if err != nil || tbl.Overlay != nil {
			t.Errorf("%s decorated: %v %v", typ, err, tbl.Overlay)
		}
	}
	custom := &kube.Table{Type: kube.ResourceType{Group: "example.com", Resource: "pods"}, Columns: []string{"NAME"}}
	if kube.AttachUsage(custom, snap, "shop").Overlay != nil {
		t.Error("a custom resource called pods was decorated")
	}
}

func TestOverlayAbsentWhenReadingsAreNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error // nil: the metrics API is not served
		want string
		warn bool
	}{
		{"absent", nil, "Metrics are not available on this cluster", false},
		{"unavailable", apierrors.NewServiceUnavailable("no endpoints"), "The metrics API is not answering", true},
		{"denied", forbidden("pods"), "Permission denied reading", true},
		{"unauthenticated", apierrors.NewUnauthorized("token expired"), "The cluster did not accept your credentials: token expired", true},
		{"timed out", apierrors.NewTimeoutError("slow", 1), "did not answer in time", true},
		{"failed", errors.New("connection reset by peer"), "Cannot read", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := busyCluster()
			if tc.err != nil {
				f.FailMetrics(tc.err)
			} else {
				f = kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"))
			}
			for _, typ := range []string{"pods", "nodes"} {
				own, err := f.List(context.Background(), resolve(t, f, typ), "shop", 0)
				if err != nil {
					t.Fatal(err)
				}
				tbl := listUsage(t, f, typ, "shop")
				if len(tbl.Columns) != len(own.Columns) || tbl.Overlay.Present() || tbl.Overlay.Snap != nil {
					t.Fatalf("%s: columns %v (own %v)", typ, tbl.Columns, own.Columns)
				}
				for _, r := range tbl.Rows {
					if len(r.Cells) != len(own.Columns) {
						t.Fatalf("%s: row %s has %d cells", typ, r.Name, len(r.Cells))
					}
				}
				line, warn := tbl.Overlay.Line(time.Now())
				if !strings.Contains(tbl.Overlay.Note, tc.want) || line != tbl.Overlay.Note || warn != tc.warn ||
					!strings.Contains(line, "Usage is unknown, not zero.") || !strings.HasPrefix(line, "No CPU/memory columns") {
					t.Fatalf("%s: line %q warn %v, want %q", typ, line, warn, tc.want)
				}
			}
		})
	}

	// Readings of one namespace never decorate a listing of all of them.
	f := busyCluster()
	snap := f.Metrics(context.Background(), kube.AllNamespaces, 0).Narrow("shop")
	all, err := f.List(context.Background(), resolve(t, f, "pods"), kube.AllNamespaces, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := len(all.Columns)
	kube.AttachUsage(all, snap, kube.AllNamespaces)
	if len(all.Columns) != n || all.Overlay.Present() || !strings.Contains(all.Overlay.Note, "cover namespace shop only, and this listing covers all namespaces") {
		t.Fatalf("columns %v, note %q", all.Columns, all.Overlay.Note)
	}
	ops, _ := f.List(context.Background(), resolve(t, f, "pods"), "ops", 0)
	kube.AttachUsage(ops, snap, "ops")
	if ops.Overlay.Present() || !strings.Contains(ops.Overlay.Note, "not namespace ops") {
		t.Fatalf("note %q", ops.Overlay.Note)
	}
}

func TestOverlayDropsColumnsWithNothingKnown(t *testing.T) {
	ctx := context.Background()
	f := busyCluster()
	podsType := resolve(t, f, "pods")
	tbl, err := f.List(ctx, podsType, "shop", 0)
	if err != nil {
		t.Fatal(err)
	}
	// From here on the pod objects cannot be read: usage is known,
	// requests and limits are not.
	f.Dynamic.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group != "" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	snap := f.Metrics(ctx, "shop", 0)
	if snap.Sources.Pods.OK() || !snap.Sources.PodMetrics.OK() {
		t.Fatalf("setup: %+v", snap.Sources)
	}
	kube.AttachUsage(tbl, snap, "shop")
	if got := strings.Join(tbl.Columns[tbl.Overlay.From:], " "); got != "CPU MEM" {
		t.Fatalf("with bounds unknown the columns are %q, want only CPU MEM", got)
	}
	if line, warn := tbl.Overlay.Line(time.Now()); !warn || !strings.Contains(line, "Requests and limits are unknown (permission denied reading pods in namespace shop), so usage is shown without percentages.") {
		t.Fatalf("line %q", line)
	}
	if cell(t, tbl, "web-1", "CPU") != "490m" || cell(t, tbl, "web-pending", "CPU") != metrics.NoData {
		t.Fatal("per-row values")
	}

	// "no lim" is a fact: a column of it is kept.
	f = kubetest.New(kubetest.Node("node-a", "4", "8Gi"),
		kubetest.PodOn("shop", "free-1", "node-a", kubetest.Container("app", "", "", "", "")))
	f.SetNodeMetrics("node-a", "100m", "1Gi")
	f.SetPodMetrics("shop", "free-1", kubetest.Usage{Container: "app", CPU: "0", Memory: "0"})
	free := listUsage(t, f, "pods", "shop")
	if got := strings.Join(free.Rows[0].Cells[free.Overlay.From:], " "); got != "0m no req no lim 0 no req no lim" {
		t.Fatalf("a pod with no bounds and zero usage: %q", got)
	}
	if free.Overlay.Note != "" {
		t.Fatalf("note %q", free.Overlay.Note)
	}

	// No listed pod has a sample: no columns at all, and the reason.
	f = kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "quiet-1"))
	f.SetNodeMetrics("node-a", "100m", "1Gi")
	quiet := listUsage(t, f, "pods", "shop")
	if quiet.Overlay.Present() || len(quiet.Columns) != quiet.Overlay.From ||
		!strings.Contains(quiet.Overlay.Note, "returned no sample for any listed pod") {
		t.Fatalf("columns %v note %q", quiet.Columns, quiet.Overlay.Note)
	}
}

// Over a real HTTP client (client-go's fake runs every call behind one lock,
// so it cannot show two requests in flight): a hanging metrics API costs a
// listing at most UsageOverlayWait, the read carries on, and the next listing
// gets the columns from it without asking again.
func TestListWithUsageDoesNotWaitForASlowMetricsAPI(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return false
	})
	var once sync.Once
	answer := func() { once.Do(func() { close(release) }) }
	t.Cleanup(answer) // before the server is closed
	ctx := context.Background()
	// The stand-in serves no discovery: name the type directly.
	pods := kube.ResourceType{Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"list"}}

	start := time.Now()
	tbl, err := c.ListWithUsage(ctx, pods, kube.AllNamespaces, 0, kube.MetricsFreshFor)
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > kube.UsageOverlayWait+2*time.Second {
		t.Fatalf("the listing waited %v", waited)
	}
	if tbl.Overlay.Present() || len(tbl.Rows) != 1 || !strings.Contains(tbl.Overlay.Note, "did not answer in time") {
		t.Fatalf("rows %d, columns %v, note %q", len(tbl.Rows), tbl.Columns, tbl.Overlay.Note)
	}

	// The read carried on: once it lands, the next listing has the columns
	// without starting another.
	answer()
	tbl, err = c.ListWithUsage(ctx, pods, kube.AllNamespaces, 0, kube.MetricsFreshFor)
	if err != nil {
		t.Fatal(err)
	}
	if !tbl.Overlay.Present() || tbl.Rows[0].Cells[tbl.Overlay.From] != "3m" {
		t.Fatalf("after the read completed: columns %v, note %q", tbl.Columns, tbl.Overlay.Note)
	}
	n := calls.Load()
	if n != 2 {
		t.Fatalf("%d metrics requests, want 2 (nodes + pods, once)", n)
	}
	for range 3 {
		if _, err := c.ListWithUsage(ctx, pods, kube.AllNamespaces, 0, kube.MetricsFreshFor); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != n {
		t.Fatalf("listings within the freshness window asked the metrics API again (%d → %d)", n, calls.Load())
	}
}

func TestListWithUsageReturnsListErrors(t *testing.T) {
	f := busyCluster()
	f.Dynamic.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group != "" || a.GetNamespace() != "shop" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope"))
	})
	if _, err := f.ListWithUsage(context.Background(), resolve(t, f, "pods"), "shop", 0, kube.MetricsFreshFor); !kube.IsDenied(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestOverlayLine(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	o := func(s *metrics.Snapshot, note string) *kube.UsageOverlay {
		return &kube.UsageOverlay{From: 1, Columns: []int{1}, Note: note, Snap: s}
	}
	fresh := &metrics.Snapshot{TakenAt: now.Add(-2 * time.Second), SampleTime: now.Add(-8 * time.Second), Window: 15 * time.Second}
	for _, tc := range []struct {
		name string
		o    *kube.UsageOverlay
		want string
		warn bool
	}{
		{"fresh", o(fresh, ""), "CPU/MEM sampled 8s ago · 15s window", false},
		{"fresh with a caveat", o(fresh, "Requests and limits are unknown (x), so usage is shown without percentages."),
			"CPU/MEM sampled 8s ago · 15s window · Requests and limits are unknown (x), so usage is shown without percentages.", false},
		{"stale", o(&metrics.Snapshot{TakenAt: now, SampleTime: now.Add(-2 * time.Minute), Window: 15 * time.Second}, ""),
			"CPU/MEM sampled 2m ago · 15s window · OLD", true},
		{"held", o(&metrics.Snapshot{TakenAt: now.Add(-20 * time.Second), SampleTime: now.Add(-25 * time.Second), Window: 15 * time.Second,
			Held: &metrics.Hold{Cause: metrics.SourceStatus{Source: metrics.PodMetrics, State: metrics.Unavailable}, FailedAt: now.Add(-5 * time.Second)}}, ""),
			"CPU/MEM NOT CURRENT: refresh failing since 5s ago — showing the readings from 20s ago (The metrics API is not answering)", true},
		{"absent", &kube.UsageOverlay{Note: "No CPU/memory columns — x."}, "No CPU/memory columns — x.", false},
	} {
		got, warn := tc.o.Line(now)
		if got != tc.want || warn != tc.warn {
			t.Errorf("%s: %q warn %v, want %q warn %v", tc.name, got, warn, tc.want, tc.warn)
		}
	}
	// Through the cache: a failed refresh keeps the columns, marked, with
	// the cause.
	f := busyCluster()
	listUsage(t, f, "pods", "shop")
	f.FailMetrics(apierrors.NewServiceUnavailable("metrics-server is down"))
	tbl, err := f.ListWithUsage(context.Background(), resolve(t, f, "pods"), "shop", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	line, warn := tbl.Overlay.Line(time.Now())
	if !tbl.Overlay.Present() || !warn || !strings.HasPrefix(line, "CPU/MEM NOT CURRENT: refresh failing since") || !strings.HasSuffix(line, "(The metrics API is not answering)") {
		t.Fatalf("held: %q warn %v", line, warn)
	}
}
