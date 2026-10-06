package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k2stui/internal/agent"
	"github.com/fardani235/k2stui/internal/approval"
	"github.com/fardani235/k2stui/internal/audit"
	"github.com/fardani235/k2stui/internal/kube/kubetest"
	"github.com/fardani235/k2stui/internal/llm"
	"github.com/fardani235/k2stui/internal/tools"
)

type harness struct {
	t     *testing.T
	f     *kubetest.Fake
	stub  *llm.Stub
	gate  *approval.Gate
	trail *audit.Log
	path  string
	agent *agent.Agent

	mu        sync.Mutex
	events    []agent.Event
	proposals []approval.Proposal
	focus     agent.Focus
}

func newHarness(t *testing.T, limits agent.Limits, steps ...llm.StubStep) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		f:    kubetest.New(kubetest.Deployment("shop", "web", 2), kubetest.Pod("shop", "web-1")),
		stub: llm.NewStub(steps...),
		gate: approval.NewGate(),
		path: filepath.Join(t.TempDir(), "audit.jsonl"),
		focus: agent.Focus{Context: "test-ctx", Namespace: "shop", Type: "pods", Kind: "Pod", Namespaced: true,
			Selected: "web-1", SelectedNamespace: "shop", View: "list"},
	}
	h.f.SetLogs("shop", "", false, "boot\npanic: out of memory\n")
	var err error
	if h.trail, err = audit.Open(h.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.trail.Close() })
	h.agent = h.newAgent(h.trail, limits)
	return h
}

func (h *harness) newAgent(trail *audit.Log, limits agent.Limits) *agent.Agent {
	return agent.New(agent.Config{
		Provider: h.stub, Registry: tools.NewRegistry(h.f.Cluster, tools.DefaultOptions()),
		Gate: h.gate, Audit: trail, Limits: limits, Info: h.f.Info, Approver: "tester@host", Session: "sess",
		Focus: func() agent.Focus {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.focus
		},
	})
}

func (h *harness) emit(ev agent.Event) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

// start runs a request in the background and returns a channel closed when
// it finishes.
func (h *harness) start(ctx context.Context, text string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.agent.Run(ctx, text, h.emit)
	}()
	return done
}

func (h *harness) run(text string) {
	h.t.Helper()
	select {
	case <-h.start(context.Background(), text):
	case <-time.After(10 * time.Second):
		h.t.Fatal("the request did not finish")
	}
}

// human answers every proposal with decide, like a person at the keyboard.
func (h *harness) human(decide func(approval.Proposal) approval.Decision) {
	go func() {
		for req := range h.gate.Requests() {
			h.mu.Lock()
			h.proposals = append(h.proposals, req.Proposal)
			h.mu.Unlock()
			req.Decide(decide(req.Proposal))
		}
	}()
}

func (h *harness) seenProposals() []approval.Proposal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]approval.Proposal(nil), h.proposals...)
}

func (h *harness) done() agent.EventDone {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.events) - 1; i >= 0; i-- {
		if d, ok := h.events[i].(agent.EventDone); ok {
			return d
		}
	}
	h.t.Fatal("no EventDone")
	return agent.EventDone{}
}

func (h *harness) notices() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, ev := range h.events {
		if n, ok := ev.(agent.EventNotice); ok {
			b.WriteString(n.Text + "\n")
		}
	}
	return b.String()
}

