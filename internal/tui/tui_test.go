package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/llm"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

// ui drives a Model the way the Bubble Tea runtime would, but synchronously:
// commands are executed and their messages fed back until things settle.
type ui struct {
	t *testing.T
	m *Model
	f *kubetest.Fake
}

func fakeCluster() *kubetest.Fake {
	f := kubetest.New(
		kubetest.Namespace("default"), kubetest.Namespace("shop"), kubetest.Namespace("ops"),
		kubetest.Pod("shop", "web-1", "app", "sidecar"), kubetest.Pod("shop", "web-2"), kubetest.Pod("ops", "cron-1"),
		kubetest.Deployment("shop", "web", 2), kubetest.Widget("shop", "w1"),
	)
	f.SetLogs("shop", "app", false, "app says hello\n")
	f.SetLogs("shop", "sidecar", false, "sidecar says hello\n")
	return f
}

func newUI(t *testing.T, f *kubetest.Fake, mod func(*Deps)) *ui {
	t.Helper()
	deps := Deps{Cluster: f.Cluster, Focus: &FocusStore{}, Namespace: "shop", AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		AgentErr: "no API key for provider \"anthropic\": the environment variable ANTHROPIC_API_KEY is not set."}
	if mod != nil {
		mod(&deps)
	}
	u := &ui{t: t, m: New(deps), f: f}
	t.Cleanup(u.m.Shutdown)
	// Initial loads, without arming the blocking listeners.
	u.pump(u.m.loadTypes(false))
	u.pump(u.m.loadNamespaces())
	u.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	return u
}

// pump runs cmd and feeds what it produces back into the model. Commands
// that do not finish quickly (cursor blink, listeners, timers) are left
// behind, as they would be in the runtime's background.
func (u *ui) pump(cmd tea.Cmd) {
	u.t.Helper()
	if cmd == nil {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(100 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			u.pump(c)
		}
	case tea.QuitMsg:
	default:
		// Only our own messages drive state; ignore library housekeeping.
		switch msg.(type) {
		case typesMsg, nsMsg, listMsg, subjectMsg, eventsMsg, auditMsg, logMsg, agentEventMsg, proposalMsg, metricsMsg:
			u.send(msg)
		}
	}
}

func (u *ui) send(msg tea.Msg) {
	u.t.Helper()
	_, cmd := u.m.Update(msg)
	u.pump(cmd)
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+l":
		return tea.KeyMsg{Type: tea.KeyCtrlL}
	case "ctrl+y":
		return tea.KeyMsg{Type: tea.KeyCtrlY}
	case "ctrl+n":
		return tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// keys presses keys in order. Multi-rune strings that are not key names are
// typed character by character.
func (u *ui) keys(ks ...string) {
	u.t.Helper()
	for _, k := range ks {
		u.send(keyMsg(k))
	}
}

func (u *ui) typeText(s string) {
	u.t.Helper()
	for _, r := range s {
		u.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (u *ui) screen() string { return ansi.Strip(u.m.View()) }

// chat is the copilot transcript as plain text, unwrapped.
func (u *ui) chat() string {
	var b strings.Builder
	for _, it := range u.m.transcript {
		b.WriteString(it.text + "\n")
	}
	return b.String()
}

func (u *ui) wantChat(subs ...string) {
	u.t.Helper()
	s := u.chat()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			u.t.Fatalf("copilot transcript lacks %q:\n%s", sub, s)
		}
	}
}

func (u *ui) want(subs ...string) {
	u.t.Helper()
	s := u.screen()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			u.t.Fatalf("screen lacks %q:\n%s", sub, s)
		}
	}
}

func (u *ui) wantNot(subs ...string) {
	u.t.Helper()
	s := u.screen()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			u.t.Fatalf("screen should not show %q:\n%s", sub, s)
		}
	}
}

