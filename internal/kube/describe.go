package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Field is one line of a resource summary.
type Field struct {
	Key   string
	Value string
}

// YAML renders the object as YAML. managedFields is dropped: it is
// bookkeeping, and it buries the manifest.
func YAML(o *unstructured.Unstructured) string {
	c := o.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	b, err := yaml.Marshal(c.Object)
	if err != nil {
		return "# cannot render YAML: " + err.Error()
	}
	return string(b)
}

// Summarize extracts the key fields of any object: identity, age, owner,
// status, then whatever is specific to its kind (containers and restarts for
// a pod, replica counts for a workload, …), then its conditions.
func Summarize(o *unstructured.Unstructured, now time.Time) []Field {
	var f []Field
	add := func(k, v string) {
		if v != "" {
			f = append(f, Field{k, v})
		}
	}
	add("Kind", o.GetKind()+" ("+o.GetAPIVersion()+")")
	add("Name", o.GetName())
	add("Namespace", o.GetNamespace())
	if ts := o.GetCreationTimestamp(); !ts.IsZero() {
		add("Created", ts.Time.UTC().Format(time.RFC3339)+" ("+Age(ts.Time, now)+" ago)")
	}
	if dt := o.GetDeletionTimestamp(); dt != nil {
		add("Deleting", "since "+dt.Time.UTC().Format(time.RFC3339))
	}
	var owners []string
	for _, r := range o.GetOwnerReferences() {
		owners = append(owners, r.Kind+"/"+r.Name)
	}
	add("Owned by", strings.Join(owners, ", "))
	add("Status", StatusOf(o))
	add("Labels", joinMap(o.GetLabels(), 6))
	add("Annotations", annotationSummary(o.GetAnnotations()))

	switch o.GetKind() {
	case "Pod":
		f = append(f, podFields(o)...)
	case "Deployment", "StatefulSet", "ReplicaSet", "DaemonSet":
		f = append(f, workloadFields(o)...)
	case "Service":
		f = append(f, serviceFields(o)...)
	case "Node":
		f = append(f, nodeFields(o)...)
	}

	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		v := str(m["status"])
		if r := str(m["reason"]); r != "" {
			v += " (" + r + ")"
		}
		if msg := str(m["message"]); msg != "" {
			v += ": " + msg
		}
		add("Condition "+str(m["type"]), v)
	}
	return f
}

func podFields(o *unstructured.Unstructured) []Field {
	var f []Field
	add := func(k, v string) {
		if v != "" {
			f = append(f, Field{k, v})
		}
	}
	add("Node", nestedStr(o, "spec", "nodeName"))
	add("Pod IP", nestedStr(o, "status", "podIP"))
	add("QoS", nestedStr(o, "status", "qosClass"))
	if r := nestedStr(o, "status", "reason"); r != "" {
		add("Reason", r+" "+nestedStr(o, "status", "message"))
	}

	statuses := map[string]map[string]any{}
	for _, field := range []string{"initContainerStatuses", "containerStatuses", "ephemeralContainerStatuses"} {
		list, _, _ := unstructured.NestedSlice(o.Object, "status", field)
		for _, s := range list {
			if m, ok := s.(map[string]any); ok {
				statuses[str(m["name"])] = m
			}
		}
	}
	for _, field := range []string{"initContainers", "containers"} {
		list, _, _ := unstructured.NestedSlice(o.Object, "spec", field)
		for _, c := range list {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name := str(m["name"])
			label := "Container " + name
			if field == "initContainers" {
				label = "Init container " + name
			}
			v := "image=" + str(m["image"])
			if st, ok := statuses[name]; ok {
				v += fmt.Sprintf(", ready=%v, restarts=%v, state=%s", st["ready"], num(st["restartCount"]), containerState(st["state"]))
				if last := containerState(st["lastState"]); last != "" && last != "unknown" {
					v += ", last=" + last
				}
			} else {
				v += ", no status yet"
			}
			add(label, v)
		}
	}
	return f
}

