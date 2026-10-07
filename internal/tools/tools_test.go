package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

var ctx = context.Background()

func registry(f *kubetest.Fake) *tools.Registry {
	return tools.NewRegistry(f.Cluster, tools.DefaultOptions())
}

func args(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type M = map[string]any

// grant obtains a real approval.Grant the only way there is: through the
// gate, with something answering as the human.
func grant(t *testing.T, p *tools.Plan, action approval.Action) *approval.Grant {
	t.Helper()
	prop, err := p.Proposal("test intent")
	if err != nil {
		t.Fatal(err)
	}
	g := approval.NewGate()
	go func() { (<-g.Requests()).Decide(approval.Decision{Action: action}) }()
	_, gr, err := g.Ask(ctx, prop)
	if err != nil {
		t.Fatal(err)
	}
	return gr
}

// 5.1: the registry is exactly the v1 set, correctly tiered.
func TestRegistryContents(t *testing.T) {
	r := registry(kubetest.New())
	want := map[string]tools.Tier{
		"list_resources": tools.Read, "get_resource": tools.Read, "describe_resource": tools.Read,
		"get_logs": tools.Read, "get_events": tools.Read,
		"scale": tools.Mutate, "rollout_restart": tools.Mutate, "set_labels": tools.Mutate, "set_annotations": tools.Mutate,
	}
	specs := r.Specs()
	if len(specs) != len(want) {
		t.Fatalf("registry has %d tools, want exactly %d", len(specs), len(want))
	}
	for _, s := range specs {
		tier, ok := want[s.Name]
		if !ok || tier != s.Tier {
			t.Errorf("unexpected tool %q (tier %s)", s.Name, s.Tier)
		}
		var schema map[string]any
		if err := json.Unmarshal(s.Schema, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s: bad schema: %v", s.Name, err)
		}
	}
}

// 5.1: unknown or malformed calls return a descriptive error and execute
// nothing.
func TestUnknownAndMalformedCalls(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 2))
	r := registry(f)

	cases := []struct {
		name, tool string
		args       string
		mutate     bool
		wantErr    string
	}{
		{"unknown tool", "frobnicate", `{}`, false, `unknown tool "frobnicate"`},
		{"unknown field", "get_resource", `{"type":"pods","name":"x","namespace":"a","bogus":1}`, false, `unknown field "bogus"`},
		{"wrong type", "list_resources", `{"type":42}`, false, "cannot unmarshal number"},
		{"missing required", "get_resource", `{"type":"pods","namespace":"a"}`, false, `"name" is required`},
		{"not json", "get_logs", `not json`, false, "invalid arguments for get_logs"},
		{"args is a string", "get_logs", `"{\"pod\":\"x\"}"`, false, "cannot unmarshal string"},
		{"missing namespace for namespaced type", "get_resource", `{"type":"pods","name":"x"}`, false, `"namespace" is required`},
		{"scale without replicas", "scale", `{"type":"deployment","namespace":"shop","name":"web","reason":"r"}`, true, `"replicas" is required`},
		{"scale negative", "scale", `{"type":"deployment","namespace":"shop","name":"web","replicas":-1,"reason":"r"}`, true, "cannot be negative"},
		{"scale without reason", "scale", `{"type":"deployment","namespace":"shop","name":"web","replicas":3}`, true, `"reason" is required`},
		{"labels with nothing to do", "set_labels", `{"type":"deployment","namespace":"shop","name":"web","reason":"r"}`, true, "at least one key"},
		{"invalid label key", "set_labels", `{"type":"deployment","namespace":"shop","name":"web","set":{"bad key!":"v"},"reason":"r"}`, true, "not a valid key"},
		{"invalid label value", "set_labels", `{"type":"deployment","namespace":"shop","name":"web","set":{"k":"not valid!"},"reason":"r"}`, true, "not a valid value"},
		{"key in set and remove", "set_labels", `{"type":"deployment","namespace":"shop","name":"web","set":{"k":"v"},"remove":["k"],"reason":"r"}`, true, "both"},
		{"smuggled field on mutate", "scale", `{"type":"deployment","namespace":"shop","name":"web","replicas":3,"reason":"r","image":"evil"}`, true, `unknown field "image"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if c.mutate {
				_, err = r.Plan(ctx, c.tool, json.RawMessage(c.args))
			} else {
				_, err = r.Read(ctx, c.tool, json.RawMessage(c.args))
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("malformed calls reached the cluster: %+v", w)
	}
}

// The tiers cannot be crossed: a mutate tool cannot be run as a read, and a
// read tool cannot be planned.
func TestTiersCannotBeCrossed(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 2))
	r := registry(f)
	if _, err := r.Read(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 5, "reason": "r"})); err == nil {
		t.Fatal("a mutate tool ran through Read")
	}
	if _, err := r.Plan(ctx, "get_resource", args(M{"type": "pods", "namespace": "shop", "name": "x"})); err == nil {
		t.Fatal("a read tool was planned as a mutation")
	}
	if len(f.MutatingActions()) != 0 {
		t.Fatal(f.MutatingActions())
	}
}

// 5.3 / 8.4: delete, apply/patch, image changes and friends are refused with
// an explanation, by the registry.
func TestUnsupportedMutationsAreRefused(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 2), kubetest.Pod("shop", "web-1"))
	r := registry(f)
	for _, name := range []string{
		"delete_resource", "delete_pod", "apply_manifest", "kubectl_apply", "patch_resource",
		"set_image", "update_image", "exec", "run_command", "drain_node", "cordon", "create_resource", "rollback",
	} {
		for _, call := range []func() error{
			func() error { _, err := r.Read(ctx, name, args(M{"namespace": "shop", "name": "web"})); return err },
			func() error { _, err := r.Plan(ctx, name, args(M{"namespace": "shop", "name": "web"})); return err },
		} {
			err := call()
			var unknown *tools.UnknownToolError
			if !errors.As(err, &unknown) {
				t.Fatalf("%s: got %v", name, err)
			}
			for _, want := range []string{"refused", "outside the set of changes", "nothing was executed", "cannot delete resources"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: refusal lacks %q: %s", name, want, err)
				}
			}
		}
	}

	// The supported tools cannot be bent into the unsupported ones either.
	bent := []struct{ tool, argsJSON, want string }{
		{"scale", `{"type":"pods","namespace":"shop","name":"web-1","replicas":0,"reason":"r"}`, "only works on deployments, statefulsets, replicasets"},
		{"rollout_restart", `{"type":"pods","namespace":"shop","name":"web-1","reason":"r"}`, "only works on"},
		{"set_annotations", `{"type":"deployment","namespace":"shop","name":"web","set":{"kubectl.kubernetes.io/last-applied-configuration":"{}"},"reason":"r"}`, "system-managed"},
		{"set_annotations", `{"type":"deployment","namespace":"shop","name":"web","set":{"kubectl.kubernetes.io/restartedAt":"now"},"reason":"r"}`, "use rollout_restart"},
		{"set_labels", `{"type":"pods","namespace":"shop","name":"web-1","remove":["pod-template-hash"],"reason":"r"}`, "system-managed"},
		{"scale", `{"type":"deployment","namespace":"shop","name":"web","replicas":100000,"reason":"r"}`, "above the configured maximum"},
	}
	for _, b := range bent {
		_, err := r.Plan(ctx, b.tool, json.RawMessage(b.argsJSON))
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s %s: got %v, want %q", b.tool, b.argsJSON, err, b.want)
		}
	}
	if len(f.Writes()) != 0 {
		t.Fatalf("refused calls reached the cluster: %+v", f.Writes())
	}
}

// 5.2: read tools return their results.
func TestReadTools(t *testing.T) {
	now := metav1.Now()
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db", Annotations: map[string]string{
			"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"aHVudGVyMg=="}}`}},
		Data: map[string][]byte{"password": []byte("hunter2")},
	}
	f := kubetest.New(
		kubetest.Pod("shop", "web-1"), kubetest.Pod("ops", "cron-1"), kubetest.Deployment("shop", "web", 2),
		kubetest.Event("shop", "e1", "web-1", "Warning", "BackOff", "Back-off restarting failed container", now),
		kubetest.Widget("shop", "w1"), secret,
	)
	f.SetLogs("shop", "", false, "starting\npanic: missing DATABASE_URL\n")
	f.SetLogs("shop", "", true, "previous run output\n")
	r := registry(f)

	read := func(tool string, a M) string {
		t.Helper()
		out, err := r.Read(ctx, tool, args(a))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return out
	}

	out := read("list_resources", M{"type": "pods"})
	for _, want := range []string{"all namespaces", "2 found", "NAMESPACE", "web-1", "cron-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("list (all namespaces) lacks %q:\n%s", want, out)
		}
	}
	out = read("list_resources", M{"type": "po", "namespace": "shop"})
	if !strings.Contains(out, "web-1") || strings.Contains(out, "cron-1") {
		t.Errorf("namespaced list wrong:\n%s", out)
	}
	if out = read("list_resources", M{"type": "widgets"}); !strings.Contains(out, "w1") {
		t.Errorf("custom resource not listed:\n%s", out)
	}
	if out = read("list_resources", M{"type": "services", "namespace": "shop"}); !strings.Contains(out, "0 found") || !strings.Contains(out, "succeeded and returned no resources") {
		t.Errorf("empty listing is not explicit:\n%s", out)
	}

	out = read("get_resource", M{"type": "deployment", "namespace": "shop", "name": "web"})
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "replicas: 2") {
		t.Errorf("get_resource:\n%s", out)
	}

	out = read("describe_resource", M{"type": "pod", "namespace": "shop", "name": "web-1"})
	for _, want := range []string{"Kind: Pod", "Status: Running", "Events (oldest first)", "BackOff", "Back-off restarting"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe lacks %q:\n%s", want, out)
		}
	}

	if out = read("get_logs", M{"namespace": "shop", "pod": "web-1"}); !strings.Contains(out, "panic: missing DATABASE_URL") {
		t.Errorf("logs:\n%s", out)
	}
	if out = read("get_logs", M{"namespace": "shop", "pod": "web-1", "previous": true}); !strings.Contains(out, "previous run output") {
		t.Errorf("previous logs:\n%s", out)
	}
	if out = read("get_logs", M{"namespace": "ops", "pod": "cron-1"}); !strings.Contains(out, "no log output") {
		t.Errorf("empty logs are not explicit: %q", out)
	}

	if out = read("get_events", M{"namespace": "shop", "type": "pod", "name": "web-1"}); !strings.Contains(out, "BackOff") {
		t.Errorf("events:\n%s", out)
	}
	if out = read("get_events", M{"namespace": "ops"}); !strings.Contains(out, "no matching events") {
		t.Errorf("empty events are not explicit: %q", out)
	}

	// Secret values never reach the model.
	out = read("get_resource", M{"type": "secret", "namespace": "shop", "name": "db"})
	if strings.Contains(out, "aHVudGVyMg") || strings.Contains(out, "hunter2") {
		t.Fatalf("secret value leaked to the model:\n%s", out)
	}
	if !strings.Contains(out, "password: <redacted by k8s-copilot>") {
		t.Errorf("redaction marker missing:\n%s", out)
	}

	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("read tools changed something: %v", got)
	}
}

