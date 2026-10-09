package kube

import (
	"testing"
	"time"

	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// Old readings stand in for a failed refresh only for a while. After that
// the failure itself is what callers get: stale numbers do not stay on show
// indefinitely with a warning nobody reads any more.
func TestHeldReadingsExpire(t *testing.T) {
	t0 := time.Now()
	down := metrics.SourceStatus{State: metrics.Unavailable}
	good := &metrics.Snapshot{TakenAt: t0}
	failed := func(after time.Duration) *metrics.Snapshot {
		return &metrics.Snapshot{TakenAt: t0.Add(after), Sources: metrics.Sources{
			NodeMetrics: down, PodMetrics: down,
		}}
	}
	if !good.HasUsage() || failed(0).HasUsage() {
		t.Fatal("test snapshots are not what they are meant to be")
	}

	usable := (*metrics.Snapshot).HasUsage
	e := &metricsEntry{last: failed(MetricsHoldFor - time.Second), good: good}
	if v := e.view(usable); v.Held == nil || v.TakenAt != t0 {
		t.Fatalf("within the hold: %+v", v)
	}
	e.last = failed(MetricsHoldFor + time.Second)
	if v := e.view(usable); v.Held != nil || v.HasUsage() {
		t.Fatalf("past the hold the old readings are still handed out: held=%v", v.Held)
	}
	// A partial success is current data, not a reason to show older data.
	partial := failed(time.Second)
	partial.Sources.NodeMetrics = metrics.SourceStatus{}
	e.last = partial
	if v := e.view(usable); v != partial {
		t.Fatal("a snapshot with some usage was replaced by an older one")
	}
	if (&metricsEntry{}).view(usable) != nil {
		t.Fatal("an entry that was never read has a view")
	}
}