func (h *harness) entries() []audit.Entry {
	h.t.Helper()
	rep, err := audit.Verify(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	if !rep.OK() {
		h.t.Fatalf("audit trail is not intact: %v", rep.Problems)
	}
	return rep.Entries
}

// toolResults returns the results the model was given in its nth request
// (0-based), i.e. the answers to the calls it made in the previous turn.
func (h *harness) toolResults(n int) []llm.ToolResult {
	h.t.Helper()
	if n >= len(h.stub.Requests) {
		h.t.Fatalf("the model was only called %d times", len(h.stub.Requests))
	}
	msgs := h.stub.Requests[n].Messages
	return msgs[len(msgs)-1].ToolResults
}

func call(id, name string, a map[string]any) llm.ToolCall {
	b, _ := json.Marshal(a)
	return llm.ToolCall{ID: id, Name: name, Args: b}
}

func scaleCall(id string, replicas int) llm.ToolCall {
	return call(id, "scale", map[string]any{"type": "deployment", "namespace": "shop", "name": "web", "replicas": replicas, "reason": "the pod is out of memory under load"})
}

func (h *harness) replicas() int64 {
	h.t.Helper()
	ty, err := h.f.Resolve(context.Background(), "deployments")
	if err != nil {
		h.t.Fatal(err)
	}
	o, err := h.f.Get(context.Background(), ty, "shop", "web")
	if err != nil {
		h.t.Fatal(err)
	}
	spec := o.Object["spec"].(map[string]any)
	switch n := spec["replicas"].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	h.t.Fatalf("replicas is %T", spec["replicas"])
	return 0
}

var approve = func(approval.Proposal) approval.Decision { return approval.Decision{Action: approval.Approve} }
var reject = func(approval.Proposal) approval.Decision { return approval.Decision{Action: approval.Reject} }

// 4.2 / 5.2: a scripted turn — a read tool runs by itself and its result
// goes back to the model.
func TestReadToolAutoExecutes(t *testing.T) {
	h := newHarness(t, agent.Limits{},
		llm.Call(call("c1", "get_logs", map[string]any{"namespace": "shop", "pod": "web-1"})),
		llm.Text("It ran out of memory."),
	)
	h.run("why does this pod keep restarting?")

	if err := h.done().Err; err != nil {
		t.Fatal(err)
	}
	res := h.toolResults(1)
	if len(res) != 1 || res[0].CallID != "c1" || res[0].IsError || !strings.Contains(res[0].Content, "panic: out of memory") {
		t.Fatalf("tool result: %+v", res)
	}
	if got := h.f.MutatingActions(); len(got) != 0 {
		t.Fatalf("a read changed something: %v", got)
	}
	if len(h.entries()) != 0 {
		t.Fatal("a read was audited as a gated action")
	}
	// The model was offered exactly the registry's tools.
	if n := len(h.stub.Requests[0].Tools); n != 9 {
		t.Fatalf("model was offered %d tools", n)
	}
}

// 4.4: the focus is in every request and follows the browser.
func TestFocusIsInjectedEachRequest(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Text("ok"), llm.Text("ok"))
	h.run("what is wrong here?")
	first := h.stub.Requests[0].Messages[0].Text
	for _, want := range []string{"kube context: test-ctx", "namespace: shop", "resource type: pods", "selected resource: Pod web-1 in namespace shop", "what is wrong here?"} {
		if !strings.Contains(first, want) {
			t.Errorf("first request lacks %q:\n%s", want, first)
		}
	}

	h.mu.Lock()
	h.focus = agent.Focus{Context: "test-ctx", AllNamespaces: true, Type: "deployments.apps", Kind: "Deployment", Namespaced: true}
	h.mu.Unlock()
	h.run("and now?")
	msgs := h.stub.Requests[1].Messages
	second := msgs[len(msgs)-1].Text
	for _, want := range []string{"namespace: (all namespaces)", "resource type: deployments.apps", "selected resource: (none)"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request lacks %q:\n%s", want, second)
		}
	}
	if strings.Contains(second, "web-1") {
		t.Errorf("second request claims a selection that is gone:\n%s", second)
	}
	// History carried over.
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages, want user/assistant/user", len(msgs))
	}
}

