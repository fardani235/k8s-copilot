package tools_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

func loadedCluster() *kubetest.Fake {
	pending := kubetest.PodOn("shop", "web-pending", "", kubetest.Container("app", "100m", "", "64Mi", ""))
	pending.Status.Phase = corev1.PodPending
	f := kubetest.New(
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
	return f
}

func wantAll(t *testing.T, what, out string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(out, sub) {
			t.Errorf("%s lacks %q:\n%s", what, sub, out)
		}
	}
}

// The copilot can read load at every level of the hierarchy.
func TestMetricsTool(t *testing.T) {
	f := loadedCluster()
	r := registry(f)
	read := func(a M) string {
		t.Helper()
		out, err := r.Read(ctx, "get_metrics", args(a))
		if err != nil {
			t.Fatalf("get_metrics %v: %v", a, err)
		}
		return out
	}

	out := read(M{"level": "nodes"})
	wantAll(t, "nodes", out,
		"sampled", "15s window", "pods read for all namespaces",
		"cluster: CPU 3.1 of 6 allocatable (52%) · memory 9.8Gi of 12Gi allocatable (82%)",
		"2 nodes · 4 pods in all namespaces",
		"nodes, by cpu:", "NODE    CPU  %CPU    MEM  %MEM  PODS",
		"node-b  1.9   95%  3.8Gi   95%     1  <- HIGH: CPU 95% of allocatable, memory 95% of allocatable",
		"node-a  1.2   30%    6Gi   75%     2  <- elevated: memory 75% of allocatable",
		"is no reading (unknown), never zero")
	if strings.Index(out, "node-b") > strings.Index(out, "node-a  1.2") {
		t.Errorf("not sorted by CPU, busiest first:\n%s", out)
	}

	out = read(M{"level": "namespaces", "sort": "name"})
	wantAll(t, "namespaces", out, "namespaces, by name:", "ops           1    0m  no req     8Mi  no req", "shop          3  647m     72%  1022Mi     74%")

	out = read(M{"level": "pods"})
	wantAll(t, "pods", out,
		"shop       web-1        490m    163%  no lim  510Mi    177%  no lim  node-a",
		"shop       db-0         157m     31%     16%  512Mi     50%     50%  node-b",
		"ops        cron-1         0m  no req  no lim    8Mi  no req  no lim  node-a",
		"shop       web-pending     —       —       —      —       —       —  <none>",
		"note: 1 pod shown as — (1 not running, so there is nothing to measure). That is unknown, not zero.")

	out = read(M{"level": "pods", "namespace": "shop", "sort": "memory", "limit": 1})
	wantAll(t, "pods of a namespace", out,
		"pods read for namespace shop", "namespace shop: CPU 647m of 900m requested (72%)",
		"pods in namespace shop, by memory — the top 1 of 3:", "db-0")
	if strings.Contains(out, "web-1") || strings.Contains(out, "cron-1") {
		t.Errorf("limit or namespace not applied:\n%s", out)
	}

	out = read(M{"level": "pods", "node": "node-a"})
	wantAll(t, "pods of a node", out, "node node-a: CPU 1.2 of 4 allocatable (30%)", "pods on node node-a, by cpu:", "web-1", "cron-1")
	if strings.Contains(out, "db-0") {
		t.Errorf("a pod of another node is listed:\n%s", out)
	}

	out = read(M{"level": "containers", "namespace": "shop", "pod": "web-1"})
	wantAll(t, "containers", out,
		"pod shop/web-1: CPU 490m, no limit, 300m requested · memory 510Mi, no limit, 288Mi requested", "on node node-a · Running",
		"app        480m     250m     500m     96%  490Mi    256Mi    512Mi     96%         0  -          <- HIGH: CPU 96% of limit, memory 96% of limit",
		"sidecar     10m      50m     none  no lim   20Mi     32Mi     none  no lim         0  -")

	// A pod that is not there is said to be not there; the source did answer.
	out = read(M{"level": "containers", "namespace": "shop", "pod": "gone"})
	wantAll(t, "missing pod", out, "Pod shop/gone is not in the readings", "The metrics API did answer")

	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("reading metrics changed something: %v", got)
	}
}