// Permission failures are reported as such, never as "nothing found".
func TestReadToolSurfacesForbidden(t *testing.T) {
	f := kubetest.New()
	f.Dynamic.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	_, err := registry(f).Read(ctx, "list_resources", args(M{"type": "pods", "namespace": "shop"}))
	if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "not an absence of resources") {
		t.Fatalf("got %v", err)
	}
}

func TestReadResultIsCapped(t *testing.T) {
	f := kubetest.New(kubetest.Pod("shop", "web-1"))
	f.SetLogs("shop", "", false, strings.Repeat("line of log output\n", 5000)+"THE LAST LINE\n")
	r := tools.NewRegistry(f.Cluster, tools.Options{MaxResultBytes: 2000})
	out, err := r.Read(ctx, "get_logs", args(M{"namespace": "shop", "pod": "web-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 2200 || !strings.Contains(out, "THE LAST LINE") || !strings.Contains(out, "omitted") {
		t.Fatalf("len=%d; tail kept=%v", len(out), strings.Contains(out, "THE LAST LINE"))
	}
}

// 5.3: each verb sends exactly the intended patch, and only after approval.
func TestMutationPatchShapes(t *testing.T) {
	dep := kubetest.Deployment("shop", "web", 2)
	dep.Annotations = map[string]string{"team": "payments"}
	f := kubetest.New(dep, kubetest.Pod("shop", "web-1"), kubetest.Namespace("shop"))
	r := registry(f)

	cases := []struct {
		tool      string
		args      M
		wantRes   string
		wantSub   string
		wantBody  string // exact, or a prefix when it ends in "…"
		wantTitle string
		check     func(t *testing.T)
	}{
		{
			tool: "scale", args: M{"type": "deploy", "namespace": "shop", "name": "web", "replicas": 5, "reason": "more load"},
			wantRes: "deployments", wantSub: "scale", wantBody: `{"spec":{"replicas":5}}`,
			wantTitle: "Scale Deployment shop/web from 2 to 5 replicas",
			check: func(t *testing.T) {
				d, _ := f.Get(ctx, mustResolve(t, f, "deploy"), "shop", "web")
				if n, _, _ := nestedInt(d.Object, "spec", "replicas"); n != 5 {
					t.Fatalf("replicas = %d after apply", n)
				}
			},
		},
		{
			tool: "rollout_restart", args: M{"type": "deployment", "namespace": "shop", "name": "web", "reason": "stuck"},
			wantRes: "deployments", wantBody: `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"…`,
			wantTitle: "Restart Deployment shop/web",
		},
		{
			tool: "set_labels", args: M{"type": "deployment", "namespace": "shop", "name": "web", "set": M{"tier": "frontend"}, "remove": []string{"app"}, "reason": "r"},
			wantRes: "deployments", wantBody: `{"metadata":{"labels":{"app":null,"tier":"frontend"}}}`,
			wantTitle: "Change 2 labels on Deployment shop/web",
		},
		{
			tool: "set_annotations", args: M{"type": "deployment", "namespace": "shop", "name": "web", "set": M{"team": "checkout", "note": "see INC-1"}, "reason": "r"},
			wantRes: "deployments", wantBody: `{"metadata":{"annotations":{"note":"see INC-1","team":"checkout"}}}`,
			wantTitle: "Change 2 annotations on Deployment shop/web",
		},
		{
			tool: "set_labels", args: M{"type": "namespace", "name": "shop", "set": M{"env": "prod"}, "reason": "r"},
			wantRes: "namespaces", wantBody: `{"metadata":{"labels":{"env":"prod"}}}`,
			wantTitle: "Change 1 label on Namespace shop",
		},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.wantRes, func(t *testing.T) {
			before := len(f.Writes())
			p, err := r.Plan(ctx, c.tool, args(c.args))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(p.Title, c.wantTitle) {
				t.Errorf("title %q, want prefix %q", p.Title, c.wantTitle)
			}
			if got := len(f.Writes()); got != before {
				t.Fatal("planning sent a request")
			}

			// Without dry-run and approval nothing can be applied.
			if _, err := p.Apply(ctx, nil); !errors.Is(err, tools.ErrNotApproved) {
				t.Fatalf("Apply without a grant: %v", err)
			}
			if err := p.DryRun(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Apply(ctx, nil); !errors.Is(err, tools.ErrNotApproved) {
				t.Fatalf("Apply with a nil grant after dry-run: %v", err)
			}
			if _, err := p.Apply(ctx, &approval.Grant{}); !errors.Is(err, tools.ErrNotApproved) {
				t.Fatalf("Apply with a forged grant: %v", err)
			}
			if got := len(f.Writes()) - before; got != 1 {
				t.Fatalf("unapproved Apply attempts reached the cluster: %+v", f.Writes()[before:])
			}

			result, err := p.Apply(ctx, grant(t, p, approval.Approve))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(result, "applied") {
				t.Errorf("result: %q", result)
			}

			ws := f.Writes()[before:]
			if len(ws) != 2 || !ws[0].DryRun || ws[1].DryRun {
				t.Fatalf("want exactly [dry-run, real], got %+v", ws)
			}
			for _, w := range ws {
				if w.Resource != c.wantRes || w.Subresource != c.wantSub || w.PatchType != "application/merge-patch+json" || w.Name != c.args["name"] {
					t.Errorf("request: %+v", w)
				}
				if prefix, isPrefix := strings.CutSuffix(c.wantBody, "…"); isPrefix {
					if !strings.HasPrefix(w.Body, prefix) {
						t.Errorf("body %s, want prefix %s", w.Body, prefix)
					}
				} else if w.Body != c.wantBody {
					t.Errorf("body %s, want %s", w.Body, c.wantBody)
				}
			}
			if ws[0].Body != ws[1].Body {
				t.Errorf("the applied request differs from the validated one:\n%s\n%s", ws[0].Body, ws[1].Body)
			}
			if c.check != nil {
				c.check(t)
			}
		})
	}
	// Across all of that, the only mutating verb ever used was patch.
	for _, a := range f.MutatingActions() {
		if !strings.HasPrefix(a, "patch ") {
			t.Fatalf("unexpected mutating action %q", a)
		}
	}
}

func mustResolve(t *testing.T, f *kubetest.Fake, name string) kube.ResourceType {
	t.Helper()
	ty, err := f.Resolve(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return ty
}

func nestedInt(obj map[string]any, path ...string) (int64, bool, error) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false, nil
		}
		cur = m[p]
	}
	switch n := cur.(type) {
	case int64:
		return n, true, nil
	case float64:
		return int64(n), true, nil
	}
	return 0, false, nil
}

