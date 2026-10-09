package tui

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/llm"
	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

// loadedCluster is two nodes and a handful of pods with known readings:
// node-b is hot, web-1's app container is at its limits, one pod is pending.
func loadedCluster() *kubetest.Fake {
	pending := kubetest.PodOn("shop", "web-pending", "", kubetest.Container("app", "100m", "", "64Mi", ""))
	pending.Status.Phase = corev1.PodPending
	f := kubetest.New(
		kubetest.Namespace("default"), kubetest.Namespace("shop"), kubetest.Namespace("ops"),
		kubetest.Node("node-a", "4", "8Gi"), kubetest.Node("node-b", "2", "4Gi"),
		kubetest.PodOn("shop", "web-1", "node-a",
			kubetest.Container("app", "250m", "500m", "256Mi", "512Mi"), kubetest.Container("sidecar", "50m", "", "32Mi", "")),
		kubetest.PodOn("shop", "db-0", "node-b", kubetest.Container("db", "500m", "1", "1Gi", "1Gi")),
		kubetest.PodOn("ops", "cron-1", "node-a", kubetest.Container("cron", "", "", "", "")),
		pending,
	)
	f.SetNodeMetrics("node-a", "1200m", "6Gi")
	f.SetNodeMetrics("node-b", "1900m", "3891Mi")
	f.SetPodMetrics("shop", "web-1", kubetest.Usage{Container: "app", CPU: "480m", Memory: "490Mi"}, kubetest.Usage{Container: "sidecar", CPU: "10m", Memory: "20Mi"})
	f.SetPodMetrics("shop", "db-0", kubetest.Usage{Container: "db", CPU: "156340215n", Memory: "524288Ki"})
	f.SetPodMetrics("ops", "cron-1", kubetest.Usage{Container: "cron", CPU: "0", Memory: "8Mi"})
	f.SetLogs("shop", "app", false, "app says hello\n")
	f.SetLogs("shop", "sidecar", false, "sidecar says hello\n")
	return f
}

// looksLikeAReading matches what a made-up zero would look like on screen.
var looksLikeAReading = regexp.MustCompile(`(^|[\s│])0(m|%|Mi|Gi)?([\s│]|$)`)

