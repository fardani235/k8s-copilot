package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

const (
	typeDesc = "Resource type as kubectl would name it: plural, singular, kind or short name, optionally group-qualified (pods, deploy, Deployment, deployments.apps, certificates.cert-manager.io)."
	nsDesc   = "Namespace. Leave out for cluster-scoped types."
)

func (r *Registry) registerRead() {
	r.add(&tool{
		Spec: Spec{
			Name:        "list_resources",
			Description: "List resources of any type the cluster serves (including custom resources) as a kubectl-style table. Read-only.",
			Tier:        Read,
			Schema: schema(map[string]string{
				"type":      strProp(typeDesc),
				"namespace": strProp("Namespace to list in. Leave out to list across all namespaces."),
				"limit":     intProp("Maximum rows (default 100, max 500)."),
			}, "type"),
		},
		read: r.listResources,
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "get_resource",
			Description: "Fetch one resource as YAML. Secret values are redacted. Read-only.",
			Tier:        Read,
			Schema: schema(map[string]string{
				"type": strProp(typeDesc), "namespace": strProp(nsDesc), "name": strProp("Resource name."),
			}, "type", "name"),
		},
		read: r.getResource,
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "describe_resource",
			Description: "Summarise one resource: key fields, container states and restart counts, conditions, and its recent events. The best first look at something that is misbehaving. Read-only.",
			Tier:        Read,
			Schema: schema(map[string]string{
				"type": strProp(typeDesc), "namespace": strProp(nsDesc), "name": strProp("Resource name."),
			}, "type", "name"),
		},
		read: r.describeResource,
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "get_logs",
			Description: "Read the most recent log lines of a pod's container. Use previous=true to read the last crashed instance of a restarting container. Read-only.",
			Tier:        Read,
			Schema: schema(map[string]string{
				"namespace":  strProp("Pod namespace."),
				"pod":        strProp("Pod name."),
				"container":  strProp("Container name. Leave out for the pod's default container."),
				"previous":   boolProp("Read the previous (terminated) instance instead of the current one."),
				"tail_lines": intProp("How many lines from the end (default 200, max 2000)."),
			}, "namespace", "pod"),
		},
		read: r.getLogs,
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "get_events",
			Description: "Read cluster events, oldest first. Give type and name for the events about one resource, or only a namespace for everything in it. Read-only.",
			Tier:        Read,
			Schema: schema(map[string]string{
				"namespace":     strProp("Namespace. Leave out for all namespaces."),
				"type":          strProp("Resource type of the object the events are about. " + typeDesc),
				"name":          strProp("Name of the object the events are about."),
				"warnings_only": boolProp("Only return Warning events."),
			}),
		},
		read: r.getEvents,
	})
}

// explain turns an API error into a tool result that cannot be mistaken for
// "nothing there": permission failures say they are permission failures.
func explain(err error, what string) error {
	switch {
	case kube.IsDenied(err):
		return fmt.Errorf("permission denied: the cluster refused to let the current user %s: %s. This is an authorization failure, not an absence of resources — tell the user", what, kube.Reason(err))
	case apierrors.IsNotFound(err):
		return fmt.Errorf("not found: cannot %s: %s", what, kube.Reason(err))
	default:
		return fmt.Errorf("cannot %s: %s", what, kube.Reason(err))
	}
}

func (r *Registry) cap(s string) string { return textutil.Truncate(s, r.opts.MaxResultBytes) }

type listArgs struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace"`
	Limit     int    `json:"limit"`
}

