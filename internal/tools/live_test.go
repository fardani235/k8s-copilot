package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

// TestLiveClusterReadOnly checks the things fakes cannot: that a real API
// server prints Tables for us, and that it accepts the exact patch shapes the
// mutate tools send — using dry-run only.
//
// It is opt-in (K8S_COPILOT_LIVE=1) and uses the current kubeconfig context. It
// reads, and sends dryRun=All requests, which the server validates and does
// not persist. It never applies anything; it asserts afterwards that nothing
// changed.
//
//	K8S_COPILOT_LIVE=1 go test ./internal/tools -run Live -v
//
// K8S_COPILOT_LIVE_DEPLOYMENT=namespace/name picks the Deployment to dry-run
// against (default kube-system/coredns).
func TestLiveClusterReadOnly(t *testing.T) {
	if os.Getenv("K8S_COPILOT_LIVE") != "1" {
		t.Skip("set K8S_COPILOT_LIVE=1 to run against the current kubeconfig context (read-only + dry-run)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := kube.Connect(ctx, kube.ConnectOptions{UserAgent: "k8s-copilot-live-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connected: context %s, server %s", c.Info.Context, c.Info.Server)

	types, warn, err := c.Types(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if warn != nil {
		t.Logf("discovery warning: %v", warn)
	}
	crds := 0
	for _, ty := range types {
		if strings.Contains(ty.Group, ".") && !strings.HasSuffix(ty.Group, ".k8s.io") {
			crds++
		}
	}
	t.Logf("%d listable resource types (%d from non-core API groups)", len(types), crds)

	nsName, depName := "kube-system", "coredns"
	if v := os.Getenv("K8S_COPILOT_LIVE_DEPLOYMENT"); v != "" {
		var ok bool
		if nsName, depName, ok = strings.Cut(v, "/"); !ok {
			t.Fatal("K8S_COPILOT_LIVE_DEPLOYMENT must be namespace/name")
		}
	}

	pods, err := c.Resolve(ctx, "pods")
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := c.List(ctx, pods, nsName, 0)
	if err != nil {
		t.Fatal(err)
	}
	cols := strings.Join(tbl.Columns, " ")
	if !tbl.ServerColumns || !strings.Contains(cols, "READY") || !strings.Contains(cols, "STATUS") || !strings.Contains(cols, "RESTARTS") {
		t.Fatalf("expected the server's own pod columns, got %q (server columns: %v)", cols, tbl.ServerColumns)
	}
	t.Logf("pods in %s:\n%s", nsName, tools.FormatTable(tbl))

	nodes, _ := c.Resolve(ctx, "nodes")
	ntbl, err := c.List(ctx, nodes, "ignored-for-cluster-scoped", 0)
	if err != nil || len(ntbl.Rows) == 0 || strings.Contains(strings.Join(ntbl.Columns, " "), "NAMESPACE") {
		t.Fatalf("nodes: %v %+v", err, ntbl)
	}

	r := tools.NewRegistry(c, tools.DefaultOptions())
	out, err := r.Read(ctx, "describe_resource", args(M{"type": "deployment", "namespace": nsName, "name": depName}))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("describe:\n%s", out)

	deploys, _ := c.Resolve(ctx, "deployments")
	before, err := c.Get(ctx, deploys, nsName, depName)
	if err != nil {
		t.Fatal(err)
	}
	replicas, _, _ := nestedInt(before.Object, "spec", "replicas")

	for _, tc := range []struct {
		tool string
		args M
	}{
		{"scale", M{"type": "deployment", "namespace": nsName, "name": depName, "replicas": replicas + 1, "reason": "live dry-run test"}},
		{"rollout_restart", M{"type": "deployment", "namespace": nsName, "name": depName, "reason": "live dry-run test"}},
		{"set_labels", M{"type": "deployment", "namespace": nsName, "name": depName, "set": M{"k8s-copilot-live-test": "x"}, "reason": "live dry-run test"}},
		{"set_annotations", M{"type": "deployment", "namespace": nsName, "name": depName, "set": M{"k8s-copilot.dev/live-test": "x"}, "reason": "live dry-run test"}},
	} {
		p, err := r.Plan(ctx, tc.tool, args(tc.args))
		if err != nil {
			t.Fatalf("%s: plan: %v", tc.tool, err)
		}
		if err := p.DryRun(ctx); err != nil {
			t.Fatalf("%s: the API server rejected the dry-run: %v\n%s", tc.tool, err, p.Request())
		}
		t.Logf("%s: dry-run accepted — %s\n%s", tc.tool, p.Title, p.Request())
	}

	after, err := c.Get(ctx, deploys, nsName, depName)
	if err != nil {
		t.Fatal(err)
	}
	if before.GetResourceVersion() != after.GetResourceVersion() {
		// Controllers update status all the time, so compare what we touch.
		a, _, _ := nestedInt(after.Object, "spec", "replicas")
		if a != replicas || after.GetLabels()["k8s-copilot-live-test"] != "" || after.GetAnnotations()["k8s-copilot.dev/live-test"] != "" || after.GetGeneration() != before.GetGeneration() {
			t.Fatalf("the dry-runs changed the deployment")
		}
	}
	t.Logf("verified: %s/%s unchanged (generation %d, %d replicas)", nsName, depName, after.GetGeneration(), replicas)
}
