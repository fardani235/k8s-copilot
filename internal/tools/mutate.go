package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// RestartAnnotation is the pod-template annotation `kubectl rollout restart`
// sets; using the same key keeps kubectl's rollout tooling consistent.
const RestartAnnotation = "kubectl.kubernetes.io/restartedAt"

// Plan is one fully specified change: which object, the exact patch body, the
// state it was computed against. Building a Plan sends nothing. The only
// method that changes the cluster is Apply, and Apply demands an
// approval.Grant for this plan's digest.
type Plan struct {
	ID        string
	Tool      string
	Title     string
	Type      kube.ResourceType
	Namespace string
	Name      string
	// Args are the validated arguments, re-encoded (what the human may edit).
	Args   json.RawMessage
	Reason string

	Changes       []audit.Change
	Reversible    bool
	Reversibility string
	Warnings      []string

	c           *kube.Cluster
	body        []byte
	subresource string
	// recheck re-reads the object and fails if the "before" state the human
	// was shown no longer holds.
	recheck func(ctx context.Context) error
	// describe summarises the object returned by the server after applying.
	describe func(*unstructured.Unstructured) string

	dryRunOK bool
}

// Target identifies the object for the dialog and the audit entry.
func (p *Plan) Target() audit.Target {
	return audit.Target{
		Context: p.c.Info.Context, Server: p.c.Info.Server,
		APIVersion: p.Type.APIVersion(), Kind: p.Type.Kind,
		Namespace: p.Namespace, Name: p.Name,
	}
}

// Request is the exact API call, for the dialog's detail view and the audit
// entry.
func (p *Plan) Request() string {
	path := "/api/" + p.Type.Version
	if p.Type.Group != "" {
		path = "/apis/" + p.Type.Group + "/" + p.Type.Version
	}
	if p.Type.Namespaced {
		path += "/namespaces/" + p.Namespace
	}
	path += "/" + p.Type.Resource + "/" + p.Name
	if p.subresource != "" {
		path += "/" + p.subresource
	}
	return "PATCH " + path + "  (application/merge-patch+json)\n" + string(p.body)
}

// Digest fingerprints the request. An approval covers exactly one digest.
func (p *Plan) Digest() string {
	sum := sha256.Sum256([]byte(p.ID + "\n" + p.c.Info.Server + "\n" + p.Request()))
	return hex.EncodeToString(sum[:])
}

// DryRun sends the change with dryRun=All: the API server runs admission and
// validation and persists nothing. An error is the server's rejection.
func (p *Plan) DryRun(ctx context.Context) error {
	_, err := p.c.MergePatch(ctx, p.Type, p.Namespace, p.Name, p.body, true, p.subresource)
	p.dryRunOK = err == nil
	return err
}

// Proposal is the human-facing view of the plan. It exists only for plans
// whose dry-run passed.
func (p *Plan) Proposal(intent string) (approval.Proposal, error) {
	if !p.dryRunOK {
		return approval.Proposal{}, errors.New("internal error: proposal requested for a plan that has not passed dry-run")
	}
	return approval.Proposal{
		ID: p.ID, Tool: p.Tool, Title: p.Title, Target: p.Target(),
		Changes: p.Changes, Args: p.Args, Request: p.Request(), Digest: p.Digest(),
		DryRun:     "passed — the API server accepted this exact request with dryRun=All",
		Reversible: p.Reversible, Reversibility: p.Reversibility, Warnings: p.Warnings,
		Intent: intent, ModelReason: p.Reason,
	}, nil
}

// ErrNotApproved is returned by Apply without a matching grant.
var ErrNotApproved = errors.New("refusing to apply: no human approval for this exact change")

// StaleError means the object changed between proposal and approval, so what
// the human approved is no longer what would happen.
type StaleError struct{ Msg string }

func (e *StaleError) Error() string { return e.Msg }

// Apply sends the change for real. It requires a grant issued by the approval
// gate for this plan, and re-checks that the object still looks the way it
// did when the human was shown the before→after.
func (p *Plan) Apply(ctx context.Context, grant *approval.Grant) (string, error) {
	if !p.dryRunOK || !grant.Covers(p.Digest()) {
		return "", ErrNotApproved
	}
	if err := p.recheck(ctx); err != nil {
		return "", err
	}
	o, err := p.c.MergePatch(ctx, p.Type, p.Namespace, p.Name, p.body, false, p.subresource)
	if err != nil {
		return "", err
	}
	return p.describe(o), nil
}

