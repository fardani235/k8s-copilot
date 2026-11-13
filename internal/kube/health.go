package kube

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// The reads behind the health screen and get_findings. Each is one typed,
// read-only `list`, capped at DefaultListLimit rows, with its own status: a
// read that was refused, timed out or cut short is a stated fact about the
// observation, never an empty list (K-40, K-43).

const (
	// HealthReadTimeout bounds each read. A slow resource type costs at most
	// this long and does not hold up the others, which run beside it.
	HealthReadTimeout = 10 * time.Second
	// HealthFreshFor is how old an observation may be and still be handed
	// out instead of reading again — the reuse window that gives the screen
	// and the copilot the same findings.
	HealthFreshFor = 15 * time.Second
	// AnyFallback asks for the latest observation whatever namespace it fell
	// back to. (Not a valid namespace name, so it cannot be one.)
	AnyFallback = "*"
	// HealthObservationBound is the longest HealthObservation can take: a
	// read already out (a cluster-wide read and a namespace one), then at
	// most one of its own.
	HealthObservationBound = 4 * HealthReadTimeout
)

// healthReadTimeout is HealthReadTimeout, as a variable so that a test can
// make a slow read time out without waiting ten seconds.
var healthReadTimeout = HealthReadTimeout

// Listed is the result of one capped, typed list.
type Listed[T any] struct {
	Items []T
	// Truncated is set when the cluster had more than were returned. Cut is
	// how many more, when the server says; 0 when it does not.
	Truncated bool
	Cut       int64
}

// capList cuts items at the cap and records what was cut — from what the
// server said about the rest, and from what it returned beyond the limit.
func capList[T any](items []T, meta metav1.ListMeta) Listed[T] {
	l := Listed[T]{Items: items}
	if meta.Continue != "" {
		l.Truncated = true
		if meta.RemainingItemCount != nil && *meta.RemainingItemCount > 0 {
			l.Cut = *meta.RemainingItemCount
		}
	}
	if len(items) > DefaultListLimit {
		l.Truncated = true
		l.Cut += int64(len(items) - DefaultListLimit)
		l.Items = items[:DefaultListLimit]
	}
	return l
}

func healthListOptions() metav1.ListOptions { return metav1.ListOptions{Limit: DefaultListLimit} }

// ListPods lists pods, in one namespace or (AllNamespaces) all of them.
func (c *Cluster) ListPods(ctx context.Context, namespace string) (Listed[corev1.Pod], error) {
	l, err := c.cs.CoreV1().Pods(namespace).List(ctx, healthListOptions())
	if err != nil {
		return Listed[corev1.Pod]{}, err
	}
	return capList(l.Items, l.ListMeta), nil
}

// ListNodes lists the nodes.
func (c *Cluster) ListNodes(ctx context.Context) (Listed[corev1.Node], error) {
	l, err := c.cs.CoreV1().Nodes().List(ctx, healthListOptions())
	if err != nil {
		return Listed[corev1.Node]{}, err
	}
	return capList(l.Items, l.ListMeta), nil
}

// ListDeployments lists Deployments.
func (c *Cluster) ListDeployments(ctx context.Context, namespace string) (Listed[appsv1.Deployment], error) {
	l, err := c.cs.AppsV1().Deployments(namespace).List(ctx, healthListOptions())
	if err != nil {
		return Listed[appsv1.Deployment]{}, err
	}
	return capList(l.Items, l.ListMeta), nil
}

// ListStatefulSets lists StatefulSets.
func (c *Cluster) ListStatefulSets(ctx context.Context, namespace string) (Listed[appsv1.StatefulSet], error) {
	l, err := c.cs.AppsV1().StatefulSets(namespace).List(ctx, healthListOptions())
	if err != nil {
		return Listed[appsv1.StatefulSet]{}, err
	}
	return capList(l.Items, l.ListMeta), nil
}

// ListDaemonSets lists DaemonSets.
func (c *Cluster) ListDaemonSets(ctx context.Context, namespace string) (Listed[appsv1.DaemonSet], error) {
	l, err := c.cs.AppsV1().DaemonSets(namespace).List(ctx, healthListOptions())
	if err != nil {
		return Listed[appsv1.DaemonSet]{}, err
	}
	return capList(l.Items, l.ListMeta), nil
}