// eventually waits for asynchronous work (the agent goroutine) while
// delivering its events to the model.
func (u *ui) eventually(what string, cond func() bool) {
	u.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			u.t.Fatalf("timed out waiting for %s:\n%s", what, u.screen())
		}
		// Deliver directly and drop the returned command: it is the
		// re-armed listener, which here would compete with this loop.
		select {
		case ev := <-u.m.events:
			u.m.Update(agentEventMsg{ev})
		case req := <-u.gateRequests():
			u.m.Update(proposalMsg{req})
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (u *ui) gateRequests() <-chan *approval.Request {
	if u.m.deps.Gate == nil {
		return nil
	}
	return u.m.deps.Gate.Requests()
}

// --- browser ------------------------------------------------------------------

func TestBrowserListsPodsOfInitialNamespace(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.want("ctx test-ctx", "ns shop", "pods · namespace shop · 2", "NAME", "STATUS", "AGE", "web-1", "web-2", "Running")
	u.wantNot("cron-1")

	f := u.m.deps.Focus.Get()
	if f.Namespace != "shop" || f.Type != "pods" || f.Selected != "web-1" || f.SelectedNamespace != "shop" || f.AllNamespaces {
		t.Fatalf("focus: %+v", f)
	}
}

// 2.2 / 2.3: namespace selection, all-namespaces shows each row's namespace.
func TestNamespacePicker(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("n")
	u.want("Namespace", "(all namespaces)", "default", "ops", "shop")
	u.typeText("ops")
	u.keys("enter")
	u.want("ns ops", "pods · namespace ops · 1", "cron-1")
	u.wantNot("web-1")

	u.keys("n", "up", "up", "up", "up", "enter") // top entry: all namespaces
	u.want("ns (all)", "pods · all namespaces · 3", "NAMESPACE", "web-1", "cron-1")
	if f := u.m.deps.Focus.Get(); !f.AllNamespaces || f.Selected == "" || f.SelectedNamespace == "" {
		t.Fatalf("focus: %+v", f)
	}
	for _, r := range u.m.table.Rows {
		if r.Cells[0] != r.Namespace || r.Namespace == "" {
			t.Fatalf("row without namespace: %+v", r)
		}
	}
}

// 2.1: any discovered type can be chosen, including a CRD and cluster-scoped
// ones, which carry no namespace scope.
func TestTypePicker(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("t")
	u.want("Resource type", "deployments.apps", "widgets.example.com", "nodes", "cluster-scoped")
	u.wantNot("pods/log", "bindings")
	u.typeText("widg")
	u.keys("enter")
	u.want("widgets.example.com · namespace shop · 1", "w1", "Ready")

	u.keys("t")
	u.typeText("namespaces")
	u.keys("enter")
	u.want("namespaces · cluster-wide · 3", "ns (cluster-scoped)", "default", "ops", "shop")
	if strings.Contains(strings.Join(u.m.table.Columns, " "), "NAMESPACE") {
		t.Fatal("a cluster-scoped listing has a NAMESPACE column")
	}
	u.keys("n")
	u.want("cluster-scoped: there is no namespace to choose")
	if f := u.m.deps.Focus.Get(); f.Type != "namespaces" || f.Namespaced || f.AllNamespaces {
		t.Fatalf("focus: %+v", f)
	}
}

// 2.4: empty and permission-denied are explicit, different states.
func TestEmptyAndForbiddenStates(t *testing.T) {
	f := fakeCluster()
	u := newUI(t, f, nil)
	u.keys("t")
	u.typeText("services")
	u.keys("enter")
	u.want("No services found (namespace shop).")

	f.Dynamic.PrependReactor("list", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("user tester cannot list secrets"))
	})
	u.keys("t")
	u.typeText("secrets")
	u.keys("enter")
	u.want("Permission denied", "not allowed to list secrets", "not an empty list", "cannot list secrets")
	u.wantNot("No secrets found")
}

// Discovery failing shows the failure, never a partial type list.
func TestDiscoveryFailureIsShown(t *testing.T) {
	f := fakeCluster()
	f.Discovery.Lists, f.Discovery.Err = nil, errors.New("the server is currently unable to handle the request")
	u := newUI(t, f, nil)
	u.want("Cannot discover resource types", "unable to handle the request")
	u.keys("t")
	u.want("resource types are unavailable")
}