// A dry-run changes nothing, and a grant for one plan does not open another.
func TestDryRunChangesNothingAndGrantsAreBound(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 2), kubetest.Deployment("shop", "api", 2))
	r := registry(f)
	web, err := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 0, "reason": "r"}))
	if err != nil {
		t.Fatal(err)
	}
	api, err := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "api", "replicas": 0, "reason": "r"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := web.DryRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := api.DryRun(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("dry-run changed something: %v", got)
	}

	// A proposal is only available once dry-run has passed.
	fresh, _ := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 7, "reason": "r"}))
	if _, err := fresh.Proposal("x"); err == nil {
		t.Fatal("a proposal was produced for a plan that never passed dry-run")
	}

	webGrant := grant(t, web, approval.Approve)
	if _, err := api.Apply(ctx, webGrant); !errors.Is(err, tools.ErrNotApproved) {
		t.Fatalf("a grant for web applied api: %v", err)
	}
	if rejected := grant(t, api, approval.Reject); rejected != nil {
		t.Fatal("a rejection produced a grant")
	}
	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("something was applied: %v", got)
	}
	if !strings.Contains(strings.Join(web.Warnings, " "), "Scaling to 0") {
		t.Errorf("no warning for scale-to-zero: %v", web.Warnings)
	}
}

