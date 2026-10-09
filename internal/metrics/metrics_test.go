package metrics_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fardani235/k8s-copilot/internal/metrics"
)

type obj = map[string]any

func u(o obj) unstructured.Unstructured { return unstructured.Unstructured{Object: o} }

func nodeObj(name, cpu, mem string, ready bool) unstructured.Unstructured {
	status := "True"
	if !ready {
		status = "False"
	}
	return u(obj{"metadata": obj{"name": name}, "status": obj{
		"allocatable": obj{"cpu": cpu, "memory": mem},
		"conditions":  []any{obj{"type": "Ready", "status": status}},
	}})
}

func nodeMetric(name, cpu, mem string, at time.Time) unstructured.Unstructured {
	return u(obj{"metadata": obj{"name": name}, "timestamp": at.UTC().Format(time.RFC3339), "window": "15s",
		"usage": obj{"cpu": cpu, "memory": mem}})
}

// container is name, then cpu request, cpu limit, memory request, memory
// limit ("" = not set).
func container(name, cpuReq, cpuLim, memReq, memLim string) obj {
	res := obj{}
	put := func(kind, key, v string) {
		if v == "" {
			return
		}
		if res[kind] == nil {
			res[kind] = obj{}
		}
		res[kind].(obj)[key] = v
	}
	put("requests", "cpu", cpuReq)
	put("limits", "cpu", cpuLim)
	put("requests", "memory", memReq)
	put("limits", "memory", memLim)
	return obj{"name": name, "resources": res}
}

func podObj(ns, name, node, phase string, containers ...obj) unstructured.Unstructured {
	cs := make([]any, len(containers))
	for i, c := range containers {
		cs[i] = c
	}
	return u(obj{"metadata": obj{"namespace": ns, "name": name},
		"spec": obj{"nodeName": node, "containers": cs}, "status": obj{"phase": phase}})
}

func podMetric(ns, name string, at time.Time, usage ...[3]string) unstructured.Unstructured {
	cs := make([]any, len(usage))
	for i, c := range usage {
		cs[i] = obj{"name": c[0], "usage": obj{"cpu": c[1], "memory": c[2]}}
	}
	return u(obj{"metadata": obj{"namespace": ns, "name": name}, "timestamp": at.UTC().Format(time.RFC3339), "window": "15s", "containers": cs})
}

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func ok(src metrics.Source) metrics.SourceStatus { return metrics.SourceStatus{Source: src} }

func allOK() metrics.Sources {
	return metrics.Sources{NodeMetrics: ok(metrics.NodeMetrics), PodMetrics: ok(metrics.PodMetrics), Nodes: ok(metrics.NodeObjects), Pods: ok(metrics.PodObjects)}
}

// sample is a small cluster: node-a busy on memory, node-b silent
// (NotReady); a pod near its memory limit, one with no limits, one pending.
func sample() metrics.Inputs {
	return metrics.Inputs{
		TakenAt: t0.Add(10 * time.Second), Sources: allOK(),
		Nodes:       []unstructured.Unstructured{nodeObj("node-a", "4", "8Gi", true), nodeObj("node-b", "4", "8Gi", false)},
		NodeMetrics: []unstructured.Unstructured{nodeMetric("node-a", "1", "7373Mi", t0)},
		Pods: []unstructured.Unstructured{
			podObj("shop", "web-1", "node-a", "Running", container("app", "250m", "500m", "256Mi", "512Mi")),
			podObj("shop", "free-1", "node-a", "Running", container("app", "", "", "", "")),
			podObj("shop", "new-1", "", "Pending", container("app", "100m", "", "", "")),
			podObj("ops", "done-1", "node-a", "Succeeded", container("job", "", "", "", "")),
		},
		PodMetrics: []unstructured.Unstructured{
			podMetric("shop", "web-1", t0, [3]string{"app", "100m", "490Mi"}),
			podMetric("shop", "free-1", t0, [3]string{"app", "0", "0"}),
		},
	}
}

func cells(t *testing.T, l *metrics.Listing, key string) map[string]string {
	t.Helper()
	for _, r := range l.Rows {
		if r.Key == key {
			out := map[string]string{}
			for i, c := range l.Columns {
				if !c.Bar {
					out[c.Name] = r.Cells[i]
				}
			}
			return out
		}
	}
	t.Fatalf("no row %q in %s", key, l.What)
	return nil
}

func TestFormatting(t *testing.T) {
	for _, tc := range []struct {
		a    metrics.Amount
		want string
	}{
		{metrics.Amount{}, "—"}, {metrics.Known(0), "0m"}, {metrics.Millicores(431), "431m"}, {metrics.Millicores(999), "999m"},
		{metrics.Millicores(1000), "1"}, {metrics.Millicores(1930), "1.93"}, {metrics.Millicores(12400), "12.4"}, {metrics.Millicores(8000), "8"},
		// A fraction of a millicore was measured: it is not written as nothing.
		{metrics.Known(100_000), "1m"}, {metrics.Known(1), "1m"}, {metrics.Known(431_000_001), "432m"},
	} {
		if got := metrics.FormatCPU(tc.a); got != tc.want {
			t.Errorf("FormatCPU(%+v) = %q, want %q", tc.a, got, tc.want)
		}
	}
	for _, tc := range []struct {
		a    metrics.Amount
		want string
	}{
		{metrics.Amount{}, "—"}, {metrics.Known(0), "0"}, {metrics.Known(512 << 10), "512Ki"}, {metrics.Known(431 << 20), "431Mi"},
		{metrics.Known(1 << 30), "1Gi"}, {metrics.Known(12025908429), "11.2Gi"}, {metrics.Known(3 << 40), "3Ti"},
	} {
		if got := metrics.FormatMem(tc.a); got != tc.want {
			t.Errorf("FormatMem(%+v) = %q, want %q", tc.a, got, tc.want)
		}
	}

	for _, tc := range []struct {
		used, of metrics.Amount
		want     int
		ok       bool
	}{
		{metrics.Known(1), metrics.Known(3), 33, true}, {metrics.Known(2), metrics.Known(3), 67, true},
		{metrics.Known(150), metrics.Known(100), 150, true}, {metrics.Known(0), metrics.Known(100), 0, true},
		{metrics.Amount{}, metrics.Known(100), 0, false}, {metrics.Known(5), metrics.Amount{}, 0, false}, {metrics.Known(5), metrics.Known(0), 0, false},
	} {
		if got, ok := metrics.Percent(tc.used, tc.of); got != tc.want || ok != tc.ok {
			t.Errorf("Percent(%+v, %+v) = %d, %v; want %d, %v", tc.used, tc.of, got, ok, tc.want, tc.ok)
		}
	}

	if metrics.HeatOf(74) != metrics.Calm || metrics.HeatOf(75) != metrics.Elevated || metrics.HeatOf(89) != metrics.Elevated || metrics.HeatOf(90) != metrics.High {
		t.Error("heat thresholds moved")
	}
}

