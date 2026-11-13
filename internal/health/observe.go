package health

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// Read is the outcome of one list behind an observation.
//
// The zero value is a read that was never made. It is not a read that found
// nothing: a class whose read was not Done is not watched.
type Read struct {
	// Done is set once the read was attempted, whatever its outcome.
	Done bool
	// Status is the outcome of the read the items came from. Its Source
	// names the resource ("pods", "deployments", …); its Scope is the
	// namespace it was limited to, empty for the whole cluster.
	Status metrics.SourceStatus
	// Refused is the cluster-wide refusal that made this a namespace read;
	// nil when the read was cluster-wide.
	Refused *metrics.SourceStatus
	// Truncated is set when there were more objects than the read returned
	// (ListCap). Cut is how many more, when the server said; 0 if unknown.
	Truncated bool
	Cut       int64
	// At is when the answer was received, by the local clock.
	At time.Time
}

// OK reports whether the read answered.
func (r Read) OK() bool { return r.Done && r.Status.OK() }

// Observation is everything one look at the cluster obtained.
type Observation struct {
	// At is when the observation was assembled; ages and grace periods are
	// measured against it.
	At time.Time

	Pods     []corev1.Pod
	PodsRead Read

	Nodes     []corev1.Node
	NodesRead Read

	Deployments      []appsv1.Deployment
	DeploymentsRead  Read
	StatefulSets     []appsv1.StatefulSet
	StatefulSetsRead Read
	DaemonSets       []appsv1.DaemonSet
	DaemonSetsRead   Read

	// Events are Warning events (others are ignored), read over EventWindow.
	Events     []corev1.Event
	EventsRead Read

	// Metrics is the shared metrics snapshot; nil when it was not part of
	// the observation. MetricsRefused is the cluster-wide refusal when its
	// pods were narrowed to one namespace.
	Metrics        *metrics.Snapshot
	MetricsRefused *metrics.SourceStatus
}

