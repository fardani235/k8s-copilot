// Package metrics is the single model of "how busy is it right now".
//
// The metrics screen and the copilot's get_metrics tool are both thin
// renderers of what this package produces: the same Snapshot, the same
// Listing (columns, cells, percentages, what counts as "hot"), the same
// sentences when a source is missing. That is what keeps the copilot's answer
// and the screen from contradicting each other — they cannot compute a number
// differently because neither computes numbers at all.
//
// The package does no I/O. internal/kube reads the four sources (node
// metrics, pod metrics, nodes, pods) and hands the raw objects to Build.
//
// One rule runs through everything here: unknown is not zero. A reading that
// could not be obtained is an Amount with OK false, it is drawn as "—", and it
// never takes part in arithmetic as if it were 0.
package metrics

import (
	"sort"
	"time"
)

// Amount is a quantity that may be unknown. CPU is in nanocores (the unit the
// metrics API reports in), memory in bytes. The zero value is "unknown",
// deliberately: a forgotten assignment shows up as "—" on the screen, not as
// a believable 0.
type Amount struct {
	V  int64
	OK bool
}

// Known is a measured or declared amount.
func Known(v int64) Amount { return Amount{V: v, OK: true} }

// Millicores is a known amount of CPU, given in millicores.
func Millicores(m int64) Amount { return Known(m * 1_000_000) }

// sum adds the known amounts. The result is unknown only if none was known.
func sum(a, b Amount) Amount {
	switch {
	case !a.OK:
		return b
	case !b.OK:
		return a
	}
	return Known(a.V + b.V)
}

// SourceState says how one of the four reads went.
type SourceState int

const (
	// OK: the cluster answered. An empty answer is still an answer.
	OK SourceState = iota
	// Absent: the API is not served at all (404) — no metrics-server or
	// equivalent is installed.
	Absent
	// Unavailable: the API is registered but nothing healthy is behind it
	// (503 and the like) — metrics-server is down, starting, or broken.
	Unavailable
	// Denied: the cluster knows who this is and refuses them (403).
	Denied
	// Unauthenticated: the cluster did not accept the credentials at all
	// (401) — an expired token or certificate, not a missing permission.
	Unauthenticated
	// TimedOut: no answer in time.
	TimedOut
	// Failed: anything else.
	Failed
)

// Source names one of the four reads.
type Source string

const (
	NodeMetrics Source = "node metrics"
	PodMetrics  Source = "pod metrics"
	NodeObjects Source = "nodes"
	PodObjects  Source = "pods"
)

// isMetricsAPI reports whether the source is served by metrics.k8s.io, as
// opposed to the core API.
func (s Source) isMetricsAPI() bool { return s == NodeMetrics || s == PodMetrics }

// SourceStatus is the outcome of one read.
type SourceStatus struct {
	Source Source
	State  SourceState
	// Scope is the namespace the read was limited to; empty for cluster-wide.
	Scope string
	// Reason is the server's (or the transport's) own words. It comes from
	// outside: sanitise it before drawing it.
	Reason string
}

// OK reports whether the source answered.
func (s SourceStatus) OK() bool { return s.State == OK }

// Sources is the outcome of all four reads behind a Snapshot.
type Sources struct {
	NodeMetrics SourceStatus
	PodMetrics  SourceStatus
	Nodes       SourceStatus
	Pods        SourceStatus
}

// Node is one node's load.
type Node struct {
	Name string
	// Ready is "Ready", "NotReady" or "" when the node object was not
	// readable.
	Ready string
	// CPU and Mem are usage. Unknown when the metrics API returned no sample
	// for this node.
	CPU, Mem Amount
	// CPUAlloc and MemAlloc are what the node can give to pods.
	CPUAlloc, MemAlloc Amount
	// Pods counts the node's pods that have not terminated. Known only in a
	// cluster-wide snapshot: in a namespace-scoped one it would be a partial
	// count presented as a total.
	Pods    Amount
	Sampled time.Time
	Window  time.Duration
}

// The roles a container can have besides being a regular one.
const (
	RoleSidecar   = "sidecar"   // a restartable init container
	RoleInit      = "init"      // an ordinary init container, still running
	RoleEphemeral = "ephemeral" // a debug container
)

