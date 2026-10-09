package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/metrics"
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

// TestLiveMetricsReadOnly reads the current context's load the way the
// metrics screen and get_metrics do, and checks that whatever the cluster
// answers is understood: readings where there are readings, and a stated
// reason — never zeros — where there are not.
//
// It is opt-in (K8S_COPILOT_LIVE=1), uses the current kubeconfig context and
// sends list requests only.
//
//	K8S_COPILOT_LIVE=1 go test ./internal/tools -run LiveMetrics -v
func TestLiveMetricsReadOnly(t *testing.T) {
	if os.Getenv("K8S_COPILOT_LIVE") != "1" {
		t.Skip("set K8S_COPILOT_LIVE=1 to run against the current kubeconfig context (read-only)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := kube.Connect(ctx, kube.ConnectOptions{UserAgent: "k8s-copilot-live-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connected: context %s, server %s", c.Info.Context, c.Info.Server)

	s := c.Metrics(ctx, kube.AllNamespaces, 0)
	for _, st := range []metrics.SourceStatus{s.Sources.NodeMetrics, s.Sources.PodMetrics, s.Sources.Nodes, s.Sources.Pods} {
		t.Logf("%-12s %s — %s", st.Source, st.Headline(), st.Reason)
		if st.State == metrics.Failed {
			t.Errorf("%s failed in a way k8s-copilot does not recognise: %s", st.Source, st.Reason)
		}
	}
	if !s.Sources.Nodes.OK() || len(s.Nodes) == 0 {
		t.Fatalf("no nodes read: %+v", s.Sources.Nodes)
	}

	r := tools.NewRegistry(c, tools.DefaultOptions())
	for _, a := range []M{{"level": "nodes"}, {"level": "namespaces"}, {"level": "pods", "namespace": "kube-system", "limit": 5}} {
		out, err := r.Read(ctx, "get_metrics", args(a))
		switch {
		case err != nil && s.HasUsage() && s.Sources.PodMetrics.OK() && s.Sources.NodeMetrics.OK():
			t.Errorf("get_metrics %v failed although the metrics API answers: %v", a, err)
		case err != nil:
			t.Logf("get_metrics %v → (as an error result)\n%v", a, err)
			if !strings.Contains(err.Error(), "UNKNOWN, not zero") {
				t.Errorf("the model is not told that usage is unknown: %v", err)
			}
		default:
			t.Logf("get_metrics %v →\n%s", a, out)
		}
	}

	if s.HasUsage() {
		// Readings: every node the cluster lists has one, or is marked.
		for _, n := range s.Nodes {
			if !n.CPU.OK && n.Ready == "Ready" {
				t.Logf("note: Ready node %s has no sample yet", n.Name)
			}
			if n.CPU.OK && n.CPUAlloc.OK && n.CPU.V > n.CPUAlloc.V*2 {
				t.Errorf("node %s: CPU usage %dm against %dm allocatable — a unit is being misread", n.Name, n.CPU.V, n.CPUAlloc.V)
			}
		}
		if s.SampleTime.IsZero() {
			t.Error("readings without a sample time")
		}
		t.Logf("freshness: %s", s.Freshness(time.Now()).Text)
	} else {
		// No readings: nothing may claim one.
		for _, n := range s.Nodes {
			if n.CPU.OK || n.Mem.OK {
				t.Errorf("node %s has a reading although the metrics API gave none", n.Name)
			}
		}
		for _, p := range s.Pods {
			if p.CPU.OK || p.Mem.OK {
				t.Errorf("pod %s has a reading although the metrics API gave none", p.Key())
			}
		}
		if s.Cluster.CPU.OK {
			t.Error("cluster totals claim a reading")
		}
	}
}