// Observe classifies an observation. It is pure: the same observation always
// gives the same report, which is how the screen and the copilot are given the
// same findings.
func Observe(obs *Observation) *Report {
	if obs == nil {
		obs = &Observation{}
	}
	// A read that never happened still has a name, so that its class can say
	// what was not read.
	o := *obs
	for _, n := range []struct {
		r    *Read
		name string
	}{{&o.PodsRead, "pods"}, {&o.NodesRead, "nodes"}, {&o.DeploymentsRead, "deployments"}, {&o.StatefulSetsRead, "statefulsets"},
		{&o.DaemonSetsRead, "daemonsets"}, {&o.EventsRead, "events"}} {
		if n.r.Status.Source == "" {
			n.r.Status.Source = metrics.Source(n.name)
		}
	}
	obs = &o
	r := &Report{ReadAt: obs.At}
	var all []Finding
	add := func(c Class, fs []Finding) {
		if c.State != Watched {
			fs = nil // an unwatched class contributes nothing
		}
		if c.Total == 0 {
			c.Total = len(fs)
		}
		c.Findings = len(fs)
		r.Classes = append(r.Classes, c)
		all = append(all, fs...)
	}

	// Workloads: pods.
	wc := classOf(Workloads, obs.PodsRead)
	add(wc, workloadFindings(obs))

	// Nodes.
	add(classOf(Nodes, obs.NodesRead), nodeFindings(obs))

	// Rollouts: three reads, and the pods for two of them.
	rc := classOf(Rollouts, obs.DeploymentsRead, obs.StatefulSetsRead, obs.DaemonSetsRead)
	rf := rolloutFindings(obs)
	if rc.State == Watched && (obs.StatefulSetsRead.OK() || obs.DaemonSetsRead.OK()) {
		switch {
		case !obs.PodsRead.OK():
			rc.Gaps = append(rc.Gaps, Gap{Source: "pods (StatefulSet and DaemonSet stalls are judged from their pods)", State: stateOf(obs.PodsRead), Why: whyOf(obs.PodsRead)})
		case obs.PodsRead.Truncated:
			rc.Truncated = append(rc.Truncated, fmt.Sprintf("StatefulSet and DaemonSet stalls judged from the first %d pods only", len(obs.Pods)))
		}
		// Pods read for one namespace only: StatefulSets and DaemonSets
		// elsewhere cannot be judged, however widely they were listed.
		if ns := obs.PodsRead.Status.Scope; obs.PodsRead.OK() && ns != "" && rc.Scope != ns {
			rc.Limits = append(rc.Limits, clean(fmt.Sprintf("StatefulSet and DaemonSet stalls judged in namespace %s only, where the pods were read", ns), maxMessage))
		}
	}
	add(rc, rf)

	// Usage: the metrics snapshot.
	uc, uf := usage(obs)
	add(uc, uf)

	// Events.
	ec := classOf(Events, obs.EventsRead)
	ef := eventFindings(obs)
	ec.Total = len(ef)
	if len(ef) > MaxEventFindings {
		ef = ef[:MaxEventFindings]
	}
	if ec.State != Watched {
		ec.Total = 0
	}
	add(ec, ef)

	order := map[ClassID]int{}
	for i, id := range ClassOrder {
		order[id] = i
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := &all[i], &all[j]
		if a.Class != b.Class {
			return order[a.Class] < order[b.Class]
		}
		if a.Class == Events {
			return a.Since.After(b.Since) // newest first
		}
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		return a.Key() < b.Key()
	})
	r.Findings = all

	// The age of the report is that of its oldest read.
	for _, rd := range []Read{obs.PodsRead, obs.NodesRead, obs.DeploymentsRead, obs.StatefulSetsRead, obs.DaemonSetsRead, obs.EventsRead} {
		if rd.Done && !rd.At.IsZero() && rd.At.Before(r.ReadAt) {
			r.ReadAt = rd.At
		}
	}
	if obs.Metrics != nil && !obs.Metrics.TakenAt.IsZero() && obs.Metrics.TakenAt.Before(r.ReadAt) {
		r.ReadAt = obs.Metrics.TakenAt
	}
	return r
}

// --- availability -------------------------------------------------------------

// stateOf maps the outcome of one read to the state it gives its class.
func stateOf(r Read) ClassState {
	if !r.Done {
		return Unavailable
	}
	switch r.Status.State {
	case metrics.OK:
		return Watched
	case metrics.Denied, metrics.Unauthenticated:
		return Forbidden
	}
	return Unavailable
}

// whyOf explains a read that did not answer, in the metrics screen's words.
func whyOf(r Read) string {
	if !r.Done {
		name := string(r.Status.Source)
		if name == "" {
			name = "this source"
		}
		return clean(name+" was not read.", maxMessage)
	}
	text := r.Status.Headline() + "."
	if r.Status.Reason != "" {
		text += " The server said: " + r.Status.Reason + "."
	}
	if r.Refused != nil {
		text = r.Refused.Headline() + "; the browser's namespace was tried instead. " + text
	}
	return clean(text, maxMessage)
}