// sameObject fails when the name now belongs to a different object than the
// one the proposal was computed from (deleted and recreated meanwhile).
func sameObject(planned, now *unstructured.Unstructured) error {
	if planned.GetUID() != now.GetUID() {
		return &StaleError{Msg: fmt.Sprintf("not applied: %s %s was deleted and recreated since this was approved; it is a different object now", now.GetKind(), qualified(now.GetNamespace(), now.GetName()))}
	}
	return nil
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("p%d", time.Now().UnixNano())
	}
	return "p" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------

func (r *Registry) registerMutate() {
	const gated = " Nothing happens when you call this: the change is validated with a server-side dry-run and then shown to the user, who approves or declines it. The result tells you which."
	reason := strProp("One or two plain sentences for the user: why this change, based on what you found.")

	r.add(&tool{
		Spec: Spec{
			Name:        "scale",
			Description: "Propose changing the replica count of a Deployment, StatefulSet or ReplicaSet." + gated,
			Tier:        Mutate,
			Schema: schema(map[string]string{
				"type":      strProp("deployment, statefulset or replicaset."),
				"namespace": strProp("Namespace of the workload."),
				"name":      strProp("Name of the workload."),
				"replicas":  intProp("Desired replica count (0 or more)."),
				"reason":    reason,
			}, "type", "namespace", "name", "replicas", "reason"),
		},
		plan: r.planScale,
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "rollout_restart",
			Description: "Propose a rolling restart of a Deployment, StatefulSet or DaemonSet (the same thing `kubectl rollout restart` does: pods are replaced according to the workload's update strategy; nothing else changes)." + gated,
			Tier:        Mutate,
			Schema: schema(map[string]string{
				"type":      strProp("deployment, statefulset or daemonset."),
				"namespace": strProp("Namespace of the workload."),
				"name":      strProp("Name of the workload."),
				"reason":    reason,
			}, "type", "namespace", "name", "reason"),
		},
		plan: r.planRestart,
	})
	metaProps := func(what string) map[string]string {
		return map[string]string{
			"type":      strProp(typeDesc),
			"namespace": strProp(nsDesc),
			"name":      strProp("Resource name."),
			"set":       fmt.Sprintf(`{"type":"object","additionalProperties":{"type":"string"},"description":"%s to add or overwrite, as key: value."}`, what),
			"remove":    fmt.Sprintf(`{"type":"array","items":{"type":"string"},"description":"%s keys to remove."}`, what),
			"reason":    reason,
		}
	}
	r.add(&tool{
		Spec: Spec{
			Name:        "set_labels",
			Description: "Propose adding, changing or removing labels in a resource's own metadata (not its pod template)." + gated,
			Tier:        Mutate,
			Schema:      schema(metaProps("Labels"), "type", "name", "reason"),
		},
		plan: func(ctx context.Context, raw json.RawMessage) (*Plan, error) {
			return r.planMetadata(ctx, "set_labels", "labels", raw)
		},
	})
	r.add(&tool{
		Spec: Spec{
			Name:        "set_annotations",
			Description: "Propose adding, changing or removing annotations in a resource's own metadata (not its pod template)." + gated,
			Tier:        Mutate,
			Schema:      schema(metaProps("Annotations"), "type", "name", "reason"),
		},
		plan: func(ctx context.Context, raw json.RawMessage) (*Plan, error) {
			return r.planMetadata(ctx, "set_annotations", "annotations", raw)
		},
	})
}

// workload resolves and fetches a workload, insisting its type is one of the
// allowed apps/v1 resources. The allow-list is on the resolved
// group/resource, so a look-alike custom resource cannot slip through.
func (r *Registry) workload(ctx context.Context, toolName, typ, ns, name string, allowed ...string) (kube.ResourceType, *unstructured.Unstructured, error) {
	t, o, err := r.fetch(ctx, toolName, objArgs{Type: typ, Namespace: ns, Name: name})
	if err != nil {
		return t, nil, err
	}
	ok := false
	for _, a := range allowed {
		if t.Group == "apps" && t.Resource == a {
			ok = true
		}
	}
	if !ok {
		return t, nil, fmt.Errorf("refused: %s only works on %s (apps/v1), not on %s; nothing was proposed", toolName, strings.Join(allowed, ", "), t.String())
	}
	if err := r.guardNamespace(ns); err != nil {
		return t, nil, err
	}
	return t, o, nil
}