// ListWarningEvents lists the Warning events that last occurred within window
// of now. The server is asked for Warning events only, capped at
// DefaultListLimit rows; the type and the window are applied here as well —
// the selector is an optimisation, this is the guarantee. A cut read says so:
// the events beyond the cap may be the newest.
func (c *Cluster) ListWarningEvents(ctx context.Context, namespace string, window time.Duration, now time.Time) (Listed[corev1.Event], error) {
	opts := healthListOptions()
	opts.FieldSelector = fields.OneTermEqualSelector("type", corev1.EventTypeWarning).String()
	l, err := c.cs.CoreV1().Events(namespace).List(ctx, opts)
	if err != nil {
		return Listed[corev1.Event]{}, err
	}
	out := capList(l.Items, l.ListMeta)
	from := now.Add(-window)
	kept := out.Items[:0:0]
	for i := range out.Items {
		e := &out.Items[i]
		if e.Type == corev1.EventTypeWarning && !eventTime(e).Before(from) {
			kept = append(kept, *e)
		}
	}
	out.Items = kept
	return out, nil
}

// --- one observation ------------------------------------------------------------

// readScoped makes one read across the cluster and, if the cluster refuses
// that, again for the fallback namespace, saying which it was.
func readScoped[T any](ctx context.Context, source metrics.Source, fallback string, list func(context.Context, string) (Listed[T], error)) ([]T, health.Read) {
	attempt := func(namespace string) (Listed[T], metrics.SourceStatus) {
		rctx, cancel := context.WithTimeout(ctx, healthReadTimeout)
		defer cancel()
		// The bound holds whether or not the transport honours the context:
		// a call that does not come back in time is left behind, and its
		// answer, if it ever comes, is dropped.
		type result struct {
			l   Listed[T]
			err error
		}
		ch := make(chan result, 1)
		go func() {
			l, err := list(rctx, namespace)
			ch <- result{l, err}
		}()
		var l Listed[T]
		var err error
		select {
		case res := <-ch:
			l, err = res.l, res.err
		case <-rctx.Done():
			err = rctx.Err()
		}
		if err != nil && rctx.Err() != nil {
			err = rctx.Err() // the deadline, rather than however the transport phrased it
		}
		return l, classify(source, namespace, err)
	}
	l, st := attempt(AllNamespaces)
	r := health.Read{Done: true, Status: st}
	if st.State == metrics.Denied && fallback != AllNamespaces && fallback != AnyFallback {
		refused := st
		l, st = attempt(fallback)
		r.Status, r.Refused = st, &refused
	}
	r.At = time.Now()
	if !st.OK() {
		return nil, r
	}
	r.Truncated, r.Cut = l.Truncated, l.Cut
	return l.Items, r
}

// healthMetrics is the usage part of an observation: the shared metrics
// snapshot, never a read of its own (K-30). Readings up to MetricsFreshFor
// old are reused, so with the metrics screen open the two screens show the
// same numbers.
func (c *Cluster) healthMetrics(ctx context.Context, fallback string) (*metrics.Snapshot, *metrics.SourceStatus) {
	ctx, cancel := context.WithTimeout(ctx, healthReadTimeout)
	defer cancel()
	wide := c.Metrics(ctx, AllNamespaces, MetricsFreshFor)
	if st, denied := wide.PodsDeniedClusterWide(); denied && fallback != AllNamespaces && fallback != AnyFallback {
		if narrow := c.Metrics(ctx, fallback, MetricsFreshFor); narrow.Sources.PodMetrics.OK() {
			return narrow, &st
		}
	}
	return wide, nil
}

// readHealth makes every read of an observation side by side.
func (c *Cluster) readHealth(ctx context.Context, fallback string) *health.Observation {
	obs := &health.Observation{}
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	// Each goroutine fills fields of its own.
	run(func() { obs.Pods, obs.PodsRead = readScoped(ctx, metrics.PodObjects, fallback, c.ListPods) })
	run(func() {
		obs.Nodes, obs.NodesRead = readScoped(ctx, metrics.NodeObjects, AllNamespaces, func(ctx context.Context, _ string) (Listed[corev1.Node], error) {
			return c.ListNodes(ctx)
		})
	})
	run(func() {
		obs.Deployments, obs.DeploymentsRead = readScoped(ctx, "deployments", fallback, c.ListDeployments)
	})
	run(func() {
		obs.StatefulSets, obs.StatefulSetsRead = readScoped(ctx, "statefulsets", fallback, c.ListStatefulSets)
	})
	run(func() { obs.DaemonSets, obs.DaemonSetsRead = readScoped(ctx, "daemonsets", fallback, c.ListDaemonSets) })
	run(func() {
		now := time.Now()
		obs.Events, obs.EventsRead = readScoped(ctx, "events", fallback, func(ctx context.Context, ns string) (Listed[corev1.Event], error) {
			return c.ListWarningEvents(ctx, ns, health.EventWindow, now)
		})
	})
	run(func() { obs.Metrics, obs.MetricsRefused = c.healthMetrics(ctx, fallback) })
	wg.Wait()
	obs.At = time.Now()
	return obs
}