// classOf derives a class's state from the reads it depends on. Every read
// answered: watched. None did: not watched, for the gravest reason among
// them. Some did: watched in part, naming what was not read.
func classOf(id ClassID, reads ...Read) Class {
	c := Class{ID: id}
	var ok []Read
	var failed []Read
	for _, r := range reads {
		if r.OK() {
			ok = append(ok, r)
		} else {
			failed = append(failed, r)
		}
	}
	if len(ok) == 0 {
		c.State = NotApplicable
		var whys []string
		for _, r := range failed {
			s := stateOf(r)
			if s == Forbidden || (s == Unavailable && c.State != Forbidden) {
				c.State = s
			}
			whys = append(whys, whyOf(r))
		}
		if len(failed) == 0 {
			c.State, whys = Unavailable, []string{"nothing was read."}
		}
		c.Why = strings.Join(whys, " ")
		return c
	}
	c.State = Watched
	for _, r := range failed {
		c.Gaps = append(c.Gaps, Gap{Source: clean(string(r.Status.Source), maxReason), State: stateOf(r), Why: whyOf(r)})
	}
	var why []string
	for _, r := range ok {
		if r.Status.Scope != "" {
			c.Scope = clean(r.Status.Scope, maxName)
			if r.Refused != nil {
				why = append(why, r.Refused.Headline())
			} else {
				why = append(why, string(r.Status.Source)+" were read for that namespace only")
			}
		}
		if r.Truncated {
			c.Truncated = append(c.Truncated, truncatedText(r))
		}
	}
	c.ScopeWhy = clean(strings.Join(why, "; "), maxMessage)
	return c
}

func truncatedText(r Read) string {
	src := clean(string(r.Status.Source), maxReason)
	if r.Cut > 0 {
		return fmt.Sprintf("only the first %d %s were read (%d more were not)", ListCap, src, r.Cut)
	}
	return fmt.Sprintf("only the first %d %s were read (more exist and were not)", ListCap, src)
}

// --- workloads ----------------------------------------------------------------

func isImagePull(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "RegistryUnavailable":
		return true
	}
	return false
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// notReadySince is when the pod's containers stopped being ready, as the
// cluster records it; zero if it does not.
func notReadySince(p *corev1.Pod) time.Time {
	for _, t := range []corev1.PodConditionType{corev1.ContainersReady, corev1.PodReady} {
		if c := podCondition(p, t); c != nil && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	if p.Status.StartTime != nil {
		return p.Status.StartTime.Time
	}
	return time.Time{}
}

func lastExit(cs *corev1.ContainerStatus) string {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		return ""
	}
	s := t.Reason
	if s == "" {
		s = "exited"
	}
	return fmt.Sprintf("%s, exit code %d", s, t.ExitCode)
}

