package tui

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

func crashingPod(ns, name, container string) *corev1.Pod {
	p := kubetest.Pod(ns, name, container)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container, RestartCount: 7,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}}}}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(time.Now().Add(-3 * time.Minute))}}
	return p
}

func stalledDeployment(ns, name string) *appsv1.Deployment {
	d := kubetest.Deployment(ns, name, 3)
	d.Status = appsv1.DeploymentStatus{Replicas: 3, AvailableReplicas: 1, UpdatedReplicas: 2, ReadyReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "api-7c9" has timed out progressing.`, LastTransitionTime: metav1.NewTime(time.Now().Add(-8 * time.Minute))}}}
	return d
}

// sickCluster has a crash-looping pod and a stalled rollout, beside pods and
// a node that are fine. No metrics API is served.
func sickCluster() *kubetest.Fake {
	f := kubetest.New(
		kubetest.Namespace("default"), kubetest.Namespace("shop"), kubetest.Namespace("ops"),
		kubetest.Node("node-a", "4", "8Gi"),
		crashingPod("shop", "web-1", "app"), kubetest.Pod("shop", "web-2"), kubetest.Pod("ops", "cron-1"),
		stalledDeployment("shop", "api"),
	)
	f.SetLogs("shop", "app", false, "app says hello\n")
	return f
}

// podLists counts the typed pod lists the fake received: the health reads.
func podLists(f *kubetest.Fake) int {
	n := 0
	for _, a := range f.Clientset.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

// selectFinding moves the cursor onto the finding with this reason.
func (u *ui) selectFinding(reason string) {
	u.t.Helper()
	hs := &u.m.health
	for i, idx := range hs.rows {
		if hs.report.Findings[idx].Reason == reason {
			hs.cursor = i
			u.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}) // redraw, republish the focus
			return
		}
	}
	u.t.Fatalf("no finding %q on show", reason)
}

// 4.1: one key from any browser view; findings by class, class states, the
// age; the browser exactly as it was on the way back.
func TestHealthKeyOpensAndReturnsWithBrowserState(t *testing.T) {
	u := newUI(t, sickCluster(), nil)
	u.keys("/")
	u.typeText("web")
	u.keys("enter", "down")
	ns, typ, filter := u.m.curNS, u.m.curType.String(), u.m.filter
	sel, _ := u.m.selectedRow()
	if ns != "shop" || typ != "pods" || filter != "web" || sel.Name != "web-2" {
		t.Fatalf("setup: %s %s %q %s", ns, typ, filter, sel.Name)
	}

	u.keys("H")
	if u.m.view != vHealth {
		t.Fatalf("view %v", u.m.view)
	}
	u.want("health · 2 findings", "read ", " ago",
		"WRK Pod shop/web-1 [app]  CrashLoopBackOff", "ROL Deployment shop/api  ProgressDeadlineExceeded",
		"workloads watched · 1", "rollouts  watched · 1", "nodes     watched", "events    watched")
	u.wantProse("usage not applicable — Metrics are not available on this cluster")
	// Key hints for this screen, not the listing's.
	u.want("enter detail", "l logs", "/ filter", "r refresh", "esc back")
	u.wantNot("n namespace", "t type")

	u.keys("esc")
	got, _ := u.m.selectedRow()
	if u.m.view != vList || u.m.curNS != ns || u.m.curType.String() != typ || u.m.filter != filter || got.Name != "web-2" {
		t.Fatalf("back in the browser: view %v ns %s type %s filter %q selected %s", u.m.view, u.m.curNS, u.m.curType.String(), u.m.filter, got.Name)
	}

	// From another browser view too, and back to it.
	u.keys("enter")
	if u.m.view != vDetail {
		t.Fatalf("view %v", u.m.view)
	}
	u.keys("H")
	u.want("health · 2 findings")
	u.keys("esc")
	if u.m.view != vDetail {
		t.Fatalf("H from the detail view did not return to it: %v", u.m.view)
	}
}

// 4.2: one filter over object, namespace, class and reason; the cursor
// follows its finding when a refresh reorders the list.
func TestHealthFilterAndStickySelection(t *testing.T) {
	f := sickCluster()
	u := newUI(t, f, nil)
	u.keys("H", "/")
	u.typeText("deadline")
	u.want("/deadline", "1 of 2", "ProgressDeadlineExceeded")
	u.wantNot("CrashLoopBackOff")
	u.keys("enter")
	for q, n := range map[string]int{"rollouts": 1, "shop": 2, "api": 1, "crashloop": 1, "deployment": 1, "web-1": 1, "ops": 0} {
		u.m.setFilter(q)
		if len(u.m.health.rows) != n {
			t.Errorf("filter %q: %d rows, want %d", q, len(u.m.health.rows), n)
		}
	}
	u.m.setFilter("deadline")
	u.keys("esc") // clears the filter, stays on the screen
	if u.m.view != vHealth || u.m.health.filter != "" || len(u.m.health.rows) != 2 {
		t.Fatalf("esc with a filter: view %v filter %q rows %d", u.m.view, u.m.health.filter, len(u.m.health.rows))
	}
	if u.m.filter != "" && u.m.filter != "web" {
		t.Fatalf("the browser's filter was touched: %q", u.m.filter)
	}

	u.selectFinding("ProgressDeadlineExceeded")
	// A new finding sorts ahead of it.
	if err := f.Clientset.Tracker().Add(crashingPod("ops", "aaa-1", "app")); err != nil {
		t.Fatal(err)
	}
	u.keys("r")
	u.want("health · 3 findings")
	if sel, ok := u.m.health.selected(); !ok || sel.Reason != "ProgressDeadlineExceeded" || u.m.health.cursor != 2 {
		t.Fatalf("the cursor did not follow its finding: %+v at %d", sel, u.m.health.cursor)
	}
}

// 4.3: enter, l and e open the finding's object; leaving comes back to the
// same finding.
func TestHealthOpensSubjectViews(t *testing.T) {
	u := newUI(t, sickCluster(), nil)
	u.keys("H")
	u.selectFinding("CrashLoopBackOff")
	back := func() {
		t.Helper()
		u.keys("esc")
		if sel, ok := u.m.health.selected(); u.m.view != vHealth || !ok || sel.Reason != "CrashLoopBackOff" {
			t.Fatalf("not back on the same finding: view %v %+v", u.m.view, sel)
		}
	}
	u.keys("enter")
	if u.m.view != vDetail || u.m.subj.name != "web-1" || u.m.subj.namespace != "shop" || u.m.subj.typ.Resource != "pods" {
		t.Fatalf("detail: %v %+v", u.m.view, u.m.subj)
	}
	u.want("detail · Pod shop/web-1")
	back()
	u.keys("l")
	if u.m.view != vLogs || u.m.logs.container() != "app" {
		t.Fatalf("logs: %v %q", u.m.view, u.m.logs.container())
	}
	u.eventually("logs", func() bool { return strings.Contains(u.screen(), "app says hello") })
	back()
	u.keys("e")
	if u.m.view != vEvents || u.m.subj.name != "web-1" {
		t.Fatalf("events: %v", u.m.view)
	}
	back()

	// A Deployment has no logs; its detail opens as a Deployment.
	u.selectFinding("ProgressDeadlineExceeded")
	u.keys("l")
	if u.m.view != vHealth || !strings.Contains(u.m.status, "logs belong to pods") {
		t.Fatalf("logs of a Deployment: view %v status %q", u.m.view, u.m.status)
	}
	u.keys("d")
	if u.m.view != vDetail || u.m.subj.typ.Kind != "Deployment" || u.m.subj.name != "api" {
		t.Fatalf("deployment detail: %+v", u.m.subj)
	}
}

// 4.4: reads only while visible, at max(refresh_interval, 15s), never on top
// of one still out.
func TestHealthRefreshesOnlyWhileVisible(t *testing.T) {
	f := sickCluster()
	u := newUI(t, f, func(d *Deps) { d.Refresh = 5 * time.Second })
	if u.m.healthInterval() != minHealthInterval {
		t.Fatalf("interval %v", u.m.healthInterval())
	}
	tick := func() { u.send(tickMsg{}) }
	due := func() {
		u.m.health.asked = time.Now().Add(-minHealthInterval)
		u.m.health.obs.At = time.Now().Add(-time.Minute) // the cached observation has aged with it
	}

	tick()
	if podLists(f) != 0 {
		t.Fatal("health was read while the screen was closed")
	}
	u.keys("H")
	calls := podLists(f)
	if calls != 1 {
		t.Fatalf("opening read pods %d times", calls)
	}
	tick()
	if podLists(f) != calls {
		t.Fatal("re-read before the interval elapsed")
	}
	due()
	tick()
	if podLists(f) != calls+1 {
		t.Fatalf("when due: %d reads", podLists(f)-calls)
	}
	calls = podLists(f)

	// A read still out: the tick waits for it.
	due()
	u.m.health.loading = true
	tick()
	if podLists(f) != calls {
		t.Fatal("a read was started on top of one still out")
	}
	u.m.health.loading = false

	// Hidden behind the maximized copilot, or left: no polling.
	due()
	u.keys("m")
	tick()
	u.keys("esc") // back from the copilot
	if u.m.copilotMax {
		u.keys("tab")
	}
	if podLists(f) != calls {
		t.Fatal("read while hidden behind the copilot")
	}
	if u.m.view == vHealth {
		u.keys("esc")
	}
	due()
	tick()
	if podLists(f) != calls {
		t.Fatal("read after the screen was left")
	}

	// Refresh off: read when opened and on r only, and say so.
	f = sickCluster()
	u = newUI(t, f, nil)
	u.keys("H")
	u.want("auto-refresh off")
	if u.m.healthDue() {
		t.Fatal("due with refresh off")
	}
}

// 4.4: a slow read keeps the UI responsive, says it is reading rather than
// showing an empty result; leaving and coming back works meanwhile.
func TestHealthSlowReadDoesNotBlock(t *testing.T) {
	f := sickCluster()
	release := make(chan struct{})
	var once sync.Once
	f.Clientset.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-release
		return false, nil, nil
	})
	defer once.Do(func() { close(release) })
	u := newUI(t, f, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		u.keys("H")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pressing H blocked on the read")
	}
	u.want("health · reading…", "Reading the cluster")
	u.wantNot("No findings", "0 findings")
	u.keys("esc")
	if u.m.view != vList {
		t.Fatalf("could not leave while reading: %v", u.m.view)
	}
	once.Do(func() { close(release) })
	u.keys("H")
	u.eventually("the findings", func() bool { return strings.Contains(u.screen(), "health · 2 findings") })
}

// 4.5: the focus names the screen, its classes and the selected finding;
// leaving restores the browser's.
func TestHealthFocusText(t *testing.T) {
	u := newUI(t, sickCluster(), nil)
	u.keys("H")
	u.selectFinding("CrashLoopBackOff")
	f := u.m.deps.Focus.Get()
	text := f.Text()
	for _, want := range []string{
		"view: health screen: the cluster's current findings, grouped by class; read 0s ago; 2 findings; classes: workloads watched, nodes watched, rollouts watched, usage not applicable, events watched",
		"namespace: shop\n",
		"selected finding: [workloads] CrashLoopBackOff on Pod shop/web-1 container app",
		"selected resource: Pod web-1 in namespace shop",
		"resource type: pods (kind Pod, namespaced)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("focus lacks %q:\n%s", want, text)
		}
	}
	// A finding in another namespace: the focus's namespace is still the
	// browser's — the one the screen falls back to, and the one get_findings
	// should be given — and the finding's own is stated beside it.
	u.m.curNS = "ops"
	u.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if f := u.m.deps.Focus.Get(); f.Namespace != "ops" || f.SelectedNamespace != "shop" || !strings.Contains(f.Text(), "selected resource: Pod web-1 in namespace shop") {
		t.Fatalf("focus namespace: %+v", f)
	}
	u.m.curNS = "shop"
	// Nothing selected: the screen and its classes, no object.
	u.m.setFilter("nothing-matches")
	u.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	f = u.m.deps.Focus.Get()
	if f.Selected != "" || !strings.Contains(f.View, "no finding selected") || !strings.Contains(f.View, "classes: workloads watched") {
		t.Fatalf("no selection: %+v", f)
	}
	u.keys("esc", "esc")
	f = u.m.deps.Focus.Get()
	if f.View != "list" || f.Type != "pods" || f.Namespace != "shop" || f.Selected != "web-1" {
		t.Fatalf("browser focus not restored: %+v", f)
	}
}

// 4.6: inside the pane at 60×16 and narrower, each finding's class and object
// kept, longer text cut with a marker.
func TestHealthFitsNarrowPane(t *testing.T) {
	u := newUI(t, sickCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight})
	u.keys("H")
	u.fitsTerminal()
	u.want("WRK Pod shop/web-1", "ROL Deployment shop/a")
	u.keys("c") // beside the copilot: a 24-cell pane
	u.fitsTerminal()
	w, _ := u.m.browserInner()
	pane := ansi.Strip(u.m.viewHealth(w, 12))
	if !strings.Contains(pane, "WRK Pod shop/web-1") || !strings.Contains(pane, "…") {
		t.Fatalf("a %d-cell pane lost the class or object, or cut without a marker:\n%s", w, pane)
	}
	// The class that was not watched is the first thing on the class line.
	if !strings.Contains(strings.Split(pane, "\n")[1], "USE n/a") {
		t.Fatalf("the narrow class line lost usage's state:\n%s", pane)
	}
	// Every size from tiny to wide, with hostile names: never outside the
	// pane.
	f := sickCluster()
	evil := crashingPod("shop", "x\x1b[2J"+strings.Repeat("長", 40), "c\u202e")
	_ = f.Clientset.Tracker().Add(evil)
	u = newUI(t, f, nil)
	u.keys("H")
	for w := 1; w <= 130; w += 7 {
		for h := 1; h <= 40; h += 3 {
			out := ansi.Strip(u.m.viewHealth(w, h))
			lines := strings.Split(out, "\n")
			if len(lines) > h {
				t.Fatalf("%d lines into a pane of %d", len(lines), h)
			}
			for _, l := range lines {
				if lipgloss.Width(l) > w {
					t.Fatalf("a %d-cell line into a pane of %d: %q", lipgloss.Width(l), w, l)
				}
				if strings.ContainsAny(l, "\x1b\u202e") {
					t.Fatalf("control characters reached the screen: %q", l)
				}
			}
		}
	}

	// Below the minimum: the usual message; enlarged: the screen again.
	u.send(tea.WindowSizeMsg{Width: 40, Height: 10})
	u.want("Terminal too small")
	u.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	u.want("health · ")
	u.fitsTerminal()
}

// A class that was not read is shown as not watched — and the screen never
// says "no findings" as if the cluster were clear.
func TestHealthShowsWhatIsNotWatched(t *testing.T) {
	f := kubetest.New(kubetest.Namespace("shop"), kubetest.Node("node-a", "4", "8Gi"))
	f.Clientset.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("RBAC: no"))
	})
	u := newUI(t, f, nil)
	u.keys("H")
	u.wantProse("workloads forbidden — Permission denied reading pods across all namespaces",
		"No findings among what was read. A class marked above as not watched, or watched in part, was not fully checked")
	u.wantNot("Every class was watched")

	// The one case that is clear says exactly that, and what it does not cover.
	f = kubetest.New(kubetest.Namespace("shop"), kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "ok"))
	f.SetNodeMetrics("node-a", "400m", "2Gi")
	f.SetPodMetrics("shop", "ok", kubetest.Usage{Container: "app", CPU: "10m", Memory: "20Mi"})
	u = newUI(t, f, nil)
	u.keys("H")
	u.wantProse("No findings. Every class was watched across the whole cluster and none reported a problem.", "Not covered by any class")
}

// The copilot read first: the screen, opened within the reuse window, shows
// what the copilot was given, without a further read.
func TestHealthShowsWhatTheCopilotWasGiven(t *testing.T) {
	f := sickCluster()
	u := newUI(t, f, nil)
	out, err := tools.NewRegistry(f.Cluster, tools.DefaultOptions()).Read(u.m.ctx, "get_findings", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := podLists(f)
	u.keys("H")
	if podLists(f) != calls {
		t.Fatal("the screen read again although the copilot's reading was fresh")
	}
	for i := range u.m.health.report.Findings {
		fd := &u.m.health.report.Findings[i]
		if !strings.Contains(out, fd.Object()) || !strings.Contains(out, fd.Reason) {
			t.Errorf("the copilot was not given %s %s", fd.Object(), fd.Reason)
		}
	}
	for i := range u.m.health.report.Classes {
		if !strings.Contains(out, u.m.health.report.Classes[i].Line()) {
			t.Errorf("the copilot was told something else about %s", u.m.health.report.Classes[i].ID)
		}
	}
	// And the other way: what the screen holds is what the tool hands out.
	obs := f.HealthObservation(u.m.ctx, kube.AnyFallback, kube.HealthFreshFor)
	if obs != u.m.health.obs {
		t.Fatal("the tool would be handed a different observation from the screen's")
	}
}

// 4.7: the help lists the key and the screen's keys, and the README's sample
// of the screen is what the screen draws.
func TestHealthHelpAndReadmeMatchTheScreen(t *testing.T) {
	help := helpText()
	for _, want := range []string{"H                         health: what is broken right now", "HEALTH (H)", "filter by object, namespace, class or reason", "Not covered"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(readme), "### Health")
	section, _, _ = strings.Cut(section, "### Changes")
	u := newUI(t, sickCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	u.keys("H")
	for _, line := range []string{
		"workloads watched · 1", "nodes     watched", "rollouts  watched · 1", "events    watched",
		"usage     not applicable — Metrics are not available on this cluster:",
		"WRK Pod shop/web-1 [app]  CrashLoopBackOff  3m", "ROL Deployment shop/api  ProgressDeadlineExceeded  8m",
		"health · 2 findings",
	} {
		if !strings.Contains(section, line) {
			t.Errorf("the README's sample lacks %q", line)
		}
		u.want(line)
	}
	for _, key := range []string{"`enter`, `d`", "`l` `e`", "`/`", "`r`", "`esc`"} {
		if !strings.Contains(section, key) {
			t.Errorf("the README's key table lacks %s", key)
		}
	}
}
