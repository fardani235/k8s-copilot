package kube_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

func statefulSet(ns, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
}

func daemonSet(ns, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

// listedVerbs counts the typed client's actions by verb and resource.
func listedVerbs(f *kubetest.Fake) map[string]int {
	out := map[string]int{}
	for _, a := range f.Clientset.Actions() {
		out[a.GetVerb()+" "+a.GetResource().Resource]++
	}
	return out
}

// 2.1: typed, read-only, one list each.
func TestTypedHealthListings(t *testing.T) {
	f := kubetest.New(
		kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"), kubetest.Pod("ops", "cron-1"),
		kubetest.Deployment("shop", "web", 2), statefulSet("data", "db", 3), daemonSet("ops", "agent"),
	)
	ctx := context.Background()
	pods, err := f.ListPods(ctx, kube.AllNamespaces)
	if err != nil || len(pods.Items) != 2 || pods.Truncated {
		t.Fatalf("pods: %+v %v", pods, err)
	}
	shop, err := f.ListPods(ctx, "shop")
	if err != nil || len(shop.Items) != 1 || shop.Items[0].Name != "web-1" {
		t.Fatalf("pods in shop: %+v %v", shop, err)
	}
	nodes, err := f.ListNodes(ctx)
	if err != nil || len(nodes.Items) != 1 || nodes.Items[0].Status.Allocatable.Cpu().String() != "4" {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	d, err := f.ListDeployments(ctx, kube.AllNamespaces)
	if err != nil || len(d.Items) != 1 || *d.Items[0].Spec.Replicas != 2 {
		t.Fatalf("deployments: %+v %v", d, err)
	}
	s, err := f.ListStatefulSets(ctx, kube.AllNamespaces)
	if err != nil || len(s.Items) != 1 || *s.Items[0].Spec.Replicas != 3 {
		t.Fatalf("statefulsets: %+v %v", s, err)
	}
	ds, err := f.ListDaemonSets(ctx, kube.AllNamespaces)
	if err != nil || len(ds.Items) != 1 {
		t.Fatalf("daemonsets: %+v %v", ds, err)
	}

	// Each helper returns its own error.
	f.Clientset.PrependReactor("list", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(appsv1.Resource("statefulsets"), "", errors.New("no"))
	})
	if _, err := f.ListStatefulSets(ctx, kube.AllNamespaces); !apierrors.IsForbidden(err) {
		t.Fatalf("statefulsets error: %v", err)
	}
	if _, err := f.ListDaemonSets(ctx, kube.AllNamespaces); err != nil {
		t.Fatalf("one helper's error leaked into another: %v", err)
	}

	obs := f.HealthObservation(ctx, "shop", 0)
	if obs.StatefulSetsRead.Status.State != metrics.Denied || !obs.DaemonSetsRead.OK() || !obs.PodsRead.OK() || !obs.NodesRead.OK() {
		t.Fatalf("statuses: sts %+v ds %+v", obs.StatefulSetsRead, obs.DaemonSetsRead)
	}
	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("health reads sent %v", got)
	}
	for verb := range listedVerbs(f) {
		if verb[:5] != "list " {
			t.Errorf("a health read sent %q", verb)
		}
	}
}

// 2.1: rows beyond the cap are counted, whether the server says how many
// remain or simply returns too many.
func TestTruncationIsReported(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < kube.DefaultListLimit+12; i++ {
		objs = append(objs, kubetest.Pod("shop", fmt.Sprintf("web-%03d", i)))
	}
	f := kubetest.New(objs...)
	l, err := f.ListPods(context.Background(), kube.AllNamespaces)
	if err != nil || len(l.Items) != kube.DefaultListLimit || !l.Truncated || l.Cut != 12 {
		t.Fatalf("got %d items, truncated %v, cut %d, %v", len(l.Items), l.Truncated, l.Cut, err)
	}

	// A server that honours the limit and says what remains.
	remaining := int64(4321)
	f = kubetest.New()
	f.Clientset.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.NodeList{ListMeta: metav1.ListMeta{Continue: "next", RemainingItemCount: &remaining}, Items: []corev1.Node{{}}}, nil
	})
	f.Clientset.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &appsv1.DeploymentList{ListMeta: metav1.ListMeta{Continue: "next"}}, nil
	})
	n, _ := f.ListNodes(context.Background())
	d, _ := f.ListDeployments(context.Background(), kube.AllNamespaces)
	if !n.Truncated || n.Cut != remaining || !d.Truncated || d.Cut != 0 {
		t.Fatalf("nodes %+v, deployments %+v", n, d)
	}
	// And the observation carries it to the class.
	obs := f.HealthObservation(context.Background(), kube.AllNamespaces, 0)
	if !obs.NodesRead.Truncated || obs.NodesRead.Cut != remaining {
		t.Fatalf("observation lost the truncation: %+v", obs.NodesRead)
	}
	if c := health.Observe(obs).Class(health.Nodes); c.Clear() {
		t.Fatalf("a truncated node read is clear: %q", c.Line())
	}
}