func workloadFindings(obs *Observation) []Finding {
	if !obs.PodsRead.OK() {
		return nil
	}
	var out []Finding
	for i := range obs.Pods {
		p := &obs.Pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue // finished: nothing is running to be broken
		}
		base := Finding{Class: Workloads, Kind: "Pod", Namespace: clean(p.Namespace, maxName), Name: clean(p.Name, maxName)}
		images := map[string]string{}
		for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
			images[c.Name] = c.Image
		}
		flagged := false
		statuses := append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for j := range statuses {
			cs := &statuses[j]
			f := base
			f.Container = clean(cs.Name, maxName)
			f.Count = int(cs.RestartCount)
			w := cs.State.Waiting
			switch {
			case w != nil && w.Reason == "CrashLoopBackOff":
				f.Severity, f.Reason = Critical, "CrashLoopBackOff"
				msg := fmt.Sprintf("container %s is crash-looping: restarted %d times", cs.Name, cs.RestartCount)
				if e := lastExit(cs); e != "" {
					msg += "; last exit: " + e
				}
				f.Message, f.Since = clean(msg, maxMessage), notReadySince(p)
			case w != nil && isImagePull(w.Reason):
				image := cs.Image
				if image == "" {
					image = images[cs.Name]
				}
				f.Severity, f.Reason = Critical, clean(w.Reason, maxReason)
				msg := fmt.Sprintf("container %s cannot pull image %s", cs.Name, image)
				if w.Message != "" {
					msg += ": " + w.Message
				}
				f.Message, f.Since = clean(msg, maxMessage), notReadySince(p)
			case w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" &&
				!notReadySince(p).IsZero() && obs.At.Sub(notReadySince(p)) > NotReadyGrace:
				// Stuck before it ever runs: a missing Secret or ConfigMap
				// (CreateContainerConfigError), a bad command
				// (RunContainerError), …
				f.Severity, f.Reason = Critical, clean(w.Reason, maxReason)
				msg := fmt.Sprintf("container %s has been waiting (%s) for longer than %s", cs.Name, w.Reason, metrics.Age(NotReadyGrace))
				if w.Message != "" {
					msg += ": " + w.Message
				}
				f.Message, f.Since = clean(msg, maxMessage), notReadySince(p)
			case cs.RestartCount > RestartThreshold:
				f.Severity, f.Reason = Warning, "Restarting"
				msg := fmt.Sprintf("container %s has restarted %d times (more than %d)", cs.Name, cs.RestartCount, RestartThreshold)
				if t := cs.LastTerminationState.Terminated; t != nil {
					msg += "; last exit: " + lastExit(cs)
					if !t.FinishedAt.IsZero() {
						msg += ", " + metrics.Age(obs.At.Sub(t.FinishedAt.Time)) + " ago"
					}
				}
				f.Message = clean(msg, maxMessage)
			default:
				continue
			}
			flagged = true
			out = append(out, f)
		}
		// Running with no container ready. A pod already reported above is
		// not reported again for the consequence of the same cause.
		if flagged || p.Status.Phase != corev1.PodRunning || len(p.Spec.Containers) == 0 {
			continue
		}
		ready := map[string]bool{}
		for _, cs := range p.Status.ContainerStatuses {
			ready[cs.Name] = cs.Ready
		}
		var notReady []string
		for _, c := range p.Spec.Containers {
			if !ready[c.Name] {
				notReady = append(notReady, c.Name)
			}
		}
		if len(notReady) < len(p.Spec.Containers) {
			continue // at least one is ready
		}
		since := notReadySince(p)
		if since.IsZero() || obs.At.Sub(since) <= NotReadyGrace {
			continue
		}
		f := base
		f.Severity, f.Reason, f.Since = Warning, "NotReady", since
		f.Message = clean(fmt.Sprintf("running, but none of its containers is ready (%s) for longer than %s", strings.Join(notReady, ", "), metrics.Age(NotReadyGrace)), maxMessage)
		out = append(out, f)
	}
	return out
}

// --- nodes --------------------------------------------------------------------