// 4.3: a run that never stops asking for tools is stopped at the bound, and
// says so.
func TestIterationLimit(t *testing.T) {
	h := newHarness(t, agent.Limits{MaxIterations: 3, MaxToolCalls: 100},
		llm.Call(call("c", "list_resources", map[string]any{"type": "pods", "namespace": "shop"})))
	h.stub.Loop = true
	h.run("loop forever")

	if h.stub.Calls() != 3 {
		t.Fatalf("model called %d times, want 3", h.stub.Calls())
	}
	if !strings.Contains(h.notices(), "limit of 3 model turns") || !strings.Contains(h.notices(), "Nothing unapproved was applied") {
		t.Fatalf("limit not reported: %q", h.notices())
	}
	if err := h.done().Err; err != nil {
		t.Fatal(err)
	}
	if got := h.f.MutatingActions(); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestToolCallLimit(t *testing.T) {
	list := func(id string) llm.ToolCall {
		return call(id, "list_resources", map[string]any{"type": "pods", "namespace": "shop"})
	}
	h := newHarness(t, agent.Limits{MaxIterations: 10, MaxToolCalls: 3},
		llm.Call(list("a"), list("b")),
		llm.Call(list("c"), list("d"), scaleCall("e", 9)),
		llm.Text("unreachable"),
	)
	h.human(approve) // would approve anything — it must never be asked
	h.run("go")

	if h.stub.Calls() != 2 {
		t.Fatalf("model called %d times after the limit", h.stub.Calls())
	}
	if !strings.Contains(h.notices(), "limit of 3 tool calls") {
		t.Fatalf("limit not reported: %q", h.notices())
	}
	if len(h.seenProposals()) != 0 || len(h.f.Writes()) != 0 {
		t.Fatal("a mutate call past the limit was acted on")
	}

	// The conversation stays well-formed: every call got a result — also the
	// ones that were not executed — so a real provider accepts the next
	// request.
	res := h.toolResults(1)
	if len(res) != 2 {
		t.Fatalf("results for first turn: %+v", res)
	}
	h.stub.Loop = true // replay "unreachable" text for the follow-up
	h.run("continue")
	msgs := h.stub.Requests[2].Messages
	last := msgs[len(msgs)-2] // the tool results before the new user message
	if len(last.ToolResults) != 3 {
		t.Fatalf("want a result for each of the 3 calls, got %+v", last.ToolResults)
	}
	for i, wantErr := range []bool{false, true, true} {
		if last.ToolResults[i].IsError != wantErr {
			t.Errorf("result %d: %+v", i, last.ToolResults[i])
		}
	}
	if !strings.Contains(last.ToolResults[2].Content, "not executed") {
		t.Errorf("the skipped mutate call: %+v", last.ToolResults[2])
	}
}

// 4.5: a provider failure is reported, nothing is applied, and the next
// request works.
func TestProviderFailure(t *testing.T) {
	boom := &llm.Error{Provider: "stub", Status: 529, Msg: "overloaded"}
	h := newHarness(t, agent.Limits{}, llm.Fail(boom), llm.Text("back"))
	h.run("hello")
	if err := h.done().Err; !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if len(h.f.Writes()) != 0 || len(h.entries()) != 0 {
		t.Fatal("something happened on a failed turn")
	}
	h.run("again")
	if err := h.done().Err; err != nil {
		t.Fatalf("agent unusable after a provider failure: %v", err)
	}
}

// 6.4: read → dry-run → approval → apply → real result → continue.
func TestEndToEndApproved(t *testing.T) {
	h := newHarness(t, agent.Limits{},
		llm.Call(call("c1", "describe_resource", map[string]any{"type": "pod", "namespace": "shop", "name": "web-1"})),
		llm.Call(scaleCall("c2", 5)),
		llm.Call(call("c3", "get_resource", map[string]any{"type": "deployment", "namespace": "shop", "name": "web"})),
		llm.Text("Scaled to 5 and verified."),
	)
	h.human(approve)
	h.run("web is overloaded, fix it")

	if err := h.done().Err; err != nil {
		t.Fatal(err)
	}
	// What the human was shown.
	props := h.seenProposals()
	if len(props) != 1 {
		t.Fatalf("%d proposals", len(props))
	}
	p := props[0]
	if p.Tool != "scale" || p.Title != "Scale Deployment shop/web from 2 to 5 replicas" ||
		p.Target.Kind != "Deployment" || p.Target.Namespace != "shop" || p.Target.Name != "web" || p.Target.Context != "test-ctx" ||
		len(p.Changes) != 1 || p.Changes[0].Before != "2" || p.Changes[0].After != "5" ||
		!strings.Contains(p.DryRun, "passed") || !p.Reversible || !strings.Contains(p.Reversibility, "scale back to 2") ||
		p.Intent != "web is overloaded, fix it" || !strings.Contains(p.ModelReason, "out of memory") ||
		!strings.Contains(p.Request, "PATCH /apis/apps/v1/namespaces/shop/deployments/web/scale") {
		t.Fatalf("proposal: %+v", p)
	}

	// What reached the cluster: one dry-run, then the identical real request.
	ws := h.f.Writes()
	if len(ws) != 2 || !ws[0].DryRun || ws[1].DryRun || ws[0].Body != ws[1].Body {
		t.Fatalf("writes: %+v", ws)
	}
	if h.replicas() != 5 {
		t.Fatalf("replicas = %d", h.replicas())
	}

	// The model got the real result and went on to verify.
	res := h.toolResults(2)
	if len(res) != 1 || res[0].IsError || !strings.Contains(res[0].Content, "approved by user and applied") {
		t.Fatalf("mutate result: %+v", res)
	}
	if verify := h.toolResults(3); !strings.Contains(verify[0].Content, "replicas: 5") {
		t.Fatalf("follow-up read: %+v", verify)
	}

	// The record.
	// The record: the approval is written before the request is sent, the
	// result after it.
	es := h.entries()
	if len(es) != 2 || es[0].ProposalID != es[1].ProposalID || es[0].Outcome.Status != audit.OutcomeApplying ||
		es[0].Decision.Action != audit.DecisionApproved {
		t.Fatalf("audit entries: %+v", es)
	}
	e := es[1]
	if e.Intent != "web is overloaded, fix it" || e.Tool != "scale" || e.Target.Name != "web" ||
		!strings.Contains(string(e.Args), `"replicas":5`) || e.DryRun.Result != audit.DryRunPassed ||
		e.Decision.Action != audit.DecisionApproved || e.Decision.By != "tester@host" || e.Decision.At == nil ||
		e.Outcome.Status != audit.OutcomeApplied || e.Model != "stub/scripted" || e.Session != "sess" || e.Time.IsZero() {
		t.Fatalf("audit entry: %+v", e)
	}
}

// 6.2 / 6.4: a rejection applies nothing and comes back as "declined by
// user", and the loop carries on.
func TestRejected(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("Understood, leaving it."))
	h.human(reject)
	h.run("scale it")

	if err := h.done().Err; err != nil {
		t.Fatal(err)
	}
	res := h.toolResults(1)
	if len(res) != 1 || res[0].IsError || !strings.HasPrefix(res[0].Content, "declined by user") {
		t.Fatalf("result: %+v", res)
	}
	if h.stub.Calls() != 2 {
		t.Fatal("the loop did not resume after the rejection")
	}
	if n := len(h.f.RealWrites()); n != 0 || h.replicas() != 2 {
		t.Fatalf("rejected change was applied (%d writes, %d replicas)", n, h.replicas())
	}
	es := h.entries()
	if len(es) != 1 || es[0].Decision.Action != audit.DecisionRejected || es[0].Outcome.Status != audit.OutcomeDeclined ||
		es[0].DryRun.Result != audit.DryRunPassed || !strings.Contains(es[0].Outcome.Detail, "nothing was applied") {
		t.Fatalf("audit: %+v", es)
	}
}