// noInventedReadings fails if the screen shows a table header or anything
// that reads as "zero usage".
func (u *ui) noInventedReadings() {
	u.t.Helper()
	body := u.browserPane()
	for _, header := range []string{"%CPU", "%MEM", "CPU-REQ"} {
		if strings.Contains(body, header) {
			u.t.Fatalf("a table (%s) is drawn although there are no readings:\n%s", header, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		// The tab bar counts things that really were counted.
		if strings.Contains(line, "1 nodes") {
			continue
		}
		if looksLikeAReading.MatchString(line) {
			u.t.Fatalf("something that reads as zero usage is on screen: %q\n%s", line, body)
		}
	}
}

// prose is the screen as running text: pane borders and the bars of the
// gauges removed, whitespace collapsed, so that a sentence can be looked for
// wherever the pane happened to wrap it.
func (u *ui) prose() string {
	s := strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ", "█", " ", "░", " ").Replace(u.screen())
	return strings.Join(strings.Fields(s), " ")
}

func (u *ui) wantProse(subs ...string) {
	u.t.Helper()
	text := u.prose()
	for _, sub := range subs {
		if !strings.Contains(text, sub) {
			u.t.Fatalf("screen does not say %q:\n%s", sub, u.screen())
		}
	}
}

// browserPane is the screen without the header and footer lines.
func (u *ui) browserPane() string {
	lines := strings.Split(u.screen(), "\n")
	if len(lines) < 3 {
		return ""
	}
	return strings.Join(lines[1:len(lines)-1], "\n")
}

// fitsTerminal fails if the layout is broken: the screen as a whole must fit
// the terminal, and — the stricter half — what the metrics screen draws must
// fit its pane by itself. The pane would clip anything too wide or too long,
// so an overflow would not show as a broken frame; it would show as a line
// wrapped by the frame, pushing the rows below it out of sight.
func (u *ui) fitsTerminal() {
	u.t.Helper()
	lines := strings.Split(u.screen(), "\n")
	if len(lines) > u.m.height {
		u.t.Fatalf("%d lines on a %d-row terminal:\n%s", len(lines), u.m.height, u.screen())
	}
	for _, l := range lines {
		if w := lipgloss.Width(l); w > u.m.width {
			u.t.Fatalf("a %d-cell line on a %d-column terminal: %q\n%s", w, u.m.width, l, u.screen())
		}
	}
	if u.m.view != vMetrics || u.m.copilotMax {
		return
	}
	w, h := u.m.browserInner()
	own := strings.Split(ansi.Strip(u.m.viewMetrics(w, h)), "\n")
	if len(own) > h {
		u.t.Fatalf("the metrics screen draws %d lines into a pane of %d:\n%s", len(own), h, strings.Join(own, "\n"))
	}
	for _, l := range own {
		if got := lipgloss.Width(l); got > w {
			u.t.Fatalf("the metrics screen draws a %d-cell line into a pane of %d: %q", got, w, l)
		}
	}
}

func forbiddenUnless(allowed func(group, resource, namespace string) bool) clienttesting.ReactionFunc {
	return func(a clienttesting.Action) (bool, runtime.Object, error) {
		gvr := a.GetResource()
		if allowed(gvr.Group, gvr.Resource, a.GetNamespace()) {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("RBAC: access denied"))
	}
}

// The big picture, then down to one container, and back.
func TestMetricsScreenDrillDown(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil)
	u.keys("M")

	// Nodes, with the cluster's totals above them, busiest first.
	u.want("metrics · cluster", "sampled", "15s window",
		"CPU 3.1 of 6 allocatable (52%)", "memory 9.8Gi of 12Gi allocatable (82%)", "2 nodes · 4 pods in all namespaces",
		"1 nodes (2)", "2 namespaces (2)", "3 pods (4)", "by cpu ↓",
		"NODE", "%CPU", "%MEM", "PODS",
		"node-b  1.9   95%  ██████████  3.8Gi   95%  ██████████     1",
		"node-a  1.2   30%  ███░░░░░░░    6Gi   75%  ████████░░     2")
	if u.m.view != vMetrics || u.m.metrics.selectedKey() != "node-b" {
		t.Fatalf("view %v, selected %q", u.m.view, u.m.metrics.selectedKey())
	}
	if fo := u.m.deps.Focus.Get(); fo.Type != "nodes" || fo.Kind != "Node" || fo.Selected != "node-b" || !strings.Contains(fo.View, "metrics screen, listing nodes by cpu") {
		t.Fatalf("focus on the nodes tab: %+v", fo)
	}

	// Namespaces.
	u.keys("2")
	u.want("NAMESPACE", "%CPU/R", "shop          3  647m     72%  1022Mi     74%", "ops           1    0m  no req     8Mi  no req")

	// Into a namespace: its pods, against its requests.
	u.keys("enter")
	u.want("metrics · cluster › namespace shop", "namespace shop", "CPU 647m of 900m requested (72%)", "pods in namespace shop · 3",
		"web-1        490m    163%  no lim  510Mi    177%  no lim  node-a",
		"web-pending     —       —       —      —       —       —  <none>",
		"1 pod shown as — (1 not running, so there is nothing to measure). That is unknown, not zero.")
	u.wantNot("cron-1", "NAMESPACE ")
	if fo := u.m.deps.Focus.Get(); fo.Type != "pods" || fo.Selected != "web-1" || fo.SelectedNamespace != "shop" || !strings.Contains(fo.View, "pods in namespace shop") {
		t.Fatalf("focus in a namespace: %+v", fo)
	}

	// Into a pod: its containers, with requests, limits and what is near them.
	u.keys("enter")
	u.want("metrics · cluster › namespace shop › pod shop/web-1", "pod shop/web-1", "CPU 490m, no limit, 300m requested",
		"on node node-a · Running", "containers of pod shop/web-1 · 2",
		"CONTAINER", "CPU-REQ", "%MEM/L", "RESTARTS", "LAST-EXIT",
		"app        480m     250m     500m     96%  490Mi    256Mi    512Mi     96%         0  -",
		"sidecar     10m      50m     none  no lim   20Mi     32Mi     none  no lim         0  -")
	if fo := u.m.deps.Focus.Get(); fo.Selected != "web-1" || fo.SelectedNamespace != "shop" || !strings.Contains(fo.View, "container app highlighted") {
		t.Fatalf("focus on a container: %+v", fo)
	}
	u.keys("enter")
	u.want("a container is as far down as it goes")

	// Back up, one level at a time, landing where one came from.
	u.keys("down", "esc")
	u.want("pods in namespace shop · 3")
	if u.m.metrics.selectedKey() != "shop/web-1" {
		t.Fatalf("back from a pod landed on %q", u.m.metrics.selectedKey())
	}
	u.keys("esc")
	u.want("2 namespaces (2)", "NAMESPACE")
	if u.m.metrics.selectedKey() != "shop" || u.m.view != vMetrics {
		t.Fatalf("back from a namespace landed on %q in view %v", u.m.metrics.selectedKey(), u.m.view)
	}
	// From the top, esc leaves — with the browser as it was.
	u.keys("esc")
	u.want("pods · namespace shop · 3")
	if u.m.view != vList || u.m.curNS != "shop" {
		t.Fatalf("view %v ns %q", u.m.view, u.m.curNS)
	}

	// The other way down: a node's pods. 1 2 3 jump from anywhere.
	u.keys("M")
	u.want("2 namespaces (2)", "NAMESPACE") // the tab last used
	u.keys("1", "down", "enter")
	u.want("metrics · cluster › node node-a", "node node-a", "CPU 1.2 of 4 allocatable (30%)", "Ready · 2 pods using CPU 490m, memory 518Mi",
		"pods on node node-a · 2", "shop       web-1", "ops        cron-1")
	u.wantNot("db-0")
	u.keys("3")
	u.want("3 pods (4)", "db-0", "cron-1")
	if len(u.m.metrics.path) != 0 {
		t.Fatal("a tab key did not return to the top level")
	}
	u.keys("left")
	u.want("NAMESPACE  PODS")
	u.keys("right", "right")
	u.want("NODE    CPU")

	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("the metrics screen changed something: %v", got)
	}
}