// The heart of it: a reading that is not there is "—", and a real zero is
// "0". They are never confused, in either direction.
func TestUnknownIsNeverZero(t *testing.T) {
	s := metrics.Build(sample())

	nodes := s.List(metrics.Query{Level: metrics.LevelNodes})
	b := cells(t, nodes, "node-b")
	if b["CPU"] != "—" || b["%CPU"] != "—" || b["MEM"] != "—" || b["%MEM"] != "—" {
		t.Fatalf("a node with no sample: %v", b)
	}
	if b["NODE"] != "node-b (NotReady)" {
		t.Fatalf("the reason a node is silent is not shown: %q", b["NODE"])
	}
	var noted bool
	for _, n := range nodes.Notes {
		noted = noted || strings.Contains(n.Text, "1 node shown as —") && strings.Contains(n.Text, "unknown, not zero")
	}
	if !noted {
		t.Fatalf("no note explains the —: %+v", nodes.Notes)
	}
	// The silent node is left out of the totals on both sides of the ratio.
	if s.Cluster.Reporting != 1 || s.Cluster.CPU != metrics.Millicores(1000) || s.Cluster.CPUAlloc != metrics.Millicores(4000) {
		t.Fatalf("cluster totals: %+v", s.Cluster)
	}
	if facts := strings.Join(nodes.Summary.Facts, " "); !strings.Contains(facts, "1 of 2 nodes reporting") {
		t.Fatalf("summary does not say a node is missing from the totals: %q", facts)
	}

	pods := s.List(metrics.Query{Level: metrics.LevelPods})
	free := cells(t, pods, "shop/free-1")
	if free["CPU"] != "0m" || free["MEM"] != "0" {
		t.Fatalf("a real zero must show as zero: %v", free)
	}
	if free["%CPU/L"] != "no lim" || free["%MEM/R"] != "no req" {
		t.Fatalf("\"none set\" must not look like \"unknown\": %v", free)
	}
	pending := cells(t, pods, "shop/new-1")
	if pending["CPU"] != "—" || pending["MEM"] != "—" || pending["%CPU/R"] != "—" || pending["NODE"] != "<none>" {
		t.Fatalf("a pod with no sample: %v", pending)
	}
	if _, listed := s.Pod("ops", "done-1"); listed {
		t.Fatal("a finished pod is listed")
	}

	// And nothing anywhere reads 0 for the things that were not measured.
	zero := regexp.MustCompile(`^0(m|%|Ki|Mi)?$`)
	for name, c := range b {
		// PODS is a count that was really made: nothing is scheduled on
		// node-b, and 0 is the truth.
		if name != "PODS" && zero.MatchString(c) {
			t.Errorf("node-b column %s shows %q for a missing reading", name, c)
		}
	}
	if b["PODS"] != "0" {
		t.Errorf("node-b PODS = %q, want the real count 0", b["PODS"])
	}
	for name, c := range pending {
		if zero.MatchString(c) {
			t.Errorf("pending pod column %s shows %q for a missing reading", name, c)
		}
	}
}

// With the usage source gone there is no table at all — only the reason.
func TestMissingSourceIsAStateNotATable(t *testing.T) {
	for _, tc := range []struct {
		state    metrics.SourceState
		headline string
		explain  string
	}{
		{metrics.Absent, "Metrics are not available on this cluster", "installs nothing"},
		{metrics.Unavailable, "The metrics API is not answering", "registered on this cluster but nothing healthy is answering"},
		{metrics.Denied, "Permission denied reading pod metrics across all namespaces", "authorization failure, not an idle cluster"},
		{metrics.TimedOut, "The metrics API did not answer in time", "it is not zero"},
		{metrics.Failed, "Cannot read pod metrics", "failed"},
	} {
		in := sample()
		in.Sources.PodMetrics = metrics.SourceStatus{Source: metrics.PodMetrics, State: tc.state, Reason: "server words"}
		in.Sources.NodeMetrics = metrics.SourceStatus{Source: metrics.NodeMetrics, State: tc.state, Reason: "server words"}
		s := metrics.Build(in)
		for _, level := range metrics.Levels {
			l := s.List(metrics.Query{Level: level, Namespace: "shop", Pod: "web-1"})
			if l.Unavailable == nil || len(l.Rows) != 0 || len(l.Columns) != 0 {
				t.Fatalf("state %v, level %s: got a table (%d rows) instead of an unavailable state", tc.state, level, len(l.Rows))
			}
		}
		st := *s.List(metrics.Query{Level: metrics.LevelPods}).Unavailable
		if st.Headline() != tc.headline || !strings.Contains(st.Explain(), tc.explain) || st.Reason != "server words" {
			t.Errorf("state %v: headline %q, explanation %q", tc.state, st.Headline(), st.Explain())
		}
		if sum := s.List(metrics.Query{Level: metrics.LevelNodes}).Summary; len(sum.Gauges) != 0 || !strings.Contains(sum.Missing, "totals unknown") {
			t.Errorf("state %v: summary claims totals: %+v", tc.state, sum)
		}
	}
	// The five states read differently from each other.
	seen := map[string]bool{}
	for _, st := range []metrics.SourceState{metrics.Absent, metrics.Unavailable, metrics.Denied, metrics.TimedOut, metrics.Failed} {
		seen[metrics.SourceStatus{Source: metrics.PodMetrics, State: st}.Explain()] = true
	}
	if len(seen) != 5 {
		t.Fatalf("only %d distinct explanations for 5 states", len(seen))
	}
}