// 5.4 / 6.4: a proposal the server rejects in dry-run never reaches the
// human; the reason goes to the model and into the record.
func TestDryRunRejected(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("The quota blocks it."))
	h.f.OnWrite = func(w kubetest.Write) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web",
			errors.New("exceeded quota: compute-quota"))
	}
	h.human(approve)
	h.run("scale it")

	if n := len(h.seenProposals()); n != 0 {
		t.Fatalf("an invalid proposal was shown for approval (%d)", n)
	}
	res := h.toolResults(1)
	if !res[0].IsError || !strings.Contains(res[0].Content, "exceeded quota") || !strings.Contains(res[0].Content, "not offered to the user") ||
		!strings.Contains(res[0].Content, "Permission denied") {
		t.Fatalf("result: %+v", res)
	}
	if n := len(h.f.RealWrites()); n != 0 {
		t.Fatalf("%d real writes", n)
	}
	es := h.entries()
	if len(es) != 1 || es[0].DryRun.Result != audit.DryRunRejected || !strings.Contains(es[0].DryRun.Message, "exceeded quota") ||
		es[0].Decision.Action != audit.DecisionNone || es[0].Outcome.Status != audit.OutcomeDryRunRejected {
		t.Fatalf("audit: %+v", es)
	}
	// The user is told too.
	h.mu.Lock()
	defer h.mu.Unlock()
	told := false
	for _, ev := range h.events {
		if c, ok := ev.(agent.EventProposalClosed); ok && c.Outcome == audit.OutcomeDryRunRejected && strings.Contains(c.Detail, "exceeded quota") {
			told = true
		}
	}
	if !told {
		t.Fatal("the dry-run rejection was not surfaced to the user")
	}
}