// Container is one container's load and its declared bounds.
type Container struct {
	Name string
	// Role is empty for a regular container, else one of the Role constants.
	Role string
	// SpecKnown is false when the pod spec does not describe this container
	// (the spec was unreadable, or the metrics API reported a container the
	// spec does not have). Its requests and limits are then unknown, as
	// opposed to "none set".
	SpecKnown bool
	CPU, Mem  Amount
	// Requests and limits. Unknown either because none is set (SpecKnown is
	// then true) or because the spec is not known.
	CPUReq, CPULim Amount
	MemReq, MemLim Amount
	Restarts       int64
	// State is "running", or "waiting: Reason" / "terminated: Reason".
	State string
	// LastExit is why the previous instance ended ("OOMKilled", "Error", …).
	LastExit string
}

// Pod is one pod's load: the sum of its containers.
type Pod struct {
	Namespace string
	Name      string
	Node      string
	Phase     string
	// SpecKnown is false when the pod object could not be read (or the pod
	// appeared in the metrics API only). Its node, requests and limits are
	// then unknown.
	SpecKnown bool
	// BoundsKnown is false when the pod's requests and limits cannot be
	// stated: the spec is not known, or a running container is not in it.
	// "No limit" may only be said when this is true.
	BoundsKnown bool
	CPU, Mem    Amount
	// CPUReq / MemReq sum the running containers that declare a request.
	// CPULim / MemLim are known only if every running container declares
	// one: a pod with one unlimited container has no limit.
	CPUReq, CPULim Amount
	MemReq, MemLim Amount
	// Containers are the containers that run while the pod is up, plus any
	// other (init, ephemeral) the metrics API reports as running.
	Containers []Container
	Sampled    time.Time
	Window     time.Duration

	// dormant holds what the spec declares for init and ephemeral
	// containers, which are listed only when they are running.
	dormant map[string]Container
}

// Key identifies the pod.
func (p *Pod) Key() string { return p.Namespace + "/" + p.Name }

// Reporting says whether the metrics API returned a sample for the pod.
func (p *Pod) Reporting() bool { return p.CPU.OK || p.Mem.OK }

// Namespace is the load of the pods of one namespace.
type Namespace struct {
	Name string
	// Pods are the namespace's non-terminated pods; Running of those are in
	// phase Running; Reporting of those have a sample.
	Pods, Running, Reporting int
	// CPU and Mem sum the reporting pods. When Reporting < Running they are a
	// lower bound, and the listing says so.
	CPU, Mem Amount
	// CPUReq and MemReq sum the pods' requests; unknown if any pod's bounds
	// are unknown (BoundsKnown false), since a share of a partial sum would
	// be wrong.
	CPUReq, MemReq Amount
	BoundsKnown    bool
}

// Cluster is the load of the nodes together. It is computed from node
// metrics, not from pods, so it includes the kubelet and system daemons.
type Cluster struct {
	Nodes     int
	Reporting int
	CPU, Mem  Amount
	// CPUAlloc and MemAlloc sum the *reporting* nodes only, so that the
	// percentage compares like with like when a node has no sample. They are
	// unknown if any reporting node's allocatable is: usage of four nodes
	// over the capacity of three is not a percentage of anything.
	CPUAlloc, MemAlloc Amount
}

// Hold marks a Snapshot that is being shown because a newer read got no
// usage at all. It is never shown without saying so.
type Hold struct {
	// Cause is why the newest read has nothing.
	Cause SourceStatus
	// FailedAt is when that newest read was made.
	FailedAt time.Time
}

// Snapshot is everything known about load at one moment.
type Snapshot struct {
	// Scope is the namespace the pod reads were limited to; empty means
	// cluster-wide. Nodes are always cluster-wide.
	Scope   string
	TakenAt time.Time
	Sources Sources

	Nodes      []Node
	Pods       []Pod
	Namespaces []Namespace
	Cluster    Cluster

	// SampleTime is when the readings were sampled, by the cluster's clock;
	// zero if there were none. Nodes and pods are sampled separately: it is
	// the older of the two (each taken at its newest), so that one fresh
	// source cannot vouch for a stale one. Window is its averaging window.
	SampleTime time.Time
	Window     time.Duration

	// Truncated lists sources that had more items than were read.
	Truncated []Source

	// Held is set when this is an earlier snapshot standing in for a failed
	// refresh.
	Held *Hold
}