func (r *Registry) guardNamespace(ns string) error {
	for _, p := range r.opts.ProtectedNamespaces {
		if p == ns && ns != "" {
			return fmt.Errorf("refused: namespace %q is protected by configuration; k8s-copilot proposes no changes there", ns)
		}
	}
	return nil
}

func systemNamespaceWarning(ns string) []string {
	if strings.HasPrefix(ns, "kube-") {
		return []string{fmt.Sprintf("%s is a system namespace: changes here can affect the whole cluster.", ns)}
	}
	return nil
}

type scaleArgs struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Replicas  *int64 `json:"replicas"`
	Reason    string `json:"reason"`
}

func (r *Registry) planScale(ctx context.Context, raw json.RawMessage) (*Plan, error) {
	const tn = "scale"
	var a scaleArgs
	if err := decode(tn, raw, &a); err != nil {
		return nil, err
	}
	if err := require(tn, "namespace", a.Namespace, "reason", a.Reason); err != nil {
		return nil, err
	}
	if a.Replicas == nil {
		return nil, &ArgError{Tool: tn, Msg: `"replicas" is required`}
	}
	want := *a.Replicas
	if want < 0 {
		return nil, &ArgError{Tool: tn, Msg: `"replicas" cannot be negative`}
	}
	if want > int64(r.opts.MaxReplicas) {
		return nil, fmt.Errorf("refused: %d replicas is above the configured maximum of %d that k8s-copilot will propose; nothing was proposed", want, r.opts.MaxReplicas)
	}
	t, o, err := r.workload(ctx, tn, a.Type, a.Namespace, a.Name, "deployments", "statefulsets", "replicasets")
	if err != nil {
		return nil, err
	}
	current := replicasOf(o)
	if current == want {
		return nil, fmt.Errorf("%s %s/%s already has %d replicas; there is nothing to change", t.Kind, a.Namespace, a.Name, want)
	}

	body, _ := json.Marshal(map[string]any{"spec": map[string]any{"replicas": want}})
	args, _ := json.Marshal(a)
	p := &Plan{
		ID: newID(), Tool: tn, c: r.c, Type: t, Namespace: a.Namespace, Name: a.Name,
		Args: args, Reason: a.Reason,
		Title:         fmt.Sprintf("Scale %s %s/%s from %d to %d replicas", t.Kind, a.Namespace, a.Name, current, want),
		Changes:       []audit.Change{{Field: "spec.replicas", Before: fmt.Sprint(current), After: fmt.Sprint(want)}},
		Reversible:    true,
		Reversibility: fmt.Sprintf("Reversible: scale back to %d. Pods removed by scaling down are gone; new ones are created from the same template.", current),
		Warnings:      systemNamespaceWarning(a.Namespace),
		body:          body,
		subresource:   "scale",
	}
	if want == 0 {
		p.Warnings = append(p.Warnings, "Scaling to 0 stops every pod of this workload: it will serve nothing until scaled up again.")
	}
	for _, ref := range o.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			p.Warnings = append(p.Warnings, fmt.Sprintf("This %s is controlled by %s/%s, which will probably set the replica count back.", t.Kind, ref.Kind, ref.Name))
		}
	}
	p.recheck = func(ctx context.Context) error {
		now, err := r.c.Get(ctx, t, a.Namespace, a.Name)
		if err != nil {
			return err
		}
		if err := sameObject(o, now); err != nil {
			return err
		}
		if got := replicasOf(now); got != current {
			return &StaleError{Msg: fmt.Sprintf("not applied: %s %s/%s now has %d replicas, not the %d shown when this was approved. Someone or something else changed it; look again before proposing", t.Kind, a.Namespace, a.Name, got, current)}
		}
		return nil
	}
	p.describe = func(*unstructured.Unstructured) string {
		return fmt.Sprintf("applied: %s %s/%s spec.replicas is now %d (was %d). The controller will add or remove pods to match; check readiness before reporting success.", t.Kind, a.Namespace, a.Name, want, current)
	}
	return p, nil
}