func TestMetricsSortFilterAndStickyCursor(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil)
	u.keys("M", "3")
	order := func() []string {
		var keys []string
		for _, r := range u.m.metrics.list.Rows {
			keys = append(keys, r.Key)
		}
		return keys
	}
	if got := strings.Join(order(), " "); got != "shop/web-1 shop/db-0 ops/cron-1 shop/web-pending" {
		t.Fatalf("by cpu: %s", got)
	}
	u.keys("down") // db-0
	u.keys("s")
	u.want("by memory ↓")
	if got := strings.Join(order(), " "); got != "shop/db-0 shop/web-1 ops/cron-1 shop/web-pending" {
		t.Fatalf("by memory: %s", got)
	}
	if u.m.metrics.selectedKey() != "shop/db-0" {
		t.Fatalf("re-sorting moved the selection to %q", u.m.metrics.selectedKey())
	}
	u.keys("s")
	u.want("by name")
	u.wantNot("by name ↓")
	u.keys("s")
	u.want("by cpu ↓")

	// A refresh that reorders the rows leaves the cursor on the same pod.
	f.SetPodMetrics("shop", "db-0", kubetest.Usage{Container: "db", CPU: "900m", Memory: "600Mi"})
	u.keys("r")
	if got := order(); got[0] != "shop/db-0" || u.m.metrics.selectedKey() != "shop/db-0" {
		t.Fatalf("after refresh: order %v, selected %q", got, u.m.metrics.selectedKey())
	}
	u.want("db-0         900m    180%     90%  600Mi     59%     59%  node-b")

	u.keys("/")
	u.typeText("web")
	u.want("/web", "2 of 4", "web-1", "web-pending")
	u.wantNot("cron-1", "db-0 ")
	u.keys("enter")
	if u.m.filtering || u.m.metrics.filter != "web" || u.m.filter != "" {
		t.Fatalf("filter state: filtering=%v metrics=%q list=%q", u.m.filtering, u.m.metrics.filter, u.m.filter)
	}
	u.keys("/")
	u.typeText("zzz")
	u.keys("enter")
	u.want(`Nothing matches the filter "webzzz"`)
	u.keys("esc") // clears the filter, stays on the screen
	u.want("cron-1", "db-0")
	if u.m.view != vMetrics || u.m.metrics.filter != "" {
		t.Fatalf("esc with a filter: view %v filter %q", u.m.view, u.m.metrics.filter)
	}
}

// From a hot pod straight to its logs, events and detail, and back to where
// one was.
func TestMetricsOpensLogsEventsDetail(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	u.keys("M", "3")
	u.keys("l")
	u.want("logs · Pod shop/web-1", "app says hello")
	if fo := u.m.deps.Focus.Get(); fo.Selected != "web-1" || fo.View != "logs" {
		t.Fatalf("focus in logs: %+v", fo)
	}
	u.keys("esc")
	u.want("metrics · cluster", "3 pods (4)")
	if u.m.view != vMetrics || u.m.metrics.selectedKey() != "shop/web-1" {
		t.Fatalf("back from logs: view %v, selected %q", u.m.view, u.m.metrics.selectedKey())
	}

	// From a container, the logs open on that container.
	u.keys("enter", "down", "l")
	u.want("logs · Pod shop/web-1", "container sidecar (2/2)", "sidecar says hello")
	u.keys("esc")
	u.want("containers of pod shop/web-1")

	u.keys("d")
	u.want("detail · Pod shop/web-1", "kind: Pod")
	u.keys("esc")
	u.keys("e")
	u.want("events · Pod shop/web-1")
	u.keys("esc", "esc", "1")

	u.keys("d")
	u.want("detail · Node node-b", "kind: Node")
	u.keys("esc")
	u.keys("l")
	u.want("logs belong to pods")
	if u.m.view != vMetrics {
		t.Fatalf("view %v", u.m.view)
	}
}

// Each way the metrics source can be missing is said plainly and differently
// — and none of them is an empty table or a zero.
func TestMetricsUnavailableIsSaidPlainly(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		u := newUI(t, fakeCluster(), nil) // serves no metrics API, like a cluster without metrics-server
		u.keys("M")
		u.want("metrics · cluster", "Metrics are not available on this cluster",
			"does not serve the metrics API (metrics.k8s.io)", "metrics-server", "it installs nothing",
			"The server said: the server could not find the requested resource", "r tries again")
		// Said once, by the body — and the footer offers only what works here.
		u.wantNot("totals unknown", "drill down", "sort", "filter")
		u.want("r try again", "esc back")
		u.noInventedReadings()
		for _, tab := range []string{"2", "3"} {
			u.keys(tab)
			u.want("Metrics are not available on this cluster")
			u.noInventedReadings()
		}
		// Nothing to drill into, and saying so does not break anything.
		u.keys("enter", "s", "down", "l", "d")
		u.want("Metrics are not available on this cluster")
		// The rest of the tool is unaffected.
		u.keys("esc")
		u.want("pods · namespace shop · 2", "web-1")
	})

	t.Run("registered but broken", func(t *testing.T) {
		f := loadedCluster()
		f.FailMetrics(apierrors.NewServiceUnavailable("the server is currently unable to handle the request"))
		u := newUI(t, f, nil)
		u.keys("M")
		u.wantProse("The metrics API is not answering", "registered on this cluster but nothing healthy is answering", "metrics-server is down",
			"it is not zero", "currently unable to handle the request")
		u.wantNot("Metrics are not available on this cluster")
		u.noInventedReadings()
	})

	t.Run("not permitted", func(t *testing.T) {
		f := loadedCluster()
		f.FailMetrics(apierrors.NewForbidden(kubetest.PodMetricsGVR.GroupResource(), "", errors.New(`User "dev" cannot list resource "pods" in API group "metrics.k8s.io"`)))
		u := newUI(t, f, nil)
		u.keys("M")
		u.want("Permission denied reading node metrics", "authorization failure, not an idle cluster", `User "dev" cannot list`)
		u.keys("3")
		u.want("Permission denied reading pod metrics across all namespaces", "Namespace shop could not be read either")
		u.wantNot("Metrics are not available", "not answering")
		u.noInventedReadings()
	})

	t.Run("recovers", func(t *testing.T) {
		f := loadedCluster()
		f.FailMetrics(apierrors.NewServiceUnavailable("starting"))
		u := newUI(t, f, nil)
		u.keys("M")
		u.want("The metrics API is not answering")
		f.FailMetrics(nil)
		u.keys("r")
		u.want("node-b  1.9   95%")
		u.wantNot("not answering")
	})
}