// One source missing leaves the others standing, each caveat stated.
func TestPartialSources(t *testing.T) {
	// Usage readable, capacity not: numbers without percentages.
	in := sample()
	in.Sources.Nodes = metrics.SourceStatus{Source: metrics.NodeObjects, State: metrics.Denied}
	l := metrics.Build(in).List(metrics.Query{Level: metrics.LevelNodes})
	if a := cells(t, l, "node-a"); a["CPU"] != "1" || a["%CPU"] != "—" {
		t.Fatalf("node-a without capacity: %v", a)
	}
	if len(l.Notes) == 0 || !l.Notes[0].Warn || !strings.Contains(l.Notes[0].Text, "Node capacity is unknown") {
		t.Fatalf("notes: %+v", l.Notes)
	}

	// Usage readable, pod specs not: "—" for bounds, never "no lim".
	in = sample()
	in.Sources.Pods = metrics.SourceStatus{Source: metrics.PodObjects, State: metrics.Denied}
	s := metrics.Build(in)
	l = s.List(metrics.Query{Level: metrics.LevelPods})
	web := cells(t, l, "shop/web-1")
	if web["CPU"] != "100m" || web["%CPU/L"] != "—" || web["%MEM/R"] != "—" {
		t.Fatalf("web-1 without its spec: %v", web)
	}
	if _, has := web["NODE"]; has {
		t.Fatal("a NODE column is shown though nothing says which node a pod is on")
	}
	if l.Notes[len(l.Notes)-1].Text == "" || !strings.Contains(l.Notes[len(l.Notes)-1].Text, "Requests and limits are unknown") {
		t.Fatalf("notes: %+v", l.Notes)
	}
	// …and so the pods of one node cannot be told apart.
	if onNode := s.List(metrics.Query{Level: metrics.LevelPods, Node: "node-a"}); onNode.Unavailable == nil || onNode.Unavailable.Source != metrics.PodObjects {
		t.Fatalf("pods on a node without pod objects: %+v", onNode)
	}
	c := s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "web-1"})
	if app := cells(t, c, "app"); app["CPU-LIM"] != "—" || app["%MEM/L"] != "—" {
		t.Fatalf("container without its spec: %v", app)
	}

	// Node metrics refused, pod metrics fine: one tab is a state, the other a table.
	in = sample()
	in.Sources.NodeMetrics = metrics.SourceStatus{Source: metrics.NodeMetrics, State: metrics.Denied}
	s = metrics.Build(in)
	if s.List(metrics.Query{Level: metrics.LevelNodes}).Unavailable == nil {
		t.Fatal("nodes listed without node metrics")
	}
	if l := s.List(metrics.Query{Level: metrics.LevelPods}); l.Unavailable != nil || len(l.Rows) != 3 {
		t.Fatalf("pods: %+v", l)
	}
}

func TestListings(t *testing.T) {
	s := metrics.Build(sample())

	// Nodes: usage against allocatable, flagged when hot.
	nodes := s.List(metrics.Query{Level: metrics.LevelNodes})
	a := cells(t, nodes, "node-a")
	if a["CPU"] != "1" || a["%CPU"] != "25%" || a["MEM"] != "7.2Gi" || a["%MEM"] != "90%" || a["PODS"] != "2" {
		t.Fatalf("node-a: %v", a)
	}
	if r := nodes.Rows[0]; r.Key != "node-a" || r.Heat != metrics.High || len(r.Flags) != 1 || r.Flags[0] != "memory 90% of allocatable" {
		t.Fatalf("node-a row: %+v", r)
	}
	if got := nodes.Summary.Lines()[0]; got != "cluster: CPU 1 of 4 allocatable (25%) · memory 7.2Gi of 8Gi allocatable (90%)" {
		t.Fatalf("summary: %q", got)
	}

	// Namespaces: a lower bound is called one.
	in := sample()
	in.Pods = append(in.Pods, podObj("shop", "quiet-1", "node-a", "Running", container("app", "", "", "", "")))
	nss := metrics.Build(in).List(metrics.Query{Level: metrics.LevelNamespaces})
	if shop := cells(t, nss, "shop"); shop["PODS"] != "4" || shop["CPU"] != "100m" || shop["%CPU/R"] != "29%" {
		t.Fatalf("shop: %v", shop)
	}
	if len(nss.Notes) != 1 || !strings.Contains(nss.Notes[0].Text, "lower bound") || !strings.Contains(nss.Notes[0].Text, "shop (2 of 3)") {
		t.Fatalf("notes: %+v", nss.Notes)
	}

	// Pods: against requests and limits; near a limit is HIGH.
	pods := s.List(metrics.Query{Level: metrics.LevelPods, Namespace: "shop"})
	web := cells(t, pods, "shop/web-1")
	if web["CPU"] != "100m" || web["%CPU/R"] != "40%" || web["%CPU/L"] != "20%" || web["MEM"] != "490Mi" || web["%MEM/R"] != "191%" || web["%MEM/L"] != "96%" {
		t.Fatalf("web-1: %v", web)
	}
	if _, has := web["NAMESPACE"]; has {
		t.Fatal("NAMESPACE column in a single-namespace listing")
	}
	if pods.Rows[0].Key != "shop/web-1" || pods.Rows[0].Heat != metrics.High || pods.Rows[0].Flags[0] != "memory 96% of limit" {
		t.Fatalf("first row: %+v", pods.Rows[0])
	}
	// Above a *request* is normal and is not flagged.
	if pods.What != "pods in namespace shop" || pods.Summary.Gauges[1].Heat() != metrics.Calm {
		t.Fatalf("what %q, gauge heat %v", pods.What, pods.Summary.Gauges[1].Heat())
	}
	if pods.Rows[len(pods.Rows)-1].Key != "shop/new-1" {
		t.Fatal("a pod with no reading is not sorted last")
	}

	onNode := s.List(metrics.Query{Level: metrics.LevelPods, Node: "node-a"})
	if len(onNode.Rows) != 2 || onNode.Summary.Subject != "node node-a" || onNode.Summary.Gauges[1].Heat() != metrics.High {
		t.Fatalf("pods on node-a: %d rows, summary %+v", len(onNode.Rows), onNode.Summary)
	}
	if empty := s.List(metrics.Query{Level: metrics.LevelPods, Node: "node-b"}); empty.Empty != "No pods on node node-b." || empty.Unavailable != nil {
		t.Fatalf("an empty node: %+v", empty)
	}

	// Sorting and filtering.
	byName := s.List(metrics.Query{Level: metrics.LevelPods, Sort: metrics.ByName})
	if byName.Rows[0].Key != "shop/free-1" || byName.Rows[2].Key != "shop/web-1" {
		t.Fatalf("by name: %v", byName.Rows)
	}
	byMem := s.List(metrics.Query{Level: metrics.LevelPods, Sort: metrics.ByMemory, Filter: "WEB"})
	if len(byMem.Rows) != 1 || byMem.Total != 3 || byMem.Rows[0].Name != "web-1" {
		t.Fatalf("filtered: %d of %d", len(byMem.Rows), byMem.Total)
	}
}

