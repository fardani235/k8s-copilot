package metrics

import (
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Inputs are the raw results of the four reads. Items of a source whose
// status is not OK are ignored.
type Inputs struct {
	Scope   string
	TakenAt time.Time
	Sources Sources

	// NodeMetrics and PodMetrics are metrics.k8s.io NodeMetrics / PodMetrics
	// objects; Nodes and Pods are core/v1 objects.
	NodeMetrics []unstructured.Unstructured
	PodMetrics  []unstructured.Unstructured
	Nodes       []unstructured.Unstructured
	Pods        []unstructured.Unstructured

	Truncated []Source
}

// Build joins usage (from the metrics API) with capacity, requests and
// limits (from the node and pod objects) into a Snapshot.
//
// Either side of a join may be missing. A node or pod that only one source
// knows about is still listed, with the other half unknown.
func Build(in Inputs) *Snapshot {
	s := &Snapshot{Scope: in.Scope, TakenAt: in.TakenAt, Sources: in.Sources, Truncated: in.Truncated}

	// --- nodes ---
	nodes := map[string]*Node{}
	var nodeOrder []string
	node := func(name string) *Node {
		n := nodes[name]
		if n == nil {
			n = &Node{Name: name}
			nodes[name] = n
			nodeOrder = append(nodeOrder, name)
		}
		return n
	}
	if in.Sources.Nodes.OK() {
		for i := range in.Nodes {
			o := &in.Nodes[i]
			n := node(o.GetName())
			n.CPUAlloc = cpuAt(o.Object, "status", "allocatable", "cpu")
			n.MemAlloc = memAt(o.Object, "status", "allocatable", "memory")
			n.Ready = nodeReady(o)
		}
	}
	if in.Sources.NodeMetrics.OK() {
		for i := range in.NodeMetrics {
			o := &in.NodeMetrics[i]
			n := node(o.GetName())
			n.CPU = cpuAt(o.Object, "usage", "cpu")
			n.Mem = memAt(o.Object, "usage", "memory")
			n.Sampled, n.Window = sampleOf(o)
		}
	}
	sort.Strings(nodeOrder)
	for _, name := range nodeOrder {
		s.Nodes = append(s.Nodes, *nodes[name])
	}

	// --- pods ---
	pods := map[string]*Pod{}
	finished := map[string]bool{}
	var podOrder []string
	if in.Sources.Pods.OK() {
		for i := range in.Pods {
			o := &in.Pods[i]
			phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
			if phase == "Succeeded" || phase == "Failed" {
				// Finished: it uses nothing and reserves nothing.
				finished[o.GetNamespace()+"/"+o.GetName()] = true
				continue
			}
			p := podFromSpec(o, phase)
			pods[p.Key()] = p
			podOrder = append(podOrder, p.Key())
		}
	}
	if in.Sources.PodMetrics.OK() {
		for i := range in.PodMetrics {
			o := &in.PodMetrics[i]
			key := o.GetNamespace() + "/" + o.GetName()
			p := pods[key]
			if p == nil {
				if finished[key] {
					continue
				}
				// Known to the metrics API only: list it, bounds unknown.
				p = &Pod{Namespace: o.GetNamespace(), Name: o.GetName()}
				pods[key] = p
				podOrder = append(podOrder, key)
			}
			p.addUsage(o)
		}
	}
	sort.Strings(podOrder)
	for _, key := range podOrder {
		p := pods[key]
		p.settleBounds()
		s.Pods = append(s.Pods, *p)
	}

	s.derive()
	return s
}

// podFromSpec reads what a pod object declares: node, phase, containers with
// their requests and limits, restart counts.
//
// The containers that run while the pod is up — the regular ones and
// restartable init containers (sidecars) — are listed. Ordinary init
// containers and ephemeral ones are remembered but listed only if the metrics
// API reports them: an init container still running is the pod's whole
// usage, and must be shown against its own limit.
func podFromSpec(o *unstructured.Unstructured, phase string) *Pod {
	p := &Pod{Namespace: o.GetNamespace(), Name: o.GetName(), Phase: phase, SpecKnown: true, dormant: map[string]Container{}}
	p.Node, _, _ = unstructured.NestedString(o.Object, "spec", "nodeName")

	statuses := map[string]map[string]any{}
	for _, field := range []string{"initContainerStatuses", "containerStatuses", "ephemeralContainerStatuses"} {
		list, _, _ := unstructured.NestedSlice(o.Object, "status", field)
		for _, st := range list {
			if m, ok := st.(map[string]any); ok {
				statuses[str(m["name"])] = m
			}
		}
	}

	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		list, _, _ := unstructured.NestedSlice(o.Object, "spec", field)
		for _, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			c := Container{Name: str(m["name"]), SpecKnown: true}
			c.CPUReq = cpuAt(m, "resources", "requests", "cpu")
			c.MemReq = memAt(m, "resources", "requests", "memory")
			c.CPULim = cpuAt(m, "resources", "limits", "cpu")
			c.MemLim = memAt(m, "resources", "limits", "memory")
			if st := statuses[c.Name]; st != nil {
				c.Restarts = int64Of(st["restartCount"])
				c.State = containerState(st["state"])
				if last, ok := st["lastState"].(map[string]any); ok {
					if term, ok := last["terminated"].(map[string]any); ok {
						c.LastExit = str(term["reason"])
					}
				}
			}
			switch {
			case field == "containers":
				p.Containers = append(p.Containers, c)
			case field == "initContainers" && str(m["restartPolicy"]) == "Always":
				c.Role = RoleSidecar
				p.Containers = append(p.Containers, c)
			case field == "initContainers":
				c.Role = RoleInit
				p.dormant[c.Name] = c
			default:
				c.Role = RoleEphemeral
				p.dormant[c.Name] = c
			}
		}
	}
	return p
}