// Someone who may see pods in their namespace but not metrics for the whole
// cluster (nor nodes) still gets a screen — their namespace — and is told
// what they are not seeing.
func TestMetricsWithPartialPermissions(t *testing.T) {
	f := loadedCluster()
	f.Dynamic.PrependReactor("list", "*", forbiddenUnless(func(group, resource, namespace string) bool {
		return group != "metrics.k8s.io" && resource != "pods" && resource != "nodes" || resource == "pods" && namespace == "shop"
	}))
	u := newUI(t, f, nil) // the browser is in namespace shop
	u.keys("M")

	// Nodes: refused, and said to be refused.
	u.want("Permission denied reading node metrics", "authorization failure, not an idle cluster", "Pod metrics are readable: press 2 or 3")
	u.noInventedReadings()

	// Pods: the namespace that can be read, with the refusal on show.
	u.keys("3")
	u.want("web-1        490m    163%  no lim  510Mi    177%  no lim  node-a", "db-0",
		"Permission denied reading pod metrics across all namespaces: showing namespace shop only")
	u.wantNot("cron-1")
	if u.m.metrics.scope != "shop" || u.m.metrics.narrowed == nil {
		t.Fatalf("scope %q, narrowed %v", u.m.metrics.scope, u.m.metrics.narrowed)
	}
	if fo := u.m.deps.Focus.Get(); fo.Namespace != "shop" || fo.AllNamespaces {
		t.Fatalf("focus does not say the screen is limited to a namespace: %+v", fo)
	}
	// The summary cannot give cluster totals, and does not pretend to.
	u.want("totals unknown — permission denied reading node metrics")

	// Pods visible, metrics not: the pod listing still works, the metrics screen says why it cannot.
	f2 := loadedCluster()
	f2.Dynamic.PrependReactor("list", "*", forbiddenUnless(func(group, resource, namespace string) bool { return group != "metrics.k8s.io" }))
	u2 := newUI(t, f2, nil)
	u2.want("pods · namespace shop · 3", "web-1")
	u2.keys("M", "3")
	u2.want("Permission denied reading pod metrics", "the readings exist, you may not see them")
	u2.noInventedReadings()
	u2.keys("esc")
	u2.want("pods · namespace shop · 3")
}

// A refresh that fails keeps the last readings on show, visibly marked as
// not current, with the cause.
func TestMetricsRefreshFailureIsMarked(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil)
	u.keys("M")
	u.want("node-b  1.9   95%", "sampled")

	f.FailMetrics(apierrors.NewServiceUnavailable("metrics-server is down"))
	u.keys("r")
	u.want("node-b  1.9   95%")                                       // still there…
	u.wantProse("refresh failing since", "showing the readings from", // …and said to be old
		"Not current: The metrics API is not answering (metrics-server is down). These are the last readings that were obtained.")
	u.wantNot("sampled ")

	f.FailMetrics(nil)
	u.keys("r")
	u.want("node-b  1.9   95%", "sampled")
	u.wantNot("Not current", "refresh failing")
}

// The screen refreshes itself — while it is visible, at the pace the source
// can keep up with, and never on top of a read that is still out.
func TestMetricsRefreshesItself(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, func(d *Deps) { d.Refresh = 5 * time.Second })
	if u.m.metricsInterval() != minMetricsInterval {
		t.Fatalf("interval %v", u.m.metricsInterval())
	}
	tick := func() { u.send(tickMsg{}) }

	// Not open: ticks do not touch the metrics API.
	tick()
	if f.MetricsCalls() != 0 {
		t.Fatal("metrics were read while the screen was closed")
	}
	u.keys("M")
	calls := f.MetricsCalls()
	u.want("sampled", "node-b  1.9")
	u.wantNot("auto-refresh off")

	// Too soon: the source has nothing new yet.
	tick()
	if f.MetricsCalls() != calls {
		t.Fatal("re-read before the interval elapsed")
	}
	// Due: new numbers appear by themselves. (In real use the interval has
	// passed and the readings have aged with it; here both are made so.)
	f.SetNodeMetrics("node-b", "400m", "1Gi")
	u.m.metrics.asked = time.Now().Add(-minMetricsInterval)
	u.m.metrics.snap.TakenAt = time.Now().Add(-time.Minute)
	if f.MetricsCalls() != calls {
		t.Fatal("setting the test up read the metrics API")
	}
	tick()
	if f.MetricsCalls() != calls+2 {
		t.Fatalf("when due: %d metrics requests, want 2 (nodes and pods, once)", f.MetricsCalls()-calls)
	}
	u.want("node-b  400m   20%")

	// Hidden behind the maximized copilot, or left: no polling.
	calls = f.MetricsCalls()
	u.m.metrics.asked = time.Now().Add(-time.Hour)
	u.keys("m")
	tick()
	u.keys("esc", "esc")
	u.m.metrics.asked = time.Now().Add(-time.Hour)
	tick()
	if f.MetricsCalls() != calls || u.m.view != vList {
		t.Fatalf("polled while not visible (%d → %d calls), view %v", calls, f.MetricsCalls(), u.m.view)
	}

	// With refresh switched off the screen says so.
	off := newUI(t, loadedCluster(), nil)
	off.keys("M")
	off.want("auto-refresh off")
}