// 2.5: detail shows summary + YAML; going back keeps selection and scope.
func TestDetailRoundTrip(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("down") // select web-2
	u.keys("enter")
	u.want("detail · Pod shop/web-2", "Kind", "Pod (v1)", "Status", "Running", "YAML", "apiVersion: v1", "name: web-2")
	if f := u.m.deps.Focus.Get(); f.Selected != "web-2" || f.View != "detail" {
		t.Fatalf("focus in detail: %+v", f)
	}
	u.keys("esc")
	u.want("pods · namespace shop · 2")
	if row, _ := u.m.selectedRow(); row.Name != "web-2" || u.m.curNS != "shop" || u.m.curType.Resource != "pods" {
		t.Fatalf("selection or scope lost: row=%+v ns=%q type=%v", row, u.m.curNS, u.m.curType)
	}
}

// 3.1: logs, container switching, explicit empty state.
func TestLogsView(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("l")
	u.want("logs · Pod shop/web-1", "container app (1/2)", "app says hello")
	u.keys("s")
	u.want("container sidecar (2/2)", "sidecar says hello")
	u.wantNot("app says hello")
	u.keys("v") // previous instance: nothing there
	u.want("previous instance", "has produced no log output")
	u.keys("esc")
	u.want("pods · namespace shop")

	// Logs are for pods; other kinds say so instead of hanging.
	u.keys("t")
	u.typeText("deployments")
	u.keys("enter", "l")
	u.want("logs belong to pods")
}

// Follow mode streams and the buffer is bounded.
func TestLogBufferIsBounded(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.f.SetLogs("shop", "app", false, strings.Repeat("x\n", maxLogLines+250)+"the newest line\n")
	u.keys("l")
	u.eventually("log stream to end", func() bool { return u.m.logs.done })
	if n := len(u.m.logs.lines); n != maxLogLines {
		t.Fatalf("%d lines buffered, want the cap of %d", n, maxLogLines)
	}
	if last := u.m.logs.lines[len(u.m.logs.lines)-1]; last != "the newest line" {
		t.Fatalf("newest line dropped; last is %q", last)
	}
}

// 3.2: events for the selected resource.
func TestEventsView(t *testing.T) {
	f := kubetest.New(kubetest.Namespace("shop"), kubetest.Pod("shop", "web-1"))
	u := newUI(t, f, nil)
	u.keys("e")
	u.want("events · Pod shop/web-1", "No events for this resource")
}

// Escape sequences from the cluster cannot reach the terminal.
func TestClusterTextIsSanitized(t *testing.T) {
	f := fakeCluster()
	f.SetLogs("shop", "app", false, "ok\x1b]0;pwned\x07 \x1b[2J\x1b[31mred\x1b[0m done\n")
	u := newUI(t, f, nil)
	u.keys("l")
	raw := u.m.View()
	if strings.Contains(raw, "\x1b]0;") || strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "pwned\x07") {
		t.Fatalf("escape sequence from a log line reached the screen: %q", u.m.logs.lines)
	}
	u.want("ok", "red done")
}

// 2.6: too-small terminal, quit from every view.
func TestTooSmallAndQuit(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.send(tea.WindowSizeMsg{Width: 40, Height: 10})
	u.want("Terminal too small", "60×16", "40×10")
	u.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	u.want("pods · namespace shop")

	isQuit := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}
	for _, open := range [][]string{nil, {"enter"}, {"l"}, {"e"}, {"?"}, {"A"}, {"n"}, {"tab"}} {
		u := newUI(t, fakeCluster(), nil)
		u.keys(open...)
		if _, cmd := u.m.Update(keyMsg("ctrl+c")); !isQuit(cmd) {
			t.Errorf("ctrl+c does not quit after %v", open)
		}
	}
	for _, open := range [][]string{nil, {"enter"}, {"l"}, {"e"}, {"?"}, {"A"}} {
		u := newUI(t, fakeCluster(), nil)
		u.keys(open...)
		if _, cmd := u.m.Update(keyMsg("q")); !isQuit(cmd) {
			t.Errorf("q does not quit after %v", open)
		}
	}
}