// replicasOf reads spec.replicas; the API defaults it to 1 when unset.
func replicasOf(o *unstructured.Unstructured) int64 {
	n, ok, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
	if !ok {
		return 1
	}
	return n
}

type restartArgs struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

func (r *Registry) planRestart(ctx context.Context, raw json.RawMessage) (*Plan, error) {
	const tn = "rollout_restart"
	var a restartArgs
	if err := decode(tn, raw, &a); err != nil {
		return nil, err
	}
	if err := require(tn, "namespace", a.Namespace, "reason", a.Reason); err != nil {
		return nil, err
	}
	t, o, err := r.workload(ctx, tn, a.Type, a.Namespace, a.Name, "deployments", "statefulsets", "daemonsets")
	if err != nil {
		return nil, err
	}
	if paused, _, _ := unstructured.NestedBool(o.Object, "spec", "paused"); paused {
		return nil, fmt.Errorf("%s %s/%s is paused; a restart would do nothing until it is resumed, so nothing was proposed", t.Kind, a.Namespace, a.Name)
	}
	before, found, _ := unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", RestartAnnotation)
	if !found {
		before = "(not set)"
	}
	// Fixed now, so the dry-run and the real request are byte-identical.
	stamp := time.Now().UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{RestartAnnotation: stamp}}}}})
	args, _ := json.Marshal(a)

	pods := "its pods"
	if n, ok, _ := unstructured.NestedInt64(o.Object, "spec", "replicas"); ok {
		pods = fmt.Sprintf("its %d pod(s)", n)
	}
	strategy, _, _ := unstructured.NestedString(o.Object, "spec", "strategy", "type")
	if strategy == "" {
		strategy, _, _ = unstructured.NestedString(o.Object, "spec", "updateStrategy", "type")
	}
	p := &Plan{
		ID: newID(), Tool: tn, c: r.c, Type: t, Namespace: a.Namespace, Name: a.Name,
		Args: args, Reason: a.Reason,
		Title: fmt.Sprintf("Restart %s %s/%s (replace %s)", t.Kind, a.Namespace, a.Name, pods),
		Changes: []audit.Change{{
			Field:  "spec.template.metadata.annotations[" + RestartAnnotation + "]",
			Before: before, After: stamp,
		}},
		Reversible:    false,
		Reversibility: "Not undoable, but not destructive: the pod template is otherwise unchanged, so the replacement pods run the same spec. The restart itself cannot be taken back once pods have been replaced.",
		Warnings:      systemNamespaceWarning(a.Namespace),
		body:          body,
	}
	switch strategy {
	case "Recreate":
		p.Warnings = append(p.Warnings, "Update strategy is Recreate: all pods stop before new ones start, so there will be downtime.")
	case "OnDelete":
		p.Warnings = append(p.Warnings, "Update strategy is OnDelete: pods are only replaced when someone deletes them, so this restart will not take effect by itself.")
	}
	if n, ok, _ := unstructured.NestedInt64(o.Object, "spec", "replicas"); ok && n == 1 && t.Resource == "statefulsets" {
		p.Warnings = append(p.Warnings, "Single-replica StatefulSet: the pod is stopped before its replacement starts, so there will be a short outage.")
	}
	p.recheck = func(ctx context.Context) error {
		now, err := r.c.Get(ctx, t, a.Namespace, a.Name)
		if err != nil {
			return err
		}
		if err := sameObject(o, now); err != nil {
			return err
		}
		cur, found, _ := unstructured.NestedString(now.Object, "spec", "template", "metadata", "annotations", RestartAnnotation)
		if !found {
			cur = "(not set)"
		}
		if cur != before {
			return &StaleError{Msg: fmt.Sprintf("not applied: %s %s/%s was already restarted by someone else (restartedAt is now %s) since this was approved", t.Kind, a.Namespace, a.Name, cur)}
		}
		return nil
	}
	p.describe = func(*unstructured.Unstructured) string {
		return fmt.Sprintf("applied: rollout restart of %s %s/%s started at %s. Pods are being replaced according to the update strategy; check rollout progress before reporting success.", t.Kind, a.Namespace, a.Name, stamp)
	}
	return p, nil
}