// A metrics API that hangs does not hang the interface, is not asked again
// while it hangs, and is not drawn as an empty cluster meanwhile.
//
// (That other requests to the cluster carry on meanwhile is shown with a real
// HTTP client in kube.TestMetricsOverTheWire: client-go's fake serialises
// every call behind one lock, so it cannot be shown here.)
func TestMetricsSlowSourceDoesNotBlock(t *testing.T) {
	f := loadedCluster()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.Dynamic.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group == "metrics.k8s.io" {
			<-release
		}
		return false, nil, nil
	})
	u := newUI(t, f, func(d *Deps) { d.Refresh = 5 * time.Second })

	start := time.Now()
	u.keys("M")
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("opening the screen blocked for %v", waited)
	}
	u.want("metrics · cluster", "Reading metrics from the cluster…")
	u.noInventedReadings()

	// Ticks while it is still out do not start more reads.
	seq := u.m.metrics.seq
	u.m.metrics.asked = time.Now().Add(-time.Hour)
	u.send(tickMsg{})
	u.send(tickMsg{})
	if u.m.metrics.seq != seq || !u.m.metrics.loading {
		t.Fatalf("reads piled up: seq %d → %d", seq, u.m.metrics.seq)
	}

	// The interface is live: keys are handled, other views open, and the
	// way out works.
	start = time.Now()
	u.keys("down", "2", "?")
	u.want("METRICS (M)")
	u.keys("esc")
	u.want("Reading metrics from the cluster…")
	u.keys("esc")
	u.want("pods · namespace shop · 3", "web-1")
	if u.m.view != vList {
		t.Fatalf("view %v", u.m.view)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("five key presses took %v with the metrics API hanging", waited)
	}
}

// Too small, small, and narrow beside the copilot: readable, never broken.
func TestMetricsOnSmallTerminals(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func() *kubetest.Fake
	}{{"with readings", loadedCluster}, {"without a metrics API", fakeCluster}} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUI(t, tc.f(), nil)
			u.keys("M")
			for _, size := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {72, 18}, {MinWidth, MinHeight}} {
				u.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				for _, keys := range [][]string{{"1"}, {"2"}, {"3"}, {"3", "enter"}, {"1", "enter"}} {
					u.keys(keys...)
					u.fitsTerminal()
					u.want("metrics")
					// Beside the copilot the pane is as narrow as 24 cells.
					u.keys("c")
					u.fitsTerminal()
					u.keys("c")
				}
			}
			// Below the minimum: the tool's usual message, not a broken screen.
			u.send(tea.WindowSizeMsg{Width: MinWidth - 1, Height: MinHeight})
			u.want("Terminal too small")
			u.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight - 1})
			u.want("Terminal too small")
			u.send(tea.WindowSizeMsg{Width: 120, Height: 40})
			u.want("metrics · cluster")
		})
	}

	// Smaller than any pane the layout hands out today, long names, every
	// level: the renderer itself never draws outside what it is given, and
	// never panics on a size it cannot do anything useful with.
	long := loadedCluster()
	long.SetPodMetrics("a-namespace-with-quite-a-long-name-indeed", "a-pod-with-a-name-as-long-as-kubernetes-allows-them-to-be-63chr",
		kubetest.Usage{Container: "a-container-with-a-long-name-too", CPU: "123456m", Memory: "1023999Mi"})
	long.SetNodeMetrics("ip-10-123-456-789.eu-central-1.compute.internal", "123456m", "1023999Mi")
	for _, f := range []*kubetest.Fake{long, fakeCluster()} {
		u := newUI(t, f, nil)
		u.keys("M")
		for _, keys := range [][]string{{"1"}, {"2"}, {"3"}, {"3", "enter"}, {"1", "enter"}} {
			u.keys(keys...)
			for _, w := range []int{0, 1, 3, 7, 12, 19, 24, 33} {
				for _, h := range []int{0, 1, 2, 4, 9, 12} {
					lines := strings.Split(ansi.Strip(u.m.viewMetrics(w, h)), "\n")
					if len(lines) > max(h, 1) {
						t.Fatalf("%dx%d after %v: %d lines\n%s", w, h, keys, len(lines), strings.Join(lines, "\n"))
					}
					for _, l := range lines {
						if got := lipgloss.Width(l); got > w {
							t.Fatalf("%dx%d after %v: a %d-cell line: %q", w, h, keys, got, l)
						}
					}
				}
			}
		}
	}

	// At the minimum size the essentials are still there.
	u := newUI(t, loadedCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight})
	u.keys("M")
	u.want("metrics · cluster", "old", "CPU 3.1/6 alloc (52%)", "NODE", "%CPU", "%MEM", "node-b", "95%")
	u.keys("c") // 24 cells beside the copilot: shares and age survive
	u.want("metrics", "old", "CPU 52%", "memory 82%", "node-b", "95%")
	u.keys("c", "3")
	u.want("POD", "CPU", "MEM", "web-1", "490m", "510Mi")

	nope := newUI(t, fakeCluster(), nil)
	nope.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight})
	nope.keys("M", "c")
	nope.want("Metrics are not", "available on this", "does not")
	nope.noInventedReadings()
}

// Names and server messages come from the cluster. None of it gets to drive
// the terminal.
func TestMetricsTextIsSanitized(t *testing.T) {
	f := loadedCluster()
	f.SetPodMetrics("shop", "evil\x1b[2J\x1b]0;pwned\x07pod", kubetest.Usage{Container: "c\x1b[31m", CPU: "1", Memory: "1Mi"})
	u := newUI(t, f, nil)
	hostile := func(where string) {
		t.Helper()
		// The raw output, not the stripped one: stripping would hide exactly
		// what is being looked for.
		if raw := u.m.View(); strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]0;") || strings.Contains(raw, "\x1b[31m") {
			t.Fatalf("%s: a name carried an escape sequence to the screen", where)
		}
	}
	u.keys("M", "3")
	u.want("evilpod")
	hostile("the pods listing")
	u.keys("2")
	hostile("the namespaces listing")
	u.keys("3", "enter")
	hostile("the containers listing")

	f = loadedCluster()
	f.FailMetrics(apierrors.NewServiceUnavailable("down\x1b[2J\x1b]0;pwned\x07"))
	u = newUI(t, f, nil)
	u.keys("M")
	if raw := u.m.View(); strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]0;") {
		t.Fatal("a server message carried an escape sequence to the screen")
	}
	u.want("The server said: down")
}