// 2.2: refused across the cluster, the browser's namespace is read instead,
// and the read says which namespace and why.
func TestHealthListingsFallBackToNamespace(t *testing.T) {
	f := kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"), kubetest.Pod("ops", "cron-1"), kubetest.Deployment("shop", "web", 1))
	var mu sync.Mutex
	var asked []string
	f.Clientset.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		asked = append(asked, a.GetResource().Resource+"@"+a.GetNamespace())
		mu.Unlock()
		if a.GetResource().Resource == "nodes" || a.GetNamespace() == "shop" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("cannot list at the cluster scope"))
	})
	obs := f.HealthObservation(context.Background(), "shop", 0)
	r := obs.PodsRead
	if !r.OK() || r.Status.Scope != "shop" || r.Refused == nil || r.Refused.State != metrics.Denied || r.Refused.Scope != "" {
		t.Fatalf("pods read: %+v", r)
	}
	if len(obs.Pods) != 1 || obs.Pods[0].Name != "web-1" {
		t.Fatalf("pods: %+v", obs.Pods)
	}
	if !obs.NodesRead.OK() || obs.NodesRead.Refused != nil {
		t.Fatalf("nodes are cluster-scoped and were readable: %+v", obs.NodesRead)
	}
	if obs.DeploymentsRead.Status.Scope != "shop" || len(obs.Deployments) != 1 {
		t.Fatalf("deployments: %+v", obs.DeploymentsRead)
	}
	c := health.Observe(obs).Class(health.Workloads)
	if c.Scope != "shop" || c.Clear() {
		t.Fatalf("workloads: %q", c.Line())
	}

	// Without a namespace to fall back to, the refusal is what is reported.
	obs = f.HealthObservation(context.Background(), kube.AllNamespaces, 0)
	if obs.PodsRead.Status.State != metrics.Denied || obs.PodsRead.Refused != nil || len(obs.Pods) != 0 {
		t.Fatalf("no fallback: %+v", obs.PodsRead)
	}
	if c := health.Observe(obs).Class(health.Workloads); c.State != health.Forbidden {
		t.Fatalf("workloads without fallback: %v", c.State)
	}
}

// 2.3
func TestWarningEventsWindowAndCap(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(-d)) }
	f := kubetest.New(
		kubetest.Event("shop", "e1", "web-1", "Warning", "BackOff", "restarting", at(time.Minute)),
		kubetest.Event("shop", "e2", "web-1", "Normal", "Pulled", "pulled", at(time.Minute)),
		kubetest.Event("shop", "e3", "web-1", "Warning", "BackOff", "old", at(health.EventWindow+time.Minute)),
	)
	l, err := f.ListWarningEvents(context.Background(), kube.AllNamespaces, health.EventWindow, now)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "e1" || l.Truncated {
		t.Fatalf("got %+v, %v", l, err)
	}
	// The server is asked for Warnings only.
	for _, a := range f.Clientset.Actions() {
		if la, ok := a.(clienttesting.ListActionImpl); ok && la.GetResource().Resource == "events" {
			if fs := la.GetListRestrictions().Fields.String(); fs != "type=Warning" {
				t.Errorf("field selector %q", fs)
			}
		}
	}

	var objs []runtime.Object
	for i := 0; i < kube.DefaultListLimit+3; i++ {
		objs = append(objs, kubetest.Event("shop", fmt.Sprintf("e%03d", i), "web-1", "Warning", "BackOff", "x", at(time.Minute)))
	}
	f = kubetest.New(objs...)
	l, _ = f.ListWarningEvents(context.Background(), kube.AllNamespaces, health.EventWindow, now)
	if len(l.Items) != kube.DefaultListLimit || !l.Truncated || l.Cut != 3 {
		t.Fatalf("cap: %d items, truncated %v, cut %d", len(l.Items), l.Truncated, l.Cut)
	}
}

