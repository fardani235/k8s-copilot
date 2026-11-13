package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/kube"
)

// get_findings gives the copilot the report the health screen shows.
//
// It classifies nothing of its own: it asks kube.Cluster.HealthObservation for
// the observation (the one behind the screen, if that is recent enough), has
// internal/health classify it — the same pure function the screen calls — and
// prints the report. What is wrong, how it is described and why a class was
// not watched all come from internal/health.
func (r *Registry) registerHealth() {
	r.add(&tool{
		Spec: Spec{
			Name: "get_findings",
			Description: "Read the cluster's current health findings: crash-looping or repeatedly restarting containers, images that cannot be pulled, running pods with no ready container, nodes NotReady or under memory/disk pressure, Deployments/StatefulSets/DaemonSets whose rollout is not progressing, containers and nodes at the metrics screen's red (90% of a limit or of allocatable), and recent Warning events. " +
				"Use it to answer \"is anything broken?\" or \"what is wrong?\" instead of inferring health from listings. These are the same findings the user sees on the health screen. Current state only: no history. " +
				"Every class of finding comes with its state: watched, watched in part, forbidden, unavailable or not applicable. A class that was not watched was NOT checked — never describe it as healthy. Read-only.",
			Tier: Read,
			Schema: schema(map[string]string{
				"namespace": strProp("Only used if the cluster refuses a cluster-wide read: the namespace to read instead. Give the namespace line of the browser focus (on the health screen that is the namespace the screen falls back to). Leave out to use whatever the health screen last used."),
			}),
		},
		read: r.getFindings,
	})
}

type findingsArgs struct {
	Namespace string `json:"namespace"`
}

func (r *Registry) getFindings(ctx context.Context, raw json.RawMessage) (string, error) {
	const name = "get_findings"
	var a findingsArgs
	if err := decode(name, raw, &a); err != nil {
		return "", err
	}
	fallback := strings.TrimSpace(a.Namespace)
	switch fallback {
	case "", "*":
		fallback = kube.AnyFallback
	default:
		if problems := validation.IsDNS1123Label(fallback); len(problems) > 0 {
			return "", &ArgError{Tool: name, Msg: fmt.Sprintf("%q is not a valid namespace name: %s", fallback, problems[0])}
		}
	}
	// A recent observation is reused rather than re-read, so that what the
	// model is told is what is on the user's screen.
	obs := r.c.HealthObservation(ctx, fallback, kube.HealthFreshFor)
	return FindingsReport(health.Observe(obs), time.Now(), r.opts.MaxResultBytes), nil
}

// FindingsReport writes a report for the model within maxBytes: findings are
// dropped — and counted — to fit, never a class's state.
func FindingsReport(rep *health.Report, now time.Time, maxBytes int) string {
	return rep.Text(now, maxBytes)
}
