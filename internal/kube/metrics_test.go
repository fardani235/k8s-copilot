package kube_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// busyCluster is two nodes and four pods with known readings.
func busyCluster() *kubetest.Fake {
	pending := kubetest.PodOn("shop", "web-pending", "", kubetest.Container("app", "100m", "", "64Mi", ""))
	pending.Status.Phase = corev1.PodPending
	done := kubetest.PodOn("ops", "job-done", "node-b", kubetest.Container("job", "", "", "", ""))
	done.Status.Phase = corev1.PodSucceeded

	f := kubetest.New(
		kubetest.Node("node-a", "4", "8Gi"), kubetest.Node("node-b", "2", "4Gi"),
		kubetest.Namespace("shop"), kubetest.Namespace("ops"),
		kubetest.PodOn("shop", "web-1", "node-a",
			kubetest.Container("app", "250m", "500m", "256Mi", "512Mi"),
			kubetest.Container("sidecar", "50m", "", "32Mi", "")),
		kubetest.PodOn("shop", "db-0", "node-b", kubetest.Container("db", "500m", "1", "1Gi", "1Gi")),
		kubetest.PodOn("ops", "cron-1", "node-a", kubetest.Container("cron", "", "", "", "")),
		pending, done,
	)
	f.SetNodeMetrics("node-a", "1200m", "6Gi")
	f.SetNodeMetrics("node-b", "1900m", "3891Mi")
	f.SetPodMetrics("shop", "web-1", kubetest.Usage{Container: "app", CPU: "480m", Memory: "490Mi"}, kubetest.Usage{Container: "sidecar", CPU: "10m", Memory: "20Mi"})
	f.SetPodMetrics("shop", "db-0", kubetest.Usage{Container: "db", CPU: "156340215n", Memory: "524288Ki"})
	f.SetPodMetrics("ops", "cron-1", kubetest.Usage{Container: "cron", CPU: "0", Memory: "8Mi"})
	return f
}

func forbidden(what string) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: what}, "", errors.New("User \"tester\" cannot list resource \""+what+"\" in API group \"metrics.k8s.io\""))
}

func TestMetricsJoinsUsageWithBounds(t *testing.T) {
	f := busyCluster()
	s := f.Metrics(context.Background(), kube.AllNamespaces, 0)

	for name, st := range map[string]metrics.SourceStatus{
		"node metrics": s.Sources.NodeMetrics, "pod metrics": s.Sources.PodMetrics, "nodes": s.Sources.Nodes, "pods": s.Sources.Pods,
	} {
		if !st.OK() {
			t.Fatalf("%s: %+v", name, st)
		}
	}

	a, _ := s.Node("node-a")
	if a.CPU != metrics.Millicores(1200) || a.CPUAlloc != metrics.Millicores(4000) || a.Mem != metrics.Known(6<<30) || a.MemAlloc != metrics.Known(8<<30) {
		t.Fatalf("node-a: %+v", a)
	}
	// Two running pods and nothing else: the finished job is not counted,
	// and the pending pod is on no node.
	if a.Pods != metrics.Known(2) || a.Ready != "Ready" {
		t.Fatalf("node-a pods/ready: %+v", a)
	}
	if s.Cluster.CPU != metrics.Millicores(3100) || s.Cluster.CPUAlloc != metrics.Millicores(6000) || s.Cluster.Nodes != 2 || s.Cluster.Reporting != 2 {
		t.Fatalf("cluster: %+v", s.Cluster)
	}

	web, ok := s.Pod("shop", "web-1")
	if !ok || web.CPU != metrics.Millicores(490) || web.Mem != metrics.Known(510<<20) || web.Node != "node-a" {
		t.Fatalf("web-1: %+v", web)
	}
	// One container has no limit, so the pod has none; requests still add up.
	if web.CPUReq != metrics.Millicores(300) || web.CPULim.OK || web.MemLim.OK {
		t.Fatalf("web-1 bounds: %+v", web)
	}
	// Nanocores and Ki, as metrics-server really reports them — kept exact.
	db, _ := s.Pod("shop", "db-0")
	if db.CPU != metrics.Known(156340215) || db.Mem != metrics.Known(512<<20) || db.MemLim != metrics.Known(1<<30) {
		t.Fatalf("db-0: %+v", db)
	}
	// A sample of zero is a reading; a pod with no sample is not.
	cron, _ := s.Pod("ops", "cron-1")
	pend, _ := s.Pod("shop", "web-pending")
	if cron.CPU != metrics.Known(0) || pend.CPU.OK || pend.Reporting() {
		t.Fatalf("zero vs unknown: cron %+v pending %+v", cron.CPU, pend.CPU)
	}
	if _, ok := s.Pod("ops", "job-done"); ok {
		t.Fatal("a finished pod is listed")
	}

	if len(s.Namespaces) != 2 {
		t.Fatalf("namespaces: %+v", s.Namespaces)
	}
	shop := s.Namespaces[1]
	if shop.Name != "shop" || shop.Pods != 3 || shop.Running != 2 || shop.Reporting != 2 || shop.CPU != metrics.Known(490_000_000+156340215) {
		t.Fatalf("shop: %+v", shop)
	}
	if s.SampleTime.IsZero() || s.Window != 15*time.Second {
		t.Fatalf("sample time %v window %v", s.SampleTime, s.Window)
	}

	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("reading metrics changed something: %v", got)
	}
}