// The whole point of the shared model: what the copilot is told is what is
// on the screen — the same readings, read once.
func TestCopilotSeesWhatTheScreenShows(t *testing.T) {
	f := loadedCluster()
	c := newCopilotUIOn(t, f,
		llm.Call(
			llm.ToolCall{ID: "m1", Name: "get_metrics", Args: []byte(`{"level":"nodes"}`)},
			llm.ToolCall{ID: "m2", Name: "get_metrics", Args: []byte(`{"level":"pods","namespace":"shop"}`)},
			llm.ToolCall{ID: "m3", Name: "get_metrics", Args: []byte(`{"level":"containers","namespace":"shop","pod":"web-1"}`)},
		),
		llm.Text("node-b is at 95% CPU."))
	c.keys("M")
	calls := f.MetricsCalls()

	// The numbers move after the screen has drawn them. The copilot must be
	// given what the user is looking at, not a second, different read.
	f.SetNodeMetrics("node-b", "100m", "100Mi")
	f.SetPodMetrics("shop", "web-1", kubetest.Usage{Container: "app", CPU: "1m", Memory: "1Mi"})

	c.ask("why is this node busy?")
	c.eventually("the answer", func() bool { return !c.m.busy })
	if f.MetricsCalls() != calls {
		t.Fatalf("the copilot re-read the metrics API (%d → %d calls) instead of using the readings on screen", calls, f.MetricsCalls())
	}
	if first := c.stub.Requests[0].Messages[0].Text; !strings.Contains(first, "view: metrics screen, listing nodes by cpu") || !strings.Contains(first, "selected resource: Node node-b") {
		t.Fatalf("the request does not carry the metrics focus:\n%s", first)
	}

	var results []llm.ToolResult
	for _, m := range c.stub.Requests[1].Messages {
		results = append(results, m.ToolResults...)
	}
	if len(results) != 3 {
		t.Fatalf("tool results: %+v", results)
	}
	c.keys("tab") // back to the browser pane

	// Every row the model was given is on the screen, cell for cell.
	sameRows := func(result llm.ToolResult, header string) {
		t.Helper()
		if result.IsError {
			t.Fatalf("tool error: %s", result.Content)
		}
		_, table, found := strings.Cut(result.Content, header)
		if !found {
			t.Fatalf("no %q table in the tool result:\n%s", header, result.Content)
		}
		screen := c.prose()
		rows := 0
		for _, line := range strings.Split(header+table, "\n") {
			if line == "" {
				break
			}
			row, _, _ := strings.Cut(line, "  <- ") // the flag is the screen's colour
			row = strings.TrimPrefix(row, "note: ") // caveats are on the screen too, below the table
			if !strings.Contains(screen, strings.Join(strings.Fields(row), " ")) {
				t.Errorf("the model was told %q, which is not on the screen:\n%s", row, c.screen())
			}
			rows++
		}
		if rows < 2 {
			t.Fatalf("no rows compared for %q", header)
		}
	}
	summary := func(result llm.ToolResult, subs ...string) {
		t.Helper()
		for _, sub := range subs {
			if !strings.Contains(result.Content, sub) {
				t.Errorf("tool result lacks %q:\n%s", sub, result.Content)
			}
		}
		c.want(subs...)
	}

	c.send(tea.WindowSizeMsg{Width: 200, Height: 50})
	c.keys("c") // hide the copilot: the whole width for the table
	sameRows(results[0], "NODE ")
	summary(results[0], "CPU 3.1 of 6 allocatable (52%)", "memory 9.8Gi of 12Gi allocatable (82%)")
	c.keys("2", "enter") // namespace shop, the busiest
	sameRows(results[1], "POD ")
	summary(results[1], "CPU 647m of 900m requested (72%)")
	c.keys("enter") // web-1
	sameRows(results[2], "CONTAINER ")
	summary(results[2], "CPU 490m, no limit, 300m requested")

	// What is "hot" is the same thing in both: every row the model is told
	// is HIGH or elevated is drawn in that colour on the screen, selected or
	// not (TestSelectedRowKeepsItsHeat), because both take it from the row.
	c.keys("1")
	for _, row := range c.m.metrics.list.Rows {
		flagged := strings.Contains(results[0].Content, row.Name+" ") && strings.Contains(results[0].Content, "<- "+row.Heat.String()+":")
		if (row.Heat != metrics.Calm) != flagged {
			t.Errorf("%s: heat %q on the screen, flagged for the model: %v\n%s", row.Key, row.Heat, flagged, results[0].Content)
		}
		want := colBad
		if row.Heat == metrics.Elevated {
			want = colWarn
		}
		if row.Heat != metrics.Calm && metricsRowStyle(row.Heat, true).GetForeground() != want {
			t.Errorf("%s is %s for the model but would not be drawn so", row.Key, row.Heat)
		}
	}
	if !strings.Contains(results[0].Content, "<- HIGH: CPU 95% of allocatable") {
		t.Fatalf("node-b is not flagged for the model:\n%s", results[0].Content)
	}
}

// With no metrics API the copilot and the screen say the same thing.
func TestCopilotAndScreenAgreeWhenMetricsAreMissing(t *testing.T) {
	c := newCopilotUIOn(t, fakeCluster(),
		llm.Call(llm.ToolCall{ID: "m1", Name: "get_metrics", Args: []byte(`{"level":"pods","namespace":"shop"}`)}),
		llm.Text("I cannot see usage here: this cluster has no metrics API."))
	c.keys("M", "3")
	c.ask("is anything busy?")
	c.eventually("the answer", func() bool { return !c.m.busy })

	var result llm.ToolResult
	for _, m := range c.stub.Requests[1].Messages {
		for _, r := range m.ToolResults {
			result = r
		}
	}
	if !result.IsError {
		t.Fatalf("the model was given readings: %s", result.Content)
	}
	for _, same := range []string{"Metrics are not available on this cluster", "does not serve the metrics API (metrics.k8s.io)"} {
		if !strings.Contains(result.Content, same) {
			t.Errorf("the model was not told %q:\n%s", same, result.Content)
		}
	}
	c.keys("tab", "c")
	c.want("Metrics are not available on this cluster", "does not serve the metrics API (metrics.k8s.io)")
	// The failure is in the transcript as a failure, not as an empty result.
	c.keys("c")
	c.wantChat("get_metrics(level=pods, namespace=shop) → metrics unavailable")
}