// 2.4: the observation takes usage from the metrics cache — the readings the
// metrics screen holds — not from a read of its own, and passes every read's
// status through as it came.
func TestObservationReusesMetricsSnapshot(t *testing.T) {
	f := busyCluster()
	ctx := context.Background()
	snap := f.Metrics(ctx, kube.AllNamespaces, 0)
	calls := f.MetricsCalls()
	obs := f.HealthObservation(ctx, kube.AllNamespaces, 0)
	if f.MetricsCalls() != calls {
		t.Fatalf("the observation read the metrics API again: %d more calls", f.MetricsCalls()-calls)
	}
	if obs.Metrics != snap {
		t.Fatal("the observation does not hold the cached snapshot")
	}

	// Statuses pass through unchanged.
	f = busyCluster()
	f.FailMetrics(forbidden("nodes"))
	f.Clientset.PrependReactor("list", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd is sad")
	})
	obs = f.HealthObservation(ctx, kube.AllNamespaces, 0)
	if st := obs.Metrics.Sources.NodeMetrics; st.State != metrics.Denied || st.Reason == "" {
		t.Fatalf("metrics status changed on the way: %+v", st)
	}
	if st := obs.DaemonSetsRead.Status; st.State != metrics.Unavailable || st.Reason != "etcd is sad" || st.Source != "daemonsets" {
		t.Fatalf("daemonsets status: %+v", st)
	}

	// The screen and the copilot are handed the same observation within the
	// reuse window, and a fresh one when asked to read.
	a := f.HealthObservation(ctx, kube.AllNamespaces, kube.HealthFreshFor)
	b := f.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor)
	if a != b || a != obs {
		t.Fatal("two callers within the reuse window got different observations")
	}
	if f.HealthObservation(ctx, kube.AllNamespaces, 0) == a {
		t.Fatal("maxAge 0 did not read again")
	}
}

// Concurrent callers share one read and the race detector has nothing to say.
func TestHealthObservationSharesOneRead(t *testing.T) {
	f := busyCluster()
	var wg sync.WaitGroup
	got := make([]*health.Observation, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = f.HealthObservation(context.Background(), kube.AllNamespaces, kube.HealthFreshFor)
		}()
	}
	wg.Wait()
	for _, o := range got[1:] {
		if o != got[0] {
			t.Fatal("concurrent callers got different observations")
		}
	}
	if n := listedVerbs(f)["list pods"]; n != 1 {
		t.Fatalf("pods listed %d times for 8 concurrent callers", n)
	}
}

// A caller that stops waiting gets an observation that says nothing was read
// in time — not one that reads as healthy.
func TestHealthCallerCanStopWaiting(t *testing.T) {
	f := busyCluster()
	release := make(chan struct{})
	f.Clientset.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-release
		return false, nil, nil
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	obs := f.HealthObservation(ctx, kube.AllNamespaces, 0)
	r := health.Observe(obs)
	if r.AllClear() || len(r.Unwatched()) != len(r.Classes) {
		t.Fatalf("an abandoned observation: %s", r.Summary())
	}
}

// The copilot asks for "whatever is latest" after the screen read with its
// namespace fallback: it is handed the screen's observation, not a read
// falling back to another namespace.
func TestAnyFallbackTakesTheScreensObservation(t *testing.T) {
	f := kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"), kubetest.Pod("default", "x"))
	f.Clientset.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource == "nodes" || a.GetNamespace() != "" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("no"))
	})
	ctx := context.Background()
	screen := f.HealthObservation(ctx, "shop", kube.HealthFreshFor)
	before := listedVerbs(f)["list pods"]
	copilot := f.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor)
	if copilot != screen || listedVerbs(f)["list pods"] != before {
		t.Fatal("the copilot was handed another read than the screen's")
	}
	// Named explicitly with the same namespace — what the focus now gives
	// the model — it is the same too.
	if f.HealthObservation(ctx, "shop", kube.HealthFreshFor) != screen {
		t.Fatal("the same namespace read again")
	}
}