func TestContainers(t *testing.T) {
	in := sample()
	in.Pods[0] = u(obj{
		"metadata": obj{"namespace": "shop", "name": "web-1"},
		"spec": obj{"nodeName": "node-a",
			"initContainers": []any{container("migrate", "1", "1", "1Gi", "1Gi"), func() obj {
				c := container("proxy", "10m", "", "16Mi", "32Mi")
				c["restartPolicy"] = "Always"
				return c
			}()},
			"containers": []any{container("app", "250m", "500m", "256Mi", "512Mi")}},
		"status": obj{"phase": "Running", "containerStatuses": []any{obj{
			"name": "app", "restartCount": int64(7), "state": obj{"running": obj{}},
			"lastState": obj{"terminated": obj{"reason": "OOMKilled", "exitCode": int64(137)}}}}},
	})
	in.PodMetrics[0] = podMetric("shop", "web-1", t0, [3]string{"app", "100m", "490Mi"}, [3]string{"proxy", "5m", "30Mi"})
	s := metrics.Build(in)

	l := s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "web-1"})
	if len(l.Rows) != 2 {
		t.Fatalf("a finished init container is listed: %+v", l.Rows)
	}
	app := cells(t, l, "app")
	if app["CPU"] != "100m" || app["CPU-REQ"] != "250m" || app["CPU-LIM"] != "500m" || app["%CPU/L"] != "20%" ||
		app["MEM"] != "490Mi" || app["MEM-LIM"] != "512Mi" || app["%MEM/L"] != "96%" || app["RESTARTS"] != "7" || app["LAST-EXIT"] != "OOMKilled" {
		t.Fatalf("app: %v", app)
	}
	if r := l.Rows[0]; r.Heat != metrics.High || strings.Join(r.Flags, "; ") != "memory 96% of limit; previous instance was OOMKilled" {
		t.Fatalf("app row: %+v", r)
	}
	proxy := cells(t, l, "proxy")
	if proxy["CONTAINER"] != "proxy (sidecar)" || proxy["CPU-LIM"] != "none" || proxy["%CPU/L"] != "no lim" || proxy["%MEM/L"] != "94%" {
		t.Fatalf("proxy: %v", proxy)
	}
	// The pod's limit: memory is fully limited, CPU is not.
	p, _ := s.Pod("shop", "web-1")
	if p.CPULim.OK || p.MemLim != metrics.Known(544<<20) || p.CPUReq != metrics.Millicores(260) {
		t.Fatalf("pod bounds: %+v", p)
	}
	if got := l.Summary.Lines()[0]; got != "pod shop/web-1: CPU 105m, no limit, 260m requested · memory 520Mi of 544Mi limit (96%), 272Mi requested" {
		t.Fatalf("summary: %q", got)
	}

	gone := s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "nope"})
	if gone.Unavailable != nil || !strings.Contains(gone.Empty, "is not in the readings") {
		t.Fatalf("a pod that is gone: %+v", gone)
	}
}