func TestMetricsKeyIsEverywhereAndQuitWorks(t *testing.T) {
	for _, open := range [][]string{nil, {"enter"}, {"l"}, {"e"}, {"?"}, {"A"}} {
		u := newUI(t, loadedCluster(), nil)
		u.keys(open...)
		u.keys("M")
		if u.m.view != vMetrics {
			t.Errorf("M after %v: view %v", open, u.m.view)
		}
		u.want("metrics · cluster")
	}
	for _, in := range [][]string{{"M"}, {"M", "3", "enter"}} {
		for _, quit := range []string{"q", "ctrl+c"} {
			u := newUI(t, loadedCluster(), nil)
			u.keys(in...)
			_, cmd := u.m.Update(keyMsg(quit))
			if cmd == nil {
				t.Fatalf("%s does not quit from %v", quit, in)
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("%s does not quit from %v", quit, in)
			}
		}
	}
	// Typed into the copilot, M is a letter.
	u := newUI(t, loadedCluster(), nil)
	u.keys("tab")
	u.typeText("Memory?")
	if u.m.view != vList || u.m.input.Value() != "Memory?" {
		t.Fatalf("view %v, input %q", u.m.view, u.m.input.Value())
	}
	// The key is advertised.
	u.keys("esc")
	u.want("M metrics")
	u.keys("?")
	u.want("METRICS (M)", "It never means zero")
}

// --- found by the independent review -------------------------------------------

// The copilot had to read the cluster because what the screen held was too
// old to hand out (refresh off, or a long interval). The screen takes up
// those same readings, so that what the model quotes is what is on show —
// without asking the cluster again.
func TestScreenAdoptsWhatTheCopilotRead(t *testing.T) {
	f := loadedCluster()
	u := newUI(t, f, nil) // refresh switched off
	u.keys("M")
	u.want("node-b  1.9   95%")

	// Time passes; the cluster moves on; the screen, with refresh off, does
	// not know.
	u.m.metrics.snap.TakenAt = time.Now().Add(-20 * time.Second)
	f.SetNodeMetrics("node-b", "400m", "1Gi")
	calls := f.MetricsCalls()

	// The copilot asks. What the screen holds is too old to hand out, so the
	// tool reads the cluster.
	told, err := tools.NewRegistry(f.Cluster, tools.DefaultOptions()).Read(context.Background(), "get_metrics", []byte(`{"level":"nodes"}`))
	if err != nil || !strings.Contains(told, "node-b  400m   20%") {
		t.Fatalf("the model was not given the fresh reading: %v\n%s", err, told)
	}
	u.want("node-b  1.9   95%") // the screen is, for this instant, behind

	// The tool call ends. Deliver that the way the runtime does: run what the
	// update returns.
	_, cmd := u.m.Update(agentEventMsg{agent.EventToolEnd{Call: llm.ToolCall{Name: "get_metrics", Args: []byte(`{"level":"nodes"}`)}, Result: told}})
	adopted := false
	if batch, ok := cmdMsg(cmd).(tea.BatchMsg); ok {
		for _, sub := range batch {
			if msg, ok := cmdMsg(sub).(metricsMsg); ok {
				u.m.Update(msg)
				adopted = true
			}
		}
	}
	if !adopted {
		t.Fatal("the end of a get_metrics call did not make the screen take up the readings")
	}
	u.want("node-b  400m   20%")
	u.wantNot("node-b  1.9   95%")
	if got := f.MetricsCalls() - calls; got != 2 {
		t.Fatalf("%d metrics requests: the screen should take the copilot's readings (2, read once), not read again", got)
	}

	// Any other tool finishing, or the screen not being up, changes nothing.
	_, cmd = u.m.Update(agentEventMsg{agent.EventToolEnd{Call: llm.ToolCall{Name: "get_logs"}}})
	if _, isBatch := cmdMsg(cmd).(tea.BatchMsg); isBatch {
		t.Fatal("a tool that is not get_metrics made the screen re-read")
	}
	u.keys("esc")
	if u.m.adoptMetrics() != nil {
		t.Fatal("the metrics screen is not showing, yet it re-reads")
	}
}

// cmdMsg runs a command that returns promptly and gives its message; nil for
// no command or one that blocks (a listener, a timer).
func cmdMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

// The age on the screen keeps counting on a screen nobody touches — also
// with refresh switched off, when nothing else would redraw it.
func TestMetricsAgeKeepsCounting(t *testing.T) {
	u := newUI(t, loadedCluster(), nil) // Refresh 0: no ticks at all
	_, cmd := u.m.Update(keyMsg("M"))
	if cmd == nil {
		t.Fatal("opening the screen armed nothing")
	}
	u.pump(cmd)
	gen := u.m.metrics.clock
	if gen == 0 {
		t.Fatal("no redraw timer was started")
	}
	// While the screen is up the timer re-arms itself…
	if u.m.onMetricsClock(metricsClockMsg{gen}) == nil {
		t.Fatal("the redraw timer stopped while the screen is showing")
	}
	// …and what it redraws is older, and eventually says so.
	u.want("sampled")
	u.wantNot("OLD")
	u.m.metrics.snap.SampleTime = time.Now().Add(-4 * time.Minute)
	u.m.Update(metricsClockMsg{gen})
	u.want("sampled 4m", "OLD")

	// A timer from before is retired by a newer one, and none runs once the
	// screen is left: one timer, or none.
	u.keys("3", "l") // into a pod's logs
	if u.m.onMetricsClock(metricsClockMsg{gen}) != nil {
		t.Fatal("the redraw timer runs while another view is showing")
	}
	u.keys("esc") // back: a new timer
	if u.m.metrics.clock == gen || u.m.onMetricsClock(metricsClockMsg{gen}) != nil {
		t.Fatal("a retired timer is still honoured")
	}
	if u.m.onMetricsClock(metricsClockMsg{u.m.metrics.clock}) == nil {
		t.Fatal("no timer after returning to the screen")
	}
}

