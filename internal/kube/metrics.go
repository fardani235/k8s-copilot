package kube

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// Usage comes from the resource metrics API (metrics.k8s.io), the one
// `kubectl top` reads. It is fetched with the dynamic client — the same
// read-only `list` the browser uses for everything else — so it needs no new
// dependency, no new permission and nothing installed in the cluster. If the
// cluster does not serve it, or will not let this user read it, that is
// reported as such (metrics.SourceStatus); nothing is guessed.
var (
	nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
	podMetricsGVR  = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	nodesGVR       = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	podsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

const (
	// MetricsTimeout bounds one refresh. A slow metrics API costs at most
	// this long, off the UI goroutine.
	MetricsTimeout = 20 * time.Second
	// MetricsFreshFor is how old a reading may be and still be handed out
	// instead of asking the cluster again. metrics-server takes a new sample
	// every 15s by default, so asking more often returns the same numbers.
	MetricsFreshFor = 15 * time.Second
	// MetricsHoldFor is how long the last good readings stand in — marked as
	// such — when a refresh gets no usage at all. Beyond it the failure is
	// shown instead.
	MetricsHoldFor = 60 * time.Second

	metricsPageSize = 500
	// metricsMaxItems caps one source. Beyond it the snapshot says it is
	// truncated rather than read without bound.
	metricsMaxItems = 20_000
)

// metricsCache keeps the latest readings, so that the screen and the copilot
// are handed the same Snapshot instead of two reads taken a few seconds
// apart.
//
// There is one cluster-wide read, and everyone's node readings come from it.
// Pod readings come from it too — a question about one namespace is answered
// by narrowing it — unless the cluster refuses this user the pods of the
// whole cluster. Only then are a namespace's pods read on their own, and they
// are joined to the same node readings. So two callers can never hold
// snapshots of the same thing taken at different moments.
type metricsCache struct {
	mu      sync.Mutex
	entries map[string]*metricsEntry
}

type metricsEntry struct {
	last *metrics.Snapshot // the newest read, whatever it got
	good *metrics.Snapshot // the newest read that got what it was for
	// inflight is closed when the running read finishes.
	inflight chan struct{}
}

// view is what callers are given: the newest read, or — if that got nothing
// but a recent one did — the recent one, marked as held.
func (e *metricsEntry) view(usable func(*metrics.Snapshot) bool) *metrics.Snapshot {
	if e.last == nil {
		return nil
	}
	if !usable(e.last) && e.good != nil && e.last.TakenAt.Sub(e.good.TakenAt) <= MetricsHoldFor {
		return e.good.HeldBy(e.last)
	}
	return e.last
}

// Metrics returns the cluster's current load: usage per node, namespace, pod
// and container, joined with capacity, requests and limits.
//
// namespace limits the pods to one namespace; AllNamespaces is every pod.
// Nodes are always cluster-wide.
//
// Readings no older than maxAge are returned as they are (maxAge 0 always
// asks the cluster). Concurrent callers share one read. The result is never
// nil and never an error: each of the four sources carries its own status,
// and a source that failed is a stated fact about the snapshot, not a missing
// snapshot.
//
// It only ever sends `list` requests.
func (c *Cluster) Metrics(ctx context.Context, namespace string, maxAge time.Duration) *metrics.Snapshot {
	wide := c.metrics.get(ctx, AllNamespaces, maxAge, (*metrics.Snapshot).HasUsage,
		func(ctx context.Context) *metrics.Snapshot { return c.readMetrics(ctx, AllNamespaces, true) })
	if namespace == AllNamespaces {
		return wide
	}
	if _, refused := wide.PodsDeniedClusterWide(); !refused {
		return wide.Narrow(namespace)
	}
	// Not allowed to read pods across the cluster; one namespace may be
	// another matter.
	pods := c.metrics.get(ctx, namespace, maxAge, func(s *metrics.Snapshot) bool { return s.Sources.PodMetrics.OK() },
		func(ctx context.Context) *metrics.Snapshot { return c.readMetrics(ctx, namespace, false) })
	return wide.WithPodsOf(pods)
}

// get returns the readings kept under key if they are young enough, and
// otherwise reads — joining a read that is already out rather than starting
// a second.
func (mc *metricsCache) get(ctx context.Context, key string, maxAge time.Duration,
	usable func(*metrics.Snapshot) bool, read func(context.Context) *metrics.Snapshot) *metrics.Snapshot {
	mc.mu.Lock()
	if mc.entries == nil {
		mc.entries = map[string]*metricsEntry{}
	}
	e := mc.entries[key]
	if e == nil {
		e = &metricsEntry{}
		mc.entries[key] = e
	}
	if maxAge > 0 && e.last != nil && time.Since(e.last.TakenAt) <= maxAge {
		defer mc.mu.Unlock()
		return e.view(usable)
	}
	done := e.inflight
	if done == nil {
		done = make(chan struct{})
		e.inflight = done
		// The read is detached from this caller: someone else may be waiting
		// on it, and it is bounded by its own timeout.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), MetricsTimeout)
		go func() {
			defer cancel()
			snap := read(rctx)
			mc.mu.Lock()
			e.last = snap
			if usable(snap) {
				e.good = snap
			}
			e.inflight = nil
			mc.mu.Unlock()
			close(done)
		}()
	}
	mc.mu.Unlock()

	select {
	case <-done:
		mc.mu.Lock()
		defer mc.mu.Unlock()
		return e.view(usable)
	case <-ctx.Done():
		return abandoned(key, ctx.Err())
	}
}

// abandoned is the snapshot of a caller that stopped waiting.
func abandoned(namespace string, err error) *metrics.Snapshot {
	var src metrics.Sources
	for _, s := range []struct {
		st     *metrics.SourceStatus
		source metrics.Source
	}{{&src.NodeMetrics, metrics.NodeMetrics}, {&src.PodMetrics, metrics.PodMetrics}, {&src.Nodes, metrics.NodeObjects}, {&src.Pods, metrics.PodObjects}} {
		*s.st = classify(s.source, namespace, err)
	}
	return metrics.Build(metrics.Inputs{Scope: namespace, TakenAt: time.Now(), Sources: src})
}

// readMetrics makes the reads side by side and joins them: all four, or —
// for a namespace's pods on their own — the two pod reads.
func (c *Cluster) readMetrics(ctx context.Context, namespace string, withNodes bool) *metrics.Snapshot {
	in := metrics.Inputs{Scope: namespace}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	read := func(source metrics.Source, gvr schema.GroupVersionResource, ns string, cached bool, st *metrics.SourceStatus, items *[]unstructured.Unstructured) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, truncated, err := c.listAll(ctx, gvr, ns, cached)
			if err != nil && ctx.Err() != nil {
				err = ctx.Err() // the deadline, rather than however the transport phrased it
			}
			mu.Lock()
			defer mu.Unlock()
			*st = classify(source, ns, err)
			if err == nil {
				*items = got
				if truncated {
					in.Truncated = append(in.Truncated, source)
				}
			}
		}()
	}
	if withNodes {
		read(metrics.NodeMetrics, nodeMetricsGVR, "", false, &in.Sources.NodeMetrics, &in.NodeMetrics)
	}
	read(metrics.PodMetrics, podMetricsGVR, namespace, false, &in.Sources.PodMetrics, &in.PodMetrics)
	// Capacity, requests and limits change rarely, and usage is a sample that
	// is seconds old anyway: let the API server answer these two from its
	// cache instead of making a quorum read on every refresh.
	if withNodes {
		read(metrics.NodeObjects, nodesGVR, "", true, &in.Sources.Nodes, &in.Nodes)
	}
	read(metrics.PodObjects, podsGVR, namespace, true, &in.Sources.Pods, &in.Pods)
	wg.Wait()
	in.TakenAt = time.Now()
	return metrics.Build(in)
}

