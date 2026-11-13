package kube

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// 4.4: one read that does not answer is abandoned after its own bound, with a
// stated reason — even if the transport ignores the deadline — and does not
// hold up a read beside it.
//
// (Tested on readScoped directly: client-go's fake clientset serialises every
// call behind one lock, so a blocked fake would block its neighbours too,
// which no real API server does.)
func TestSlowHealthReadIsAbandonedWithAReason(t *testing.T) {
	healthReadTimeout = 150 * time.Millisecond
	defer func() { healthReadTimeout = HealthReadTimeout }()
	release := make(chan struct{})
	defer close(release)

	stuck := func(context.Context, string) (Listed[appsv1.StatefulSet], error) {
		<-release // ignores its context, as a misbehaving transport would
		return Listed[appsv1.StatefulSet]{}, nil
	}
	quick := func(context.Context, string) (Listed[appsv1.DaemonSet], error) {
		return Listed[appsv1.DaemonSet]{Items: []appsv1.DaemonSet{{}}}, nil
	}

	obs := &health.Observation{}
	start := time.Now()
	var wg sync.WaitGroup
	var quickAt time.Duration
	wg.Add(2)
	go func() {
		defer wg.Done()
		obs.StatefulSets, obs.StatefulSetsRead = readScoped(context.Background(), "statefulsets", AllNamespaces, stuck)
	}()
	go func() {
		defer wg.Done()
		obs.DaemonSets, obs.DaemonSetsRead = readScoped(context.Background(), "daemonsets", AllNamespaces, quick)
		quickAt = time.Since(start)
	}()
	wg.Wait()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the stuck read held on for %v", took)
	}
	if quickAt > 100*time.Millisecond || !obs.DaemonSetsRead.OK() || len(obs.DaemonSets) != 1 {
		t.Fatalf("the quick read waited for the stuck one (%v): %+v", quickAt, obs.DaemonSetsRead)
	}
	if st := obs.StatefulSetsRead.Status; st.State != metrics.TimedOut {
		t.Fatalf("statefulsets: %+v", st)
	}
	obs.DeploymentsRead = health.Read{Done: true, Status: metrics.SourceStatus{Source: "deployments"}}
	c := health.Observe(obs).Class(health.Rollouts)
	if c.Clear() || !strings.Contains(c.Line(), "statefulsets not read (unavailable: Listing statefulsets timed out") {
		t.Fatalf("rollouts: %q", c.Line())
	}
}