type metaArgs struct {
	Type      string            `json:"type"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Set       map[string]string `json:"set,omitempty"`
	Remove    []string          `json:"remove,omitempty"`
	Reason    string            `json:"reason"`
}

// managedKeys are owned by controllers or by kubectl itself. Changing them by
// hand breaks ownership, rollout history or `kubectl apply`, so they are
// refused rather than proposed.
var managedKeys = map[string]map[string]string{
	"labels": {
		"pod-template-hash":                  "it ties a pod to its ReplicaSet",
		"controller-revision-hash":           "it ties a pod to its controller revision",
		"statefulset.kubernetes.io/pod-name": "it is managed by the StatefulSet controller",
		"apps.kubernetes.io/pod-index":       "it is managed by the StatefulSet controller",
		"controller-uid":                     "it ties a pod to its Job",
		"batch.kubernetes.io/controller-uid": "it ties a pod to its Job",
		"job-name":                           "it ties a pod to its Job",
		"batch.kubernetes.io/job-name":       "it ties a pod to its Job",
		"kubernetes.io/metadata.name":        "it is set by the API server",
	},
	"annotations": {
		"kubectl.kubernetes.io/last-applied-configuration": "kubectl apply depends on it",
		"deployment.kubernetes.io/revision":                "it is the Deployment's rollout history",
		"deployment.kubernetes.io/desired-replicas":        "it is managed by the Deployment controller",
		"deployment.kubernetes.io/max-replicas":            "it is managed by the Deployment controller",
		"control-plane.alpha.kubernetes.io/leader":         "it is a leader-election record",
		RestartAnnotation:                                  "use rollout_restart instead",
	},
}

const maxMetaKeys = 20

func (r *Registry) planMetadata(ctx context.Context, tn, field string, raw json.RawMessage) (*Plan, error) {
	var a metaArgs
	if err := decode(tn, raw, &a); err != nil {
		return nil, err
	}
	if err := require(tn, "reason", a.Reason); err != nil {
		return nil, err
	}
	if len(a.Set)+len(a.Remove) == 0 {
		return nil, &ArgError{Tool: tn, Msg: `give at least one key in "set" or "remove"`}
	}
	if len(a.Set)+len(a.Remove) > maxMetaKeys {
		return nil, &ArgError{Tool: tn, Msg: fmt.Sprintf("at most %d keys per proposal", maxMetaKeys)}
	}
	for _, k := range a.Remove {
		if _, dup := a.Set[k]; dup {
			return nil, &ArgError{Tool: tn, Msg: fmt.Sprintf("key %q is in both \"set\" and \"remove\"", k)}
		}
	}
	keys := make([]string, 0, len(a.Set)+len(a.Remove))
	for k := range a.Set {
		keys = append(keys, k)
	}
	keys = append(keys, a.Remove...)
	sort.Strings(keys)
	for _, k := range keys {
		if why, managed := managedKeys[field][k]; managed {
			return nil, fmt.Errorf("refused: the %s key %q is system-managed (%s); nothing was proposed", strings.TrimSuffix(field, "s"), k, why)
		}
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, &ArgError{Tool: tn, Msg: fmt.Sprintf("%q is not a valid key: %s", k, strings.Join(errs, "; "))}
		}
		if v, set := a.Set[k]; set && field == "labels" {
			if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
				return nil, &ArgError{Tool: tn, Msg: fmt.Sprintf("%q is not a valid value for label %q: %s", v, k, strings.Join(errs, "; "))}
			}
		}
	}

	t, o, err := r.fetch(ctx, tn, objArgs{Type: a.Type, Namespace: a.Namespace, Name: a.Name})
	if err != nil {
		return nil, err
	}
	if !t.Namespaced {
		a.Namespace = ""
	}
	if err := r.guardNamespace(a.Namespace); err != nil {
		return nil, err
	}
	if !t.Namespaced && t.Resource == "namespaces" {
		if err := r.guardNamespace(a.Name); err != nil {
			return nil, err
		}
	}

	read := func(o *unstructured.Unstructured) map[string]string {
		if field == "labels" {
			return o.GetLabels()
		}
		return o.GetAnnotations()
	}
	current := read(o)
	patch := map[string]any{}
	before := map[string]*string{} // nil = absent
	var changes []audit.Change
	for _, k := range keys {
		old, had := current[k]
		newVal, setting := a.Set[k]
		switch {
		case setting && had && old == newVal:
			continue // already so
		case !setting && !had:
			continue // already absent
		}
		ch := audit.Change{Field: "metadata." + field + "[" + k + "]", Before: "(not set)", After: "(removed)"}
		if had {
			ch.Before = textutil.Truncate(old, 512)
			v := old
			before[k] = &v
		} else {
			before[k] = nil
		}
		if setting {
			ch.After = textutil.Truncate(newVal, 512)
			patch[k] = newVal
		} else {
			patch[k] = nil // JSON merge patch: null deletes the key
		}
		changes = append(changes, ch)
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("%s %s already has exactly these %s; there is nothing to change", t.Kind, qualified(a.Namespace, a.Name), field)
	}

	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{field: patch}})
	args, _ := json.Marshal(a)
	target := fmt.Sprintf("%s %s", t.Kind, qualified(a.Namespace, a.Name))
	p := &Plan{
		ID: newID(), Tool: tn, c: r.c, Type: t, Namespace: a.Namespace, Name: a.Name,
		Args: args, Reason: a.Reason,
		Title:         fmt.Sprintf("Change %d %s on %s", len(changes), plural(len(changes), strings.TrimSuffix(field, "s")), target),
		Changes:       changes,
		Reversible:    true,
		Reversibility: "Reversible: put each key back to the \"before\" value shown (they are also kept in the audit trail).",
		Warnings:      append(systemNamespaceWarning(a.Namespace), metadataWarnings(field, t, o, keys)...),
		body:          body,
	}
	p.recheck = func(ctx context.Context) error {
		now, err := r.c.Get(ctx, t, a.Namespace, a.Name)
		if err != nil {
			return err
		}
		if err := sameObject(o, now); err != nil {
			return err
		}
		cur := read(now)
		for k, want := range before {
			got, has := cur[k]
			if (want == nil) != !has || (want != nil && *want != got) {
				return &StaleError{Msg: fmt.Sprintf("not applied: %s key %q on %s changed since this was approved; look again before proposing", strings.TrimSuffix(field, "s"), k, target)}
			}
		}
		return nil
	}
	p.describe = func(*unstructured.Unstructured) string {
		var parts []string
		for _, ch := range changes {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", ch.Field, ch.Before, ch.After))
		}
		return fmt.Sprintf("applied to %s: %s", target, strings.Join(parts, "; "))
	}
	return p, nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// metadataWarnings points out when "just a label" is more than a label.
func metadataWarnings(field string, t kube.ResourceType, o *unstructured.Unstructured, keys []string) []string {
	var w []string
	if field == "labels" {
		switch {
		case t.Group == "" && t.Resource == "pods":
			w = append(w, "Pod labels are what Services and controllers select on: changing one can take this pod out of load-balancing or make its controller start a replacement.")
		case t.Group == "" && t.Resource == "namespaces":
			w = append(w, "Namespace labels can switch Pod Security levels and NetworkPolicy/webhook selection for everything in the namespace.")
		case t.Group == "" && t.Resource == "nodes":
			w = append(w, "Node labels drive scheduling (nodeSelector, affinity): changing one can stop pods from scheduling here or attract new ones.")
		}
	} else {
		switch {
		case t.Resource == "services" || t.Resource == "ingresses" || t.Resource == "gateways":
			w = append(w, fmt.Sprintf("Annotations on a %s are often read by controllers (load balancers, ingress, DNS, certificates): this may reconfigure real infrastructure.", t.Kind))
		case t.Group == "" && t.Resource == "namespaces":
			w = append(w, "Namespace annotations can be policy inputs (schedulers, service meshes, admission) for everything in the namespace.")
		}
	}
	for _, k := range keys {
		prefix, _, hasPrefix := strings.Cut(k, "/")
		if hasPrefix && (prefix == "kubernetes.io" || prefix == "k8s.io" || strings.HasSuffix(prefix, ".kubernetes.io") || strings.HasSuffix(prefix, ".k8s.io")) {
			w = append(w, fmt.Sprintf("%q is in a namespace reserved for Kubernetes components, which may act on it.", k))
		}
	}
	if len(o.GetOwnerReferences()) > 0 && field == "labels" && !(t.Group == "" && t.Resource == "pods") {
		w = append(w, "This object is managed by another object, which may overwrite the change.")
	}
	return w
}
