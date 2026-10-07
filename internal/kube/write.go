package kube

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// FieldManager identifies k8s-copilot's writes in managedFields.
const FieldManager = "k8s-copilot"

// MergePatch is THE write path: the only place in the whole program that
// sends a mutating request to the cluster. It can only JSON-merge-patch an
// existing object (or its named subresource) — there is no create, no delete,
// no replace.
//
// With dryRun set the API server runs the full request (admission,
// validation) and persists nothing.
//
// Callers: internal/tools mutate plans only, and only via the approval gate.
// internal/guard fails the build's tests if a write call appears anywhere
// else.
func (c *Cluster) MergePatch(ctx context.Context, t ResourceType, namespace, name string, body []byte, dryRun bool, subresource string) (*unstructured.Unstructured, error) {
	opts := metav1.PatchOptions{FieldManager: FieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if !t.Namespaced {
		namespace = ""
	}
	var sub []string
	if subresource != "" {
		sub = []string{subresource}
	}
	return c.dyn.Resource(t.GVR()).Namespace(namespace).Patch(ctx, name, types.MergePatchType, body, opts, sub...)
}