// M from a view that was opened from the metrics screen goes back to the
// screen as it was left, rather than opening a second one at the top.
func TestMetricsKeyReturnsToTheOpenScreen(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	u.keys("M", "2", "enter", "enter") // shop → web-1's containers
	u.want("containers of pod shop/web-1")
	u.keys("d")
	u.want("detail · Pod shop/web-1")
	u.keys("M")
	u.want("containers of pod shop/web-1")
	if len(u.m.metrics.path) != 2 || u.m.view != vMetrics {
		t.Fatalf("path %v, view %v", u.m.metrics.path, u.m.view)
	}
	u.keys("l", "M")
	u.want("containers of pod shop/web-1")
	// And the way out is as short as before: no extra screens were stacked.
	u.keys("esc", "esc", "esc")
	if u.m.view != vList || len(u.m.stack) != 0 {
		t.Fatalf("view %v, stack %v", u.m.view, u.m.stack)
	}
}

// The row under the cursor keeps its heat colour: by default that is the
// busiest row.
func TestSelectedRowKeepsItsHeat(t *testing.T) {
	for _, tc := range []struct {
		heat metrics.Heat
		want lipgloss.TerminalColor
	}{{metrics.High, colBad}, {metrics.Elevated, colWarn}} {
		for _, selected := range []bool{false, true} {
			if got := metricsRowStyle(tc.heat, selected).GetForeground(); got != tc.want {
				t.Errorf("heat %v, selected %v: foreground %v, want %v", tc.heat, selected, got, tc.want)
			}
		}
		if metricsRowStyle(tc.heat, true).GetBackground() != stSelected.GetBackground() {
			t.Errorf("heat %v: the selected row lost its highlight", tc.heat)
		}
	}
	if metricsRowStyle(metrics.Calm, true).GetBackground() != stSelected.GetBackground() {
		t.Error("a calm selected row is not highlighted")
	}
	// The hottest node is first and under the cursor when the screen opens.
	u := newUI(t, loadedCluster(), nil)
	u.keys("M")
	if row, _ := u.m.metrics.selectedRow(); row.Key != "node-b" || row.Heat != metrics.High || u.m.metrics.cursor != 0 {
		t.Fatalf("selected: %+v", row)
	}
}

// In the narrowest pane a filter is still visible — while typing and after —
// and so is the fact that only some rows are showing.
func TestMetricsFilterIsVisibleInANarrowPane(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight})
	u.keys("M", "3", "c", "/")
	u.typeText("web")
	u.want("/web", "2 of 4")
	u.keys("enter")
	u.want("/web 2 of 4", "web-1", "web-pending")
	u.wantNot("db-0", "cron-1")
	u.fitsTerminal()
}

// Caveats are not cut short when there is room under the rows.
func TestMetricsCaveatsUseTheRoomThereIs(t *testing.T) {
	u := newUI(t, loadedCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	u.keys("M", "3", "c") // beside the copilot
	u.wantProse("1 pod shown as — (1 not running, so there is nothing to measure). That is unknown, not zero.")
	u.fitsTerminal()
}

// Drilled into a namespace when the screen is limited to another: it does
// not claim that namespace is empty.
func TestMetricsDoesNotCallAnUnreadNamespaceEmpty(t *testing.T) {
	f := loadedCluster()
	restricted := false
	f.Dynamic.PrependReactor("list", "*", forbiddenUnless(func(group, resource, namespace string) bool {
		return !restricted || resource != "pods" || namespace == "shop"
	}))
	u := newUI(t, f, nil) // the browser is in namespace shop
	u.keys("M", "2", "down", "enter")
	u.want("pods in namespace ops", "cron-1")

	restricted = true
	u.keys("r")
	u.wantNot("No pods in namespace ops")
	if len(u.m.metrics.path) != 0 || u.m.metrics.scope != "shop" {
		t.Fatalf("path %v, scope %q", u.m.metrics.path, u.m.metrics.scope)
	}
	u.want("namespace ops is no longer readable")
	u.keys("3")
	u.want("web-1")
	u.wantProse("showing namespace shop only")

	// The refusal is lifted: the screen widens again by itself.
	restricted = false
	u.keys("r")
	u.want("cron-1")
	u.wantNot("showing namespace shop only")
	if u.m.metrics.scope != "" || u.m.metrics.narrowed != nil {
		t.Fatalf("scope %q, narrowed %v", u.m.metrics.scope, u.m.metrics.narrowed)
	}
}

// Credentials that were not accepted are not described as a missing
// permission.
func TestMetricsExpiredCredentialsAreSaidToBeThat(t *testing.T) {
	f := loadedCluster()
	f.FailMetrics(apierrors.NewUnauthorized("token has expired"))
	u := newUI(t, f, nil)
	u.keys("M", "3")
	u.wantProse("The cluster did not accept your credentials", "expired token or certificate", "authentication failure, not an idle cluster", "token has expired")
	u.wantNot("Permission denied", "not allowed")
	u.noInventedReadings()
}