// Every way the metrics API can be missing is a different, stated fact — and
// none of them produces readings.
func TestMetricsSourceStates(t *testing.T) {
	ctx := context.Background()

	t.Run("not installed", func(t *testing.T) {
		f := kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"))
		s := f.Metrics(ctx, kube.AllNamespaces, 0)
		if s.Sources.NodeMetrics.State != metrics.Absent || s.Sources.PodMetrics.State != metrics.Absent {
			t.Fatalf("%+v", s.Sources)
		}
		if s.HasUsage() || !s.Sources.Nodes.OK() || !s.Sources.Pods.OK() {
			t.Fatalf("%+v", s.Sources)
		}
		// What is known stays known; usage stays unknown, not zero.
		n, _ := s.Node("node-a")
		if n.CPU.OK || n.CPUAlloc != metrics.Millicores(4000) || s.Cluster.CPU.OK {
			t.Fatalf("node %+v cluster %+v", n, s.Cluster)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
		want metrics.SourceState
	}{
		{"forbidden", forbidden("pods"), metrics.Denied},
		{"credentials rejected", apierrors.NewUnauthorized("token expired"), metrics.Unauthenticated},
		{"rate limited", apierrors.NewTooManyRequests("slow down", 1), metrics.Failed},
		{"broken backend", apierrors.NewServiceUnavailable("the server is currently unable to handle the request"), metrics.Unavailable},
		{"server timeout", apierrors.NewTimeoutError("request did not complete within requested timeout", 0), metrics.TimedOut},
		{"deadline", fmt.Errorf("Get \"https://x/apis/metrics.k8s.io\": %w", context.DeadlineExceeded), metrics.TimedOut},
		{"anything else", errors.New("unexpected EOF"), metrics.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := busyCluster()
			f.FailMetrics(tc.err)
			s := f.Metrics(ctx, kube.AllNamespaces, 0)
			if s.Sources.PodMetrics.State != tc.want || s.Sources.NodeMetrics.State != tc.want {
				t.Fatalf("state = %v / %v, want %v", s.Sources.NodeMetrics.State, s.Sources.PodMetrics.State, tc.want)
			}
			if s.Sources.PodMetrics.Reason == "" {
				t.Fatal("the server's reason was lost")
			}
			for _, p := range s.Pods {
				if p.CPU.OK || p.Mem.OK {
					t.Fatalf("a failed read produced a reading: %+v", p)
				}
			}
		})
	}

	// Allowed to see pods in one namespace, not cluster-wide and not nodes:
	// each source says so on its own.
	t.Run("partial permissions", func(t *testing.T) {
		f := busyCluster()
		f.Dynamic.PrependReactor("list", "*", denyUnless(func(group, resource, namespace string) bool {
			return resource == "pods" && namespace == "shop"
		}))
		wide := f.Metrics(ctx, kube.AllNamespaces, 0)
		if st, denied := wide.PodsDeniedClusterWide(); !denied || st.Source != metrics.PodMetrics {
			t.Fatalf("cluster-wide: %+v", wide.Sources)
		}
		s := f.Metrics(ctx, "shop", 0)
		if !s.Sources.PodMetrics.OK() || !s.Sources.Pods.OK() || s.Sources.NodeMetrics.State != metrics.Denied || s.Sources.Nodes.State != metrics.Denied {
			t.Fatalf("namespace: %+v", s.Sources)
		}
		if len(s.Pods) != 3 || len(s.Nodes) != 0 {
			t.Fatalf("pods %d nodes %d", len(s.Pods), len(s.Nodes))
		}
	})
}

// The cache is what lets the screen and the copilot be handed the same
// readings.
func TestMetricsAreSharedNotRefetched(t *testing.T) {
	ctx := context.Background()
	f := busyCluster()

	first := f.Metrics(ctx, kube.AllNamespaces, 0)
	calls := f.MetricsCalls()
	if again := f.Metrics(ctx, kube.AllNamespaces, time.Minute); again != first {
		t.Fatal("a fresh snapshot was not reused")
	}
	// A question about one namespace is answered from the cluster-wide
	// readings: same numbers, no second read.
	narrow := f.Metrics(ctx, "shop", time.Minute)
	if f.MetricsCalls() != calls {
		t.Fatalf("the metrics API was asked again (%d → %d calls)", calls, f.MetricsCalls())
	}
	if narrow.Scope != "shop" || len(narrow.Pods) != 3 || len(narrow.Namespaces) != 1 {
		t.Fatalf("narrowed: scope %q, %d pods, %d namespaces", narrow.Scope, len(narrow.Pods), len(narrow.Namespaces))
	}
	w, _ := first.Pod("shop", "web-1")
	n, _ := narrow.Pod("shop", "web-1")
	if w.CPU != n.CPU || w.Mem != n.Mem || narrow.SampleTime != first.SampleTime {
		t.Fatal("narrowing changed the readings")
	}
	// In a namespace's view a per-node pod count would be a partial count.
	if a, _ := narrow.Node("node-a"); a.Pods.OK {
		t.Fatal("a namespace-scoped snapshot claims to know a node's pod count")
	}

	if f.Metrics(ctx, kube.AllNamespaces, 0) == first || f.MetricsCalls() == calls {
		t.Fatal("maxAge 0 must ask the cluster")
	}

	// Many callers at once share one read.
	f = busyCluster()
	var wg sync.WaitGroup
	snaps := make([]*metrics.Snapshot, 8)
	for i := range snaps {
		wg.Add(1)
		go func() { defer wg.Done(); snaps[i] = f.Metrics(ctx, kube.AllNamespaces, time.Minute) }()
	}
	wg.Wait()
	if got := f.MetricsCalls(); got != 2 {
		t.Fatalf("8 concurrent callers made %d metrics requests, want 2 (nodes + pods, once)", got)
	}
}

// A refresh that fails keeps the last good readings on show for a while —
// marked as held, with the cause — instead of flickering to an error or,
// worse, passing old numbers off as current.
func TestMetricsHoldOverIsLabelled(t *testing.T) {
	ctx := context.Background()
	f := busyCluster()
	good := f.Metrics(ctx, kube.AllNamespaces, 0)

	f.FailMetrics(apierrors.NewServiceUnavailable("metrics-server is down"))
	held := f.Metrics(ctx, kube.AllNamespaces, 0)
	if held.Held == nil || held.Held.Cause.State != metrics.Unavailable || !strings.Contains(held.Held.Cause.Reason, "metrics-server is down") {
		t.Fatalf("held: %+v", held.Held)
	}
	if held.TakenAt != good.TakenAt || len(held.Pods) != len(good.Pods) || good.Held != nil {
		t.Fatal("the held snapshot is not the last good one, or the original was modified")
	}
	if fr := held.Freshness(time.Now()); !fr.Warn || !strings.Contains(fr.Text, "refresh failing") {
		t.Fatalf("freshness of a held snapshot: %+v", fr)
	}

	f.FailMetrics(nil)
	if s := f.Metrics(ctx, kube.AllNamespaces, 0); s.Held != nil || !s.HasUsage() {
		t.Fatalf("recovered snapshot: held=%v", s.Held)
	}

	// Never having had readings, there is nothing to hold: the failure shows.
	f = kubetest.New(kubetest.Node("node-a", "4", "8Gi"))
	if s := f.Metrics(ctx, kube.AllNamespaces, 0); s.Held != nil || s.HasUsage() {
		t.Fatalf("first read failed, yet: %+v", s)
	}
}

// A caller that gives up gets an answer straight away — and it is not zeros.
func TestMetricsCallerCanStopWaiting(t *testing.T) {
	f := busyCluster()
	release := make(chan struct{})
	defer close(release)
	f.Dynamic.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-release // the cluster does not answer
		return false, nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	s := f.Metrics(ctx, kube.AllNamespaces, 0)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("a caller that had given up waited %v", waited)
	}
	if s.HasUsage() || s.Sources.PodMetrics.OK() {
		t.Fatalf("%+v", s.Sources)
	}
}