// 4.1: toggling and focusing the copilot leaves the browser where it was.
func TestCopilotToggleKeepsBrowserState(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("down")
	before := u.m.deps.Focus.Get()

	u.keys("c")
	u.want("copilot", "The copilot is not available", "ANTHROPIC_API_KEY", "web-2")
	u.keys("c")
	u.wantNot("The copilot is not available")
	u.keys("tab") // open and focus
	u.want("copilot")
	u.typeText("qnt") // typed into the input, not interpreted as browser keys
	if u.m.pick != nil || u.m.view != vList {
		t.Fatal("keys typed in the copilot leaked into the browser")
	}
	u.keys("esc")
	if u.m.pane != paneBrowser {
		t.Fatal("esc did not return to the browser")
	}
	if after := u.m.deps.Focus.Get(); after != before {
		t.Fatalf("browser state changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if row, _ := u.m.selectedRow(); row.Name != "web-2" {
		t.Fatalf("selection lost: %+v", row)
	}
}

// A long "copilot is not available" reason in a short pane is cut to the
// pane; it does not push the pane's frame off the screen. (Found with a real
// configuration whose message is longer than the one these tests use.)
func TestUnavailableCopilotKeepsItsFrame(t *testing.T) {
	u := newUI(t, fakeCluster(), func(d *Deps) {
		d.AgentErr = "no API key for provider \"openrouter\": the environment variable OPENROUTER_API_KEY is not set. " +
			"Set it (export OPENROUTER_API_KEY=…) and restart, or choose another provider with --provider (anthropic, openai, openrouter, openai-compatible)."
	})
	for _, size := range [][2]int{{MinWidth, MinHeight}, {72, 18}, {120, 40}} {
		u.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, keys := range [][]string{{"c"}, {"m"}} {
			u.keys(keys...)
			lines := strings.Split(u.screen(), "\n")
			if len(lines) != size[1] {
				t.Fatalf("%dx%d after %v: %d lines\n%s", size[0], size[1], keys, len(lines), u.screen())
			}
			panes := 2
			if u.m.copilotMax {
				panes = 1
			}
			if frame := lines[len(lines)-2]; strings.Count(frame, "╰") != panes || strings.Count(frame, "╯") != panes {
				t.Fatalf("%dx%d after %v: a pane lost its bottom border\n%s", size[0], size[1], keys, u.screen())
			}
			u.want("The copilot is not available")
			u.keys("esc", "c") // back to the browser alone
			if u.m.copilotOpen {
				u.keys("c")
			}
		}
	}
}

// Maximize: m from the browser gives the copilot the whole body and focuses
// it; esc and tab restore the split with the browser's state intact.
func TestCopilotMaximizeAndRestore(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("down") // select web-2
	u.keys("/")
	u.typeText("web")
	u.keys("enter") // filter the listing
	before := u.m.deps.Focus.Get()

	// m maximizes a hidden copilot, fills the body, and focuses it.
	u.keys("m")
	u.want("copilot", "The copilot is not available")
	u.wantNot("pods · namespace shop")
	if u.m.pane != paneCopilot || !u.m.copilotMax || !u.m.copilotOpen {
		t.Fatalf("maximize did not focus the copilot: pane=%v max=%v open=%v", u.m.pane, u.m.copilotMax, u.m.copilotOpen)
	}

	// esc restores the split and leaves the browser exactly as it was.
	u.keys("esc")
	u.want("pods · namespace shop")
	if u.m.pane != paneBrowser || u.m.copilotMax {
		t.Fatalf("esc did not restore the split: pane=%v max=%v", u.m.pane, u.m.copilotMax)
	}
	if u.m.filter != "web" {
		t.Fatalf("filter lost across maximize: %q", u.m.filter)
	}
	if row, _ := u.m.selectedRow(); row.Name != "web-2" {
		t.Fatalf("selection lost across maximize: %+v", row)
	}
	if after := u.m.deps.Focus.Get(); after != before {
		t.Fatalf("browser focus changed:\nbefore %+v\nafter  %+v", before, after)
	}

	// tab restores it too.
	u.keys("m")
	if !u.m.copilotMax {
		t.Fatal("m did not maximize on the second press")
	}
	u.keys("tab")
	if u.m.pane != paneBrowser || u.m.copilotMax {
		t.Fatalf("tab did not restore the split: pane=%v max=%v", u.m.pane, u.m.copilotMax)
	}

	// An open sub-view survives maximize and restore as well.
	u.keys("enter")
	if u.m.view != vDetail {
		t.Fatalf("enter did not open detail: %v", u.m.view)
	}
	u.keys("m")
	u.keys("esc")
	if u.m.view != vDetail {
		t.Fatalf("sub-view lost across maximize: %v", u.m.view)
	}
	u.want("detail")
}

// --- copilot + approval -------------------------------------------------------

type copilotUI struct {
	*ui
	stub  *llm.Stub
	trail *audit.Log
}

func newCopilotUI(t *testing.T, steps ...llm.StubStep) *copilotUI {
	t.Helper()
	return newCopilotUIOn(t, fakeCluster(), steps...)
}

// newCopilotUIOn is newCopilotUI over a given cluster.
func newCopilotUIOn(t *testing.T, f *kubetest.Fake, steps ...llm.StubStep) *copilotUI {
	t.Helper()
	stub := llm.NewStub(steps...)
	gate := approval.NewGate()
	focus := &FocusStore{}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	trail, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trail.Close() })
	ag := agent.New(agent.Config{
		Provider: stub, Registry: tools.NewRegistry(f.Cluster, tools.DefaultOptions()), Gate: gate, Audit: trail,
		Focus: focus.Get, Info: f.Info, Approver: "tester", Session: "s",
	})
	u := newUI(t, f, func(d *Deps) {
		d.Agent, d.AgentErr, d.Gate, d.Focus, d.AuditPath = ag, "", gate, focus, path
	})
	return &copilotUI{ui: u, stub: stub, trail: trail}
}

