package kube

import (
	"context"
	"io"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
)

// LogOptions selects which log lines to read.
type LogOptions struct {
	Container string
	Follow    bool
	Previous  bool  // the previous (crashed) instance of the container
	TailLines int64 // 0 = server default (everything)
	// LimitBytes bounds a non-follow read. 0 = unbounded.
	LimitBytes int64
}

// Logs opens a pod's log stream. The caller closes it.
func (c *Cluster) Logs(ctx context.Context, namespace, pod string, o LogOptions) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{Container: o.Container, Follow: o.Follow, Previous: o.Previous}
	if o.TailLines > 0 {
		opts.TailLines = &o.TailLines
	}
	if o.LimitBytes > 0 {
		opts.LimitBytes = &o.LimitBytes
	}
	return c.cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// Containers returns a pod's container names in the order a person would look
// at them: regular containers, then init, then ephemeral.
func Containers(pod *unstructured.Unstructured) []string {
	var out []string
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		list, _, _ := unstructured.NestedSlice(pod.Object, "spec", field)
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				if n, _ := m["name"].(string); n != "" {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// Event is one cluster event, flattened for display.
type Event struct {
	Time    time.Time
	Type    string // Normal | Warning
	Reason  string
	Object  string // "Pod/web-1"
	Message string
	Count   int32
}

// ObjectRef identifies the object events are about. Kind and Name may be
// empty to mean "everything in the namespace".
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// Events returns the events involving ref, oldest first (most recent last).
func (c *Cluster) Events(ctx context.Context, ref ObjectRef) ([]Event, error) {
	sel := fields.Set{}
	if ref.Name != "" {
		sel["involvedObject.name"] = ref.Name
	}
	if ref.Kind != "" {
		sel["involvedObject.kind"] = ref.Kind
	}
	if ref.UID != "" {
		sel["involvedObject.uid"] = ref.UID
	}
	l, err := c.cs.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: sel.AsSelector().String(),
		Limit:         1000,
	})
	if err != nil {
		return nil, err
	}
	var out []Event
	for i := range l.Items {
		e := &l.Items[i]
		io := e.InvolvedObject
		// Filter again client-side: the selector is an optimisation, this is
		// the guarantee.
		if (ref.Name != "" && io.Name != ref.Name) ||
			(ref.Kind != "" && io.Kind != ref.Kind) ||
			(ref.UID != "" && string(io.UID) != ref.UID) ||
			(ref.Namespace != "" && e.Namespace != ref.Namespace) {
			continue
		}
		count := e.Count
		if e.Series != nil && e.Series.Count > count {
			count = e.Series.Count
		}
		if count == 0 {
			count = 1
		}
		out = append(out, Event{
			Time: eventTime(e), Type: e.Type, Reason: e.Reason,
			Object: io.Kind + "/" + io.Name, Message: e.Message, Count: count,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

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