// 6.2: an unanswered proposal waits — and waits — and nothing is applied.
// Ending the request closes it unapplied and records that.
func TestUnansweredProposalNeverApplies(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 0)), llm.Text("unreachable"))
	ctx, cancel := context.WithCancel(context.Background())
	done := h.start(ctx, "scale it down")

	var req *approval.Request
	select {
	case req = <-h.gate.Requests():
	case <-time.After(5 * time.Second):
		t.Fatal("no proposal arrived")
	}

	// Nobody answers.
	select {
	case <-done:
		t.Fatal("the loop moved on without a decision")
	case <-time.After(400 * time.Millisecond):
	}
	if n := len(h.f.RealWrites()); n != 0 || h.replicas() != 2 {
		t.Fatalf("applied while waiting: %d writes, %d replicas", n, h.replicas())
	}
	if h.stub.Calls() != 1 {
		t.Fatal("the model was called again while a proposal was pending")
	}
	if len(h.entries()) != 0 {
		t.Fatal("an outcome was recorded before any decision")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not end the request")
	}
	// A late "yes" on a dead proposal does nothing.
	req.Decide(approval.Decision{Action: approval.Approve})
	time.Sleep(50 * time.Millisecond)

	if n := len(h.f.RealWrites()); n != 0 || h.replicas() != 2 {
		t.Fatalf("applied after cancel: %d writes, %d replicas", n, h.replicas())
	}
	es := h.entries()
	if len(es) != 1 || es[0].Outcome.Status != audit.OutcomeCancelled || es[0].Decision.Action != audit.DecisionNone {
		t.Fatalf("audit: %+v", es)
	}
	if !errors.Is(h.done().Err, context.Canceled) {
		t.Fatalf("done: %v", h.done().Err)
	}
}

// 6.3: dry-run passed, the human approved, and the apply still failed.
func TestApplyFailsAfterApproval(t *testing.T) {
	for name, mk := range map[string]func() error{
		"conflict": func() error {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("the object has been modified"))
		},
		"rbac": func() error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("role revoked"))
		},
		"admission": func() error {
			return apierrors.NewBadRequest(`admission webhook "policy.example.com" denied the request: replicas capped at 3`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("It failed."))
			h.f.OnWrite = func(w kubetest.Write) error {
				if w.DryRun {
					return nil
				}
				return mk()
			}
			h.human(approve)
			h.run("scale it")

			res := h.toolResults(1)
			if !res[0].IsError || !strings.Contains(res[0].Content, "apply failed after approval") {
				t.Fatalf("result: %+v", res)
			}
			if h.replicas() != 2 {
				t.Fatalf("replicas = %d", h.replicas())
			}
			es := h.entries()
			if len(es) != 2 || es[0].Outcome.Status != audit.OutcomeApplying ||
				es[1].Decision.Action != audit.DecisionApproved || es[1].Outcome.Status != audit.OutcomeFailed || es[1].Outcome.Error == "" {
				t.Fatalf("audit: %+v", es)
			}
			if name == "rbac" && !strings.Contains(res[0].Content, "permission denied") {
				t.Errorf("RBAC failure not named as such: %s", res[0].Content)
			}
			if h.stub.Calls() != 2 {
				t.Fatal("the failure did not go back to the model")
			}
		})
	}
}