func (c *copilotUI) ask(text string) {
	c.t.Helper()
	c.keys("tab")
	c.typeText(text)
	c.keys("enter")
}

// arm skips the dialog's type-ahead guard.
func (c *copilotUI) arm() { c.m.modal.shownAt = time.Now().Add(-time.Hour) }

// approve presses the approve chord and confirms, past the confirm delay.
func (c *copilotUI) approve() {
	c.t.Helper()
	c.arm()
	c.keys("ctrl+y")
	c.m.modal.confirmAt = time.Now().Add(-time.Hour)
	c.keys("enter")
}

func (c *copilotUI) replicas() int64 {
	c.t.Helper()
	ty, _ := c.f.Resolve(context.Background(), "deployments")
	o, err := c.f.Get(context.Background(), ty, "shop", "web")
	if err != nil {
		c.t.Fatal(err)
	}
	n, _ := o.Object["spec"].(map[string]any)["replicas"].(int64)
	return n
}

func scaleStep(replicas int) llm.StubStep {
	return llm.Call(llm.ToolCall{ID: "c1", Name: "scale", Args: []byte(
		`{"type":"deployment","namespace":"shop","name":"web","replicas":` + strconv.Itoa(replicas) + `,"reason":"web is saturated; two more replicas spread the load"}`)})
}

// 4.4: a question is sent with the focus; the answer appears in the pane.
func TestAskUsesFocusAndShowsAnswer(t *testing.T) {
	c := newCopilotUI(t,
		llm.Call(llm.ToolCall{ID: "r1", Name: "get_logs", Args: []byte(`{"namespace":"shop","pod":"web-2","container":"app"}`)}),
		llm.Text("web-2 is fine: its log shows a clean start."))
	c.keys("down") // select web-2 before asking
	c.ask("is this pod healthy?")
	c.eventually("the answer", func() bool { return !c.m.busy })

	c.want("you", "is this pod healthy?", "copilot")
	c.wantChat("get_logs(container=app, namespace=shop, pod=web-2)", "app says hello", "web-2 is fine")
	first := c.stub.Requests[0].Messages[0].Text
	for _, want := range []string{"namespace: shop", "resource type: pods", "selected resource: Pod web-2 in namespace shop", "is this pod healthy?"} {
		if !strings.Contains(first, want) {
			t.Errorf("request lacks %q:\n%s", want, first)
		}
	}
}