// listAll lists a resource to the end, following continue tokens, up to
// metricsMaxItems.
func (c *Cluster) listAll(ctx context.Context, gvr schema.GroupVersionResource, namespace string, cached bool) (items []unstructured.Unstructured, truncated bool, err error) {
	opts := metav1.ListOptions{Limit: metricsPageSize}
	if cached {
		opts.ResourceVersion = "0"
	}
	for {
		l, err := c.dyn.Resource(gvr).Namespace(namespace).List(ctx, opts)
		if err != nil {
			return nil, false, err
		}
		items = append(items, l.Items...)
		next := l.GetContinue()
		if next == "" {
			return items, false, nil
		}
		// A page that brings nothing but promises more would be followed
		// forever: stop, and say the list is incomplete.
		if len(items) >= metricsMaxItems || len(l.Items) == 0 {
			return items, true, nil
		}
		opts = metav1.ListOptions{Limit: metricsPageSize, Continue: next}
	}
}

// classify turns the error of one read into what it means. The distinctions
// matter to the person reading the screen: "not installed", "broken", "not
// allowed" and "slow" call for different reactions, and none of them is
// "idle".
func classify(source metrics.Source, namespace string, err error) metrics.SourceStatus {
	st := metrics.SourceStatus{Source: source, Scope: namespace}
	if err == nil {
		return st
	}
	st.Reason = Reason(err)
	var netErr net.Error
	switch {
	case apierrors.IsUnauthorized(err):
		// Who are you, not what may you do: an expired token, not RBAC.
		st.State = metrics.Unauthenticated
	case apierrors.IsForbidden(err):
		st.State = metrics.Denied
	case apierrors.IsNotFound(err):
		// The group or resource is not served: no APIService for
		// metrics.k8s.io.
		st.State = metrics.Absent
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err),
		errors.As(err, &netErr) && netErr.Timeout():
		st.State = metrics.TimedOut
	case apierrors.IsServiceUnavailable(err), apierrors.IsInternalError(err), apierrors.IsUnexpectedServerError(err):
		// The aggregator answers 503 when the APIService exists but its
		// backend (metrics-server) does not respond. (Being rate-limited, a
		// 429, is not that: it falls through to "failed", with the server's
		// own words.)
		st.State = metrics.Unavailable
	default:
		st.State = metrics.Failed
	}
	return st
}