func TestFreshnessAndHold(t *testing.T) {
	s := metrics.Build(sample())
	if !s.SampleTime.Equal(t0) || s.Window != 15*time.Second {
		t.Fatalf("sample time %v window %v", s.SampleTime, s.Window)
	}
	if f := s.Freshness(t0.Add(8 * time.Second)); f.Text != "sampled 8s ago · 15s window" || f.Short != "8s old" || f.Warn {
		t.Fatalf("%+v", f)
	}
	if f := s.Freshness(t0.Add(4 * time.Minute)); !f.Warn || !strings.Contains(f.Text, "sampled 4m ago") || !strings.Contains(f.Text, "OLD") || !strings.HasPrefix(f.Short, "OLD") {
		t.Fatalf("an old sample is not flagged: %+v", f)
	}
	// A cluster clock ahead of ours must not produce a negative age.
	if f := s.Freshness(t0.Add(-time.Minute)); f.Text != "sampled 0s ago · 15s window" {
		t.Fatalf("%+v", f)
	}

	failedIn := sample()
	failedIn.TakenAt = t0.Add(30 * time.Second)
	failedIn.Sources.NodeMetrics = metrics.SourceStatus{Source: metrics.NodeMetrics, State: metrics.Unavailable}
	failedIn.Sources.PodMetrics = metrics.SourceStatus{Source: metrics.PodMetrics, State: metrics.Unavailable}
	failed := metrics.Build(failedIn)
	if failed.HasUsage() {
		t.Fatal("a snapshot with both metrics sources down claims usage")
	}
	held := s.HeldBy(failed)
	if s.Held != nil || held.Held == nil || held.Held.Cause.State != metrics.Unavailable {
		t.Fatalf("held: %+v (original modified: %v)", held.Held, s.Held != nil)
	}
	f := held.Freshness(t0.Add(40 * time.Second))
	if !f.Warn || f.Text != "refresh failing since 10s ago — showing the readings from 30s ago" || !strings.HasPrefix(f.Short, "NOT CURRENT") {
		t.Fatalf("%+v", f)
	}

	// Without any sample there is no "sampled" claim.
	none := sample()
	none.NodeMetrics, none.PodMetrics = nil, nil
	if f := metrics.Build(none).Freshness(t0.Add(15 * time.Second)); f.Text != "read 5s ago" {
		t.Fatalf("%+v", f)
	}
}

func TestNarrowKeepsTheReadings(t *testing.T) {
	in := sample()
	in.Pods = append(in.Pods, podObj("ops", "cron-1", "node-a", "Running", container("c", "", "", "", "")))
	in.PodMetrics = append(in.PodMetrics, podMetric("ops", "cron-1", t0, [3]string{"c", "7m", "9Mi"}))
	wide := metrics.Build(in)
	shop := wide.Narrow("shop")
	if shop.Scope != "shop" || len(shop.Pods) != 3 || len(shop.Namespaces) != 1 || len(wide.Pods) != 4 || wide.Scope != "" {
		t.Fatalf("narrowed: %d pods, %d namespaces; original now %d pods", len(shop.Pods), len(shop.Namespaces), len(wide.Pods))
	}
	// The same listing comes out of either, cell for cell.
	q := metrics.Query{Level: metrics.LevelPods, Namespace: "shop"}
	if a, b := wide.List(q).Table(0), shop.List(q).Table(0); a != b {
		t.Fatalf("narrowing changed the listing:\n%s\nvs\n%s", a, b)
	}
	if n, _ := shop.Node("node-a"); n.Pods.OK {
		t.Fatal("a namespace-scoped snapshot claims a node's pod count")
	}
	if n, _ := wide.Node("node-a"); n.Pods != metrics.Known(3) {
		t.Fatalf("narrowing modified the original: %+v", n.Pods)
	}
	if nodes := shop.List(metrics.Query{Level: metrics.LevelNodes}); len(nodes.Columns) != 7 {
		t.Fatalf("a PODS column in a namespace-scoped node listing: %+v", nodes.Columns)
	}

	// Narrowing readings that are being held keeps them marked as held.
	failed := sample()
	failed.Sources.PodMetrics = metrics.SourceStatus{Source: metrics.PodMetrics, State: metrics.Unavailable}
	failed.Sources.NodeMetrics = metrics.SourceStatus{Source: metrics.NodeMetrics, State: metrics.Unavailable}
	if held := wide.HeldBy(metrics.Build(failed)).Narrow("shop"); held.Held == nil || len(held.Pods) != 3 {
		t.Fatalf("narrowing dropped the hold: %+v", held.Held)
	}
	// Narrowing a read that failed gives the failure, in that namespace's terms.
	l := metrics.Build(failed).Narrow("shop").List(q)
	if l.Unavailable == nil || l.Unavailable.Scope != "shop" || len(l.Rows) != 0 {
		t.Fatalf("a failed read, narrowed: %+v", l)
	}
}

func TestTableText(t *testing.T) {
	l := metrics.Build(sample()).List(metrics.Query{Level: metrics.LevelNodes})
	got := l.Table(0)
	want := "" +
		"NODE               CPU  %CPU    MEM  %MEM  PODS\n" +
		"node-a               1   25%  7.2Gi   90%     2  <- HIGH: memory 90% of allocatable\n" +
		"node-b (NotReady)    —     —      —     —     0\n"
	if got != want {
		t.Fatalf("table:\n%s\nwant:\n%s", got, want)
	}
	if one := l.Table(1); strings.Count(one, "\n") != 2 || strings.Contains(one, "node-b") {
		t.Fatalf("limit ignored:\n%s", one)
	}

	g := l.Summary.Gauges[1]
	if g.Text() != "memory 7.2Gi of 8Gi allocatable (90%)" || g.Short() != "memory 7.2Gi/8Gi alloc (90%)" || g.Tiny() != "memory 90%" || !g.Bounded() {
		t.Fatalf("gauge: %q / %q / %q", g.Text(), g.Short(), g.Tiny())
	}
}

func TestTruncatedSourcesAreFlagged(t *testing.T) {
	in := sample()
	in.Truncated = []metrics.Source{metrics.PodObjects}
	l := metrics.Build(in).List(metrics.Query{Level: metrics.LevelNamespaces})
	last := l.Notes[len(l.Notes)-1]
	if !last.Warn || !strings.Contains(last.Text, "more pods than were read") {
		t.Fatalf("notes: %+v", l.Notes)
	}
}