// addUsage adds a PodMetrics object's readings to the pod.
func (p *Pod) addUsage(o *unstructured.Unstructured) {
	p.Sampled, p.Window = sampleOf(o)
	list, _, _ := unstructured.NestedSlice(o.Object, "containers")
	// No containers in the sample means nothing was measured. The pod stays
	// without a reading: that is not the same as a reading of zero.
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := str(m["name"])
		cpu, mem := cpuAt(m, "usage", "cpu"), memAt(m, "usage", "memory")
		found := false
		for i := range p.Containers {
			if p.Containers[i].Name == name {
				p.Containers[i].CPU, p.Containers[i].Mem = cpu, mem
				found = true
			}
		}
		if !found {
			// An init or ephemeral container that is running, with the
			// bounds it declares — or one the spec does not describe at all,
			// whose bounds are then unknown rather than "none".
			c, declared := p.dormant[name]
			if !declared {
				c = Container{Name: name}
			}
			c.CPU, c.Mem = cpu, mem
			p.Containers = append(p.Containers, c)
		}
		p.CPU = sum(p.CPU, cpu)
		p.Mem = sum(p.Mem, mem)
	}
}

// settleBounds works out what the pod as a whole may use, from the
// containers that are running.
//
// While an ordinary init container runs, the pod is that container (and any
// sidecars already started): the regular containers have not begun, and
// measuring the init container against their limits would be meaningless.
// Otherwise it is everything listed.
//
// A request is the sum of the requests. A limit exists only if every running
// container has one: a single unlimited container makes the pod unlimited.
// If any running container is not described by the spec, nothing about the
// pod's bounds can be said, and they are unknown — not "none".
func (p *Pod) settleBounds() {
	initialising := false
	for _, c := range p.Containers {
		if c.Role == RoleInit && (c.CPU.OK || c.Mem.OK) {
			initialising = true
		}
	}
	p.CPUReq, p.MemReq, p.CPULim, p.MemLim = Amount{}, Amount{}, Amount{}, Amount{}
	p.BoundsKnown = p.SpecKnown
	cpuLimited, memLimited, any := true, true, false
	for _, c := range p.Containers {
		if initialising && !c.CPU.OK && !c.Mem.OK {
			continue // not started yet
		}
		any = true
		p.BoundsKnown = p.BoundsKnown && c.SpecKnown
		p.CPUReq = sum(p.CPUReq, c.CPUReq)
		p.MemReq = sum(p.MemReq, c.MemReq)
		p.CPULim = sum(p.CPULim, c.CPULim)
		p.MemLim = sum(p.MemLim, c.MemLim)
		cpuLimited = cpuLimited && c.CPULim.OK
		memLimited = memLimited && c.MemLim.OK
	}
	if !cpuLimited || !any {
		p.CPULim = Amount{}
	}
	if !memLimited || !any {
		p.MemLim = Amount{}
	}
	if !p.BoundsKnown {
		p.CPUReq, p.MemReq, p.CPULim, p.MemLim = Amount{}, Amount{}, Amount{}, Amount{}
	}
}

func nodeReady(o *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			if m["status"] == "True" {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return ""
}

func containerState(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"running", "waiting", "terminated"} {
		inner, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		if k == "running" {
			return "running"
		}
		if r := str(inner["reason"]); r != "" {
			return k + ": " + r
		}
		return k
	}
	return ""
}

// sampleOf reads the timestamp and window the metrics API attaches to a
// reading.
func sampleOf(o *unstructured.Unstructured) (time.Time, time.Duration) {
	var at time.Time
	if ts, _, _ := unstructured.NestedString(o.Object, "timestamp"); ts != "" {
		at, _ = time.Parse(time.RFC3339, ts)
	}
	var window time.Duration
	if w, _, _ := unstructured.NestedString(o.Object, "window"); w != "" {
		window, _ = time.ParseDuration(w)
	}
	return at, window
}

// Quantities beyond these are not readings of anything: they are refused
// rather than allowed to overflow into a believable number.
const (
	maxCores = 1e9  // a billion cores
	maxBytes = 1e18 // an exabyte
)

// cpuAt reads a CPU quantity ("250m", "2", "156340215n") as nanocores.
//
// Nanocores, not millicores: the metrics API reports in nanocores, an idle
// container uses a fraction of a millicore, and rounding each one up before
// adding them turns three hundred idle containers into 300m.
func cpuAt(obj map[string]any, path ...string) Amount {
	q, ok := quantityAt(obj, path...)
	if !ok {
		return Amount{}
	}
	if f := q.AsApproximateFloat64(); f < 0 || f > maxCores {
		return Amount{}
	}
	return Known(q.ScaledValue(resource.Nano))
}

// memAt reads a memory quantity ("512Mi", "1362696Ki") as bytes.
func memAt(obj map[string]any, path ...string) Amount {
	q, ok := quantityAt(obj, path...)
	if !ok {
		return Amount{}
	}
	if f := q.AsApproximateFloat64(); f < 0 || f > maxBytes {
		return Amount{}
	}
	return Known(q.Value())
}

func quantityAt(obj map[string]any, path ...string) (resource.Quantity, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	if !found || err != nil || v == nil {
		return resource.Quantity{}, false
	}
	s, isString := v.(string)
	if !isString {
		s = fmt.Sprint(v) // a bare number in hand-written YAML
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, false
	}
	return q, true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func int64Of(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}