func nodeFindings(obs *Observation) []Finding {
	if !obs.NodesRead.OK() {
		return nil
	}
	var out []Finding
	for i := range obs.Nodes {
		n := &obs.Nodes[i]
		hasReady := false
		for _, c := range n.Status.Conditions {
			hasReady = hasReady || c.Type == corev1.NodeReady
		}
		if !hasReady {
			out = append(out, Finding{Class: Nodes, Severity: Warning, Kind: "Node", Name: clean(n.Name, maxName), Reason: "NoReadyCondition",
				Message: "node reports no Ready condition: whether it is ready is unknown", Since: n.CreationTimestamp.Time})
		}
		for _, c := range n.Status.Conditions {
			f := Finding{Class: Nodes, Kind: "Node", Name: clean(n.Name, maxName), Since: c.LastTransitionTime.Time}
			switch {
			case c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue:
				f.Severity, f.Reason = Critical, "NotReady"
				msg := "node is not ready"
				if c.Status == corev1.ConditionUnknown {
					msg = "node status is unknown: the kubelet has stopped reporting"
				}
				if c.Reason != "" {
					msg += " (" + c.Reason + ")"
				}
				if c.Message != "" {
					msg += ": " + c.Message
				}
				f.Message = clean(msg, maxMessage)
			case (c.Type == corev1.NodeMemoryPressure || c.Type == corev1.NodeDiskPressure) && c.Status == corev1.ConditionTrue:
				f.Severity, f.Reason = Warning, string(c.Type)
				what := "memory"
				if c.Type == corev1.NodeDiskPressure {
					what = "disk"
				}
				msg := "node reports " + what + " pressure"
				if c.Message != "" {
					msg += ": " + c.Message
				}
				f.Message = clean(msg, maxMessage)
			default:
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// --- rollouts -----------------------------------------------------------------

// stalledPod finds a pod controlled by uid that has not been ready for longer
// than the grace period: the evidence that a StatefulSet's or DaemonSet's
// rollout is stuck rather than under way.
func stalledPod(obs *Observation, namespace string, uid types.UID) (*corev1.Pod, time.Duration) {
	for i := range obs.Pods {
		p := &obs.Pods[i]
		if p.Namespace != namespace || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		owned := false
		for _, o := range p.OwnerReferences {
			if o.UID == uid && o.Controller != nil && *o.Controller {
				owned = true
			}
		}
		if !owned {
			continue
		}
		if c := podCondition(p, corev1.PodReady); c != nil && c.Status == corev1.ConditionTrue {
			continue
		}
		since := notReadySince(p)
		if since.IsZero() {
			since = p.CreationTimestamp.Time
		}
		if !since.IsZero() && obs.At.Sub(since) > NotReadyGrace {
			return p, obs.At.Sub(since)
		}
	}
	return nil, 0
}

// podsCover reports whether the pod read covers a namespace.
func podsCover(obs *Observation, namespace string) bool {
	return obs.PodsRead.OK() && (obs.PodsRead.Status.Scope == "" || obs.PodsRead.Status.Scope == namespace)
}

func rolloutFindings(obs *Observation) []Finding {
	var out []Finding
	if obs.DeploymentsRead.OK() {
		for i := range obs.Deployments {
			d := &obs.Deployments[i]
			if d.Spec.Paused {
				continue // paused on purpose
			}
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			st := d.Status
			if st.ObservedGeneration >= d.Generation && st.UpdatedReplicas == desired && st.AvailableReplicas == desired && st.Replicas == desired {
				continue // complete
			}
			var cond *appsv1.DeploymentCondition
			for j := range st.Conditions {
				c := &st.Conditions[j]
				if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse {
					cond = c
					break
				}
				if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue && cond == nil {
					cond = c
				}
			}
			if cond == nil {
				continue // under way, as far as the cluster says
			}
			reason := cond.Reason
			if reason == "" {
				reason = string(cond.Type)
			}
			msg := fmt.Sprintf("rollout is not progressing: %d of %d replicas available, %d updated", st.AvailableReplicas, desired, st.UpdatedReplicas)
			if cond.Message != "" {
				msg += ": " + cond.Message
			}
			out = append(out, Finding{Class: Rollouts, Severity: Critical, Kind: "Deployment",
				Namespace: clean(d.Namespace, maxName), Name: clean(d.Name, maxName),
				Reason: clean(reason, maxReason), Message: clean(msg, maxMessage), Since: cond.LastTransitionTime.Time})
		}
	}
	stalled := func(kind, namespace, name string, uid types.UID, ready, desired int32) {
		if ready >= desired || !podsCover(obs, namespace) {
			return
		}
		p, held := stalledPod(obs, namespace, uid)
		if p == nil {
			return
		}
		msg := fmt.Sprintf("rollout is not progressing: %d of %d ready; pod %s has not been ready for %s", ready, desired, p.Name, metrics.Age(held))
		out = append(out, Finding{Class: Rollouts, Severity: Critical, Kind: kind,
			Namespace: clean(namespace, maxName), Name: clean(name, maxName),
			Reason: "NotProgressing", Message: clean(msg, maxMessage), Since: obs.At.Add(-held)})
	}
	if obs.StatefulSetsRead.OK() {
		for i := range obs.StatefulSets {
			s := &obs.StatefulSets[i]
			desired := int32(1)
			if s.Spec.Replicas != nil {
				desired = *s.Spec.Replicas
			}
			// Updates held back by OnDelete or a partition are deliberate:
			// only replicas that are not ready count.
			stalled("StatefulSet", s.Namespace, s.Name, s.UID, s.Status.ReadyReplicas, desired)
		}
	}
	if obs.DaemonSetsRead.OK() {
		for i := range obs.DaemonSets {
			d := &obs.DaemonSets[i]
			stalled("DaemonSet", d.Namespace, d.Name, d.UID, d.Status.NumberReady, d.Status.DesiredNumberScheduled)
		}
	}
	return out
}

// --- usage --------------------------------------------------------------------

func metricsRead(st metrics.SourceStatus) Read { return Read{Done: true, Status: st} }

func usage(obs *Observation) (Class, []Finding) {
	s := obs.Metrics
	if s == nil {
		return Class{ID: Usage, State: NotApplicable, Why: "Metrics were not part of this reading."}, nil
	}
	if s.Held != nil {
		c := Class{ID: Usage, State: Unavailable, Why: clean(fmt.Sprintf("The latest metrics refresh failed (%s); earlier readings are not presented as current.", s.Held.Cause.Headline()), maxMessage)}
		return c, nil
	}
	src := s.Sources
	if !src.NodeMetrics.OK() && !src.PodMetrics.OK() {
		c := classOf(Usage, metricsRead(src.NodeMetrics), metricsRead(src.PodMetrics))
		if src.NodeMetrics.State == metrics.Absent || src.PodMetrics.State == metrics.Absent {
			c.State = NotApplicable
			c.Why = clean(metrics.SourceStatus{Source: metrics.NodeMetrics, State: metrics.Absent}.Headline()+
				": the cluster does not serve the metrics API (metrics.k8s.io). k8s-copilot installs nothing.", maxMessage)
		}
		return c, nil
	}
	c := classOf(Usage, metricsRead(src.NodeMetrics), metricsRead(src.PodMetrics), metricsRead(src.Nodes), metricsRead(src.Pods))
	if s.Scope != "" {
		c.Scope = clean(s.Scope, maxName)
		why := "pod metrics were read for that namespace only"
		if obs.MetricsRefused != nil {
			why = obs.MetricsRefused.Headline()
		}
		c.ScopeWhy = clean(why, maxMessage)
	}
	for _, t := range s.Truncated {
		c.Truncated = append(c.Truncated, clean(fmt.Sprintf("%s were cut short; not every reading was read", t), maxMessage))
	}
	// The metrics API answered, but that is not the same as having a
	// reading for everything: a node or running pod without one is unknown,
	// not idle.
	var silentNodes, silentPods, reporting int
	if src.NodeMetrics.OK() {
		for i := range s.Nodes {
			if s.Nodes[i].CPU.OK || s.Nodes[i].Mem.OK {
				reporting++
			} else {
				silentNodes++
			}
		}
	}
	if src.PodMetrics.OK() {
		for i := range s.Pods {
			switch p := &s.Pods[i]; {
			case p.Reporting():
				reporting++
			case p.Phase == "Running":
				silentPods++
			}
		}
	}
	if reporting == 0 && silentNodes+silentPods > 0 {
		return Class{ID: Usage, State: Unavailable, Why: "The metrics API answered but holds no readings for any node or running pod (metrics-server may still be starting, or cannot reach the kubelets). Usage is unknown, not idle."}, nil
	}
	if silentNodes > 0 {
		c.Limits = append(c.Limits, fmt.Sprintf("no reading for %s, whose usage is unknown", plural(silentNodes, "node")))
	}
	if silentPods > 0 {
		c.Limits = append(c.Limits, fmt.Sprintf("no reading for %s, whose usage is unknown", plural(silentPods, "running pod")))
	}

	var out []Finding
	hot := func(used, of metrics.Amount, format func(metrics.Amount) string) (int, string, bool) {
		pct, ok := metrics.Percent(used, of)
		if !ok || metrics.HeatOf(pct) != metrics.High {
			return 0, "", false
		}
		return pct, fmt.Sprintf("%s of %s", format(used), format(of)), true
	}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		for _, g := range []struct {
			what      string
			used, of  metrics.Amount
			formatter func(metrics.Amount) string
		}{{"CPU", n.CPU, n.CPUAlloc, metrics.FormatCPU}, {"memory", n.Mem, n.MemAlloc, metrics.FormatMem}} {
			pct, amounts, ok := hot(g.used, g.of, g.formatter)
			if !ok {
				continue
			}
			out = append(out, Finding{Class: Usage, Severity: Warning, Kind: "Node", Name: clean(n.Name, maxName),
				Reason:  fmt.Sprintf("%s at %d%% of allocatable", g.what, pct),
				Message: clean(fmt.Sprintf("node uses %s allocatable %s (%d%%)", amounts, g.what, pct), maxMessage), Since: n.Sampled})
		}
	}
	for i := range s.Pods {
		p := &s.Pods[i]
		for j := range p.Containers {
			ct := &p.Containers[j]
			for _, g := range []struct {
				what      string
				used, of  metrics.Amount
				formatter func(metrics.Amount) string
			}{{"CPU", ct.CPU, ct.CPULim, metrics.FormatCPU}, {"memory", ct.Mem, ct.MemLim, metrics.FormatMem}} {
				pct, amounts, ok := hot(g.used, g.of, g.formatter)
				if !ok {
					continue
				}
				consequence := "an OOM kill is close"
				if g.what == "CPU" {
					consequence = "it is being throttled"
				}
				out = append(out, Finding{Class: Usage, Severity: Warning, Kind: "Pod",
					Namespace: clean(p.Namespace, maxName), Name: clean(p.Name, maxName), Container: clean(ct.Name, maxName),
					Reason:  fmt.Sprintf("%s at %d%% of limit", g.what, pct),
					Message: clean(fmt.Sprintf("container %s uses %s %s limit (%d%%): %s", ct.Name, amounts, g.what, pct, consequence), maxMessage), Since: p.Sampled})
			}
		}
	}
	return c, out
}

// --- events -------------------------------------------------------------------

// eventTime is the latest time the cluster records for an event.
func eventTime(e *corev1.Event) time.Time {
	t := e.CreationTimestamp.Time
	for _, c := range []time.Time{e.FirstTimestamp.Time, e.EventTime.Time, e.LastTimestamp.Time} {
		if c.After(t) {
			t = c
		}
	}
	if e.Series != nil && e.Series.LastObservedTime.Time.After(t) {
		t = e.Series.LastObservedTime.Time
	}
	return t
}

func eventFindings(obs *Observation) []Finding {
	if !obs.EventsRead.OK() {
		return nil
	}
	from := obs.At.Add(-EventWindow)
	groups := map[string]*Finding{}
	var order []string
	for i := range obs.Events {
		e := &obs.Events[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		at := eventTime(e)
		if at.Before(from) {
			continue
		}
		count := int(e.Count)
		if e.Series != nil && int(e.Series.Count) > count {
			count = int(e.Series.Count)
		}
		count = max(count, 1)
		io := e.InvolvedObject
		ns := io.Namespace
		if ns == "" && io.Kind != "Node" && io.Kind != "Namespace" {
			ns = e.Namespace
		}
		key := strings.Join([]string{io.Kind, ns, io.Name, e.Reason, e.Message}, "\x00")
		if g := groups[key]; g != nil {
			g.Count += count
			if at.After(g.Since) {
				g.Since = at
			}
			continue
		}
		kind := io.Kind
		if kind == "" {
			kind = "Object"
		}
		groups[key] = &Finding{Class: Events, Severity: Warning, Kind: clean(kind, maxReason),
			Namespace: clean(ns, maxName), Name: clean(io.Name, maxName),
			Reason: clean(e.Reason, maxReason), Message: clean(e.Message, maxMessage), Since: at, Count: count}
		order = append(order, key)
	}
	out := make([]Finding, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out
}