// One Snapshot is read by the screen (the UI goroutine) and by the copilot's
// tool (the agent's goroutine) at the same time. Nothing that reads it may
// write to it. Run under -race.
func TestSnapshotIsSafeToShare(t *testing.T) {
	in := sample()
	in.Pods = append(in.Pods, podObj("ops", "cron-1", "node-a", "Running", container("c", "", "", "", "")))
	in.PodMetrics = append(in.PodMetrics, podMetric("ops", "cron-1", t0, [3]string{"c", "7m", "9Mi"}))
	s := metrics.Build(in)
	want := s.List(metrics.Query{Level: metrics.LevelPods}).Table(0)

	done := make(chan string)
	for i := 0; i < 8; i++ {
		go func() {
			var last string
			for j := 0; j < 50; j++ {
				for _, lv := range metrics.Levels {
					for _, by := range []metrics.Sort{metrics.ByCPU, metrics.ByMemory, metrics.ByName} {
						s.List(metrics.Query{Level: lv, Namespace: "shop", Pod: "web-1", Sort: by, Filter: "w"})
					}
				}
				s.Narrow("shop").List(metrics.Query{Level: metrics.LevelNamespaces})
				s.HeldBy(s).Freshness(t0)
				last = s.List(metrics.Query{Level: metrics.LevelPods}).Table(0)
			}
			done <- last
		}()
	}
	for i := 0; i < 8; i++ {
		if got := <-done; got != want {
			t.Fatalf("the snapshot changed while being read:\n%s\nwant:\n%s", got, want)
		}
	}
}

// An init container that is still running is the pod's whole usage. It is
// shown against its own limit — not as having none, and not against the
// limits of containers that have not started.
func TestRunningInitContainerKeepsItsBounds(t *testing.T) {
	pod := podObj("shop", "migrate-1", "node-a", "Pending", container("app", "100m", "2", "64Mi", "4Gi"))
	pod.Object["spec"].(obj)["initContainers"] = []any{container("init-db", "500m", "500m", "256Mi", "256Mi")}
	in := metrics.Inputs{TakenAt: t0, Sources: allOK(), Pods: []unstructured.Unstructured{pod},
		PodMetrics: []unstructured.Unstructured{podMetric("shop", "migrate-1", t0, [3]string{"init-db", "500m", "255Mi"})}}
	s := metrics.Build(in)

	cs := s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "migrate-1"})
	init := cells(t, cs, "init-db")
	if init["CONTAINER"] != "init-db (init)" || init["CPU-LIM"] != "500m" || init["%CPU/L"] != "100%" || init["MEM-LIM"] != "256Mi" || init["%MEM/L"] != "100%" {
		t.Fatalf("the running init container: %v", init)
	}
	if app := cells(t, cs, "app"); app["CPU"] != "—" || app["CPU-LIM"] != "2" {
		t.Fatalf("the container that has not started: %v", app)
	}
	pods := s.List(metrics.Query{Level: metrics.LevelPods})
	row := cells(t, pods, "shop/migrate-1")
	if row["%CPU/L"] != "100%" || row["%MEM/L"] != "100%" || row["%CPU/R"] != "100%" {
		t.Fatalf("the pod is measured against containers that are not running: %v", row)
	}
	if r := pods.Rows[0]; r.Heat != metrics.High || len(r.Flags) != 2 {
		t.Fatalf("a pod at its limits is not flagged: %+v", r)
	}

	// A container the spec does not describe at all: unknown, never "none".
	in.PodMetrics = []unstructured.Unstructured{podMetric("shop", "migrate-1", t0, [3]string{"app", "50m", "32Mi"}, [3]string{"stranger", "10m", "8Mi"})}
	s = metrics.Build(in)
	cs = s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "migrate-1"})
	if x := cells(t, cs, "stranger"); x["CPU-LIM"] != "—" || x["%MEM/L"] != "—" || x["CPU-REQ"] != "—" {
		t.Fatalf("a container not in the spec: %v", x)
	}
	if app := cells(t, cs, "app"); app["CPU-LIM"] != "2" {
		t.Fatalf("its neighbour, which is in the spec: %v", app)
	}
	row = cells(t, s.List(metrics.Query{Level: metrics.LevelPods}), "shop/migrate-1")
	if row["%CPU/L"] != "—" || row["%MEM/R"] != "—" {
		t.Fatalf("pod bounds stated although one running container's are unknown: %v", row)
	}
	if got := strings.Join(s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "migrate-1"}).Summary.Lines(), " | "); strings.Contains(got, "no limit") || !strings.Contains(got, "requests and limits unknown") {
		t.Fatalf("summary: %s", got)
	}
	if ns := cells(t, s.List(metrics.Query{Level: metrics.LevelNamespaces}), "shop"); ns["%CPU/R"] != "—" {
		t.Fatalf("a namespace's share of requests computed over a partial sum: %v", ns)
	}

	// A debug container really has no limit, and saying so is true.
	pod.Object["spec"].(obj)["ephemeralContainers"] = []any{obj{"name": "debugger"}}
	in.PodMetrics = []unstructured.Unstructured{podMetric("shop", "migrate-1", t0, [3]string{"app", "50m", "32Mi"}, [3]string{"debugger", "1m", "1Mi"})}
	cs = metrics.Build(in).List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "migrate-1"})
	if d := cells(t, cs, "debugger"); d["CONTAINER"] != "debugger (ephemeral)" || d["CPU-LIM"] != "none" || d["%CPU/L"] != "no lim" {
		t.Fatalf("an ephemeral container: %v", d)
	}
}

// A sample that lists no containers measured nothing. That is not a reading
// of zero.
func TestSampleWithoutContainersIsNotZero(t *testing.T) {
	for name, containers := range map[string]any{"missing": "absent", "null": nil, "empty": []any{}} {
		o := obj{"metadata": obj{"namespace": "shop", "name": "web-1"}, "timestamp": t0.Format(time.RFC3339), "window": "15s"}
		if containers != "absent" {
			o["containers"] = containers
		}
		in := sample()
		in.PodMetrics = []unstructured.Unstructured{u(o)}
		s := metrics.Build(in)
		p, _ := s.Pod("shop", "web-1")
		if p.CPU.OK || p.Mem.OK || p.Reporting() {
			t.Fatalf("containers %s: the pod has a reading: %+v / %+v", name, p.CPU, p.Mem)
		}
		row := cells(t, s.List(metrics.Query{Level: metrics.LevelPods}), "shop/web-1")
		if row["CPU"] != "—" || row["MEM"] != "—" || row["%MEM/L"] != "—" {
			t.Fatalf("containers %s: %v", name, row)
		}
		sum := strings.Join(s.List(metrics.Query{Level: metrics.LevelContainers, Namespace: "shop", Pod: "web-1"}).Summary.Lines(), " ")
		if !strings.Contains(sum, "CPU — ") || !strings.Contains(sum, "memory — ") || strings.Contains(sum, "CPU 0m") {
			t.Fatalf("containers %s: summary %q", name, sum)
		}
	}
}

