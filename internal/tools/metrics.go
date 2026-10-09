package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// get_metrics gives the copilot the readings the metrics screen shows.
//
// It does not compute anything of its own: it asks kube.Cluster.Metrics for
// the snapshot (the one the screen is showing, if that is recent enough),
// asks the snapshot for the same Listing the screen draws, and prints it as
// text. The numbers, the percentages, what counts as HIGH and the wording of
// "this source is unavailable" all come from internal/metrics.
func (r *Registry) registerMetrics() {
	r.add(&tool{
		Spec: Spec{
			Name: "get_metrics",
			Description: "Read current CPU and memory usage from the cluster's metrics API (the source of `kubectl top`), set against node capacity and pod requests/limits. " +
				"Levels, from the big picture down: nodes (plus cluster totals), namespaces, pods, containers (of one pod). " +
				"Use it to check whether something is actually CPU-saturated or short of memory instead of inferring it from logs. " +
				"These are the same readings the user sees on the metrics screen. Current values only: no history. " +
				"If the metrics API is missing, broken or not permitted, the result says so — that means usage is unknown, never that it is zero. Read-only.",
			Tier: Read,
			Schema: schema(map[string]string{
				"level":     `{"type":"string","enum":["nodes","namespaces","pods","containers"],"description":"What to list. nodes: every node against its allocatable, with cluster totals. namespaces: usage summed per namespace. pods: pods, optionally of one namespace and/or one node. containers: the containers of one pod, with requests, limits, restarts and last exit reason."}`,
				"namespace": strProp("pods / namespaces: only this namespace (leave out for all namespaces). containers: the pod's namespace (required)."),
				"node":      strProp("pods only: just the pods running on this node."),
				"pod":       strProp("containers only: the pod name (required)."),
				"sort":      `{"type":"string","enum":["cpu","memory","name"],"description":"Order of the rows, highest first for cpu and memory (default cpu)."}`,
				"limit":     intProp("Maximum rows (default 30, max 200). The result says when there are more."),
			}, "level"),
		},
		read: r.getMetrics,
	})
}

type metricsArgs struct {
	Level     string `json:"level"`
	Namespace string `json:"namespace"`
	Node      string `json:"node"`
	Pod       string `json:"pod"`
	Sort      string `json:"sort"`
	Limit     int    `json:"limit"`
}

func (r *Registry) getMetrics(ctx context.Context, raw json.RawMessage) (string, error) {
	const name = "get_metrics"
	var a metricsArgs
	if err := decode(name, raw, &a); err != nil {
		return "", err
	}
	bad := func(format string, args ...any) (string, error) {
		return "", &ArgError{Tool: name, Msg: fmt.Sprintf(format, args...)}
	}
	q := metrics.Query{
		Level: metrics.Level(strings.ToLower(strings.TrimSpace(a.Level))), Namespace: strings.TrimSpace(a.Namespace),
		Node: strings.TrimSpace(a.Node), Pod: strings.TrimSpace(a.Pod), Sort: metrics.Sort(strings.ToLower(strings.TrimSpace(a.Sort))),
	}
	if q.Namespace == "*" {
		q.Namespace = kube.AllNamespaces
	}
	switch q.Level {
	case metrics.LevelNodes:
		if q.Namespace != "" || q.Node != "" || q.Pod != "" {
			return bad(`level "nodes" lists every node and takes no "namespace", "node" or "pod"; to see what runs on a node use level "pods" with "node"`)
		}
	case metrics.LevelNamespaces:
		if q.Node != "" || q.Pod != "" {
			return bad(`level "namespaces" takes no "node" or "pod"`)
		}
	case metrics.LevelPods:
		if q.Pod != "" {
			return bad(`"pod" is for level "containers"; level "pods" lists pods`)
		}
	case metrics.LevelContainers:
		if q.Namespace == "" || q.Pod == "" {
			return bad(`level "containers" needs both "namespace" and "pod"`)
		}
		if q.Node != "" {
			return bad(`level "containers" takes no "node"`)
		}
	case "":
		return bad(`"level" is required: one of nodes, namespaces, pods, containers`)
	default:
		return bad(`"level" must be one of nodes, namespaces, pods, containers, not %q`, a.Level)
	}
	switch q.Sort {
	case "":
		q.Sort = metrics.ByCPU
	case metrics.ByCPU, metrics.ByMemory, metrics.ByName:
	default:
		return bad(`"sort" must be cpu, memory or name, not %q`, a.Sort)
	}
	switch {
	case a.Limit <= 0:
		a.Limit = 30
	case a.Limit > 200:
		a.Limit = 200
	}
	// Names are checked here rather than sent: a malformed one is the
	// caller's mistake, and must not come back looking like something the
	// cluster said about its metrics.
	for _, n := range []struct{ arg, value string }{{"namespace", q.Namespace}, {"node", q.Node}, {"pod", q.Pod}} {
		if n.value == "" {
			continue
		}
		problems := validation.IsDNS1123Subdomain(n.value)
		if n.arg == "namespace" {
			problems = validation.IsDNS1123Label(n.value)
		}
		if len(problems) > 0 {
			return bad("%q is not a valid %s name: %s", n.value, n.arg, problems[0])
		}
	}

	// Recent readings are reused rather than re-read, so that what the model
	// quotes is what is on the user's screen.
	snap := r.c.Metrics(ctx, q.Namespace, kube.MetricsFreshFor)
	l, now := snap.List(q), time.Now()
	for limit := a.Limit; ; {
		out, err := MetricsReport(snap, l, now, limit)
		if err != nil {
			return "", err
		}
		// Too long for one result: show fewer rows — and say so — rather
		// than let the cap cut the report off mid-table.
		if len(out) <= r.opts.MaxResultBytes || limit <= 1 {
			return r.cap(out), nil
		}
		limit = max(1, min(limit-1, limit*r.opts.MaxResultBytes/len(out)))
	}
}