// If the object changed between proposal and approval, the approval no
// longer describes what would happen: refuse.
func TestApplyRefusesWhenStateMoved(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 2))
	r := registry(f)
	p, err := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 5, "reason": "r"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.DryRun(ctx); err != nil {
		t.Fatal(err)
	}
	g := grant(t, p, approval.Approve)

	// Someone else scales it while the human is reading the dialog.
	other, _ := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 9, "reason": "r"}))
	other.DryRun(ctx)
	if _, err := other.Apply(ctx, grant(t, other, approval.Approve)); err != nil {
		t.Fatal(err)
	}

	_, err = p.Apply(ctx, g)
	var stale *tools.StaleError
	if !errors.As(err, &stale) || !strings.Contains(err.Error(), "now has 9 replicas, not the 2 shown") {
		t.Fatalf("got %v", err)
	}
	if n := len(f.RealWrites()); n != 1 {
		t.Fatalf("%d real writes, want only the other actor's", n)
	}
}

func TestNoOpAndGuards(t *testing.T) {
	dep := kubetest.Deployment("kube-system", "coredns", 2)
	paused := kubetest.Deployment("shop", "paused", 1)
	paused.Spec.Paused = true
	f := kubetest.New(dep, paused, kubetest.Deployment("shop", "web", 2))

	r := registry(f)
	if _, err := r.Plan(ctx, "scale", args(M{"type": "deployment", "namespace": "shop", "name": "web", "replicas": 2, "reason": "r"})); err == nil || !strings.Contains(err.Error(), "already has 2 replicas") {
		t.Fatalf("no-op scale: %v", err)
	}
	if _, err := r.Plan(ctx, "set_labels", args(M{"type": "deployment", "namespace": "shop", "name": "web", "set": M{"app": "web"}, "reason": "r"})); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("no-op labels: %v", err)
	}
	if _, err := r.Plan(ctx, "rollout_restart", args(M{"type": "deployment", "namespace": "shop", "name": "paused", "reason": "r"})); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused restart: %v", err)
	}
	p, err := r.Plan(ctx, "rollout_restart", args(M{"type": "deployment", "namespace": "kube-system", "name": "coredns", "reason": "r"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "system namespace") || p.Reversible {
		t.Errorf("restart in kube-system: warnings=%v reversible=%v", p.Warnings, p.Reversible)
	}

	protected := tools.NewRegistry(f.Cluster, tools.Options{ProtectedNamespaces: []string{"kube-system"}})
	if _, err := protected.Plan(ctx, "rollout_restart", args(M{"type": "deployment", "namespace": "kube-system", "name": "coredns", "reason": "r"})); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected namespace: %v", err)
	}
	if len(f.Writes()) != 0 {
		t.Fatal(f.Writes())
	}
}