func (r *Registry) listResources(ctx context.Context, raw json.RawMessage) (string, error) {
	var a listArgs
	if err := decode("list_resources", raw, &a); err != nil {
		return "", err
	}
	if err := require("list_resources", "type", a.Type); err != nil {
		return "", err
	}
	switch {
	case a.Limit <= 0:
		a.Limit = 100
	case a.Limit > 500:
		a.Limit = 500
	}
	if a.Namespace == "*" {
		a.Namespace = kube.AllNamespaces
	}
	t, err := r.c.Resolve(ctx, a.Type)
	if err != nil {
		return "", err
	}
	tbl, err := r.c.List(ctx, t, a.Namespace, a.Limit)
	if err != nil {
		return "", explain(err, "list "+t.String())
	}
	scope := "cluster-scoped"
	if t.Namespaced {
		scope = "namespace " + a.Namespace
		if a.Namespace == kube.AllNamespaces {
			scope = "all namespaces"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %d found", t.String(), scope, len(tbl.Rows))
	if tbl.More {
		fmt.Fprintf(&b, " — truncated at the limit of %d, more exist", a.Limit)
	}
	b.WriteString("\n")
	if len(tbl.Rows) == 0 {
		b.WriteString("(none — the list request succeeded and returned no resources)\n")
		return b.String(), nil
	}
	b.WriteString(FormatTable(tbl))
	return r.cap(b.String()), nil
}

// FormatTable renders a table as aligned plain text.
func FormatTable(t *kube.Table) string {
	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = len(c)
	}
	for _, row := range t.Rows {
		for i, c := range row.Cells {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			if i == len(cells)-1 {
				b.WriteString(c)
			} else {
				fmt.Fprintf(&b, "%-*s  ", widths[i], c)
			}
		}
		b.WriteByte('\n')
	}
	line(t.Columns)
	for _, row := range t.Rows {
		line(row.Cells)
	}
	return b.String()
}

type objArgs struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r *Registry) fetch(ctx context.Context, toolName string, a objArgs) (kube.ResourceType, *unstructured.Unstructured, error) {
	if err := require(toolName, "type", a.Type, "name", a.Name); err != nil {
		return kube.ResourceType{}, nil, err
	}
	t, err := r.c.Resolve(ctx, a.Type)
	if err != nil {
		return t, nil, err
	}
	if t.Namespaced && a.Namespace == "" {
		return t, nil, &ArgError{Tool: toolName, Msg: fmt.Sprintf("%q is required because %s is a namespaced type", "namespace", t.String())}
	}
	o, err := r.c.Get(ctx, t, a.Namespace, a.Name)
	if err != nil {
		return t, nil, explain(err, fmt.Sprintf("get %s %s", t.String(), qualified(a.Namespace, a.Name)))
	}
	return t, o, nil
}

func qualified(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func (r *Registry) getResource(ctx context.Context, raw json.RawMessage) (string, error) {
	var a objArgs
	if err := decode("get_resource", raw, &a); err != nil {
		return "", err
	}
	_, o, err := r.fetch(ctx, "get_resource", a)
	if err != nil {
		return "", err
	}
	return r.cap(kube.YAML(r.redact(o))), nil
}

// redact returns a copy that is safe to send to a model provider: Secret
// values are replaced by their size, and the last-applied annotation — which
// repeats the whole manifest, values included — is dropped from Secrets.
func (r *Registry) redact(o *unstructured.Unstructured) *unstructured.Unstructured {
	if !r.opts.RedactSecrets || o.GetKind() != "Secret" || o.GroupVersionKind().Group != "" {
		return o
	}
	c := o.DeepCopy()
	for _, field := range []string{"data", "stringData"} {
		m, ok, _ := unstructured.NestedMap(c.Object, field)
		if !ok {
			continue
		}
		for k := range m {
			m[k] = "<redacted by k8s-copilot>"
		}
		_ = unstructured.SetNestedMap(c.Object, m, field)
	}
	ann := c.GetAnnotations()
	if _, ok := ann["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		ann["kubectl.kubernetes.io/last-applied-configuration"] = "<redacted by k8s-copilot>"
		c.SetAnnotations(ann)
	}
	return c
}

func (r *Registry) describeResource(ctx context.Context, raw json.RawMessage) (string, error) {
	var a objArgs
	if err := decode("describe_resource", raw, &a); err != nil {
		return "", err
	}
	_, o, err := r.fetch(ctx, "describe_resource", a)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range kube.Summarize(o, time.Now()) {
		fmt.Fprintf(&b, "%s: %s\n", f.Key, f.Value)
	}
	b.WriteString("\nEvents (oldest first):\n")
	evs, err := r.c.Events(ctx, kube.ObjectRef{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())})
	switch {
	case err != nil:
		fmt.Fprintf(&b, "(%v)\n", explain(err, "list events"))
	case len(evs) == 0:
		b.WriteString("(none recorded — events expire after about an hour)\n")
	default:
		b.WriteString(formatEvents(evs, false))
	}
	return r.cap(b.String()), nil
}