// 6.1: the dialog shows verb, target, before→after, dry-run, reversibility.
func TestApprovalDialogContentScale(t *testing.T) {
	c := newCopilotUI(t, scaleStep(4), llm.Text("done"))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })

	c.want(
		"APPROVAL NEEDED", "nothing has been changed",
		"Scale Deployment shop/web from 2 to 4 replicas",
		"Action", "scale",
		"Target", "Deployment shop/web", "(apps/v1)",
		"Cluster", "test-ctx", "https://test.invalid:6443",
		"Change", "spec.replicas:  2  →  4",
		"Dry-run", "✓ passed", "dryRun=All",
		"Undo", "Reversible: scale back to 2",
		"You asked", "web is slow",
		"Its reason", "web is saturated", "not verified",
		"ctrl+y approve", "ctrl+n reject", "e edit", "There is no timeout",
	)
	c.arm()
	c.keys("x")
	c.want("PATCH /apis/apps/v1/namespaces/shop/deployments/web/scale", `{"spec":{"replicas":4}}`)
}

func TestApprovalDialogContentLabels(t *testing.T) {
	c := newCopilotUI(t, llm.Call(llm.ToolCall{ID: "c1", Name: "set_labels", Args: []byte(
		`{"type":"pod","namespace":"shop","name":"web-1","set":{"quarantine":"true"},"remove":["app"],"reason":"take it out of rotation"}`)}), llm.Text("ok"))
	c.ask("isolate this pod")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.want(
		"Change 2 labels on Pod shop/web-1", "set_labels", "Pod shop/web-1", "(v1)",
		"metadata.labels[app]:  web-1  →  (removed)",
		"metadata.labels[quarantine]:  (not set)  →  true",
		"✓ passed", "Reversible: put each key back",
		"Pod labels are what Services and controllers select on",
	)
}

// 6.2: approving takes two deliberate keys; the change then happens once.
func TestApproveFlow(t *testing.T) {
	c := newCopilotUI(t, scaleStep(4), llm.Text("Scaled to 4."))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })

	// Keys that arrive right as the dialog appears are ignored.
	c.keys("ctrl+y", "enter")
	if c.m.modal.decided != "" || c.m.modal.confirming {
		t.Fatal("type-ahead answered the dialog")
	}
	c.arm()

	// Nothing that can be typed as text decides anything — including a
	// question the user was still writing, sent with enter.
	c.typeText("why is it slow? yes y n")
	c.keys("enter")
	c.typeText("yes")
	c.keys("enter")
	if c.m.modal.decided != "" || c.m.pending == nil || len(c.f.RealWrites()) != 0 {
		t.Fatal("ordinary typing answered the dialog")
	}
	// (The "e" in that text opened the argument editor, which only a chord
	// can submit; leave it.)
	c.keys("esc")
	c.arm()

	// The chord alone approves nothing; any key other than enter backs out,
	// and an enter that comes in the same instant is ignored too.
	c.keys("ctrl+y")
	c.want("Apply this change to the cluster?")
	c.keys("enter")
	if c.m.modal.decided != "" {
		t.Fatal("chord and enter in one bounce approved")
	}
	c.keys("ctrl+y", "x")
	if c.m.modal.decided != "" || len(c.f.RealWrites()) != 0 {
		t.Fatal("approved without confirmation")
	}
	c.approve()
	c.eventually("the turn to finish", func() bool { return !c.m.busy })

	if got := c.replicas(); got != 4 {
		t.Fatalf("replicas = %d", got)
	}
	if n := len(c.f.RealWrites()); n != 1 {
		t.Fatalf("%d real writes", n)
	}
	if c.m.pending != nil {
		t.Fatal("dialog still open")
	}
	c.wantChat("Applied with your approval: Scale Deployment shop/web from 2 to 4 replicas", "Scaled to 4.")

	// And it is in the trail, visible from the UI.
	c.keys("esc", "A")
	c.eventually("the audit view", func() bool { return strings.Contains(c.screen(), "Integrity: OK") })
	c.want("Integrity: OK — 2 entries", "scale", "Deployment shop/web", "you asked:  web is slow", "approved by tester", "applying", "applied")
}