// MetricsReport writes a listing for the model. When the readings behind the
// listing could not be obtained it returns an error that says why, in the
// same words the screen uses, and that insists on what it means: unknown,
// not zero.
func MetricsReport(s *metrics.Snapshot, l *metrics.Listing, now time.Time, limit int) (string, error) {
	if st := l.Unavailable; st != nil {
		var b strings.Builder
		switch st.State {
		case metrics.Denied:
			b.WriteString("permission denied: ")
		case metrics.Unauthenticated:
			b.WriteString("credentials rejected: ")
		default:
			b.WriteString("metrics unavailable: ")
		}
		b.WriteString(st.Headline() + ". " + st.Explain())
		if st.Reason != "" {
			b.WriteString(" The server said: " + textutil.OneLine(textutil.Sanitize(st.Reason)) + ".")
		}
		b.WriteString(" Usage is UNKNOWN, not zero: do not describe anything as idle, healthy or lightly loaded on this basis. Tell the user plainly that this is why you cannot give CPU or memory figures, then rely on other evidence (events, restart counts, OOMKilled, logs).")
		if _, denied := s.PodsDeniedClusterWide(); denied && st.Source == metrics.PodMetrics {
			b.WriteString(` Reading a single namespace may still be allowed: call again with "namespace" set (the browser focus names the one the user is in).`)
		}
		switch {
		case st.Source == metrics.PodMetrics && s.Sources.NodeMetrics.OK():
			b.WriteString(` Node metrics are readable: level "nodes" works.`)
		case st.Source == metrics.NodeMetrics && s.Sources.PodMetrics.OK():
			b.WriteString(` Pod metrics are readable: levels "namespaces", "pods" and "containers" work.`)
		}
		return "", errors.New(b.String())
	}

	// Everything that qualifies the numbers comes before the numbers: if the
	// result has to be shortened, it is rows that go, not caveats.
	var b strings.Builder
	clean := func(s string) string { return textutil.OneLine(textutil.Sanitize(s)) }
	fresh := s.Freshness(now)
	fmt.Fprintf(&b, "Metrics API readings, %s; pods read for %s.\n", fresh.Text, clean(metrics.ScopeText(s.Scope)))
	if s.Held != nil {
		fmt.Fprintf(&b, "WARNING: the latest refresh failed (%s), so these are the last readings that were obtained, not current ones. Say so when you quote them.\n", clean(s.Held.Cause.Headline()))
	} else if fresh.Warn {
		b.WriteString("WARNING: the newest sample is old; the metrics source may have stopped updating. Say so when you quote these numbers.\n")
	}
	for _, line := range l.Summary.Lines() {
		b.WriteString(line + "\n")
	}
	for _, n := range l.Notes {
		b.WriteString("note: " + clean(n.Text) + "\n")
	}
	fmt.Fprintf(&b, "Reading the table: %s is no reading (unknown), never zero. %%…/R is usage as a share of requests, %%…/L of limits; \"no req\" / \"no lim\" / \"none\" mean none is set. HIGH is %d%% or more of a hard bound (allocatable, limit), elevated %d%% or more.\n\n",
		metrics.NoData, metrics.HighAt, metrics.ElevatedAt)

	if l.Empty != "" {
		b.WriteString(clean(l.Empty) + " (The metrics API did answer.)\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "%s, by %s", clean(l.What), l.Query.Sort)
	if limit > 0 && len(l.Rows) > limit {
		fmt.Fprintf(&b, " — the top %d of %d", limit, len(l.Rows))
	}
	b.WriteString(":\n" + l.Table(limit))
	return b.String(), nil
}