// HasUsage reports whether the snapshot carries any usage readings at all.
func (s *Snapshot) HasUsage() bool {
	return s.Sources.NodeMetrics.OK() || s.Sources.PodMetrics.OK()
}

// UsageFailure returns the status that best explains a snapshot without
// usage: the node-metrics failure, or the pod-metrics one.
func (s *Snapshot) UsageFailure() SourceStatus {
	if !s.Sources.PodMetrics.OK() {
		return s.Sources.PodMetrics
	}
	return s.Sources.NodeMetrics
}

// PodsDeniedClusterWide reports that a cluster-wide snapshot could not read
// the pods because this user may not — the case in which a narrower,
// namespace-scoped read can still succeed.
func (s *Snapshot) PodsDeniedClusterWide() (SourceStatus, bool) {
	if s.Scope != "" {
		return SourceStatus{}, false
	}
	for _, st := range []SourceStatus{s.Sources.PodMetrics, s.Sources.Pods} {
		if st.State == Denied {
			return st, true
		}
	}
	return SourceStatus{}, false
}

// Narrow returns the part of a cluster-wide snapshot that concerns one
// namespace, with the same readings. Per-node pod counts become unknown, as
// they are in any namespace-scoped snapshot.
func (s *Snapshot) Narrow(namespace string) *Snapshot {
	if namespace == "" || s.Scope == namespace {
		return s
	}
	c := *s
	c.Scope = namespace
	c.Pods = nil
	for _, p := range s.Pods {
		if p.Namespace == namespace {
			c.Pods = append(c.Pods, p)
		}
	}
	c.Nodes = append([]Node(nil), s.Nodes...)
	c.Sources.PodMetrics.Scope, c.Sources.Pods.Scope = namespace, namespace
	c.derive()
	return &c
}

// WithPodsOf returns s with its pods replaced by those of a namespace-scoped
// read: the node readings of s, the pod readings of pods. This is how someone
// who may not read pods cluster-wide is still shown the same node readings as
// everyone else, beside the pods of the namespace they may read.
func (s *Snapshot) WithPodsOf(pods *Snapshot) *Snapshot {
	c := *s
	c.Scope = pods.Scope
	c.Pods = append([]Pod(nil), pods.Pods...)
	c.Nodes = append([]Node(nil), s.Nodes...)
	c.Sources.PodMetrics, c.Sources.Pods = pods.Sources.PodMetrics, pods.Sources.Pods
	c.Truncated = append(append([]Source(nil), s.Truncated...), pods.Truncated...)
	if pods.TakenAt.Before(c.TakenAt) {
		c.TakenAt = pods.TakenAt
	}
	if c.Held == nil {
		c.Held = pods.Held
	}
	c.derive()
	return &c
}

// HeldBy returns a copy of s marked as standing in for a failed refresh.
func (s *Snapshot) HeldBy(failed *Snapshot) *Snapshot {
	c := *s
	c.Held = &Hold{Cause: failed.UsageFailure(), FailedAt: failed.TakenAt}
	return &c
}