// When the source is missing the model is told so as an error, in the
// screen's own words, and told what that does and does not mean.
func TestMetricsToolSaysWhenTheSourceIsMissing(t *testing.T) {
	read := func(f *kubetest.Fake, a M) string {
		t.Helper()
		out, err := registry(f).Read(ctx, "get_metrics", args(a))
		if err == nil {
			t.Fatalf("get_metrics %v returned readings from a source that is not there:\n%s", a, out)
		}
		return err.Error()
	}
	never := []string{"0m", "0%", "CPU  %CPU", "POD "}

	absent := read(kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1")), M{"level": "pods"})
	wantAll(t, "absent", absent, "metrics unavailable: Metrics are not available on this cluster",
		"does not serve the metrics API (metrics.k8s.io)", "installs nothing", "could not find the requested resource",
		"Usage is UNKNOWN, not zero", "do not describe anything as idle")

	f := loadedCluster()
	f.FailMetrics(apierrors.NewServiceUnavailable("the server is currently unable to handle the request"))
	// Fresh registry, so there are no earlier readings to hold over.
	broken := read(f, M{"level": "nodes"})
	wantAll(t, "broken", broken, "metrics unavailable: The metrics API is not answering", "registered on this cluster but nothing healthy is answering", "UNKNOWN, not zero")

	f = loadedCluster()
	f.Dynamic.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		gvr := a.GetResource()
		if gvr.Resource == "pods" && a.GetNamespace() == "shop" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("RBAC: access denied"))
	})
	denied := read(f, M{"level": "pods"})
	wantAll(t, "denied", denied, "permission denied: Permission denied reading pod metrics across all namespaces",
		"authorization failure, not an idle cluster", "RBAC: access denied", `call again with "namespace" set`)
	if out, err := registry(f).Read(ctx, "get_metrics", args(M{"level": "pods", "namespace": "shop"})); err != nil || !strings.Contains(out, "web-1") {
		t.Fatalf("the namespace the user may read: %v\n%s", err, out)
	}
	deniedNodes := read(f, M{"level": "nodes"})
	wantAll(t, "denied nodes", deniedNodes, "permission denied: Permission denied reading node metrics")

	for name, text := range map[string]string{"absent": absent, "broken": broken, "denied": denied} {
		for _, bad := range never {
			if strings.Contains(text, bad) {
				t.Errorf("%s: a missing source produced what looks like a reading (%q):\n%s", name, bad, text)
			}
		}
	}
}

// A refresh that fails after a good one is passed on as old, not as current.
func TestMetricsToolFlagsHeldReadings(t *testing.T) {
	f := loadedCluster()
	f.Metrics(ctx, kube.AllNamespaces, 0)
	f.FailMetrics(apierrors.NewServiceUnavailable("metrics-server is down"))
	held := f.Metrics(ctx, kube.AllNamespaces, 0)
	if held.Held == nil {
		t.Fatal("the readings are not marked as held")
	}
	q := metrics.Query{Level: metrics.LevelNodes}
	out, err := tools.MetricsReport(held, held.List(q), time.Now(), 30)
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, "held", out, "refresh failing since",
		"WARNING: the latest refresh failed (The metrics API is not answering), so these are the last readings that were obtained, not current ones",
		"node-b  1.9   95%")

	// A question about one namespace gets the same held readings, with the
	// same warning — not "unavailable" while the screen still shows numbers.
	out, err = registry(f).Read(ctx, "get_metrics", args(M{"level": "pods", "namespace": "shop"}))
	if err != nil {
		t.Fatalf("held on the screen, unavailable to the copilot: %v", err)
	}
	wantAll(t, "held, one namespace", out, "WARNING: the latest refresh failed", "db-0", "web-1")

	// An old sample is flagged too.
	f.FailMetrics(nil)
	now := f.Metrics(ctx, kube.AllNamespaces, 0)
	out, err = tools.MetricsReport(now, now.List(q), time.Now().Add(10*time.Minute), 30)
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, "old", out, "OLD", "WARNING: the newest sample is old")
}