func formatEvents(evs []kube.Event, withObject bool) string {
	var b strings.Builder
	now := time.Now()
	for _, e := range evs {
		fmt.Fprintf(&b, "%s ago  %-7s  %s", kube.Age(e.Time, now), e.Type, e.Reason)
		if e.Count > 1 {
			fmt.Fprintf(&b, " (x%d)", e.Count)
		}
		if withObject {
			fmt.Fprintf(&b, "  %s", e.Object)
		}
		fmt.Fprintf(&b, "  %s\n", textutil.OneLine(e.Message))
	}
	return b.String()
}

type logArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	TailLines int64  `json:"tail_lines"`
}

func (r *Registry) getLogs(ctx context.Context, raw json.RawMessage) (string, error) {
	var a logArgs
	if err := decode("get_logs", raw, &a); err != nil {
		return "", err
	}
	if err := require("get_logs", "namespace", a.Namespace, "pod", a.Pod); err != nil {
		return "", err
	}
	switch {
	case a.TailLines <= 0:
		a.TailLines = 200
	case a.TailLines > 2000:
		a.TailLines = 2000
	}
	what := fmt.Sprintf("read logs of pod %s/%s", a.Namespace, a.Pod)
	rc, err := r.c.Logs(ctx, a.Namespace, a.Pod, kube.LogOptions{Container: a.Container, Previous: a.Previous, TailLines: a.TailLines})
	if err != nil {
		return "", explain(err, what)
	}
	defer rc.Close()
	// Keep the END of the output: the newest lines are the ones that explain
	// a crash. (The API's own limitBytes keeps the beginning, so it is not
	// used.) The read itself is bounded in case a line is enormous.
	data, err := readTail(io.LimitReader(rc, 32<<20), r.opts.MaxResultBytes*2)
	if err != nil {
		return "", explain(err, what)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		which := "current"
		if a.Previous {
			which = "previous"
		}
		return fmt.Sprintf("(no log output: the %s instance of this container has not written anything, or has not started)", which), nil
	}
	return textutil.TruncateHead(string(data), r.opts.MaxResultBytes), nil
}

// readTail reads r to the end and returns at most the last keep bytes.
func readTail(r io.Reader, keep int) ([]byte, error) {
	buf := make([]byte, 0, keep*2)
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > keep*2 {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	return buf, nil
}

type eventArgs struct {
	Namespace    string `json:"namespace"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	WarningsOnly bool   `json:"warnings_only"`
}

func (r *Registry) getEvents(ctx context.Context, raw json.RawMessage) (string, error) {
	var a eventArgs
	if err := decode("get_events", raw, &a); err != nil {
		return "", err
	}
	if (a.Type == "") != (a.Name == "") {
		return "", &ArgError{Tool: "get_events", Msg: `"type" and "name" must be given together`}
	}
	if a.Namespace == "*" {
		a.Namespace = kube.AllNamespaces
	}
	ref := kube.ObjectRef{Namespace: a.Namespace, Name: a.Name}
	if a.Type != "" {
		t, err := r.c.Resolve(ctx, a.Type)
		if err != nil {
			return "", err
		}
		ref.Kind = t.Kind
	}
	evs, err := r.c.Events(ctx, ref)
	if err != nil {
		return "", explain(err, "list events")
	}
	if a.WarningsOnly {
		kept := evs[:0]
		for _, e := range evs {
			if e.Type == "Warning" {
				kept = append(kept, e)
			}
		}
		evs = kept
	}
	if len(evs) == 0 {
		return "(no matching events — the request succeeded; events expire after about an hour)", nil
	}
	return textutil.TruncateHead(formatEvents(evs, a.Name == ""), r.opts.MaxResultBytes), nil
}