// Edit: the edited proposal is validated and shown again; only the second
// approval applies, and both steps are recorded.
func TestEditRevalidatesAndAsksAgain(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 50)), llm.Text("Scaled to 3 as you edited."))
	n := 0
	h.human(func(p approval.Proposal) approval.Decision {
		n++
		if n == 1 {
			return approval.Decision{Action: approval.Edit,
				EditedArgs: json.RawMessage(`{"type":"deployment","namespace":"shop","name":"web","replicas":3,"reason":"three is plenty"}`)}
		}
		return approval.Decision{Action: approval.Approve}
	})
	h.run("scale it")

	props := h.seenProposals()
	if len(props) != 2 || props[0].Changes[0].After != "50" || props[1].Changes[0].After != "3" || props[0].Digest == props[1].Digest {
		t.Fatalf("proposals: %+v", props)
	}
	if h.replicas() != 3 {
		t.Fatalf("replicas = %d, want the edited 3", h.replicas())
	}
	var bodies []string
	for _, w := range h.f.Writes() {
		bodies = append(bodies, fmt.Sprintf("%v:%s", w.DryRun, w.Body))
	}
	want := `true:{"spec":{"replicas":50}} true:{"spec":{"replicas":3}} false:{"spec":{"replicas":3}}`
	if got := strings.Join(bodies, " "); got != want {
		t.Fatalf("writes:\n got %s\nwant %s", got, want)
	}
	es := h.entries()
	if len(es) != 3 || es[0].Decision.Action != audit.DecisionEdited || es[0].Outcome.Status != audit.OutcomeSuperseded ||
		es[1].Outcome.Status != audit.OutcomeApplying ||
		es[2].Decision.Action != audit.DecisionApproved || es[2].Outcome.Status != audit.OutcomeApplied {
		t.Fatalf("audit: %+v", es)
	}
}

// An edit cannot be used to get around validation.
func TestEditToSomethingInvalidAppliesNothing(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("ok"))
	h.human(func(approval.Proposal) approval.Decision {
		return approval.Decision{Action: approval.Edit, EditedArgs: json.RawMessage(`{"type":"pods","namespace":"shop","name":"web-1","replicas":0,"reason":"x"}`)}
	})
	h.run("scale it")
	res := h.toolResults(1)
	if !res[0].IsError || !strings.Contains(res[0].Content, "edited version is not valid") {
		t.Fatalf("result: %+v", res)
	}
	if len(h.f.RealWrites()) != 0 {
		t.Fatal("something was applied")
	}
}

// 7.3: no working audit trail, no change — the human is not even asked.
func TestNoAuditNoMutation(t *testing.T) {
	t.Run("no trail", func(t *testing.T) {
		h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("ok"))
		h.agent = h.newAgent(nil, agent.Limits{})
		h.human(approve)
		h.run("scale it")
		res := h.toolResults(1)
		if !res[0].IsError || !strings.Contains(res[0].Content, "audit trail is not available") {
			t.Fatalf("result: %+v", res)
		}
		if len(h.seenProposals()) != 0 || len(h.f.Writes()) != 0 {
			t.Fatal("a change was proposed without an audit trail")
		}
	})
	t.Run("trail breaks mid-session", func(t *testing.T) {
		h := newHarness(t, agent.Limits{},
			llm.Call(scaleCall("c1", 5)), llm.Call(scaleCall("c2", 7)), llm.Text("ok"))
		// The storage disappears just before the first decision is recorded.
		h.human(func(approval.Proposal) approval.Decision {
			h.trail.Close()
			return approval.Decision{Action: approval.Reject}
		})
		h.run("scale it")

		if !strings.Contains(h.notices(), "AUDIT FAILURE") || !strings.Contains(h.notices(), "NOT in the audit trail") {
			t.Fatalf("the audit failure was not reported: %q", h.notices())
		}
		second := h.toolResults(2)
		if !second[0].IsError || !strings.Contains(second[0].Content, "refused") {
			t.Fatalf("a change was proposed after the trail broke: %+v", second)
		}
		if len(h.seenProposals()) != 1 || len(h.f.RealWrites()) != 0 {
			t.Fatalf("proposals=%d writes=%d", len(h.seenProposals()), len(h.f.RealWrites()))
		}
	})
}