func TestRejectFlow(t *testing.T) {
	c := newCopilotUI(t, scaleStep(4), llm.Text("Okay, leaving it at 2."))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.arm()
	c.keys("ctrl+n")
	c.eventually("the turn to finish", func() bool { return !c.m.busy })

	if c.replicas() != 2 || len(c.f.RealWrites()) != 0 {
		t.Fatal("a rejected change was applied")
	}
	c.wantChat("Declined, nothing changed", "Okay, leaving it at 2.")
	res := c.stub.Requests[1].Messages
	if got := res[len(res)-1].ToolResults[0].Content; !strings.HasPrefix(got, "declined by user") {
		t.Fatalf("model was told: %q", got)
	}
}

// Hiding the dialog is not an answer: the browser works, the proposal waits,
// nothing is applied, and it can be brought back.
func TestHideKeepsWaiting(t *testing.T) {
	c := newCopilotUI(t, scaleStep(4), llm.Text("ok"))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.arm()
	c.keys("esc")
	c.want("APPROVAL WAITING — press ctrl+p", "nothing happens until you decide")
	c.wantNot("APPROVAL NEEDED")

	// Browse around while it waits.
	c.keys("tab", "down", "enter")
	c.want("detail · Pod shop/web-2", "APPROVAL WAITING")
	time.Sleep(300 * time.Millisecond)
	if c.m.pending == nil || !c.m.busy || len(c.f.RealWrites()) != 0 || c.replicas() != 2 {
		t.Fatal("the hidden proposal did not simply wait")
	}
	if c.stub.Calls() != 1 {
		t.Fatal("the loop moved on")
	}

	// From the copilot pane too: p is just a letter there, esc does not
	// discard the proposal, ctrl+p brings it back.
	c.keys("esc", "tab")
	c.typeText("p")
	c.keys("esc")
	if c.m.pending == nil || !c.m.busy {
		t.Fatal("esc in the copilot pane threw the waiting proposal away")
	}
	c.keys("tab", "ctrl+p")
	c.want("APPROVAL NEEDED", "Scale Deployment shop/web from 2 to 4 replicas")
	c.arm()
	c.keys("ctrl+n")
	c.eventually("the turn to finish", func() bool { return !c.m.busy })
}

func TestEditFlow(t *testing.T) {
	c := newCopilotUI(t, scaleStep(9), llm.Text("Scaled to 3."))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.arm()
	c.keys("e")
	c.want("Edit the arguments", `"replicas": 9`)

	c.m.editor.SetValue(`{"replicas": 3`)
	c.keys("ctrl+s")
	c.want("not a valid JSON object")
	if c.m.modal.decided != "" {
		t.Fatal("invalid edit was submitted")
	}

	c.m.editor.SetValue(`{"type":"deployment","namespace":"shop","name":"web","replicas":3,"reason":"three will do"}`)
	c.keys("ctrl+s")
	c.eventually("the re-validated proposal", func() bool {
		return c.m.pending != nil && strings.Contains(c.m.pending.Proposal.Title, "to 3 replicas")
	})
	if len(c.f.RealWrites()) != 0 {
		t.Fatal("the edit itself applied something")
	}
	c.want("Scale Deployment shop/web from 2 to 3 replicas", "three will do")
	c.approve()
	c.eventually("the turn to finish", func() bool { return !c.m.busy })
	if c.replicas() != 3 {
		t.Fatalf("replicas = %d", c.replicas())
	}
}

// Quitting while a proposal waits closes it unapplied, and records that.
func TestQuitWhileWaiting(t *testing.T) {
	c := newCopilotUI(t, scaleStep(4), llm.Text("unreachable"))
	c.ask("web is slow")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.arm()
	c.keys("esc") // hide; it keeps waiting
	if _, cmd := c.m.Update(keyMsg("ctrl+c")); cmd == nil {
		t.Fatal("ctrl+c did not quit")
	}
	c.m.Shutdown() // what main does once the program has stopped

	if c.replicas() != 2 || len(c.f.RealWrites()) != 0 {
		t.Fatal("quitting applied the waiting proposal")
	}
	rep, _ := audit.Verify(c.m.deps.AuditPath)
	if !rep.OK() || len(rep.Entries) != 1 || rep.Entries[0].Outcome.Status != audit.OutcomeCancelled || rep.Entries[0].Decision.Action != audit.DecisionNone {
		t.Fatalf("audit: %+v %v", rep.Entries, rep.Problems)
	}
}