// Sums are made of what was measured, not of what was displayed.
func TestSmallReadingsAreNotRoundedUpBeforeAdding(t *testing.T) {
	in := metrics.Inputs{TakenAt: t0, Sources: allOK(), Nodes: []unstructured.Unstructured{nodeObj("node-a", "4", "8Gi", true)},
		NodeMetrics: []unstructured.Unstructured{nodeMetric("node-a", "30m", "1Gi", t0)}}
	for i := 0; i < 100; i++ {
		name := "idle-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		in.Pods = append(in.Pods, podObj("shop", name, "node-a", "Running", container("a", "", "", "", ""), container("b", "", "", "", ""), container("c", "", "", "", "")))
		in.PodMetrics = append(in.PodMetrics, podMetric("shop", name, t0, [3]string{"a", "100000n", "1Mi"}, [3]string{"b", "100000n", "1Mi"}, [3]string{"c", "100000n", "1Mi"}))
	}
	s := metrics.Build(in)
	if len(s.Pods) != 100 {
		t.Fatalf("%d pods", len(s.Pods))
	}
	// 300 containers at a tenth of a millicore are 30m, not 300m.
	if ns := s.Namespaces[0]; ns.CPU != metrics.Millicores(30) {
		t.Fatalf("namespace CPU = %+v, want 30m", ns.CPU)
	}
	if shop := cells(t, s.List(metrics.Query{Level: metrics.LevelNamespaces}), "shop"); shop["CPU"] != "30m" {
		t.Fatalf("shop: %v", shop)
	}
	// …and the node's own reading and the sum of its pods now agree.
	if facts := strings.Join(s.List(metrics.Query{Level: metrics.LevelPods, Node: "node-a"}).Summary.Lines(), " | "); !strings.Contains(facts, "CPU 30m of 4 allocatable") || !strings.Contains(facts, "100 pods using CPU 30m") {
		t.Fatalf("node summary: %s", facts)
	}
	// One of them alone is still shown as using something.
	if row := s.List(metrics.Query{Level: metrics.LevelPods}).Rows[0]; row.Cells[2] != "1m" {
		t.Fatalf("a pod using 0.3m is shown as %q", row.Cells[2])
	}
}

// A percentage is of something. Usage of four nodes over the capacity of
// three is not.
func TestClusterShareNeedsEveryReportingNodesCapacity(t *testing.T) {
	in := metrics.Inputs{TakenAt: t0, Sources: allOK(),
		Nodes:       []unstructured.Unstructured{nodeObj("node-a", "4", "8Gi", true)},
		NodeMetrics: []unstructured.Unstructured{nodeMetric("node-a", "2", "4Gi", t0), nodeMetric("node-new", "2", "4Gi", t0)}}
	s := metrics.Build(in)
	if s.Cluster.CPU != metrics.Millicores(4000) || s.Cluster.CPUAlloc.OK || s.Cluster.MemAlloc.OK {
		t.Fatalf("cluster: %+v", s.Cluster)
	}
	l := s.List(metrics.Query{Level: metrics.LevelNodes})
	if got := l.Summary.Lines()[0]; got != "cluster: CPU 4 · memory 8Gi" {
		t.Fatalf("summary claims a share: %q", got)
	}
	if l.Summary.Gauges[0].Heat() != metrics.Calm {
		t.Fatal("the cluster is flagged on a percentage that does not exist")
	}
	if a := cells(t, l, "node-a"); a["%CPU"] != "50%" {
		t.Fatalf("node-a: %v", a)
	}
	if n := cells(t, l, "node-new"); n["CPU"] != "2" || n["%CPU"] != "—" {
		t.Fatalf("node-new: %v", n)
	}
}

// Limits bind containers. A pod with one container at its limit is in
// trouble however roomy its neighbour is.
func TestPodIsAsHotAsItsHottestContainer(t *testing.T) {
	in := metrics.Inputs{TakenAt: t0, Sources: allOK(),
		Pods: []unstructured.Unstructured{podObj("shop", "web-1", "node-a", "Running",
			container("app", "200m", "200m", "100Mi", "100Mi"), container("proxy", "100m", "2", "64Mi", "1Gi"))},
		PodMetrics: []unstructured.Unstructured{podMetric("shop", "web-1", t0, [3]string{"app", "200m", "99Mi"}, [3]string{"proxy", "10m", "10Mi"})}}
	l := metrics.Build(in).List(metrics.Query{Level: metrics.LevelPods})
	r := l.Rows[0]
	if row := cells(t, l, "shop/web-1"); row["%CPU/L"] != "10%" || row["%MEM/L"] != "10%" {
		t.Fatalf("pod-level shares: %v", row)
	}
	if r.Heat != metrics.High || strings.Join(r.Flags, "; ") != "app: CPU 100% of limit; app: memory 99% of limit" {
		t.Fatalf("the pod row: heat %v, flags %v", r.Heat, r.Flags)
	}
	if text := l.Table(0); !strings.Contains(text, "<- HIGH: app: CPU 100% of limit, app: memory 99% of limit") {
		t.Fatalf("the model is not told:\n%s", text)
	}
}