func denyUnless(allowed func(group, resource, namespace string) bool) clienttesting.ReactionFunc {
	return func(a clienttesting.Action) (bool, runtime.Object, error) {
		gvr := a.GetResource()
		if allowed(gvr.Group, gvr.Resource, a.GetNamespace()) {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("RBAC: access denied"))
	}
}

// --- the real wire format -------------------------------------------------------

// What a real API server and a real metrics-server send, byte for byte in
// shape: nanocore CPU, Ki memory, RFC 3339 timestamps, a fractional window.
const (
	wireNodeMetrics = `{"kind":"NodeMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{},"items":[
	 {"metadata":{"name":"minikube","creationTimestamp":"2026-10-09T10:00:15Z","labels":{"kubernetes.io/hostname":"minikube"}},
	  "timestamp":"2026-10-09T10:00:05Z","window":"20.021s","usage":{"cpu":"312457893n","memory":"1362696Ki"}}]}`
	wirePodMetrics = `{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{},"items":[
	 {"metadata":{"name":"coredns-7d764666f9-abcde","namespace":"kube-system","creationTimestamp":"2026-10-09T10:00:15Z"},
	  "timestamp":"2026-10-09T10:00:02Z","window":"15.3s","containers":[{"name":"coredns","usage":{"cpu":"2145873n","memory":"14520Ki"}}]}]}`
	wireNodes = `{"kind":"NodeList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[
	 {"metadata":{"name":"minikube"},"status":{"capacity":{"cpu":"8","memory":"16266012Ki"},"allocatable":{"cpu":"8","memory":"16266012Ki","pods":"110"},
	  "conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}]}}]}`
	wirePods = `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[
	 {"metadata":{"name":"coredns-7d764666f9-abcde","namespace":"kube-system"},
	  "spec":{"nodeName":"minikube","containers":[{"name":"coredns","resources":{"limits":{"memory":"170Mi"},"requests":{"cpu":"100m","memory":"70Mi"}}}]},
	  "status":{"phase":"Running","containerStatuses":[{"name":"coredns","restartCount":3,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}]}`
)