// healthCache keeps the latest observation, so that the screen and the
// copilot are handed the same one rather than two reads taken seconds apart.
type healthCache struct {
	mu       sync.Mutex
	last     *health.Observation
	fallback string // the namespace last was read with as its fallback
	inflight chan struct{}
	// reading is the fallback of the read in flight.
	reading string
}

// needsFallback reports whether a fallback namespace made, or would make, a
// difference to an observation: some cluster-wide read was refused.
func needsFallback(obs *health.Observation) bool {
	for _, r := range []health.Read{obs.PodsRead, obs.DeploymentsRead, obs.StatefulSetsRead, obs.DaemonSetsRead, obs.EventsRead} {
		if r.Refused != nil || (r.Status.State == metrics.Denied && r.Status.Scope == AllNamespaces) {
			return true
		}
	}
	if obs.MetricsRefused != nil {
		return true
	}
	if obs.Metrics != nil {
		_, denied := obs.Metrics.PodsDeniedClusterWide()
		return denied
	}
	return false
}

func (hc *healthCache) fits(fallback string) bool {
	return hc.last != nil && (fallback == AnyFallback || hc.fallback == fallback || !needsFallback(hc.last))
}

// HealthObservation returns what the cluster looks like right now, for
// internal/health to classify.
//
// fallback is the namespace to read instead when a cluster-wide read is
// refused (the browser's namespace); AllNamespaces for none; AnyFallback for
// whatever the latest observation used (or, if a read is needed, the
// context's default namespace).
//
// An observation no older than maxAge, made with a fallback that fits, is
// handed out as it is (maxAge 0 always reads). Concurrent callers share one
// read. The result is never nil: every read carries its own status.
//
// It only ever sends `list` requests.
func (c *Cluster) HealthObservation(ctx context.Context, fallback string, maxAge time.Duration) *health.Observation {
	hc := &c.health
	for attempt := 0; ; attempt++ {
		hc.mu.Lock()
		if maxAge > 0 && hc.fits(fallback) && time.Since(hc.last.At) <= maxAge {
			defer hc.mu.Unlock()
			return hc.last
		}
		done := hc.inflight
		if done == nil {
			want := fallback
			if want == AnyFallback {
				want = c.Info.Namespace
				if hc.last != nil {
					want = hc.fallback
				}
			}
			done = make(chan struct{})
			hc.inflight, hc.reading = done, want
			// Detached from this caller: someone else may be waiting on it,
			// and every read in it is bounded by HealthReadTimeout (twice,
			// for a namespace fallback).
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*healthReadTimeout)
			go func() {
				defer cancel()
				obs := c.readHealth(rctx, want)
				hc.mu.Lock()
				hc.last, hc.fallback, hc.inflight = obs, want, nil
				hc.mu.Unlock()
				close(done)
			}()
		}
		hc.mu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return abandonedHealth(ctx.Err())
		}
		hc.mu.Lock()
		// The read just finished is the newest there is. If it was made for
		// another namespace and that matters, read again for this one.
		if hc.fits(fallback) || attempt >= 1 {
			defer hc.mu.Unlock()
			return hc.last
		}
		hc.mu.Unlock()
	}
}

// abandonedHealth is the observation of a caller that stopped waiting: every
// read attempted, none answered in time.
func abandonedHealth(err error) *health.Observation {
	now := time.Now()
	read := func(source metrics.Source) health.Read {
		return health.Read{Done: true, Status: classify(source, AllNamespaces, err), At: now}
	}
	return &health.Observation{
		At: now, PodsRead: read(metrics.PodObjects), NodesRead: read(metrics.NodeObjects),
		DeploymentsRead: read("deployments"), StatefulSetsRead: read("statefulsets"), DaemonSetsRead: read("daemonsets"),
		EventsRead: read("events"), Metrics: abandoned(AllNamespaces, err),
	}
}