// A pod only the metrics API knows about is on an unknown node — not on none.
func TestPodKnownOnlyToTheMetricsAPI(t *testing.T) {
	in := sample()
	in.PodMetrics = append(in.PodMetrics, podMetric("shop", "ghost", t0, [3]string{"c", "3", "1Gi"}))
	s := metrics.Build(in)
	l := s.List(metrics.Query{Level: metrics.LevelPods})
	g := cells(t, l, "shop/ghost")
	if g["NODE"] != "—" || g["CPU"] != "3" || g["%CPU/R"] != "—" || g["%CPU/L"] != "—" {
		t.Fatalf("ghost: %v", g)
	}
	if pending := cells(t, l, "shop/new-1"); pending["NODE"] != "<none>" {
		t.Fatalf("an unscheduled pod: %v", pending)
	}
	// Its usage is not divided by other pods' requests.
	if shop := cells(t, s.List(metrics.Query{Level: metrics.LevelNamespaces}), "shop"); shop["%CPU/R"] != "—" {
		t.Fatalf("shop: %v", shop)
	}
}

// Quantities that are not readings of anything are refused, not overflowed
// into something believable.
func TestAbsurdQuantitiesAreUnknown(t *testing.T) {
	in := metrics.Inputs{TakenAt: t0, Sources: allOK(),
		Nodes:       []unstructured.Unstructured{nodeObj("node-a", "4", "8Gi", true)},
		NodeMetrics: []unstructured.Unstructured{nodeMetric("node-a", "1E", "-5Gi", t0)},
		Pods:        []unstructured.Unstructured{podObj("shop", "big", "node-a", "Running", container("c", "1", "1", "1", "1"))},
		PodMetrics:  []unstructured.Unstructured{podMetric("shop", "big", t0, [3]string{"c", "100m", "1Ei"})}}
	s := metrics.Build(in)
	if a := cells(t, s.List(metrics.Query{Level: metrics.LevelNodes}), "node-a"); a["CPU"] != "—" || a["MEM"] != "—" || a["%CPU"] != "—" {
		t.Fatalf("node-a: %v", a)
	}
	row := cells(t, s.List(metrics.Query{Level: metrics.LevelPods}), "shop/big")
	if len(row["%MEM/L"]) > 8 || strings.HasPrefix(row["%MEM/L"], "-") {
		t.Fatalf("an overflowed percentage: %v", row)
	}
	// The largest values that are accepted still do not overflow.
	if p, ok := metrics.Percent(metrics.Known(1<<62), metrics.Known(1)); !ok || p != 999_999 {
		t.Fatalf("Percent = %d, %v", p, ok)
	}
	if _, ok := metrics.Percent(metrics.Known(-1), metrics.Known(5)); ok {
		t.Fatal("a negative reading has a percentage")
	}
}

// The readings are as old as the older of the two sources.
func TestSampleTimeIsTheOlderSource(t *testing.T) {
	in := sample()
	in.NodeMetrics = []unstructured.Unstructured{nodeMetric("node-a", "1", "1Gi", t0.Add(10*time.Minute))}
	s := metrics.Build(in)
	if !s.SampleTime.Equal(t0) {
		t.Fatalf("sample time %v: a fresh node sample vouches for ten-minute-old pod samples", s.SampleTime)
	}
	if f := s.Freshness(t0.Add(10*time.Minute + 5*time.Second)); !f.Warn || !strings.Contains(f.Text, "OLD") {
		t.Fatalf("%+v", f)
	}
}

// A namespace that was not read is not an empty namespace.
func TestNamespaceOutsideWhatWasRead(t *testing.T) {
	shop := metrics.Build(sample()).Narrow("shop")
	l := shop.List(metrics.Query{Level: metrics.LevelPods, Namespace: "ops"})
	if strings.Contains(l.Empty, "No pods") || !strings.Contains(l.Empty, "Namespace ops was not read") || !strings.Contains(l.Empty, "namespace shop only") {
		t.Fatalf("%q", l.Empty)
	}
}

// A name cannot pass itself off as another row of the table the model reads.
func TestTableTextCannotBeForged(t *testing.T) {
	in := sample()
	in.PodMetrics = append(in.PodMetrics, podMetric("shop", "x\nshop  innocent  0m  0%  0%\x1b[2J", t0, [3]string{"c", "1", "1Mi"}))
	text := metrics.Build(in).List(metrics.Query{Level: metrics.LevelPods, Namespace: "shop"}).Table(0)
	if strings.Count(text, "\n") != 5 || strings.Contains(text, "\x1b") {
		t.Fatalf("the table has a forged line or an escape sequence:\n%q", text)
	}
}

// WithPodsOf sets one namespace's pods beside the cluster's node readings.
func TestWithPodsOf(t *testing.T) {
	wideIn := sample()
	wideIn.Sources.PodMetrics = metrics.SourceStatus{Source: metrics.PodMetrics, State: metrics.Denied}
	wideIn.Sources.Pods = metrics.SourceStatus{Source: metrics.PodObjects, State: metrics.Denied}
	wide := metrics.Build(wideIn)

	podsIn := sample()
	podsIn.Scope, podsIn.Nodes, podsIn.NodeMetrics = "shop", nil, nil
	podsIn.TakenAt = wide.TakenAt.Add(-time.Second)
	got := wide.WithPodsOf(metrics.Build(podsIn))

	if got.Scope != "shop" || len(got.Pods) != 3 || len(got.Nodes) != 2 || !got.Sources.PodMetrics.OK() || !got.Sources.NodeMetrics.OK() {
		t.Fatalf("scope %q, %d pods, %d nodes, %+v", got.Scope, len(got.Pods), len(got.Nodes), got.Sources)
	}
	if a, _ := got.Node("node-a"); a.CPU != metrics.Millicores(1000) || a.Pods.OK {
		t.Fatalf("node-a: %+v", a)
	}
	if !got.TakenAt.Equal(podsIn.TakenAt) || len(wide.Pods) != 0 {
		t.Fatalf("taken at %v; the original now has %d pods", got.TakenAt, len(wide.Pods))
	}
}