func TestMetricsToolArguments(t *testing.T) {
	r := registry(loadedCluster())
	for _, tc := range []struct {
		a    M
		want string
	}{
		{M{}, `"level" is required`},
		{M{"level": "cluster"}, `"level" must be one of nodes, namespaces, pods, containers`},
		{M{"level": "nodes", "namespace": "shop"}, `level "nodes" lists every node`},
		{M{"level": "namespaces", "node": "node-a"}, `takes no "node"`},
		{M{"level": "pods", "pod": "web-1"}, `"pod" is for level "containers"`},
		{M{"level": "containers", "pod": "web-1"}, `needs both "namespace" and "pod"`},
		{M{"level": "pods", "sort": "restarts"}, `"sort" must be cpu, memory or name`},
		{M{"level": "pods", "window": "5m"}, `unknown field "window"`},
		{M{"level": "pods", "namespace": "a/b"}, `"a/b" is not a valid namespace name`},
		{M{"level": "containers", "namespace": "shop", "pod": "web 1"}, `"web 1" is not a valid pod name`},
		{M{"level": "pods", "node": "node\na"}, `is not a valid node name`},
	} {
		_, err := r.Read(ctx, "get_metrics", args(tc.a))
		if err == nil || !strings.Contains(err.Error(), "invalid arguments for get_metrics") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want an argument error containing %q", tc.a, err, tc.want)
		}
	}
	// "*" means all namespaces, as it does for the other tools.
	if out, err := r.Read(ctx, "get_metrics", args(M{"level": "pods", "namespace": "*"})); err != nil || !strings.Contains(out, "cron-1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := r.Plan(ctx, "get_metrics", args(M{"level": "nodes"})); err == nil {
		t.Fatal("get_metrics can be planned as a mutation")
	}
}

// A long listing is shortened by showing fewer rows — and saying how many —
// never by cutting off the caveats that say how to read it.
func TestMetricsToolKeepsItsCaveatsWhenShortened(t *testing.T) {
	f := kubetest.New(kubetest.Node("node-a", "64", "256Gi"))
	f.SetNodeMetrics("node-a", "8", "64Gi")
	for i := 0; i < 220; i++ {
		name := fmt.Sprintf("payments-reconciliation-worker-%03d-7d764666f9-x%04d", i, i)
		f.Dynamic.Tracker().Add(kubetest.PodOn("a-namespace-with-a-realistic-name", name, "node-a", kubetest.Container("worker", "100m", "", "128Mi", "")))
		if i%2 == 0 {
			f.SetPodMetrics("a-namespace-with-a-realistic-name", name, kubetest.Usage{Container: "worker", CPU: "50m", Memory: "64Mi"})
		}
	}
	const limit = 12_000
	r := tools.NewRegistry(f.Cluster, tools.Options{MaxResultBytes: limit})
	out, err := r.Read(ctx, "get_metrics", args(M{"level": "pods", "limit": 200}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > limit {
		t.Fatalf("%d bytes, over the limit of %d", len(out), limit)
	}
	wantAll(t, "shortened", out, "note: 110 pods shown as —", "That is unknown, not zero", "is no reading (unknown), never zero", " of 220:")
	if strings.Contains(out, "more bytes)") || strings.Contains(out, "the top 200 of 220") {
		t.Fatalf("the result was cut off, or claims rows it does not have:\n%s", out[len(out)-300:])
	}
	// It says how many rows it really shows.
	var shown int
	if _, err := fmt.Sscanf(out[strings.Index(out, "the top "):], "the top %d of 220", &shown); err != nil || shown < 20 || shown >= 200 {
		t.Fatalf("rows shown: %d (%v)", shown, err)
	}
	if got := strings.Count(out, "payments-reconciliation-worker-"); got != shown {
		t.Fatalf("says the top %d, lists %d", shown, got)
	}
}

// Credentials that were not accepted are not a missing permission, and a
// narrower read would not help.
func TestMetricsToolExpiredCredentials(t *testing.T) {
	f := loadedCluster()
	f.FailMetrics(apierrors.NewUnauthorized("token has expired"))
	_, err := registry(f).Read(ctx, "get_metrics", args(M{"level": "pods"}))
	if err == nil {
		t.Fatal("readings without credentials")
	}
	wantAll(t, "401", err.Error(), "credentials rejected: The cluster did not accept your credentials", "expired token or certificate", "authentication failure", "UNKNOWN, not zero")
	if strings.Contains(err.Error(), "namespace may still be allowed") || strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a 401 is described as a permission problem: %v", err)
	}
}