// A long proposal scrolls; no line of it is hidden behind the hint, and the
// decision keys stay on screen.
func TestDialogScrollsWithoutHidingContent(t *testing.T) {
	c := newCopilotUI(t, llm.Call(llm.ToolCall{ID: "c1", Name: "set_annotations", Args: []byte(
		`{"type":"deployment","namespace":"shop","name":"web","set":{"a1":"1","a2":"2","a3":"3","a4":"4","a5":"5","a6":"6","a7":"7","a8":"8","zz-last":"LASTVALUE"},"reason":"r"}`)}), llm.Text("ok"))
	c.ask("annotate")
	c.eventually("the proposal", func() bool { return c.m.pending != nil })
	c.send(tea.WindowSizeMsg{Width: 80, Height: 18})
	c.arm()
	c.want("more line(s) below", "ctrl+y approve")
	seen := c.screen()
	for i := 0; i < 40; i++ {
		c.keys("down")
		seen += c.screen()
	}
	c.want("end of proposal", "ctrl+y approve")
	for _, want := range []string{"metadata.annotations[a1]", "metadata.annotations[zz-last]", "LASTVALUE", "Its reason"} {
		if !strings.Contains(seen, want) {
			t.Errorf("%q can never be scrolled into view", want)
		}
	}
	c.keys("ctrl+n")
	c.eventually("the turn to finish", func() bool { return !c.m.busy })
}

// Escape sequences in a model-chosen tool name or an event type do not reach
// the terminal.
func TestModelAndEventTextIsSanitized(t *testing.T) {
	c := newCopilotUI(t, llm.Text("ok"))
	c.keys("tab")
	c.m.busy = true
	c.m.onAgentEvent(agent.EventToolStart{Call: llm.ToolCall{Name: "get_logs\x1b]0;pwned\x07\x1b[2J"}})
	if raw := c.m.View(); strings.Contains(raw, "\x1b]0;") || strings.Contains(raw, "\x1b[2J") {
		t.Fatal("a tool name carried an escape sequence to the screen")
	}
	out := renderEvents([]kube.Event{{Type: "Warn\x1b[2Jing", Reason: "R\x1b]0;x\x07", Message: "m"}}, nil, 80)
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]0;") {
		t.Fatal("an event field carried an escape sequence to the screen")
	}
}

// The type picker survives the type list changing underneath it.
func TestTypePickerSurvivesRefresh(t *testing.T) {
	u := newUI(t, fakeCluster(), nil)
	u.keys("t")
	u.typeText("widg")
	u.send(typesMsg{err: errors.New("discovery went away")}) // m.types is now empty
	u.keys("enter")
	if u.m.curType.Resource != "widgets" {
		t.Fatalf("picked %v", u.m.curType)
	}
}

// 4.5: a provider failure shows in the pane; the browser carries on.
func TestProviderFailureLeavesBrowserUsable(t *testing.T) {
	c := newCopilotUI(t, llm.Fail(&llm.Error{Provider: "anthropic", Status: 529, Msg: "overloaded"}))
	c.ask("hello")
	c.eventually("the failure", func() bool { return !c.m.busy })
	c.wantChat("The model request failed", "overloaded", "Nothing was changed in the cluster")
	c.keys("tab", "down", "enter")
	c.want("detail · Pod shop/web-2")
	if len(c.f.Writes()) != 0 {
		t.Fatal("something was written")
	}
}

func TestFocusStoreSeenByAgentAfterChange(t *testing.T) {
	c := newCopilotUI(t, llm.Text("one"), llm.Text("two"))
	c.ask("first")
	c.eventually("first answer", func() bool { return !c.m.busy })
	c.keys("tab", "n")
	c.typeText("ops")
	c.keys("enter")
	c.ask("second")
	c.eventually("second answer", func() bool { return c.stub.Calls() == 2 && !c.m.busy })
	msgs := c.stub.Requests[1].Messages
	last := msgs[len(msgs)-1].Text
	if !strings.Contains(last, "namespace: ops") || !strings.Contains(last, "selected resource: Pod cron-1 in namespace ops") {
		t.Fatalf("the agent did not see the new focus:\n%s", last)
	}
}

var _ = kube.AllNamespaces