// apiServer is a stand-in API server. metricsAPI decides what the aggregated
// metrics API does: answer, be absent, be broken, refuse.
func apiServer(t *testing.T, metricsAPI func(w http.ResponseWriter, r *http.Request) bool) *kube.Cluster {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("metrics made a %s request to %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"gitVersion":"v1.35.1"}`)
		case "/api/v1/nodes":
			io.WriteString(w, wireNodes)
		case "/api/v1/pods":
			io.WriteString(w, wirePods)
		case "/apis/metrics.k8s.io/v1beta1/nodes", "/apis/metrics.k8s.io/v1beta1/pods":
			if metricsAPI(w, r) {
				return
			}
			if strings.HasSuffix(r.URL.Path, "/nodes") {
				io.WriteString(w, wireNodeMetrics)
			} else {
				io.WriteString(w, wirePodMetrics)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "cfg")
	writeKubeconfig(t, path, "c", srv.URL, "c")
	c, err := kube.Connect(context.Background(), kube.ConnectOptions{Kubeconfig: path})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMetricsOverTheWire(t *testing.T) {
	ctx := context.Background()

	t.Run("served", func(t *testing.T) {
		var cachedReads atomic.Int32
		c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Query().Get("resourceVersion") != "" {
				cachedReads.Add(1)
			}
			return false
		})
		s := c.Metrics(ctx, kube.AllNamespaces, 0)
		if !s.Sources.NodeMetrics.OK() || !s.Sources.PodMetrics.OK() || !s.Sources.Nodes.OK() || !s.Sources.Pods.OK() {
			t.Fatalf("%+v", s.Sources)
		}
		n, _ := s.Node("minikube")
		if n.CPU != metrics.Known(312457893) || n.Mem != metrics.Known(1362696<<10) || n.CPUAlloc != metrics.Millicores(8000) || n.Ready != "Ready" || n.Window != 20021*time.Millisecond {
			t.Fatalf("node: %+v", n)
		}
		p, _ := s.Pod("kube-system", "coredns-7d764666f9-abcde")
		if p.CPU != metrics.Known(2145873) || p.Mem != metrics.Known(14520<<10) || p.MemLim != metrics.Known(170<<20) || p.CPULim.OK || p.CPUReq != metrics.Millicores(100) {
			t.Fatalf("pod: %+v", p)
		}
		if c := p.Containers[0]; c.Restarts != 3 || c.LastExit != "OOMKilled" || c.State != "running" {
			t.Fatalf("container: %+v", c)
		}
		// Nodes were sampled at :05, pods at :02. The readings are as old as
		// the older of the two.
		if want := time.Date(2026, 10, 9, 10, 0, 2, 0, time.UTC); !s.SampleTime.Equal(want) || s.Window != 15300*time.Millisecond {
			t.Fatalf("sample time %v (window %v), want %v", s.SampleTime, s.Window, want)
		}
		if cachedReads.Load() != 0 {
			t.Fatal("the metrics API was asked for a cached (resourceVersion) read; it does not support one")
		}
	})

	// 404 on the group: no metrics-server.
	t.Run("absent", func(t *testing.T) {
		c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"the server could not find the requested resource","reason":"NotFound","details":{},"code":404}`)
			return true
		})
		s := c.Metrics(ctx, kube.AllNamespaces, 0)
		if s.Sources.NodeMetrics.State != metrics.Absent || s.Sources.PodMetrics.State != metrics.Absent || !s.Sources.Pods.OK() {
			t.Fatalf("%+v", s.Sources)
		}
	})

	// 503 from the aggregator, as plain text: the APIService exists, its
	// backend does not answer.
	t.Run("registered but broken", func(t *testing.T) {
		c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "service unavailable\n")
			return true
		})
		s := c.Metrics(ctx, kube.AllNamespaces, 0)
		if s.Sources.NodeMetrics.State != metrics.Unavailable || s.Sources.PodMetrics.State != metrics.Unavailable {
			t.Fatalf("%+v", s.Sources)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"pods.metrics.k8s.io is forbidden: User \"dev\" cannot list resource \"pods\" in API group \"metrics.k8s.io\" at the cluster scope","reason":"Forbidden","details":{"group":"metrics.k8s.io","kind":"pods"},"code":403}`)
			return true
		})
		s := c.Metrics(ctx, kube.AllNamespaces, 0)
		st := s.Sources.PodMetrics
		if st.State != metrics.Denied || !strings.Contains(st.Reason, `User "dev" cannot list`) {
			t.Fatalf("%+v", st)
		}
		if !strings.Contains(st.Explain(), "authorization failure, not an idle cluster") {
			t.Fatalf("explanation: %q", st.Explain())
		}
	})

	// A metrics API that hangs costs the caller its own patience, no more,
	// and the answer says what happened.
	t.Run("slow", func(t *testing.T) {
		release := make(chan struct{})
		var metricsRequests atomic.Int32
		c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			metricsRequests.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return true
		})
		// Registered after the server's own cleanup, so it runs before it:
		// the hung handlers are let go, then the server closes.
		t.Cleanup(func() { close(release) })
		short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		s := c.Metrics(short, kube.AllNamespaces, 0)
		if waited := time.Since(start); waited > 3*time.Second {
			t.Fatalf("waited %v for a hung metrics API", waited)
		}
		if s.Sources.PodMetrics.State != metrics.TimedOut || s.HasUsage() {
			t.Fatalf("%+v", s.Sources)
		}
		// The read is still out, hanging. The rest of the cluster is not
		// held up behind it.
		start = time.Now()
		if err := c.Probe(ctx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited > 3*time.Second {
			t.Fatalf("another request waited %v behind the hung metrics API", waited)
		}
		// And asking again joins the read that is out instead of sending a
		// second one.
		before := metricsRequests.Load()
		again, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel2()
		c.Metrics(again, kube.AllNamespaces, 0)
		if got := metricsRequests.Load(); got != before {
			t.Fatalf("a second read was sent to a metrics API that is still hanging on the first (%d → %d requests)", before, got)
		}
	})
}