// The gate itself: it waits, and cancellation never yields a grant.
func TestGateBlocksUntilDecision(t *testing.T) {
	g := approval.NewGate()
	type result struct {
		d   approval.Decision
		gr  *approval.Grant
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, gr, err := g.Ask(ctx, approval.Proposal{ID: "p1", Digest: "abc"})
		done <- result{d, gr, err}
	}()
	req := <-g.Requests()
	select {
	case r := <-done:
		t.Fatalf("Ask returned without a decision: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	req.Decide(approval.Decision{Action: approval.Approve})
	req.Decide(approval.Decision{Action: approval.Reject}) // only the first counts
	r := <-done
	if r.err != nil || r.d.Action != approval.Approve || !r.gr.Covers("abc") || r.gr.Covers("other") {
		t.Fatalf("%+v", r)
	}

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		d, gr, err := g.Ask(cctx, approval.Proposal{ID: "p2", Digest: "abc"})
		done <- result{d, gr, err}
	}()
	late := <-g.Requests()
	cancel()
	r = <-done
	if !errors.Is(r.err, approval.ErrCancelled) || r.gr != nil {
		t.Fatalf("cancelled Ask: %+v", r)
	}
	late.Decide(approval.Decision{Action: approval.Approve}) // too late; must be harmless

	var zero approval.Decision
	if zero.Action != approval.Reject {
		t.Fatal("the zero Decision must be a rejection")
	}
	if (*approval.Grant)(nil).Covers("") || (&approval.Grant{}).Covers("") {
		t.Fatal("an empty grant covers something")
	}
}