// derive computes everything that follows from Nodes and Pods: per-node pod
// counts, namespaces, cluster totals, the sample time.
func (s *Snapshot) derive() {
	// Pods per node — only when every pod was read.
	perNode := map[string]int64{}
	countable := s.Scope == "" && s.Sources.Pods.OK()
	for i := range s.Pods {
		if s.Pods[i].Node != "" {
			perNode[s.Pods[i].Node]++
		}
	}
	var nodeSample, podSample sample
	s.Cluster = Cluster{Nodes: len(s.Nodes)}
	cpuComparable, memComparable := true, true
	for i := range s.Nodes {
		n := &s.Nodes[i]
		n.Pods = Amount{}
		if countable {
			n.Pods = Known(perNode[n.Name])
		}
		if n.CPU.OK || n.Mem.OK {
			s.Cluster.Reporting++
			s.Cluster.CPU = sum(s.Cluster.CPU, n.CPU)
			s.Cluster.Mem = sum(s.Cluster.Mem, n.Mem)
			s.Cluster.CPUAlloc = sum(s.Cluster.CPUAlloc, n.CPUAlloc)
			s.Cluster.MemAlloc = sum(s.Cluster.MemAlloc, n.MemAlloc)
			cpuComparable = cpuComparable && (!n.CPU.OK || n.CPUAlloc.OK)
			memComparable = memComparable && (!n.Mem.OK || n.MemAlloc.OK)
		}
		nodeSample.note(n.Sampled, n.Window)
	}
	if !cpuComparable {
		s.Cluster.CPUAlloc = Amount{}
	}
	if !memComparable {
		s.Cluster.MemAlloc = Amount{}
	}

	byNS := map[string]*Namespace{}
	for i := range s.Pods {
		p := &s.Pods[i]
		ns := byNS[p.Namespace]
		if ns == nil {
			ns = &Namespace{Name: p.Namespace, BoundsKnown: true}
			byNS[p.Namespace] = ns
		}
		ns.Pods++
		if p.Phase == "Running" {
			ns.Running++
		}
		if p.Reporting() {
			ns.Reporting++
			ns.CPU = sum(ns.CPU, p.CPU)
			ns.Mem = sum(ns.Mem, p.Mem)
		}
		ns.CPUReq = sum(ns.CPUReq, p.CPUReq)
		ns.MemReq = sum(ns.MemReq, p.MemReq)
		ns.BoundsKnown = ns.BoundsKnown && p.BoundsKnown
		podSample.note(p.Sampled, p.Window)
	}
	s.Namespaces = s.Namespaces[:0:0]
	for _, ns := range byNS {
		if !ns.BoundsKnown {
			ns.CPUReq, ns.MemReq = Amount{}, Amount{}
		}
		s.Namespaces = append(s.Namespaces, *ns)
	}
	sort.Slice(s.Namespaces, func(i, j int) bool { return s.Namespaces[i].Name < s.Namespaces[j].Name })

	// The older of the two sources' newest samples.
	at := nodeSample
	if at.at.IsZero() || (!podSample.at.IsZero() && podSample.at.Before(at.at)) {
		at = podSample
	}
	s.SampleTime, s.Window = at.at, at.window
}

// sample tracks the newest sample of one source.
type sample struct {
	at     time.Time
	window time.Duration
}

func (n *sample) note(t time.Time, window time.Duration) {
	if !t.IsZero() && t.After(n.at) {
		n.at, n.window = t, window
	}
}

// Pod finds a pod by namespace and name.
func (s *Snapshot) Pod(namespace, name string) (*Pod, bool) {
	for i := range s.Pods {
		if s.Pods[i].Namespace == namespace && s.Pods[i].Name == name {
			return &s.Pods[i], true
		}
	}
	return nil, false
}

// Node finds a node by name.
func (s *Snapshot) Node(name string) (*Node, bool) {
	for i := range s.Nodes {
		if s.Nodes[i].Name == name {
			return &s.Nodes[i], true
		}
	}
	return nil, false
}

// podTotals sums a set of pods.
type podTotals struct {
	Pods, Running, Reporting int
	CPU, Mem                 Amount
	CPUReq, MemReq           Amount
	BoundsKnown              bool
}

func totalsOf(pods []*Pod) podTotals {
	t := podTotals{BoundsKnown: true}
	for _, p := range pods {
		t.BoundsKnown = t.BoundsKnown && p.BoundsKnown
		t.Pods++
		if p.Phase == "Running" {
			t.Running++
		}
		if p.Reporting() {
			t.Reporting++
			t.CPU = sum(t.CPU, p.CPU)
			t.Mem = sum(t.Mem, p.Mem)
		}
		t.CPUReq = sum(t.CPUReq, p.CPUReq)
		t.MemReq = sum(t.MemReq, p.MemReq)
	}
	if !t.BoundsKnown {
		t.CPUReq, t.MemReq = Amount{}, Amount{}
	}
	return t
}