// The approval must be on disk before anything is sent: if that write fails,
// an approved change is NOT applied.
func TestApprovedButUnrecordableIsNotApplied(t *testing.T) {
	for name, sabotage := range map[string]func(h *harness){
		"storage fails":      func(h *harness) { h.trail.Close() },
		"audit file removed": func(h *harness) { os.Remove(h.path) },
		"audit file rotated": func(h *harness) { os.Rename(h.path, h.path+".1"); os.WriteFile(h.path, nil, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Call(scaleCall("c2", 6)), llm.Text("ok"))
			h.human(func(approval.Proposal) approval.Decision {
				sabotage(h) // while the human was reading the dialog
				return approval.Decision{Action: approval.Approve}
			})
			h.run("scale it")

			if n := len(h.f.RealWrites()); n != 0 || h.replicas() != 2 {
				t.Fatalf("applied without a record: %d writes, %d replicas", n, h.replicas())
			}
			res := h.toolResults(1)
			if !res[0].IsError || !strings.Contains(res[0].Content, "not applied") {
				t.Fatalf("result: %+v", res)
			}
			if !strings.Contains(h.notices(), "AUDIT FAILURE") {
				t.Fatalf("not reported: %q", h.notices())
			}
			// And from then on nothing is proposed at all.
			if second := h.toolResults(2); !second[0].IsError || !strings.Contains(second[0].Content, "refused") || len(h.seenProposals()) != 1 {
				t.Fatalf("second proposal after the trail broke: %+v", second)
			}
		})
	}
}

// 5.1 / 5.3: unknown and out-of-scope tools come back as errors and the loop
// continues; nothing runs.
func TestUnknownToolGoesBackAsError(t *testing.T) {
	h := newHarness(t, agent.Limits{},
		llm.Call(call("c1", "delete_resource", map[string]any{"type": "pod", "namespace": "shop", "name": "web-1"}),
			call("c2", "get_logs", map[string]any{"pod": 12})),
		llm.Text("I cannot delete it; here is how you can."),
	)
	h.human(approve)
	h.run("delete this pod")

	res := h.toolResults(1)
	if len(res) != 2 || !res[0].IsError || !strings.Contains(res[0].Content, "refused: deleting resources is outside") {
		t.Fatalf("results: %+v", res)
	}
	if !res[1].IsError || !strings.Contains(res[1].Content, "invalid arguments for get_logs") {
		t.Fatalf("malformed call: %+v", res[1])
	}
	if got := h.f.MutatingActions(); len(got) != 0 || len(h.seenProposals()) != 0 {
		t.Fatalf("something ran: %v", got)
	}
}

// Authorization errors from the cluster reach the model as such.
func TestForbiddenReadReachesModel(t *testing.T) {
	h := newHarness(t, agent.Limits{},
		llm.Call(call("c1", "list_resources", map[string]any{"type": "secrets", "namespace": "shop"})), llm.Text("You lack access."))
	h.f.Dynamic.PrependReactor("list", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no RBAC rule"))
	})
	h.run("what secrets are here?")
	res := h.toolResults(1)
	if !res[0].IsError || !strings.Contains(res[0].Content, "permission denied") || !strings.Contains(res[0].Content, "no RBAC rule") {
		t.Fatalf("result: %+v", res)
	}
}

// One request at a time.
func TestBusy(t *testing.T) {
	h := newHarness(t, agent.Limits{}, llm.Call(scaleCall("c1", 5)), llm.Text("ok"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.start(ctx, "first")
	req := <-h.gate.Requests()

	var second agent.EventDone
	h.agent.Run(context.Background(), "second", func(ev agent.Event) {
		if d, ok := ev.(agent.EventDone); ok {
			second = d
		}
	})
	if !errors.Is(second.Err, agent.ErrBusy) {
		t.Fatalf("got %v", second.Err)
	}
	if h.agent.Reset() {
		t.Fatal("the conversation was reset under a running request")
	}
	req.Decide(approval.Decision{Action: approval.Reject})
	<-done
}