// containerState renders {"waiting":{"reason":"CrashLoopBackOff",…}} as
// "waiting (CrashLoopBackOff)" and adds exit code/signal for terminated ones.
func containerState(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"running", "waiting", "terminated"} {
		d, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		s := k
		var parts []string
		if r := str(d["reason"]); r != "" {
			parts = append(parts, r)
		}
		if k == "terminated" {
			parts = append(parts, "exit "+num(d["exitCode"]))
			if sig := num(d["signal"]); sig != "" && sig != "0" {
				parts = append(parts, "signal "+sig)
			}
			if at := str(d["finishedAt"]); at != "" {
				parts = append(parts, "at "+at)
			}
		}
		if msg := str(d["message"]); msg != "" {
			parts = append(parts, msg)
		}
		if len(parts) > 0 {
			s += " (" + strings.Join(parts, ", ") + ")"
		}
		return s
	}
	return "unknown"
}

func workloadFields(o *unstructured.Unstructured) []Field {
	var f []Field
	get := func(path ...string) string {
		v, ok, _ := unstructured.NestedFieldNoCopy(o.Object, path...)
		if !ok {
			return "0"
		}
		return num(v)
	}
	if o.GetKind() == "DaemonSet" {
		f = append(f, Field{"Pods", fmt.Sprintf("desired=%s, ready=%s, up-to-date=%s, available=%s",
			get("status", "desiredNumberScheduled"), get("status", "numberReady"),
			get("status", "updatedNumberScheduled"), get("status", "numberAvailable"))})
	} else {
		f = append(f, Field{"Replicas", fmt.Sprintf("desired=%s, ready=%s, up-to-date=%s, available=%s",
			get("spec", "replicas"), get("status", "readyReplicas"),
			get("status", "updatedReplicas"), get("status", "availableReplicas"))})
	}
	if paused, _, _ := unstructured.NestedBool(o.Object, "spec", "paused"); paused {
		f = append(f, Field{"Paused", "true"})
	}
	if sel, ok, _ := unstructured.NestedStringMap(o.Object, "spec", "selector", "matchLabels"); ok {
		f = append(f, Field{"Selector", joinMap(sel, 0)})
	}
	list, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
	var images []string
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			images = append(images, str(m["name"])+"="+str(m["image"]))
		}
	}
	if len(images) > 0 {
		f = append(f, Field{"Images", strings.Join(images, ", ")})
	}
	return f
}

func serviceFields(o *unstructured.Unstructured) []Field {
	var f []Field
	f = append(f, Field{"Type", nestedStr(o, "spec", "type")})
	if ip := nestedStr(o, "spec", "clusterIP"); ip != "" {
		f = append(f, Field{"Cluster IP", ip})
	}
	ports, _, _ := unstructured.NestedSlice(o.Object, "spec", "ports")
	var ps []string
	for _, p := range ports {
		if m, ok := p.(map[string]any); ok {
			ps = append(ps, fmt.Sprintf("%s/%s→%s", num(m["port"]), str(m["protocol"]), num(m["targetPort"])))
		}
	}
	if len(ps) > 0 {
		f = append(f, Field{"Ports", strings.Join(ps, ", ")})
	}
	if sel, ok, _ := unstructured.NestedStringMap(o.Object, "spec", "selector"); ok {
		f = append(f, Field{"Selector", joinMap(sel, 0)})
	}
	return f
}

func nodeFields(o *unstructured.Unstructured) []Field {
	var f []Field
	if un, _, _ := unstructured.NestedBool(o.Object, "spec", "unschedulable"); un {
		f = append(f, Field{"Unschedulable", "true"})
	}
	if v := nestedStr(o, "status", "nodeInfo", "kubeletVersion"); v != "" {
		f = append(f, Field{"Kubelet", v})
	}
	if alloc, ok, _ := unstructured.NestedStringMap(o.Object, "status", "allocatable"); ok {
		f = append(f, Field{"Allocatable", joinMap(alloc, 0)})
	}
	return f
}

func nestedStr(o *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(o.Object, path...)
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int64:
		return fmt.Sprint(x)
	case float64:
		return fmt.Sprint(int64(x))
	default:
		return fmt.Sprint(x)
	}
}

// joinMap renders k=v pairs sorted by key; max > 0 truncates with a count.
func joinMap(m map[string]string, max int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for i, k := range keys {
		if max > 0 && i == max {
			parts = append(parts, fmt.Sprintf("… +%d more", len(keys)-max))
			break
		}
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// annotationSummary lists annotation keys only: values are often huge
// (last-applied-configuration) and are in the YAML anyway.
func annotationSummary(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 6 {
		return strings.Join(keys[:6], ", ") + fmt.Sprintf(", … +%d more", len(keys)-6)
	}
	return strings.Join(keys, ", ")
}