// However the questions are put — the whole cluster or one namespace, by the
// screen or by the copilot, in whatever order — the answers are cut from the
// same readings. (An earlier version kept a separate snapshot per namespace,
// and one of those could outlive a newer cluster-wide read: the screen showed
// a pod at 99% of its limit while the copilot was told 16%.)
func TestMetricsAreCoherentAcrossScopes(t *testing.T) {
	ctx := context.Background()
	cpuOf := func(s *metrics.Snapshot) metrics.Amount {
		p, ok := s.Pod("shop", "db-0")
		if !ok {
			t.Fatalf("db-0 is not in the readings (scope %q, sources %+v)", s.Scope, s.Sources)
		}
		return p.CPU
	}

	// A namespace is asked about first; then the numbers move and the whole
	// cluster is read; then the namespace is asked about again.
	f := busyCluster()
	before := cpuOf(f.Metrics(ctx, "shop", kube.MetricsFreshFor))
	f.SetPodMetrics("shop", "db-0", kubetest.Usage{Container: "db", CPU: "990m", Memory: "1000Mi"})
	wide := f.Metrics(ctx, kube.AllNamespaces, 0)
	again := f.Metrics(ctx, "shop", kube.MetricsFreshFor)
	if cpuOf(wide) != metrics.Millicores(990) || cpuOf(again) != cpuOf(wide) || cpuOf(again) == before {
		t.Fatalf("cluster-wide says %+v, the namespace says %+v (before the change: %+v)", cpuOf(wide), cpuOf(again), before)
	}
	if again.SampleTime != wide.SampleTime || again.TakenAt != wide.TakenAt {
		t.Fatal("the namespace's answer is not cut from the cluster-wide read")
	}

	// The source goes down. What is held for the cluster is held for the
	// namespace too: not numbers on one side and "unavailable" on the other.
	f.FailMetrics(apierrors.NewServiceUnavailable("metrics-server is down"))
	heldWide := f.Metrics(ctx, kube.AllNamespaces, 0)
	heldNS := f.Metrics(ctx, "shop", kube.MetricsFreshFor)
	if heldWide.Held == nil || heldNS.Held == nil || cpuOf(heldNS) != cpuOf(heldWide) {
		t.Fatalf("held cluster-wide: %v; for the namespace: %v (sources %+v)", heldWide.Held != nil, heldNS.Held != nil, heldNS.Sources)
	}

	// A failure seen through a namespace does not linger once the cluster
	// answers again.
	f = busyCluster()
	f.FailMetrics(apierrors.NewServiceUnavailable("blip"))
	if s := f.Metrics(ctx, "shop", kube.MetricsFreshFor); s.HasUsage() {
		t.Fatal("readings from a source that is down")
	}
	f.FailMetrics(nil)
	f.Metrics(ctx, kube.AllNamespaces, 0)
	if s := f.Metrics(ctx, "shop", kube.MetricsFreshFor); !s.Sources.PodMetrics.OK() || cpuOf(s) != metrics.Known(156340215) {
		t.Fatalf("after recovery the namespace still reports the failure: %+v", s.Sources.PodMetrics)
	}

	// Refused cluster-wide: the namespace's pods are read on their own, and
	// set beside the *same* node readings everyone else gets.
	f = busyCluster()
	f.Dynamic.PrependReactor("list", "*", denyUnless(func(group, resource, namespace string) bool {
		return resource == "nodes" || namespace == "shop"
	}))
	wide = f.Metrics(ctx, kube.AllNamespaces, 0)
	if _, refused := wide.PodsDeniedClusterWide(); !refused || !wide.Sources.NodeMetrics.OK() {
		t.Fatalf("%+v", wide.Sources)
	}
	shop := f.Metrics(ctx, "shop", kube.MetricsFreshFor)
	if shop.Scope != "shop" || !shop.Sources.PodMetrics.OK() || len(shop.Pods) != 3 {
		t.Fatalf("scope %q, %d pods, %+v", shop.Scope, len(shop.Pods), shop.Sources.PodMetrics)
	}
	a, _ := shop.Node("node-a")
	b, _ := wide.Node("node-a")
	if a.CPU != b.CPU || a.Sampled != b.Sampled || a.Pods.OK {
		t.Fatalf("node-a beside the namespace's pods: %+v; cluster-wide: %+v", a, b)
	}
	// The refusal is lifted: the next read is cluster-wide again by itself.
	f = busyCluster()
	var lifted atomic.Bool
	f.Dynamic.PrependReactor("list", "*", denyUnless(func(group, resource, namespace string) bool {
		return lifted.Load() || resource == "nodes" || namespace == "shop"
	}))
	f.Metrics(ctx, "shop", 0)
	lifted.Store(true)
	if s := f.Metrics(ctx, "ops", 0); !s.Sources.PodMetrics.OK() || len(s.Pods) != 1 {
		t.Fatalf("after the refusal was lifted: %+v, %d pods", s.Sources.PodMetrics, len(s.Pods))
	}
}

// A metrics API that keeps promising more and delivering nothing is not
// followed for ever.
func TestMetricsPagingCannotLoop(t *testing.T) {
	var requests atomic.Int32
	c := apiServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		requests.Add(1)
		io.WriteString(w, `{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"continue":"again"},"items":[]}`)
		return true
	})
	start := time.Now()
	s := c.Metrics(context.Background(), kube.AllNamespaces, 0)
	if waited := time.Since(start); waited > 5*time.Second || requests.Load() > 4 {
		t.Fatalf("%d requests in %v", requests.Load(), waited)
	}
	if len(s.Truncated) != 2 {
		t.Fatalf("an incomplete list is not said to be incomplete: %v", s.Truncated)
	}
}
